package core

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// p3Agent / p3Session fake an agent with a control channel (Claude Code).
type p3Agent struct {
	namedStubAgent
	mu        sync.Mutex
	session   *p3Session
	liveErr   error
	liveCalls []AgentSessionOptions
	efforts   map[string][]string
	usage     *UsageReport
	opts      AgentSessionOptions
}

func (a *p3Agent) StartSession(context.Context, string) (AgentSession, error) { return a.session, nil }
func (a *p3Agent) SetSessionOptions(opts AgentSessionOptions)                 { a.opts = opts }
func (a *p3Agent) ApplyLiveSettings(_ context.Context, _ AgentSession, opts AgentSessionOptions) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.liveCalls = append(a.liveCalls, opts)
	return a.liveErr
}
func (a *p3Agent) EffortsForModel(_ context.Context, model string) []string { return a.efforts[model] }
func (a *p3Agent) Usage(context.Context, AgentSession) (*UsageReport, error) {
	if a.usage == nil {
		return nil, errors.New("no usage")
	}
	return a.usage, nil
}

type p3Session struct {
	mu          sync.Mutex
	events      chan Event
	steered     []string
	interrupts  int
	responses   []PermissionResult
	onInterrupt func()
}

func newP3Session() *p3Session { return &p3Session{events: make(chan Event, 8)} }

func (s *p3Session) Send(string, []ImageAttachment, []FileAttachment) error { return nil }
func (s *p3Session) RespondPermission(_ string, r PermissionResult) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.responses = append(s.responses, r)
	return nil
}
func (s *p3Session) Events() <-chan Event     { return s.events }
func (s *p3Session) CurrentSessionID() string { return "agent-s1" }
func (s *p3Session) Alive() bool              { return true }
func (s *p3Session) Close() error             { return nil }
func (s *p3Session) Steer(prompt string, _ []ImageAttachment, _ []FileAttachment) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.steered = append(s.steered, prompt)
	return nil
}
func (s *p3Session) Interrupt(context.Context) error {
	s.mu.Lock()
	s.interrupts++
	cb := s.onInterrupt
	s.mu.Unlock()
	if cb != nil {
		cb()
	}
	return nil
}
func (s *p3Session) ContextUsage(context.Context) (*ContextReport, error) {
	return &ContextReport{UsedTokens: 28561, MaxTokens: 1000000, Percent: 3, Categories: []ContextCategory{{Name: "System prompt", Tokens: 3016}}}, nil
}
func (s *p3Session) SessionUsage(context.Context) (*UsageReport, error) {
	return &UsageReport{HasCost: true, SessionCostUSD: 1.25}, nil
}

func p3Engine(t *testing.T) (*Engine, *p3Agent, *p3Session, *stubPlatformEngine) {
	t.Helper()
	sess := newP3Session()
	agent := &p3Agent{namedStubAgent: namedStubAgent{name: "claudecode"}, session: sess}
	p := &stubPlatformEngine{n: "telegram"}
	e := NewEngine("p", agent, []Platform{p}, "", LangEnglish)
	return e, agent, sess, p
}

