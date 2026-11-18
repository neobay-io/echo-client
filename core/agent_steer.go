package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
)

// Steering (T3.3): a message for the turn that is running, read by the agent
// after its current step instead of after the turn ends.

// steerTarget returns the chat's agent session if a turn is running in it
// and the agent can take a message mid-turn.
func (e *Engine) steerTarget(sessionKey string) Steerer {
	e.interactiveMu.Lock()
	state := e.interactiveStates[sessionKey]
	e.interactiveMu.Unlock()
	if state == nil || state.agentSession == nil || !state.agentSession.Alive() || !state.steerable() {
		return nil
	}
	st, _ := state.agentSession.(Steerer)
	return st
}

// steerText is the text after the command word, newlines kept.
func steerText(content string) string {
	content = strings.TrimSpace(content)
	if i := strings.IndexAny(content, " \t\r\n"); i >= 0 {
		return strings.TrimSpace(content[i+1:])
	}
	return ""
}

// cmdSteer sends a message into the running turn. Without a running turn
// the message is handled as an ordinary one.
func (e *Engine) cmdSteer(p Platform, msg *Message) {
	text := steerText(msg.Content)
	if text == "" && len(msg.Images)+len(msg.Files) == 0 {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgKey("steer_usage")))
		return
	}
	st := e.steerTarget(msg.SessionKey)
	if st == nil {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgKey("steer_no_turn")))
		next := *msg
		next.Content = text
		e.handleMessage(p, &next)
		return
	}
	steered := *msg // keeps the quoted message and attachments
	steered.Content = text
	if err := e.steer(st, msg.SessionKey, &steered); err != nil {
		e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgError), err))
		return
	}
	e.reply(p, msg.ReplyCtx, e.i18n.T(MsgKey("steer_done")))
}

// steer sends msg into the running turn and records it in the chat history.
func (e *Engine) steer(st Steerer, sessionKey string, msg *Message) error {
	session := e.sessions.GetOrCreateActive(sessionKey)
	prompt := e.buildAgentPrompt(session, msg)
	if err := st.Steer(prompt, msg.Images, msg.Files); err != nil {
		return err
	}
	session.AddHistory("user", prompt)
	e.sessions.Save()
	return nil
}

// steerQueuedItem moves a queued message into the running turn
// (`/queue steer <id>`, the "Insert now" button).
func (e *Engine) steerQueuedItem(sessionKey, queueKey, id string) string {
	st := e.steerTarget(sessionKey)
	if st == nil {
		return e.i18n.T(MsgKey("steer_no_turn_queued"))
	}
	item, ok := e.takePromptQueueItem(queueKey, id)
	if !ok {
		return e.i18n.T(MsgQueueItemNotFound)
	}
	if err := e.steer(st, sessionKey, &item.Message); err != nil {
		e.requeueItemFront(item)
		return fmt.Sprintf(e.i18n.T(MsgError), err)
	}
	return e.i18n.T(MsgKey("steer_done"))
}

// queuedReplyButtons offers "Insert now" for the message just queued as id,
// when a running turn the agent can take messages into is ahead of it.
func (e *Engine) queuedReplyButtons(sessionKey, id string) [][]ButtonOption {
	if id == "" || e.steerTarget(sessionKey) == nil {
		return nil
	}
	return [][]ButtonOption{{{Text: e.i18n.T(MsgKey("steer_btn_insert")), Data: "cmd:/queue steer " + id}}}
}

// replyQueued tells the user their message was queued (as id), with "Insert
// now" when it could go into the running turn instead.
func (e *Engine) replyQueued(p Platform, msg *Message, pos int, id string) {
	text := e.i18n.Tf(MsgQueueEnqueued, pos)
	if buttons := e.queuedReplyButtons(msg.SessionKey, id); buttons != nil {
		e.replyWithButtons(p, msg.ReplyCtx, text, buttons)
		return
	}
	e.reply(p, msg.ReplyCtx, text)
}

func (e *Engine) lastQueuedItemID(queueKey string) string {
	e.promptQueueMu.Lock()
	defer e.promptQueueMu.Unlock()
	state := e.promptQueues[queueKey]
	if state == nil || len(state.Items) == 0 {
		return ""
	}
	return state.Items[len(state.Items)-1].ID
}

// takePromptQueueItem removes a user message from a queue by ID.
func (e *Engine) takePromptQueueItem(queueKey, id string) (queuedPrompt, bool) {
	e.promptQueueMu.Lock()
	defer e.promptQueueMu.Unlock()
	state := e.promptQueues[queueKey]
	if state == nil {
		return queuedPrompt{}, false
	}
	for i, item := range state.Items {
		if item.ID == id && !item.isCron() {
			state.Items = append(state.Items[:i], state.Items[i+1:]...)
			return item, true
		}
	}
	return queuedPrompt{}, false
}

