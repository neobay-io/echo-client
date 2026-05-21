# Forward Collection Design

This document describes a new cross-platform feature for **Telegram** and **Feishu**:

- Users can forward or send multiple messages to `cc-connect`
- `cc-connect` should **buffer** those messages instead of sending each one to the agent immediately
- The user can later send one final instruction telling the agent how to process the collected content

The design intentionally targets both Telegram and Feishu with one consistent core behavior, while allowing platform-specific enhancements where the underlying platform exposes richer forward metadata.

## Problem

Current behavior is immediate:

- any non-empty text message is sent to the agent right away
- reply context only includes the single replied-to message
- file/image buffering only works for attachment-only messages without text

This is not a good fit for workflows like:

1. forwarding several chat messages from another conversation
2. forwarding several screenshots/files
3. adding a final instruction such as:
   `Please summarize the forwarded discussion and tell me what I should reply`

Today the forwarded messages are processed one by one, which produces noise and loses the intended batch context.

## Goals

- Support a staged "collect first, process later" workflow in Telegram and Feishu
- Keep the default immediate-processing behavior unchanged unless the user explicitly enters collection mode
- Allow forwarded text, replied text, attachments, and mixed messages to be collected into one batch
- Make the final send behavior deterministic and easy to understand
- Reuse as much existing buffering and interaction infrastructure as possible

## Non-Goals

- Perfect reconstruction of original forward metadata on every platform
- Persisting buffered collections across restarts in the first implementation
- Building a full mailbox or long-term knowledge store
- Automatically inferring user intent from arbitrary forwarded traffic in Feishu

## Product Shape

### Primary UX: Explicit Collection Mode

Collection mode is the canonical cross-platform behavior.

The user explicitly starts a collection session, forwards/sends many messages, then explicitly flushes them to the agent with one instruction.

Suggested commands:

- `/collect start`
- `/collect status`
- `/collect cancel`
- `/collect send <instruction>`
- `/collect send` followed by the instruction in the next message

Examples:

```text
/collect start
```

Then the user forwards many messages.

Finally:

```text
/collect send Summarize these forwarded messages and suggest a reply.
```

Alternative two-step flush:

```text
/collect send
```

Then:

```text
Summarize these forwarded messages and suggest a reply.
```

### Why Explicit Mode Is The Default

This avoids several problems:

- it does not break existing users who rely on immediate processing
- Feishu forwarding metadata is less explicit than Telegram, so explicit mode guarantees parity
- it makes user intent unambiguous

### Optional Telegram Enhancement

Telegram exposes forwarded-message fields in the Bot API. After the core feature ships, Telegram can optionally support:

- `auto_stage_forwarded = true`

Behavior:

- if a forwarded message arrives while no collection is active
- and this option is enabled
- `cc-connect` automatically starts a collection and buffers the forwarded message
- it replies with guidance such as:
  `Buffered 1 forwarded message. Send more, then /collect send <instruction>.`

This should remain opt-in because it changes current behavior.

## User-Facing Behavior

### `/collect start`

Starts collection mode for the current session.

Expected response:

```text
Collection mode enabled.
Forward or send messages now. When ready, use:
/collect send <instruction>
```

If already active:

```text
Collection mode is already active.
Buffered items: 6
```

### While Collection Mode Is Active

Incoming messages are buffered instead of processed by the agent.

Buffered message types:

- plain text
- forwarded text
- reply-with-context text
- images
- files
- post/rich-text messages
- captioned media

Behavior:

- no agent turn starts
- no session lock is required
- a short acknowledgment may be returned, but should be rate-limited to avoid spam

Suggested acknowledgment style:

- after each item:
  `Buffered item 3.`
- or every few items:
  `Buffered 5 items so far.`

### `/collect status`

Returns current collection stats:

- whether collection mode is active
- number of buffered items
- number of buffered images/files
- rough text size
- whether forwarded items were detected

### `/collect cancel`

