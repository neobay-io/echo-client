package core

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Tests must not depend on which CLIs this machine has installed, nor touch
// the lock directory real services use.
func init() {
	defaultUpgradeCLIInspector = func(_ context.Context, agentType string) AgentCLIInfo {
		return AgentCLIInfo{Agent: agentType, Binary: AgentCLIBinary(agentType)}
	}
	defaultAgentUpgradeLockDir = func() string { return "" }
}

func fakeInspector(installs map[string]AgentCLIInfo) func(context.Context, string) AgentCLIInfo {
	return func(_ context.Context, agentType string) AgentCLIInfo {
		if info, ok := installs[agentType]; ok {
			return info
		}
		return AgentCLIInfo{Agent: agentType, Binary: AgentCLIBinary(agentType)}
	}
}

const standaloneCodexPath = "/Users/u/.local/bin/codex"

func standaloneCodex(others ...AgentCLIInstall) AgentCLIInfo {
	return AgentCLIInfo{
		Agent:  "codex",
		Binary: "codex",
		Selected: &AgentCLIInstall{
			Path:     standaloneCodexPath,
			Resolved: "/Volumes/x/.codex/packages/standalone/releases/0.153.0-aarch64-apple-darwin/bin/codex",
		},
		Others: others,
	}
}

func TestDetectInstallRoute(t *testing.T) {
	cases := []struct {
		target, path, resolved, want string
	}{
		{"codex", "/u/.local/bin/codex", "/u/.codex/packages/standalone/releases/1/bin/codex", installRouteStandalone},
		{"codex", "/opt/homebrew/bin/codex", "/opt/homebrew/lib/node_modules/@openai/codex/bin/codex.js", installRouteNPM},
		{"codex", "/opt/homebrew/bin/codex", "/opt/homebrew/Caskroom/codex/0.1/codex", installRouteBrew},
		{"codex", "/usr/local/bin/codex", "/usr/local/bin/codex", installRouteUnknown},
		{"claudecode", "/u/.local/bin/claude", "/u/.local/share/claude/versions/2.1.326", installRouteNative},
		{"claudecode", "/opt/homebrew/bin/claude", "/opt/homebrew/lib/node_modules/@anthropic-ai/claude-code/cli.js", installRouteNPM},
		{"gemini", "/opt/homebrew/bin/gemini", "/opt/homebrew/lib/node_modules/@google/gemini-cli/bundle/gemini.js", installRouteNPM},
	}
	for _, tc := range cases {
		got := detectInstallRoute(tc.target, &AgentCLIInstall{Path: tc.path, Resolved: tc.resolved})
		if got != tc.want {
			t.Errorf("detectInstallRoute(%s, %s) = %q, want %q", tc.target, tc.resolved, got, tc.want)
		}
	}
	if detectInstallRoute("codex", nil) != "" {
		t.Error("nil install should have no route")
	}
}

// T10.1: the version probe and the update must target the copy agents run.
func TestUpgradeTargetsTheBinaryAgentsRun(t *testing.T) {
	mgr := NewAgentUpgradeManager(singleTargetUpgradeConfig("codex", "package_manager", "codex --version", "npm install -g @openai/codex@latest"))
	npmCopy := AgentCLIInstall{Path: "/opt/homebrew/bin/codex", Resolved: "/opt/homebrew/lib/node_modules/@openai/codex/bin/codex.js"}
	mgr.SetCLIInspector(fakeInspector(map[string]AgentCLIInfo{"codex": standaloneCodex(npmCopy)}))
	var ran []string
	mgr.SetCommandRunner(func(_ context.Context, cmd string) (string, error) {
		ran = append(ran, cmd)
		if strings.HasSuffix(cmd, "--version") {
			return "codex-cli 0.153.0", nil
		}
		return "installed", nil
	})

	results, err := mgr.Run(context.Background(), "codex")
	if err != nil || len(results) != 1 || results[0].Err != nil {
		t.Fatalf("Run = %#v, %v", results, err)
	}
	if len(ran) != 3 || ran[0] != "'"+standaloneCodexPath+"' --version" || ran[1] != codexStandaloneInstallCommand {
		t.Fatalf("commands = %#v", ran)
	}
	st := mgr.Statuses(context.Background())
	var codex AgentUpgradeStatus
	for _, s := range st {
		if s.Name == "codex" {
			codex = s
		}
	}
	if codex.InstallRoute != installRouteStandalone || codex.BinaryPath != standaloneCodexPath ||
		!strings.Contains(codex.Warning, "/opt/homebrew/bin/codex") {
		t.Fatalf("status = %#v", codex)
	}
}

func TestExplicitCommandsWinOverDetectedRoute(t *testing.T) {
	cfg := singleTargetUpgradeConfig("codex", "custom", "/abs/codex --version", "my-upgrade")
	target := cfg.Targets["codex"]
	target.VersionCommandSet, target.UpdateCommandSet = true, true
	cfg.Targets["codex"] = target
	mgr := NewAgentUpgradeManager(cfg)
	mgr.SetCLIInspector(fakeInspector(map[string]AgentCLIInfo{"codex": standaloneCodex()}))
	var ran []string
	mgr.SetCommandRunner(func(_ context.Context, cmd string) (string, error) {
		ran = append(ran, cmd)
		return "codex 1", nil
	})
	if _, err := mgr.Run(context.Background(), "codex"); err != nil {
		t.Fatal(err)
	}
	if len(ran) != 3 || ran[0] != "/abs/codex --version" || ran[1] != "my-upgrade" {
		t.Fatalf("commands = %#v", ran)
	}
}

