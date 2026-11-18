package core

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestHelpHasFlag(t *testing.T) {
	help := "Usage: tool [options]\n  -p, --prompt <text>   Prompt\n  --prompt-file <f>\n  -S, --session [id]\n  --model=<m>"
	for flag, want := range map[string]bool{
		"--prompt": true, "-p": true, "-S": true, "--session": true, "--model": true,
		"--prompt-f": false, "--mode": false, "-s": false, "--sess": false,
	} {
		if got := helpHasFlag(help, flag); got != want {
			t.Errorf("helpHasFlag(%q) = %v, want %v", flag, got, want)
		}
	}
	if got := missingCLIFlags(help, []string{"--prompt", "--yolo", "-S"}); len(got) != 1 || got[0] != "--yolo" {
		t.Fatalf("missing = %v", got)
	}
}

func TestLastAgentVersion(t *testing.T) {
	for in, want := range map[string]string{
		"0.169.1\n": "0.169.1",
		"Qoder CLI update (check only)\n\nChecking for updates...\nUpdate available: 1.1.89 -> 1.1.91": "1.1.91",
		"codex-cli 0.153.0": "0.153.0",
		"installed":         "",
		"codex 1":           "",
	} {
		if got := lastAgentVersion(in); got != want {
			t.Errorf("lastAgentVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

// T10.2: a CLI already at the newest release is not reinstalled.
func TestUpgradeSkipsWhenAlreadyLatest(t *testing.T) {
	cfg := singleTargetUpgradeConfig("codex", "custom", "codex --version", "codex-update")
	mgr := NewAgentUpgradeManager(cfg)
	latest := "0.169.1"
	var ran []string
	mgr.SetCommandRunner(func(_ context.Context, cmd string) (string, error) {
		ran = append(ran, cmd)
		switch {
		case cmd == "codex --version":
			return "codex-cli 0.169.1", nil
		case strings.HasPrefix(cmd, "npm view '@openai/codex' version"):
			return latest + "\n", nil
		case cmd == "codex-update":
			return "installed", nil
		}
		return "", errors.New("unexpected " + cmd)
	})
	results, err := mgr.Run(context.Background(), "codex")
	if err != nil || len(results) != 1 {
		t.Fatalf("Run = %#v, %v", results, err)
	}
	if r := results[0]; !r.Skipped || r.Reason != "already the latest version (0.169.1)" || r.LatestVersion != "0.169.1" {
		t.Fatalf("result = %#v", r)
	}
	for _, cmd := range ran {
		if cmd == "codex-update" {
			t.Fatal("installer ran although the CLI is up to date")
		}
	}

	// A newer release is installed; so is anything when the lookup fails.
	for _, l := range []string{"0.170.0", ""} {
		latest, ran = l, nil
		results, _ = mgr.Run(context.Background(), "codex")
		if results[0].Skipped || ran[len(ran)-2] != "codex-update" {
			t.Fatalf("latest %q: result = %#v, ran = %v", l, results[0], ran)
		}
	}
}

// T10.5: a CLI that changed on disk restarts idle chat processes and drops
// cached model lists.
type modelCacheAgent struct {
	namedStubAgent
	invalidated atomic.Int32
}

func (a *modelCacheAgent) InvalidateModelCache() { a.invalidated.Add(1) }

func TestAgentCLIChangeRetiresIdleSessions(t *testing.T) {
	var mu sync.Mutex
	id := agentCLIIdentity{path: "/u/.local/share/claude/versions/2.1.326", size: 1}
	prev := agentCLIIdentityOf
	agentCLIIdentityOf = func(Agent) (agentCLIIdentity, bool) {
		mu.Lock()
		defer mu.Unlock()
		return id, true
	}
	defer func() { agentCLIIdentityOf = prev }()

	agent := &modelCacheAgent{namedStubAgent: namedStubAgent{name: "claudecode"}}
	p := &stubPlatformEngine{n: "telegram"}
	e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)
	session := e.sessions.GetOrCreateActive("tg:chat:user")

	first := e.getOrCreateInteractiveState("tg:chat:user", p, "ctx", session)
	if again := e.getOrCreateInteractiveState("tg:chat:user", p, "ctx", session); again != first {
		t.Fatal("unchanged CLI restarted the session")
	}
	mu.Lock()
	id = agentCLIIdentity{path: "/u/.local/share/claude/versions/2.1.327", size: 2}
	mu.Unlock()
	if next := e.getOrCreateInteractiveState("tg:chat:user", p, "ctx", session); next == first {
		t.Fatal("upgraded CLI kept the old agent process")
	}
	if n := agent.invalidated.Load(); n != 1 {
		t.Fatalf("model cache invalidated %d times, want 1", n)
	}
}

// Chat messages that arrive during an upgrade are queued and sent after it.
func TestChatMessageQueuedDuringUpgradeRunsAfterIt(t *testing.T) {
	prevPoll := agentUpgradePollInterval
	agentUpgradePollInterval = 10 * time.Millisecond
	defer func() { agentUpgradePollInterval = prevPoll }()

	sent := make(chan string, 4)
	session := &jobTestSession{events: make(chan Event, 4)}
	session.onSend = func(prompt string) {
		sent <- prompt
		session.events <- Event{Type: EventResult, Content: "done", SessionID: "s1"}
	}
	p := &stubPlatformEngine{n: "telegram"}
	e := NewEngine("test", &codexNamedAgent{jobTestAgent{session: session}}, []Platform{p}, "", LangEnglish)
	mgr := NewAgentUpgradeManager(AgentUpgradeConfig{Enabled: true})
	e.SetAgentUpgradeManager(mgr)
	if !mgr.beginTargetRun("codex") {
		t.Fatal("beginTargetRun")
	}

	e.handleMessage(p, &Message{SessionKey: "tg:chat:user", Platform: "telegram", UserID: "user", Content: "hello there", ReplyCtx: "ctx"})
	p.sentMu.Lock()
	replies := append([]string(nil), p.sent...)
	p.sentMu.Unlock()
	if len(replies) == 0 || !strings.Contains(replies[len(replies)-1], "queued (#1)") {
		t.Fatalf("replies = %q", replies)
	}
	select {
	case prompt := <-sent:
		t.Fatalf("sent %q during the upgrade", prompt)
	case <-time.After(100 * time.Millisecond):
	}

	mgr.endTargetRun("codex")
	select {
	case prompt := <-sent:
		if !strings.Contains(prompt, "hello there") {
			t.Fatalf("prompt = %q", prompt)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("queued message was not sent after the upgrade")
	}
}
