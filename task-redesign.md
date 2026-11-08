# Redesign proposal for the native `task` tool (read-only phase)

**Status: approved (Option B; Q1 yes, Q2 no, Q3 all or nothing, Q4 yes, Q5 yes, Q6 stateless offset) and implemented 2026-11-08; see `backend-go/DECISIONS.md`, "Native `task` redesign".**

I changed no files. `git status --porcelain` shows only `?? docs/`. I ran one benchmark pass (`go test -bench TaskStoreList`) and several read-only greps. Paths are under `backend-go/internal/` unless stated.

## 1. Diagnosis

Verdicts: **REAL** means worth fixing, **MINOR** means small but cheap, **OK** means leave it.

| # | Finding | Evidence | Verdict |
|---|---|---|---|
| 1 | Definition cost. The input schema is 1,747 B compact (my measurement). Your 1,866 B figure is presumably the schema plus the envelope; I did not reconstruct it. The three `allOf` rules are 627 B of that, and `action`, `status`, `priority`, `documents` and `tags` are about 600 B. Anthropic strips top-level `allOf` before the request (DECISIONS.md:356), so there the model pays about 1.1 KB and never sees the rules. `Execute` already enforces all of them with clearer errors (`missing_title`, `missing_task_id`, `missing_update`, task.go:~330-449). | task.go:15-45, 77-100 | **MINOR.** Roughly 130 tokens per request. Worth trimming only as a side effect. |
| 2 | One action per call, no batch. Planning N tasks costs N round trips plus N full-file rewrites. Without `ReadOnly`, `ChangesState` is true for every task call (tools/catalog.go:324-338). `toolBatchConcurrency` serializes only when scheduling is `"ordered"` (session/tool_batch.go:30-40). Under any other scheduling, an `add` and a `list` in one response can run concurrently and race. I did not check the default scheduling value. | task.go:224-449 | **REAL.** This is the largest round-trip cost. |
| 3 | List and search surface. Filters are `status`, `priority`, `tag` and `limit` only. There is no text search, no `offset` or cursor, and no total. Default order is status rank (blocked, in-progress, todo, done), then priority, then `updated_at` descending, then id (task_store.go:249-302). The default window is 100 lines and `max_list_limit` is at most 500. When `has_more` is set, the only way forward is a bigger `limit` or a narrower filter. Documents show only as `documents: N` (task.go:301-329), so finding the task linked to a given file means `get` on each one. | task.go:224-329 | **REAL.** `has_more` has no continuation. |
| 4 | `task_not_found` runs a second `store.List(status all, Limit=ListLimit)`. That is another full load, sort and clone, and it prints up to 100 ids with done tasks last and no truncation marker (task.go:~520-535, test at valid_values_errors_test.go:87-114). | task.go:451-564 | **MINOR.** About 600 B of output on a miss. The recovery aid itself is good. |
| 5 | A miss rewrites the file. `Update` and `Delete` return nil when the id is not found. `mutate` then calls `write` unconditionally, so a typo'd id rewrites `tasks.json`, bumps nothing, and can fail on the size cap with a misleading storage error. `done` on an already-done task refreshes `updated_at` and reorders it. | task_store.go:303-364 | **REAL**, and a cheap correctness fix. |
| 6 | Persistence. Every mutation does load, validate all records, mutate, marshal and rename. The lock is one in-process `sync.Mutex`. There is no cross-process lock. `sessionlock.Acquire` exists (sessionlock/lock.go:103) and is unused here. There is no fsync. Rename is atomic, so reads are never torn, but two processes sharing a directory can lose updates. A power cut could leave an empty file, which `load` refuses as "not valid JSON", so the store is blocked until someone fixes it by hand. Cost is bounded by the caps (1000 tasks, 1 MiB). In my benchmark run, `List100` took 0.63 ms and 146 KB/op. `ListManyMatches` (3000 tasks, limit 10) took 18.3 ms, 5.0 MB/op and 54k allocs/op. The waste is that `List` clones every match before sorting and truncating (task_store.go:~270). | task_store.go:153-205, 303-444 | Speed is **OK**. Cloning all matches is **MINOR** and an easy fix. The cross-process lock is **MINOR**, needed only if two backends share a directory (assumed, not verified). |
| 7 | Ids and ownership. `T-%03d` from a monotonic `next_id` is fine (task_store.go:207-247). The shape is `T-[0-9]{3,}` (task.go:74). An id like `1` or `t-1` fails with `invalid_task_id`. `normalizeTaskArguments` only uppercases and trims (task.go:102-193). One `TaskStore` is cached per storage directory and shared by every session, profile and sub-agent (app/resolver.go:841-862). Records carry no owner. `ExecutionContext.SessionKey` exists for scoping (tools/catalog.go:40-43), but the task tool does not use it. | | Ids are **OK**. Lenient id parsing is **MINOR**. Missing ownership is **a deliberate feature** (a parent sees child progress), not a bug. See open question Q2. |
| 8 | Statecontext and checkpoint coupling. The only link is `TaskStateSource.List(TaskListFilter{Statuses, Limit})` (task.go:566-703). The summary mode makes one unbounded `List`. Any additive field on `TaskListFilter` is invisible to it. The task storage directory is in `checkpoint.HarnessPaths` (checkpoint/service.go:268-282), so `tasks.json` and any file beside it is already excluded from snapshots. | | **OK.** Keep the sort and the `Statuses` and `Limit` semantics and nothing moves. |
| 9 | `ReadOnly` is not a free fix. Setting it also changes sub-agent pools (`childToolPools`, native/subagent.go:249-250) and plan mode (tools/plan_mode.go:29-30). | | Don't. Batch removes the need. |
| 10 | Output verbosity. Single-task results echo the whole record: `action`, `task_id`, `title`, `status`, `priority`, `tags` and `documents` (task.go:566-585). The add golden is 256 B. List lines are already compact. | | **OK for single calls.** Echoes inside a batch should be leaner. |

