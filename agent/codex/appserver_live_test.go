//go:build live

package codex

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// One real turn through `codex app-server` (uses a little Codex quota):
// go test -tags live -run TestAppServerLiveTurn ./agent/codex
func TestAppServerLiveTurn(t *testing.T) {
	raw, err := New(map[string]any{"work_dir": t.TempDir(), "reasoning_level": "low"})
	if err != nil {
		t.Fatal(err)
	}
	a := raw.(*Agent)
	sess, err := a.StartSession(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sess.Close() }()
	if _, ok := sess.(*appServerSession); !ok {
		t.Fatalf("session = %T", sess)
	}
	if err := sess.Send("Reply with exactly the word PONG and nothing else.", nil, nil); err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	timeout := time.After(2 * time.Minute)
	for {
		select {
		case ev := <-sess.Events():
			switch ev.Type {
			case core.EventText:
				text.WriteString(ev.Content)
			case core.EventError:
				t.Fatal(ev.Error)
			case core.EventResult:
				if !strings.Contains(text.String(), "PONG") || ev.SessionID == "" {
					t.Fatalf("text = %q, thread = %q", text.String(), ev.SessionID)
				}
				usage, err := a.Usage(context.Background(), sess)
				if err != nil || len(usage.Lines) == 0 {
					t.Fatalf("usage = %#v, %v", usage, err)
				}
				t.Logf("thread %s, usage %+v", ev.SessionID, usage.Lines)
				return
			}
		case <-timeout:
			t.Fatalf("no result; text so far %q", text.String())
		}
	}
}
