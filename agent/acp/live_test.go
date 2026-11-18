//go:build live

package acp

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// Live checks against the installed CLIs (go test -tags live -run TestLive
// ./agent/acp). They use the account's quota: one short turn each.
func liveSession(t *testing.T, agent, command string, mode string) *Session {
	t.Helper()
	path, err := core.ResolveAgentCLI(agent)
	if err != nil {
		path, err = exec.LookPath(command)
	}
	if err != nil {
		t.Skipf("%s not installed", agent)
	}
	s, err := Start(context.Background(), Config{Agent: agent, Command: path, Args: []string{"acp"}, Env: os.Environ(), WorkDir: t.TempDir(), Mode: mode})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func liveTurn(t *testing.T, s *Session, prompt string) (string, core.Event) {
	t.Helper()
	if err := s.Send(prompt, nil, nil); err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	deadline := time.After(3 * time.Minute)
	for {
		select {
		case ev, ok := <-s.Events():
			if !ok {
				t.Fatal("events closed")
			}
			switch ev.Type {
			case core.EventText:
				text.WriteString(ev.Content)
			case core.EventPermissionRequest:
				_ = s.RespondPermission(ev.RequestID, core.PermissionResult{Behavior: "deny"})
			case core.EventResult, core.EventError:
				return text.String(), ev
			}
		case <-deadline:
			t.Fatal("turn did not end")
		}
	}
}

func TestLiveKimiACP(t *testing.T) {
	s := liveSession(t, "kimi", "kimi", core.PermissionModePlan)
	text, end := liveTurn(t, s, "Reply with exactly the word PONG and nothing else. Do not use tools.")
	if end.Type != core.EventResult || !strings.Contains(text, "PONG") {
		t.Fatalf("text %q, end %+v", text, end)
	}
	if err := s.Apply(context.Background(), "", "", core.PermissionModeDefault); err != nil {
		t.Fatal(err)
	}
	t.Logf("kimi session %s modes %v", s.CurrentSessionID(), s.Modes())
}

func TestLiveAgyACP(t *testing.T) {
	s := liveSession(t, "antigravity", "agy", core.PermissionModePlan)
	text, end := liveTurn(t, s, "Reply with exactly the word PONG and nothing else. Do not use tools.")
	if end.Type != core.EventResult || !strings.Contains(text, "PONG") {
		t.Fatalf("text %q, end %+v", text, end)
	}
	// Interrupt a longer turn: it ends as interrupted and the session stays.
	if err := s.Send("Count from 1 to 200, one number per line. Do not use tools.", nil, nil); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * time.Second)
	if err := s.Interrupt(context.Background()); err != nil {
		t.Fatal(err)
	}
	for ev := range s.Events() {
		if ev.Type == core.EventResult || ev.Type == core.EventError {
			if ev.Type != core.EventResult || !ev.Interrupted {
				t.Fatalf("end = %+v", ev)
			}
			break
		}
	}
	if !s.Alive() {
		t.Fatal("session ended by the interrupt")
	}
	t.Logf("agy session %s modes %v", s.CurrentSessionID(), s.Modes())
}
