package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// flakyPlatform fails its first `failures` Start calls (every call when
// failures < 0) and then starts. If gate is set, the gateCall-th Start call
// blocks until gate is closed. Like Telegram, it must not be sent through
// before a successful Start.
type flakyPlatform struct {
	name     string
	failures int
	gate     chan struct{}
	gateCall int
	entered  chan int // receives each Start call number on entry, if set

	mu            sync.Mutex
	startCalls    int
	inStart       int
	maxInStart    int
	stopCalls     int
	running       bool
	registered    int
	sent          []string
	sentUnstarted int
}

func (p *flakyPlatform) Name() string { return p.name }

func (p *flakyPlatform) Start(MessageHandler) error {
	p.mu.Lock()
	p.startCalls++
	call := p.startCalls
	p.inStart++
	if p.inStart > p.maxInStart {
		p.maxInStart = p.inStart
	}
	p.mu.Unlock()

	if p.entered != nil {
		p.entered <- call
	}
	if p.gate != nil && call == p.gateCall {
		<-p.gate
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	p.inStart--
	if p.failures < 0 || call <= p.failures {
		return fmt.Errorf("getMe: dial tcp: i/o timeout (call %d)", call)
	}
	p.running = true
	return nil
}

func (p *flakyPlatform) Reply(ctx context.Context, rctx any, content string) error {
	return p.Send(ctx, rctx, content)
}

func (p *flakyPlatform) Send(_ context.Context, _ any, content string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.running {
		p.sentUnstarted++
		return errors.New("send before start")
	}
	p.sent = append(p.sent, content)
	return nil
}

func (p *flakyPlatform) Stop() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopCalls++
	p.running = false
	return nil
}

func (p *flakyPlatform) RegisterCommands([]BotCommandInfo) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.registered++
	return nil
}

func (p *flakyPlatform) ReconstructReplyCtx(sessionKey string) (any, error) {
	return sessionKey, nil
}

type flakySnapshot struct {
	startCalls, maxInStart, stopCalls, registered, sentUnstarted int
	running                                                      bool
	sent                                                         []string
}

func (p *flakyPlatform) snapshot() flakySnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	return flakySnapshot{
		startCalls:    p.startCalls,
		maxInStart:    p.maxInStart,
		stopCalls:     p.stopCalls,
		registered:    p.registered,
		sentUnstarted: p.sentUnstarted,
		running:       p.running,
		sent:          append([]string(nil), p.sent...),
	}
}

func newPlatformRetryTestEngine(platforms ...Platform) *Engine {
	e := NewEngine("test", &stubAgent{}, platforms, "", LangEnglish)
	e.platformRetryInitial = time.Millisecond
	e.platformRetryMax = 4 * time.Millisecond
	e.platformRetryStopWait = 2 * time.Second
	return e
}

// retriesDone reports whether every retry goroutine has exited within d.
func retriesDone(e *Engine, d time.Duration) bool {
	done := make(chan struct{})
	go func() {
		e.platformRetryWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

func waitForStartCall(t *testing.T, entered <-chan int, want int) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case call := <-entered:
			if call == want {
				return
			}
		case <-deadline:
			t.Fatalf("timed out waiting for Start call %d", want)
		}
	}
}

func TestEngineStartRetriesFailedPlatformUntilItStarts(t *testing.T) {
	healthy := &flakyPlatform{name: "feishu"}
	flaky := &flakyPlatform{name: "telegram", failures: 3}
	e := newPlatformRetryTestEngine(healthy, flaky)

	if err := e.Start(); err != nil {
		t.Fatalf("Start() = %v, want nil when one platform started", err)
	}
	waitForCondition(t, func() bool { return flaky.snapshot().registered == 1 }, "telegram to start on retry and register commands")

	if !e.platformRunning(1) {
		t.Fatal("platformRunning(telegram) = false after successful retry")
	}
	// Give a buggy retry loop a chance to keep going.
	time.Sleep(20 * time.Millisecond)

	got := flaky.snapshot()
	if got.startCalls != 4 {
		t.Fatalf("telegram Start calls = %d, want 4 (3 failures + 1 success)", got.startCalls)
	}
	if got.maxInStart != 1 {
		t.Fatalf("telegram concurrent Start calls = %d, want 1", got.maxInStart)
	}
	if got.registered != 1 {
		t.Fatalf("telegram RegisterCommands calls = %d, want 1", got.registered)
	}
	if h := healthy.snapshot(); h.startCalls != 1 || h.registered != 1 {
		t.Fatalf("feishu Start/RegisterCommands = %d/%d, want 1/1", h.startCalls, h.registered)
	}
	if !retriesDone(e, time.Second) {
		t.Fatal("retry goroutine still running after platform started")
	}

	if err := e.Stop(); err != nil {
		t.Fatalf("Stop() = %v", err)
	}
	if s := flaky.snapshot().stopCalls; s != 1 {
		t.Fatalf("telegram Stop calls = %d, want 1", s)
	}
	if s := healthy.snapshot().stopCalls; s != 1 {
		t.Fatalf("feishu Stop calls = %d, want 1", s)
	}
}

