# Conversation Continuity

You are operating inside a custom agent harness where older conversation history may be compacted.

Maintain a small `RESUME_STATE` only when necessary to preserve continuity of an unfinished task.

The repository, files, and tool results are the source of truth. `RESUME_STATE` is only a lightweight checkpoint for recovering after compaction.

## Core Rule

Do **not** read or write memory on every turn.

Only access `resume_state` when one of these conditions is true:

1. Resuming after compaction or when required task context is missing.
2. Starting a new multi-step task that is likely to continue across multiple turns.
3. The active task materially changes.
4. A significant implementation milestone is reached.
5. `nextAction` materially changes.
6. A durable architectural or implementation decision is made.
7. A new unresolved issue or blocker affects future work.
8. The task becomes blocked or completed.
9. The current state would be difficult or expensive to reconstruct from the remaining conversation.

Do **not** update `resume_state` for:

* small edits
* typo fixes
* formatting changes
* minor refactors that do not change the plan
* repeated verification
* routine tool calls
* intermediate observations
* changes already obvious from the immediately preserved conversation
* multiple edits that are part of the same current implementation step

Prefer one checkpoint after a meaningful batch of work rather than one checkpoint per edit.

## Avoid Unnecessary Reads

If the current conversation already contains a valid and recent `RESUME_STATE` or enough context to continue correctly, do not retrieve it from memory again.

Retrieve `resume_state` only when its contents are actually needed.

If the memory record `ref` is already known, reuse it. Do not list memory records again.

## Avoid Unnecessary Writes

Before updating memory, compare the proposed state conceptually with the existing checkpoint.

Do not write an update unless at least one of these materially changed:

* task goal
* task status
* current implementation state
* next action
* important decisions
* unresolved issues
* blockers
* relevant continuation files or symbols

A changed file alone is not sufficient reason to update memory.

A completed small substep is not sufficient reason to update memory if `currentState` and `nextAction` remain effectively the same.

Increase `revision` only when a memory write is actually performed.

## Checkpoint Granularity

`RESUME_STATE` represents the latest logical task, not individual edits.

Example:

Bad:

```text
edit function A
→ update memory

fix type error
→ update memory

rename variable
→ update memory

run test
→ update memory
```

Good:

```text
edit function A
fix type error
rename variable
run test

→ update memory once because the implementation milestone and nextAction changed
```

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

When context has been compacted or continuation context is missing:

1. Retrieve the latest `resume_state`.
2. Read its task goal, current state, and `nextAction`.
3. Use available recent conversation and tool context to validate it.
4. Inspect only the necessary code/files.
5. Continue from `nextAction`.
6. Do not redo completed work unless verification is necessary.
7. If the repository contradicts the checkpoint, trust the repository.
8. Update memory later only if the recovered state materially changes.

## Memory Storage

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

## Request-Minimization Priority

Memory calls are optional bookkeeping, not part of normal execution.

Prioritize:

```text
perform useful work
→ accumulate meaningful state changes
→ checkpoint only when continuity value justifies the memory request
```

instead of:

```text
read memory
→ perform one small action
→ write memory
→ repeat
```

For short tasks that can reasonably finish within the current context, do not create or update `resume_state` unless there is important unfinished state worth preserving.

For long tasks, checkpoint at logical boundaries rather than turn boundaries.

The goal is to minimize memory requests while still allowing reliable recovery after compaction.

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
- Before reading multiple files to understand how code connects, query Codebase Memory first.
- If you discover a separate bug or issue while working on the requested task, do not silently fix or ignore it. Tell me what you found, where it is, and whether it affects the current task.
- Preserve `messages.jsonl` as a golden-output compatibility contract. Code may be refactored or extended, but generated `messages.jsonl` must retain the same observable representation unless the task explicitly requires an output-format change. After changes, regenerate and diff `messages.jsonl`. Treat unrelated differences as regressions.
- Do not finish after implementation. Finish only after the result is demonstrated to work and, where relevant, demonstrated to meet acceptable performance. Workflow: IMPLEMENT → RUN → TEST → MEASURE → FIX → REPEAT.
 
**Don't kill/restart the current backend process, it's used by current conversation.
cwd: `/home/www/Downloads/tmp/myagent3`
