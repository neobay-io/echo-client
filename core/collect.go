package core

import (
	"fmt"
	"strings"
	"time"
)

const (
	maxCollectedItems = 50
	collectAckEvery   = 5
)

var collectTTL = 24 * time.Hour

type pendingCollectedBatch struct {
	CreatedAt           time.Time
	UpdatedAt           time.Time
	ExpiresAt           time.Time
	Items               []collectedItem
	Images              []ImageAttachment
	Files               []FileAttachment
	AwaitingInstruction bool
}

type collectedAttachmentRef struct {
	FileName string
	MimeType string
}

type collectedMessageSnapshot struct {
	MessageID       string
	UserID          string
	UserName        string
	Content         string
	QuotedMessageID string
	QuotedUserID    string
	QuotedUserName  string
	QuotedContent   string
	FromVoice       bool
}

type collectedItem struct {
	Message       collectedMessageSnapshot
	Platform      string
	CollectedAt   time.Time
	Forwarded     bool
	ForwardSource string
	ForwardChat   string
	ForwardDate   time.Time
	ImageCount    int
	FileCount     int
	ImageRefs     []collectedAttachmentRef
	FileRefs      []collectedAttachmentRef
}

func cloneCollectedItems(items []collectedItem) []collectedItem {
	if len(items) == 0 {
		return nil
	}
	out := make([]collectedItem, len(items))
	copy(out, items)
	for i := range out {
		out[i].ImageRefs = cloneCollectedAttachmentRefs(out[i].ImageRefs)
		out[i].FileRefs = cloneCollectedAttachmentRefs(out[i].FileRefs)
	}
	return out
}

func cloneCollectedAttachmentRefs(refs []collectedAttachmentRef) []collectedAttachmentRef {
	if len(refs) == 0 {
		return nil
	}
	out := make([]collectedAttachmentRef, len(refs))
	copy(out, refs)
	return out
}

func collectedImageRefs(images []ImageAttachment) []collectedAttachmentRef {
	if len(images) == 0 {
		return nil
	}
	refs := make([]collectedAttachmentRef, 0, len(images))
	for _, img := range images {
		refs = append(refs, collectedAttachmentRef{
			FileName: strings.TrimSpace(img.FileName),
			MimeType: strings.TrimSpace(img.MimeType),
		})
	}
	return refs
}

func collectedFileRefs(files []FileAttachment) []collectedAttachmentRef {
	if len(files) == 0 {
		return nil
	}
	refs := make([]collectedAttachmentRef, 0, len(files))
	for _, f := range files {
		refs = append(refs, collectedAttachmentRef{
			FileName: strings.TrimSpace(f.FileName),
			MimeType: strings.TrimSpace(f.MimeType),
		})
	}
	return refs
}

func clonePendingCollection(batch *pendingCollectedBatch) *pendingCollectedBatch {
	if batch == nil {
		return nil
	}
	return &pendingCollectedBatch{
		CreatedAt:           batch.CreatedAt,
		UpdatedAt:           batch.UpdatedAt,
		ExpiresAt:           batch.ExpiresAt,
		Items:               cloneCollectedItems(batch.Items),
		Images:              cloneImages(batch.Images),
		Files:               cloneFiles(batch.Files),
		AwaitingInstruction: batch.AwaitingInstruction,
	}
}

func (e *Engine) expirePendingCollectionIfNeeded(p Platform, msg *Message, now time.Time) {
	e.collectMu.Lock()
	batch := e.pendingCollect[msg.SessionKey]
	if batch == nil || (!batch.ExpiresAt.IsZero() && batch.ExpiresAt.After(now)) {
		e.collectMu.Unlock()
		return
	}
	delete(e.pendingCollect, msg.SessionKey)
	e.collectMu.Unlock()

	e.reply(p, msg.ReplyCtx, e.i18n.T(MsgCollectExpired))
}

func (e *Engine) getPendingCollection(sessionKey string) *pendingCollectedBatch {
	e.collectMu.Lock()
	defer e.collectMu.Unlock()
	return clonePendingCollection(e.pendingCollect[sessionKey])
}

func (e *Engine) hasPendingCollection(sessionKey string) bool {
	e.collectMu.Lock()
	defer e.collectMu.Unlock()
	_, ok := e.pendingCollect[sessionKey]
	return ok
}

