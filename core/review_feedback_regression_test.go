package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

func TestDrainEventsClosedChannelReturns(t *testing.T) {
	events := make(chan Event, 2)
	events <- Event{Type: EventError, Error: ErrAgentSessionNotFound}
	close(events)
	done := make(chan struct{})
	go func() { drainEvents(events); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("draining closed channel did not return")
	}
}

type trackedFailureSession struct {
	stubAgentSession
	events  chan Event
	entered chan struct{}
	release chan struct{}
	closes  atomic.Int32
}

func (s *trackedFailureSession) Events() <-chan Event { return s.events }
func (s *trackedFailureSession) Send(string, []ImageAttachment, []FileAttachment) error {
	close(s.entered)
	<-s.release
	return ErrAgentSessionNotFound
}
func (s *trackedFailureSession) Close() error { s.closes.Add(1); return nil }

func TestManagedFailureAfterStateReplacementKeepsNewState(t *testing.T) {
	p := &stubCardPlatform{n: "test"}
	e := NewEngine("test", &namedStubAgent{name: "claudecode"}, nil, "", LangEnglish)
	session := e.sessions.GetOrCreateActive("test:chat")
	session.AgentSessionID = "old"
	old := &trackedFailureSession{events: make(chan Event), entered: make(chan struct{}), release: make(chan struct{})}
	state := newInteractiveState(old, p, "ctx", false)
	e.interactiveStates["test:chat"] = state
	done := make(chan error, 1)
	go func() {
		_, err := e.runManagedTurn(state, session, "test:chat", "review", managedTurnOpts{})
		done <- err
	}()
	select {
	case <-old.entered:
	case <-time.After(time.Second):
		t.Fatal("send did not start")
	}
	e.cleanupInteractiveState("test:chat")
	fresh := &trackedFailureSession{events: make(chan Event)}
	newState := newInteractiveState(fresh, p, "new", false)
	e.interactiveMu.Lock()
	e.interactiveStates["test:chat"] = newState
	e.interactiveMu.Unlock()
	// Even a replacement process for the same local session must be preserved.
	session.mu.Lock()
	session.AgentSessionID = "new"
	session.mu.Unlock()
	close(old.release)
	select {
	case err := <-done:
		if !errors.Is(err, ErrAgentSessionNotFound) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("old turn did not finish")
	}
	if e.interactiveStates["test:chat"] != newState || fresh.closes.Load() != 0 || session.AgentSessionID != "new" {
		t.Fatal("old failure damaged replacement state or binding")
	}
}

type blockingErrorPlatform struct {
	stubCardPlatform
	entered chan struct{}
	release chan struct{}
}

func (p *blockingErrorPlatform) Send(ctx context.Context, replyCtx any, text string) error {
	close(p.entered)
	<-p.release
	return p.stubCardPlatform.Send(ctx, replyCtx, text)
}

func TestInteractiveFailureAfterErrorSendKeepsNewState(t *testing.T) {
	for _, kind := range []EventType{EventError, EventResult} {
		t.Run(string(kind), func(t *testing.T) {
			p := &blockingErrorPlatform{stubCardPlatform: stubCardPlatform{n: "test"}, entered: make(chan struct{}), release: make(chan struct{})}
			e := NewEngine("test", &namedStubAgent{name: "claudecode"}, nil, "", LangEnglish)
			session := e.sessions.GetOrCreateActive("test:chat")
			session.AgentSessionID = "old"
			old := &trackedFailureSession{events: make(chan Event, 1)}
			old.events <- Event{Type: kind, Error: ErrAgentSessionNotFound, Content: "missing"}
			state := newInteractiveState(old, p, "old", false)
			e.interactiveStates["test:chat"] = state
			done := make(chan struct{})
			go func() { e.processInteractiveEvents(state, session, "test:chat", "msg", time.Now(), nil); close(done) }()
			select {
			case <-p.entered:
			case <-time.After(time.Second):
				t.Fatal("error send did not start")
			}
			e.cleanupInteractiveState("test:chat")
			fresh := &trackedFailureSession{events: make(chan Event)}
			newState := newInteractiveState(fresh, p, "new", false)
			e.interactiveMu.Lock()
			e.interactiveStates["test:chat"] = newState
			e.interactiveMu.Unlock()
			session.mu.Lock()
			session.AgentSessionID = "new"
			session.mu.Unlock()
			close(p.release)
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("old event handler did not finish")
			}
			if e.interactiveStates["test:chat"] != newState || fresh.closes.Load() != 0 || session.AgentSessionID != "new" {
				t.Fatal("late cleanup damaged new state")
			}
		})
	}
}

func TestReviewCycleResultErrorDoesNotRequestRevision(t *testing.T) {
	authErr := errors.New("Gemini authentication failed")
	rm := NewRelayManager("")
	originAgent := &scriptedRecordingAgent{responses: []string{"<review_packet>diff</review_packet>"}}
	p := &stubCardPlatform{n: "feishu"}
	origin := NewEngine("origin", originAgent, []Platform{p}, "", LangEnglish)
	reviewer := NewEngine("reviewer", &recoveryTestAgent{turns: [][]Event{{{Type: EventResult, Error: authErr, Content: authErr.Error(), Done: true}}}}, nil, "", LangEnglish)
	rm.RegisterEngine(origin.name, origin)
	rm.RegisterEngine(reviewer.name, reviewer)
	origin.SetRelayManager(rm)
	session := origin.sessions.GetOrCreateActive("feishu:chat:user")
	session.AddHistory("assistant", "Original implementation")
	if err := origin.startReviewCycle("feishu:chat:user", "reviewer"); !errors.Is(err, authErr) {
		t.Fatalf("expected authentication failure, got %v", err)
	}
	if sends := originAgent.session.Sends(); len(sends) != 1 {
		t.Fatalf("revision executed after failed review: %v", sends)
	}
}