func TestEngineStartErrorsWhenAllPlatformsFailButKeepsRetrying(t *testing.T) {
	flaky := &flakyPlatform{name: "telegram", failures: 1}
	e := newPlatformRetryTestEngine(flaky)

	if err := e.Start(); err == nil {
		t.Fatal("Start() = nil, want error when every platform failed")
	}
	waitForCondition(t, func() bool { return flaky.snapshot().running }, "telegram to start on retry")
	if err := e.Stop(); err != nil {
		t.Fatalf("Stop() = %v", err)
	}
}

func TestEngineStartTwiceIsRejected(t *testing.T) {
	p := &flakyPlatform{name: "feishu"}
	e := newPlatformRetryTestEngine(p)
	if err := e.Start(); err != nil {
		t.Fatalf("first Start() = %v", err)
	}
	if err := e.Start(); err == nil {
		t.Fatal("second Start() = nil, want error")
	}
	if got := p.snapshot().startCalls; got != 1 {
		t.Fatalf("Start calls = %d, want 1", got)
	}
	_ = e.Stop()
}

func TestEngineStopCancelsPlatformStartRetry(t *testing.T) {
	never := &flakyPlatform{name: "telegram", failures: -1}
	healthy := &flakyPlatform{name: "feishu"}
	e := newPlatformRetryTestEngine(healthy, never)

	if err := e.Start(); err != nil {
		t.Fatalf("Start() = %v", err)
	}
	waitForCondition(t, func() bool { return never.snapshot().startCalls >= 3 }, "several retries")

	stopStarted := time.Now()
	if err := e.Stop(); err != nil {
		t.Fatalf("Stop() = %v", err)
	}
	if elapsed := time.Since(stopStarted); elapsed > time.Second {
		t.Fatalf("Stop took %v, want prompt return", elapsed)
	}
	if !retriesDone(e, 100*time.Millisecond) {
		t.Fatal("retry goroutine still running after Stop returned")
	}

	calls := never.snapshot().startCalls
	time.Sleep(30 * time.Millisecond)
	got := never.snapshot()
	if got.startCalls != calls {
		t.Fatalf("Start calls went from %d to %d after Stop", calls, got.startCalls)
	}
	if got.stopCalls != 1 {
		t.Fatalf("Stop calls on failed platform = %d, want 1 (as before retries existed)", got.stopCalls)
	}
	if got.registered != 0 {
		t.Fatalf("RegisterCommands calls = %d, want 0", got.registered)
	}
	if e.platformRunning(1) {
		t.Fatal("platformRunning(never-started platform) = true after Stop")
	}
}

func TestEngineContextCancelStopsPlatformStartRetry(t *testing.T) {
	never := &flakyPlatform{name: "telegram", failures: -1}
	e := newPlatformRetryTestEngine(never)
	e.platformRetryInitial = time.Hour // the retry is parked in its backoff wait
	e.platformRetryMax = time.Hour

	_ = e.Start()
	e.cancel()

	if !retriesDone(e, time.Second) {
		t.Fatal("retry goroutine did not exit after the engine context was cancelled")
	}
	if got := never.snapshot().startCalls; got != 1 {
		t.Fatalf("Start calls = %d, want 1 (no retry after cancellation)", got)
	}
}

func TestEngineStopDuringInFlightRetryStopsPlatformWhenStartReturns(t *testing.T) {
	gate := make(chan struct{})
	flaky := &flakyPlatform{name: "telegram", failures: 1, gate: gate, gateCall: 2, entered: make(chan int, 16)}
	healthy := &flakyPlatform{name: "feishu"}
	e := newPlatformRetryTestEngine(healthy, flaky)
	e.platformRetryStopWait = 20 * time.Millisecond

	if err := e.Start(); err != nil {
		t.Fatalf("Start() = %v", err)
	}
	waitForStartCall(t, flaky.entered, 2) // retry is now blocked inside Start

	if err := e.Stop(); err != nil {
		t.Fatalf("Stop() = %v", err)
	}
	if got := flaky.snapshot().stopCalls; got != 0 {
		t.Fatalf("Stop called %d times while Start was in flight, want 0", got)
	}
	if got := healthy.snapshot().stopCalls; got != 1 {
		t.Fatalf("feishu Stop calls = %d, want 1", got)
	}

	close(gate) // the in-flight Start now succeeds, after the engine stopped
	if !retriesDone(e, 2*time.Second) {
		t.Fatal("retry goroutine did not exit after its in-flight Start returned")
	}
	got := flaky.snapshot()
	if got.stopCalls != 1 || got.running {
		t.Fatalf("after late start: Stop calls = %d, running = %v; want 1, false", got.stopCalls, got.running)
	}
	if got.startCalls != 2 {
		t.Fatalf("Start calls = %d, want 2", got.startCalls)
	}
	if got.registered != 0 {
		t.Fatalf("RegisterCommands calls = %d, want 0 for a platform started after Stop", got.registered)
	}
	if e.platformRunning(1) {
		t.Fatal("platformRunning = true for platform that came up after Stop")
	}
}

