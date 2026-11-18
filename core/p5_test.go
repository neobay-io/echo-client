package core

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// ── T5.1 timers ────────────────────────────────────────────────

func TestParseTimerWhen(t *testing.T) {
	now := time.Date(2026, 11, 18, 15, 0, 0, 0, time.Local)
	cases := []struct {
		args []string
		want time.Time
		used int
	}{
		{[]string{"30m", "x"}, now.Add(30 * time.Minute), 1},
		{[]string{"1h30m"}, now.Add(90 * time.Minute), 1},
		{[]string{"2d", "x"}, now.Add(48 * time.Hour), 1},
		{[]string{"16:45"}, time.Date(2026, 11, 18, 16, 45, 0, 0, time.Local), 1},
		{[]string{"09:00"}, time.Date(2026, 11, 19, 9, 0, 0, 0, time.Local), 1}, // already past today
		{[]string{"2026-11-20", "09:30", "x"}, time.Date(2026, 11, 20, 9, 30, 0, 0, time.Local), 2},
		{[]string{"2026-11-20T09:30"}, time.Date(2026, 11, 20, 9, 30, 0, 0, time.Local), 1},
		{[]string{"2026-11-20T09:30:00Z"}, time.Date(2026, 11, 20, 9, 30, 0, 0, time.UTC), 1},
	}
	for _, c := range cases {
		got, used, err := parseTimerWhen(c.args, now)
		if err != nil || !got.Equal(c.want) || used != c.used {
			t.Errorf("%v: got %v (%d) err=%v, want %v (%d)", c.args, got, used, err, c.want, c.used)
		}
	}
	for _, bad := range [][]string{{"soon"}, {"-5m"}, {"0m"}, {"2026-11-01", "09:00"}, {"400d"}, {"1.5d"}, {}} {
		if _, _, err := parseTimerWhen(bad, now); err == nil {
			t.Errorf("%v: accepted", bad)
		}
	}
}

func TestFormatTimerRemaining(t *testing.T) {
	now := time.Now()
	for d, want := range map[time.Duration]string{
		30 * time.Second:               "30s",
		90 * time.Second:               "2m",
		2*time.Hour + 5*time.Minute:    "2h5m",
		26*time.Hour + 30*time.Minute:  "1d2h",
		-time.Minute:                   "now",
		48*time.Hour + 10*time.Second:  "2d",
		47*time.Hour + 59*time.Minute:  "1d23h",
		time.Hour - 30*time.Second:     "1h",
		3*time.Hour + 59*time.Second:   "3h1m",
		24 * time.Hour:                 "1d",
		24*time.Hour + 20*time.Minute:  "1d",
		time.Duration(0):               "now",
		59*time.Minute + 1*time.Second: "1h",
	} {
		if got := formatTimerRemaining(now.Add(d), now); got != want {
			t.Errorf("%v: %q, want %q", d, got, want)
		}
	}
}

func newP5CronScheduler(t *testing.T) (*CronScheduler, *CronStore) {
	t.Helper()
	store, err := NewCronStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cs := NewCronScheduler(store)
	t.Cleanup(cs.Stop)
	return cs, store
}

func TestTimerRunsOnceAndStaysListed(t *testing.T) {
	cs, store := newP5CronScheduler(t)
	job := &CronJob{ID: "t1", Project: "p", SessionKey: "k", Kind: "timer", RunAt: time.Now().Add(time.Hour), Prompt: "hi", Enabled: true}
	if err := cs.AddJob(job); err != nil {
		t.Fatal(err)
	}
	if err := cs.Start(); err != nil {
		t.Fatal(err)
	}
	if next := cs.NextRun("t1"); next.Sub(job.RunAt).Abs() > time.Second {
		t.Fatalf("next run %v, want %v", next, job.RunAt)
	}
	store.MarkRun("t1", nil)
	if got := store.Get("t1"); got == nil || got.Enabled {
		t.Fatalf("timer after its run: %+v", got)
	}
	if err := cs.EnableJob("t1"); err == nil {
		t.Fatal("a timer that ran was enabled again")
	}
	if err := cs.UpdateJob("t1", "enabled", true); err == nil {
		t.Fatal("a timer that ran was enabled again through an edit")
	}
	if err := cs.UpdateJob("t1", "cron_expr", "* * * * *"); err == nil {
		t.Fatal("a timer took a cron expression")
	}
}

func TestTimerValidation(t *testing.T) {
	cs, _ := newP5CronScheduler(t)
	if err := cs.AddJob(&CronJob{ID: "a", Kind: "timer", Prompt: "x", Enabled: true}); err == nil {
		t.Fatal("timer without a time accepted")
	}
	if err := cs.AddJob(&CronJob{ID: "b", Kind: "timer", RunAt: time.Now().Add(time.Hour), Enabled: true}); err == nil {
		t.Fatal("timer without a prompt accepted")
	}
}

