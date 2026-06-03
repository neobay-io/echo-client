# docs/designs/

Point-in-time design documents. Naming: `YYYY-MM-DD-{slug}.md`.

Follows the canonical [docs-organization pattern](https://github.com/neobay-io/skipping-stone/blob/main/patterns/docs-organization/README.md) — full convention lives there. This README only indexes this directory's current entries.

Note: cc-connect's `docs/` root also holds **persistent platform integration guides** (`feishu.md`, `slack.md`, etc.) which are continuously updated — they are NOT design docs and do not get date prefixes.

Status lifecycle: `draft → proposed → accepted → shipped / superseded / rejected`

## Current designs

| Date | File | Status | Description |
|------|------|--------|-------------|
| 2026-04-08 | [`2026-04-08-skills-integration.md`](./2026-04-08-skills-integration.md) | proposed | Per-project native Skills integration — how cc-connect should support and surface skill assets within agent workspaces. |
| 2026-04-16 | [`2026-04-16-reasoning-integration.md`](./2026-04-16-reasoning-integration.md) | proposed | Reasoning-level integration — runtime + config-driven reasoning-depth selection across Codex, Claude Code, Gemini CLI, Qoder. Stable user-facing chat control; preserves native effort knobs where available. |
| 2026-05-09 | [`2026-05-09-transcript-collector.md`](./2026-05-09-transcript-collector.md) | proposed | New `transcript-collector` daemon: scans local AI CLI transcript paths (`~/.claude/projects/`, `~/.codex/sessions/`, ...), parses + summarizes new sessions incrementally, attributes person/org/project via cwd lookup + Echo `/git-binding`, pushes `AgentSessionSummary` to Echo. Companion to `neobay-io/echo/docs/designs/2026-05-09-echo-v2-extension.md` §4.12. |
| 2026-05-20 | [`2026-05-20-forward-collection.md`](./2026-05-20-forward-collection.md) | proposed | Cross-platform (Telegram + Feishu) forwarded-message buffering — users can forward / send multiple messages to cc-connect which buffers them instead of dispatching each to the agent immediately; user then sends one final instruction telling the agent how to process the collected content. Consistent core behavior across platforms with room for platform-specific enhancements where richer forward metadata is available. |

## Adding a new design doc

1. Pick a `slug` (kebab-case, short, descriptive)
2. File: `docs/designs/YYYY-MM-DD-{slug}.md` using today's date
3. Status frontmatter (draft / proposed / accepted / superseded / shipped)
4. Link from this README's "Current designs" table
5. PR review

## Status lifecycle

```
draft → proposed → accepted → shipped
                      └→ superseded
                      └→ rejected
```

After ship: design content gets folded into perpetual reference docs (README.md / INSTALL.md / platform-specific docs/). The dated design doc stays as historical record.
