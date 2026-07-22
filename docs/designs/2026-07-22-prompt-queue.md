# Prompt Queue Design

Status: proposed

This document describes a prompt queue for `echo-client` interactive chat turns.
When a conversation is already running an agent turn, ordinary follow-up prompts
should be queued instead of being rejected with:

```text
⏳ Previous request still processing, please wait...
```

The queue is intentionally scoped to the actual agent-session serialization
boundary, while preserving the original chat context for replies.

## Problem

Today the engine uses `Session.TryLock()` to reject a new ordinary prompt when
the current local session is busy. This is safe, but it is a poor UX for users
who naturally send several follow-up prompts while the agent is still working.

The existing agent-session turn lock also serializes different local sessions
that share the same underlying agent session ID. A prompt queue must align with
that same boundary, otherwise two chats sharing one Codex/Qoder/Claude session
can build separate queues that are invisible to each other and still race for
the lower-level turn lock.

## Goals

- Queue ordinary text and attachment prompts instead of rejecting them while a
  compatible turn is already running.
- Execute queued prompts in FIFO order by default.
- Support `/queue` for viewing and managing queued prompts.
- Support card buttons to delete, move up, move down, clear, and refresh queued
  prompts.
- Make queues shared across chats that are bound to the same underlying agent
  session.
- Preserve the original chat as the reply destination for each queued prompt.
- Keep management commands immediate and out of the prompt queue.

## Non-Goals

- Persisting queued prompts across process restarts in the first version.
- Aborting or skipping the currently running agent turn from the queue UI.
- Queuing management, shell, custom, or skill commands in the first version.
- Perfect long-term storage of attachment payloads.

## Queue Scope

The queue key should match the agent turn serialization boundary:

```text
<agent-name>:<agent-session-id>
```

For a session that does not yet have an `AgentSessionID`, the engine uses a
temporary local queue key:

```text
pending:<session-key>:<local-session-id>
```

When the first turn binds a real agent session ID, queued prompts on the
temporary key can be migrated to the real key.

This means two Feishu groups that use `/switch` to point at the same Codex
session see and operate on the same queue. Each queued item still keeps its
origin session key, platform, and reply context so the eventual response goes
back to the user/chat that sent it.

## Data Model

Suggested in-memory model:

```go
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
```

The engine owns:

```go
promptQueueMu  sync.Mutex
promptQueues   map[string]*promptQueueState
promptQueueSeq uint64
```

`Message.Images` and `Message.Files` must be cloned when the prompt is queued.
The first version keeps queued attachments in memory.

## Command Classification

Prompt queue eligibility must be decided after ordinary message preprocessing,
but management commands must be detected before attachment-specific command
gates can accidentally make them look like ordinary prompts.

Rules:

- Any message whose trimmed content starts with `/` is not eligible for prompt
  queueing.
- Built-in commands execute immediately.
- Custom commands, skill commands, and shell commands execute immediately if
  possible; if their session is busy, they keep the existing busy behavior.
- Unknown slash commands are not queued. If the target session can start
  immediately, keep the historical behavior and forward them to the agent after
  sending the unknown-command notice. If the target session is busy or another
  queued prompt is ahead, do not enqueue the unknown slash input.
- Permission responses are not queued.
- Pending collection and pending attachment flows keep their own existing
  semantics.

This means a typo such as `/quue` does not get queued and later sent to the
agent as an unintended prompt.

## Enqueue Flow

For ordinary prompts:

1. Resolve pending interactions, voice confirmation, read-aloud requests,
   collection state, pending attachments, aliases, rate limit, and banned words.
2. Resolve the active local session.
3. Resolve the prompt queue key from `agentName + AgentSessionID`, or a temporary
   local key if no agent session has been bound yet.
4. If the local session is busy, the queue key is running, or the queue already
   contains items, enqueue the prompt and reply with its queue position.
5. Otherwise mark the queue key as running and start the turn immediately.

Rate limiting gates enqueue. Queued prompts are not charged against the rate
limit again when drained.

## Drain Flow

When a turn finishes:

1. Release the local session lock and agent turn lock through the existing turn
   cleanup.
2. Mark the queue key as not running.
3. Pop the next queued prompt for that queue key.
4. Validate that the origin session still points at the stored
   `AgentSessionIDSnapshot`. If it no longer matches, skip that prompt and
   notify the origin chat.
5. Try to lock the origin local session and start the queued turn.
6. Repeat until the queue is empty.

The queue mutex must not be held while acquiring session locks, agent-session
locks, or while sending to the agent.

## `/switch` Behavior

`/switch` changes the active agent session for the current chat. Any queued
prompts from that chat that were captured against the previous agent session
must not silently run against the new target.

First-version behavior:

- On successful `/switch`, cancel queued prompts whose `OriginSessionKey` is the
  current chat and whose `AgentSessionIDSnapshot` does not match the new target.
- Reply with the normal switch success plus a short note if queued prompts were
  cancelled.
- Prompts from other chats sharing the previous agent session remain in that
  previous queue.

## `/queue` Command

Suggested command forms:

```text
/queue
/queue list
/queue delete <id|index>
/queue up <id|index>
/queue down <id|index>
/queue clear
```

`/queue` displays the queue for the current queue key. If the platform supports
cards, render a card. Otherwise render a plain text fallback.

Each queued item should show:

- stable monotonic ID
- source chat/user where available
- age
- prompt preview
- image/file counts

Suggested card actions:

```text
nav:/queue
act:/queue up <id>
act:/queue down <id>
act:/queue delete <id>
act:/queue clear
```

Action handlers must validate that the target item still belongs to the current
queue key. IDs must not be reused.

## Current Turn Row

The running prompt can be shown as display-only metadata, but it should not have
delete or skip buttons in the first version. Stopping an active turn involves
agent interruption, partial output, permission prompts, and event stream cleanup;
that remains `/stop` territory.

## Limits

Suggested first-version limits:

- maximum 20 queued prompts per queue key
- maximum 50 MB total queued attachment bytes per queue key
- maximum 120 runes per preview in cards/text lists

If a limit is exceeded, reject the enqueue request with a clear error.

## Restart Behavior

The first implementation is in-memory only. On process restart, queued prompts
are lost. This should be visible in `/queue` documentation and can be addressed
later with a persistent queue and attachment spool directory.

## Test Plan

- Busy ordinary prompt is queued instead of returning the previous busy message.
- Current turn completion automatically starts the next queued prompt.
- Multiple queued prompts execute FIFO.
- Two different session keys sharing the same `AgentSessionID` use the same
  queue.
- `/queue` lists queued prompts.
- Card actions delete, move up, move down, and clear queued prompts.
- Slash commands, including slash commands with attachments, do not enter the
  prompt queue.
- Unknown slash commands do not enter the prompt queue.
- Attachments are cloned and preserved for queued prompt execution.
- Queue full and queued attachment byte limits return clear errors.
- `/switch` cancels stale queued prompts from the switching chat.
- Stale card actions cannot mutate another queue.
