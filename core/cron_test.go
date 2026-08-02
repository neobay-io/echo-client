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

type cronShellAgent struct {
	stubAgent
	workDir string
}

func (a *cronShellAgent) GetWorkDir() string { return a.workDir }

type cronReplyPlatform struct {
	stubPlatformEngine
}

func (p *cronReplyPlatform) ReconstructReplyCtx(sessionKey string) (any, error) {
	return sessionKey, nil
}

type blockingCronAgent struct {
	mu      sync.Mutex
	session *blockingCronSession
}

func (a *blockingCronAgent) Name() string { return "blocking" }
func (a *blockingCronAgent) StartSession(_ context.Context, _ string) (AgentSession, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.session == nil {
		a.session = newBlockingCronSession()
	}
	return a.session, nil
}
func (a *blockingCronAgent) ListSessions(_ context.Context) ([]AgentSessionInfo, error) {
	return nil, nil
}
func (a *blockingCronAgent) Stop() error { return nil }

type blockingCronSession struct {
	events  chan Event
	closeCh chan struct{}
	once    sync.Once
}

func newBlockingCronSession() *blockingCronSession {
	return &blockingCronSession{
		events:  make(chan Event),
		closeCh: make(chan struct{}),
	}
}

func (s *blockingCronSession) Send(_ string, _ []ImageAttachment, _ []FileAttachment) error {
	<-s.closeCh
	return nil
}
func (s *blockingCronSession) RespondPermission(_ string, _ PermissionResult) error { return nil }
func (s *blockingCronSession) Events() <-chan Event                                 { return s.events }
func (s *blockingCronSession) CurrentSessionID() string                             { return "blocking-cron" }
func (s *blockingCronSession) Alive() bool                                          { return true }
func (s *blockingCronSession) Close() error {
	s.once.Do(func() {
		close(s.closeCh)
		close(s.events)
	})
	return nil
}

// recordingCronAgent records each StartSession resume id and returns a session
// that immediately emits an EventResult carrying a (forked) session id.
type recordingCronAgent struct {
	mu      sync.Mutex
	resumes []string
	forkID  string
}

func (a *recordingCronAgent) Name() string { return "rec-cron" }
func (a *recordingCronAgent) StartSession(_ context.Context, sessionID string) (AgentSession, error) {
	a.mu.Lock()
	a.resumes = append(a.resumes, sessionID)
	fork := a.forkID
	a.mu.Unlock()
	return newRecordingCronSession(fork), nil
}
func (a *recordingCronAgent) ListSessions(_ context.Context) ([]AgentSessionInfo, error) {
	return nil, nil
}
func (a *recordingCronAgent) Stop() error { return nil }
func (a *recordingCronAgent) lastResume() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.resumes) == 0 {
		return ""
	}
	return a.resumes[len(a.resumes)-1]
}

type recordingCronSession struct {
	fork   string
	events chan Event
	once   sync.Once
}

func newRecordingCronSession(fork string) *recordingCronSession {
	return &recordingCronSession{fork: fork, events: make(chan Event, 2)}
}

func (s *recordingCronSession) Send(_ string, _ []ImageAttachment, _ []FileAttachment) error {
	s.events <- Event{Type: EventResult, Content: "done", SessionID: s.fork}
	return nil
}
func (s *recordingCronSession) RespondPermission(_ string, _ PermissionResult) error { return nil }
func (s *recordingCronSession) Events() <-chan Event                                 { return s.events }
func (s *recordingCronSession) CurrentSessionID() string                             { return s.fork }
func (s *recordingCronSession) Alive() bool                                          { return true }
func (s *recordingCronSession) Close() error {
	s.once.Do(func() { close(s.events) })
	return nil
}

