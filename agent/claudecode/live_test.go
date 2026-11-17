//go:build live

// Live checks against the real Claude Code CLI and account. Not part of the
// default test run: go test -tags live -run TestLive ./agent/claudecode/ -v

package claudecode

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func liveClaudeTurn(t *testing.T, s core.AgentSession, prompt string) string {
	t.Helper()
	if err := s.Send(prompt, nil, nil); err != nil {
		t.Fatalf("send: %v", err)
	}
	var text strings.Builder
	timeout := time.After(4 * time.Minute)
	for {
		select {
		case evt := <-s.Events():
			switch evt.Type {
			case core.EventText:
				text.WriteString(evt.Content)
			case core.EventError:
				t.Fatalf("turn error: %v", evt.Error)
			case core.EventPermissionRequest:
				t.Fatalf("unexpected permission request for %s", evt.ToolName)
			case core.EventResult:
				text.WriteString(evt.Content)
				if evt.Done {
					return text.String()
				}
			}
		case <-timeout:
			t.Fatal("turn timed out")
		}
	}
}

func hasArgPair(args []string, flag, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

// Per-chat overrides reach the claude process, and a /model switch resumes the
// same transcript (context survives) instead of starting over.
func TestLiveClaudeSessionOverridesAndModelSwitchKeepContext(t *testing.T) {
	workDir := t.TempDir()
	a, err := New(map[string]any{"work_dir": workDir, "model": "sonnet", "mode": "bypassPermissions"})
	if err != nil {
		t.Fatal(err)
	}
	agent := a.(*Agent)

	agent.SetSessionOptions(core.AgentSessionOptions{Model: "haiku", ReasoningLevel: "low", Mode: "acceptEdits"})
	s1, err := agent.StartSession(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	args := s1.(*claudeSession).cmd.Args
	if !hasArgPair(args, "--model", "haiku") || !hasArgPair(args, "--effort", "low") || !hasArgPair(args, "--permission-mode", "acceptEdits") {
		t.Fatalf("overrides missing from claude args: %v", args[1:12])
	}
	reply := liveClaudeTurn(t, s1, "Remember the code word ORCHID-42. Reply with exactly: NOTED")
	t.Logf("turn 1 (haiku): %q", strings.TrimSpace(reply))
	transcript := s1.CurrentSessionID()
	_ = s1.Close()
	if transcript == "" {
		t.Fatal("no session id after first turn")
	}

	// What /model does now: same transcript, new model on the next process.
	agent.SetSessionOptions(core.AgentSessionOptions{Model: "sonnet", ReasoningLevel: "low", Mode: "acceptEdits"})
	s2, err := agent.StartSession(context.Background(), transcript)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s2.Close() }()
	args = s2.(*claudeSession).cmd.Args
	if !hasArgPair(args, "--resume", transcript) || !hasArgPair(args, "--model", "sonnet") {
		t.Fatalf("resume args = %v", args[1:12])
	}
	reply = liveClaudeTurn(t, s2, "What was the code word I asked you to remember? Reply with just the code word.")
	t.Logf("turn 2 (sonnet, resumed %s): %q", transcript, strings.TrimSpace(reply))
	if !strings.Contains(reply, "ORCHID-42") {
		t.Fatalf("context lost after model switch: %q", reply)
	}

	// Zero options fall back to project defaults.
	agent.SetSessionOptions(core.AgentSessionOptions{})
	agent.mu.Lock()
	model, level, mode := agent.startSettingsLocked()
	agent.mu.Unlock()
	if model != "sonnet" || level != "" || mode != "bypassPermissions" {
		t.Fatalf("defaults = %q %q %q", model, level, mode)
	}
}