// A timer missed while the service was down: shortly overdue runs at start,
// long overdue is recorded as missed, and fired timers are pruned after a day.
func TestTimerMissedAndPruned(t *testing.T) {
	cs, store := newP5CronScheduler(t)
	recent := &CronJob{ID: "recent", Project: "p", SessionKey: "k", Kind: "timer", RunAt: time.Now().Add(-2 * time.Minute), Prompt: "x", Enabled: true}
	stale := &CronJob{ID: "stale", Project: "p", SessionKey: "k", Kind: "timer", RunAt: time.Now().Add(-3 * time.Hour), Prompt: "x", Enabled: true}
	old := &CronJob{ID: "old", Project: "p", SessionKey: "k", Kind: "timer", RunAt: time.Now().Add(-72 * time.Hour), Prompt: "x", LastRun: time.Now().Add(-48 * time.Hour)}
	for _, j := range []*CronJob{recent, stale, old} {
		if err := store.Add(j); err != nil {
			t.Fatal(err)
		}
	}
	if err := cs.Start(); err != nil {
		t.Fatal(err)
	}
	if next := cs.NextRun("recent"); next.IsZero() || time.Until(next) > time.Minute {
		t.Fatalf("recently missed timer next run = %v", next)
	}
	if got := store.Get("stale"); got == nil || got.Enabled || !strings.Contains(got.LastError, "missed") {
		t.Fatalf("stale timer = %+v", got)
	}
	if store.Get("old") != nil {
		t.Fatal("old fired timer was not pruned")
	}
}

func TestTimerCommand(t *testing.T) {
	e, _, _, p := p3Engine(t)
	cs, store := newP5CronScheduler(t)
	e.SetCronScheduler(cs)
	cs.RegisterEngine("p", e)
	msg := &Message{SessionKey: p3Key, ReplyCtx: "ctx"}

	e.cmdTimer(p, msg, []string{"45m", "check", "the", "deploy"})
	p.waitSent(t, "Timer `")
	timers := e.jobsForSessionByKind(p3Key, "timer")
	if len(timers) != 1 || timers[0].Prompt != "check the deploy" || time.Until(timers[0].RunAt) < 44*time.Minute {
		t.Fatalf("timers = %+v", timers)
	}
	id := timers[0].ID

	e.cmdTimer(p, msg, []string{"list"})
	if got := p.waitSent(t, "Timers (1)"); !strings.Contains(got, id) || !strings.Contains(got, "check the deploy") {
		t.Fatalf("list = %q", got)
	}
	// Timers are not cron jobs.
	e.cmdCronList(p, msg)
	if strings.Contains(p.lastSent(), id) {
		t.Fatalf("/cron list shows the timer: %q", p.lastSent())
	}
	e.cmdTimer(p, msg, []string{"yesterday", "x"})
	p.waitSent(t, "invalid time")

	e.cmdTimer(p, msg, []string{"del", id})
	p.waitSent(t, "Deleted timer")
	if store.Get(id) != nil {
		t.Fatal("timer not deleted")
	}
}

// ── T5.2 self-paced loops, /goal ───────────────────────────────

func TestParseLoopNext(t *testing.T) {
	for reply, want := range map[string]time.Duration{
		"done.\n<loop-control>next 20m</loop-control>": 20 * time.Minute,
		"<loop-control> next 2h </loop-control>":       2 * time.Hour,
		"<loop-control>next 10s</loop-control>":        time.Minute,
		"<loop-control>next 30d</loop-control>":        7 * 24 * time.Hour,
		"<loop-control>next 1 d</loop-control>":        24 * time.Hour,
	} {
		got, ok := parseLoopNext(reply)
		if !ok || got != want {
			t.Errorf("%q: %v %v, want %v", reply, got, ok, want)
		}
	}
	for _, reply := range []string{"no control", "<loop-control>next soon</loop-control>", "<loop-control>next 5w</loop-control>"} {
		if _, ok := parseLoopNext(reply); ok {
			t.Errorf("%q parsed", reply)
		}
	}
}