// TestRunCronIsolatedTurnAccumulatesAndKeepsActive verifies the core of the
// cron/session redesign: a cron turn resumes the job's own lineage, persists the
// forked id so context accumulates across runs, and never touches the user's
// active session or creates an interactiveState for the chat.
func TestRunCronIsolatedTurnAccumulatesAndKeepsActive(t *testing.T) {
	store, err := NewCronStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewCronStore: %v", err)
	}
	scheduler := NewCronScheduler(store)
	agent := &recordingCronAgent{forkID: "fork-1"}
	p := &cronReplyPlatform{stubPlatformEngine{n: "test"}}
	engine := NewEngine("test", agent, []Platform{p}, "", LangEnglish)
	engine.SetCronScheduler(scheduler)
	scheduler.RegisterEngine("test", engine)

	// The user's active session in this chat — cron must leave it untouched.
	userSess := engine.sessions.GetOrCreateActive("test:chat")
	userSess.AgentSessionID = "user-active-id"

	job := &CronJob{
		ID: "job-1", Project: "test", SessionKey: "test:chat",
		CronExpr: "* * * * *", Prompt: "hi", Enabled: true,
		AgentSessionID: "cron-seed", CreatedAt: time.Now(),
	}
	if err := store.Add(job); err != nil {
		t.Fatalf("store.Add: %v", err)
	}

	// Run 1: resumes the seed, forks to fork-1.
	if err := engine.runCronIsolatedTurn(context.Background(), "job-1", job, p, "ctx"); err != nil {
		t.Fatalf("run1: %v", err)
	}
	if got := agent.lastResume(); got != "cron-seed" {
		t.Fatalf("run1 resumed %q, want cron-seed", got)
	}
	if got := store.Get("job-1").AgentSessionID; got != "fork-1" {
		t.Fatalf("run1 persisted id = %q, want fork-1 (accumulated)", got)
	}
	if got := engine.sessions.GetOrCreateActive("test:chat").AgentSessionID; got != "user-active-id" {
		t.Fatalf("cron changed the user's active session to %q", got)
	}
	engine.interactiveMu.Lock()
	_, hasState := engine.interactiveStates["test:chat"]
	engine.interactiveMu.Unlock()
	if hasState {
		t.Fatal("cron created an interactiveState for the user's chat")
	}

	// Run 2: now resumes fork-1 (accumulated lineage), forks to fork-2.
	agent.forkID = "fork-2"
	if err := engine.runCronIsolatedTurn(context.Background(), "job-1", store.Get("job-1"), p, "ctx"); err != nil {
		t.Fatalf("run2: %v", err)
	}
	if got := agent.lastResume(); got != "fork-1" {
		t.Fatalf("run2 resumed %q, want fork-1 (accumulated lineage)", got)
	}
	if got := store.Get("job-1").AgentSessionID; got != "fork-2" {
		t.Fatalf("run2 persisted id = %q, want fork-2", got)
	}
}

func TestAPIServerHandleCronAddRequiresSessionKey(t *testing.T) {
	store, err := NewCronStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewCronStore: %v", err)
	}

	engine := NewEngine("test", &stubAgent{}, nil, "", LangEnglish)
	server := &APIServer{
		engines: map[string]*Engine{"test": engine},
		cron:    NewCronScheduler(store),
	}

	body := strings.NewReader(`{"project":"test","cron_expr":"0 6 * * *","prompt":"hello"}`)
	req := httptest.NewRequest(http.MethodPost, "/cron/add", body)
	rec := httptest.NewRecorder()

	server.handleCronAdd(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "session_key is required") {
		t.Fatalf("body = %q, want session_key error", rec.Body.String())
	}
}

func TestEngineExecuteCronShell(t *testing.T) {
	p := &stubPlatformEngine{n: "test"}
	engine := NewEngine("test", &cronShellAgent{workDir: t.TempDir()}, []Platform{p}, "", LangEnglish)

	job := &CronJob{
		Exec:    "printf hello",
		WorkDir: t.TempDir(),
	}
	if err := engine.executeCronShell(p, nil, job); err != nil {
		t.Fatalf("executeCronShell: %v", err)
	}
	if len(p.sent) != 1 {
		t.Fatalf("sent count = %d, want 1", len(p.sent))
	}
	if !strings.Contains(p.sent[0], "hello") {
		t.Fatalf("sent message = %q, want command output", p.sent[0])
	}
}

func TestAPIServerHandleCronAddAcceptsExec(t *testing.T) {
	store, err := NewCronStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewCronStore: %v", err)
	}

	engine := NewEngine("test", &stubAgent{}, nil, "", LangEnglish)
	engine.interactiveStates["telegram:1:1"] = &interactiveState{}
	server := &APIServer{
		engines: map[string]*Engine{"test": engine},
		cron:    NewCronScheduler(store),
	}

	body := strings.NewReader(`{"project":"test","session_key":"telegram:1:1","cron_expr":"0 6 * * *","exec":"printf hello"}`)
	req := httptest.NewRequest(http.MethodPost, "/cron/add", body)
	rec := httptest.NewRecorder()

	server.handleCronAdd(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var job CronJob
	if err := json.Unmarshal(rec.Body.Bytes(), &job); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if job.Exec != "printf hello" {
		t.Fatalf("job.Exec = %q, want %q", job.Exec, "printf hello")
	}
}

func TestEngineActiveSessionKeys(t *testing.T) {
	engine := NewEngine("test", &stubAgent{}, nil, "", LangEnglish)
	engine.interactiveStates["b"] = &interactiveState{}
	engine.interactiveStates["a"] = &interactiveState{}

	got := engine.ActiveSessionKeys()
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("ActiveSessionKeys() = %v, want [a b]", got)
	}
}

