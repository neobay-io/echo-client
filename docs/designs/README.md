# docs/designs/

Point-in-time design documents. Naming: `YYYY-MM-DD-{slug}.md`.

## Convention

Design docs describe what was proposed / decided at a **specific point in time** for a **specific scope of changes**. They differ from perpetual reference docs (the existing `docs/*.md` platform integration guides like `feishu.md`, `slack.md`, etc.) which are continuously updated.

When a milestone's design ships and stabilizes, relevant content gets folded into the perpetual reference docs; the dated design doc remains as historical record.

Modeled on co-ceo's pattern: time-bound artifacts use date prefix; stable references don't.

## Current designs

| File | Status | Description |
|------|--------|-------------|
| [`2026-05-09-transcript-collector.md`](./2026-05-09-transcript-collector.md) | proposed | New `transcript-collector` daemon module: scans local AI CLI transcript paths (`~/.claude/projects/`, `~/.codex/sessions/`, etc.), parses + summarizes new sessions incrementally, attributes person/org/project via cwd lookup + Echo `/git-binding`, pushes `AgentSessionSummary` to Echo. Companion to `neobay-io/echo/docs/designs/2026-05-09-echo-v2-extension.md` §4.12. |

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
