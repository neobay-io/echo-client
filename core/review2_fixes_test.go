package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// relayTestSession never ends its turn and counts Close calls.
type relayTestSession struct {
	events chan Event
	closed atomic.Int32
}

func (s *relayTestSession) Send(string, []ImageAttachment, []FileAttachment) error { return nil }
func (s *relayTestSession) RespondPermission(string, PermissionResult) error       { return nil }
func (s *relayTestSession) Events() <-chan Event                                   { return s.events }
func (s *relayTestSession) CurrentSessionID() string                               { return "relay-session" }
func (s *relayTestSession) Alive() bool                                            { return true }
func (s *relayTestSession) Close() error                                           { s.closed.Add(1); return nil }

type relayTestAgent struct{ session *relayTestSession }

func (a *relayTestAgent) Name() string { return "job-test" }
func (a *relayTestAgent) StartSession(context.Context, string) (AgentSession, error) {
	return a.session, nil
}
func (a *relayTestAgent) ListSessions(context.Context) ([]AgentSessionInfo, error) { return nil, nil }
func (a *relayTestAgent) Stop() error                                              { return nil }

// Review finding 5: a cancelled relay returns instead of waiting forever for
// an agent that will not send a terminal event, and closes its session.
func TestRelayReturnsOnCancelAndClosesSession(t *testing.T) {
	session := &relayTestSession{events: make(chan Event)}
	engine := NewEngine("relay-proj", &relayTestAgent{session: session}, nil, "", LangEnglish)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := engine.HandleRelay(ctx, "other", "chat-1", "hello")
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want deadline exceeded", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("HandleRelay hung after its context was cancelled")
	}
	if session.closed.Load() == 0 {
		t.Fatal("relay agent session was not closed")
	}
	if n := engine.ActiveAgentWork(); n != 0 {
		t.Fatalf("background work still counted after the relay returned: %d", n)
	}
}

// codexNamedAgent is a job test agent named like a real upgrade target.
type codexNamedAgent struct{ jobTestAgent }

func (a *codexNamedAgent) Name() string { return "codex" }

