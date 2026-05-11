# cc-connect Transcript Collector — Design

> **Status:** Draft (2026-05-12). Not yet implemented.
>
> **Owner:** Edward (architecture) + cc-connect maintainers + Codex CLI (implementation).
>
> **Purpose:** Extend cc-connect to collect conversation transcripts from local AI agents that are NOT routed through cc-connect itself (Claude Code CLI direct, Codex CLI direct, Gemini CLI, Qoder CLI, etc.), summarize them, and push to Echo's `AgentSessionSummary` endpoint for ingestion into per-org/per-project intel repos.
>
> **Why this matters:** Victor's co-ceo proved that capturing all conversation context (not just scheduled-task output) is the most valuable accumulation mechanism. For an intelligence-org built on multi-tool workflows (Edward uses 7+ tools across multiple machines), the system has a blind spot if it only sees cc-connect-routed sessions.

---

## 0. TL;DR

cc-connect today knows about sessions it **routes** (claudecode / codex / qoder / gemini / etc. invoked through its agent process manager). It does NOT see sessions where Edward runs these CLIs **directly** in terminal, nor sessions in **Claude Mac UI** / **web-based agents**.

Add a `transcript-collector` module to cc-connect that:

1. **Watches** transcript directories on disk (Claude Code CLI, Codex CLI, Gemini CLI, Qoder CLI)
2. **Detects** new / appended sessions since last sync (incremental, idempotent)
3. **Summarizes** transcript via configured agent (cheap model, e.g., haiku / mini)
4. **Attributes** the session: which Person, which Org, which Project (heuristics: cwd, files mentioned, repo binding)
5. **Pushes** to Echo's `POST /api/v1/agent-session-summaries` endpoint
6. **Marks** as processed (last_offset / sha) to avoid duplicate work

For **web-only agents** (Claude Mac UI, ChatGPT, Antigravity, etc.) — chat content is server-side, not collectable from disk. Use **skill-based push** instead (covered in §6).

---

## 1. Architecture context

```
   ┌────────────────────────────────────────────────────────────┐
   │                  Edward's host (Mac Mini / laptop)          │
   │                                                              │
   │  ┌────────────────────────────────────────────────────┐    │
   │  │ Local AI agents (NOT routed through cc-connect)     │    │
   │  │                                                       │    │
   │  │  Claude Code CLI  → ~/.claude/projects/<cwd>/*.jsonl │    │
   │  │  Codex CLI        → ~/.codex/sessions/<yyyy>/.../*.jsonl│  │
   │  │                     + ~/.codex/session_index.jsonl   │    │
   │  │  Gemini CLI       → ~/.gemini/sessions/* (TBD)        │    │
   │  │  Qoder CLI        → ~/.qoder/sessions/* (TBD)         │    │
   │  └─────────────────────┬──────────────────────────────┘    │
   │                        │ filesystem reads                    │
   │                        ▼                                     │
   │  ┌────────────────────────────────────────────────────┐    │
   │  │ cc-connect (NEW module: transcript-collector)        │    │
   │  │                                                       │    │
   │  │  ├── watcher (fsnotify) OR poller (cron, 10min)      │    │
   │  │  ├── parsers (per agent type)                         │    │
   │  │  ├── summarizer (calls local agent for compression)   │    │
   │  │  ├── attributor (cwd → project / org)                 │    │
   │  │  ├── state store (~/.cc-connect/transcripts.db)       │    │
   │  │  └── publisher (HTTP POST to Echo)                    │    │
   │  └─────────────────────┬──────────────────────────────┘    │
   │                        │                                     │
   └────────────────────────┼─────────────────────────────────────┘
                            │ HTTPS via worker gateway or direct
                            ▼
   ┌────────────────────────────────────────────────────────────┐
   │   Echo Server (control plane)                                │
   │                                                              │
   │   POST /api/v1/agent-session-summaries                       │
   │     └──> AgentSessionSummary row (PG)                        │
   │           └──> dispatch processing task                       │
   │                  └──> cc-connect (back to execution plane)    │
   │                         └──> files to intel repo markdown    │
   └────────────────────────────────────────────────────────────┘
```

**Key invariant** (per skipping-stone PROTOCOL.md): cc-connect does git writes; Echo only reads git + dispatches tasks. The collector reports SUMMARIES to Echo; Echo dispatches the "file this into intel repo" task back to cc-connect for actual markdown commits.

---

## 2. Supported sources (Phase 1)