func TestCompressFailureAllowsFreshChatTurn(t *testing.T) {
	for _, kind := range []EventType{EventError, EventResult} {
		t.Run(string(kind), func(t *testing.T) {
			agent := &recoveryTestAgent{turns: [][]Event{{{Type: kind, Error: errors.New("Request too large"), Done: true}}, {{Type: EventResult, Content: "small reply", Done: true}}}}
			p := &stubCardPlatform{n: "test"}
			e := NewEngine("test", agent, nil, "", LangEnglish)
			session := e.sessions.GetOrCreateActive("test:chat")
			session.AgentSessionID = "old-id"
			state := e.getOrCreateInteractiveState("test:chat", p, "ctx", session)
			if err := state.agentSession.Send("/compact", nil, nil); err != nil {
				t.Fatal(err)
			}
			e.processCompressEvents(state, session, "test:chat", p, "ctx")
			if session.AgentSessionID != "" || !session.NeedsReplay || !agent.sessions[0].closed {
				t.Fatal("failed compress did not reset and close old process")
			}
			newState, _, _, _, err := e.startInteractiveTurn(p, &Message{SessionKey: "test:chat", Content: "small prompt", ReplyCtx: "ctx"}, session, nil)
			if err != nil || newState == state {
				t.Fatalf("next turn did not recover: %v", err)
			}
			if len(agent.resumeIDs) != 2 || agent.resumeIDs[1] != "" {
				t.Fatalf("reused failed transcript: %v", agent.resumeIDs)
			}
		})
	}
}

func TestCollectMissingSessionKeepsBatchAndCleansFailedState(t *testing.T) {
	agent := &recoveryTestAgent{turns: [][]Event{nil}, sendErr: ErrAgentSessionNotFound}
	p := &stubCardPlatform{n: "test"}
	e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)
	session := e.sessions.GetOrCreateActive("test:user1")
	session.AgentSessionID = "missing-id"
	for _, content := range []string{"/collect start", "source message", "/collect send summarize this"} {
		e.handleMessage(p, &Message{SessionKey: "test:user1", Platform: "test", UserID: "user1", Content: content, ReplyCtx: "ctx"})
	}
	waitForCondition(t, func() bool {
		session.mu.Lock()
		done := session.AgentSessionID == "" && !session.busy
		session.mu.Unlock()
		return done && !e.isPendingCollectionFlushing("test:user1")
	}, "missing-session collection failure")
	if batch := e.getPendingCollection("test:user1"); batch == nil || len(batch.Items) != 1 {
		t.Fatalf("lost batch: %#v", batch)
	}
	e.interactiveMu.Lock()
	state := e.interactiveStates["test:user1"]
	e.interactiveMu.Unlock()
	if state != nil || len(agent.sessions) != 1 || !agent.sessions[0].closed {
		t.Fatal("failed state retained or batch automatically replayed")
	}
	if session.HistoryLen() != 0 {
		t.Fatal("failed collection send was recorded as delivered")
	}
}

func TestCronRuntimeErrorPreservesPrivateBinding(t *testing.T) {
	for _, message := range []string{"Workspace artifact /foo does not exist", "thread/resume: failed to resolve rollout path /foo"} {
		t.Run(message, func(t *testing.T) {
			store, err := NewCronStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			agent := &recoveryTestAgent{turns: [][]Event{{{Type: EventToolUse, ToolName: "Read"}, {Type: EventError, Error: errors.New(message)}}}}
			p := &stubCardPlatform{n: "test"}
			e := NewEngine("test", agent, nil, "", LangEnglish)
			e.SetCronScheduler(NewCronScheduler(store))
			e.sessions.GetOrCreateActive("test:chat").AgentSessionID = "unrelated-chat-transcript"
			job := &CronJob{ID: "job", Project: "test", SessionKey: "test:chat", CronExpr: "* * * * *", Prompt: "review", AgentSessionID: "private-id", CreatedAt: time.Now()}
			if err := store.Add(job); err != nil {
				t.Fatal(err)
			}
			if err := e.runCronIsolatedTurn(context.Background(), job.ID, job, p, "ctx"); err == nil {
				t.Fatal("runtime failure lost")
			}
			if got := store.Get(job.ID).AgentSessionID; got != "private-id" {
				t.Fatalf("cron binding changed: %q", got)
			}
		})
	}
}

type boundedErrorPlatform struct{ stubCardPlatform }

func (p *boundedErrorPlatform) Send(ctx context.Context, r any, text string) error {
	if utf8.RuneCountInString(text) > maxPlatformMessageLen {
		return errors.New("message too long")
	}
	return p.stubCardPlatform.Send(ctx, r, text)
}

func TestLongAgentErrorIsDeliveredInChunks(t *testing.T) {
	p := &boundedErrorPlatform{stubCardPlatform{n: "telegram"}}
	e := NewEngine("test", &stubAgent{}, nil, "", LangEnglish)
	session := e.sessions.GetOrCreateActive("test:chat")
	err := errors.New(strings.Repeat("x", 5000))
	events := make(chan Event, 1)
	events <- Event{Type: EventError, Error: err}
	state := newInteractiveState(&eventfulStubAgentSession{events: events}, p, "ctx", false)
	e.interactiveStates["test:chat"] = state
	e.processInteractiveEvents(state, session, "test:chat", "msg", time.Now(), nil)
	if len(p.sent) < 2 || strings.Join(p.sent, "") != fmt.Sprintf(e.i18n.T(MsgError), err) {
		t.Fatal("long error was rejected or truncated")
	}
}