// waitSent waits until a sent message contains want and returns it.
func (p *stubPlatformEngine) waitSent(t *testing.T, want string) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		p.sentMu.Lock()
		for i := len(p.sent) - 1; i >= 0; i-- {
			if strings.Contains(p.sent[i], want) {
				out := p.sent[i]
				p.sentMu.Unlock()
				return out
			}
		}
		all := strings.Join(p.sent, " | ")
		p.sentMu.Unlock()
		if time.Now().After(deadline) {
			t.Fatalf("no message containing %q; sent: %s", want, all)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (p *stubPlatformEngine) lastSent() string {
	p.sentMu.Lock()
	defer p.sentMu.Unlock()
	if len(p.sent) == 0 {
		return ""
	}
	return p.sent[len(p.sent)-1]
}

const p3Key = "tg:chat:user"

func p3State(e *Engine, sess *p3Session, p Platform) *interactiveState {
	st := newInteractiveState(sess, p, "ctx", false)
	e.interactiveMu.Lock()
	e.interactiveStates[p3Key] = st
	e.interactiveMu.Unlock()
	return st
}

// T3.1: a setting change goes to the running process; the process is only
// retired when the live switch fails.
func TestSettingChangesApplyLive(t *testing.T) {
	e, agent, sess, p := p3Engine(t)
	st := p3State(e, sess, p)
	e.applySessionSetting(p3Key, func(o *AgentSessionOptions) { o.Model = "fable" }, nil)
	if st.stale || len(agent.liveCalls) != 1 || agent.liveCalls[0].Model != "fable" {
		t.Fatalf("stale = %v, live calls = %#v", st.stale, agent.liveCalls)
	}
	// P3 review K1: a running turn is never switched live (another member of
	// a shared chat could loosen its permissions mid-turn); it restarts later.
	end := st.beginTurn()
	e.applySessionSetting(p3Key, func(o *AgentSessionOptions) { o.Mode = "bypassPermissions" }, nil)
	end()
	if len(agent.liveCalls) != 1 || !st.stale {
		t.Fatalf("running turn switched live: calls = %d, stale = %v", len(agent.liveCalls), st.stale)
	}
	st.stale = false
	agent.liveErr = errors.New("refused")
	e.applySessionSetting(p3Key, func(o *AgentSessionOptions) { o.Mode = "plan" }, nil)
	if !st.stale {
		t.Fatal("a failed live switch did not retire the process")
	}
}

// T3.4: /stop interrupts the running turn and keeps the process.
func TestStopInterruptsAndKeepsProcess(t *testing.T) {
	e, _, sess, p := p3Engine(t)
	st := p3State(e, sess, p)
	endTurn := st.beginTurn()
	sess.onInterrupt = func() { go endTurn() }
	e.cmdStop(p, &Message{SessionKey: p3Key, ReplyCtx: "ctx"})
	p.waitSent(t, "stopped")
	e.interactiveMu.Lock()
	kept := e.interactiveStates[p3Key] == st
	e.interactiveMu.Unlock()
	if sess.interrupts != 1 || !kept {
		t.Fatalf("interrupts = %d, state kept = %v", sess.interrupts, kept)
	}
	// Without a running turn /stop falls back to stopping the process.
	e.cmdStop(p, &Message{SessionKey: p3Key, ReplyCtx: "ctx"})
	e.interactiveMu.Lock()
	_, still := e.interactiveStates[p3Key]
	e.interactiveMu.Unlock()
	if sess.interrupts != 1 || still {
		t.Fatalf("idle stop: interrupts = %d, state kept = %v", sess.interrupts, still)
	}
}

// T3.4: an interrupt the turn does not answer in time falls back to killing.
func TestStopFallsBackWhenTurnDoesNotEnd(t *testing.T) {
	prev := interruptWait
	interruptWait = 100 * time.Millisecond
	defer func() { interruptWait = prev }()
	e, _, sess, p := p3Engine(t)
	st := p3State(e, sess, p)
	defer st.beginTurn()()
	e.cmdStop(p, &Message{SessionKey: p3Key, ReplyCtx: "ctx"})
	p.waitSent(t, "stopped")
	e.interactiveMu.Lock()
	_, still := e.interactiveStates[p3Key]
	e.interactiveMu.Unlock()
	if still {
		t.Fatal("process kept although the interrupted turn never ended")
	}
}

// T3.3: /steer and the queue's "Insert now".
func TestSteerIntoRunningTurn(t *testing.T) {
	e, _, sess, p := p3Engine(t)
	st := p3State(e, sess, p)
	end := st.beginTurn()
	defer end()

	e.cmdSteer(p, &Message{SessionKey: p3Key, Content: "/steer also update\nthe docs", ReplyCtx: "ctx"})
	if len(sess.steered) != 1 || !strings.Contains(sess.steered[0], "also update\nthe docs") {
		t.Fatalf("steered = %q", sess.steered)
	}
	if h := e.sessions.GetOrCreateActive(p3Key).GetHistory(0); len(h) != 1 || h[0].Role != "user" {
		t.Fatalf("history = %#v", h)
	}

	// A message queued behind the turn offers "Insert now"; the button
	// moves it into the turn.
	s := e.sessions.GetOrCreateActive(p3Key)
	queueKey, agentID := e.promptQueueKey(p3Key, s)
	pos, itemID, err := e.enqueuePromptID(queueKey, s.ID, agentID, &Message{SessionKey: p3Key, Platform: "telegram", Content: "queued one", ReplyCtx: "ctx"})
	if err != nil {
		t.Fatal(err)
	}
	// Another message queued after it must not take its button.
	_, _, _ = e.enqueuePromptID(queueKey, s.ID, agentID, &Message{SessionKey: p3Key, Platform: "telegram", Content: "someone else's", ReplyCtx: "ctx"})
	buttons := e.queuedReplyButtons(p3Key, itemID)
	if pos != 1 || len(buttons) != 1 || !strings.HasPrefix(buttons[0][0].Data, "cmd:/queue steer q") {
		t.Fatalf("buttons = %#v", buttons)
	}
	id := strings.TrimPrefix(buttons[0][0].Data, "cmd:/queue steer ")
	if note := e.queueAction(p3Key, "steer "+id); !strings.Contains(note, "Added to the running turn") {
		t.Fatalf("queue steer = %q", note)
	}
	if len(sess.steered) != 2 || !strings.Contains(sess.steered[1], "queued one") || e.lastQueuedItemID(queueKey) == itemID {
		t.Fatalf("steered = %q, queue left %q", sess.steered, e.lastQueuedItemID(queueKey))
	}

	// No running turn: nothing to insert into.
	end()
	if note := e.queueAction(p3Key, "steer q999"); !strings.Contains(note, "No turn is running") {
		t.Fatalf("idle queue steer = %q", note)
	}
	if e.queuedReplyButtons(p3Key, "q1") != nil {
		t.Fatal("offered Insert now without a running turn")
	}
}

// T3.5: AskUserQuestion becomes a question with options; the answer goes
// back as the tool's input.
func TestAskUserQuestionFlow(t *testing.T) {
	e, _, sess, p := p3Engine(t)
	st := p3State(e, sess, p)
	input := map[string]any{"questions": []any{
		map[string]any{"question": "Which database?", "header": "DB", "options": []any{
			map[string]any{"label": "Postgres", "description": "relational"},
			map[string]any{"label": "SQLite"},
		}},
		map[string]any{"question": "Which features?", "multiSelect": true, "options": []any{
			map[string]any{"label": "Auth"}, map[string]any{"label": "Billing"}, map[string]any{"label": "Search"},
		}},
	}}
	st.approveAll = true // questions are asked even with "allow all"
	decision := newPendingDecision(ToolAskUserQuestion, input)
	pending := &pendingPermission{RequestID: "r1", ToolName: ToolAskUserQuestion, ToolInput: input, Resolved: make(chan struct{}), decision: decision}
	st.pending = pending
	e.sendDecisionPrompt(p3Key, p, "ctx", decision)
	if out := p.lastSent(); !strings.Contains(out, "**DB** — Which database?") || !strings.Contains(out, "1. Postgres — relational") {
		t.Fatalf("question = %q", out)
	}
	e.handlePendingPermission(p, &Message{SessionKey: p3Key, ReplyCtx: "ctx"}, "SQLite")
	if out := p.lastSent(); !strings.Contains(out, "(2/2) Which features?") {
		t.Fatalf("second question = %q", out)
	}
	e.handlePendingPermission(p, &Message{SessionKey: p3Key, ReplyCtx: "ctx"}, "1,3")
	select {
	case <-pending.Resolved:
	default:
		t.Fatal("answered question not resolved")
	}
	if len(sess.responses) != 1 || sess.responses[0].Behavior != "allow" {
		t.Fatalf("responses = %#v", sess.responses)
	}
	answers, _ := sess.responses[0].UpdatedInput["answers"].(map[string]any)
	if answers["Which database?"] != "SQLite" || answers["Which features?"] != "Auth, Search" || sess.responses[0].UpdatedInput["questions"] == nil {
		t.Fatalf("answers = %#v", sess.responses[0].UpdatedInput)
	}
	// Free text is an "Other" answer.
	q := askQuestion{Question: "q", Options: []askOption{{Label: "A"}}}
	if q.answerFor("something else") != "something else" || q.answerFor("a") != "A" {
		t.Fatal("answerFor")
	}
}

// T3.5: ExitPlanMode: approve, keep planning, or send changes.
func TestPlanApprovalFlow(t *testing.T) {
	for _, tc := range []struct {
		reply, behavior, message string
	}{
		{"approve plan", "allow", ""},
		{"keep planning", "deny", "keep planning"},
		{"use a queue instead of polling", "deny", "use a queue instead of polling"},
	} {
		e, _, sess, p := p3Engine(t)
		st := p3State(e, sess, p)
		input := map[string]any{"plan": "1. add a table\n2. migrate"}
		d := newPendingDecision(ToolExitPlanMode, input)
		st.pending = &pendingPermission{RequestID: "r", ToolName: ToolExitPlanMode, ToolInput: input, Resolved: make(chan struct{}), decision: d}
		e.sendDecisionPrompt(p3Key, p, "ctx", d)
		if !strings.Contains(p.lastSent(), "1. add a table") {
			t.Fatalf("plan prompt = %q", p.lastSent())
		}
		e.handlePendingPermission(p, &Message{SessionKey: p3Key, ReplyCtx: "ctx"}, tc.reply)
		if len(sess.responses) != 1 || sess.responses[0].Behavior != tc.behavior || !strings.Contains(sess.responses[0].Message, tc.message) {
			t.Fatalf("%q: responses = %#v", tc.reply, sess.responses)
		}
	}
}

// T3.6: /usage, /context and the cost line of /status.
func TestUsageContextAndStatusCost(t *testing.T) {
	e, agent, sess, p := p3Engine(t)
	agent.usage = &UsageReport{Plan: "max", HasCost: true, SessionCostUSD: 0.5, Lines: []UsageLine{
		{Name: "5-hour limit", UsedPercent: 11, ResetsAt: time.Date(2026, 11, 18, 13, 50, 0, 0, time.Local)},
	}}
	e.cmdUsage(p, &Message{SessionKey: p3Key, ReplyCtx: "ctx"})
	if out := p.waitSent(t, "📊 Usage"); !strings.Contains(out, "📊 Usage (max)") || !strings.Contains(out, "5-hour limit: 11% used, resets 11-18 13:50") || !strings.Contains(out, "$0.50") {
		t.Fatalf("usage = %q", out)
	}
	e.cmdContext(p, &Message{SessionKey: p3Key, ReplyCtx: "ctx"})
	if out := p.lastSent(); !strings.Contains(out, "no agent process is running") && !strings.Contains(out, "No agent process") {
		t.Fatalf("context without a process = %q", out)
	}
	p3State(e, sess, p)
	e.cmdContext(p, &Message{SessionKey: p3Key, ReplyCtx: "ctx"})
	if out := p.waitSent(t, "tokens (3%)"); !strings.Contains(out, "28,561 / 1,000,000 tokens (3%)") || !strings.Contains(out, "System prompt: 3,016") {
		t.Fatalf("context = %q", out)
	}
	if line := e.sessionCostLine(p3Key); !strings.Contains(line, "$1.25") {
		t.Fatalf("status cost = %q", line)
	}
}

// T3.2: /effort offers what the chat's model supports.
func TestEffortOptionsFollowModel(t *testing.T) {
	e, agent, _, _ := p3Engine(t)
	agent.efforts = map[string][]string{"haiku": {"low", "medium"}, "fable": {"low", "high", "ultracode"}}
	levels := []ReasoningLevelOption{{Name: "auto"}, {Name: "low"}, {Name: "medium"}, {Name: "high"}, {Name: "max"}}
	names := func(ls []ReasoningLevelOption) string {
		var out []string
		for _, l := range ls {
			out = append(out, l.Name)
		}
		return strings.Join(out, ",")
	}
	if got := names(e.reasoningLevelsForModel(levels, "haiku")); got != "auto,low,medium" {
		t.Fatalf("haiku = %s", got)
	}
	if got := names(e.reasoningLevelsForModel(levels, "fable")); got != "auto,low,high,ultracode" {
		t.Fatalf("fable = %s", got)
	}
	if got := names(e.reasoningLevelsForModel(levels, "unknown")); got != "auto,low,medium,high,max" {
		t.Fatalf("unknown model = %s", got)
	}
}

// T3.7: a cron run that extended an agent session makes the chat holding it
// restart its process before the next turn.
func TestCronRunRetiresChatHoldingTheSession(t *testing.T) {
	e, _, sess, p := p3Engine(t)
	st := p3State(e, sess, p)
	e.retireStatesForAgentSession("other-session")
	if st.stale {
		t.Fatal("unrelated session retired")
	}
	e.retireStatesForAgentSession("", "agent-s1")
	if !st.stale {
		t.Fatal("chat holding the cron's session was not retired")
	}
}

// P3 review 1: an option is answered by its number; a label that looks like
// a command never runs as one.
func TestQuestionOptionsSendNumbers(t *testing.T) {
	e, _, sess, p := p3Engine(t)
	st := p3State(e, sess, p)
	input := map[string]any{"questions": []any{map[string]any{"question": "Pick", "options": []any{
		map[string]any{"label": "/mode yolo"}, map[string]any{"label": "safe"},
	}}}}
	d := newPendingDecision(ToolAskUserQuestion, input)
	st.pending = &pendingPermission{RequestID: "r", ToolName: ToolAskUserQuestion, ToolInput: input, Resolved: make(chan struct{}), decision: d}
	e.sendDecisionPrompt(p3Key, p, "ctx", d)
	e.promptMu.Lock()
	prompt := e.prompts[p3Key]
	e.promptMu.Unlock()
	for _, c := range prompt.Choices {
		if strings.HasPrefix(c.SendText, "/") {
			t.Fatalf("choice %q sends %q", c.Label, c.SendText)
		}
	}
	e.handleMessage(p, &Message{SessionKey: p3Key, Platform: "telegram", UserID: "u", Content: "1", ReplyCtx: "ctx"})
	if len(sess.responses) != 1 {
		t.Fatalf("responses = %#v", sess.responses)
	}
	answers, _ := sess.responses[0].UpdatedInput["answers"].(map[string]any)
	if answers["Pick"] != "/mode yolo" {
		t.Fatalf("answers = %#v", answers)
	}
	if mode := e.effectiveMode(e.sessions.GetOrCreateActive(p3Key)); mode == "bypassPermissions" || mode == "yolo" {
		t.Fatalf("an option label switched the mode to %q", mode)
	}
}

// P3 review 2: concurrent answers to one question are taken once.
func TestConcurrentAnswersTakenOnce(t *testing.T) {
	e, _, sess, p := p3Engine(t)
	st := p3State(e, sess, p)
	input := map[string]any{"questions": []any{map[string]any{"question": "Q", "options": []any{map[string]any{"label": "A"}}}}}
	d := newPendingDecision(ToolAskUserQuestion, input)
	pending := &pendingPermission{RequestID: "r", ToolName: ToolAskUserQuestion, ToolInput: input, Resolved: make(chan struct{}), decision: d}
	st.pending = pending
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.handleDecisionReply(p, &Message{SessionKey: p3Key, ReplyCtx: "ctx"}, st, pending, "1")
		}()
	}
	wg.Wait()
	if len(sess.responses) != 1 {
		t.Fatalf("answered %d times", len(sess.responses))
	}
}