| Source | Path pattern | Format | Detection | Phase |
|--------|--------------|--------|-----------|-------|
| **Claude Code CLI** | `~/.claude/projects/<encoded-cwd>/<session-id>.jsonl` | JSONL: one event per line (user msg, assistant msg, tool calls) | fs watch on dir, mtime + size delta | P1 |
| **Codex CLI** | `~/.codex/sessions/<yyyy>/<mm>/<dd>/<thread-id>.jsonl` + `~/.codex/session_index.jsonl` | JSONL + index registry | tail session_index.jsonl | P1 |
| **Gemini CLI** | TBD — needs install + investigation | TBD | TBD | P2 |
| **Qoder CLI** | TBD | TBD | TBD | P2 |
| **OpenCode** | TBD | TBD | TBD | P3 |
| **iFlow CLI** | TBD | TBD | TBD | P3 |
| **Cursor Agent local sessions** | depends on Cursor IDE storage (likely in workspace) | TBD | TBD | P3 |

**Skill-based push sources (no fs collection):**

| Source | Mechanism |
|--------|-----------|
| **Claude Mac UI / Claude.ai web** | Project instruction includes "at session end POST summary to {ECHO_URL}/api/v1/agent-session-summaries" |
| **ChatGPT / Custom GPT** | Action defined in Custom GPT calls Echo endpoint |
| **Antigravity** | If MCP/extension supported, similar skill |
| **Codex Web UI** | Skill via Codex Web instruction |

---

## 3. Data flow (incremental sync)

### 3.1 State storage

cc-connect maintains `~/.cc-connect/transcripts.db` (SQLite) tracking:

```sql
CREATE TABLE collected_sessions (
    source TEXT NOT NULL,                   -- claude_code_cli | codex_cli | gemini_cli | qoder_cli | ...
    source_session_id TEXT NOT NULL,        -- session ID from the agent
    source_file_path TEXT NOT NULL,         -- absolute path on disk
    last_offset INTEGER NOT NULL DEFAULT 0, -- byte offset of last consumed content
    last_line_count INTEGER NOT NULL DEFAULT 0,
    last_file_sha TEXT,                     -- for sanity verification
    last_sync_at INTEGER NOT NULL,          -- unix ts of last successful push
    person_slug TEXT,                       -- attributed Person (default: configured operator)
    org_slug TEXT,                          -- attributed Org (from attribution)
    project_slug TEXT,                      -- attributed Project (from attribution)
    workspace_cwd TEXT,                     -- detected working directory at session start
    summary_count INTEGER NOT NULL DEFAULT 0,  -- number of summary pushes for this session
    status TEXT NOT NULL DEFAULT 'active',  -- active | completed | error | skipped
    notes TEXT,
    PRIMARY KEY (source, source_session_id)
);

CREATE TABLE collector_runs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    started_at INTEGER NOT NULL,
    finished_at INTEGER,
    sessions_seen INTEGER NOT NULL DEFAULT 0,
    sessions_new INTEGER NOT NULL DEFAULT 0,
    sessions_updated INTEGER NOT NULL DEFAULT 0,
    summaries_pushed INTEGER NOT NULL DEFAULT 0,
    errors INTEGER NOT NULL DEFAULT 0,
    error_detail TEXT
);
```

### 3.2 Sync logic per session

```
For each session detected:
  1. Read source_file_path from offset last_offset
  2. If new content:
     a. Parse new events (per-format parser)
     b. Append to in-memory session buffer
     c. If session shows "completion signal" (user said /quit, exit, or session inactive > 30min):
        - mark as completed; generate FINAL summary
     d. Else if new content exceeds threshold (e.g., 50 messages or 200KB since last summary):
        - generate INCREMENTAL summary (for long-running sessions)
     e. Else: keep buffering, don't push yet
  3. Push summary to Echo (see §5)
  4. Update collected_sessions.last_offset on successful push
```

