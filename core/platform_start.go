package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

const (
	defaultPlatformRetryInitialDelay = 5 * time.Second
	defaultPlatformRetryMaxDelay     = 5 * time.Minute
	// defaultPlatformRetryStopWait bounds how long Stop waits for retry
	// goroutines. One that is sleeping between attempts exits at once; one
	// blocked inside Platform.Start (which takes no context) cannot be
	// interrupted, so Stop does not hold shutdown hostage to it. That goroutine
	// still exits, and stops the platform, as soon as Start returns.
	defaultPlatformRetryStopWait = 5 * time.Second
)

// ErrPlatformNotRunning is returned when a message targets a platform whose
// Start failed and has not succeeded on a background retry. Sending through a
// platform that never started can fail obscurely or panic (Telegram, for
// example, has no bot client until Start succeeds), so such sends are refused.
var ErrPlatformNotRunning = errors.New("is not running: it failed to start and is being retried")

// platformRunState tracks a platform's lifecycle so start retries never race
// with Stop or with each other. All transitions happen under
// Engine.platformRunMu.
type platformRunState int

const (
	// platformStateUnknown: Engine.Start has not run. Treated as available so
	// engines driven without Start (tests, tools) behave as before.
	platformStateUnknown platformRunState = iota
	platformStateRunning
	// platformStateRetrying: Start failed; a retry goroutine is sleeping
	// before its next attempt.
	platformStateRetrying
	// platformStateStarting: a retry goroutine is inside Platform.Start.
	platformStateStarting
	// platformStateStopped: Engine.Stop ran before the platform came up.
	platformStateStopped
)

type platformRun struct {
	state platformRunState
	// onRunning holds work deferred until a retry brings the platform up
	// (e.g. the restart notification). Dropped if the engine stops first.
	onRunning []func()
}

// beginPlatformRuns prepares per-platform state and the retry context for
// Engine.Start. Calling Start twice would start every platform twice, so it is
// refused.
func (e *Engine) beginPlatformRuns() (context.Context, error) {
	e.platformRunMu.Lock()
	defer e.platformRunMu.Unlock()
	if e.platformRuns != nil {
		return nil, fmt.Errorf("[%s] engine already started", e.name)
	}
	if e.ctx.Err() != nil {
		return nil, fmt.Errorf("[%s] engine already stopped", e.name)
	}
	e.platformRuns = make([]platformRun, len(e.platforms))
	ctx, cancel := context.WithCancel(e.ctx)
	e.platformRetryCancel = cancel
	return ctx, nil
}

// beginInitialStart moves a platform into starting for its first Start call.
// It reports false if Engine.Stop already ran.
func (e *Engine) beginInitialStart(idx int) bool {
	e.platformRunMu.Lock()
	defer e.platformRunMu.Unlock()
	if e.platformRuns[idx].state == platformStateStopped {
		return false
	}
	e.platformRuns[idx].state = platformStateStarting
	return true
}

// afterPlatformStarted runs the steps that follow a successful Platform.Start,
// whether on the first try (attempt 0) or on a background retry.
func (e *Engine) afterPlatformStarted(p Platform, attempt int) {
	if attempt > 0 {
		slog.Info("platform started", "project", e.name, "platform", p.Name(), "retry_attempt", attempt)
	} else {
		slog.Info("platform started", "project", e.name, "platform", p.Name())
	}

	// Register commands on platforms that support it (e.g. Telegram setMyCommands)
	if registrar, ok := p.(CommandRegistrar); ok {
		commands := e.GetAllCommands()
		if err := registrar.RegisterCommands(commands); err != nil {
			slog.Error("platform command registration failed", "project", e.name, "platform", p.Name(), "error", err)
		} else {
			slog.Debug("platform commands registered", "project", e.name, "platform", p.Name(), "count", len(commands))
		}
	}
}

// scheduleStartRetry starts the single retry goroutine for a platform whose
// first Start failed.
func (e *Engine) scheduleStartRetry(ctx context.Context, idx int, p Platform) {
	e.platformRunMu.Lock()
	e.platformRuns[idx].state = platformStateRetrying
	e.platformRunMu.Unlock()

	e.platformRetryWG.Add(1)
	go e.retryPlatformStart(ctx, idx, p)
}

func (e *Engine) retryPlatformStart(ctx context.Context, idx int, p Platform) {
	defer e.platformRetryWG.Done()

	delay := e.platformRetryInitialDelay()
	for attempt := 1; ; attempt++ {
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}

		if !e.beginStartAttempt(ctx, idx) {
			return
		}
		err := p.Start(e.handleMessage)
		onRunning, stopped := e.endStartAttempt(idx, err == nil)
		if stopped {
			// Engine.Stop ran during Start and skipped this platform, so
			// stopping it is this goroutine's job, as Stop does for every
			// other platform whether or not it started.
			if stopErr := p.Stop(); stopErr != nil {
				slog.Warn("platform stop after cancelled start retry failed", "project", e.name, "platform", p.Name(), "error", stopErr)
			}
			return
		}
		if err == nil {
			e.afterPlatformStarted(p, attempt)
			for _, fn := range onRunning {
				fn()
			}
			return
		}

		delay = nextPlatformRetryDelay(delay, e.platformRetryMaxDelay())
		slog.Warn("platform start retry failed", "project", e.name, "platform", p.Name(), "attempt", attempt, "error", err, "retry_in", delay)
	}
}