func (e *Engine) startPendingCollection(sessionKey string, now time.Time) *pendingCollectedBatch {
	e.collectMu.Lock()
	defer e.collectMu.Unlock()
	if batch := e.pendingCollect[sessionKey]; batch != nil {
		return clonePendingCollection(batch)
	}
	batch := &pendingCollectedBatch{
		CreatedAt: now,
		UpdatedAt: now,
		ExpiresAt: now.Add(collectTTL),
	}
	e.pendingCollect[sessionKey] = batch
	return clonePendingCollection(batch)
}

func (e *Engine) clearPendingCollection(sessionKey string) bool {
	e.collectMu.Lock()
	defer e.collectMu.Unlock()
	if e.pendingCollect[sessionKey] == nil {
		return false
	}
	delete(e.pendingCollect, sessionKey)
	return true
}

func (e *Engine) setPendingCollectionAwaitingInstruction(sessionKey string, awaiting bool) bool {
	e.collectMu.Lock()
	defer e.collectMu.Unlock()
	batch := e.pendingCollect[sessionKey]
	if batch == nil {
		return false
	}
	batch.AwaitingInstruction = awaiting
	batch.UpdatedAt = time.Now()
	return true
}

func (e *Engine) isPendingCollectionAwaitingInstruction(sessionKey string) bool {
	e.collectMu.Lock()
	defer e.collectMu.Unlock()
	batch := e.pendingCollect[sessionKey]
	return batch != nil && batch.AwaitingInstruction
}

func (e *Engine) collectedItemCount(sessionKey string) int {
	e.collectMu.Lock()
	defer e.collectMu.Unlock()
	batch := e.pendingCollect[sessionKey]
	if batch == nil {
		return 0
	}
	return len(batch.Items)
}

func (e *Engine) absorbPendingAttachmentsIntoCollection(sessionKey string) {
	pending := e.getPendingAttachments(sessionKey)
	if pending == nil || (len(pending.Images) == 0 && len(pending.Files) == 0) {
		return
	}
	e.clearPendingAttachments(sessionKey)

	e.collectMu.Lock()
	defer e.collectMu.Unlock()
	batch := e.pendingCollect[sessionKey]
	if batch == nil {
		return
	}
	now := time.Now()
	if len(batch.Items) < maxCollectedItems {
		batch.Items = append(batch.Items, collectedItem{
			CollectedAt: now,
			ImageCount:  len(pending.Images),
			FileCount:   len(pending.Files),
			ImageRefs:   collectedImageRefs(pending.Images),
			FileRefs:    collectedFileRefs(pending.Files),
		})
	}
	batch.Images = append(batch.Images, cloneImages(pending.Images)...)
	batch.Files = append(batch.Files, cloneFiles(pending.Files)...)
	batch.UpdatedAt = now
}

func (e *Engine) bufferPendingCollection(msg *Message) (int, bool) {
	e.collectMu.Lock()
	defer e.collectMu.Unlock()

	batch := e.pendingCollect[msg.SessionKey]
	if batch == nil {
		return 0, false
	}
	if len(batch.Items) >= maxCollectedItems {
		return len(batch.Items), true
	}

	now := time.Now()
	batch.Items = append(batch.Items, collectedItem{
		Message: collectedMessageSnapshot{
			MessageID:       msg.MessageID,
			UserID:          msg.UserID,
			UserName:        msg.UserName,
			Content:         strings.TrimSpace(msg.Content),
			QuotedMessageID: msg.QuotedMessageID,
			QuotedUserID:    msg.QuotedUserID,
			QuotedUserName:  msg.QuotedUserName,
			QuotedContent:   msg.QuotedContent,
			FromVoice:       msg.FromVoice,
		},
		Platform:      msg.Platform,
		CollectedAt:   now,
		Forwarded:     msg.Forwarded,
		ForwardSource: strings.TrimSpace(msg.ForwardSource),
		ForwardChat:   strings.TrimSpace(msg.ForwardChat),
		ForwardDate:   msg.ForwardDate,
		ImageCount:    len(msg.Images),
		FileCount:     len(msg.Files),
		ImageRefs:     collectedImageRefs(msg.Images),
		FileRefs:      collectedFileRefs(msg.Files),
	})
	batch.Images = append(batch.Images, cloneImages(msg.Images)...)
	batch.Files = append(batch.Files, cloneFiles(msg.Files)...)
	batch.UpdatedAt = now
	batch.AwaitingInstruction = false
	return len(batch.Items), false
}

