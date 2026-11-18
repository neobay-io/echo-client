package core

import (
	"context"
	"strings"
	"testing"
	"time"
)

type backgroundSession struct {
	jobTestSession
	h func(BackgroundOutput)
}

func (s *backgroundSession) OnBackgroundOutput(h func(BackgroundOutput)) { s.h = h }

func (s *backgroundSession) handler(text string) { s.h(BackgroundOutput{Text: text}) }

// Output of a turn the agent started by itself is posted to the chat it
// replies to and kept in the session's history.
func TestBackgroundOutputPostedToChat(t *testing.T) {
	p := &stubPlatformEngine{n: "test"}
	e := NewEngine("bg", &jobTestAgent{}, []Platform{p}, "", LangEnglish)
	session := e.sessions.GetOrCreateActive("test:chat:u")
	as := &backgroundSession{jobTestSession: jobTestSession{events: make(chan Event)}}
	state := newInteractiveState(as, p, "ctx", false)
	e.watchBackgroundOutput(state, session)
	if as.h == nil {
		t.Fatal("no handler registered")
	}
	as.handler("  The build finished.  ")
	deadline := time.Now().Add(2 * time.Second)
	for {
		p.sentMu.Lock()
		got := strings.Join(p.sent, "|")
		p.sentMu.Unlock()
		if got == "The build finished." {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("sent = %q", got)
		}
		time.Sleep(10 * time.Millisecond)
	}
	h := session.GetHistory(0)
	if len(h) == 0 || h[len(h)-1].Role != "assistant" || h[len(h)-1].Content != "The build finished." {
		t.Fatalf("history = %+v", h)
	}

	// Output that comes while a turn is running waits until that turn is
	// done, not for a turn started after it.
	turn := make(chan struct{})
	state.mu.Lock()
	state.turnDone = turn
	state.mu.Unlock()
	as.handler("second")
	as.handler("third")
	time.Sleep(100 * time.Millisecond)
	if n := len(p.Sent()); n != 1 {
		t.Fatalf("posted during the turn: %q", p.Sent())
	}
	state.mu.Lock()
	state.turnDone = make(chan struct{}) // the next turn has begun
	state.mu.Unlock()
	close(turn)
	deadline = time.Now().Add(2 * time.Second)
	for len(p.Sent()) < 3 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := strings.Join(p.Sent(), "|"); got != "The build finished.|second|third" {
		t.Fatalf("sent = %q, want in order after the turn", got)
	}

	// A stopped chat state posts nothing.
	state.stop()
	as.handler("late")
	time.Sleep(50 * time.Millisecond)
	if n := len(p.Sent()); n != 3 {
		t.Fatalf("sent after stop: %q", p.Sent())
	}
}

// An agent that returns no model list: /model says so instead of showing
// an empty list.
func TestModelListEmptyExplained(t *testing.T) {
	p := &stubPlatformEngine{n: "test"}
	e := NewEngine("m", &noModelsAgent{}, []Platform{p}, "", LangEnglish)
	e.cmdModel(p, &Message{SessionKey: "test:chat:u", ReplyCtx: "ctx"}, nil)
	if got := strings.Join(p.Sent(), "\n"); !strings.Contains(got, "did not return its model list") {
		t.Fatalf("reply = %q", got)
	}
}

// noModelsAgent has no model set and lists none.
type noModelsAgent struct{ stubModelModeAgent }

func (a *noModelsAgent) GetModel() string { return "" }

// With a current model set but no catalog, /model still says the list was
// not loaded (the current model alone is not a catalog).
func TestModelListEmptyExplainedWithCurrentModel(t *testing.T) {
	p := &stubPlatformEngine{n: "test"}
	e := NewEngine("m2", &stubModelModeAgent{}, []Platform{p}, "", LangEnglish)
	e.cmdModel(p, &Message{SessionKey: "test:chat:u", ReplyCtx: "ctx"}, nil)
	if got := strings.Join(p.Sent(), "\n"); !strings.Contains(got, "did not return its model list") || !strings.Contains(got, "model-a") {
		t.Fatalf("reply = %q", got)
	}
}

type exclusiveAgent struct {
	jobTestAgent
	exclusive bool
}

