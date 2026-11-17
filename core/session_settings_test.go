package core

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// optionsAgent implements every switcher plus SessionOptionsInjector and
// records what each StartSession received.
type optionsAgent struct {
	mu      sync.Mutex
	model   string
	level   string
	mode    string
	pending AgentSessionOptions
	starts  []optionsStart
	deleted []string
}

type optionsStart struct {
	sessionID string
	opts      AgentSessionOptions
}

func (a *optionsAgent) Name() string { return "codex" }
func (a *optionsAgent) StartSession(_ context.Context, sessionID string) (AgentSession, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.starts = append(a.starts, optionsStart{sessionID: sessionID, opts: a.pending})
	return &stubAgentSession{}, nil
}
func (a *optionsAgent) ListSessions(context.Context) ([]AgentSessionInfo, error) { return nil, nil }
func (a *optionsAgent) Stop() error                                              { return nil }
func (a *optionsAgent) SetSessionOptions(opts AgentSessionOptions) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.pending = opts
}
func (a *optionsAgent) SetModel(m string) { a.model = m }
func (a *optionsAgent) GetModel() string  { return a.model }
func (a *optionsAgent) AvailableModels(context.Context) []ModelOption {
	return []ModelOption{{Name: "gpt-a"}, {Name: "gpt-b"}}
}
func (a *optionsAgent) SetReasoningLevel(l string) { a.level = l }
func (a *optionsAgent) GetReasoningLevel() string  { return a.level }
func (a *optionsAgent) AvailableReasoningLevels() []ReasoningLevelOption {
	return []ReasoningLevelOption{{Name: "low"}, {Name: "medium"}, {Name: "high"}}
}
func (a *optionsAgent) SetMode(m string) { a.mode = m }
func (a *optionsAgent) GetMode() string  { return a.mode }
func (a *optionsAgent) PermissionModes() []PermissionModeInfo {
	return []PermissionModeInfo{
		{Key: "default", Name: "Default"},
		{Key: "acceptEdits", Name: "Accept Edits"},
		{Key: "auto", Name: "Auto"},
		{Key: "bypassPermissions", Name: "YOLO"},
	}
}
func (a *optionsAgent) DeleteSession(_ context.Context, id string) error {
	a.deleted = append(a.deleted, id)
	return nil
}

func (a *optionsAgent) lastStart(t *testing.T) optionsStart {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.starts) == 0 {
		t.Fatal("no session was started")
	}
	return a.starts[len(a.starts)-1]
}

func TestSettingsCommandsKeepContextAndStayInTheirChat(t *testing.T) {
	agent := &optionsAgent{model: "project-model", level: "medium", mode: "bypassPermissions"}
	p := &stubPlatformEngine{n: "test"}
	e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)

	chatA := e.sessions.GetOrCreateActive("test:a")
	chatA.AgentSessionID = "agent-a"
	chatA.AddHistory("user", "remember 42")
	e.sessions.GetOrCreateActive("test:b")

	msgA := &Message{SessionKey: "test:a", ReplyCtx: "ctx"}
	e.cmdModel(p, msgA, []string{"gpt-b"})
	e.cmdEffort(p, msgA, []string{"high"})
	e.cmdMode(p, msgA, []string{"yolo"}) // legacy alias → canonical key
	e.cmdMode(p, msgA, []string{"acceptEdits"})

	if chatA.AgentSessionID != "agent-a" || chatA.HistoryLen() != 1 {
		t.Fatalf("settings change dropped context: agent session %q, history %d", chatA.AgentSessionID, chatA.HistoryLen())
	}
	want := AgentSessionOptions{Model: "gpt-b", ReasoningLevel: "high", Mode: "acceptEdits"}
	if got := chatA.Settings(); got != want {
		t.Fatalf("chat A settings = %#v, want %#v", got, want)
	}
	if agent.model != "project-model" || agent.level != "medium" || agent.mode != "bypassPermissions" {
		t.Fatalf("project defaults changed: %q %q %q", agent.model, agent.level, agent.mode)
	}

	stateA := e.getOrCreateInteractiveState("test:a", p, "ctx", chatA)
	if stateA.agentSession == nil {
		t.Fatal("chat A session did not start")
	}
	if got := agent.lastStart(t); got.sessionID != "agent-a" || got.opts != want {
		t.Fatalf("chat A start = %#v, want resume agent-a with %#v", got, want)
	}

	chatB := e.sessions.ActiveSession("test:b")
	e.getOrCreateInteractiveState("test:b", p, "ctx", chatB)
	if got := agent.lastStart(t); !got.opts.IsZero() {
		t.Fatalf("chat B inherited chat A's settings: %#v", got.opts)
	}
	if got := e.effectiveModel(chatB); got != "project-model" {
		t.Fatalf("chat B effective model = %q, want project default", got)
	}
}

func TestSettingsChangeRetiresProcessWithoutInterruptingTheTurn(t *testing.T) {
	agent := &optionsAgent{}
	p := &stubPlatformEngine{n: "test"}
	e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)
	session := e.sessions.GetOrCreateActive("test:a")
	state := e.getOrCreateInteractiveState("test:a", p, "ctx", session)

	e.cmdEffort(p, &Message{SessionKey: "test:a", ReplyCtx: "ctx"}, []string{"low"})

	e.interactiveMu.Lock()
	current, ok := e.interactiveStates["test:a"]
	stale := ok && current.stale
	e.interactiveMu.Unlock()
	if !ok || current != state || !stale {
		t.Fatalf("running state was closed or not marked stale (present=%v stale=%v)", ok, stale)
	}
	select {
	case <-state.stopped:
		t.Fatal("running turn was stopped by /effort")
	default:
	}

	next := e.getOrCreateInteractiveState("test:a", p, "ctx", session)
	if next == state {
		t.Fatal("stale process was reused for the next turn")
	}
	if got := agent.lastStart(t).opts.ReasoningLevel; got != "low" {
		t.Fatalf("next start reasoning = %q, want low", got)
	}
}

