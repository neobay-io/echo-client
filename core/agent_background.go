package core

import (
	"log/slog"
	"strings"
)

// BackgroundReporter is implemented by agent sessions that can produce
// output outside the engine's turns: a turn the agent starts by itself
// (Claude Code, when a background task it started finishes) or its answer
// to a message steered in after the turn's result. The handler gets the
// text of each such turn when it ends, from the session's reader; it must
// not block.
type BackgroundReporter interface {
	OnBackgroundOutput(handler func(BackgroundOutput))
}

// BackgroundOutput is the text of a turn the agent ran by itself after the
// engine's last turn got its answer (with a prompt of the engine's waiting,
// the session sends EventBackground into that turn instead). It is posted
// once the engine has finished the turn running at the time.
type BackgroundOutput struct {
	Text string
}

type queuedBackground struct {
	text  string
	after <-chan struct{} // the turn running when it came, or nil
}

// backgroundQueueSize bounds the background outputs waiting for delivery.
const backgroundQueueSize = 16

// watchBackgroundOutput posts a chat process's background output to the
// chat and keeps it in the session's history, through one ordered worker
// (the agent's reader only queues it).
func (e *Engine) watchBackgroundOutput(state *interactiveState, session *Session) {
	bg, ok := state.agentSession.(BackgroundReporter)
	if !ok {
		return
	}
	queue := make(chan queuedBackground, backgroundQueueSize)
	go e.deliverBackgroundOutput(state, session, queue)
	bg.OnBackgroundOutput(func(out BackgroundOutput) {
		text := strings.TrimSpace(out.Text)
		if text == "" {
			return
		}
		// The turn running now got its answer just before this; a turn
		// that starts later comes after this output.
		item := queuedBackground{text: text, after: state.runningTurn()}
		select {
		case queue <- item:
		case <-state.stopped:
		default:
			slog.Warn("agent output outside a turn dropped: too much waiting", "session", session.ID)
		}
	})
}

// deliverBackgroundOutput posts queued output in order, each once the
// engine has finished the turn that was running when it came (and posted
// that turn's answer), until the chat's process is retired or the engine
// stops.
func (e *Engine) deliverBackgroundOutput(state *interactiveState, session *Session, queue <-chan queuedBackground) {
	for {
		var item queuedBackground
		select {
		case item = <-queue:
		case <-state.stopped:
			return
		case <-e.ctx.Done():
			return
		}
		if item.after != nil {
			select {
			case <-item.after:
			case <-state.stopped:
				return
			case <-e.ctx.Done():
				return
			}
		}
		select {
		case <-state.stopped:
			return
		default:
		}
		state.mu.Lock()
		p, replyCtx := state.platform, state.replyCtx
		state.mu.Unlock()
		if p == nil {
			continue
		}
		text := item.text
		slog.Info("agent output outside a turn", "session", session.ID, "chars", len(text))
		session.AddHistory("assistant", text)
		e.sessions.Save()
		for _, chunk := range splitMessage(text, maxPlatformMessageLen) {
			e.send(p, replyCtx, chunk)
		}
	}
}

// ExclusiveSessions is implemented by agents whose session can be open in
// one process at a time (Codex app-server refuses thread/resume while
// another process holds the thread; Kimi Code's ACP server refuses
// session/resume). A background turn that resumes a chat's session first
// closes the chat's idle process; the chat resumes the session (with what
// the background turn added) on its next message.
type ExclusiveSessions interface {
	SessionsAreExclusive() bool
}

// releaseChatProcesses closes the idle chat processes holding agent session
// id, for an agent with exclusive sessions. The caller holds the session's
// turn lock, so no chat turn runs on it.
func (e *Engine) releaseChatProcesses(id string) {
	ex, ok := e.agent.(ExclusiveSessions)
	if id == "" || !ok || !ex.SessionsAreExclusive() {
		return
	}
	// Detach exactly the states found, in one step: a /switch may install
	// another process under the same chat key right after.
	type held struct {
		key   string
		state *interactiveState
	}
	var found []held
	e.interactiveMu.Lock()
	for key, st := range e.interactiveStates {
		if st != nil && st.agentSession != nil && st.agentSession.CurrentSessionID() == id {
			delete(e.interactiveStates, key)
			found = append(found, held{key, st})
		}
	}
	e.interactiveMu.Unlock()
	for _, h := range found {
		slog.Info("closing the chat's agent process so a background turn can resume its session", "session_key", h.key, "agent_session", id)
		e.clearInteractionPrompt(h.key)
		e.closeInteractiveState(h.state)
	}
}
