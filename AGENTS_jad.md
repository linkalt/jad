# Conversation Continuity

Older conversation history may be compacted. Use one small `RESUME_STATE` for the latest logical task only when needed to preserve unfinished multi-step work; it is a checkpoint, not a history of edits. The repository, files, recent conversation, and tool results remain the source of truth.

## Decision Rules

Read and update are separate decisions; needing one does not automatically imply the other.

### Read existing state
Read `resume_state` only when its contents are needed to continue work and cannot be reliably reconstructed from recent conversation and the repository. A new turn, task, or compaction alone does not require a read when the needed context is already available. Reuse a known ref; do not list records to rediscover it.

### Create a checkpoint
Create one shared record only when a multi-step task is likely to outlive the current context and no suitable record exists. Do not create a record for a short task that can finish in this context.

### Update a checkpoint
Update the existing record once after a meaningful batch when its goal, status, current state, next action, durable decisions, unresolved issues, blockers, or useful continuation context materially changes. Do not update after individual edits, routine tests, or other substeps in the same batch. A reason to update does not by itself require reading first if the new state is clear from the repository and recent conversation. Increment `revision` only on an actual write. A changed file alone is not enough to justify an update.

When a task is blocked or completed, update an existing record only if that status change helps future continuation; do not create a record solely to log completion of a short task.

## RESUME_STATE Structure

```json
{
  "revision": 0,
  "task": {
    "id": "",
    "goal": "",
    "status": "working"
  },
  "lastAction": "",
  "currentState": "",
  "nextAction": "",
  "context": {
    "files": [],
    "symbols": []
  },
  "decisions": [],
  "unresolved": [],
  "blockers": []
}
```

## Field Rules

### `revision`

Increment only when the stored checkpoint materially changes.

### `task.id`

Stable identifier for the current logical task. Keep it unchanged while working toward the same outcome.

### `task.goal`

Describe the intended outcome, not the latest edit.

### `task.status`

One of:

* `working`
* `blocked`
* `completed`

### `lastAction`

Describe the latest meaningful batch or milestone, not every individual edit.

### `currentState`

Describe what is true now.

Do not retell task history.

### `nextAction`

The single most useful concrete continuation step.

It should be specific enough that another agent can continue without repeating broad analysis.

### `context.files`

Include only files that are useful for locating the continuation point.

Do not track every touched file.

### `context.symbols`

Include only important continuation symbols.

### `decisions`

Store durable decisions that would be costly or harmful to accidentally reverse.

Do not store obvious implementation details.

### `unresolved`

Store issues that still require investigation or a decision.

### `blockers`

Store only issues currently preventing progress.

## Resuming After Compaction

When continuation context is missing and the decision rules above call for recovery:

1. Retrieve the latest `resume_state`.
2. Read its task goal, current state, and `nextAction`.
3. Use available recent conversation and tool context to validate it.
4. Inspect only the necessary code/files.
5. Continue from `nextAction`.
6. Do not redo completed work unless verification is necessary.
7. If the repository contradicts the checkpoint, trust the repository.
8. Update memory later only if the recovered state materially changes.

## Memory Storage For `RESUME_STATE`

Maintain at most one shared memory record with:

```text
type: "resume_state"
```

Store the complete `RESUME_STATE` directly in its `data` field.

Do not wrap it in another object.

When creating the record:

* provide only `type` and `data`
* let the memory system assign the `ref`

When updating:

* use the known exact `ref`
* do not rediscover the record if its `ref` is already available

If the `ref` is unknown and the state is actually needed:

1. list records with `type: "resume_state"`
2. reuse the existing record if present
3. create one only if none exists

## Bug Report Memory

Store one record per bug using `type: "bug_report"`. If an integration requires `type: "memory"`, add `kind: "bug_report"` in `data`. Keep observations separate from diagnosis, and use references or short excerpts instead of full logs. Do not duplicate the memory system's `ref`.

Example:

```json
{
  "type": "bug_report",
  "data": {
    "schema_version": 1,
    "title": "",
    "status": "investigating",
    "severity": "medium",
    "summary": "",
    "component": "",
    "environment": {},
    "reproduction": {
      "preconditions": [],
      "steps": [],
      "expected": "",
      "actual": ""
    },
    "evidence": [],
    "diagnosis": {"confidence": "unknown", "notes": ""},
    "workaround": null,
    "resolution": {"commit": null, "verification": []}
  }
}
```

