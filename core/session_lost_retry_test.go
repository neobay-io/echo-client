package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func lostSessionAgent(second []Event) *recoveryTestAgent {
	missing := fmt.Errorf("%w: No conversation found with session ID: old", ErrAgentSessionNotFound)
	return &recoveryTestAgent{turns: [][]Event{{{Type: EventError, Error: missing}}, second}}
}

func historyText(s *Session) string {
	var out []string
	for _, h := range s.GetHistory(0) {
		out = append(out, h.Role+":"+h.Content)
	}
	return strings.Join(out, "|")
}

// A chat message that finds the agent's session gone before anything ran is
// sent again, once, in a new session that gets the chat's history first. The
// history keeps the user's own text, not the replayed prompt.
func TestChatMessageRetriedWhenAgentSessionLost(t *testing.T) {
	agent := lostSessionAgent([]Event{{Type: EventResult, Content: "fresh answer", Done: true}})
	p := &stubPlatformEngine{n: "test"}
	e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)
	session := e.sessions.GetOrCreateActive("test:chat")
	session.AgentSessionID = "old"
	session.AddHistory("user", "earlier question")
	session.AddHistory("assistant", "earlier answer")

	e.handleMessage(p, &Message{SessionKey: "test:chat", Platform: "test", UserID: "u", Content: "new question", ReplyCtx: "ctx"})
	waitForCondition(t, func() bool {
		return strings.Contains(strings.Join(p.Sent(), "|"), "fresh answer")
	}, "answer from the new session")

	sent := strings.Join(p.Sent(), "|")
	if !strings.Contains(sent, "Sending your message again in a new session") || strings.Contains(sent, "Error") {
		t.Fatalf("sent = %q", sent)
	}
	if len(agent.resumeIDs) != 2 || agent.resumeIDs[0] != "old" || agent.resumeIDs[1] != "" {
		t.Fatalf("resume ids = %q", agent.resumeIDs)
	}
	prompt := agent.sessions[1].prompt
	if !strings.HasPrefix(prompt, replayHeader) || !strings.Contains(prompt, "User: earlier question\nAssistant: earlier answer") ||
		!strings.HasSuffix(prompt, replayFooter+"new question") || strings.Count(prompt, "new question") != 1 {
		t.Fatalf("retried prompt = %q", prompt)
	}
	waitForCondition(t, func() bool { return strings.HasSuffix(historyText(session), "assistant:fresh answer") }, "answer in history")
	if got, want := historyText(session), "user:earlier question|assistant:earlier answer|user:new question|assistant:fresh answer"; got != want {
		t.Fatalf("history = %q, want %q", got, want)
	}
	if session.NeedsReplay {
		t.Fatal("replay still pending after the retried turn")
	}
}

// When the new session fails the same way, the error is shown; no third try.
func TestChatMessageRetriedOnlyOnce(t *testing.T) {
	missing := fmt.Errorf("%w: gone again", ErrAgentSessionNotFound)
	agent := lostSessionAgent([]Event{{Type: EventError, Error: missing}})
	p := &stubPlatformEngine{n: "test"}
	e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)
	session := e.sessions.GetOrCreateActive("test:chat")
	session.AgentSessionID = "old"

	e.handleMessage(p, &Message{SessionKey: "test:chat", Platform: "test", UserID: "u", Content: "question", ReplyCtx: "ctx"})
	waitForCondition(t, func() bool { return strings.Contains(strings.Join(p.Sent(), "|"), "gone again") }, "error after the retry")
	waitForCondition(t, func() bool {
		session.mu.Lock()
		defer session.mu.Unlock()
		return !session.busy
	}, "turn released")
	if len(agent.resumeIDs) != 2 {
		t.Fatalf("resume ids = %q", agent.resumeIDs)
	}
	if got := historyText(session); got != "user:question" {
		t.Fatalf("history = %q", got)
	}
}

// A missing session reported after the turn showed output is not retried:
// the agent already acted on the message.
func TestLostSessionAfterOutputNotRetried(t *testing.T) {
	missing := fmt.Errorf("%w: late", ErrAgentSessionNotFound)
	agent := &recoveryTestAgent{turns: [][]Event{{{Type: EventText, Content: "partial"}, {Type: EventError, Error: missing}}}}
	p := &stubPlatformEngine{n: "test"}
	e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)
	session := e.sessions.GetOrCreateActive("test:chat")
	session.AgentSessionID = "old"

	e.handleMessage(p, &Message{SessionKey: "test:chat", Platform: "test", UserID: "u", Content: "question", ReplyCtx: "ctx"})
	waitForCondition(t, func() bool { return strings.Contains(strings.Join(p.Sent(), "|"), "late") }, "error shown")
	if len(agent.resumeIDs) != 1 {
		t.Fatalf("resume ids = %q", agent.resumeIDs)
	}
}