## 2. Options

**A. Tune-up, same surface.**
- Surface: the six actions stay. Add `offset` and `q` to list, add `total`, drop `allOf`, trim descriptions.
- Fixes: no write on a miss, `not_found` uses the same read, clone only the returned page.
- Effect: definition drops to about 1.2 KB on non-Anthropic providers. Round trips are unchanged.
- Cost: about 150 LOC plus tests.

**B. Option A plus an atomic batch (`ops`), a cross-process lock and lenient ids.**
- Surface: the flat single-call shape is kept as is. A new optional `ops` array runs 1 to N actions in one call, all or nothing, in one load and one write.
- Store: `tasks.json` and `schema_version` stay unchanged. A hidden lock file via `sessionlock` is added beside it.
- Effect: N operations become 1 round trip and 1 write. Definition is about 1.24 KB (schema) against 1,747 B today.
- Cost: about 600-900 LOC including tests, a setting, the manifest and the goldens.

**C. Owner-scoped store with per-owner ids, an append-only log, and document sidecars.**
- Effect: the best isolation.
- Cost: about 2000 LOC. It breaks `tasks.json`, the id shape, statecontext semantics and sharing, and nothing in the evidence shows a problem it solves.

**Recommendation: B.** Round trips are the real cost (finding 2). The definition size is a small cost, and the store is fast enough at its caps. A fixes only the small items and leaves the round-trip cost. C pays migration risk for isolation nobody has asked for, and it removes the parent's view of child progress.

## 3. API sketch for B

**Definition.** The description becomes `"Create and maintain session tasks. Pass ops to run several actions in one call."`. The `allOf` is removed (see Q1).

```json
{"type":"object","properties":{
 "action":{"type":"string","enum":["list","get","add","update","done","delete"],"description":"Inferred when omitted."},
 "id":{"type":"string","description":"Task id, e.g. T-001."},
 "title":{"type":"string","minLength":1},
 "status":{"type":"string","enum":["todo","in-progress","blocked","done","all"],"description":"all is a list filter. List default: not done."},
 "priority":{"type":"string","enum":["low","normal","high","urgent"],"description":"Default normal on add."},
 "tags":{"type":"array","items":{"type":"string"},"description":"[] clears on update."},
 "documents":{"type":"array","items":{"type":"string"},"description":"Planning-document paths; [] clears."},
 "tag":{"type":"string","description":"List filter."},
 "q":{"type":"string","description":"List: words found in title, tags or document paths."},
 "limit":{"type":"integer","minimum":1},
 "offset":{"type":"integer","minimum":0,"description":"List: skip this many."},
 "detail":{"type":"boolean","description":"List: add document paths and update time."},
 "ops":{"type":"array","minItems":1,"items":{"type":"object"},"description":"Batch instead of the fields above: objects with the same fields, run in order, all or nothing."}
},"additionalProperties":false}
```

I measured this at 1,243 B compact. `ExampleOperation` stays `{"title":"Plan the next change"}`.