func TestCronStoreUpdateRejectsReadOnlyFieldAliases(t *testing.T) {
	store, err := NewCronStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewCronStore: %v", err)
	}
	job := &CronJob{
		ID:         "job-1",
		Project:    "test",
		SessionKey: "test:session",
		CronExpr:   "* * * * *",
		Prompt:     "hello",
		Enabled:    true,
		CreatedAt:  time.Now(),
		LastError:  "original",
	}
	if err := store.Add(job); err != nil {
		t.Fatalf("store.Add: %v", err)
	}

	if ok := store.Update(job.ID, "ID", "hacked"); ok {
		t.Fatal("expected uppercase read-only alias update to be rejected")
	}
	got := store.Get(job.ID)
	if got == nil || got.ID != "job-1" {
		t.Fatalf("job ID mutated unexpectedly: %#v", got)
	}
	if ok := store.Update(job.ID, "LastError", "forged"); ok {
		t.Fatal("expected LastError alias update to be rejected")
	}
}

func TestAPIServerHandleCronInfoRequiresGET(t *testing.T) {
	store, err := NewCronStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewCronStore: %v", err)
	}
	scheduler := NewCronScheduler(store)
	server := &APIServer{cron: scheduler}

	req := httptest.NewRequest(http.MethodPost, "/cron/info?id=job-1", strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	server.handleCronInfo(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}

func TestEngineExecuteCronJobCancelsTimedOutInteractiveRun(t *testing.T) {
	agent := &blockingCronAgent{}
	p := &cronReplyPlatform{stubPlatformEngine{n: "test"}}
	engine := NewEngine("test", agent, []Platform{p}, "", LangEnglish)
	job := &CronJob{
		ID:         "job-1",
		Project:    "test",
		SessionKey: "test:session",
		CronExpr:   "* * * * *",
		Prompt:     "hello",
		Enabled:    true,
		CreatedAt:  time.Now(),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- engine.ExecuteCronJob(ctx, job)
	}()

	select {
	case err := <-done:
		if err != context.DeadlineExceeded {
			t.Fatalf("ExecuteCronJob err = %v, want %v", err, context.DeadlineExceeded)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ExecuteCronJob did not finish after timeout cleanup")
	}
	engine.interactiveMu.Lock()
	defer engine.interactiveMu.Unlock()
	if _, ok := engine.interactiveStates[job.SessionKey]; ok {
		t.Fatalf("interactive state for %q was not cleaned up", job.SessionKey)
	}
}

func TestCronSchedulerUpdateJobDoesNotPersistInvalidSessionMode(t *testing.T) {
	store, err := NewCronStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewCronStore: %v", err)
	}
	scheduler := NewCronScheduler(store)
	job := &CronJob{
		ID:         "job-1",
		Project:    "test",
		SessionKey: "test:session",
		CronExpr:   "* * * * *",
		Prompt:     "hello",
		Enabled:    true,
		CreatedAt:  time.Now(),
	}
	if err := store.Add(job); err != nil {
		t.Fatalf("store.Add: %v", err)
	}

	if err := scheduler.UpdateJob(job.ID, "session_mode", "bogus-mode"); err == nil {
		t.Fatal("expected invalid session_mode to be rejected")
	}
	got := store.Get(job.ID)
	if got == nil {
		t.Fatal("expected stored job")
	} else if got.SessionMode != "" {
		t.Fatalf("SessionMode = %q, want unchanged empty string", got.SessionMode)
	}
}

func TestParseLoopIntervalSpec(t *testing.T) {
	tests := []struct {
		in       string
		wantNorm string
		wantDur  time.Duration
		wantErr  bool
	}{
		{in: "5s", wantNorm: "5s", wantDur: 5 * time.Second},
		{in: "1s", wantErr: true},
		{in: "30m", wantNorm: "30m", wantDur: 30 * time.Minute},
		{in: "12h", wantNorm: "12h", wantDur: 12 * time.Hour},
		{in: "5d", wantNorm: "5d", wantDur: 5 * 24 * time.Hour},
		{in: "0s", wantErr: true},
		{in: "5x", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			gotDur, gotNorm, err := ParseLoopIntervalSpec(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseLoopIntervalSpec(%q) error = nil, want error", tc.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseLoopIntervalSpec(%q) error = %v", tc.in, err)
			}
			if gotNorm != tc.wantNorm || gotDur != tc.wantDur {
				t.Fatalf("ParseLoopIntervalSpec(%q) = (%v, %q), want (%v, %q)", tc.in, gotDur, gotNorm, tc.wantDur, tc.wantNorm)
			}
		})
	}
}

