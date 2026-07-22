package core

import (
	"fmt"
	"log/slog"
	"strings"
	"time"
)

const (
	maxPromptQueueItems           = 20
	maxPromptQueueAttachmentBytes = 50 * 1024 * 1024
	promptQueuePreviewRunes       = 120
)

type queuedPrompt struct {
	ID                     string
	QueueKey               string
	OriginSessionKey       string
	OriginPlatform         string
	AgentSessionIDSnapshot string
	Message                Message
	CreatedAt              time.Time
}

type promptQueueState struct {
	Running bool
	Items   []queuedPrompt
}

type promptQueueSnapshot struct {
	QueueKey string
	Running  bool
	Items    []queuedPrompt
}

func (e *Engine) promptQueueKey(sessionKey string, session *Session) (string, string) {
	agentName, agentSessionID := e.agentSessionTurnIdentity(session)
	if strings.TrimSpace(agentSessionID) != "" {
		return agentName + ":" + strings.TrimSpace(agentSessionID), strings.TrimSpace(agentSessionID)
	}
	localSessionID := ""
	if session != nil {
		session.mu.Lock()
		localSessionID = strings.TrimSpace(session.ID)
		session.mu.Unlock()
	}
	if localSessionID == "" {
		localSessionID = "default"
	}
	return "pending:" + sessionKey + ":" + localSessionID, ""
}

func cloneQueuedMessage(msg *Message) Message {
	if msg == nil {
		return Message{}
	}
	out := *msg
	out.Images = cloneImages(msg.Images)
	out.Files = cloneFiles(msg.Files)
	if msg.Audio != nil {
		audio := *msg.Audio
		audio.Data = append([]byte(nil), msg.Audio.Data...)
		out.Audio = &audio
	}
	return out
}

func promptQueueAttachmentBytes(msg *Message) int {
	if msg == nil {
		return 0
	}
	total := 0
	for _, img := range msg.Images {
		total += len(img.Data)
	}
	for _, file := range msg.Files {
		total += len(file.Data)
	}
	return total
}

func promptQueuePreview(content string) string {
	content = strings.Join(strings.Fields(strings.TrimSpace(content)), " ")
	if content == "" {
		return "(attachments only)"
	}
	runes := []rune(content)
	if len(runes) > promptQueuePreviewRunes {
		return string(runes[:promptQueuePreviewRunes]) + "..."
	}
	return content
}

func (e *Engine) beginPromptTurnOrEnqueue(queueKey, agentSessionID string, msg *Message) (bool, int, error) {
	e.promptQueueMu.Lock()
	defer e.promptQueueMu.Unlock()

	state := e.promptQueues[queueKey]
	if state == nil {
		state = &promptQueueState{}
		e.promptQueues[queueKey] = state
	}
	if state.Running || len(state.Items) > 0 {
		pos, err := e.enqueuePromptLocked(state, queueKey, agentSessionID, msg)
		return false, pos, err
	}
	state.Running = true
	return true, 0, nil
}

func (e *Engine) enqueuePrompt(queueKey, agentSessionID string, msg *Message) (int, error) {
	e.promptQueueMu.Lock()
	defer e.promptQueueMu.Unlock()

	state := e.promptQueues[queueKey]
	if state == nil {
		state = &promptQueueState{}
		e.promptQueues[queueKey] = state
	}
	return e.enqueuePromptLocked(state, queueKey, agentSessionID, msg)
}

func (e *Engine) enqueuePromptLocked(state *promptQueueState, queueKey, agentSessionID string, msg *Message) (int, error) {
	if len(state.Items) >= maxPromptQueueItems {
		return 0, fmt.Errorf("queue full")
	}
	totalBytes := promptQueueAttachmentBytes(msg)
	for i := range state.Items {
		totalBytes += promptQueueAttachmentBytes(&state.Items[i].Message)
	}
	if totalBytes > maxPromptQueueAttachmentBytes {
		return 0, fmt.Errorf("queue attachments too large")
	}
	e.promptQueueSeq++
	item := queuedPrompt{
		ID:                     fmt.Sprintf("q%d", e.promptQueueSeq),
		QueueKey:               queueKey,
		OriginSessionKey:       msg.SessionKey,
		OriginPlatform:         msg.Platform,
		AgentSessionIDSnapshot: strings.TrimSpace(agentSessionID),
		Message:                cloneQueuedMessage(msg),
		CreatedAt:              time.Now(),
	}
	state.Items = append(state.Items, item)
	return len(state.Items), nil
}