Argument handling:
- **Loose `ops` items.** The `ops` items are loosely typed because `executeNativeTask` already validates by hand (`taskNoExtra`, parsers, task.go:451-564). Each op goes through the existing `normalizeTaskArguments` and the same per-action validation.
- **Ops conflicts.** `ops` combined with any other top-level field is `invalid_batch`.
- **Id normalization.** Ids normalize `1`, `T-1` and `t1` to `T-001`. Normalization stays idempotent, because `%03d` of a `next_id` always round-trips.
- **Batch size.** The new setting `max_batch_ops` (default 20, hard max 200) bounds the batch.

**Output.** The envelope is unchanged: header JSON, `--- INFORMATION ---` lines, and an optional CONTENT block (tools/output.go:47-109).

Single flat call outputs:
- `add`, `get`, `update`, `done` and `delete` are byte-identical to today, so the add golden survives.
- `list` keeps the summary `N task(s) listed` and the line format `id | status | priority | title[ | tags: a,b][ | documents: N]`.

`list` information lines:
```
action: list
count: 100
total: 137
has_more: true
next_offset: 100
```
- `total` is the number of matches before the window. `has_more` and `next_offset` appear only when more remain.
- `offset` beyond `total` gives `0 tasks listed`, with `total` shown and no `has_more`.
- `detail:true` turns the documents segment into ` | documents: a.md,b.md` and appends ` | updated: 2026-05-01T10:00:00.000Z`.
- The byte budget `max_list_bytes` still truncates, and then `next_offset` is `offset + count`.

Batch success (`ops` present):
```
{"id":"call-1","status":"ok","summary":"4 ops applied"}

--- INFORMATION ---
action: batch
count: 4

--- CONTENT token=… encoding=utf8 ---
1 add T-004 | todo | normal
2 update T-001 | in-progress | high
3 done T-002
4 delete T-003
--- END CONTENT token=… ---
```
- The summary is `N ops applied` if any op mutates, otherwise `N ops run`.
- Mutation lines omit the title and tags because the model just sent them. This also keeps their size bounded.
- A `get` op prints the full list-style line: `5 get T-001 | todo | normal | Title | tags: x`.
- A `list` op prints `6 list 2 of 7, next_offset 2`, then its task lines indented by two spaces. List sections share the `max_list_bytes` budget. Mutation lines are never cut.

Batch failure (nothing written):
```
{"id":"call-1","status":"not_found","summary":"Op 3 failed: nothing was applied"}

--- INFORMATION ---
error_type: execution
error_code: batch_failed
failed_op: 3
op_error_code: task_not_found
op_error: No task has that id
task_id: T-009
open_ids: T-004, T-007 (+9 more)
applied: 0
```
- The status is the failing op's status: `invalid_input`, `not_found` or `error`.
- Pre-flight errors, with the same envelope shape and no `failed_op`:
  - `invalid_batch`: "ops must be a list of 1 to N task objects and cannot be combined with other fields".
  - `batch_too_large`: "A batch holds at most N ops".

Single `task_not_found`:
- Same `error_code`, `task_id` and summary as today.
- `available_ids` is renamed `open_ids`: non-done ids in list order, capped at `list_limit`, with `(+N more)` and no done tasks.
- It comes from the same locked read (a `Miss{OpenIDs, Total}` filled where the lookup failed), so there is no second load.

**Store changes.**
- **Apply.** Add `TaskStore.Apply(ops, policy)`. It takes the lock, loads once, runs each op against the in-memory document (reusing the `Create`, `Update` and `Delete` closure bodies), and writes once. An op error aborts before `write`. `Create`, `Update` and `Delete` become thin wrappers over it.
- **No-op writes.** `mutate` takes a "no change" sentinel, so a miss or a `done` on an already-done task writes nothing and leaves `updated_at` alone.
- **List.** `TaskListFilter` gains `Offset` and `Query`. `TaskListResult` gains `Total`. Filter and sort first, then clone only the returned page.
- **Query matching.** `Query` matches every lowercased word as a substring of the title, a tag or a document path, the same rule as `recordsContainsTerms` (records_search.go).
- **Lock.** `sessionlock.Acquire(ctx, PathFor(tasks.json))` wraps `mutate`, `Get` and `List`. The `sync.Mutex` stays in front of it.
- **Write.** Add `f.Sync()` before the rename. This is optional and cheap.

## 4. Compatibility and migration

