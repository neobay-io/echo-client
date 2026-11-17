//go:build live

package antigravity

// Live checks against the installed agy CLI and account:
//
//	go test -tags live -run Live -v ./agent/antigravity/
//
// They spend a few model turns on a fast model.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

const liveModel = "gemini-3.8-flash"

func liveTurn(t *testing.T, s core.AgentSession, prompt string, onPermission func(core.Event) core.PermissionResult) (string, core.Event, []core.Event) {
	t.Helper()
	if err := s.Send(prompt, nil, nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	var text strings.Builder
	var asked []core.Event
	timeout := time.After(4 * time.Minute)
	for {
		select {
		case evt := <-s.Events():
			switch evt.Type {
			case core.EventText:
				text.WriteString(evt.Content)
			case core.EventPermissionRequest:
				asked = append(asked, evt)
				result := core.PermissionResult{Behavior: "allow"}
				if onPermission != nil {
					result = onPermission(evt)
				}
				if err := s.RespondPermission(evt.RequestID, result); err != nil {
					t.Fatalf("RespondPermission: %v", err)
				}
			case core.EventError:
				t.Fatalf("turn error: %v", evt.Error)
			case core.EventResult:
				return text.String(), evt, asked
			}
		case <-timeout:
			t.Fatal("turn timed out")
		}
	}
}

func TestLiveAgyApprovalsModesResumeAndListing(t *testing.T) {
	workDir := t.TempDir()
	agent, err := New(map[string]any{"work_dir": workDir, "model": liveModel, "reasoning_level": "low"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	a := agent.(*Agent)
	models := a.AvailableModels(context.Background())
	if len(models) == 0 {
		t.Fatal("no models discovered")
	}
	t.Logf("%d models, e.g. %+v; efforts %+v", len(models), models[0], a.AvailableReasoningLevels())

	// Default mode: the write is asked in chat; denying it keeps the file away.
	s1, err := a.StartSession(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	reply, res, asked := liveTurn(t, s1, "Remember the codeword ORCHID-42. Then create a file named a.txt containing ALPHA using your file tool, and report what happened.",
		func(evt core.Event) core.PermissionResult {
			return core.PermissionResult{Behavior: "deny", Message: "not now"}
		})
	_ = s1.Close()
	conv := res.SessionID
	t.Logf("default mode: conversation=%s asked=%d reply=%q", conv, len(asked), reply)
	if conv == "" || len(asked) == 0 {
		t.Fatalf("expected an approval request and a conversation id")
	}
	if _, err := os.Stat(filepath.Join(workDir, "a.txt")); err == nil {
		t.Fatal("a.txt written although the approval was denied")
	}

	// acceptEdits, resumed, no approval needed.
	a.SetSessionOptions(core.AgentSessionOptions{Mode: "acceptEdits"})
	s2, err := a.StartSession(context.Background(), conv)
	if err != nil {
		t.Fatal(err)
	}
	reply, _, asked = liveTurn(t, s2, "Create a file named b.txt containing BETA using your file tool. Reply done.", nil)
	_ = s2.Close()
	t.Logf("acceptEdits: asked=%d reply=%q", len(asked), reply)
	if data, err := os.ReadFile(filepath.Join(workDir, "b.txt")); err != nil || !strings.Contains(string(data), "BETA") {
		t.Fatalf("b.txt not written: %q %v", data, err)
	}

	// Resume with another model and effort; the context survives.
	a.SetSessionOptions(core.AgentSessionOptions{Mode: "bypassPermissions", Model: "gemini-4-argon", ReasoningLevel: "low"})
	s3, err := a.StartSession(context.Background(), conv)
	if err != nil {
		t.Fatal(err)
	}
	reply, _, _ = liveTurn(t, s3, "What codeword did I ask you to remember? Reply with the codeword only.", nil)
	_ = s3.Close()
	t.Logf("model switch: %q", reply)
	if !strings.Contains(reply, "ORCHID-42") {
		t.Fatalf("context lost: %q", reply)
	}

	sessions, err := a.ListSessions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range sessions {
		if s.ID == conv {
			found = true
			t.Logf("listed: %+v", s)
			if strings.HasPrefix(s.Summary, "If you need") {
				t.Fatalf("summary shows the options hint: %q", s.Summary)
			}
		}
	}
	if !found {
		t.Fatalf("conversation %s not listed: %+v", conv, sessions)
	}
}