// beginStartAttempt moves a platform from retrying to starting. It reports
// false if the engine is stopping, in which case no Start call may be made.
func (e *Engine) beginStartAttempt(ctx context.Context, idx int) bool {
	e.platformRunMu.Lock()
	defer e.platformRunMu.Unlock()
	if ctx.Err() != nil || e.platformRuns[idx].state != platformStateRetrying {
		return false
	}
	e.platformRuns[idx].state = platformStateStarting
	return true
}

// endStartAttempt records a retry's outcome. stopped reports that Engine.Stop
// ran while Start was in flight. On success it hands back the deferred work.
func (e *Engine) endStartAttempt(idx int, ok bool) (onRunning []func(), stopped bool) {
	e.platformRunMu.Lock()
	defer e.platformRunMu.Unlock()
	r := &e.platformRuns[idx]
	if r.state == platformStateStopped {
		return nil, true
	}
	if !ok {
		r.state = platformStateRetrying
		return nil, false
	}
	r.state = platformStateRunning
	onRunning, r.onRunning = r.onRunning, nil
	return onRunning, false
}

// stopPlatformRetries cancels retries and marks every platform that is not up
// as stopped. It returns the indexes whose retry is inside Platform.Start;
// their retry goroutines own stopping them.
func (e *Engine) stopPlatformRetries() map[int]bool {
	e.platformRunMu.Lock()
	defer e.platformRunMu.Unlock()
	if e.platformRetryCancel != nil {
		e.platformRetryCancel()
	}
	inFlight := make(map[int]bool)
	for i := range e.platformRuns {
		r := &e.platformRuns[i]
		switch r.state {
		case platformStateStarting:
			inFlight[i] = true
			r.state = platformStateStopped
		case platformStateRetrying:
			r.state = platformStateStopped
		}
		r.onRunning = nil
	}
	return inFlight
}

// waitPlatformRetries waits, up to platformRetryStopWait, for retry
// goroutines to exit after stopPlatformRetries.
func (e *Engine) waitPlatformRetries() {
	done := make(chan struct{})
	go func() {
		e.platformRetryWG.Wait()
		close(done)
	}()
	wait := e.platformRetryStopWait
	if wait <= 0 {
		wait = defaultPlatformRetryStopWait
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		slog.Warn("platform start retry still in progress at shutdown; it will stop the platform when Start returns", "project", e.name, "waited", wait)
	}
}

// platformRunning reports whether the platform at idx can be sent through:
// it started (now or on a retry), or Engine.Start has not run.
func (e *Engine) platformRunning(idx int) bool {
	e.platformRunMu.Lock()
	defer e.platformRunMu.Unlock()
	if idx >= len(e.platformRuns) {
		return true
	}
	s := e.platformRuns[idx].state
	return s == platformStateUnknown || s == platformStateRunning
}

// runningPlatform returns the configured platform with the given name, or an
// error if there is none or it has not started (see ErrPlatformNotRunning).
func (e *Engine) runningPlatform(name string) (Platform, error) {
	for i, p := range e.platforms {
		if p.Name() != name {
			continue
		}
		if !e.platformRunning(i) {
			return nil, fmt.Errorf("platform %q %w", name, ErrPlatformNotRunning)
		}
		return p, nil
	}
	return nil, fmt.Errorf("platform %q not found", name)
}

// whenPlatformRunning calls fn with the named platform now if it is running.
// If its start is being retried, fn is deferred until a retry succeeds (and
// dropped if the engine stops first); deferred reports that case. fn is
// dropped for an unknown name or a platform that never came up before Stop.
func (e *Engine) whenPlatformRunning(name string, fn func(Platform)) (deferred bool) {
	for i, p := range e.platforms {
		if p.Name() != name {
			continue
		}
		e.platformRunMu.Lock()
		state := platformStateUnknown
		if i < len(e.platformRuns) {
			state = e.platformRuns[i].state
		}
		switch state {
		case platformStateRetrying, platformStateStarting:
			e.platformRuns[i].onRunning = append(e.platformRuns[i].onRunning, func() { fn(p) })
			e.platformRunMu.Unlock()
			return true
		case platformStateStopped:
			e.platformRunMu.Unlock()
			return false
		}
		e.platformRunMu.Unlock()
		fn(p)
		return false
	}
	return false
}

// platformNotRunningNames lists platforms that are configured but down.
func (e *Engine) platformNotRunningNames() map[string]bool {
	down := make(map[string]bool)
	for i, p := range e.platforms {
		if !e.platformRunning(i) {
			down[p.Name()] = true
		}
	}
	return down
}

func (e *Engine) platformRetryInitialDelay() time.Duration {
	if e.platformRetryInitial > 0 {
		return e.platformRetryInitial
	}
	return defaultPlatformRetryInitialDelay
}

func (e *Engine) platformRetryMaxDelay() time.Duration {
	if e.platformRetryMax > 0 {
		return e.platformRetryMax
	}
	return defaultPlatformRetryMaxDelay
}

// nextPlatformRetryDelay doubles d, capped at maxDelay.
func nextPlatformRetryDelay(d, maxDelay time.Duration) time.Duration {
	if d <= 0 {
		return maxDelay
	}
	if d >= maxDelay/2 {
		return maxDelay
	}
	return d * 2
}