func (e *Engine) maybeAcknowledgeCollectedItem(p Platform, replyCtx any, count int) {
	if count <= 0 {
		return
	}
	if count != 1 && count%collectAckEvery != 0 {
		return
	}
	e.reply(p, replyCtx, e.i18n.Tf(MsgCollectBuffered, count))
}

func (e *Engine) buildPendingCollectionStatus(batch *pendingCollectedBatch) string {
	if batch == nil {
		return e.i18n.T(MsgCollectInactive)
	}

	textChars := 0
	forwardedItems := 0
	for _, item := range batch.Items {
		textChars += len(item.Message.Content)
		textChars += len(item.Message.QuotedContent)
		if item.Forwarded {
			forwardedItems++
		}
	}

	return e.i18n.Tf(
		MsgCollectStatusActive,
		len(batch.Items),
		len(batch.Images),
		len(batch.Files),
		textChars,
		forwardedItems,
		batch.CreatedAt.Format(time.RFC3339),
		batch.ExpiresAt.Format(time.RFC3339),
		boolWord(batch.AwaitingInstruction),
	)
}

func boolWord(v bool) string {
	if v {
		return "yes"
	}
	return "no"
}

func buildCollectedPrompt(batch *pendingCollectedBatch, instruction string) string {
	var b strings.Builder
	imageOrdinal := 1
	fileOrdinal := 1

	instruction = strings.TrimSpace(instruction)
	fmt.Fprintf(&b, "The user buffered the following messages before asking for help. Treat each item as separate source material, not as one continuous chat transcript.\n\nFinal instruction:\n%s\n\nCollected items: %d", instruction, len(batch.Items))
	if totalAttachments := len(batch.Images) + len(batch.Files); totalAttachments > 0 {
		fmt.Fprintf(&b, "\nAttached files included with this turn: %d", totalAttachments)
	}

	for i, item := range batch.Items {
		fmt.Fprintf(&b, "\n\nItem %d\n", i+1)
		if platform := strings.TrimSpace(item.Platform); platform != "" {
			fmt.Fprintf(&b, "Platform: %s\n", platform)
		}
		if name := strings.TrimSpace(item.Message.UserName); name != "" {
			fmt.Fprintf(&b, "Sender: %s\n", name)
		}
		if id := strings.TrimSpace(item.Message.MessageID); id != "" {
			fmt.Fprintf(&b, "Message ID: %s\n", id)
		}
		if !item.CollectedAt.IsZero() {
			fmt.Fprintf(&b, "Collected at: %s\n", item.CollectedAt.Format(time.RFC3339))
		}
		if item.Forwarded {
			forwardLine := "Forwarded: yes"
			if source := strings.TrimSpace(item.ForwardSource); source != "" {
				forwardLine = fmt.Sprintf("%s, from %s", forwardLine, source)
			}
			if chat := strings.TrimSpace(item.ForwardChat); chat != "" {
				forwardLine = fmt.Sprintf("%s, chat %s", forwardLine, chat)
			}
			if !item.ForwardDate.IsZero() {
				forwardLine = fmt.Sprintf("%s, at %s", forwardLine, item.ForwardDate.Format(time.RFC3339))
			}
			b.WriteString(forwardLine + "\n")
		}
		if item.ImageCount > 0 || item.FileCount > 0 {
			fmt.Fprintf(&b, "Attachments: %s\n", attachmentSummary(item.ImageCount, item.FileCount))
			if len(item.ImageRefs) > 0 {
				fmt.Fprintf(&b, "Image refs: %s\n", renderCollectedAttachmentRefs("image", imageOrdinal, item.ImageRefs))
				imageOrdinal += len(item.ImageRefs)
			}
			if len(item.FileRefs) > 0 {
				fmt.Fprintf(&b, "File refs: %s\n", renderCollectedAttachmentRefs("file", fileOrdinal, item.FileRefs))
				fileOrdinal += len(item.FileRefs)
			}
		}

		quoted := strings.TrimSpace(item.Message.QuotedContent)
		if quoted != "" {
			header := "Reply context"
			if name := strings.TrimSpace(item.Message.QuotedUserName); name != "" {
				header = fmt.Sprintf("Reply context from %s", name)
			}
			fmt.Fprintf(&b, "%s:\n%s\n", header, quoted)
		}

		content := strings.TrimSpace(item.Message.Content)
		if content == "" {
			b.WriteString("Message:\n[no text content]\n")
		} else {
			fmt.Fprintf(&b, "Message:\n%s\n", content)
		}
	}

	return b.String()
}