// P3 review 4: a live mode change drops an earlier "allow all".
func TestLiveModeChangeClearsAllowAll(t *testing.T) {
	e, _, sess, p := p3Engine(t)
	st := p3State(e, sess, p)
	st.approveAll = true
	e.applySessionSetting(p3Key, func(o *AgentSessionOptions) { o.Mode = "default" }, nil)
	if st.approveAll {
		t.Fatal("allow all survived a mode change")
	}
}

// P3 review 5: once the turn has its final result, nothing is steered into it.
func TestNoSteeringAfterFinalResult(t *testing.T) {
	e, _, sess, p := p3Engine(t)
	st := p3State(e, sess, p)
	defer st.beginTurn()()
	if e.steerTarget(p3Key) == nil {
		t.Fatal("running turn not steerable")
	}
	st.turnFinishing()
	if e.steerTarget(p3Key) != nil {
		t.Fatal("steerable after its final result")
	}
}

// P3 review 7 and 9: an interrupt never reaches a turn that already ended,
// and stopping a pending question retires its buttons.
func TestStopRetiresPendingPromptAndSkipsEndedTurn(t *testing.T) {
	e, _, sess, p := p3Engine(t)
	st := p3State(e, sess, p)
	end := st.beginTurn()
	sess.onInterrupt = func() { go end() }
	input := map[string]any{"plan": "x"}
	d := newPendingDecision(ToolExitPlanMode, input)
	st.pending = &pendingPermission{RequestID: "r", ToolName: ToolExitPlanMode, ToolInput: input, Resolved: make(chan struct{}), decision: d}
	e.sendDecisionPrompt(p3Key, p, "ctx", d)
	if !e.interruptRunningTurn(p3Key, st) {
		t.Fatal("interrupt failed")
	}
	e.promptMu.Lock()
	_, stillPrompt := e.prompts[p3Key]
	e.promptMu.Unlock()
	if stillPrompt || len(sess.responses) != 1 || sess.responses[0].Behavior != "deny" {
		t.Fatalf("prompt kept = %v, responses = %#v", stillPrompt, sess.responses)
	}

	// The turn ends between /stop reading it and sending the interrupt:
	// nothing is sent.
	sess.interrupts = 0
	end2 := st.beginTurn()
	done := st.runningTurn()
	end2()
	st.turnMu.Lock()
	running := st.runningTurn() == done
	st.turnMu.Unlock()
	if running {
		t.Fatal("ended turn still current")
	}
	if e.interruptRunningTurn(p3Key, st) || sess.interrupts != 0 {
		t.Fatalf("interrupted with no running turn: %d", sess.interrupts)
	}
}
