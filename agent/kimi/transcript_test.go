package kimi

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func writeKimiSession(t *testing.T, codeHome, project, id, state, wire string) string {
	t.Helper()
	dir := filepath.Join(codeHome, "sessions", project, id)
	if err := os.MkdirAll(filepath.Join(dir, "agents", "main"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(state), 0o644); err != nil {
		t.Fatal(err)
	}
	if wire != "" {
		if err := os.WriteFile(filepath.Join(dir, "agents", "main", "wire.jsonl"), []byte(wire), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestListSessionsFiltersByCwdAndStripsOptionsHint(t *testing.T) {
	codeHome := t.TempDir()
	workDir := t.TempDir()
	wrapped := core.AppendInteractionOptions("fix the login bug\nsecond line")
	writeKimiSession(t, codeHome, "wd_a", "session_mine",
		`{"id":"session_mine","cwd":"`+workDir+`","updatedAt":1794901216136,"title":""}`,
		`{"type":"context.append_message","message":{"role":"user","content":[{"type":"text","text":`+jsonString(wrapped)+`}],"origin":{"kind":"user"}}}
{"type":"context.append_message","message":{"role":"user","content":[{"type":"text","text":"<system-reminder>auto mode</system-reminder>"}],"origin":{"kind":"injection"}}}
{"type":"context.append_message","message":{"role":"assistant","content":[{"type":"text","text":"done"}]}}
{"type":"context.append_message","message":{"role":"user","content":[{"type":"text","text":"thanks"}],"origin":{"kind":"user"}}}
`)
	writeKimiSession(t, codeHome, "wd_b", "session_other", `{"id":"session_other","cwd":"/somewhere/else"}`, "")
	writeKimiSession(t, codeHome, "wd_a", "session_archived", `{"id":"session_archived","cwd":"`+workDir+`","archived":true}`, "")

	a := &Agent{workDir: workDir, configEnv: []string{"KIMI_CODE_HOME=" + codeHome}}
	sessions, err := a.ListSessions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].ID != "session_mine" {
		t.Fatalf("sessions = %+v", sessions)
	}
	s := sessions[0]
	if s.Summary != "fix the login bug" || s.MessageCount != 2 {
		t.Fatalf("summary/count = %q/%d", s.Summary, s.MessageCount)
	}
	if !s.ModifiedAt.Equal(time.UnixMilli(1794901216136)) {
		t.Fatalf("modified = %v", s.ModifiedAt)
	}
}

func TestHistoryFromWireTranscript(t *testing.T) {
	codeHome := t.TempDir()
	writeKimiSession(t, codeHome, "p", "session_h", `{"id":"session_h","cwd":"/x"}`,
		`{"type":"context.append_message","time":1700000000000,"message":{"role":"user","content":[{"type":"text","text":"  first question  "}],"origin":{"kind":"user"}}}
{"type":"context.append_loop_event","time":1700000000200,"event":{"type":"content.part","turnId":"turn-1","part":{"type":"text","text":"first "}}}
{"type":"context.append_loop_event","time":1700000000300,"event":{"type":"tool.call","turnId":"turn-1","name":"Shell"}}
{"type":"context.append_loop_event","time":1700000000400,"event":{"type":"content.part","turnId":"turn-1","part":{"type":"text","text":"reply"}}}
{"type":"agent.message.appended","time":1700000000401,"message":{"message":{"role":"assistant","content":[{"type":"text","text":"first reply"}]}}}
not-json
{"type":"context.append_message","time":1700000001000,"message":{"role":"user","content":"second question","origin":{"kind":"user"}}}
{"type":"context.append_loop_event","time":1700000001100,"event":{"type":"content.part","turnId":"turn-2","part":{"type":"text","text":"second reply"}}}
`)
	a := &Agent{configEnv: []string{"KIMI_CODE_HOME=" + codeHome}}
	entries, err := a.GetSessionHistory(context.Background(), "session_h", 0)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"user:first question", "assistant:first reply", "user:second question", "assistant:second reply"}
	if len(entries) != len(want) {
		t.Fatalf("entries = %+v", entries)
	}
	for i, e := range entries {
		if e.Role+":"+e.Content != want[i] {
			t.Fatalf("entry %d = %s:%s, want %s", i, e.Role, e.Content, want[i])
		}
	}
	limited, _ := a.GetSessionHistory(context.Background(), "session_h", 2)
	if len(limited) != 2 || limited[0].Content != "second question" {
		t.Fatalf("limited = %+v", limited)
	}
	if _, err := a.GetSessionHistory(context.Background(), "../escape", 0); err == nil {
		t.Fatal("path-like session ids must not resolve")
	}
}

func jsonString(s string) string {
	b := []byte{'"'}
	for _, r := range s {
		switch r {
		case '"':
			b = append(b, '\\', '"')
		case '\\':
			b = append(b, '\\', '\\')
		case '\n':
			b = append(b, '\\', 'n')
		default:
			b = append(b, string(r)...)
		}
	}
	return string(append(b, '"'))
}