func attachmentSummary(images, files int) string {
	parts := make([]string, 0, 2)
	if images > 0 {
		label := "images"
		if images == 1 {
			label = "image"
		}
		parts = append(parts, fmt.Sprintf("%d %s", images, label))
	}
	if files > 0 {
		label := "files"
		if files == 1 {
			label = "file"
		}
		parts = append(parts, fmt.Sprintf("%d %s", files, label))
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ", ")
}

func renderCollectedAttachmentRefs(kind string, start int, refs []collectedAttachmentRef) string {
	if len(refs) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(refs))
	for i, ref := range refs {
		label := fmt.Sprintf("%s #%d", kind, start+i)
		detail := strings.TrimSpace(ref.FileName)
		if detail == "" {
			detail = strings.TrimSpace(ref.MimeType)
		}
		if detail != "" {
			label = fmt.Sprintf("%s (%s)", label, detail)
		}
		parts = append(parts, label)
	}
	return strings.Join(parts, ", ")
}

func (e *Engine) flushPendingCollection(p Platform, msg *Message, instruction string) bool {
	instruction = strings.TrimSpace(instruction)
	if instruction == "" {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgCollectInstructionRequired))
		return true
	}

	batch := e.getPendingCollection(msg.SessionKey)
	if batch == nil || len(batch.Items) == 0 {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgCollectEmpty))
		return true
	}

	if e.agentUpgradeBlocksNewTurns() {
		e.reply(p, msg.ReplyCtx, e.agentUpgradeBlockedMessage())
		return true
	}

	session := e.sessions.GetOrCreateActive(msg.SessionKey)
	if !session.TryLock() {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgPreviousProcessing))
		return true
	}
	if e.agentUpgradeBlocksNewTurns() {
		session.Unlock()
		e.reply(p, msg.ReplyCtx, e.agentUpgradeBlockedMessage())
		return true
	}

	synthesized := &Message{
		SessionKey: msg.SessionKey,
		Platform:   msg.Platform,
		MessageID:  msg.MessageID,
		UserID:     msg.UserID,
		UserName:   msg.UserName,
		Content:    buildCollectedPrompt(batch, instruction),
		Images:     append(cloneImages(batch.Images), cloneImages(msg.Images)...),
		Files:      append(cloneFiles(batch.Files), cloneFiles(msg.Files)...),
		ReplyCtx:   msg.ReplyCtx,
	}

	if err := e.processInteractiveMessageAsync(p, synthesized, session); err != nil {
		return true
	}
	e.clearPendingCollection(msg.SessionKey)
	return true
}

func (e *Engine) cmdCollect(p Platform, msg *Message, args []string) {
	if len(args) == 0 {
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgCollectUsage))
		return
	}

	sub := matchSubCommand(strings.ToLower(args[0]), []string{"start", "status", "cancel", "send"})
	switch sub {
	case "start":
		if batch := e.getPendingCollection(msg.SessionKey); batch != nil {
			e.reply(p, msg.ReplyCtx, e.i18n.Tf(MsgCollectAlreadyActive, len(batch.Items)))
			return
		}
		e.startPendingCollection(msg.SessionKey, time.Now())
		e.absorbPendingAttachmentsIntoCollection(msg.SessionKey)
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgCollectStarted))
	case "status":
		e.reply(p, msg.ReplyCtx, e.buildPendingCollectionStatus(e.getPendingCollection(msg.SessionKey)))
	case "cancel":
		if !e.clearPendingCollection(msg.SessionKey) {
			e.reply(p, msg.ReplyCtx, e.i18n.T(MsgCollectInactive))
			return
		}
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgCollectCanceled))
	case "send":
		if e.collectedItemCount(msg.SessionKey) == 0 {
			e.reply(p, msg.ReplyCtx, e.i18n.T(MsgCollectEmpty))
			return
		}
		if len(args) == 1 {
			e.setPendingCollectionAwaitingInstruction(msg.SessionKey, true)
			e.reply(p, msg.ReplyCtx, e.i18n.T(MsgCollectInstructionPrompt))
			return
		}
		e.flushPendingCollection(p, msg, strings.Join(args[1:], " "))
	default:
		e.reply(p, msg.ReplyCtx, e.i18n.T(MsgCollectUsage))
	}
}