**Why incremental + final summaries?** Long sessions (Edward's design discussions can run 100+ messages) need progressive capture; otherwise a crash loses all context. Default cadence: every 50 messages OR every 30min of inactivity → push.

### 3.3 Polling vs filesystem watch

**P1: polling (every 10 min via cron / launchd timer).** Simpler, no fsnotify edge cases. Acceptable latency for non-realtime use.

**P2: fsnotify-based watch.** Real-time push to Echo within seconds of agent activity. Heavier resource use.

Configure via `config.toml`:

```toml
[transcripts]
enabled = true
mode = "poll"               # poll | watch (P2)
interval_secs = 600         # 10min default for poll mode
state_db_path = "~/.cc-connect/transcripts.db"
echo_endpoint = "${ECHO_URL}/api/v1/agent-session-summaries"
echo_auth_token = "${ECHO_PERSON_TOKEN}"

# per-source config
[transcripts.claude_code_cli]
enabled = true
projects_dir = "~/.claude/projects"
sessions_dir = "~/.claude/sessions"

[transcripts.codex_cli]
enabled = true
sessions_dir = "~/.codex/sessions"
session_index = "~/.codex/session_index.jsonl"

[transcripts.gemini_cli]
enabled = false   # P2

[transcripts.qoder_cli]
enabled = false   # P2

[transcripts.summarizer]
agent = "claudecode"           # which agent runs the summary
model = "haiku-4.6"            # cheap model preferred
max_summary_tokens = 1500
include_raw_excerpt = false    # if true: include first/last N msgs verbatim
template_skill = "skipping-stone/skills/intake/summarize-transcript.md"

[transcripts.attribution]
# Default Person (used unless overridden per-session)
default_person_slug = "edward"
# Workspace cwd → project mapping (overrides Echo lookup)
[transcripts.attribution.cwd_overrides]
"/Users/edward/Projects/diffus-intel" = { org = "graviti", project = "diffus" }
"/Users/edward/Projects/graviti-intel" = { org = "graviti", project = null }
"/Users/edward/Projects/edward-personal" = { org = null, project = null, person = "edward" }
# else: cc-connect queries Echo /api/v1/git-binding to find org/project for the cwd's git repo URL

[transcripts.privacy]
# Paths to skip entirely (e.g., personal sensitive)
exclude_paths = ["~/.claude/projects/-Users-edward-personal-private"]
# Patterns in session content that trigger redaction
redact_patterns = [
  "(?i)password|secret|api[_-]?key|token",
  "ssn|credit[_-]?card"
]
# Sessions matching these patterns get redacted summaries (placeholder masks)
```

---

## 4. Parsers (per source)

Each source needs a parser that takes raw file content + offset → structured events.

### 4.1 Claude Code CLI parser

Format: JSONL, one event per line. Events look like:

```json
{"type":"user","message":{"role":"user","content":"..."},"uuid":"...","timestamp":"..."}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"..."},{"type":"tool_use",...}]},...}
{"type":"tool_result","tool_use_id":"...","content":"..."}
```

Parser implementation: `core/transcripts/parsers/claude_code_cli.go`

```go
type ClaudeCodeEvent struct {
    Type      string          // user | assistant | tool_use | tool_result | meta
    Timestamp time.Time
    Role      string
    Text      string          // flattened content
    ToolName  string          // for tool_use events
    ToolInput json.RawMessage
    UUID      string
}

func ParseClaudeCodeCLI(reader io.Reader, fromOffset int64) ([]ClaudeCodeEvent, int64, error) {
    // Scan line-by-line, jsonl decode each, return events + new offset
}
```

Session metadata extraction:
- Working directory: extracted from first message or system metadata
- Session ID: filename UUID
- Models used: tracked across assistant messages

### 4.2 Codex CLI parser

Format: JSONL per session. Codex uses `session_index.jsonl` for registry (read this first).

Parser: `core/transcripts/parsers/codex_cli.go`

```go
type CodexSessionIndexEntry struct {
    ID          string    // 019c7f8e-7a24-...
    ThreadName  string    // "Implement browser recording agent"
    UpdatedAt   time.Time
}

type CodexEvent struct {
    // similar to Claude Code, format varies; needs investigation of actual session files
    Type      string
    Timestamp time.Time
    Role      string
    Text      string
    // ...
}
```

Sync strategy: tail `session_index.jsonl` for new/updated sessions; only fetch session files referenced.

### 4.3 Other sources (P2+)

Parsers added as each source's format is investigated. Common interface:

```go
type Parser interface {
    Name() string
    Discover(config Config) ([]SessionFile, error)        // find all candidate session files
    Parse(file SessionFile, fromOffset int64) (events []Event, newOffset int64, err error)
    DetectCompleted(events []Event, lastActivityAt time.Time) bool
}
```

---

## 5. Summarization

Each push to Echo is a SUMMARY, not raw transcript. Reasons:
- Raw transcripts can be massive (10MB+ for long sessions)
- Echo PG is observability backend, not log archive
- Summary is what's actually consumable by other tasks (intel filing, attention brief)

### 5.1 Summary structure

```json
{
  "source": "claude_code_cli",
  "source_session_id": "6c7e4905-e753-4093-8a3a-dfab1b987f0c",
  "source_file_path": "/Users/edward/.claude/projects/.../6c7e4905-....jsonl",
  "person_slug": "edward",
  "org_slug": "graviti",
  "project_slug": "diffus",
  "workspace_cwd": "/Users/edward/Projects/diffus-intel",
  "session_started_at": "2026-05-12T08:30:00Z",
  "session_last_activity_at": "2026-05-12T10:45:00Z",
  "is_incremental": false,
  "summary_segment_index": 1,
  "summary_md": "## Summary\n\nDuring this session...",
  "extracted_decisions": [
    {"title": "...", "decision_md": "...", "review_date_suggested": "2026-06-12"}
  ],
  "extracted_insights": [
    {"title": "...", "body_md": "...", "candidate_world_model": "customer-world"}
  ],
  "extracted_capability_gaps": [
    {"capability_slug": "image_gen", "gap_description_md": "..."}
  ],
  "key_files_touched": ["projects/diffus/capabilities/image_gen.md", "..."],
  "estimated_message_count": 87,
  "estimated_token_count": 25000,
  "models_used": ["claude-sonnet-4.6"],
  "raw_sample_excerpts": null,
  "metadata": {
    "tool_calls_count": 23,
    "files_read_count": 14,
    "files_edited_count": 6
  }
}
```

### 5.2 Summarizer skill template

Lives in skipping-stone: `skills/intake/summarize-transcript.md`

Prompt (sketch):

```markdown
# Skill: summarize-transcript

> Purpose: Condense an agent session transcript into structured summary
> Called by: cc-connect transcript-collector
> Inputs: transcript_events (JSON array), session_metadata
> Target agent: claudecode (haiku or cheap model)

## Prompt

You are summarizing an agent session conversation. Produce structured output per the schema below.

Goals:
- Surface DECISIONS made during the session (anything that should be recorded as a decision_record per intelligence-org PATTERNS.md §5)
- Surface INSIGHTS about customer / product / system / member / org / playbook (candidates for world model entries)
- Surface CAPABILITY GAPS detected (things the user wanted but couldn't do — see PATTERNS.md §4)
- Note KEY FILES touched (with their relative paths in the workspace)
- Provide concise narrative summary (5-15 sentences) covering what was accomplished + what's open

Be honest about what's UNCLEAR. Do NOT invent decisions/insights/gaps that aren't actually in the transcript.

Schema:
{{json_schema_inline}}

Transcript:
{{transcript_events}}

Session metadata:
{{session_metadata}}

Output (JSON only, no preamble):
```

### 5.3 Cost / latency

For a typical 50-message session (~20k tokens):
- Summarization input: ~20k tokens
- Output: ~1500 tokens
- Cost with haiku-class model: ~$0.01-0.05
- Latency: ~5-20 seconds

For long sessions (200+ messages): chunked + recursive summarization OR use 200k-context model directly. Default: switch to sonnet for sessions > 100k tokens transcript.

Daily volume estimate (Edward, ~5 sessions/day across CLIs): 5 × $0.05 = ~$0.25/day = ~$8/month. Acceptable.

---

## 6. Attribution: which Person / Org / Project does this session belong to?

cc-connect needs to attribute each summary correctly so Echo can file it into the right intel repo.

### 6.1 Person attribution

- Easiest: cc-connect is configured with `default_person_slug` (e.g., "edward" on Edward's host)
- Future: multi-user host support via per-user authentication of cc-connect

### 6.2 Org / Project attribution

Heuristics in order of priority:

1. **Explicit cwd override** (`config.toml [transcripts.attribution.cwd_overrides]`)
   - Edward configures: `/Users/edward/Projects/diffus-intel → graviti / diffus`

2. **Git remote → Echo binding lookup**
   - cc-connect detects session's cwd, reads git remote, queries Echo:
     ```
     GET /api/v1/git-binding?repo_url=git@github.com:diffus-me/diffus-intel.git
     → {org_slug: "graviti", project_slug: "diffus"}
     ```
   - Echo's existing `project_repo_bindings` table extended to support intel repos

3. **LLM-based inference** (fallback)
   - Summarizer skill includes attribution as one of its outputs
   - Looks at files touched + topics discussed to guess org/project
   - Marks attribution as `low_confidence: true` if unclear

4. **Unfiled bucket** (last resort)
   - If no attribution: file to `BeEdward/edward-personal/inbox/unfiled-YYYY-MM-DD-{session-id}.md`
   - Edward periodically reviews + manually files

### 6.3 Person-level transcript filing

For sessions NOT attributable to any org/project (personal reflection, exploration):
- Files into `BeEdward/edward-personal/episodes.md` as new entries
- Or `BeEdward/edward-personal/conversations/YYYY-MM-DD-{topic}.md` for longer

---

## 7. Privacy & redaction

Critical concern: transcripts may contain sensitive content (passwords mentioned, personal worries, etc.).

### 7.1 Path-level exclusions

`[transcripts.privacy.exclude_paths]` — directories never scanned. E.g., personal scratch project.

### 7.2 Pattern-based redaction

`[transcripts.privacy.redact_patterns]` — regex patterns. Matches in summary content get replaced with `[REDACTED]`.

### 7.3 Visibility on Echo side

`AgentSessionSummary.visibility` field:
- `person_only` — Edward + AI only (default for personal sessions, edward-personal repo)
- `org_members` — within the attributed org (default for project/org session)
- `public` — never (we don't expose any of this externally)

### 7.4 Self-opt-out

Edward can mark any session before/during/after via:
- Magic phrase in transcript: `cc-connect: skip` → collector marks session status=skipped
- Manual delete via Echo dashboard
- Edward edits `transcripts.db` to mark sessions as skipped

### 7.5 Retention

- Raw transcripts: never stored in Echo PG (only summaries)
- Summaries: kept indefinitely by default; Edward can purge via API
- State DB (`~/.cc-connect/transcripts.db`): keeps offsets only, not content; small footprint

---

## 8. Web-only agent skill push (companion mechanism)

For agents whose transcripts are NOT on local disk (Claude Mac UI, ChatGPT, Antigravity, Codex Web):

### 8.1 The skill

In each web agent's configuration, add an instruction:

```
At session end, before signing off, produce a structured summary of what we
accomplished and POST it to:

  https://echo.{your-domain}.com/api/v1/agent-session-summaries

with headers:
  Authorization: Bearer {ECHO_PERSON_TOKEN}
  Content-Type: application/json

with body matching this schema:
{
  "source": "claude_mac_ui" | "chatgpt" | "antigravity" | "codex_web",
  "source_session_id": "<your_session_id_if_available>",
  "person_slug": "edward",
  "org_slug": "<inferred>",
  "project_slug": "<inferred>",
  "summary_md": "...",
  "extracted_decisions": [...],
  "extracted_insights": [...],
  "extracted_capability_gaps": [...]
}

If you cannot determine org/project: set null and Echo will file as personal.
```

### 8.2 API-driven web agents

For web agents with official APIs (Anthropic Claude API, OpenAI API):
- cc-connect cron drives them directly via API
- This isn't "transcript collection" per se; it's regular task dispatch
- Already covered in ECHO-V2 §4.11 CronJob

### 8.3 Mac UI specifically

Claude Mac UI does NOT support custom skills/system prompts in the same way Projects do. Workarounds:
- Use Claude.ai web (browser) which DOES support Projects with instructions → use skill push
- For Mac UI native: manual paste-and-push tool (CLI command `cc-connect transcript-push`) where Edward copies summary text and runs the command

---

## 9. Implementation milestones

For Codex CLI pickup, suggested PR sequence:

| PR | Title | Files | Acceptance |
|----|-------|-------|-----------|
| 1 | Scaffold transcripts package + state DB | `core/transcripts/*.go`, schema migration | sqlite db created, package builds |
| 2 | Add Claude Code CLI parser | `core/transcripts/parsers/claude_code_cli.go` + tests | parses sample JSONL correctly |
| 3 | Add Codex CLI parser | `core/transcripts/parsers/codex_cli.go` + tests | parses sample, uses session_index |
| 4 | Implement polling collector loop | `core/transcripts/collector.go`, integrate with launchd timer | polls every 10min, dedups via state DB |
| 5 | Add summarizer integration | calls configured agent with `skipping-stone/skills/intake/summarize-transcript.md` template | summary generated for sample sessions |
| 6 | Add attribution module | `core/transcripts/attribution.go`, Echo `/git-binding` client | sample cwd → org/project resolution |
| 7 | Add publisher (HTTP push to Echo) | `core/transcripts/publisher.go` | POST integration with mock Echo endpoint |
| 8 | End-to-end test on Edward's machine | manual run, dry-run mode first | summaries pushed for last 10 sessions |
| 9 | Privacy: exclude_paths + redaction | implement filters | sensitive sample sessions get redacted |
| 10 | Add CLI command `cc-connect transcript-push` | manual paste-and-push for Mac UI / unsupported agents | Edward can paste summary, gets posted |
| 11 | (P2) Add fsnotify-based watcher mode | `core/transcripts/watcher.go` | real-time push within 5s of new content |
| 12 | (P2) Add Gemini CLI / Qoder CLI parsers | once paths investigated | parses sample sessions |
| 13 | Dashboard view of recent transcripts | (Echo side, separate PR) | Edward can browse / filter / re-process |

Each PR ≤ 500 LOC. Codex should request review (Edward + Claude Code) before merging.

---

## 10. Open questions

### Q-TC-1: Session "completion" detection

When is a session "done" (push final summary) vs "still active" (push incremental)?

Proposed heuristics:
- Inactivity > 30 min after last message → considered done
- File hasn't grown in last 60min → considered done
- Edward's explicit `/quit` or exit message in transcript → done

Active sessions: push incremental every 50 messages OR every 30min if no completion.

### Q-TC-2: What if cc-connect collects a session that was ALSO routed through cc-connect?

cc-connect itself records all routed sessions in `~/.cc-connect/` JSON files. If Edward has direct CLI + cc-connect routing for the same agent type, sessions could be duplicated.

Solution: cc-connect's own routed sessions get `source: cc_connect_routed`; local CLI sessions get `source: local_<agent>_cli`. Echo deduplicates if both have same content (rare) but otherwise keeps both for completeness.

### Q-TC-3: Multi-host coordination

If Edward has cc-connect on 3 machines and works in same project from all, transcripts collected from each. Echo deduplicates on `(source, source_session_id)` since session IDs are globally unique (UUIDs).

### Q-TC-4: Performance impact on running CLIs

Reading files while CLI is actively writing — read locks? CLIs typically append-only; reading shouldn't block them. Use `O_RDONLY` always; never write to source files.

### Q-TC-5: Disk space for buffered events before summarization

For very long sessions, we read content in chunks. Buffer in memory or stream-process? For Phase 1: read full delta each cycle (limited to last_offset → EOF), keep in memory, summarize, discard. If sessions exceed memory budget: chunked re-summarization.

### Q-TC-6: Edward's other machines (Mac Mini, Windows workstation)

Each machine runs its own cc-connect instance with own transcript-collector pointing at that machine's local paths. All push to the central Echo. Echo's `host_id` attribution lets you trace which machine produced which summary.

### Q-TC-7: cc-connect not running when sessions happen

If cc-connect is down, sessions still accumulate on disk. When cc-connect comes back: full sync of any sessions with timestamps after last collector_runs entry. Catches up automatically.

### Q-TC-8: Backfill of pre-existing sessions

When transcript-collector is first enabled, what about all the EXISTING sessions on disk? Optional config `[transcripts] backfill_days = 7` — collector summarizes sessions modified in last N days at first run. Default: 0 (only new sessions going forward).

---

## 11. Why this matters (closing thought)

Victor's co-ceo: "Ad-hoc 对话的知识捕获" — every conversation should leave a trace in the relevant knowledge file. Without this, the system has gaps.

For Edward's multi-tool reality: ~50% of his AI conversation might happen in tools NOT routed through cc-connect (Claude Code CLI direct in terminal, Codex CLI direct, Mac UI when on phone, etc.). Without transcript collection, ~50% of accumulation potential is lost.

This module closes that gap. Combined with the WebAgentSummary skill-push mechanism (Echo §4.12), the system captures:

| Source | Capture method |
|--------|----------------|
| cc-connect routed sessions | Native (cc-connect already stores) |
| Local CLI sessions (this design) | filesystem collector |
| Web/Mac UI sessions | skill-based push |
| Scheduled task outputs | Echo Task/Artifact records |

→ near-complete observability of all AI conversation in the org.

---

*Created 2026-05-12. Owner: Edward + cc-connect maintainers. Implementation: Codex CLI.*
*Companion: `neobay-io/echo/ECHO-V2-EXTENSION-DESIGN.md` §4.12 AgentSessionSummary.*