func TestCronSchedulerAddLoopJobNormalizesInterval(t *testing.T) {
	store, err := NewCronStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewCronStore: %v", err)
	}
	scheduler := NewCronScheduler(store)
	job := &CronJob{
		ID:           "loop-1",
		Project:      "test",
		SessionKey:   "test:session",
		Kind:         "loop",
		LoopInterval: "5d",
		Prompt:       "Ping me",
		Enabled:      true,
		CreatedAt:    time.Now(),
	}
	if err := scheduler.AddJob(job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	got := store.Get(job.ID)
	if got == nil || !got.IsLoopJob() || got.LoopInterval != "5d" {
		t.Fatalf("stored loop job = %#v", got)
	}
}

func TestCronJobScheduleSpecForLoop(t *testing.T) {
	job := &CronJob{Kind: "loop", LoopInterval: "5m"}
	got, err := job.ScheduleSpec()
	if err != nil {
		t.Fatalf("ScheduleSpec: %v", err)
	}
	if got != "@every 5m0s" {
		t.Fatalf("ScheduleSpec = %q, want %q", got, "@every 5m0s")
	}
}

func TestCronSchedulerRejectsUnknownJobKind(t *testing.T) {
	store, err := NewCronStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewCronStore: %v", err)
	}
	scheduler := NewCronScheduler(store)
	job := &CronJob{
		ID:         "job-banana",
		Project:    "test",
		SessionKey: "test:session",
		Kind:       "banana",
		CronExpr:   "* * * * *",
		Prompt:     "hello",
		Enabled:    true,
		CreatedAt:  time.Now(),
	}
	if err := scheduler.AddJob(job); err == nil {
		t.Fatal("expected unknown kind to be rejected")
	}
}

func TestEngineExecuteLoopJobAutoPausesOnPrimitive(t *testing.T) {
	store, err := NewCronStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewCronStore: %v", err)
	}
	scheduler := NewCronScheduler(store)
	agent := &scriptedRecordingAgent{responses: []string{"done\n" + loopPausePrimitive}}
	p := &cronReplyPlatform{stubPlatformEngine{n: "test"}}
	engine := NewEngine("test", agent, []Platform{p}, "", LangEnglish)
	engine.SetCronScheduler(scheduler)

	job := &CronJob{
		ID:                 "loop-1",
		Project:            "test",
		SessionKey:         "test:session",
		Kind:               "loop",
		LoopInterval:       "5m",
		Prompt:             "Check status",
		AutoPausePrimitive: true,
		Enabled:            true,
		CreatedAt:          time.Now(),
	}
	if err := scheduler.AddJob(job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	if err := engine.ExecuteCronJob(context.Background(), job); err != nil {
		t.Fatalf("ExecuteCronJob: %v", err)
	}

	got := store.Get(job.ID)
	if got == nil || got.Enabled {
		t.Fatalf("loop job = %#v, want disabled after primitive pause", got)
	}
	sends := agent.session.Sends()
	if len(sends) == 0 || !strings.Contains(sends[0].prompt, loopPausePrimitive) {
		t.Fatalf("prompt = %#v, want loop primitive instruction appended", sends)
	}
	if len(p.sent) == 0 || !strings.Contains(strings.Join(p.sent, "\n"), "paused by agent primitive") {
		t.Fatalf("sent = %#v, want pause notification", p.sent)
	}
}

func TestEngineExecuteLoopJobDoesNotAutoPauseWithoutPrimitiveOptIn(t *testing.T) {
	store, err := NewCronStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewCronStore: %v", err)
	}
	scheduler := NewCronScheduler(store)
	agent := &scriptedRecordingAgent{responses: []string{"done\n" + loopPausePrimitive}}
	p := &cronReplyPlatform{stubPlatformEngine{n: "test"}}
	engine := NewEngine("test", agent, []Platform{p}, "", LangEnglish)
	engine.SetCronScheduler(scheduler)

	job := &CronJob{
		ID:                 "loop-2",
		Project:            "test",
		SessionKey:         "test:session",
		Kind:               "loop",
		LoopInterval:       "5m",
		Prompt:             "Check status",
		AutoPausePrimitive: false,
		Enabled:            true,
		CreatedAt:          time.Now(),
	}
	if err := scheduler.AddJob(job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	if err := engine.ExecuteCronJob(context.Background(), job); err != nil {
		t.Fatalf("ExecuteCronJob: %v", err)
	}

	got := store.Get(job.ID)
	if got == nil || !got.Enabled {
		t.Fatalf("loop job = %#v, want still enabled", got)
	}
	sends := agent.session.Sends()
	if len(sends) == 0 {
		t.Fatal("expected at least one recorded send")
	}
	if strings.Contains(sends[0].prompt, loopPausePrimitive) {
		t.Fatalf("prompt = %#v, did not expect loop primitive instruction", sends)
	}
	if strings.Contains(strings.Join(p.sent, "\n"), "paused by agent primitive") {
		t.Fatalf("sent = %#v, did not expect pause notification", p.sent)
	}
}
