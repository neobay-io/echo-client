package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

type recoveryTestAgent struct {
	resumeIDs     []string
	turns         [][]Event
	sessions      []*recoveryTestSession
	sendErr       error
	closedStartup bool
}

func (a *recoveryTestAgent) Name() string { return "claudecode" }
func (a *recoveryTestAgent) StartSession(_ context.Context, id string) (AgentSession, error) {
	idx := len(a.resumeIDs)
	a.resumeIDs = append(a.resumeIDs, id)
	if id == "" {
		id = "fresh-review"
	}
	s := &recoveryTestSession{id: id, events: make(chan Event, 8), output: a.turns[idx]}
	if idx == 0 {
		s.sendErr = a.sendErr
		if a.closedStartup {
			close(s.events)
		}
	}
	a.sessions = append(a.sessions, s)
	return s, nil
}
func (a *recoveryTestAgent) ListSessions(context.Context) ([]AgentSessionInfo, error) {
	return nil, nil
}
func (a *recoveryTestAgent) Stop() error { return nil }

type recoveryTestSession struct {
	id      string
	events  chan Event
	output  []Event
	prompt  string
	closed  bool
	sendErr error
}

func (s *recoveryTestSession) Send(prompt string, _ []ImageAttachment, _ []FileAttachment) error {
	s.prompt = prompt
	if s.sendErr != nil {
		return s.sendErr
	}
	for _, event := range s.output {
		s.events <- event
	}
	close(s.events)
	return nil
}
func (s *recoveryTestSession) Events() <-chan Event                             { return s.events }
func (s *recoveryTestSession) RespondPermission(string, PermissionResult) error { return nil }
func (s *recoveryTestSession) CurrentSessionID() string                         { return s.id }
func (s *recoveryTestSession) Alive() bool                                      { return !s.closed }
func (s *recoveryTestSession) Close() error                                     { s.closed = true; return nil }

func TestReviewerRecoversMissingTranscriptOnce(t *testing.T) {
	for _, earlyFailure := range []bool{false, true} {
		t.Run(fmt.Sprint(earlyFailure), func(t *testing.T) {
			missing := fmt.Errorf("%w: missing-id", ErrAgentSessionNotFound)
			agent := &recoveryTestAgent{turns: [][]Event{
				{{Type: EventError, Error: missing}},
				{{Type: EventResult, Content: "No findings.", Done: true}},
			}}
			if earlyFailure {
				agent.sendErr = missing
				agent.closedStartup = true
			}
			p := &stubPlatformEngine{n: "test"}
			e := NewEngine("claudecode-reviewer", agent, nil, "", LangEnglish)
			session := e.sessions.GetOrCreateActive("review:test")
			session.AgentSessionID = "missing-id"
			session.AddHistory("assistant", "Earlier review remains available.")
			result, err := e.runReviewerTurn(session, "review:test", p, "ctx", "<review_packet>current diff</review_packet>")
			if err != nil || result != "No findings." {
				t.Fatalf("result=%q err=%v", result, err)
			}
			if len(agent.resumeIDs) != 2 || agent.resumeIDs[0] != "missing-id" || agent.resumeIDs[1] != "" {
				t.Fatalf("resume IDs = %v", agent.resumeIDs)
			}
			if !agent.sessions[0].closed {
				t.Fatal("failed process retained")
			}
			if agent.sessions[0].prompt != agent.sessions[1].prompt {
				t.Fatal("review packet changed on retry")
			}
			if session.AgentSessionID != "fresh-review" || session.NeedsReplay {
				t.Fatalf("session not rebound: %#v", session)
			}
			if history := session.GetHistory(0); history[0].Content != "Earlier review remains available." {
				t.Fatal("history lost")
			}
			if !strings.Contains(strings.Join(p.sent, "\n"), "Retrying once") {
				t.Fatal("missing recovery notice")
			}
		})
	}
}

func TestReviewerRejectsFailedOrIncompleteReviews(t *testing.T) {
	for _, tc := range []struct {
		name     string
		events   []Event
		attempts int
	}{
		{"empty", []Event{{Type: EventResult, Done: true}}, 1},
		{"whitespace", []Event{{Type: EventResult, Content: " \n", Done: true}}, 1},
		{"auth", []Event{{Type: EventError, Error: errors.New("OAuth session expired")}}, 1},
		{"timeout error", []Event{{Type: EventError, Error: context.DeadlineExceeded}}, 1},
		{"partial EOF", []Event{{Type: EventText, Content: "I will start reviewing"}}, 1},
		{"missing twice", []Event{{Type: EventError, Error: ErrAgentSessionNotFound}}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent := &recoveryTestAgent{turns: [][]Event{tc.events, tc.events}}
			e := NewEngine("claudecode-reviewer", agent, nil, "", LangEnglish)
			session := e.sessions.GetOrCreateActive("review:test")
			session.AgentSessionID = "existing-id"
			result, err := e.runReviewerTurn(session, "review:test", &stubPlatformEngine{n: "test"}, "ctx", "review")
			if err == nil || result != "" {
				t.Fatalf("accepted failure: result=%q err=%v", result, err)
			}
			if len(agent.resumeIDs) != tc.attempts {
				t.Fatalf("attempts=%v", agent.resumeIDs)
			}
			for _, h := range session.GetHistory(0) {
				if h.Role == "assistant" {
					t.Fatalf("stored failed review as answer: %q", h.Content)
				}
			}
			for _, s := range agent.sessions {
				if !s.closed {
					t.Fatal("failed reviewer left running")
				}
			}
		})
	}
}

func TestReviewCycleEmptyReviewerDoesNotRequestRevision(t *testing.T) {
	rm := NewRelayManager("")
	originAgent := &scriptedRecordingAgent{responses: []string{"<review_packet>diff</review_packet>"}}
	p := &stubCardPlatform{n: "feishu"}
	origin := NewEngine("origin", originAgent, []Platform{p}, "", LangEnglish)
	reviewer := NewEngine("reviewer", &recoveryTestAgent{turns: [][]Event{{{Type: EventResult, Done: true}}}}, nil, "", LangEnglish)
	rm.RegisterEngine(origin.name, origin)
	rm.RegisterEngine(reviewer.name, reviewer)
	origin.SetRelayManager(rm)
	session := origin.sessions.GetOrCreateActive("feishu:chat:user")
	session.AddHistory("assistant", "Original implementation summary")
	if err := origin.startReviewCycle("feishu:chat:user", "reviewer"); !errors.Is(err, errEmptyAgentResponse) {
		t.Fatalf("expected empty review failure, got %v", err)
	}
	if sends := originAgent.session.Sends(); len(sends) != 1 {
		t.Fatalf("origin received revision despite failure: %v", sends)
	}
}

func TestSuccessfulClaudeReplyMentioningSessionErrorKeepsBinding(t *testing.T) {
	p := &stubPlatformEngine{n: "test"}
	e := NewEngine("test", &namedStubAgent{name: "claudecode"}, nil, "", LangEnglish)
	session := e.sessions.GetOrCreateActive("test:chat")
	session.AgentSessionID = "keep-this-transcript"
	events := make(chan Event, 1)
	events <- Event{Type: EventResult, Content: "The old log says No conversation found. I have fixed it.", Done: true}
	state := newInteractiveState(&eventfulStubAgentSession{events: events}, p, "ctx", false)
	e.interactiveStates["test:chat"] = state
	e.processInteractiveEvents(state, session, "test:chat", "msg", time.Now(), nil)
	if session.AgentSessionID != "keep-this-transcript" || session.NeedsReplay {
		t.Fatal("normal explanation reset session")
	}
}
