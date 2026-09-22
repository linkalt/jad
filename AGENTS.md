# Conversation Guidelines

You are operating inside a custom agent harness where older conversation history may be compacted.

Your job is not only to complete the current task, but also to leave the conversation in a state where a future version of yourself can immediately continue the latest unfinished task after compaction.

At the end of every meaningful work turn, maintain a concise `RESUME_STATE`.

Rules:

1. `RESUME_STATE` must describe the latest active task only.
2. Do not write a general conversation summary.
3. Do not repeat large amounts of code, tool output, or history.
4. Record only information necessary to continue the task correctly.
5. Treat the codebase and tool results as the source of truth. The resume state is only a navigation and reasoning checkpoint.
6. Update the existing state rather than accumulating old states.
7. `nextAction` must be concrete enough that another agent can immediately continue without rediscovering what to do.
8. If a task is finished, mark it completed and point `nextAction` to the next active task if one exists.
9. Preserve important decisions that would otherwise be easy to accidentally reverse.
10. Preserve unresolved issues, assumptions, and blockers.
11. Include file paths and symbols only when they are relevant to continuing the task.
12. Do not include temporary observations that no longer matter.
13. When newer information conflicts with an older resume state, newer information is authoritative.
14. Increase `revision` whenever the resume state materially changes.

Use this exact structure:

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

Field semantics:

* `revision`
  Monotonically increasing state revision. Higher revision is newer.

* `task.id`
  Stable identifier for the current logical task. Keep it unchanged while continuing the same task.

* `task.goal`
  The actual outcome being worked toward, not the latest individual action.

* `task.status`
  One of:

  * `working`
  * `blocked`
  * `completed`

* `lastAction`
  The most recent meaningful action that changed the state of the task.

* `currentState`
  Describe what is true now after the last action. This should explain the implementation state, not retell the history.

* `nextAction`
  The single most useful concrete action to perform next. Prefer a specific file, symbol, check, or implementation step.

* `context.files`
  Files directly relevant to continuing the current task.

* `context.symbols`
  Functions, classes, interfaces, modules, or other symbols directly relevant to continuing.

* `decisions`
  Important implementation or architectural decisions that must remain consistent.

* `unresolved`
  Questions or technical issues that still need to be resolved.

* `blockers`
  Anything preventing progress. Leave empty when there are none.

When resuming after compaction:

1. Read the latest `RESUME_STATE` first.
2. Identify the task goal and `nextAction`.
3. Use the recent preserved conversation/tool context to validate the state.
4. Inspect the codebase only where necessary.
5. Continue from `nextAction` instead of restarting analysis from the beginning.
6. Do not redo completed work unless verification is necessary.
7. If the codebase contradicts the resume state, trust the codebase and update the state.

Do not spend excessive tokens maintaining this state. It should normally remain small and focused.

Example:

```json
{
  "revision": 42,
  "task": {
    "id": "tool-execution-refactor",
    "goal": "Move tool execution into a reusable function API shared by sequential and parallel execution paths.",
    "status": "working"
  },
  "lastAction": "Moved sequential tool execution from src/agent.ts into executeTools() in src/tools/executor.ts.",
  "currentState": "Sequential execution now uses executeTools(). The parallel branch in runAgent() still contains the old inline execution logic.",
  "nextAction": "Replace the parallel tool execution branch in src/agent.ts with executeTools() while preserving tool-call result ordering.",
  "context": {
    "files": [
      "src/agent.ts",
      "src/tools/executor.ts"
    ],
    "symbols": [
      "runAgent",
      "executeTools"
    ]
  },
  "decisions": [
    "Tool parameters should remain optional when the backend can provide safe defaults.",
    "Sequential and parallel execution must share the same execution API."
  ],
  "unresolved": [
    "Confirm how executeTools() should preserve original tool-call ordering when calls finish out of order."
  ],
  "blockers": []
}
```

The objective of `RESUME_STATE` is simple:

A future agent should be able to read it and answer:

* What am I currently trying to accomplish?
* What changed most recently?
* What is the system/code state now?
* What exactly should I do next?
* What decisions must I preserve?
* What remains unresolved?

If those questions can be answered quickly, the resume state is good.

# Frontend Guidelines

Project-specific guidance for AI coding agents.

<!-- ASTRYX:START -->
Astryx v0.6.2 · 164 components
CLI: run every command as `npx astryx <cmd>` (shown below as `astryx ...`).

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
