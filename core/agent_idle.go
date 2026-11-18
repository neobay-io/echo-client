package core

import (
	"log/slog"
	"time"
)

// Idle agent processes (T5.4, agent_session_idle_timeout_mins). A chat's
// resident agent process (Claude Code, the Codex app-server) that has had
// no turn for the configured time is closed to free memory; the chat keeps
// its agent session ID, so the next message starts a process that resumes
// it. Only idle chats are touched: no running turn, no pending question or
// approval, and the chat's session not busy.

// agentIdleSweepEvery is how often idle processes are looked for.
var agentIdleSweepEvery = time.Minute

// SetAgentSessionIdleTimeout sets how long a chat's agent process may sit
// idle before it is closed; 0 keeps processes until they exit.
func (e *Engine) SetAgentSessionIdleTimeout(d time.Duration) {
	if d < 0 {
		d = 0
	}
	e.agentIdleTimeout.Store(int64(d))
}

func (e *Engine) runAgentIdleJanitor() {
	ticker := time.NewTicker(agentIdleSweepEvery)
	defer ticker.Stop()
	for {
		select {
		case <-e.ctx.Done():
			return
		case now := <-ticker.C:
			e.closeIdleAgentSessions(now)
		}
	}
}

// touch records activity on the chat's agent process.
func (s *interactiveState) touch() {
	s.mu.Lock()
	s.lastActive = time.Now()
	s.mu.Unlock()
}

// idleSince reports whether the state's process has been idle since before
// cutoff (s.mu held).
func (s *interactiveState) idleBeforeLocked(cutoff time.Time) bool {
	return s.agentSession != nil && s.turnDone == nil && s.pending == nil && s.lastActive.Before(cutoff)
}

// closeIdleAgentSessions closes the agent processes idle for longer than the
// configured timeout and reports how many it closed.
func (e *Engine) closeIdleAgentSessions(now time.Time) int {
	timeout := time.Duration(e.agentIdleTimeout.Load())
	if timeout <= 0 {
		return 0
	}
	cutoff := now.Add(-timeout)

	e.interactiveMu.Lock()
	candidates := make(map[string]*interactiveState)
	for key, state := range e.interactiveStates {
		if state == nil {
			continue
		}
		state.mu.Lock()
		idle := state.idleBeforeLocked(cutoff)
		state.mu.Unlock()
		if idle {
			candidates[key] = state
		}
	}
	e.interactiveMu.Unlock()

	closed := 0
	for key, state := range candidates {
		if e.closeIdleAgentSession(key, state, cutoff) {
			closed++
		}
	}
	return closed
}

func (e *Engine) closeIdleAgentSession(key string, state *interactiveState, cutoff time.Time) bool {
	// Holding the chat's session busy keeps a new turn from starting while
	// the process is closed.
	session := e.sessions.ActiveSession(key)
	if session != nil {
		session.mu.Lock()
		updatedAt := session.UpdatedAt
		session.mu.Unlock()
		if !session.TryLock() {
			return false
		}
		defer func() {
			session.Unlock()
			session.mu.Lock()
			session.UpdatedAt = updatedAt // closing a process is not chat activity
			session.mu.Unlock()
			// A message that arrived meanwhile found the chat busy and was
			// queued; no turn is running to pick it up, so start it here.
			queueKey, _ := e.promptQueueKey(key, session)
			go e.drainPromptQueue(queueKey)
		}()
	}

	e.interactiveMu.Lock()
	if e.interactiveStates[key] != state {
		e.interactiveMu.Unlock()
		return false
	}
	state.mu.Lock()
	if !state.idleBeforeLocked(cutoff) {
		state.mu.Unlock()
		e.interactiveMu.Unlock()
		return false
	}
	// Keep the chat's /quiet choice for the next process.
	e.interactiveStates[key] = newInteractiveState(nil, state.platform, state.replyCtx, state.quiet)
	idleFor := time.Since(state.lastActive)
	state.mu.Unlock()
	e.interactiveMu.Unlock()

	slog.Info("closing idle agent process; the next message resumes the session", "project", e.name, "session_key", key, "idle", idleFor.Round(time.Second))
	e.closeInteractiveState(state)
	return true
}