func TestSelfPacedLoop(t *testing.T) {
	e, _, _, p := p3Engine(t)
	cs, store := newP5CronScheduler(t)
	e.SetCronScheduler(cs)
	cs.RegisterEngine("p", e)
	if err := cs.Start(); err != nil {
		t.Fatal(err)
	}
	msg := &Message{SessionKey: p3Key, ReplyCtx: "ctx"}
	e.cmdLoop(p, msg, []string{"auto", "watch", "CI"})
	p.waitSent(t, "Loop created")
	loops := e.jobsForSessionByKind(p3Key, "loop")
	if len(loops) != 1 || !loops[0].SelfPaced || loops[0].LoopInterval != defaultSelfPacedInterval || loops[0].Prompt != "watch CI" {
		t.Fatalf("loops = %+v", loops)
	}
	id := loops[0].ID
	if got := appendLoopPacing("watch CI"); !strings.Contains(got, "<loop-control>next 20m</loop-control>") || !strings.Contains(got, loopPausePrimitive) {
		t.Fatalf("pacing instruction = %q", got)
	}

	e.applyLoopPausePrimitive(p3Key, id, "CI still running.\n<loop-control>next 5m</loop-control>", p, "ctx")
	p.waitSent(t, "runs next in 5m")
	if next := cs.NextRun(id); time.Until(next) > 5*time.Minute+time.Second || time.Until(next) < 4*time.Minute {
		t.Fatalf("next run in %v, want ~5m", time.Until(next))
	}
	// Pausing still works for a self-paced loop.
	e.applyLoopPausePrimitive(p3Key, id, "All green. "+loopPausePrimitive, p, "ctx")
	if got := store.Get(id); got == nil || got.Enabled {
		t.Fatalf("loop after pause = %+v", got)
	}
}

func TestFirstThenEverySchedule(t *testing.T) {
	first := time.Now().Add(time.Minute)
	s := firstThenEvery{first: first, every: time.Hour}
	if got := s.Next(time.Now()); !got.Equal(first) {
		t.Fatalf("first = %v", got)
	}
	if got := s.Next(first); !got.Equal(first.Add(time.Hour)) {
		t.Fatalf("then = %v", got)
	}
	once := onceSchedule{at: first}
	if !once.Next(time.Now()).Equal(first) || !once.Next(first).IsZero() {
		t.Fatal("once schedule runs more than once")
	}
}

func TestGoalGoesToTheAgentWithoutNotice(t *testing.T) {
	e, _, _, p := p3Engine(t)
	msg := &Message{SessionKey: p3Key, ReplyCtx: "ctx"}
	if e.handleCommand(p, msg, "/goal ship the release") {
		t.Fatal("/goal was handled by echo-client")
	}
	if got := p.lastSent(); strings.Contains(got, "not an echo-client command") {
		t.Fatalf("notice sent: %q", got)
	}
	if e.handleCommand(p, msg, "/notacommand") {
		t.Fatal("unknown command handled")
	}
	p.waitSent(t, "not an echo-client command")
}

// ── T5.3 per-job settings ──────────────────────────────────────

type optsCronAgent struct {
	recordingCronAgent
	mu   sync.Mutex
	opts []AgentSessionOptions
}

func (a *optsCronAgent) SetSessionOptions(opts AgentSessionOptions) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.opts = append(a.opts, opts)
}

func (a *optsCronAgent) lastOpts() AgentSessionOptions {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.opts) == 0 {
		return AgentSessionOptions{}
	}
	return a.opts[len(a.opts)-1]
}

func TestCronJobOwnSettingsOverrideTheChat(t *testing.T) {
	agent := &optsCronAgent{recordingCronAgent: recordingCronAgent{forkID: "f"}}
	p := &cronReplyPlatform{stubPlatformEngine{n: "test"}}
	e := NewEngine("test", agent, []Platform{p}, "", LangEnglish)
	chat := e.sessions.GetOrCreateActive("test:chat")
	chat.UpdateSettings(func(o *AgentSessionOptions) { o.Model, o.ReasoningLevel = "chat-model", "low" })

	job := &CronJob{ID: "j", Project: "test", SessionKey: "test:chat", CronExpr: "* * * * *", Prompt: "hi", Enabled: true}
	if err := e.runCronIsolatedTurn(context.Background(), "j", job, p, "ctx"); err != nil {
		t.Fatal(err)
	}
	if got := agent.lastOpts(); got.Model != "chat-model" || got.ReasoningLevel != "low" {
		t.Fatalf("chat settings not used: %+v", got)
	}
	job.Model, job.ReasoningLevel = "job-model", "high"
	if err := e.runCronIsolatedTurn(context.Background(), "j", job, p, "ctx"); err != nil {
		t.Fatal(err)
	}
	if got := agent.lastOpts(); got.Model != "job-model" || got.ReasoningLevel != "high" {
		t.Fatalf("job settings not used: %+v", got)
	}
}