func TestComputeNextAutoRunSurvivesRestarts(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.Local)
	day := 24 * time.Hour
	grace := 2 * time.Minute
	// Restart 3h after the last run: next run stays last+24h, not now+24h.
	last := now.Add(-3 * time.Hour)
	if got := computeNextAutoRun(now, time.Time{}, last, day, grace, nil); !got.Equal(last.Add(day)) {
		t.Fatalf("next = %v, want %v", got, last.Add(day))
	}
	// Missed while down: run shortly after start.
	if got := computeNextAutoRun(now, time.Time{}, now.Add(-30*time.Hour), day, grace, nil); !got.Equal(now.Add(grace)) {
		t.Fatalf("missed run next = %v", got)
	}
	// Never ran: keep the first persisted schedule across restarts.
	first := now.Add(5 * time.Hour)
	if got := computeNextAutoRun(now, first, time.Time{}, day, grace, nil); !got.Equal(first) {
		t.Fatalf("first schedule = %v", got)
	}
	if got := computeNextAutoRun(now, time.Time{}, time.Time{}, day, grace, nil); !got.Equal(now.Add(day)) {
		t.Fatalf("fresh schedule = %v", got)
	}
	// Window moves a run to the next window start.
	w, err := parseUpgradeWindow("02:00-05:00")
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 11, 18, 2, 0, 0, 0, time.Local)
	if got := computeNextAutoRun(now, time.Time{}, now.Add(-30*time.Hour), day, grace, w); !got.Equal(want) {
		t.Fatalf("windowed next = %v, want %v", got, want)
	}
}

func TestUpgradeWindowParsingAndWrap(t *testing.T) {
	w, err := parseUpgradeWindow("23:30-01:00")
	if err != nil {
		t.Fatal(err)
	}
	at := func(h, m int) time.Time { return time.Date(2026, 1, 1, h, m, 0, 0, time.Local) }
	if !w.contains(at(23, 45)) || !w.contains(at(0, 30)) || w.contains(at(1, 0)) || w.contains(at(12, 0)) {
		t.Fatal("wrapping window membership wrong")
	}
	if got := w.next(at(12, 0)); !got.Equal(at(23, 30)) {
		t.Fatalf("next = %v", got)
	}
	for _, bad := range []string{"2-5", "02:00", "25:00-01:00", "03:00-03:00"} {
		if _, err := parseUpgradeWindow(bad); err == nil {
			t.Errorf("parseUpgradeWindow(%q) accepted", bad)
		}
	}
	if w, err := parseUpgradeWindow(""); w != nil || err != nil {
		t.Fatal("empty window should mean no window")
	}
}

func TestUpgradeStatePersistsAcrossRestartsAndNotifies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent-upgrade-state.json")
	cfg := singleTargetUpgradeConfig("codex", "custom", "codex --version", "custom-upgrade")
	version := "codex 1.0.0"
	runner := func(_ context.Context, cmd string) (string, error) {
		switch cmd {
		case "codex --version":
			return version, nil
		case "custom-upgrade":
			version = "codex 1.1.0"
			return "ok", nil
		}
		return "", errors.New("unexpected")
	}

	mgr := NewAgentUpgradeManager(cfg)
	mgr.SetStatePath(path)
	mgr.SetCommandRunner(runner)
	var mu sync.Mutex
	var notes []string
	mgr.SetNotifier(func(project, key, text string) error {
		mu.Lock()
		defer mu.Unlock()
		notes = append(notes, project+"|"+key+"|"+text)
		return nil
	})
	mgr.SetNotifyChat(AgentUpgradeNotifyChat{Project: "proj", SessionKey: "feishu:c:u"}, true)

	if _, err := mgr.Run(context.Background(), "codex"); err != nil {
		t.Fatal(err)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "codex upgraded: codex 1.0.0 → codex 1.1.0") || !strings.HasPrefix(notes[0], "proj|feishu:c:u|") {
		t.Fatalf("notes = %#v", notes)
	}
	// A run with no version change stays quiet.
	if _, err := mgr.Run(context.Background(), "codex"); err != nil {
		t.Fatal(err)
	}
	if len(notes) != 1 {
		t.Fatalf("no-op run notified: %#v", notes)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil || raw["last_results"] == nil || raw["notify"] == nil {
		t.Fatalf("state = %s", data)
	}

	restarted := NewAgentUpgradeManager(cfg)
	restarted.SetStatePath(path)
	last, ok := restarted.LastResult("codex")
	if !ok || last.BeforeVersion != "codex 1.1.0" || last.Automatic {
		t.Fatalf("last result after restart = %#v", last)
	}
	if chats := restarted.NotifyChats(); len(chats) != 1 || chats[0].Project != "proj" {
		t.Fatalf("notify chats after restart = %#v", chats)
	}
}

