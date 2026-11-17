package core

import "strings"

// perSessionSettings reports whether /model, /effort and /mode are stored on
// the chat session (agent implements SessionOptionsInjector) instead of being
// applied to the shared, project-wide agent.
func (e *Engine) perSessionSettings() bool {
	_, ok := e.agent.(SessionOptionsInjector)
	return ok
}

// effectiveModel is the model the given chat session runs with, using the same
// precedence as the Claude / Codex adapters at start: the chat's /model
// choice, then the active provider's model, then the project model.
func (e *Engine) effectiveModel(s *Session) string {
	if s != nil && e.perSessionSettings() {
		if model := strings.TrimSpace(s.Settings().Model); model != "" {
			return model
		}
	}
	if ps, ok := e.agent.(ProviderSwitcher); ok {
		if provider := ps.GetActiveProvider(); provider != nil && strings.TrimSpace(provider.Model) != "" {
			return provider.Model
		}
	}
	if ms, ok := e.agent.(ModelSwitcher); ok {
		return ms.GetModel()
	}
	return ""
}

// effectiveReasoningLevel is the reasoning level the given chat session runs with.
func (e *Engine) effectiveReasoningLevel(s *Session) string {
	if s != nil && e.perSessionSettings() {
		if level := strings.TrimSpace(s.Settings().ReasoningLevel); level != "" {
			return level
		}
	}
	if rs, ok := e.agent.(ReasoningLevelSwitcher); ok {
		return rs.GetReasoningLevel()
	}
	return ""
}

// effectiveMode is the permission mode the given chat session runs with.
func (e *Engine) effectiveMode(s *Session) string {
	if s != nil && e.perSessionSettings() {
		if mode := strings.TrimSpace(s.Settings().Mode); mode != "" {
			return mode
		}
	}
	if ms, ok := e.agent.(ModeSwitcher); ok {
		return ms.GetMode()
	}
	return ""
}

// applySessionSetting records a /model, /effort or /mode choice. With
// per-session settings it updates only this chat's active session; otherwise
// legacy applies it to the shared agent. Either way the chat keeps its agent
// session (context survives) and its process is retired before the next turn,
// so a turn already running is not interrupted.
func (e *Engine) applySessionSetting(sessionKey string, update func(*AgentSessionOptions), legacy func()) {
	// Same lock as /new and /switch: otherwise /new could copy the old
	// settings into a new active session between our lookup and update, and
	// e.g. "/mode default" would leave the new session in bypass mode.
	unlockTransition := e.lockSessionTransition(sessionKey)
	defer unlockTransition()
	if e.perSessionSettings() {
		s := e.sessions.GetOrCreateActive(sessionKey)
		s.UpdateSettings(update)
		e.sessions.Save()
	} else if legacy != nil {
		legacy()
	}
	e.retireInteractiveState(sessionKey)
}

// retireInteractiveState makes the next turn of sessionKey start a fresh agent
// process (resuming the same agent session) without stopping a running turn.
func (e *Engine) retireInteractiveState(sessionKey string) {
	e.interactiveMu.Lock()
	defer e.interactiveMu.Unlock()
	if state, ok := e.interactiveStates[sessionKey]; ok && state != nil {
		state.stale = true
	}
}
