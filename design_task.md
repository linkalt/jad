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
task({ops: [                         // several actions, one call, all or nothing
  {title: "Write tests"}, {id: "T-001", status: "done"}, {action: "delete", id: "T-002"}
]})
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

Ids may be written `1`, `T-1`, `t1` or `T-001`; all mean `T-001`. Anything that is not a positive integer after that is `invalid_task_id`.

`task({})` returns active work (`todo`, `in-progress`, `blocked`). Use a status filter for one state or `status: "all"` for every task. Lists can also filter by priority, by tag and by `q` (every word is a case-insensitive substring of the title, a tag or a document path), and are bounded by session policy. A list reports `count`, `total` (matches before the window) and, only when more remain, `has_more` and `next_offset`; `offset` skips that many matches (stateless paging). An `offset` past the total lists nothing and shows `total`. `detail: true` names the document paths and adds ` | updated: <ts>` to each line. The byte budget `max_list_bytes` can cut a page; `next_offset` is then `offset + count`. The first line of a page is always shown, even when it alone exceeds the budget, so `next_offset` always moves forward.

## Batch

`ops` holds 1 to `max_batch_ops` (default 20, hard max 200) objects with the same fields as a flat call. It cannot be combined with another top-level field (`invalid_batch`), and a longer list is `batch_too_large`. Each op is normalized and validated like a flat call, then all run in order against one loaded document and are written once. The batch is all or nothing: the first op that breaks a rule, names a task that does not exist or hits a store limit stops it before the write. The result is `batch_failed` with `failed_op` (1-based), `op_error_code`, `op_error`, `task_id`/`open_ids` for a missing task, and `applied: 0`, in the status of the failing op.

A successful batch answers `action: batch`, `count: N` and one line per op: `1 add T-004 | todo | normal`, `2 update T-001 | in-progress | high`, `3 done T-002`, `4 delete T-003`, `5 get <list line>`, and for a list `6 list 2 of 7, next_offset 2` followed by its lines indented by two spaces. Mutation lines omit the title and tags and are never cut; list sections share `max_list_bytes` (the first list line of the call is shown even when it alone exceeds it; a later section that gets no line ends its header with `, over budget, ask again alone` and prints no `next_offset`, because that would equal its own offset; ask for it in a call of its own, where it is the first list and shows a line). The summary is `N ops applied` when the file was written and `N ops run` when no op changed anything. Ops cannot refer to ids created earlier in the same batch.

`task_not_found` carries `open_ids`: the ids of tasks that are not done, in list order, at most `list_limit`, then `(+N more)`. A miss, or `done` on a task that is already done, writes nothing.

## Persistence and safety

State is stored as `tasks.json` inside `tools.native.task.storage_dir` (default `.tasks` beneath the selected profile's `paths.profiles.<name>.tool_storage` root, or `setting.cwd` when no profile is selected; a configured relative directory resolves from the session cwd; absolute directories are used as configured) as an atomic JSON document (the temporary file is synced before the rename), guarded by a process-local mutex and a cross-process lock file `.tasks.json.jad-lock` beside it (`sessionlock`), held from the load to the write, so separate processes sharing a storage directory do not lose updates. The lock file is persistent. Reading while `tasks.json` does not exist yet takes no lock and creates nothing (no directory, no lock file). IDs are never reused after deletion. A malformed task file is never overwritten: reads and mutations return an operational storage error.

`tools.native.task` configures `storage_dir`, capacity, and output limits: task count, list count/bytes, batch size (`max_batch_ops`), title length, tag/document counts and lengths, and storage bytes. A configured relative storage directory resolves from the session cwd; only the default sits under the selected profile's `tool_storage` root; absolute directories are accepted as explicit harness configuration and bypass the profile root. These settings remain backend policy rather than routine model arguments.

The definition is opt-in through `sample/tools/native/task.json` and must exactly match the in-code schema. The schema has no top-level `allOf`; `Execute` enforces the per-action rules (`missing_title`, `missing_task_id`, `missing_update`, and the rest) for flat calls and for every op of a batch. A definition file with the schema from before `ops` (`taskInputSchemaV1`) is still accepted and the current schema is served.

Decision record: `backend-go/DECISIONS.md`, "Native `task` redesign"; proposal: `task-redesign.md`.