func TestUpgradeFailuresAreNotifiedAndPersisted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	cfg := singleTargetUpgradeConfig("codex", "custom", "codex --version", "custom-upgrade")
	mgr := NewAgentUpgradeManager(cfg)
	mgr.SetStatePath(path)
	mgr.SetCommandRunner(func(_ context.Context, cmd string) (string, error) {
		if cmd == "custom-upgrade" {
			return "curl: (28) timed out", errors.New("exit status 28")
		}
		return "codex 1.0.0", nil
	})
	var notes []string
	mgr.SetNotifier(func(_, _, text string) error { notes = append(notes, text); return nil })
	mgr.SetNotifyChat(AgentUpgradeNotifyChat{Project: "p", SessionKey: "k"}, true)
	if _, err := mgr.AutoCheck(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "codex upgrade failed: exit status 28") || !strings.Contains(notes[0], "(automatic)") {
		t.Fatalf("notes = %#v", notes)
	}
	restarted := NewAgentUpgradeManager(cfg)
	restarted.SetStatePath(path)
	if st := restarted.failureState("codex"); st.ConsecutiveFailures != 1 || st.LastFailureReason != "exit status 28" {
		t.Fatalf("failure state after restart = %#v", st)
	}
	if last, _ := restarted.LastResult("codex"); last.Error != "exit status 28" || !last.Automatic {
		t.Fatalf("last result = %#v", last)
	}
}

// T10.4: two services must not upgrade the same CLI at once, and the other
// service holds new turns while one upgrades.
func TestCrossProcessUpgradeLock(t *testing.T) {
	dir := t.TempDir()
	release, ok, err := acquireUpgradeLock(dir, "codex")
	if err != nil || !ok {
		t.Fatalf("acquire = %v %v", ok, err)
	}

	other := NewAgentUpgradeManager(singleTargetUpgradeConfig("codex", "custom", "codex --version", "custom-upgrade"))
	other.SetLockDir(dir)
	held := true
	other.SetCommandRunner(func(_ context.Context, cmd string) (string, error) {
		if cmd == "custom-upgrade" && held {
			t.Error("upgrade ran while another process held the lock")
		}
		return "codex 1", nil
	})
	if !other.IsTargetRunning("codex") {
		t.Fatal("IsTargetRunning should see the other process's upgrade")
	}
	results, _ := other.Run(context.Background(), "codex")
	if len(results) != 1 || !results[0].Skipped || !strings.Contains(results[0].Reason, "another echo-client process") {
		t.Fatalf("results = %#v", results)
	}

	release()
	held = false
	if other.IsTargetRunning("codex") {
		t.Fatal("lock still reported after release")
	}
	if results, _ := other.Run(context.Background(), "codex"); len(results) != 1 || results[0].Skipped {
		t.Fatalf("run after release = %#v", results)
	}
}

func TestActiveAgentWorkCountsBackgroundTurns(t *testing.T) {
	e := NewEngine("p", &stubAgent{}, nil, "", LangEnglish)
	if e.ActiveAgentWork() != 0 {
		t.Fatal("fresh engine busy")
	}
	done := e.trackBackgroundTurn()
	s := e.sessions.GetOrCreateActive("chat")
	s.TryLock()
	if got := e.ActiveAgentWork(); got != 2 {
		t.Fatalf("ActiveAgentWork = %d, want 2", got)
	}
	done()
	s.Unlock()
	if got := e.ActiveAgentWork(); got != 0 {
		t.Fatalf("ActiveAgentWork = %d, want 0", got)
	}
}

func TestAgentUpgradeNotifyCommandRequiresAdmin(t *testing.T) {
	cfg := singleTargetUpgradeConfig("codex", "custom", "codex --version", "custom-upgrade")
	cfg.AllowedUserIDs = []string{"admin"}
	mgr := NewAgentUpgradeManager(cfg)
	p := &stubPlatformEngine{n: "test"}
	e := NewEngine("proj", &stubAgent{}, []Platform{p}, "", LangEnglish)
	e.SetAgentUpgradeManager(mgr)

	e.cmdAgentUpgrade(p, &Message{SessionKey: "test:c:u", UserID: "intruder", ReplyCtx: "x"}, []string{"notify", "on"})
	if len(mgr.NotifyChats()) != 0 {
		t.Fatal("non-admin registered a notification chat")
	}
	e.cmdAgentUpgrade(p, &Message{SessionKey: "test:c:u", UserID: "admin", ReplyCtx: "x"}, []string{"notify", "on"})
	if chats := mgr.NotifyChats(); len(chats) != 1 || chats[0] != (AgentUpgradeNotifyChat{Project: "proj", SessionKey: "test:c:u"}) {
		t.Fatalf("chats = %#v", chats)
	}
	e.cmdAgentUpgrade(p, &Message{SessionKey: "test:c:u", UserID: "admin", ReplyCtx: "x"}, []string{"notify", "off"})
	if len(mgr.NotifyChats()) != 0 {
		t.Fatal("notify off did not remove the chat")
	}
}
