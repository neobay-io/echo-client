package core

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
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

// heldSession is a minimal AgentSession whose liveness and session id are
// configurable, for testing attach's self-exclusion (High1).
type heldSession struct {
	sid   string
	alive bool
}

func (h *heldSession) Send(string, []ImageAttachment, []FileAttachment) error { return nil }
func (h *heldSession) RespondPermission(string, PermissionResult) error       { return nil }
func (h *heldSession) Events() <-chan Event                                   { return nil }
func (h *heldSession) CurrentSessionID() string                               { return h.sid }
func (h *heldSession) Alive() bool                                            { return h.alive }
func (h *heldSession) Close() error                                           { return nil }

// High1: otherChatHolding must find a live sibling chat on the same session,
// skip the querying chat, dead sessions, and the empty id.
func TestOtherChatHolding(t *testing.T) {
	e := newAttachEngine(t, &attachTestAgent{})
	e.interactiveStates["other:chat"] = &interactiveState{agentSession: &heldSession{sid: "target-id", alive: true}}
	e.interactiveStates["dead:chat"] = &interactiveState{agentSession: &heldSession{sid: "dead-id", alive: false}}

	if got := e.otherChatHolding("target-id", "me:chat"); got != "other:chat" {
		t.Errorf("otherChatHolding = %q, want other:chat", got)
	}
	if got := e.otherChatHolding("target-id", "other:chat"); got != "" {
		t.Errorf("otherChatHolding must exclude exceptKey, got %q", got)
	}
	if got := e.otherChatHolding("dead-id", "me:chat"); got != "" {
		t.Errorf("dead (not Alive) session must not hold, got %q", got)
	}
	if got := e.otherChatHolding("", "me:chat"); got != "" {
		t.Errorf("empty id must not hold, got %q", got)
	}
}

// High1: attach must refuse (and not switch work_dir / bind) when another live
// chat in this bot is using the target session.
func TestAttachSessionByID_HeldByOtherChat(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	wd := t.TempDir()
	a := &attachTestAgent{all: []AgentSessionInfo{{ID: "id1", WorkDir: wd, Path: filepath.Join(t.TempDir(), "nope.jsonl")}}}
	e := newAttachEngine(t, a)
	e.interactiveStates["other:u"] = &interactiveState{agentSession: &heldSession{sid: "id1", alive: true}}

	got := e.attachSessionByID("test:u", "id1", false)
	if !strings.Contains(got, "another chat") {
		t.Errorf("expected held-by-chat refusal, got %q", got)
	}
	if a.GetWorkDir() == wd {
		t.Error("work_dir must not switch when attach is refused")
	}
	if bound := e.sessions.GetOrCreateActive("test:u").AgentSessionID; bound == "id1" {
		t.Error("session must not bind when attach is refused")
	}
}

// High2: waitProcessGone reports a dead pid gone at once and a live pid still
// present after the timeout.
func TestWaitProcessGone(t *testing.T) {
	if !waitProcessGone(999999999, 500*time.Millisecond) {
		t.Error("nonexistent pid should be reported gone")
	}
	if waitProcessGone(os.Getpid(), 200*time.Millisecond) {
		t.Error("live process should not be reported gone within timeout")
	}
}

// Medium4: an id prefix matching more than one session is ambiguous and refused;
// a unique prefix or exact id resolves; no match is empty (not ambiguous).
func TestResolveAttachID(t *testing.T) {
	a := &attachTestAgent{all: []AgentSessionInfo{
		{ID: "abc11111-0000-0000-0000-000000000000"},
		{ID: "abc22222-0000-0000-0000-000000000000"},
		{ID: "xyz99999-0000-0000-0000-000000000000"},
	}}
	e := newAttachEngine(t, a)

	if id, ambiguous := e.resolveAttachID("abc"); !ambiguous || id != "" {
		t.Errorf("resolveAttachID(abc) = (%q,%v), want ambiguous", id, ambiguous)
	}
	if id, ambiguous := e.resolveAttachID("xyz"); ambiguous || id != "xyz99999-0000-0000-0000-000000000000" {
		t.Errorf("resolveAttachID(xyz) = (%q,%v), want unique xyz", id, ambiguous)
	}
	full := "abc11111-0000-0000-0000-000000000000"
	if id, ambiguous := e.resolveAttachID(full); ambiguous || id != full {
		t.Errorf("resolveAttachID(exact) = (%q,%v), want exact", id, ambiguous)
	}
	if id, ambiguous := e.resolveAttachID("zzz"); ambiguous || id != "" {
		t.Errorf("resolveAttachID(zzz) = (%q,%v), want empty", id, ambiguous)
	}
}
