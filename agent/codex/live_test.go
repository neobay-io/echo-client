//go:build live

// Live checks against the real Codex CLI and account. Not part of the default
// test run: go test -tags live -run TestLive ./agent/codex/ -v -timeout 20m

package codex

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func liveTurn(t *testing.T, s core.AgentSession, prompt string) (string, error) {
	t.Helper()
	if err := s.Send(prompt, nil, nil); err != nil {
		return "", err
	}
	var text strings.Builder
	timeout := time.After(5 * time.Minute)
	for {
		select {
		case evt := <-s.Events():
			switch evt.Type {
			case core.EventPermissionRequest:
				t.Fatalf("unexpected permission request for %s", evt.ToolName)
			case core.EventText:
				text.WriteString(evt.Content)
			case core.EventError:
				return text.String(), evt.Error
			case core.EventResult:
				text.WriteString(evt.Content)
				if evt.Done {
					return text.String(), nil
				}
			}
		case <-timeout:
			t.Fatal("turn timed out")
		}
	}
}

// Each mode runs a new thread and a resumed one (exec resume rejected the old
// --full-auto flag), asking Codex to write inside its workspace.
func TestLiveCodexModesWriteOnNewAndResumedThreads(t *testing.T) {
	// default passes no overrides, so whether it may write depends on the
	// user's ~/.codex/config.toml; only check that both turns complete.
	cases := []struct {
		mode       string
		checkWrite bool
	}{
		{"acceptEdits", true},
		{"auto", true},
		{"default", false},
	}
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			workDir := t.TempDir()
			a, err := New(map[string]any{"work_dir": workDir, "mode": tc.mode})
			if err != nil {
				t.Fatal(err)
			}
			agent := a.(*Agent)
			agent.SetSessionOptions(core.AgentSessionOptions{ReasoningLevel: "low"})
			s, err := agent.StartSession(context.Background(), "")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = s.Close() }()

			first := filepath.Join(workDir, "first.txt")
			out, err := liveTurn(t, s, "Create a file named first.txt in the current directory containing exactly the text: one. Do not ask questions. Then reply DONE.")
			t.Logf("turn 1 (%s): err=%v reply=%q", tc.mode, err, liveSnippet(out))
			if err != nil {
				t.Fatalf("turn 1 failed: %v", err)
			}
			threadID := s.CurrentSessionID()
			if threadID == "" {
				t.Fatal("no thread id after first turn")
			}
			if _, statErr := os.Stat(first); tc.checkWrite && statErr != nil {
				t.Fatalf("first.txt not written in %s mode", tc.mode)
			}

			second := filepath.Join(workDir, "second.txt")
			out, err = liveTurn(t, s, "Now create a file named second.txt in the current directory containing exactly the text: two. Do not ask questions. Then reply DONE.")
			t.Logf("turn 2 (%s, resumed %s): err=%v reply=%q", tc.mode, threadID, err, liveSnippet(out))
			if err != nil {
				t.Fatalf("resumed turn failed: %v", err)
			}
			if _, statErr := os.Stat(second); tc.checkWrite && statErr != nil {
				t.Fatalf("second.txt not written in %s mode (resumed thread)", tc.mode)
			}
		})
	}
}

func liveSnippet(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 160 {
		return s[:160] + "…"
	}
	return s
}
