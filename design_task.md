# Native `task` Tool Design

**Status:** Implemented

## Goal

`task` is a persistent, session-owned task plan for an agent. It records small operational tasks and links them to planning documents without reading or writing those documents.

## Minimal calls

```ts
task({})                              // list active tasks
task({title: "Plan task storage"})  // add as todo
task({title: "Implement now", status: "in-progress"})
task({id: "T-001"})                 // get
task({id: "T-001", status: "blocked"})
task({action: "done", id: "T-001"})
task({action: "delete", id: "T-001"})
```

Creation permits only `todo` (the default) and `in-progress`. Existing tasks may transition to `blocked` or `done`.

## Metadata

Each task stores an opaque, monotonic ID, title, status, priority, tags, and optional planning-document references:

```ts
task({
  title: "Implement task tool",
  documents: ["design_task.md", "docs/planning.md"]
})
```

`documents` is metadata only. Paths are trimmed and deduplicated, but not read or validated as files, which allows a task to reference a document before it exists. On update, omission preserves document links and `documents: []` clears them.

## Lifecycle and normalization

`action` is optional when intent is unambiguous: a title creates, an ID gets, an ID plus editable fields updates, and no target lists. Compatible aliases normalize before validation (`create`, `complete`, `remove`, `taskId`, `maxItems`, and spacing/case variants).

The canonical actions are `list`, `get`, `add`, `update`, `done`, and `delete`. Delete requires explicit action and an exact ID. Completion is idempotent.

`task({})` returns active work (`todo`, `in-progress`, `blocked`). Use a status filter for one state or `status: "all"` for every task. Lists can also filter by priority or tag and are bounded by session policy.

## Persistence and safety

State is stored as `tasks.json` inside `tools.native.task.storage_dir` (default `.tasks` beneath the selected profile's `paths.profiles.<name>.tool_storage` root, or relative to `setting.cwd` when no profile is selected; absolute directories are used as configured) as an atomic JSON document, guarded by a session-local mutex. The mutex does not coordinate separate sessions sharing the same profile root, so this path-scoping change does not make same-profile concurrent writes safe. IDs are never reused after deletion. A malformed task file is never overwritten: reads and mutations return an operational storage error.

`tools.native.task` configures `storage_dir`, capacity, and output limits: task count, list count/bytes, title length, tag/document counts and lengths, and storage bytes. Relative storage directories resolve from the selected profile's `tool_storage` root, or from `setting.cwd` when no profile is selected; absolute directories are accepted as explicit harness configuration and bypass the profile root. These settings remain backend policy rather than routine model arguments.

The definition is opt-in through `sample/tools/native/task.json` and must exactly match the in-code schema.