Discards the buffered collection and exits collection mode.

### `/collect send <instruction>`

Flushes the entire buffered batch into one agent turn.

Behavior:

- combines buffered items into one synthesized prompt
- appends the user's final instruction
- forwards all buffered attachments with that turn
- clears the collection buffer on success

If there is no instruction:

- enter a short-lived "awaiting final instruction" state and prompt the user to send the instruction as the next message

Expected prompt:

```text
Send the final instruction as your next message.
Example: Summarize these messages and draft a reply.
```

Rules:

- if the user sends `/collect send <instruction>` on one line, flush immediately
- if the user sends only `/collect send`, the next non-command text message is treated as the final instruction instead of being buffered as another collected item
- while awaiting the final instruction, `/collect cancel` still cancels the whole collection
- if the next message is empty or attachment-only, reject it and keep waiting for text
- if there are no buffered items, `/collect send` should fail with `No buffered messages to process.` before entering the awaiting-instruction state

If there are no buffered items:

- reject with `No buffered messages to process.`

### Slash Command Shape

This feature intentionally uses a new user-facing subcommand pattern, but it should still fit the existing engine command parser.

Recommended parser contract:

- register `/collect` as the built-in slash command
- parse the rest of the message as arguments, similar to how `/cron ...` and `/loop ...` already dispatch internally
- treat `start`, `status`, `cancel`, and `send` as recognized first-argument verbs under `/collect`

Examples:

- command = `/collect`, args = `start`
- command = `/collect`, args = `send summarize these`

This avoids relying on an exact `/collect send` command registration and keeps dispatch compatible with the current "first word is the command, remaining text is args" model.

## Prompt Construction

The final prompt should be explicit and stable, so the agent understands that these are collected items, not one continuous chat transcript.

Suggested shape:

```text
Collected messages from Telegram:

1. [Forwarded from Alice]
Can you review this contract tomorrow?

2. [Forwarded from Product Team channel]
We should delay the launch by one week.

3. [Reply context from Bob]
Let's keep the original API shape.

User instruction:
Summarize the discussion, identify action items, and draft a reply for me.
```

Rules:

- preserve per-item boundaries
- preserve source labels when known
- keep ordering identical to arrival order
- include quoted/reply context inside each buffered item
- merge buffered attachments into the final outbound message

## Platform-Specific Notes

### Telegram

Telegram is the richer platform for this feature.

The SDK already exposes forwarded-message fields such as:

- `ForwardFrom`
- `ForwardFromChat`
- `ForwardFromMessageID`
- `ForwardSenderName`
- `ForwardDate`
- `IsAutomaticForward`

This means Telegram can label buffered items as:

- forwarded from a user
- forwarded from a channel/chat
- automatic channel forward

Telegram can also later support inline buttons for collection actions:

- `Status`
- `Send`
- `Cancel`

Canonical interaction still remains the `/collect ...` commands.

### Feishu

Feishu currently has reply-context support in `cc-connect`, but the existing adapter does not explicitly extract forwarding metadata today.

That means the first implementation should treat Feishu collection mode as:

- explicit user-controlled buffering
- message content is buffered as received
- reply context remains supported
- forward provenance may be unavailable unless later discovered from the Feishu event payload

This is still useful because the feature's main value is "do not process immediately", not "perfectly reconstruct original provenance".

Note for docs linkage:

- the Feishu guide is user-facing Chinese documentation
- this design doc is implementation-facing and currently English-only
- that is acceptable for now because the target audience of this link is contributors implementing the feature, not end users configuring Feishu

## Internal Design

### New Core State

Add a new per-session buffer in `core.Engine`.

Suggested types:

```go
type pendingCollectedBatch struct {
    Active      bool
    CreatedAt   time.Time
    UpdatedAt   time.Time
    ExpiresAt   time.Time
    Platform    string
    Items       []collectedItem
    Images      []ImageAttachment
    Files       []FileAttachment
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
}

type collectedItem struct {
    Message      collectedMessageSnapshot
    Forwarded    bool
    ForwardSource string
    ForwardChat   string
    ForwardDate   time.Time
    HasImages     bool
    HasFiles      bool
}
```