| Item | Effect | Action |
|---|---|---|
| Existing `tasks.json` | Format unchanged. `schema_version` stays 1, and the codec still preserves unknown fields (task_store.go:44-151). | None. |
| `T-%03d` refs | Unchanged, plus lenient input forms. | None. |
| Sidecars | None today, none added. The lock file is hidden and sits in the already-excluded storage directory (checkpoint/service.go:268-282). | None. |
| Settings keys (`native_task_settings.go:66-188`) | Every key stays valid. `max_batch_ops` is added: default 20, max 200. | Update the key switch, `configuredInteger`, `sample/schemes/setting.schema.json:1685-1768` and the settings test (native_task_settings_test.go:1-65). No removed keys, so no old-key hint is needed. |
| Manifest `sample/tools/native/task.json` | `requireDefinition` requires an exact schema match (catalog.go:591-612). Deployed manifests would stop loading, with a hint to recopy. | Follow the `requireExecDefinition` pattern (catalog.go:~560): keep the old schema as `taskInputSchemaV1`, accept it, and show the current one. Then deployed manifests keep working. |
| Golden 1 (add) | Unchanged. | None. |
| Goldens 2 and 3 (schema errors) | They pin `#/allOf/...` ajv paths, so dropping `allOf` changes them. | Regenerate them. They come from the old Node backend, so this deliberately ends Node byte parity. See Q1. |
| `testdata/native-task-definition.json` | The TypeScript parity fixture (catalog_task_test.go:1-111) changes. | Regenerate it, under Q1. |
| `TestTaskStoreListPinsOrderAndWindow` (task_store_list_test.go:1-94) | Sort and window are unchanged, so it passes as is. | Add `offset`, `total` and `q` cases in a new test. |
| `TestTaskNotFoundListsTheTaskIDs` (valid_values_errors_test.go:87-114) | Pins the `available_ids` line. | Update it to `open_ids`, with a done task excluded. |
| Statecontext (task.go:566-703, app/state_context.go) | No change. | None. |
| Rename | Not recommended. If wanted later, follow the memory→records convention in `removedNativeTools` (sessionfs/tool_shape.go:~20): add the old name with a hint, so settings that still use it fail loudly. | None now. |
| Docs | | Update `design_task.md`, add a DECISIONS entry, and mark the task redesign done in the "Not done" list (DECISIONS.md:1978). |

## 5. Risks and open questions

**Verified in code:**
- Findings 2-10 and the store, settings, manifest and golden facts in sections 1 and 4, from entries or my own reads.
- Miss paths in `Update` and `Delete` reach `write` (task_store.go:303-364).
- `ReadOnly` has side effects on sub-agents and plan mode (subagent.go:249-250, plan_mode.go:29-30).
- Benchmarks as quoted in finding 6.

**Assumed, not verified:**
- Default tool scheduling is not `"ordered"`, so same-response races are real.
- Two backends can share one storage directory.
- A hidden lock file in the task directory has no side effects on other code paths.
- The schema sketch compiles under `tools.CompileSchema`.
- Whether a sub-agent's own task tool is built through `childToolPools` or through toolset resources. Entry #53 says the latter; I did not trace it.
- Token savings are estimated from bytes, not measured with a tokenizer.

**Needs your decision:**
- **Q1. Drop `allOf` and break Node golden parity?** I recommend yes. It saves 627 B on providers that receive it, `Execute` already enforces the rules with better errors, and Anthropic strips them anyway. The cost is regenerating goldens 2 and 3 and the TypeScript definition fixture. On Anthropic the definition grows by about 120 B because of `ops`, `q`, `offset` and `detail`.
- **Q2. Add owner scoping or an `owner` stamp?** I recommend no. Shared tasks are how a parent sees a sub-agent's progress, and statecontext relies on seeing everything. If you want one, stamp `owner` from `SessionKey` on add, informational only, and add a list filter later.
- **Q3. Batch failure semantics.** I recommend all or nothing, because a corrected retry is safe and duplicate adds are not. The alternative is partial apply with per-op error lines.
- **Q4. Whether to add the cross-process lock now.** I recommend yes. It is cheap, but it matters only if two backends really share a directory.
- **Q5. Accept the old manifest schema through `taskInputSchemaV1`.** I recommend yes. The cost is about 1.7 KB of source and it avoids breaking deployed sessions.
- **Q6. Pagination style.** I recommend a stateless `offset`. At 1000 tasks, drift between pages is tolerable, and a cursor adds complexity for no clear gain.

**Rejected:**
- `$n` back-references to ids created earlier in the same batch. They are over-engineered, and the model can chain across calls.
- `ReadOnly` on the task tool (see finding 9).
- Making `list` a separate tool.

**Residual risks:**
- A batch of 20 mutations is one all-or-nothing write, so one bad op rejects the rest. That is the intended behavior, but the model has to resend.
- `total` adds a few bytes to every list.
- The old-schema acceptance path is permanent code.