func TestCronAPIKindsAndSettings(t *testing.T) {
	e, _, _, _ := p3Engine(t)
	cs, store := newP5CronScheduler(t)
	server := &APIServer{engines: map[string]*Engine{"p": e}, cron: cs}
	post := func(body string) (int, map[string]any) {
		rec := httptest.NewRecorder()
		server.handleCronAdd(rec, httptest.NewRequest(http.MethodPost, "/cron/add", strings.NewReader(body)))
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		if rec.Code != http.StatusOK {
			out = map[string]any{"error": rec.Body.String()}
		}
		return rec.Code, out
	}

	code, job := post(`{"project":"p","session_key":"k","kind":"timer","run_at":"30m","prompt":"x"}`)
	if code != http.StatusOK || job["kind"] != "timer" {
		t.Fatalf("timer: %d %v", code, job)
	}
	if got := store.Get(job["id"].(string)); got == nil || time.Until(got.RunAt) < 29*time.Minute {
		t.Fatalf("timer run_at: %+v", got)
	}
	code, job = post(`{"project":"p","session_key":"k","kind":"loop","self_paced":true,"prompt":"x","agent_session_id":"sess-1","model":"m1"}`)
	if code != http.StatusOK || job["self_paced"] != true || job["loop_interval"] != "30m" || job["agent_session_id"] != "sess-1" || job["model"] != "m1" {
		t.Fatalf("loop: %d %v", code, job)
	}
	for _, bad := range []string{
		`{"project":"p","session_key":"k","kind":"timer","prompt":"x"}`,
		`{"project":"p","session_key":"k","kind":"timer","run_at":"30m extra","prompt":"x"}`,
		`{"project":"p","session_key":"k","kind":"weekly","prompt":"x"}`,
		`{"project":"p","session_key":"k","cron_expr":"0 6 * * *","self_paced":true,"prompt":"x"}`,
		`{"project":"p","session_key":"k","cron_expr":"0 6 * * *","prompt":"x","agent_session_id":"has space"}`,
		`{"project":"p","session_key":"k","cron_expr":"0 6 * * *","prompt":"x","mode":"nonsense"}`,
	} {
		if code, out := post(bad); code == http.StatusOK {
			t.Errorf("accepted %s: %v", bad, out)
		}
	}
}

// ── T5.4 /cancel, idle processes ───────────────────────────────

type p5Session struct {
	p3Session
	closed    bool
	autoReply bool // Send answers with a result
	sent      []string
}

func (s *p5Session) Send(prompt string, _ []ImageAttachment, _ []FileAttachment) error {
	s.mu.Lock()
	s.sent = append(s.sent, prompt)
	reply := s.autoReply
	s.mu.Unlock()
	if reply {
		s.events <- Event{Type: EventResult, Content: "ok", Done: true}
	}
	return nil
}

func (s *p5Session) Alive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.closed
}

// p5Agent starts a fresh answering session each time.
type p5Agent struct {
	namedStubAgent
	mu       sync.Mutex
	sessions []*p5Session
}

func (a *p5Agent) StartSession(context.Context, string) (AgentSession, error) {
	s := &p5Session{p3Session: p3Session{events: make(chan Event, 8)}, autoReply: true}
	a.mu.Lock()
	a.sessions = append(a.sessions, s)
	a.mu.Unlock()
	return s, nil
}
func (a *p5Agent) SetSessionOptions(AgentSessionOptions) {}

func (a *p5Agent) started() []*p5Session {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]*p5Session(nil), a.sessions...)
}

func (s *p5Session) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

func (s *p5Session) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

func p5State(e *Engine, p Platform, quiet bool) (*interactiveState, *p5Session) {
	sess := &p5Session{p3Session: p3Session{events: make(chan Event, 8)}}
	st := newInteractiveState(sess, p, "ctx", quiet)
	e.interactiveMu.Lock()
	e.interactiveStates[p3Key] = st
	e.interactiveMu.Unlock()
	return st, sess
}

func TestCancelStopsTurnDropsQueueAndStartsNewSession(t *testing.T) {
	e, _, _, p := p3Engine(t)
	old := e.sessions.GetOrCreateActive(p3Key)
	st, sess := p5State(e, p, true)
	st.beginTurn()
	queueKey, _ := e.promptQueueKey(p3Key, old)
	if _, err := e.enqueuePrompt(queueKey, old.ID, "", &Message{SessionKey: p3Key, Content: "later"}); err != nil {
		t.Fatal(err)
	}

	e.cmdCancel(p, &Message{SessionKey: p3Key, ReplyCtx: "ctx"})
	p.waitSent(t, "1 queued message(s) dropped")
	if !sess.isClosed() {
		t.Fatal("agent process not stopped")
	}
	if now := e.sessions.ActiveSession(p3Key); now == nil || now.ID == old.ID {
		t.Fatal("no new session")
	}
	e.promptQueueMu.Lock()
	n := 0
	if q := e.promptQueues[queueKey]; q != nil {
		n = len(q.Items)
	}
	e.promptQueueMu.Unlock()
	if n != 0 {
		t.Fatalf("%d queued messages left", n)
	}
	e.interactiveMu.Lock()
	next := e.interactiveStates[p3Key]
	e.interactiveMu.Unlock()
	if next == nil || !next.quiet || next.agentSession != nil {
		t.Fatalf("quiet choice not kept: %+v", next)
	}
}