Rationale:

- do not embed `core.Message` directly because it carries transient transport/runtime fields such as `ReplyCtx`, attachment payload slices, and audio state
- instead, snapshot only the prompt-relevant fields that need to survive buffering
- this reduces drift versus duplicating the fields ad hoc while keeping the buffered structure serializable and explicit

Suggested engine fields:

```go
collectMu      sync.Mutex
pendingCollect map[string]*pendingCollectedBatch
```

Keying:

- use `sessionKey`, consistent with existing attachment and voice-confirm buffers

### Message Model Extensions

Extend `core.Message` with optional forward metadata.

Suggested fields:

```go
Forwarded       bool
ForwardSource   string
ForwardChat     string
ForwardDate     time.Time
```

These fields are optional:

- Telegram populates them
- Feishu may leave them empty in the first version

### Buffering Rules

When collection mode is active:

1. incoming message does not start an agent turn
2. text becomes a `collectedItem`
3. images/files are accumulated into batch-level attachment lists
4. quoted/reply context stays attached to that collected item
5. the message is acknowledged and the function returns early

Concurrency note:

- buffering does not need the normal interactive session lock because no agent turn starts
- buffering still requires `collectMu` to guard collection state mutations, similar to how `pendingAttach` uses its own mutex

### Command Routing

Add a new built-in command group:

- `/collect start`
- `/collect status`
- `/collect cancel`
- `/collect send ...`

Recommended behavior:

- collection commands should be resolved before normal slash-command fallthrough to the agent
- `/collect send ...` should acquire the normal session lock because it starts a real agent turn
- `/collect start|status|cancel` should not require the session lock
- `/collect send` without inline instruction should store a small pending flush state so the next non-command text message is consumed as the final instruction

### Prompt Builder

Add a helper in `core/engine.go`:

```go
func buildCollectedPrompt(batch *pendingCollectedBatch, instruction string) string
```

Responsibilities:

- render items in order
- annotate forward/reply metadata when present
- add the final user instruction
- keep formatting plain and stable across platforms

### Interaction With Existing Attachment Buffering

Current `pendingAttach` behavior buffers attachment-only messages until the next text arrives.

Collection mode should supersede that behavior:

- when collection mode is active, incoming attachments go into the collection batch instead of `pendingAttach`
- when `/collect send ...` flushes, all collected attachments are attached to that single turn

This avoids double-buffering and reduces ambiguity.

### Interaction With Voice Confirmation

Recommended first version:

- keep voice transcription + confirmation behavior unchanged
- only after voice text is confirmed should it be buffered into the collection batch

This preserves the current safety behavior for voice messages.

## Suggested Implementation Steps

### Step 1: Core Data Model

- add forward metadata fields to `core.Message`
- add collection buffer structs to `core.Engine`
- add helper methods:
  - `startCollection`
  - `getCollection`
  - `appendCollectionItem`
  - `clearCollection`
  - `buildCollectedPrompt`
  - `startPendingCollectFlush`
  - `consumePendingCollectFlush`

### Step 2: Telegram Forward Metadata

- inspect `tgbotapi.Message` forward fields
- populate new `core.Message` forward metadata in the Telegram adapter
- preserve existing reply-context behavior

### Step 3: Core Command Handling

- implement `/collect start`
- implement `/collect status`
- implement `/collect cancel`
- implement `/collect send <instruction>`

### Step 4: Collection Intercept In `handleMessage`

- detect active collection mode early in `handleMessage`
- buffer incoming messages instead of sending to the agent
- route `/collect send ...` to the normal interactive flow after synthesizing the batch prompt

### Step 5: Feishu Parity

- wire Feishu messages into the same core collection mode
- keep provenance empty unless the Feishu event payload later exposes it in a reliable way