Use status values `open`, `investigating`, `confirmed`, `fixed`, `closed`, or `cannot_reproduce`; severity values `critical`, `high`, `medium`, or `low`; and diagnosis confidence `unknown`, `hypothesis`, or `confirmed`.

# Frontend Guidelines

Project-specific guidance for AI coding agents.

<!-- ASTRYX:START -->
Astryx v0.6.2 · 164 components
CLI: run every command as `astryx <cmd>` (shown below as `astryx ...`).

SETUP (once, in your app entry e.g. main.tsx) — without these, components render unstyled:
  import "@astryxdesign/core/reset.css";
  import "@astryxdesign/core/astryx.css";

WORKFLOW — discover, don't guess. Before writing UI:
1. `astryx build "<idea>"` — START HERE: returns a kit (closest [page] + [block]s + [component]s). No args = full playbook.
2. `astryx template <name> [--skeleton]` — scaffold the [page]/[block]s it named, or study their layout. Templates are reference code.
3. `astryx component <Name>` — props + examples for every component you use.

RULES:
- No <div> — components do all layout/spacing, page frame included.
- Frame first: read `astryx docs layout` before writing any page or screen — page frame, region widths, breakpoint behavior.
- Dense data = rows (Table, List/Item), never Card-wrapped list items; Card is for standalone widgets. Status = StatusDot/Token; Badge = counts only.
- Custom styling: component props first; else style/className with tokens — var(--color-*|--spacing-*|--radius-*). No raw hex/px. (No StyleX/Tailwind compiler here — don't use xstyle/utility classes.)
- Tokens for every value (`astryx docs tokens`). Brand/accent belongs in the theme (`astryx theme list` / `theme add <slug>`, or `astryx theme template` for a custom one) — never override --color-* in :root.
- SELF-CHECK before you finish: re-read the file and replace any raw <div>/<span> layout, imported .css/@apply, or hardcoded value (#hex, 16px) with the component or a token (var(--color-*|--spacing-*|…)). If unsure a component/prop exists, run `astryx component <Name>` / `astryx search "<thing>"`; don't hand-roll CSS.

MORE CLI:
  search "<query>"   find any component / hook / doc / template / block
  component --list   164 components by category
  template --list    page + block recipes
  docs <topic>       browser-support, cli-integrations, color, elevation, getting-started, icons, illustrations, internationalization, layout, migration, motion, principles, shape, spacing, styling-libraries, styling, theme, tokens, typography, working-with-ai
  swizzle <Name>     eject component source for deep customization
  upgrade --apply    run after any Astryx or integration dependency bump
<!-- ASTRYX:END -->

# Coding Guidelines

- Prefer modular code design, avoid monolitic.
- **DO NOT hardcode configurable values** such as limits, timeouts, feature flags, or enable/disable options. Put them in the configuration file with sensible defaults.
- Tool implementation guidelines document @tools-guidelines.md.
- Analyze code before propose/suggest an implementation idea.
- Before reading multiple files to understand code, query Codebase Memory first. Use code snippets for local implementation details and trace path for callers, callees, dependencies, and cross-file execution flow. Read source files only when the result is insufficient or exact verification is needed.
- If you discover a separate bug or issue while working on the requested task, do not silently fix or ignore it. Tell me what you found, where it is, and whether it affects the current task. Stored it in `memory`.
- Preserve `messages.jsonl` as a golden-output compatibility contract. Code may be refactored or extended, but generated `messages.jsonl` must retain the same observable representation unless the task explicitly requires an output-format change. After changes, regenerate and diff `messages.jsonl`. Treat unrelated differences as regressions.
- Do not finish after implementation. Finish only after the result is demonstrated to work and, where relevant, demonstrated to meet acceptable performance. Workflow: IMPLEMENT → RUN → TEST → MEASURE → FIX → REPEAT.
 
**Don't kill/restart the current backend process, it's used by current conversation.
cwd: `/home/www/Downloads/tmp/myagent3`