func TestIdleAgentProcessIsClosed(t *testing.T) {
	e, _, _, p := p3Engine(t)
	now := time.Now()

	// Disabled by default.
	st, sess := p5State(e, p, true)
	st.lastActive = now.Add(-3 * time.Hour)
	if e.closeIdleAgentSessions(now) != 0 || sess.isClosed() {
		t.Fatal("closed without a timeout configured")
	}

	e.SetAgentSessionIdleTimeout(30 * time.Minute)
	// A running turn is never idle.
	end := st.beginTurn()
	st.mu.Lock()
	st.lastActive = now.Add(-3 * time.Hour)
	st.mu.Unlock()
	if e.closeIdleAgentSessions(now) != 0 {
		t.Fatal("closed during a turn")
	}
	end()
	// Recently active: kept.
	if e.closeIdleAgentSessions(now) != 0 {
		t.Fatal("closed right after a turn")
	}
	// A busy chat session is left alone.
	st.mu.Lock()
	st.lastActive = now.Add(-time.Hour)
	st.mu.Unlock()
	chat := e.sessions.GetOrCreateActive(p3Key)
	if !chat.TryLock() {
		t.Fatal("lock")
	}
	if e.closeIdleAgentSessions(now) != 0 {
		t.Fatal("closed while the chat was busy")
	}
	chat.Unlock()

	if e.closeIdleAgentSessions(now) != 1 || !sess.isClosed() {
		t.Fatal("idle process not closed")
	}
	e.interactiveMu.Lock()
	next := e.interactiveStates[p3Key]
	e.interactiveMu.Unlock()
	if next == nil || next.agentSession != nil || !next.quiet {
		t.Fatalf("state after close: %+v", next)
	}
	if chat.AgentSessionID != "" {
		// The chat keeps whatever session it had (empty here): nothing reset.
		t.Fatalf("session id changed: %q", chat.AgentSessionID)
	}
}

// ── T5.6 native compact ────────────────────────────────────────

type compactSession struct {
	p3Session
	compacts int
}

func (s *compactSession) Compact(context.Context) error {
	s.mu.Lock()
	s.compacts++
	s.mu.Unlock()
	s.events <- Event{Type: EventResult, Done: true}
	return nil
}

func TestCompressUsesNativeCompaction(t *testing.T) {
	e, _, _, p := p3Engine(t)
	sess := &compactSession{p3Session: p3Session{events: make(chan Event, 8)}}
	st := newInteractiveState(sess, p, "ctx", false)
	e.interactiveMu.Lock()
	e.interactiveStates[p3Key] = st
	e.interactiveMu.Unlock()

	e.cmdCompress(p, &Message{SessionKey: p3Key, ReplyCtx: "ctx"})
	p.waitSent(t, e.i18n.T(MsgCompressDone))
	sess.mu.Lock()
	n := len(sess.steered)
	c := sess.compacts
	sess.mu.Unlock()
	if c != 1 || n != 0 {
		t.Fatalf("compacts=%d steered=%d", c, n)
	}
}

// A timer run that was queued behind a busy chat and then dropped (/cancel,
// /new, /queue clear) is finished with the reason instead of staying pending.
func TestDroppedQueuedTimerIsFinished(t *testing.T) {
	e, _, _, p := p3Engine(t)
	cs, store := newP5CronScheduler(t)
	e.SetCronScheduler(cs)
	job := &CronJob{ID: "tq", Project: "p", SessionKey: p3Key, Kind: "timer", RunAt: time.Now().Add(time.Hour), Prompt: "x", Enabled: true}
	if err := cs.AddJob(job); err != nil {
		t.Fatal(err)
	}
	session := e.sessions.GetOrCreateActive(p3Key)
	queueKey, _ := e.promptQueueKey(p3Key, session)
	e.promptQueueMu.Lock()
	q := &promptQueueState{}
	e.promptQueues[queueKey] = q
	_, err := e.enqueuePromptLocked(q, queueKey, "", "", "cron", "tq", &Message{SessionKey: p3Key, Content: "x"})
	e.promptQueueMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	e.cmdCancel(p, &Message{SessionKey: p3Key, ReplyCtx: "ctx"})
	if got := store.Get("tq"); got == nil || got.Enabled || !strings.Contains(got.LastError, "dropped") {
		t.Fatalf("timer after drop = %+v", got)
	}
}

// ── P5 review fixes ────────────────────────────────────────────