### Step 6: Optional Telegram Auto-Stage

- add opt-in config:

```toml
[projects.platforms.options]
auto_stage_forwarded = true
```

- if enabled, a forwarded Telegram message auto-starts collection mode

This should ship only after the explicit mode is stable.

## Limits And Safety

Recommended first-version limits:

- max buffered items: 50
- max buffered images: 20
- max buffered files: 20
- max rendered collected prompt text: reuse existing prompt safety threshold where possible
- max collection age: 24h

On overflow:

- reject the new item and tell the user to flush or cancel first

Suggested message:

```text
Collection buffer is full. Use /collect send <instruction> or /collect cancel.
```

### Stale Collection Cleanup

Collections should not live forever.

Recommended MVP policy:

- assign `ExpiresAt = CreatedAt + 24h` when collection starts
- refresh only `UpdatedAt` while buffering; do not extend `ExpiresAt` indefinitely
- on any later interaction for that `sessionKey`, if the buffer is expired, auto-cancel it before handling the new message
- send a short notification once:

```text
Your buffered collection expired after 24 hours and was discarded.
```

Optional future enhancement:

- periodic background cleanup to reclaim abandoned buffers even if the session never sends another message

## Persistence

Recommended first version:

- in-memory only
- cleared on process restart

User-facing warning:

- when `/collect start` is called, mention that the buffer is temporary until the feature is later made persistent

Future enhancement:

- persist collection buffers under `data_dir`
- restore them on restart

## Testing Plan

### Unit Tests

`core/engine_test.go`

- `/collect start` activates buffering
- buffered text does not start an agent turn
- buffered attachments are included on `/collect send`
- `/collect cancel` clears state
- `/collect status` reports correct counts
- `/collect send` without items fails
- `/collect send` without instruction fails
- `/collect send` without inline instruction enters awaiting-flush-instruction mode
- the next text message after bare `/collect send` is consumed as the final instruction and not buffered
- expired collections auto-cancel on the next interaction
- collection mode coexists correctly with reply context
- collection mode supersedes `pendingAttach`

`platform/telegram/telegram_test.go`

- forwarded Telegram user message populates forward metadata
- forwarded channel message populates chat/source metadata
- non-forwarded messages leave forward metadata empty

### Manual Tests

Telegram:

1. `/collect start`
2. forward 3 text messages
3. send `/collect status`
4. send `/collect send summarize and draft a reply`
5. verify only one agent turn happens

Feishu:

1. `/collect start`
2. send or forward multiple messages
3. send `/collect send summarize these`
4. verify one combined turn

## Rollout Recommendation

### Phase 1

- explicit collection mode on Telegram and Feishu
- no persistence
- no auto-stage
- no platform-specific UI buttons required

### Phase 2

- Telegram inline buttons / Feishu card actions for `Status`, `Send`, `Cancel`
- optional Telegram `auto_stage_forwarded`

### Phase 3

- persistence across restart
- better Feishu forward provenance if the event payload supports it

## File Impact Summary

Expected implementation touch points:

- `core/message.go`
- `core/engine.go`
- `core/i18n.go`
- `core/engine_test.go`
- `platform/telegram/telegram.go`
- `platform/telegram/telegram_test.go`
- `platform/feishu/feishu.go`
- `docs/telegram.md`
- `docs/feishu.md`

## Open Questions

1. Should plain non-forwarded text also be buffered while collection mode is active?
   Recommendation: yes. The mode should be semantic, not forward-only.

2. Should `/collect send` allow an empty instruction?
   Recommendation: no. Require an explicit instruction, but allow it either inline (`/collect send ...`) or as the very next text message after bare `/collect send`.

3. Should we auto-enter collection mode when Telegram detects a forwarded message?
   Recommendation: not by default. Make it opt-in after phase 1.

4. Should buffers survive restart?
   Recommendation: not in MVP; add later if users depend on long-lived staging.