// The same retry for a turn started on the async path (/collect send).
func TestAsyncTurnRetriedWhenAgentSessionLost(t *testing.T) {
	agent := lostSessionAgent([]Event{{Type: EventResult, Content: "fresh answer", Done: true}})
	p := &stubPlatformEngine{n: "test"}
	e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)
	session := e.sessions.GetOrCreateActive("test:chat")
	session.AgentSessionID = "old"
	if !session.TryLock() {
		t.Fatal("session busy")
	}
	started := make(chan error, 1)
	e.processInteractiveMessageAsyncWithQueueKey(p, &Message{SessionKey: "test:chat", Platform: "test", Content: "collected", ReplyCtx: "ctx"}, session, "", func(err error) { started <- err })
	if err := <-started; err != nil {
		t.Fatal(err)
	}
	waitForCondition(t, func() bool { return strings.Contains(strings.Join(p.Sent(), "|"), "fresh answer") }, "answer from the new session")
	waitForCondition(t, func() bool { return historyText(session) == "user:collected|assistant:fresh answer" }, "history")
}

// startNotFoundAgent cannot resume "old" (as Codex and the ACP agents report
// at start); a new session takes the prompt.
type startNotFoundAgent struct {
	recoveryTestAgent
}

func (a *startNotFoundAgent) StartSession(ctx context.Context, id string) (AgentSession, error) {
	if id == "old" {
		a.resumeIDs = append(a.resumeIDs, id)
		return nil, fmt.Errorf("%w: thread not found", ErrAgentSessionNotFound)
	}
	return a.recoveryTestAgent.StartSession(ctx, id)
}

// A session found missing while starting the chat's process still gets the
// history replay in the same turn's prompt.
func TestReplayWhenSessionMissingAtStart(t *testing.T) {
	agent := &startNotFoundAgent{recoveryTestAgent{turns: [][]Event{nil, {{Type: EventResult, Content: "ok", Done: true}}}}}
	p := &stubPlatformEngine{n: "test"}
	e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)
	session := e.sessions.GetOrCreateActive("test:chat")
	session.AgentSessionID = "old"
	session.AddHistory("user", "earlier question")

	e.handleMessage(p, &Message{SessionKey: "test:chat", Platform: "test", UserID: "u", Content: "next", ReplyCtx: "ctx"})
	waitForCondition(t, func() bool { return strings.Contains(strings.Join(p.Sent(), "|"), "ok") }, "answer")
	prompt := agent.sessions[0].prompt
	if !strings.HasPrefix(prompt, replayHeader+"User: earlier question"+replayFooter) || !strings.HasSuffix(prompt, "next") {
		t.Fatalf("prompt = %q", prompt)
	}
	if got := historyText(session); !strings.HasPrefix(got, "user:earlier question|user:next") {
		t.Fatalf("history = %q", got)
	}
}

// History entries that older versions saved with a (nested) replay in front
// are replayed as the user's own text.
func TestReplayStripsSavedReplays(t *testing.T) {
	e := NewEngine("test", &stubAgent{}, nil, "", LangEnglish)
	session := &Session{NeedsReplay: true}
	inner := replayHeader + "User: first" + replayFooter + "second"
	session.AddHistory("user", "first")
	session.AddHistory("user", inner)
	session.AddHistory("user", replayHeader+"User: first\nUser: "+inner+replayFooter+"third")
	session.AddHistory("user", replayHeader+"User: first")                     // a replay of an image-only message
	session.AddHistory("user", replayHeader+"User: "+inner+"\nAssistant: ans") // the same, around a nested replay
	got := e.formatSessionReplay(session, 8)
	if got != "User: first\nUser: second\nUser: third" {
		t.Fatalf("replay = %q", got)
	}
}

