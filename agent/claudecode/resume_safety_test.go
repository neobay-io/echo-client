package claudecode

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/chenhg5/cc-connect/core"
)

const safetyResumeID = "27c523cc-a4c5-45a6-8017-6e8379d35707"

type callbackWriteCloser struct{ write func([]byte) (int, error) }

func (w callbackWriteCloser) Write(b []byte) (int, error) { return w.write(b) }
func (w callbackWriteCloser) Close() error                { return nil }

func safetyClaudeSession() *claudeSession {
	cs := &claudeSession{ctx: context.Background(), events: make(chan core.Event, 8), resumeID: safetyResumeID,
		stdin: callbackWriteCloser{write: func(b []byte) (int, error) { return len(b), nil }}}
	cs.alive.Store(true)
	cs.resuming.Store(true)
	return cs
}

func missingResumeResult() map[string]any {
	return map[string]any{"subtype": "error_during_execution", "is_error": true, "num_turns": float64(0),
		"session_id": safetyResumeID, "errors": []any{"No conversation found with session ID: " + safetyResumeID}}
}

func TestMissingResumeClassificationRequiresMatchingStartup(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*claudeSession, map[string]any)
		want  bool
	}{
		{"matching startup", func(*claudeSession, map[string]any) {}, true},
		{"not resuming", func(cs *claudeSession, _ map[string]any) { cs.resuming.Store(false) }, false},
		{"other result ID", func(_ *claudeSession, r map[string]any) { r["session_id"] = "other" }, false},
		{"other diagnostic ID", func(_ *claudeSession, r map[string]any) {
			r["errors"] = []any{"No conversation found with session ID: other"}
		}, false},
		{"assistant work", func(cs *claudeSession, _ map[string]any) {
			cs.handleAssistant(map[string]any{"message": map[string]any{"content": []any{map[string]any{"type": "tool_use", "name": "Bash"}}}})
		}, false},
		{"tool result", func(cs *claudeSession, _ map[string]any) {
			cs.handleUser(map[string]any{"message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "content": ""}}}})
		}, false},
		{"previous turn completed", func(cs *claudeSession, _ map[string]any) {
			cs.handleResult(map[string]any{"subtype": "success", "num_turns": float64(1), "result": "ok"})
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cs := safetyClaudeSession()
			raw := missingResumeResult()
			tc.setup(cs, raw)
			cs.handleResult(raw)
			var last core.Event
			for len(cs.events) > 0 {
				last = <-cs.events
			}
			if got := errors.Is(last.Error, core.ErrAgentSessionNotFound); got != tc.want {
				t.Fatalf("replayable=%v want=%v error=%v", got, tc.want, last.Error)
			}
			if got := errors.Is(cs.sessionFailure(), core.ErrAgentSessionNotFound); got != tc.want {
				t.Fatalf("cached replayable=%v want=%v", got, tc.want)
			}
		})
	}
}

func TestClaudeSlashPromptDoesNotCompleteOnResumeNotification(t *testing.T) {
	for _, prompt := range []string{"/compact", "/tmp/project/file.go please inspect this path"} {
		t.Run(prompt, func(t *testing.T) {
			cs := safetyClaudeSession()
			if err := cs.Send(prompt, nil, nil); err != nil {
				t.Fatal(err)
			}
			zero := map[string]any{"subtype": "success", "num_turns": float64(0), "result": ""}
			cs.handleResult(zero)
			if len(cs.events) != 0 || !cs.resuming.Load() {
				t.Fatal("slash prompt admitted stale resume result")
			}
			if prompt == "/compact" {
				cs.handleSystem(map[string]any{"subtype": "compact_boundary"})
				cs.handleResult(zero)
			} else {
				cs.handleResult(map[string]any{"subtype": "success", "num_turns": float64(1), "result": "done"})
			}
			if ev := <-cs.events; ev.Type != core.EventResult || !ev.Done {
				t.Fatalf("real completion=%#v", ev)
			}
		})
	}
}

func TestClaudeRequestFailureDoesNotPoisonNextSend(t *testing.T) {
	cs := safetyClaudeSession()
	cs.handleResult(map[string]any{"is_error": true, "num_turns": float64(1), "errors": []any{"Request too large"}})
	if ev := <-cs.events; ev.Type != core.EventError {
		t.Fatal(ev)
	}
	if err := cs.Send("small prompt", nil, nil); err != nil {
		t.Fatalf("request error persisted: %v", err)
	}
	cs.handleResult(map[string]any{"subtype": "success", "num_turns": float64(1), "result": "ok"})
	if ev := <-cs.events; ev.Type != core.EventResult {
		t.Fatal(ev)
	}
	if err := cs.Send("next prompt", nil, nil); err != nil {
		t.Fatal(err)
	}
}

func TestClaudeSendPreservesResumeErrorArrivingDuringWrite(t *testing.T) {
	cs := safetyClaudeSession()
	cs.stdin = callbackWriteCloser{write: func([]byte) (int, error) { cs.handleResult(missingResumeResult()); return 0, io.ErrClosedPipe }}
	if err := cs.Send("review", nil, nil); !errors.Is(err, core.ErrAgentSessionNotFound) {
		t.Fatalf("lost startup error: %v", err)
	}
}