func TestEngineSendPathsRefusePlatformUntilRetrySucceeds(t *testing.T) {
	gate := make(chan struct{})
	flaky := &flakyPlatform{name: "telegram", failures: 1, gate: gate, gateCall: 2, entered: make(chan int, 16)}
	healthy := &flakyPlatform{name: "feishu"}
	e := newPlatformRetryTestEngine(healthy, flaky)

	if err := e.Start(); err != nil {
		t.Fatalf("Start() = %v", err)
	}
	t.Cleanup(func() { _ = e.Stop() })
	waitForStartCall(t, flaky.entered, 2)

	const sessionKey = "telegram:42:7"
	if err := e.sendTextToSession(sessionKey, "hello"); !errors.Is(err, ErrPlatformNotRunning) {
		t.Fatalf("sendTextToSession error = %v, want ErrPlatformNotRunning", err)
	}
	if err := e.sendCardToSession(sessionKey, NewCard().Markdown("hi").Build()); !errors.Is(err, ErrPlatformNotRunning) {
		t.Fatalf("sendCardToSession error = %v, want ErrPlatformNotRunning", err)
	}
	if err := e.ExecuteCronJob(context.Background(), &CronJob{ID: "j1", SessionKey: sessionKey, Prompt: "ping"}); !errors.Is(err, ErrPlatformNotRunning) {
		t.Fatalf("ExecuteCronJob error = %v, want ErrPlatformNotRunning", err)
	}
	if _, _, err := e.platformAndReplyContextForQueuedPrompt(queuedPrompt{OriginPlatform: "telegram", OriginSessionKey: sessionKey}); !errors.Is(err, ErrPlatformNotRunning) {
		t.Fatalf("queued prompt platform lookup error = %v, want ErrPlatformNotRunning", err)
	}
	if down := e.platformNotRunningNames(); !down["telegram"] || down["feishu"] {
		t.Fatalf("platformNotRunningNames = %v, want only telegram", down)
	}
	results := checkPlatforms(e.platforms, e.platformNotRunningNames())
	if results[0].Status != DoctorPass || results[1].Status != DoctorFail {
		t.Fatalf("doctor platform statuses = %v/%v, want pass/fail", results[0].Status, results[1].Status)
	}

	// The restart notice waits for the platform instead of being lost.
	e.SendRestartNotification("telegram", sessionKey)
	if got := flaky.snapshot(); len(got.sent) != 0 || got.sentUnstarted != 0 {
		t.Fatalf("sent %v (+%d before start) while platform was down, want nothing", got.sent, got.sentUnstarted)
	}

	close(gate)
	waitForCondition(t, func() bool { return len(flaky.snapshot().sent) == 1 }, "deferred restart notification")
	if got := flaky.snapshot().sent[0]; !strings.Contains(got, e.i18n.T(MsgRestartSuccess)) {
		t.Fatalf("deferred message = %q, want restart notification", got)
	}

	if err := e.sendTextToSession(sessionKey, "hello"); err != nil {
		t.Fatalf("sendTextToSession after retry succeeded = %v", err)
	}
	if got := flaky.snapshot(); len(got.sent) != 2 || got.sentUnstarted != 0 {
		t.Fatalf("sent = %v (+%d before start), want restart notice then hello", got.sent, got.sentUnstarted)
	}
	if len(e.platformNotRunningNames()) != 0 {
		t.Fatalf("platformNotRunningNames = %v after retry succeeded, want empty", e.platformNotRunningNames())
	}
}

func TestEngineSendPathsUnchangedWithoutStart(t *testing.T) {
	// Engines driven without Start (tests, tools) keep sending as before.
	p := &flakyPlatform{name: "telegram"}
	p.running = true
	e := newPlatformRetryTestEngine(p)
	if err := e.sendTextToSession("telegram:42:7", "hello"); err != nil {
		t.Fatalf("sendTextToSession = %v", err)
	}
	if got := p.snapshot().sent; len(got) != 1 {
		t.Fatalf("sent = %v, want 1 message", got)
	}
}

func TestNextPlatformRetryDelayCapsAtMax(t *testing.T) {
	d := defaultPlatformRetryInitialDelay
	var got []time.Duration
	for i := 0; i < 8; i++ {
		got = append(got, d)
		d = nextPlatformRetryDelay(d, defaultPlatformRetryMaxDelay)
	}
	want := []time.Duration{
		5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second,
		80 * time.Second, 160 * time.Second, 5 * time.Minute, 5 * time.Minute,
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("delays = %v, want %v", got, want)
		}
	}
}