func (a *exclusiveAgent) SessionsAreExclusive() bool { return a.exclusive }

type closeCountingSession struct {
	jobTestSession
	id     string
	closed bool
}

func (s *closeCountingSession) CurrentSessionID() string { return s.id }
func (s *closeCountingSession) Close() error             { s.closed = true; return nil }

// A cron run resuming a chat's session closes the chat's idle process only
// for agents whose sessions are exclusive.
func TestReleaseChatProcessesForExclusiveAgents(t *testing.T) {
	for _, exclusive := range []bool{true, false} {
		e := NewEngine("x", &exclusiveAgent{exclusive: exclusive}, nil, "", LangEnglish)
		held := &closeCountingSession{jobTestSession: jobTestSession{events: make(chan Event)}, id: "thr"}
		other := &closeCountingSession{jobTestSession: jobTestSession{events: make(chan Event)}, id: "other"}
		e.interactiveStates["chat:a"] = newInteractiveState(held, nil, nil, false)
		e.interactiveStates["chat:b"] = newInteractiveState(other, nil, nil, false)
		e.releaseChatProcesses("thr")
		_, stillA := e.interactiveStates["chat:a"]
		if exclusive && (stillA || !held.closed) {
			t.Fatalf("exclusive: chat process kept (closed=%v)", held.closed)
		}
		if !exclusive && (!stillA || held.closed) {
			t.Fatal("shared sessions: chat process closed")
		}
		if _, ok := e.interactiveStates["chat:b"]; !ok || other.closed {
			t.Fatal("another session's process was closed")
		}
	}
}

type bgEventAgent struct{ stubAgent }

func (a *bgEventAgent) StartSession(context.Context, string) (AgentSession, error) {
	return &scriptedAgentSession{events: make(chan Event, 4), queue: []Event{
		{Type: EventBackground, Content: "The earlier build finished."},
		{Type: EventResult, Content: "Here is the answer.", Done: true},
	}}, nil
}

// Output the agent produced by itself while the turn's prompt waited is
// posted as its own message, before the turn's answer, and kept in history.
func TestBackgroundEventPostedBeforeAnswer(t *testing.T) {
	p := &stubPlatformEngine{n: "test"}
	e := NewEngine("bgev", &bgEventAgent{}, []Platform{p}, "", LangEnglish)
	e.handleMessage(p, &Message{SessionKey: "test:chat:u", Platform: "test", UserID: "u", Content: "question", ReplyCtx: "ctx"})
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := strings.Join(p.Sent(), "|")
		if strings.Contains(got, "Here is the answer.") {
			if i, j := strings.Index(got, "The earlier build finished."), strings.Index(got, "Here is the answer."); i < 0 || i > j {
				t.Fatalf("sent = %q, want the background text first", got)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("sent = %q", got)
		}
		time.Sleep(20 * time.Millisecond)
	}
	var roles []string
	for _, h := range e.sessions.GetOrCreateActive("test:chat:u").GetHistory(0) {
		roles = append(roles, h.Role+":"+h.Content)
	}
	if got := strings.Join(roles, "|"); !strings.Contains(got, "assistant:The earlier build finished.|assistant:Here is the answer.") {
		t.Fatalf("history = %q", got)
	}
}

// The same in a managed turn (relay / cron in chat).
func TestBackgroundEventInManagedTurn(t *testing.T) {
	p := &stubPlatformEngine{n: "test"}
	e := NewEngine("bgm", &stubAgent{}, []Platform{p}, "", LangEnglish)
	session := e.sessions.GetOrCreateActive("test:m")
	state := newInteractiveState(&scriptedAgentSession{events: make(chan Event, 4), queue: []Event{
		{Type: EventBackground, Content: "earlier"},
		{Type: EventResult, Content: "final", Done: true},
	}}, p, "ctx", false)
	got, err := e.runManagedTurn(state, session, "test:m", "prompt", managedTurnOpts{})
	if err != nil || got != "final" {
		t.Fatalf("runManagedTurn = %q, %v", got, err)
	}
	if !strings.Contains(strings.Join(p.Sent(), "|"), "earlier") {
		t.Fatalf("sent = %q", p.Sent())
	}
}
