package claudecode

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestScanSessionDetail(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	lines := []string{
		`{"type":"queue-operation","sessionId":"x"}`,
		`{"type":"user","cwd":"/Users/e/Projects/foo","message":{"role":"user","content":"first question here"}}`,
		`{"type":"assistant","cwd":"/Users/e/Projects/foo","message":{"role":"assistant","content":[{"type":"text","text":"answer"}]}}`,
		`{"type":"custom-title","customTitle":"My Title","sessionId":"x"}`,
		`{"type":"user","cwd":"/Users/e/Projects/foo","message":{"role":"user","content":"second"}}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cwd, title, summary, count := scanSessionDetail(context.Background(), path)
	if cwd != "/Users/e/Projects/foo" {
		t.Errorf("cwd = %q, want /Users/e/Projects/foo", cwd)
	}
	if title != "My Title" {
		t.Errorf("title = %q, want My Title", title)
	}
	if summary != "first question here" {
		t.Errorf("summary = %q, want first question here", summary)
	}
	if count != 3 { // 2 user + 1 assistant
		t.Errorf("count = %d, want 3", count)
	}
}

func TestScanSessionDetailArrayContentSummary(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	// First user message uses structured (array) content — must still extract text.
	line := `{"type":"user","cwd":"/w","message":{"role":"user","content":[{"type":"text","text":"array question"}]}}`
	if err := os.WriteFile(path, []byte(line+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, summary, _ := scanSessionDetail(context.Background(), path)
	if summary != "array question" {
		t.Errorf("summary = %q, want array question", summary)
	}
}

func TestListAllSessions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	p1 := filepath.Join(home, ".claude", "projects", "-Users-e-Projects-foo")
	p2 := filepath.Join(home, ".claude", "projects", "-Users-e-Projects-bar")
	for _, d := range []string{p1, p2} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(dir, name, cwd, content string) {
		body := `{"type":"user","cwd":"` + cwd + `","message":{"role":"user","content":"` + content + `"}}`
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(p1, "aaaa1111-0000-0000-0000-000000000000.jsonl", "/Users/e/Projects/foo", "hi foo")
	write(p2, "bbbb2222-0000-0000-0000-000000000000.jsonl", "/Users/e/Projects/bar", "hi bar")

	a := &Agent{workDir: "/tmp"}
	got, err := a.ListAllSessions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d sessions, want 2", len(got))
	}

	for _, s := range got {
		if s.WorkDir == "" || s.Path == "" {
			t.Errorf("session %s missing WorkDir/Path: %+v", s.ID, s)
		}
		switch s.ID {
		case "aaaa1111-0000-0000-0000-000000000000":
			if s.WorkDir != "/Users/e/Projects/foo" || s.Summary != "hi foo" {
				t.Errorf("foo session wrong: %+v", s)
			}
		case "bbbb2222-0000-0000-0000-000000000000":
			if s.WorkDir != "/Users/e/Projects/bar" || s.Summary != "hi bar" {
				t.Errorf("bar session wrong: %+v", s)
			}
		default:
			t.Errorf("unexpected session id %q", s.ID)
		}
	}
}

func TestListAllSessionsNoProjectsDir(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // no .claude/projects
	a := &Agent{workDir: "/tmp"}
	got, err := a.ListAllSessions(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d sessions, want 0", len(got))
	}
}

// Medium5: a cancelled context must abort the (potentially slow) scan instead of
// walking every project dir and reading every transcript.
func TestListAllSessionsRespectsCancel(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	p := filepath.Join(home, ".claude", "projects", "-Users-e-Projects-foo")
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"type":"user","cwd":"/w","message":{"role":"user","content":"hi"}}`
	if err := os.WriteFile(filepath.Join(p, "aaaa1111-0000-0000-0000-000000000000.jsonl"), []byte(body+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before the walk starts
	a := &Agent{workDir: "/tmp"}
	if _, err := a.ListAllSessions(ctx); err == nil {
		t.Fatal("expected error from cancelled context, got nil")
	}
}
