package core

import (
	"context"
	"sync"
	"testing"
)

// compactAsker asks for a tool during compaction.
type compactAsker struct {
	p5Session
	mu2     sync.Mutex
	answers []PermissionResult
}

func (s *compactAsker) Compact(context.Context) error {
	s.events <- Event{Type: EventPermissionRequest, RequestID: "r1", ToolName: "Bash", ToolInputRaw: map[string]any{"command": "rm -rf x"}}
	s.events <- Event{Type: EventResult, Done: true}
	return nil
}

func (s *compactAsker) RespondPermission(_ string, r PermissionResult) error {
	s.mu2.Lock()
	defer s.mu2.Unlock()
	s.answers = append(s.answers, r)
	return nil
}

// P6 review: a tool requested during compaction is refused, never approved.
func TestCompactionDeniesTools(t *testing.T) {
	e, _, _, p := p3Engine(t)
	sess := &compactAsker{p5Session: p5Session{p3Session: p3Session{events: make(chan Event, 8)}}}
	st := newInteractiveState(sess, p, "ctx", false)
	e.interactiveMu.Lock()
	e.interactiveStates[p3Key] = st
	e.interactiveMu.Unlock()
	e.cmdCompress(p, &Message{SessionKey: p3Key, ReplyCtx: "ctx"})
	p.waitSent(t, e.i18n.T(MsgCompressDone))
	sess.mu2.Lock()
	defer sess.mu2.Unlock()
	if len(sess.answers) != 1 || sess.answers[0].Behavior != "deny" {
		t.Fatalf("answers = %+v", sess.answers)
	}
}

// goneAgent has no session to resume.
type goneAgent struct {
	namedStubAgent
	mu     sync.Mutex
	starts []string
}

func (a *goneAgent) StartSession(_ context.Context, id string) (AgentSession, error) {
	a.mu.Lock()
	a.starts = append(a.starts, id)
	a.mu.Unlock()
	if id != "" {
		return nil, ErrAgentSessionNotFound
	}
	return &p5Session{p3Session: p3Session{events: make(chan Event, 8)}}, nil
}

// P6 review: a chat whose agent session is gone at start gets a new one
// (and replays its history) instead of failing every message.
func TestMissingSessionAtStartIsReplaced(t *testing.T) {
	agent := &goneAgent{namedStubAgent: namedStubAgent{name: "kimi"}}
	p := &stubPlatformEngine{n: "telegram"}
	e := NewEngine("p", agent, []Platform{p}, "", LangEnglish)
	session := e.sessions.GetOrCreateActive(p3Key)
	session.AgentSessionID = "gone"
	st := e.getOrCreateInteractiveState(p3Key, p, "ctx", session)
	if st.agentSession == nil {
		t.Fatal("no process started")
	}
	if session.AgentSessionID != "" || !session.NeedsReplay {
		t.Fatalf("session not reset: id=%q replay=%v", session.AgentSessionID, session.NeedsReplay)
	}
	if len(agent.starts) != 2 || agent.starts[0] != "gone" || agent.starts[1] != "" {
		t.Fatalf("starts = %v", agent.starts)
	}
}