func TestEchoJobsIgnoreChatSettings(t *testing.T) {
	agent := &optionsAgent{}
	p := &stubPlatformEngine{n: "test"}
	e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)
	e.cmdModel(p, &Message{SessionKey: "test:a", ReplyCtx: "ctx"}, []string{"gpt-b"})

	if _, err := e.StartJobSession(context.Background(), JobRequest{}, "job-1"); err != nil {
		t.Fatalf("StartJobSession: %v", err)
	}
	if got := agent.lastStart(t).opts; !got.IsZero() {
		t.Fatalf("job session got chat overrides %#v", got)
	}
}

func TestModeCommandRejectsUnknownValues(t *testing.T) {
	agent := &optionsAgent{mode: "default"}
	p := &stubPlatformEngine{n: "test"}
	e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)
	msg := &Message{SessionKey: "test:a", ReplyCtx: "ctx"}

	for _, bad := range []string{"plan", "bogus"} { // Codex exec has no plan mode
		e.cmdMode(p, msg, []string{bad})
		if got := e.sessions.GetOrCreateActive("test:a").Settings().Mode; got != "" {
			t.Fatalf("/mode %s stored %q", bad, got)
		}
	}
	if sent := strings.Join(p.sent, "\n"); !strings.Contains(sent, "Unknown permission mode `bogus`") {
		t.Fatalf("reply = %q, want unknown-mode error", sent)
	}
}

func TestLegacyAgentsKeepProjectWideSettings(t *testing.T) {
	agent := &stubModelSwitchAgent{name: "gemini", model: "old"}
	p := &stubPlatformEngine{n: "test"}
	e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)
	s := e.sessions.GetOrCreateActive("test:a")
	s.AgentSessionID = "agent-a"

	e.cmdModel(p, &Message{SessionKey: "test:a", ReplyCtx: "ctx"}, []string{"new"})

	if agent.GetModel() != "new" {
		t.Fatalf("legacy agent model = %q, want new", agent.GetModel())
	}
	if !s.Settings().IsZero() {
		t.Fatalf("legacy agent stored per-session settings %#v", s.Settings())
	}
	if s.AgentSessionID != "agent-a" {
		t.Fatal("/model cleared the agent session")
	}
}

func TestSessionSettingsPersistAndCarryToNewSessions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	sm := NewSessionManager(path)
	first := sm.GetOrCreateActive("chat")
	first.UpdateSettings(func(o *AgentSessionOptions) { o.Model = "m1"; o.Mode = "auto" })
	first.NeedsReplay = true
	sm.Save()

	reloaded := NewSessionManager(path)
	got := reloaded.ActiveSession("chat")
	if got == nil || got.Settings() != (AgentSessionOptions{Model: "m1", Mode: "auto"}) || !got.NeedsReplay {
		t.Fatalf("reloaded session = %#v", got)
	}

	second := reloaded.NewSession("chat", "next")
	if second.Settings() != (AgentSessionOptions{Model: "m1", Mode: "auto"}) {
		t.Fatalf("/new did not inherit settings: %#v", second.Settings())
	}
	if other := reloaded.NewSession("other-chat", "x"); !other.Settings().IsZero() {
		t.Fatalf("a different chat inherited settings: %#v", other.Settings())
	}
}

func TestDeletePurgeRemovesAgentTranscriptOnlyWhenAsked(t *testing.T) {
	agent := &optionsAgent{}
	p := &stubPlatformEngine{n: "test"}
	e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)
	key := "test:a"
	keep := e.sessions.NewSession(key, "keep")
	keep.AgentSessionID = "agent-keep"
	purge := e.sessions.NewSession(key, "purge")
	purge.AgentSessionID = "agent-purge"
	shared := e.sessions.NewSession(key, "shared")
	shared.AgentSessionID = "agent-shared"
	e.sessions.NewSession("test:b", "other").AgentSessionID = "agent-shared"
	e.sessions.NewSession(key, "active")

	msg := &Message{SessionKey: key, ReplyCtx: "ctx"}
	e.cmdDelete(p, msg, []string{keep.ID})
	e.cmdDelete(p, msg, []string{purge.ID, "purge"})
	e.cmdDelete(p, msg, []string{shared.ID, "purge"})

	if len(agent.deleted) != 1 || agent.deleted[0] != "agent-purge" {
		t.Fatalf("agent transcripts deleted = %#v, want only agent-purge", agent.deleted)
	}
	if got := len(e.sessions.ListSessions(key)); got != 1 {
		t.Fatalf("remaining local sessions = %d, want 1 (active)", got)
	}
	if sent := strings.Join(p.sent, "\n"); !strings.Contains(sent, "still used by project test, session") {
		t.Fatalf("shared purge reply missing: %q", sent)
	}
}

func TestDeletePurgeUnsupportedKeepsLocalSession(t *testing.T) {
	p := &stubPlatformEngine{n: "test"}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)
	key := "test:a"
	target := e.sessions.NewSession(key, "target")
	target.AgentSessionID = "agent-x"
	e.sessions.NewSession(key, "active")

	e.cmdDelete(p, &Message{SessionKey: key, ReplyCtx: "ctx"}, []string{target.ID, "purge"})

	if got := len(e.sessions.ListSessions(key)); got != 2 {
		t.Fatalf("local sessions = %d, want 2 (nothing deleted)", got)
	}
}
