package core

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

const (
	maxPromptQueueItems           = 20
	maxPromptQueueAttachmentBytes = 50 * 1024 * 1024
	maxPromptQueueEngineBytes     = 200 * 1024 * 1024
	promptQueuePreviewRunes       = 120
	promptQueueRetryDelay         = 250 * time.Millisecond
)

type queuedPrompt struct {
	ID                     string
	QueueKey               string
	OriginSessionKey       string
	OriginLocalSessionID   string
	OriginPlatform         string
	AgentSessionIDSnapshot string
	Source                 string // "" / "user" = interactive; "cron" = scheduled job
	JobID                  string // cron job id when Source == "cron"
	Message                Message
	CreatedAt              time.Time
}

func (q queuedPrompt) isCron() bool { return q.Source == "cron" }

type promptQueueState struct {
	Running bool
	Items   []queuedPrompt
}

type promptQueueSnapshot struct {
	QueueKey string
	Running  bool
	Items    []queuedPrompt
}

// lockSessionTransition serializes interactive startup with local-session
// replacement for one chat. Entries intentionally live for the Engine lifetime,
// matching the lifetime of the SessionManager records keyed by sessionKey.
func (e *Engine) lockSessionTransition(sessionKey string) func() {
	value, _ := e.sessionTransitions.LoadOrStore(sessionKey, &sync.Mutex{})
	lock := value.(*sync.Mutex)
	lock.Lock()
	return lock.Unlock
}

// promptQueueKey returns (queueKey, agentSessionIDSnapshot).
//
// The queue is serialized per chat entry (sessionKey): a cron turn and a user
// turn in the same chat share one FIFO and one Running gate, so they run one at
// a time (needed so cron and user prompts interleave serially even though they
// use different agent sessions). The key stays stable across a turn's lifetime,
// so completePromptTurn's key-migration branch is a no-op.
//
// The second value is the agent session id. User prompts also snapshot the local
// cc-connect session id separately so switching between two local sessions that
// share one agent transcript cannot reroute queued work or history.
func (e *Engine) promptQueueKey(sessionKey string, session *Session) (string, string) {
	_, agentSessionID := e.agentSessionTurnIdentity(session)
	return "grp:" + sessionKey, strings.TrimSpace(agentSessionID)
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
	if msg.Audio != nil {
		total += len(msg.Audio.Data)
	}
	return total
}