// Messages queued against the agent session the engine replaced (it was
// gone) run in the new one instead of being skipped as "session changed".
func TestQueuedPromptKeptAfterEngineReset(t *testing.T) {
	e := NewEngine("test", &stubAgent{}, nil, "", LangEnglish)
	session := e.sessions.GetOrCreateActive("test:chat")
	session.AgentSessionID = "old"
	queueKey, agentID := e.promptQueueKey("test:chat", session)
	for _, m := range []struct{ local, agent string }{{session.ID, agentID}, {"other-local", agentID}, {session.ID, "unrelated"}} {
		if _, err := e.enqueuePrompt(queueKey, m.local, m.agent, &Message{SessionKey: "test:chat", Content: "queued"}); err != nil {
			t.Fatal(err)
		}
	}
	state := newInteractiveState(&jobTestSession{events: make(chan Event)}, nil, nil, false)
	e.interactiveStates["test:chat"] = state
	e.cleanupInteractiveTurn("test:chat", state, session, fmt.Errorf("%w: gone", ErrAgentSessionNotFound))
	e.promptQueueMu.Lock()
	var got []string
	for _, item := range e.promptQueues[queueKey].Items {
		got = append(got, item.AgentSessionIDSnapshot)
	}
	e.promptQueueMu.Unlock()
	if strings.Join(got, ",") != ",old,unrelated" {
		t.Fatalf("snapshots = %q", got)
	}
}

type steerableSession struct {
	jobTestSession
	alive bool
}

func (s *steerableSession) Steer(string, []ImageAttachment, []FileAttachment) error { return nil }
func (s *steerableSession) Alive() bool                                             { return s.alive }

// A turn that took a steered message is not retried: the retry would lose it.
func TestSteeredTurnNotRetried(t *testing.T) {
	p := &stubPlatformEngine{n: "test"}
	e := NewEngine("test", &stubAgent{}, []Platform{p}, "", LangEnglish)
	session := e.sessions.GetOrCreateActive("test:chat")
	session.AgentSessionID = "old"
	as := &steerableSession{jobTestSession: jobTestSession{events: make(chan Event)}, alive: true}
	state := newInteractiveState(as, p, "ctx", false)
	e.interactiveStates["test:chat"] = state
	result := make(chan error, 1)
	go func() { result <- e.processInteractiveEvents(state, session, "test:chat", "m", time.Now(), nil) }()
	waitForCondition(t, state.steerable, "turn running")
	if err := e.steer(as, "test:chat", &Message{SessionKey: "test:chat", Content: "extra"}); err != nil {
		t.Fatal(err)
	}
	as.events <- Event{Type: EventError, Error: fmt.Errorf("%w: gone", ErrAgentSessionNotFound)}
	if err := <-result; err != nil {
		t.Fatalf("steered turn handed back for a retry: %v", err)
	}
	if !strings.Contains(strings.Join(p.Sent(), "|"), "gone") {
		t.Fatalf("sent = %q", p.Sent())
	}
}

// restartAgent: the first process dies on Send; restarting it finds the
// session gone; the new session takes the prompt.
type restartAgent struct {
	recoveryTestAgent
	starts int
}

func (a *restartAgent) StartSession(ctx context.Context, id string) (AgentSession, error) {
	a.starts++
	switch a.starts {
	case 1:
		a.resumeIDs = append(a.resumeIDs, id)
		return &recoveryTestSession{id: id, events: make(chan Event, 1), sendErr: errors.New("broken pipe"), closed: true}, nil
	case 2:
		a.resumeIDs = append(a.resumeIDs, id)
		return nil, fmt.Errorf("%w: thread not found", ErrAgentSessionNotFound)
	}
	return a.recoveryTestAgent.StartSession(ctx, id)
}

// A restart after the process died at Send still replays the history when
// it finds the session gone.
func TestReplayAfterRestartFindsSessionGone(t *testing.T) {
	agent := &restartAgent{recoveryTestAgent: recoveryTestAgent{turns: [][]Event{nil, nil, {{Type: EventResult, Content: "ok", Done: true}}}}}
	p := &stubPlatformEngine{n: "test"}
	e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)
	session := e.sessions.GetOrCreateActive("test:chat")
	session.AgentSessionID = "old"
	session.AddHistory("user", "earlier question")

	e.handleMessage(p, &Message{SessionKey: "test:chat", Platform: "test", UserID: "u", Content: "next", ReplyCtx: "ctx"})
	waitForCondition(t, func() bool { return strings.Contains(strings.Join(p.Sent(), "|"), "ok") }, "answer")
	prompt := agent.sessions[0].prompt
	if prompt != replayHeader+"User: earlier question"+replayFooter+"next" {
		t.Fatalf("prompt = %q", prompt)
	}
	if got := historyText(session); !strings.HasPrefix(got, "user:earlier question|user:next|") {
		t.Fatalf("history = %q", got)
	}
}