// requeueItemFront puts an item back at the head of its queue (a steer that
// failed), without changing whether the queue is running.
func (e *Engine) requeueItemFront(item queuedPrompt) {
	e.promptQueueMu.Lock()
	defer e.promptQueueMu.Unlock()
	state := e.promptQueues[item.QueueKey]
	if state == nil {
		state = &promptQueueState{}
		e.promptQueues[item.QueueKey] = state
	}
	state.Items = append([]queuedPrompt{item}, state.Items...)
}

// cmdCancel (T5.4) stops the chat's running turn, drops its queued
// messages and starts a new session: /stop keeps the session and the
// queue, /new is for an idle chat.
func (e *Engine) cmdCancel(p Platform, msg *Message) {
	unlockTransition := e.lockSessionTransition(msg.SessionKey)
	defer unlockTransition()

	// The new session first: from here on a turn of the old session cannot
	// take over the chat (getOrCreateInteractiveState checks the session),
	// and nothing queued against it can start.
	s := e.sessions.NewSession(msg.SessionKey, "")
	dropped := e.cancelQueuedPromptsForSessionSwitch(msg.SessionKey, s.ID, s.AgentSessionID)
	wasRunning := e.stopCronRun(msg.SessionKey)

	e.interactiveMu.Lock()
	state := e.interactiveStates[msg.SessionKey]
	e.interactiveMu.Unlock()
	quiet, hadState := false, state != nil
	if state != nil {
		wasRunning = wasRunning || state.runningTurn() != nil
		state.mu.Lock()
		pending := state.pending
		state.pending = nil
		quiet = state.quiet
		state.mu.Unlock()
		if pending != nil {
			pending.resolve()
		}
	}
	e.cleanupInteractiveState(msg.SessionKey)
	if hadState {
		e.interactiveMu.Lock()
		if _, ok := e.interactiveStates[msg.SessionKey]; !ok {
			e.interactiveStates[msg.SessionKey] = newInteractiveState(nil, p, msg.ReplyCtx, quiet)
		}
		e.interactiveMu.Unlock()
	}
	slog.Info("cmdCancel: stopped and started a new session", "session_key", msg.SessionKey, "was_running", wasRunning, "dropped_queued", dropped)
	e.reply(p, msg.ReplyCtx, fmt.Sprintf(e.i18n.T(MsgKey("cancel_done")), dropped))
}

// cronRun is a chat's running scheduled turn (cron, loop, timer), which
// runs on its own agent session outside interactiveStates.
type cronRun struct {
	cancel  context.CancelFunc
	stopped atomic.Bool
}

// errCronStoppedByUser is the result of a scheduled turn stopped by /stop
// or /cancel.
var errCronStoppedByUser = errors.New("stopped by the user")

func (e *Engine) trackCronRun(sessionKey string, ctx context.Context) (context.Context, *cronRun) {
	ctx, cancel := context.WithCancel(ctx)
	run := &cronRun{cancel: cancel}
	e.cronRunsMu.Lock()
	if e.cronRuns == nil {
		e.cronRuns = map[string]*cronRun{}
	}
	e.cronRuns[sessionKey] = run
	e.cronRunsMu.Unlock()
	return ctx, run
}

func (e *Engine) untrackCronRun(sessionKey string, run *cronRun) {
	run.cancel()
	e.cronRunsMu.Lock()
	if e.cronRuns[sessionKey] == run {
		delete(e.cronRuns, sessionKey)
	}
	e.cronRunsMu.Unlock()
}

// stopCronRun stops the chat's running scheduled turn, if any.
func (e *Engine) stopCronRun(sessionKey string) bool {
	e.cronRunsMu.Lock()
	run := e.cronRuns[sessionKey]
	e.cronRunsMu.Unlock()
	if run == nil {
		return false
	}
	run.stopped.Store(true)
	run.cancel()
	return true
}

// errSessionReplaced: the chat started another session while a message
// waited to start its turn.
var errSessionReplaced = errors.New("the chat started another session")

// sessionReplaced reports (and tells the chat) that a message's turn was
// dropped because the chat moved to another session (/new, /switch,
// /cancel) while the turn waited, e.g. for another chat sharing its agent
// session.
func (e *Engine) sessionReplaced(p Platform, msg *Message, session *Session) bool {
	active := e.sessions.ActiveSessionID(msg.SessionKey)
	if active == "" || active == session.ID {
		return false
	}
	slog.Info("dropping a turn of a replaced session", "session_key", msg.SessionKey, "session", session.ID, "active", active)
	e.reply(p, msg.ReplyCtx, e.i18n.T(MsgQueueSkippedSessionChanged))
	return true
}