func (e *Engine) completePromptTurn(startKey string, sessionKey string, session *Session) string {
	finalKey, finalAgentSessionID := e.promptQueueKey(sessionKey, session)

	e.promptQueueMu.Lock()
	defer e.promptQueueMu.Unlock()

	shouldClearRunning := true
	if startKey != "" && finalKey != "" && startKey != finalKey {
		if oldState := e.promptQueues[startKey]; oldState != nil {
			finalState := e.promptQueues[finalKey]
			if finalState == nil {
				finalState = &promptQueueState{}
				e.promptQueues[finalKey] = finalState
			}
			finalAlreadyRunning := finalState.Running
			for i := range oldState.Items {
				oldState.Items[i].QueueKey = finalKey
				if finalAgentSessionID != "" && oldState.Items[i].AgentSessionIDSnapshot == "" {
					oldState.Items[i].AgentSessionIDSnapshot = finalAgentSessionID
				}
			}
			finalState.Items = append(finalState.Items, oldState.Items...)
			finalState.Running = oldState.Running || finalState.Running
			shouldClearRunning = !finalAlreadyRunning
			delete(e.promptQueues, startKey)
		}
	}

	state := e.promptQueues[finalKey]
	if state == nil {
		return finalKey
	}
	if shouldClearRunning {
		state.Running = false
	}
	if !state.Running && len(state.Items) == 0 {
		delete(e.promptQueues, finalKey)
	}
	return finalKey
}

func (e *Engine) drainPromptQueue(queueKey string) {
	item, ok := e.popNextQueuedPrompt(queueKey)
	if !ok {
		return
	}
	_ = e.startQueuedPrompt(item)
}

func (e *Engine) popNextQueuedPrompt(queueKey string) (queuedPrompt, bool) {
	e.promptQueueMu.Lock()
	defer e.promptQueueMu.Unlock()

	state := e.promptQueues[queueKey]
	if state == nil || state.Running || len(state.Items) == 0 {
		return queuedPrompt{}, false
	}
	item := state.Items[0]
	copy(state.Items, state.Items[1:])
	state.Items = state.Items[:len(state.Items)-1]
	state.Running = true
	return item, true
}

func (e *Engine) requeuePromptFront(item queuedPrompt) {
	e.promptQueueMu.Lock()
	defer e.promptQueueMu.Unlock()

	state := e.promptQueues[item.QueueKey]
	if state == nil {
		state = &promptQueueState{}
		e.promptQueues[item.QueueKey] = state
	}
	state.Running = false
	state.Items = append([]queuedPrompt{item}, state.Items...)
}

func (e *Engine) failQueuedPrompt(item queuedPrompt, reason string) {
	e.promptQueueMu.Lock()
	state := e.promptQueues[item.QueueKey]
	if state != nil {
		state.Running = false
		if len(state.Items) == 0 {
			delete(e.promptQueues, item.QueueKey)
		}
	}
	e.promptQueueMu.Unlock()

	if strings.TrimSpace(reason) != "" {
		if err := e.sendTextToSession(item.OriginSessionKey, reason); err != nil {
			slog.Warn("prompt queue: failed to notify skipped item", "session_key", item.OriginSessionKey, "error", err)
		}
	}
	go e.drainPromptQueue(item.QueueKey)
}

func (e *Engine) startQueuedPrompt(item queuedPrompt) bool {
	session := e.sessions.GetOrCreateActive(item.OriginSessionKey)
	_, currentAgentSessionID := e.promptQueueKey(item.OriginSessionKey, session)
	if item.AgentSessionIDSnapshot != "" && currentAgentSessionID != item.AgentSessionIDSnapshot {
		e.failQueuedPrompt(item, e.i18n.T(MsgQueueSkippedSessionChanged))
		return true
	}
	if !session.TryLock() {
		e.requeuePromptFront(item)
		return false
	}

	p, replyCtx, err := e.platformAndReplyContextForQueuedPrompt(item)
	if err != nil {
		session.Unlock()
		e.failQueuedPrompt(item, fmt.Sprintf(e.i18n.T(MsgQueueSkipped), err))
		return true
	}
	msg := item.Message
	msg.ReplyCtx = replyCtx
	go e.processInteractiveMessageAndDrainQueue(p, &msg, session, item.QueueKey)
	return true
}

func (e *Engine) platformAndReplyContextForQueuedPrompt(item queuedPrompt) (Platform, any, error) {
	for _, p := range e.platforms {
		if p.Name() == item.OriginPlatform {
			if item.Message.ReplyCtx != nil {
				return p, item.Message.ReplyCtx, nil
			}
			if rc, ok := p.(ReplyContextReconstructor); ok {
				replyCtx, err := rc.ReconstructReplyCtx(item.OriginSessionKey)
				return p, replyCtx, err
			}
			return p, nil, nil
		}
	}
	return nil, nil, fmt.Errorf("platform %q not found", item.OriginPlatform)
}

func (e *Engine) processInteractiveMessageAndDrainQueue(p Platform, msg *Message, session *Session, queueKey string) {
	e.processInteractiveMessage(p, msg, session)
	finalKey := e.completePromptTurn(queueKey, msg.SessionKey, session)
	e.drainPromptQueue(finalKey)
}