// Review 1: per-job settings are refused for agents that cannot apply them.
func TestJobSettingsNeedAnAgentThatAppliesThem(t *testing.T) {
	e := NewEngine("x", &namedStubAgent{name: "gemini"}, nil, "", LangEnglish)
	for _, job := range []*CronJob{{Model: "m"}, {ReasoningLevel: "high"}, {Mode: "plan"}} {
		if err := e.normalizeJobSettings(job); err == nil {
			t.Errorf("accepted %+v", job)
		}
	}
	if err := e.normalizeJobSettings(&CronJob{AgentSessionID: "s1"}); err != nil {
		t.Fatalf("session binding refused: %v", err)
	}
}

// stoppableCronAgent's sessions never answer: a cron run waits until stopped.
type stoppableCronAgent struct{ recordingCronAgent }

func (a *stoppableCronAgent) StartSession(context.Context, string) (AgentSession, error) {
	return &p5Session{p3Session: p3Session{events: make(chan Event, 8)}}, nil
}

// Review 2: /stop and /cancel stop a scheduled turn running in the chat.
func TestStopAndCancelStopAScheduledTurn(t *testing.T) {
	for _, command := range []string{"stop", "cancel"} {
		p := &cronReplyPlatform{stubPlatformEngine{n: "test"}}
		e := NewEngine("test", &stoppableCronAgent{}, []Platform{p}, "", LangEnglish)
		cs, store := newP5CronScheduler(t)
		e.SetCronScheduler(cs)
		job := &CronJob{ID: "c", Project: "test", SessionKey: "test:chat", CronExpr: "* * * * *", Prompt: "hi", Enabled: true}
		if err := store.Add(job); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- e.runCronIsolatedTurn(context.Background(), "c", job, p, "ctx") }()
		deadline := time.Now().Add(3 * time.Second)
		for {
			e.cronRunsMu.Lock()
			running := e.cronRuns["test:chat"] != nil
			e.cronRunsMu.Unlock()
			if running {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("cron run did not start")
			}
			time.Sleep(10 * time.Millisecond)
		}
		msg := &Message{SessionKey: "test:chat", ReplyCtx: "ctx"}
		if command == "stop" {
			e.cmdStop(p, msg)
		} else {
			e.cmdCancel(p, msg)
		}
		select {
		case err := <-done:
			if err != errCronStoppedByUser {
				t.Fatalf("%s: run ended with %v", command, err)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("%s did not stop the scheduled turn", command)
		}
		if strings.Contains(strings.Join(p.sent, "|"), "failed") {
			t.Fatalf("%s: stop reported as a failure: %v", command, p.sent)
		}
	}
}

// Review 3: a process started for one session of the chat is not reused by
// another (/new, /switch, /cancel), and a turn that waited while the chat
// moved on is dropped.
func TestProcessNotReusedAcrossChatSessions(t *testing.T) {
	agent := &p5Agent{namedStubAgent: namedStubAgent{name: "claudecode"}}
	p := &stubPlatformEngine{n: "telegram"}
	e := NewEngine("p", agent, []Platform{p}, "", LangEnglish)
	s1 := e.sessions.GetOrCreateActive(p3Key)
	first := e.getOrCreateInteractiveState(p3Key, p, "ctx", s1)
	if first.agentSession == nil {
		t.Fatal("no process")
	}
	s2 := e.sessions.NewSession(p3Key, "")
	second := e.getOrCreateInteractiveState(p3Key, p, "ctx", s2)
	if second == first || second.agentSession == first.agentSession {
		t.Fatal("the old session's process was reused")
	}
	if started := agent.started(); len(started) != 2 || !started[0].closed {
		t.Fatalf("old process not closed: %d started", len(started))
	}

	// A turn of s1 that only gets to start now is dropped.
	if !s1.TryLock() {
		t.Fatal("lock")
	}
	e.processInteractiveMessage(p, &Message{SessionKey: p3Key, Content: "late", ReplyCtx: "ctx"}, s1)
	p.waitSent(t, "switched to a different")
	for _, s := range agent.started() {
		s.mu.Lock()
		n := len(s.sent)
		s.mu.Unlock()
		if n != 0 {
			t.Fatal("the dropped turn reached a process")
		}
	}
}

