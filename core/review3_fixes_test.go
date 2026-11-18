package core

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// Review 3, finding 1: chats call startAgentSession while holding
// interactiveMu, so it must refuse during an upgrade rather than wait.
func TestStartAgentSessionRefusesDuringUpgrade(t *testing.T) {
	session := &jobTestSession{events: make(chan Event, 1)}
	engine := NewEngine("upgrade-proj", &codexNamedAgent{jobTestAgent{session: session}}, nil, "", LangEnglish)
	mgr := NewAgentUpgradeManager(DefaultAgentUpgradeConfig())
	engine.SetAgentUpgradeManager(mgr)
	if !mgr.beginTargetRun("codex") {
		t.Fatal("beginTargetRun")
	}

	done := make(chan error, 1)
	go func() {
		_, err := engine.startAgentSession(context.Background(), "", "chat", nil, AgentSessionOptions{})
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrAgentUpgradeInProgress) {
			t.Fatalf("err = %v, want ErrAgentUpgradeInProgress", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("startAgentSession waited for the upgrade")
	}
	// A chat that raced the upgrade is told so, not "failed to start".
	if msg := engine.agentStartFailedMessage("failed to start agent session"); !strings.Contains(msg, "upgrade is in progress") {
		t.Fatalf("message = %q", msg)
	}
	mgr.endTargetRun("codex")
	if msg := engine.agentStartFailedMessage("failed to start agent session"); !strings.Contains(msg, "failed to start agent session") {
		t.Fatalf("message = %q", msg)
	}
	if _, err := engine.startAgentSession(context.Background(), "", "chat", nil, AgentSessionOptions{}); err != nil {
		t.Fatalf("start after the upgrade: %v", err)
	}
}

// Review 3, finding 3: Stop during the first platform's Start also stops the
// platforms Engine.Start has not reached yet.
func TestEngineStopDuringInitialStartSkipsLaterPlatforms(t *testing.T) {
	gate := make(chan struct{})
	first := &flakyPlatform{name: "feishu", gate: gate, gateCall: 1, entered: make(chan int, 4)}
	second := &flakyPlatform{name: "telegram"}
	e := newPlatformRetryTestEngine(first, second)
	e.platformRetryStopWait = 20 * time.Millisecond

	startDone := make(chan error, 1)
	go func() { startDone <- e.Start() }()
	waitForStartCall(t, first.entered, 1)

	if err := e.Stop(); err != nil {
		t.Fatalf("Stop() = %v", err)
	}
	close(gate)
	select {
	case <-startDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Engine.Start did not return")
	}
	if got := second.snapshot(); got.startCalls != 0 || got.running {
		t.Fatalf("second platform: start calls %d, running %v; want 0, false", got.startCalls, got.running)
	}
	if e.platformRunning(1) {
		t.Fatal("platformRunning = true for a platform skipped by Stop")
	}
}

// Review 3, finding 9: a run is allowed only inside the current window.
func TestAutoRunAllowedRechecksWindow(t *testing.T) {
	at := func(h, m int) time.Time { return time.Date(2026, 11, 18, h, m, 0, 0, time.Local) }
	cfg := DefaultAgentUpgradeConfig()
	cfg.Enabled = true
	cfg.Policy = "auto"
	cfg.Window = "22:00-23:00"
	if autoRunAllowed(cfg, at(3, 0)) {
		t.Fatal("allowed at 03:00 with window 22:00-23:00")
	}
	if !autoRunAllowed(cfg, at(22, 30)) {
		t.Fatal("not allowed inside the window")
	}
	cfg.Window = "23:00-02:00" // wraps midnight
	if !autoRunAllowed(cfg, at(1, 0)) || autoRunAllowed(cfg, at(3, 0)) {
		t.Fatal("wrapping window checked wrongly")
	}
	cfg.Window = ""
	if !autoRunAllowed(cfg, at(3, 0)) {
		t.Fatal("no window should allow any time")
	}
	cfg.Window = "25:00-26:00"
	if autoRunAllowed(cfg, at(3, 0)) {
		t.Fatal("invalid window allowed a run")
	}
	cfg.Window = ""
	cfg.Policy = "off"
	if autoRunAllowed(cfg, at(3, 0)) {
		t.Fatal("policy off allowed a run")
	}
}
