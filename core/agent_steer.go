package core

import (
	"fmt"
	"strings"
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