func promptQueueItemsAttachmentBytes(items []queuedPrompt) int {
	total := 0
	for i := range items {
		total += promptQueueAttachmentBytes(&items[i].Message)
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

// enqueuePromptID is enqueuePrompt that also returns the new item's ID.
func (e *Engine) enqueuePromptID(queueKey, localSessionID, agentSessionID string, msg *Message) (int, string, error) {
	e.promptQueueMu.Lock()
	defer e.promptQueueMu.Unlock()
	state := e.promptQueues[queueKey]
	if state == nil {
		state = &promptQueueState{}
		e.promptQueues[queueKey] = state
	}
	pos, err := e.enqueuePromptLocked(state, queueKey, localSessionID, agentSessionID, "user", "", msg)
	if err != nil {
		return 0, "", err
	}
	return pos, state.Items[len(state.Items)-1].ID, nil
}

// beginPromptTurnOrEnqueueID is beginPromptTurnOrEnqueue that also returns
// the queued item's ID.
func (e *Engine) beginPromptTurnOrEnqueueID(queueKey, localSessionID, agentSessionID string, msg *Message) (bool, int, string, error) {
	e.promptQueueMu.Lock()
	defer e.promptQueueMu.Unlock()
	state := e.promptQueues[queueKey]
	if state == nil {
		state = &promptQueueState{}
		e.promptQueues[queueKey] = state
	}
	if state.Running || len(state.Items) > 0 {
		pos, err := e.enqueuePromptLocked(state, queueKey, localSessionID, agentSessionID, "user", "", msg)
		if err != nil {
			return false, 0, "", err
		}
		return false, pos, state.Items[len(state.Items)-1].ID, nil
	}
	state.Running = true
	return true, 0, "", nil
}

func (e *Engine) enqueuePrompt(queueKey, localSessionID, agentSessionID string, msg *Message) (int, error) {
	e.promptQueueMu.Lock()
	defer e.promptQueueMu.Unlock()

	state := e.promptQueues[queueKey]
	if state == nil {
		state = &promptQueueState{}
		e.promptQueues[queueKey] = state
	}
	return e.enqueuePromptLocked(state, queueKey, localSessionID, agentSessionID, "user", "", msg)
}

// enqueuePromptFront puts a user message at the head of a queue (a turn that
// could not start and keeps its place).
func (e *Engine) enqueuePromptFront(queueKey, localSessionID, agentSessionID string, msg *Message) error {
	e.promptQueueMu.Lock()
	defer e.promptQueueMu.Unlock()
	state := e.promptQueues[queueKey]
	if state == nil {
		state = &promptQueueState{}
		e.promptQueues[queueKey] = state
	}
	if _, err := e.enqueuePromptLocked(state, queueKey, localSessionID, agentSessionID, "user", "", msg); err != nil {
		return err
	}
	last := state.Items[len(state.Items)-1]
	copy(state.Items[1:], state.Items[:len(state.Items)-1])
	state.Items[0] = last
	return nil
}

func (e *Engine) enqueuePromptLocked(state *promptQueueState, queueKey, localSessionID, agentSessionID, source, jobID string, msg *Message) (int, error) {
	if len(state.Items) >= maxPromptQueueItems {
		return 0, fmt.Errorf("queue full")
	}
	totalBytes := promptQueueAttachmentBytes(msg)
	totalBytes += promptQueueItemsAttachmentBytes(state.Items)
	if totalBytes > maxPromptQueueAttachmentBytes {
		return 0, fmt.Errorf("queue attachments too large")
	}
	if e.promptQueuesAttachmentBytesLocked()+promptQueueAttachmentBytes(msg) > maxPromptQueueEngineBytes {
		return 0, fmt.Errorf("queue attachments too large")
	}
	if source == "" {
		source = "user"
	}
	e.promptQueueSeq++
	item := queuedPrompt{
		ID:                     fmt.Sprintf("q%d", e.promptQueueSeq),
		QueueKey:               queueKey,
		OriginSessionKey:       msg.SessionKey,
		OriginLocalSessionID:   strings.TrimSpace(localSessionID),
		OriginPlatform:         msg.Platform,
		AgentSessionIDSnapshot: strings.TrimSpace(agentSessionID),
		Source:                 source,
		JobID:                  jobID,
		Message:                cloneQueuedMessage(msg),
		CreatedAt:              time.Now(),
	}
	state.Items = append(state.Items, item)
	return len(state.Items), nil
}

func (e *Engine) promptQueuesAttachmentBytesLocked() int {
	total := 0
	for _, state := range e.promptQueues {
		if state == nil {
			continue
		}
		total += promptQueueItemsAttachmentBytes(state.Items)
	}
	return total
}

func (e *Engine) markPromptQueueRunning(queueKey string) {
	e.promptQueueMu.Lock()
	defer e.promptQueueMu.Unlock()
	state := e.promptQueues[queueKey]
	if state == nil {
		state = &promptQueueState{}
		e.promptQueues[queueKey] = state
	}
	state.Running = true
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
	if e.agentUpgradeBlocksNewTurns() {
		// A queued turn would only be turned away now; run it afterwards.
		e.drainQueueAfterUpgrade(queueKey)
		return
	}
	item, ok := e.popNextQueuedPrompt(queueKey)
	if !ok {
		return
	}
	if item.isCron() {
		_ = e.startQueuedCronRun(item)
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
	e.schedulePromptQueueRetry(item.QueueKey)
}

func (e *Engine) schedulePromptQueueRetry(queueKey string) {
	go func() {
		timer := time.NewTimer(promptQueueRetryDelay)
		defer timer.Stop()
		select {
		case <-e.ctx.Done():
			return
		case <-timer.C:
			e.drainPromptQueue(queueKey)
		}
	}()
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
	unlockTransition := e.lockSessionTransition(item.OriginSessionKey)
	defer unlockTransition()

	session := e.sessions.GetOrCreateActive(item.OriginSessionKey)
	if item.OriginLocalSessionID != "" && session.ID != item.OriginLocalSessionID {
		e.failQueuedPrompt(item, e.i18n.T(MsgQueueSkippedSessionChanged))
		return true
	}
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
	started := make(chan struct{})
	go e.processInteractiveMessageAndDrainQueueWithStartSignal(p, &msg, session, item.QueueKey, started)
	<-started
	return true
}

// startQueuedCronRun runs a dequeued cron item as an isolated turn while holding
// the chat's active-session busy lock, so a concurrent user turn (including the
// non-queue direct path in handleMessage) waits. It never reads or mutates the
// active session's agent id or history; the cron turn runs on the job's own
// agent session via runCronIsolatedTurn.
func (e *Engine) startQueuedCronRun(item queuedPrompt) bool {
	if e.cronScheduler == nil {
		e.failQueuedPrompt(item, "")
		return true
	}
	job := e.cronScheduler.Store().Get(item.JobID)
	if job == nil || !job.Enabled {
		e.failQueuedPrompt(item, "")
		return true
	}
	session := e.sessions.GetOrCreateActive(item.OriginSessionKey)
	if !session.TryLock() {
		e.requeuePromptFront(item)
		return false
	}
	p, replyCtx, err := e.platformAndReplyContextForQueuedPrompt(item)
	if err != nil {
		session.Unlock()
		e.failQueuedPrompt(item, "")
		return true
	}
	if job.IsTimerJob() {
		e.cronScheduler.Store().SetQueuedAt(item.JobID, time.Time{}) // runs now
	}
	go func() {
		defer func() {
			session.Unlock()
			finalKey := e.completePromptTurn(item.QueueKey, item.OriginSessionKey, session)
			e.drainPromptQueue(finalKey)
		}()
		runErr := e.runCronIsolatedTurn(e.ctx, item.JobID, job, p, replyCtx)
		if e.cronScheduler != nil {
			e.cronScheduler.Store().MarkRun(item.JobID, runErr)
		}
	}()
	return true
}

// beginCronTurnOrEnqueue mirrors beginPromptTurnOrEnqueue for cron: if the chat's
// group queue is idle it claims Running and returns true (caller runs inline);
// otherwise it enqueues a cron item and returns false so a later drain runs it.
func (e *Engine) beginCronTurnOrEnqueue(job *CronJob, sessionKey string, msg *Message) (bool, error) {
	session := e.sessions.GetOrCreateActive(sessionKey)
	queueKey, agentSessionID := e.promptQueueKey(sessionKey, session)

	e.promptQueueMu.Lock()
	defer e.promptQueueMu.Unlock()
	state := e.promptQueues[queueKey]
	if state == nil {
		state = &promptQueueState{}
		e.promptQueues[queueKey] = state
	}
	if state.Running || len(state.Items) > 0 {
		// One waiting run per job: runs that pile up behind a busy chat
		// would otherwise go back to back, ignoring the schedule.
		for _, item := range state.Items {
			if item.isCron() && item.JobID == job.ID {
				return false, nil
			}
		}
		_, err := e.enqueuePromptLocked(state, queueKey, "", agentSessionID, "cron", job.ID, msg)
		return false, err
	}
	state.Running = true
	return true, nil
}

func (e *Engine) platformAndReplyContextForQueuedPrompt(item queuedPrompt) (Platform, any, error) {
	p, err := e.runningPlatform(item.OriginPlatform)
	if err != nil {
		return nil, nil, err
	}
	if item.Message.ReplyCtx != nil {
		return p, item.Message.ReplyCtx, nil
	}
	if rc, ok := p.(ReplyContextReconstructor); ok {
		replyCtx, err := rc.ReconstructReplyCtx(item.OriginSessionKey)
		return p, replyCtx, err
	}
	return p, nil, nil
}

func (e *Engine) processInteractiveMessageAndDrainQueue(p Platform, msg *Message, session *Session, queueKey string) {
	e.processInteractiveMessageAndDrainQueueWithStartSignal(p, msg, session, queueKey, nil)
}

func (e *Engine) processInteractiveMessageAndDrainQueueWithStartSignal(p Platform, msg *Message, session *Session, queueKey string, started chan struct{}) {
	e.processInteractiveMessageWithStartSignal(p, msg, session, started)
	finalKey := e.completePromptTurn(queueKey, msg.SessionKey, session)
	e.drainPromptQueue(finalKey)
}

func (e *Engine) processInteractiveMessageWithQueueLifecycle(p Platform, msg *Message, session *Session) {
	queueKey, _ := e.promptQueueKey(msg.SessionKey, session)
	e.markPromptQueueRunning(queueKey)
	e.processInteractiveMessageAndDrainQueue(p, msg, session, queueKey)
}

func (e *Engine) processInteractiveMessageAsyncAndDrainQueue(p Platform, msg *Message, session *Session, done func(error)) {
	queueKey, _ := e.promptQueueKey(msg.SessionKey, session)
	e.markPromptQueueRunning(queueKey)
	e.processInteractiveMessageAsyncWithQueueKey(p, msg, session, queueKey, done)
}

// groupQueueRunning reports whether the chat's group queue currently has a turn
// running (user or cron). /stop uses it to avoid force-unlocking a session that
// an isolated cron run legitimately holds without an interactiveState.
func (e *Engine) groupQueueRunning(sessionKey string) bool {
	session := e.sessions.GetOrCreateActive(sessionKey)
	queueKey, _ := e.promptQueueKey(sessionKey, session)
	e.promptQueueMu.Lock()
	defer e.promptQueueMu.Unlock()
	state := e.promptQueues[queueKey]
	return state != nil && state.Running
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

func (e *Engine) cancelQueuedPromptsForSessionSwitch(sessionKey, newLocalSessionID, newAgentSessionID string) int {
	e.promptQueueMu.Lock()
	defer e.promptQueueMu.Unlock()

	cancelled := 0
	for key, state := range e.promptQueues {
		if state == nil || len(state.Items) == 0 {
			continue
		}
		kept := state.Items[:0]
		for _, item := range state.Items {
			localSessionChanged := !item.isCron() &&
				(item.OriginLocalSessionID == "" || item.OriginLocalSessionID != newLocalSessionID)
			agentSessionChanged := item.AgentSessionIDSnapshot == "" || item.AgentSessionIDSnapshot != newAgentSessionID
			if item.OriginSessionKey == sessionKey && (localSessionChanged || agentSessionChanged) {
				e.finishDroppedCronItems([]queuedPrompt{item}, "dropped: the chat started another session")
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
	case "steer":
		if len(fields) < 2 {
			return e.i18n.T(MsgQueueUsage)
		}
		return e.steerQueuedItem(sessionKey, queueKey, fields[1])
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
	e.finishDroppedCronItems(state.Items, "dropped from the queue")
	state.Items = nil
	if !state.Running {
		delete(e.promptQueues, queueKey)
	}
	return n
}

// dropQueuedCronRuns removes a job's waiting runs from every queue.
func (e *Engine) dropQueuedCronRuns(jobID, reason string) int {
	e.promptQueueMu.Lock()
	defer e.promptQueueMu.Unlock()
	dropped := 0
	for key, state := range e.promptQueues {
		if state == nil {
			continue
		}
		kept := state.Items[:0]
		for _, item := range state.Items {
			if item.isCron() && item.JobID == jobID {
				e.finishDroppedCronItems([]queuedPrompt{item}, reason)
				dropped++
				continue
			}
			kept = append(kept, item)
		}
		state.Items = kept
		if !state.Running && len(state.Items) == 0 {
			delete(e.promptQueues, key)
		}
	}
	return dropped
}

// finishDroppedCronItems records queued cron runs dropped before they ran.
// A one-shot timer is marked as run with the reason; otherwise it would stay
// pending with nothing left to fire it. Repeating jobs just run next time.
// Called with promptQueueMu held (the cron store never takes it).
func (e *Engine) finishDroppedCronItems(items []queuedPrompt, reason string) {
	if e.cronScheduler == nil {
		return
	}
	for _, item := range items {
		if !item.isCron() || item.JobID == "" {
			continue
		}
		if job := e.cronScheduler.Store().Get(item.JobID); job != nil && job.IsTimerJob() && job.Enabled {
			e.cronScheduler.Store().MarkRun(item.JobID, errors.New(reason))
		}
	}
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
	e.finishDroppedCronItems(state.Items[idx:idx+1], "deleted from the queue")
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