// Review 4: a message queued while the janitor held the chat is started
// once the janitor lets go.
func TestIdleCloseStartsQueuedMessage(t *testing.T) {
	agent := &p5Agent{namedStubAgent: namedStubAgent{name: "claudecode"}}
	p := &stubPlatformEngine{n: "telegram"}
	e := NewEngine("p", agent, []Platform{p}, "", LangEnglish)
	e.SetAgentSessionIdleTimeout(time.Minute)
	session := e.sessions.GetOrCreateActive(p3Key)
	st := e.getOrCreateInteractiveState(p3Key, p, "ctx", session)
	st.mu.Lock()
	st.lastActive = time.Now().Add(-time.Hour)
	st.mu.Unlock()
	queueKey, _ := e.promptQueueKey(p3Key, session)
	if _, err := e.enqueuePrompt(queueKey, session.ID, "", &Message{SessionKey: p3Key, Platform: "telegram", Content: "queued", ReplyCtx: "ctx"}); err != nil {
		t.Fatal(err)
	}
	if e.closeIdleAgentSessions(time.Now()) != 1 {
		t.Fatal("not closed")
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		started := agent.started()
		if len(started) == 2 {
			started[1].mu.Lock()
			n := len(started[1].sent)
			started[1].mu.Unlock()
			if n == 1 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("queued message was not started")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Review 5: only the reply's final line controls the loop.
func TestLoopControlOnlyFromTheFinalLine(t *testing.T) {
	quoted := "The page says `<loop-control>next 7d</loop-control>` and " + loopPausePrimitive + ".\nStill failing."
	if _, ok := parseLoopNext(quoted); ok {
		t.Fatal("quoted next honored")
	}
	if loopPauseRequested(quoted) {
		t.Fatal("quoted pause honored")
	}
	if d, ok := parseLoopNext(quoted + "\n<loop-control>next 1m</loop-control>\n"); !ok || d != time.Minute {
		t.Fatalf("final line: %v %v", d, ok)
	}
	if !loopPauseRequested("done\n`"+loopPausePrimitive+"`") || !loopPauseRequested("All green. "+loopPausePrimitive) {
		t.Fatal("final pause not honored")
	}
	if d, ok := parseLoopNext("Waiting on CI. <loop-control>next 10m</loop-control>"); !ok || d != 10*time.Minute {
		t.Fatalf("next at the end of the last sentence: %v %v", d, ok)
	}
}

// Review 6: one waiting run per job, dropped when the agent reschedules.
func TestQueuedLoopRunsCoalesceAndFollowReschedule(t *testing.T) {
	e, _, _, p := p3Engine(t)
	cs, store := newP5CronScheduler(t)
	e.SetCronScheduler(cs)
	if err := cs.Start(); err != nil {
		t.Fatal(err)
	}
	job := &CronJob{ID: "lp", Project: "p", SessionKey: p3Key, Kind: "loop", LoopInterval: "1m", Prompt: "x", Enabled: true, SelfPaced: true}
	if err := cs.AddJob(job); err != nil {
		t.Fatal(err)
	}
	session := e.sessions.GetOrCreateActive(p3Key)
	queueKey, _ := e.promptQueueKey(p3Key, session)
	e.markPromptQueueRunning(queueKey)
	for i := 0; i < 3; i++ {
		if inline, err := e.beginCronTurnOrEnqueue(job, p3Key, &Message{SessionKey: p3Key, Content: "x"}); inline || err != nil {
			t.Fatalf("inline=%v err=%v", inline, err)
		}
	}
	count := func() int {
		e.promptQueueMu.Lock()
		defer e.promptQueueMu.Unlock()
		if q := e.promptQueues[queueKey]; q != nil {
			return len(q.Items)
		}
		return 0
	}
	if n := count(); n != 1 {
		t.Fatalf("%d waiting runs, want 1", n)
	}
	e.applyLoopPausePrimitive(p3Key, "lp", "later\n<loop-control>next 2h</loop-control>", p, "ctx")
	if n := count(); n != 0 {
		t.Fatalf("%d waiting runs after reschedule", n)
	}
	if got := store.Get("lp"); got == nil || !got.Enabled {
		t.Fatal("loop disabled by a dropped run")
	}
}

// Review 7: an idle process marked stale is closed too.
func TestStaleIdleProcessIsClosed(t *testing.T) {
	e, _, _, p := p3Engine(t)
	e.SetAgentSessionIdleTimeout(time.Minute)
	st, sess := p5State(e, p, false)
	st.lastActive = time.Now().Add(-time.Hour)
	st.stale = true
	if e.closeIdleAgentSessions(time.Now()) != 1 || !sess.isClosed() {
		t.Fatal("stale idle process kept")
	}
}

// Review 8: a timer due in a few seconds is not pushed back.
func TestShortTimerRunsOnTime(t *testing.T) {
	cs, _ := newP5CronScheduler(t)
	if err := cs.Start(); err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(5 * time.Second)
	if err := cs.AddJob(&CronJob{ID: "s", Project: "p", SessionKey: "k", Kind: "timer", RunAt: at, Prompt: "x", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if next := cs.NextRun("s"); next.Sub(at).Abs() > time.Second {
		t.Fatalf("next run %v, want %v", next, at)
	}
}

// Review 9: /cancel keeps quiet off when the project default is quiet.
func TestCancelKeepsQuietOff(t *testing.T) {
	e, _, _, p := p3Engine(t)
	e.SetDefaultQuiet(true)
	p5State(e, p, false)
	e.cmdCancel(p, &Message{SessionKey: p3Key, ReplyCtx: "ctx"})
	e.interactiveMu.Lock()
	next := e.interactiveStates[p3Key]
	e.interactiveMu.Unlock()
	if next == nil || next.quiet {
		t.Fatalf("quiet off lost: %+v", next)
	}
}

// Kimi 1: a turn of a session the chat has left gets no process and leaves
// the current session's process alone.
func TestReplacedSessionGetsNoProcess(t *testing.T) {
	agent := &p5Agent{namedStubAgent: namedStubAgent{name: "claudecode"}}
	p := &stubPlatformEngine{n: "telegram"}
	e := NewEngine("p", agent, []Platform{p}, "", LangEnglish)
	s1 := e.sessions.GetOrCreateActive(p3Key)
	s2 := e.sessions.NewSession(p3Key, "")
	current := e.getOrCreateInteractiveState(p3Key, p, "ctx", s2)
	late := e.getOrCreateInteractiveState(p3Key, p, "ctx", s1)
	if !late.replaced || late.agentSession != nil {
		t.Fatalf("late turn got a process: %+v", late)
	}
	e.interactiveMu.Lock()
	installed := e.interactiveStates[p3Key]
	e.interactiveMu.Unlock()
	if installed != current || current.agentSession == nil || !current.agentSession.Alive() {
		t.Fatal("the current session's process was disturbed")
	}
}

// Kimi 4: a timer that fired into a busy chat's queue still runs after a
// restart, however late.
func TestQueuedTimerSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	store, err := NewCronStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	job := &CronJob{ID: "q", Project: "p", SessionKey: "k", Kind: "timer", RunAt: time.Now().Add(-3 * time.Hour), Prompt: "x", Enabled: true}
	if err := store.Add(job); err != nil {
		t.Fatal(err)
	}
	store.SetQueuedAt("q", time.Now().Add(-3*time.Hour))
	reloaded, err := NewCronStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	cs := NewCronScheduler(reloaded)
	t.Cleanup(cs.Stop)
	if err := cs.Start(); err != nil {
		t.Fatal(err)
	}
	if got := reloaded.Get("q"); got == nil || !got.Enabled || got.LastError != "" {
		t.Fatalf("queued timer marked missed: %+v", got)
	}
	if next := cs.NextRun("q"); next.IsZero() || time.Until(next) > time.Minute {
		t.Fatalf("queued timer not scheduled: %v", next)
	}
	reloaded.MarkRun("q", nil)
	if got := reloaded.Get("q"); !got.QueuedAt.IsZero() {
		t.Fatal("queued mark kept after the run")
	}
}

// Kimi 6: moving a job to another project re-checks its own settings.
func TestCronEditProjectRechecksSettings(t *testing.T) {
	claude, _, _, _ := p3Engine(t)
	gemini := NewEngine("g", &namedStubAgent{name: "gemini"}, nil, "", LangEnglish)
	cs, store := newP5CronScheduler(t)
	server := &APIServer{engines: map[string]*Engine{"p": claude, "g": gemini}, cron: cs}
	job := &CronJob{ID: "j", Project: "p", SessionKey: "k", CronExpr: "0 6 * * *", Prompt: "x", Enabled: true, Model: "m"}
	if err := store.Add(job); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	server.handleCronEdit(rec, httptest.NewRequest(http.MethodPost, "/cron/edit", strings.NewReader(`{"id":"j","field":"project","value":"g"}`)))
	if rec.Code == http.StatusOK || store.Get("j").Project != "p" {
		t.Fatalf("moved a job with its own model to an agent that cannot use it: %d %s", rec.Code, rec.Body.String())
	}
}

type unsupportedCompactSession struct{ p5Session }

func (s *unsupportedCompactSession) Compact(context.Context) error { return ErrCompactUnsupported }

// Kimi 7: an agent session that cannot compact keeps its process.
func TestCompactUnsupportedKeepsTheSession(t *testing.T) {
	e, _, _, p := p3Engine(t)
	sess := &unsupportedCompactSession{p5Session{p3Session: p3Session{events: make(chan Event, 8)}}}
	st := newInteractiveState(sess, p, "ctx", false)
	e.interactiveMu.Lock()
	e.interactiveStates[p3Key] = st
	e.interactiveMu.Unlock()
	e.cmdCompress(p, &Message{SessionKey: p3Key, ReplyCtx: "ctx"})
	p.waitSent(t, "cannot compact")
	e.interactiveMu.Lock()
	kept := e.interactiveStates[p3Key] == st
	e.interactiveMu.Unlock()
	if !kept || sess.isClosed() {
		t.Fatal("process closed")
	}
}
