package core

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// attachTestAgent implements Agent plus the optional AllSessionsLister,
// WorkDirSwitcher and HistoryProvider interfaces used by /attach.
type attachTestAgent struct {
	all     []AgentSessionInfo
	history []HistoryEntry
	workDir string
}

func (a *attachTestAgent) Name() string { return "attach-test" }
func (a *attachTestAgent) StartSession(context.Context, string) (AgentSession, error) {
	return nil, nil
}
func (a *attachTestAgent) ListSessions(context.Context) ([]AgentSessionInfo, error) {
	return a.all, nil
}
func (a *attachTestAgent) Stop() error { return nil }
func (a *attachTestAgent) ListAllSessions(context.Context) ([]AgentSessionInfo, error) {
	return a.all, nil
}
func (a *attachTestAgent) SetWorkDir(dir string) { a.workDir = dir }
func (a *attachTestAgent) GetWorkDir() string    { return a.workDir }
func (a *attachTestAgent) GetSessionHistory(context.Context, string, int) ([]HistoryEntry, error) {
	return a.history, nil
}

func newAttachEngine(t *testing.T, agent Agent) *Engine {
	t.Helper()
	return NewEngine("attach-test", agent, []Platform{&stubPlatformEngine{n: "test"}}, "", LangEnglish)
}

func TestAttachSessionByID_NotFound(t *testing.T) {
	a := &attachTestAgent{all: []AgentSessionInfo{{ID: "real-id", WorkDir: "/x"}}}
	e := newAttachEngine(t, a)
	got := e.attachSessionByID("test:u", "missing", false)
	if !strings.Contains(got, "missing") {
		t.Errorf("expected no-match message mentioning query, got %q", got)
	}
}

func TestAttachSessionByID_NoWorkDir(t *testing.T) {
	a := &attachTestAgent{all: []AgentSessionInfo{{ID: "id1", WorkDir: ""}}}
	e := newAttachEngine(t, a)
	got := e.attachSessionByID("test:u", "id1", false)
	if !strings.Contains(got, "working directory") {
		t.Errorf("expected no-workdir message, got %q", got)
	}
}

func TestAttachSessionByID_AlreadyCurrent(t *testing.T) {
	a := &attachTestAgent{all: []AgentSessionInfo{{ID: "id1", WorkDir: "/x"}}}
	e := newAttachEngine(t, a)
	e.sessions.GetOrCreateActive("test:u").AgentSessionID = "id1"
	got := e.attachSessionByID("test:u", "id1", false)
	if !strings.Contains(got, "already") {
		t.Errorf("expected already-current message, got %q", got)
	}
}

func TestAttachSessionByID_Success(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // isolate holder scan: empty ~/.claude/sessions
	wd := t.TempDir()
	a := &attachTestAgent{
		all: []AgentSessionInfo{{
			ID:      "id-success",
			WorkDir: wd,
			Path:    filepath.Join(t.TempDir(), "nope.jsonl"), // absent → no lsof holder
			Title:   "My Session",
		}},
		history: []HistoryEntry{
			{Role: "user", Content: "latest question"},
			{Role: "assistant", Content: "latest answer"},
		},
	}
	e := newAttachEngine(t, a)

	got := e.attachSessionByID("test:u", "id-success", false)

	if a.GetWorkDir() != wd {
		t.Errorf("work_dir not switched: got %q, want %q", a.GetWorkDir(), wd)
	}
	if bound := e.sessions.GetOrCreateActive("test:u").AgentSessionID; bound != "id-success" {
		t.Errorf("session not bound: got %q", bound)
	}
	if !strings.Contains(got, "My Session") {
		t.Errorf("reply missing session name: %q", got)
	}
	if !strings.Contains(got, "latest question") || !strings.Contains(got, "latest answer") {
		t.Errorf("reply missing recent context: %q", got)
	}
}

func TestScanActiveHolders(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	sdir := filepath.Join(home, ".claude", "sessions")
	if err := os.MkdirAll(sdir, 0o755); err != nil {
		t.Fatal(err)
	}
	self := os.Getpid()
	if err := os.WriteFile(filepath.Join(sdir, strconv.Itoa(self)+".json"),
		[]byte(fmt.Sprintf(`{"pid":%d,"sessionId":"live-sess","cwd":"/x","entrypoint":"sdk-cli"}`, self)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sdir, "999999999.json"),
		[]byte(`{"pid":999999999,"sessionId":"dead-sess","cwd":"/y"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	holders := scanActiveHolders()
	if _, ok := holders["live-sess"]; !ok {
		t.Error("live session should be present")
	}
	if _, ok := holders["dead-sess"]; ok {
		t.Error("dead session (nonexistent pid) should be skipped")
	}
}

func TestProcessAlive(t *testing.T) {
	if !processAlive(os.Getpid()) {
		t.Error("current process should be alive")
	}
	if processAlive(999999999) {
		t.Error("nonexistent pid should not be alive")
	}
	if processAlive(0) {
		t.Error("pid 0 should not be considered alive")
	}
}

func TestAttachDisplayName(t *testing.T) {
	cases := []struct {
		in   AgentSessionInfo
		want string
	}{
		{AgentSessionInfo{Title: "My Title", Summary: "sum"}, "My Title"},
		{AgentSessionInfo{Summary: "just summary"}, "just summary"},
		{AgentSessionInfo{}, "(empty)"},
		{AgentSessionInfo{Title: "multi\nline  text"}, "multi line text"},
	}
	for _, c := range cases {
		if got := attachDisplayName(c.in); got != c.want {
			t.Errorf("attachDisplayName(%+v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestShortenHome(t *testing.T) {
	t.Setenv("HOME", "/home/x")
	if got := shortenHome("/home/x/projects/foo"); got != "~/projects/foo" {
		t.Errorf("got %q, want ~/projects/foo", got)
	}
	if got := shortenHome("/other/path"); got != "/other/path" {
		t.Errorf("got %q, want /other/path", got)
	}
	if got := shortenHome(""); got != "?" {
		t.Errorf("got %q, want ?", got)
	}
}

func TestParseAttachCardPage(t *testing.T) {
	cases := map[string]int{
		"":               1,
		"2":              2,
		"do id-x 3":      3,
		"force id-x 5":   5,
		"garbage":        1,
		"do id-x notnum": 1,
	}
	for in, want := range cases {
		if got := parseAttachCardPage(in); got != want {
			t.Errorf("parseAttachCardPage(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestShortID(t *testing.T) {
	if got := shortID("aaaabbbbcccc"); got != "aaaabbbb" {
		t.Errorf("got %q, want aaaabbbb", got)
	}
	if got := shortID("short"); got != "short" {
		t.Errorf("got %q, want short", got)
	}
}