// Review finding 6: background turns wait while the agent's CLI is upgraded.
func TestAgentStartWaitsForUpgrade(t *testing.T) {
	prev := agentUpgradePollInterval
	agentUpgradePollInterval = 10 * time.Millisecond
	defer func() { agentUpgradePollInterval = prev }()

	session := &jobTestSession{events: make(chan Event, 1)}
	engine := NewEngine("upgrade-proj", &codexNamedAgent{jobTestAgent{session: session}}, nil, "", LangEnglish)
	mgr := NewAgentUpgradeManager(DefaultAgentUpgradeConfig())
	engine.SetAgentUpgradeManager(mgr)
	if !mgr.beginTargetRun("codex") {
		t.Fatal("beginTargetRun")
	}

	started := make(chan error, 1)
	go func() {
		_, err := engine.startAgentSessionAfterUpgrade(context.Background(), "", "job:1", nil, AgentSessionOptions{})
		started <- err
	}()
	select {
	case err := <-started:
		t.Fatalf("agent started during the upgrade (err=%v)", err)
	case <-time.After(150 * time.Millisecond):
	}
	mgr.endTargetRun("codex")
	select {
	case err := <-started:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("agent did not start after the upgrade finished")
	}

	// A caller that gives up stops waiting.
	mgr.beginTargetRun("codex")
	defer mgr.endTargetRun("codex")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := engine.startAgentSessionAfterUpgrade(ctx, "", "job:2", nil, AgentSessionOptions{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
}

// Review finding 1: the install route picks the command but never turns an
// observed or disabled target into an upgrade.
func TestInstallRouteKeepsObserveAndDisabled(t *testing.T) {
	info := AgentCLIInfo{Selected: &AgentCLIInstall{Path: "/u/.local/bin/codex", Resolved: "/u/.codex/packages/standalone/releases/1/bin/codex"}}
	for _, strategy := range []string{"observe", "disabled"} {
		target := AgentUpgradeTargetConfig{Enabled: true, Strategy: strategy}
		got, _ := applyInstallRoute("codex", target, info, installRouteStandalone)
		if got.Strategy != strategy {
			t.Errorf("strategy %q became %q", strategy, got.Strategy)
		}
	}
	got, _ := applyInstallRoute("codex", AgentUpgradeTargetConfig{Strategy: "package_manager"}, info, installRouteStandalone)
	if got.Strategy != "builtin" || got.UpdateCommand != codexStandaloneInstallCommand {
		t.Errorf("upgrading strategy not routed: %+v", got)
	}
}

// Review finding 12: pinning a Homebrew Codex must not install an npm copy.
func TestPinOnHomebrewCodexIsBlocked(t *testing.T) {
	m := NewAgentUpgradeManager(DefaultAgentUpgradeConfig())
	target := AgentUpgradeTargetConfig{Enabled: true, Strategy: "package_manager", PinVersion: "0.160.0", UpdateCommand: "brew upgrade --cask codex"}
	resolved := m.resolveTargetRules(context.Background(), "codex", target, nil, installRouteBrew)
	if resolved.blockedReason == "" || strings.Contains(resolved.cfg.UpdateCommand, "npm") {
		t.Fatalf("resolved = %+v", resolved)
	}
	resolved = m.resolveTargetRules(context.Background(), "codex", target, nil, installRouteNPM)
	if resolved.blockedReason != "" || !strings.Contains(resolved.cfg.UpdateCommand, "@openai/codex@0.160.0") {
		t.Fatalf("npm pin = %+v", resolved)
	}
}

// Review finding 7: without the cross-process lock, do not upgrade.
func TestUpgradeSkipsWhenLockUnavailable(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	m := NewAgentUpgradeManager(singleTargetUpgradeConfig("codex", "custom", "codex --version", "codex-update"))
	m.SetLockDir(filepath.Join(blocker, "locks"))
	var ran []string
	m.SetCommandRunner(func(_ context.Context, cmd string) (string, error) {
		ran = append(ran, cmd)
		return "codex 1.0.0", nil
	})
	results, err := m.Run(context.Background(), "codex")
	if err != nil || len(results) != 1 {
		t.Fatalf("Run = %+v, %v", results, err)
	}
	// Review 3: reported as a failure, so it is notified and backs off.
	if results[0].Err == nil || !strings.Contains(results[0].Err.Error(), "lock unavailable") {
		t.Fatalf("result = %+v", results[0])
	}
	if text := upgradeNotificationText(results, true); !strings.Contains(text, "codex upgrade failed: upgrade lock unavailable") {
		t.Fatalf("notification = %q", text)
	}
	if m.failureState("codex").ConsecutiveFailures != 1 {
		t.Fatalf("failure not recorded: %+v", m.failureState("codex"))
	}
	for _, cmd := range ran {
		if cmd == "codex-update" {
			t.Fatal("update ran without the lock")
		}
	}
}

// Review finding 8: concurrent saves never leave an older snapshot on disk.
func TestConcurrentStateSavesKeepTheLatest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent-upgrade-state.json")
	m := NewAgentUpgradeManager(DefaultAgentUpgradeConfig())
	m.SetStatePath(path)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			m.SetNotifyChat(AgentUpgradeNotifyChat{Project: "p", SessionKey: fmt.Sprintf("chat-%d", i%5)}, i%2 == 0)
		}(i)
	}
	wg.Wait()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved agentUpgradeState
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(saved.Notify, m.NotifyChats()) && (len(saved.Notify) != 0 || len(m.NotifyChats()) != 0) {
		t.Fatalf("saved %+v, in memory %+v", saved.Notify, m.NotifyChats())
	}
}

// bypassOnlyAgent offers a single mode, like Kimi, and records the options it
// is started with.
type bypassOnlyAgent struct {
	jobTestAgent
	opts AgentSessionOptions
}

func (a *bypassOnlyAgent) Name() string                               { return "kimi" }
func (a *bypassOnlyAgent) SetMode(string)                             {}
func (a *bypassOnlyAgent) GetMode() string                            { return PermissionModeBypass }
func (a *bypassOnlyAgent) SetSessionOptions(opts AgentSessionOptions) { a.opts = opts }
func (a *bypassOnlyAgent) PermissionModes() []PermissionModeInfo {
	return []PermissionModeInfo{{Key: PermissionModeBypass, Name: "Bypass"}}
}

// Review finding 2: a mode stored under another agent type is neither shown
// nor passed on; /status shows what actually runs.
func TestStoredModeTheAgentDoesNotOfferIsIgnored(t *testing.T) {
	agent := &bypassOnlyAgent{jobTestAgent: jobTestAgent{session: &jobTestSession{events: make(chan Event, 1)}}}
	e := NewEngine("kimi-proj", agent, nil, "", LangEnglish)
	s := e.sessions.GetOrCreateActive("chat")
	s.UpdateSettings(func(o *AgentSessionOptions) { o.Mode = "plan"; o.Model = "kimi-code/k3" })

	if got := e.effectiveMode(s); got != PermissionModeBypass {
		t.Fatalf("effectiveMode = %q, want the mode that actually runs", got)
	}
	if _, err := e.startAgentSession(context.Background(), "", "chat", nil, s.Settings()); err != nil {
		t.Fatal(err)
	}
	if agent.opts.Mode != "" || agent.opts.Model != "kimi-code/k3" {
		t.Fatalf("options passed to the agent = %+v", agent.opts)
	}
	s.UpdateSettings(func(o *AgentSessionOptions) { o.Mode = "yolo" })
	if got := e.effectiveMode(s); got != PermissionModeBypass {
		t.Fatalf("alias not resolved: %q", got)
	}
}