func (e *Engine) queueSnapshotForSession(sessionKey string) promptQueueSnapshot {
	session := e.sessions.GetOrCreateActive(sessionKey)
	queueKey, _ := e.promptQueueKey(sessionKey, session)
	e.promptQueueMu.Lock()
	defer e.promptQueueMu.Unlock()

	state := e.promptQueues[queueKey]
	if state == nil {
		return promptQueueSnapshot{QueueKey: queueKey}
	}
	items := make([]queuedPrompt, len(state.Items))
	copy(items, state.Items)
	return promptQueueSnapshot{QueueKey: queueKey, Running: state.Running, Items: items}
}

func (e *Engine) cancelQueuedPromptsForSessionSwitch(sessionKey, newAgentSessionID string) int {
	e.promptQueueMu.Lock()
	defer e.promptQueueMu.Unlock()

	cancelled := 0
	for key, state := range e.promptQueues {
		if state == nil || len(state.Items) == 0 {
			continue
		}
		kept := state.Items[:0]
		for _, item := range state.Items {
			if item.OriginSessionKey == sessionKey &&
				item.AgentSessionIDSnapshot != "" &&
				item.AgentSessionIDSnapshot != newAgentSessionID {
				cancelled++
				continue
			}
			kept = append(kept, item)
		}
		state.Items = kept
		if !state.Running && len(state.Items) == 0 {
			delete(e.promptQueues, key)
		}
	}
	return cancelled
}

func (e *Engine) queueAction(sessionKey, args string) string {
	fields := strings.Fields(strings.TrimSpace(args))
	if len(fields) == 0 || fields[0] == "list" {
		return ""
	}
	session := e.sessions.GetOrCreateActive(sessionKey)
	queueKey, _ := e.promptQueueKey(sessionKey, session)

	switch fields[0] {
	case "clear":
		n := e.clearPromptQueue(queueKey)
		if n == 0 {
			return e.i18n.T(MsgQueueEmpty)
		}
		return e.i18n.Tf(MsgQueueCleared, n)
	case "delete", "skip":
		if len(fields) < 2 {
			return e.i18n.T(MsgQueueUsage)
		}
		if e.deletePromptQueueItem(queueKey, fields[1]) {
			return e.i18n.T(MsgQueueDeleted)
		}
		return e.i18n.T(MsgQueueItemNotFound)
	case "up":
		if len(fields) < 2 {
			return e.i18n.T(MsgQueueUsage)
		}
		if e.movePromptQueueItem(queueKey, fields[1], -1) {
			return e.i18n.T(MsgQueueMoved)
		}
		return e.i18n.T(MsgQueueItemNotFound)
	case "down":
		if len(fields) < 2 {
			return e.i18n.T(MsgQueueUsage)
		}
		if e.movePromptQueueItem(queueKey, fields[1], 1) {
			return e.i18n.T(MsgQueueMoved)
		}
		return e.i18n.T(MsgQueueItemNotFound)
	default:
		return e.i18n.T(MsgQueueUsage)
	}
}

func (e *Engine) clearPromptQueue(queueKey string) int {
	e.promptQueueMu.Lock()
	defer e.promptQueueMu.Unlock()

	state := e.promptQueues[queueKey]
	if state == nil {
		return 0
	}
	n := len(state.Items)
	state.Items = nil
	if !state.Running {
		delete(e.promptQueues, queueKey)
	}
	return n
}

func (e *Engine) deletePromptQueueItem(queueKey, idOrIndex string) bool {
	e.promptQueueMu.Lock()
	defer e.promptQueueMu.Unlock()

	state := e.promptQueues[queueKey]
	if state == nil {
		return false
	}
	idx := promptQueueItemIndex(state.Items, idOrIndex)
	if idx < 0 {
		return false
	}
	copy(state.Items[idx:], state.Items[idx+1:])
	state.Items = state.Items[:len(state.Items)-1]
	if !state.Running && len(state.Items) == 0 {
		delete(e.promptQueues, queueKey)
	}
	return true
}

func (e *Engine) movePromptQueueItem(queueKey, idOrIndex string, delta int) bool {
	e.promptQueueMu.Lock()
	defer e.promptQueueMu.Unlock()

	state := e.promptQueues[queueKey]
	if state == nil {
		return false
	}
	idx := promptQueueItemIndex(state.Items, idOrIndex)
	if idx < 0 {
		return false
	}
	target := idx + delta
	if target < 0 || target >= len(state.Items) {
		return false
	}
	state.Items[idx], state.Items[target] = state.Items[target], state.Items[idx]
	return true
}

func promptQueueItemIndex(items []queuedPrompt, idOrIndex string) int {
	idOrIndex = strings.TrimPrefix(strings.TrimSpace(idOrIndex), "#")
	for i := range items {
		if items[i].ID == idOrIndex {
			return i
		}
	}
	var index int
	if _, err := fmt.Sscanf(idOrIndex, "%d", &index); err != nil || index <= 0 {
		return -1
	}
	if index > len(items) {
		return -1
	}
	return index - 1
}
