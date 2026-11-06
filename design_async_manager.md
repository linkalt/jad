# Native Async Manager Design

**Status:** Decided; ready for package-by-package approval (design only, nothing implemented)

**Scope:** One manager for the async native tools (`process`, `subagent`, later `tool_smith` and `profile_message`) under `backend-go/`

Companions: `design_process.md` (note: written for the removed TypeScript backend; the Go behaviour below is what applies), `design_tool_smith.md`, `backend-go/DECISIONS.md` (`subagent`, process entries).

Facts cite `backend-go/` paths. Items marked *(decided)* were fixed by the user; *(proposal)* is mine and adjustable through config. Section 18 lists open questions (none blocking); section 20 is the implementation plan.

## 1. Goals and non-goals

Goals:

- One place that owns every pending async handle and answers "wake me when any of them has something" (first signal wins, the rest keep running).
- A handle survives the agent run that created it. Ending a turn never kills work; only an explicit stop or the server-shutdown policy does.
- Results are files: readable again after completion, completion signals queued until consumed.
- One start model for all async-capable tools: block up to a timeout, then return a handle.
- Keep the signal vocabulary open without building kinds nobody needs yet.

Non-goals:

- No durable OS supervisor. A process or child does not survive a server restart (same boundary as `design_process.md` section 2).
- No push into the model. Nothing wakes the model by itself; a signal is delivered only to a `wait` call (or shown to the user, section 12).
- No cross-session or cross-user sharing of handles.
- No change to `bash` (blocking, group cleaned up when it returns). `exec` was unchanged in phases 1 and 2; since 2026-10-25 it is a start tool like `tool_smith` (see DECISIONS.md, "`exec` is an async tool"): `block_ms`, kind `exec`, ids `exec_<8 hex>`, and it needs no `tools.native.async`.

## 2. Terminology

- **Handle**: one unit of async work with an opaque id, a kind, an owner and a state. Ids keep today's shapes: `proc_…` (process), `sub_xxxxxxxx` (subagent, `subagent_manager.go:664`); new kinds get their own prefix.
- **Kind**: `process`, `subagent`, `tool_smith`, `profile_message`. A kind is a driver behind the manager (section 5).
- **Signal**: `{handle, kind, payload}`. The only signal kind shipped is `completed`, which fires on every terminal state (the state is in the entry).
- **Consumed**: a completion signal that a `wait` call has returned once. Consumption never deletes the result.
- **Owner**: the session key (reference + profile, `resolver.go:126`) that started the handle.

## 3. Current behaviour (facts)

Process (`internal/tools/native`):

- `process` always starts asynchronously: `process.go:174-187` returns `state: running` at once. `wait` is a separate action with `timeout_ms` clamped to `max_wait_timeout_ms` (`process.go:207-227`, defaults 10 s / 120 s, `sessionfs/native_process_settings.go:14-15`).
- `ManagedProcessManager` (`process_manager.go:114`) is shared per storage dir (`app/resolver.go:526-544`). It holds live entries with a `done` channel; `Wait` selects on it (`process_manager.go:409-431`).
- Persistence: `<storage_dir>/<id>/manifest.json` + `stdout.log`/`stderr.log`, plus `owner.json` (`process_orphan.go:25-67`). `Recover` (`process_manager.go:813`) turns `running` manifests into `lost`, stops verified orphan groups when `stop_orphans` (default true), leaves a live other server's processes alone.
- Capacity: `max_running` (default 8) per storage dir, plus `ProcessOwnerLimit` for sub-agents (`process_manager.go:211-217`). Retention: `max_completed` (100) / `retention_days` (`process_manager.go:1002`).
- Lifetime: a run ending does not stop processes; `Close` (session/server shutdown) stops all running (`process_manager.go:476-503`). `waitForProcess` records the real exit and signals leftover group members (`process_manager.go:521-562`, P-037 follow-up).
- Owner scoping exists only for sub-agent children (`process_owner.go:69-130`): the session's own processes have an empty owner and are not session-scoped beyond the shared storage dir.

Subagent:

- `subagent` blocks up to `wait_ms` (default 120 s, cap 600 s) and then returns a run id while the child keeps working (`subagent_call.go:236-275`). A writer-capable child ignores `wait_ms` unless `allow_detached_writers` (`subagent_call.go:104-113`) because its approvals belong to the parent call (`detachGate`, `subagent_call.go:126-210`).
- Follow-up: `subagent({id, wait_ms?, stop?})` (`subagent_call.go:212-233`). `stop` cancels; a call that still owns the run and is cancelled cancels the run too (`subagent_call.go:255-261`).
- `SubagentManager` (`subagent_manager.go:45`), shared per storage dir; slots `max_concurrent` (3) and queue `max_queued` (32); `keep_finished` (50). Manifests in `<storage_dir>/.runs/<id>.json` hold no answer; the answer is re-read from the child transcript (`subagent_store.go:21-51`). Recovery marks queued/running as `lost` unless another live server owns them (`subagent_manager.go:100-134`).
- A run's end stops the child's own processes (`subagent_manager.go:458`, owner = run id).
- Contradiction with decision 2: the HTTP layer cancels all of a session's runs on user stop and reset (`httpapi/handler.go:563`, `mutation_handlers.go:102`, `app/resolver.go:135`). `SubagentManager.Close` cancels all (`subagent_manager.go:652`).

tool_smith:

- Synchronous: `executeToolSmith` runs one child interpreter and returns the value (`tool_smith.go:78-79`). Timeout comes from the `bash`-style child settings (`Child NativeBashSettings`, `sessionfs/native_tool_smith_settings.go:62-64`; `shellChildRun.timeoutMs`, `bash.go:150-155`). Scripts call native tools through a Unix-socket bridge with an allow-list; `bash exec process subagent tool_smith task` can never be called from a script (`native_tool_smith_settings.go:41`). `profile_message` is not excluded.

profile_message:

- Synchronous: `tools/native/profile_message.go:136-191` bounds the whole call by `timeout_ms` and waits for the target run. The target run is an `activeRun` in the HTTP registry (`app/profile_message.go:100-135`, `httpapi/profile_run.go:42-50`); the caller's ctx ending stops it (`app/profile_message.go:120-125`). Busy target is refused (`ProfileBusyError`, line 116, optional `busy_wait_ms`). Depth is one (line 74). No persistence of the call itself: the answer is in the target profile's own transcript.

Schemas and prompts: `sample/tools/native/{process,subagent,tool_smith,profile_message}.json` hold description + input schema; `sample/schemes/setting.schema.json` documents settings (process ~1399, subagent ~1794, profile_message ~1874). No separate prompt files mention the wait parameters; the descriptions in the JSON are the prompt text.

## 4. Decisions fixed by the user

1. One manager over all async tools; `wait` returns when a signal exists; other handles keep running. Signal = `{handle, kind, payload}`; only `completed` ships (fired on every terminal state).
2. Handles always survive an agent run; only explicit stop, user stop/reset/delete, or server shutdown ends them.
3. Results persisted to files and re-readable after completion; undelivered signals are queued.
4. Unified start: every async-capable tool blocks up to `block_ms` and otherwise returns a handle. Phase 1 = manager + process + subagent; phase 2 = tool_smith + profile_message.
5. Concurrency caps are config, never tool parameters, and count live handles only.
6. The `wait` parameters of process/subagent are removed; clean break, no shim.
7. ONE tool `async` with `action` (wait/list/status/stop); `wait` returns all ready signals up to a cap, with `more`.
8. User stop and reset cancel handles ("user stop wins"). `stopped`/`lost`/`failed` are queued as signals.
9. **Q2** `block_ms` on every async tool; `timeout_ms` = work lifetime; the `async` wait action has `wait_ms`.
10. **Q6** Writer sub-agents stay blocking in phase 1: never a handle, never listed.
11. **Q4** A sub-agent child's own processes/handles are stopped when the child ends.
12. **Q5** A user reset drops that session's unread signals.
13. **Q7** A config that enables an async-capable tool without the `async` tool is rejected.
14. **Q8** No UI in phase 1 beyond counts in SSE.
15. **Q9** `profile_message` is NOT callable from `tool_smith` scripts for now.
16. **Q10** Deleting a session/profile stops its handles.
17. **Q11** Default numbers (section 11) accepted; each is a config key and adjustable.
18. **Q12** One `async` tool with per-call scheduling (section 6.2.1).
19. **Q13** New `.async/` record + bounded `index.json`.
20. **Q14** `ok` for a sub-agent = run status `done` (section 6.3).

## 5. Architecture

```text
tools: process | subagent | (tool_smith | profile_message)   <- start (blocks <= timeout)
tool:  async (wait | list | status | stop)                 <- manager surface
            |
       AsyncManager  (per storage root; owner-scoped views)
        - registry of handles (id -> record)
        - completion queue per owner
        - cap, retention, recovery
        - drivers by kind
            |
   driver: process | subagent | tool_smith | profile_message
```

A **driver** is a small interface: `Start(req) -> Handle`, `Stop(id, reason)`, `Result(id) -> ResultRef`, `Recover() -> []Record`, `Close(policy)`. The existing `ManagedProcessManager` and `SubagentManager` become drivers; their goroutine, slot, manifest and recovery code is kept, not rewritten. The new code is the registry, the signal queue, the single `async` tool, the cap, the index and the unified record.

*(proposal)* Keep one manager instance per resolver, with owner-scoped views, rather than per storage dir: the cap and the wait must span kinds, and today's per-storage-dir sharing (`resolver.go:526-544`, `sharedSubagentManager`) is by kind and would make a cross-kind wait impossible. Drivers keep their own storage dirs.

## 6. Tool surface

Two kinds of tools: the **start tools** (`process`, `subagent`, later `tool_smith`, `profile_message`) and ONE manager tool, `async` *(decided)*. Total new prompt cost is one tool definition.

### 6.1 Start semantics (all async-capable tools)

Current parameter names (checked): `process` has `timeout_ms` = max time `wait` blocks (`sample/tools/native/process.json:18`); `subagent` has `wait_ms` = max time to wait for the answer (`sample/tools/native/subagent.json:26`). The agent config key `timeout_ms` (child lifetime, `native_subagent_settings.go:57`, `subagent_manager.go:452`) is not a tool parameter.

*(decided, Q2)* One start-block parameter on every async tool: `block_ms` (integer >= 0, most time the call blocks; 0 returns a handle at once; clamped to `max_block_ms`; omitted = `default_block_ms`). `timeout_ms` keeps meaning "lifetime of the work" (the agent's setting; for `process` an optional kill-after limit is NOT added now). The `async` tool's wait action has its own `wait_ms`. `process.timeout_ms` and `subagent.wait_ms` are removed.

Behaviour:

- Finished within `block_ms`: today's normal result (process: exit + bounded logs; subagent: answer), plus `id` for re-reading.
- Not finished: `{status: ok, state: running, id, kind}` and one hint line. The work keeps running.
- The start call's context ending stops the *wait*, never the work (decision 2). Reverses `subagent_call.go:255-261`; no-op for process, which already ignores run ctx.
- *(decided, Q6; **superseded for opted-in agents by section 21**: with `tools.native.subagent.background_writers` on, a writer none of whose tools asks for approval is a handle)* **Writer sub-agents stay blocking in phase 1**: they never return a handle, never enter the async index, and `block_ms` is ignored for them, as `wait_ms` is today unless `allow_detached_writers` (`subagent_call.go:104-113`). Reason: their approvals go through the parent call's requester and the checkpoint is taken for that call (`childApproval`, `detachGate`, `subagent_call.go:126-210`); a handle that outlives the call has nobody to ask. Consequence: decision 2 does not apply to writers until a separate approval design exists. `allow_detached_writers` is left as it is (default false) and is not part of phase 1.

Process schema after migration:

```json
{"type":"object","properties":{
 "command":{"type":"string","minLength":1,"description":"Shell command. Already runs in the background: no &, nohup, disown."},
 "stdin":{"type":"string","description":"Text written once to stdin, then closed."},
 "cwd":{"type":"string","minLength":1,"description":"Defaults to the workspace."},
 "block_ms":{"type":"integer","minimum":0,"description":"Most time to block for the exit; if still running an id is returned."}
},"required":["command"],"additionalProperties":false}
```

Subagent: `task`, `agent`, `block_ms`; `id`, `wait_ms`, `stop` removed. Calling with no task still lists the agents (that is the agent catalog, unrelated to handles).

### 6.2 The `async` tool

One tool, `action` required except where inferred (`ids`/`wait_ms` only -> wait; `id` only -> status; nothing -> wait with defaults is NOT inferred, to avoid accidental blocking: nothing -> list). Compact schema, per-action fields:

```json
{"type":"object","properties":{
 "action":{"type":"string","enum":["wait","list","status","stop"]},
 "wait_ms":{"type":"integer","minimum":0,"description":"wait: most time to block; 0 polls."},
 "ids":{"type":"array","items":{"type":"string"},"maxItems":32,"description":"wait: only these handles. Default: all yours."},
 "id":{"type":"string","description":"status, stop: handle id."},
 "state":{"type":"string","enum":["active","done","failed","stopped","lost","all"],"description":"list: default active."},
 "kind":{"type":"string","description":"list: process, subagent, ..."},
 "limit":{"type":"integer","minimum":1,"description":"list: page size."},
 "cursor":{"type":"string","description":"list: from the previous page."}
},"additionalProperties":false}
```

The description (in every prompt) is one sentence per action, no handle examples. Validation: fields not belonging to the chosen action are a `invalid_input` error naming the field. `filter by owner` is not a parameter: the owner is always the caller (the server-side owner filter exists for the HTTP/UI endpoint only). `wait`, `list`, `status` are read-only; `stop` is not (per-call scheduling, section 6.2.1).

#### 6.2.1 Scheduling: one tool, read-only except `stop` *(decided, Q12 checked in code)*

Yes, it works with the existing hooks. `Catalog.ChangesState` returns `!tool.ReadOnly || CallMayChangeFiles(call)` (`tools/catalog.go:247-262`); `CallMayChangeFiles` normalizes the arguments and calls `Tool.MayChangeFiles` (`catalog.go:267-293`); the runner uses `ChangesState` to decide whether a response runs in order (`session/runner_tools.go:233`, `DECISIONS.md` "Tool call scheduling"). `subagent` and `tool_smith` already use the hook (`subagent.go:246`, `tool_smith.go:74`). So `async` is registered `ReadOnly: true` with `MayChangeFiles = (action == "stop")`: `wait`/`list`/`status` may run beside other read-only calls, a call containing `stop` is serialized.

Caveats found while checking (the design must handle them):

1. `MayChangeFiles` also feeds the checkpoint: `roundChangesWorkspace` takes a checkpoint when it is true (`runner_tools.go:176`). A `stop` would therefore trigger a workspace checkpoint when `checkpoint.enabled` (off by default) although it changes no files. Accepted: harmless and rare; cheaper than a new hook.
2. `ReadOnly: true` tools are handed to sub-agent children (`childToolPools`, `subagent.go:162-170`, which skips only `subagent` and `tool_smith` by id). *(Amended, user decision 2026-10-22: children MAY have `async`; the restriction is on scope, not on the tool.)* `async` is not skipped; a child gets it only when its agent `tools` list names it and the session enables `tools.native.async`. The child's `async` is bound to the child's own owner key (the run id, `ExecutionContext.ProcessOwner`), so wait, list, status and stop see only handles the child started, and the parent's ids answer `unknown_handle`. Only `subagent` and `tool_smith` stay excluded (no nesting).
3. The tool_smith bridge counts a nested call as a write when `!ReadOnly || MayChangeFiles` (`tool_smith_bridge.go:448`); `async` goes into `nativeToolSmithExcludedTools` from phase 1 (section 13).
4. `subagent` is itself `ReadOnly`, so a `subagent` start and an `async wait` in the same response may run in parallel and the wait may not see the new handle. Accepted; the tool description says to wait in a later call.

### 6.3 Result scheme (all actions)

Every handle appears as a **fixed-shape entry**. Order of fields is fixed; empty fields are omitted.

```json
{"id":"proc_…","kind":"process","state":"done","ok":true,
 "code":0,
 "summary":"…<= summary_max_bytes…","bytes":12000,
 "ref":"<state-dir-relative path to the full result>","t":"2026-…Z"}
```

- `state`: `running` | `queued` | `done` | `failed` | `stopped` | `timed_out` | `lost` (the unified set, section 7).
- `ok`: true only for `done` with a successful outcome (process exit code 0; subagent answered). The model can decide without reading `summary`.
- `code`: kind-specific small scalar (process exit code or signal name; omitted otherwise). New kinds put their small fixed fact here; anything larger lives in the file. This is the extensibility point for signal kinds.
- `summary`: process = tail of stderr if failed, else tail of stdout; subagent = head of the answer; cut on a UTF-8 boundary at `summary_max_bytes` (default 512), with `cut: true` when shortened.
- `bytes`: size of the full result file (process: stdout+stderr stored bytes; subagent: answer bytes). Tells the model whether reading the file is worth it.
- `ref`: path of the per-kind file that holds the full result: process = `<process state dir>/<id>/` (`stdout.log`, `stderr.log`, `manifest.json`); subagent = the transcript recorded in `.runs/<id>.json` (`subagent_store.go:40`, the answer is read from there). Read with the normal `read` tool (path-policy applies). No tool returns file contents inline except the capped `summary`.

**`ok` per kind *(decided, Q14)*.** process: `ok` = state `done` with exit code 0; a non-zero exit is `done`, `ok:false`, `code` set; `stopped`/`failed`/`lost` are `ok:false`. subagent: `ok` = run state `done` (`subagent_manager.go:461`); `timed_out`, `stopped`, `failed`, `lost` map to the same-named unified state with `ok:false`. A cut-off child still returns its partial work through `ref` (`partial` is not a state). A child that ends `done` by hitting `max_tool_rounds` is `ok:true` because the manager cannot tell; the answer text says so.

Per action:

- **wait** -> `{signals:[entry…], more?:true, pending:N}`. At least one signal unless the timeout or a cancellation ends the call (then `signals: []`, `waited:false`). Returns ALL signals ready at the moment of wake-up, oldest first, up to `wait_max_signals` (default 10); if more are ready, `more: true` and the rest stay queued for the next wait. First-wins still holds for blocking: the call returns as soon as the first signal exists; "all ready" only means those already queued then. No pending handles and nothing queued: immediate `signals: [], pending: 0`. Cancellation of the waiting call: ctx error; nothing consumed (cancellation is checked before the queue pop, both under the queue lock).
- **list** -> `{counts:{active:A, unread:U, total:T}, items:[entry-without-summary…], next?:"cursor"}`. Entries here are summaries only: `id, kind, state, ok, t` and an 80-byte `label` (agent name / command-free task preview). No `summary`, no output. Section 9.
- **status** -> one entry with `summary` up to `status_summary_max_bytes` (default 4096, head and tail like process status today, `process.go:198-200`). Does not consume the signal.
- **stop** -> terminal entry (idempotent: an ended handle returns its entry; `lost` returns `blocked` as today, `process.go:228-230`).

**Per-response byte budget.** `async_response_max_bytes` (default 8192). Entries are emitted whole; the encoder stops adding entries when the next one would exceed the budget, sets `more: true` (wait) or `next` (list), and leaves the rest queued/unlisted. If a single entry alone is larger than the budget its `summary` is shortened first (never the id/state fields). The budget also bounds `wait_max_signals` x `summary_max_bytes` (10 x 512 = 5 KB < 8 KB at defaults; the validator rejects defaults that cannot fit one full page).

## 7. Handle state machine

```text
queued -> running -> done | failed | stopped | timed_out
   `-> stopped
running -> lost     (only via restart recovery)
```

Mapping to existing states: process `running, exited->done (ok = exit 0), stopped, failed, lost`; subagent `queued, running, done, failed, timed_out, stopped, lost` (`subagent_manager.go:23-33`). Kind-specific names stay in the driver manifests (goldens and state-context stay readable); the unified set is what the `async` tool shows. A non-zero process exit is `done` with `ok:false` and `code`. Terminal states are final; exactly one terminal transition per handle under the handle lock, and the signal is enqueued after the result file is durable. *(decided Q5)* Every terminal state, including `stopped`, `lost` and `failed`, produces a queued signal.

## 8. Ownership, scoping and cancellation

- Owner = session key (reference + profile). The `async` tool acts only on the caller's own handles; other ids look like `unknown_handle` (as `subagent_manager.go:558`, `process_owner.go:69`).
- Top-level processes have no owner today (`process_owner.go:14-24`): phase 1 must stamp the session key or `wait` would deliver another session's completions on a shared storage dir.
- Sub-agent children: a child's own process handles are owned by its run id and are stopped when the run ends (`subagent_manager.go:458`); they are not visible to the parent *(decided, Q4)*. *(Amended 2026-10-22:)* a child that has the `async` tool registers its process handles under its run id (owner = run id) and waits on, lists and stops them with its own `async`; when the run ends the run's handles are stopped and their records removed (`DeleteOwnersWithPrefix(runID)`), so nothing of the child reaches the parent's async list, SSE counts or prompt notice. A child without `async` behaves as before (its processes are not handles).
- profile_message (phase 2): owner = calling profile's session key; only the caller may wait/stop; the target profile cannot see the handle. Depth-one rule stays.

### 8.1 What cancels what *(decided: user stop wins)*

| Event | process | subagent (async) | tool_smith / profile_message (ph. 2) |
|---|---|---|---|
| Agent run ends (normally, error, run cancel by model) | survives | survives | survives |
| Start call's context cancelled (run stopped while blocking) | survives | survives (changed from today) | survives |
| User **stop** of the session (`handleRunStop`, `handler.go:553-565`) | stopped (NEW: today it does not reach processes) | cancelled (as today, `CancelSessionChildren`) | cancelled |
| User **reset** (`mutation_handlers.go:84-110`) | stopped (NEW); the session's unread signals dropped *(Q5)* | cancelled (as today); unread dropped | cancelled; unread dropped |
| `async` stop on one id | that handle | that handle | that handle |
| Compaction | survives | survives | survives |
| A sub-agent run ends | its child's own processes stopped (as today, `subagent_manager.go:458`) | - | - |
| Session delete (`catalog_sessions_handler.go:102-133`) | stopped (NEW wiring, 8.2) | cancelled (as today) | cancelled |
| Profile definition delete | refused while in use (8.2); not a handle event | - | - |
| Server shutdown (`resolver.go:825-875`) | stopped, reason `server_shutdown` | cancelled | cancelled |
| Hard crash / restart | `lost` | `lost` | `lost` |

The process row is a behaviour change: the current stop/reset code path only calls `CancelSessionChildren` (sub-agents). The set of stopped handles is "this session key's live handles", so another session on the same storage dir is untouched. Stop/reset signals are queued like any other terminal state, so the next `wait` explains why a handle ended (`stopped`, reason `user_stop`); reset also clears that session's undelivered signals because the history that gave them meaning is gone.

### 8.2 How stop, reset and delete reach the managers (checked in code)

**(b) User stop and reset.** `handleRunStop` (`httpapi/handler.go:553-565`) and reset (`mutation_handlers.go:84-110`) call `h.cancelSessionChildren`, which uses the optional `SessionChildren` interface (`handler_ports.go:62-67`, documented "must not block"). Its only implementation, `Resolver.CancelSessionChildren` (`app/resolver.go:135-147`), walks `r.subagentManagers` only and never touches `r.processManagers` (`resolver.go:526-544`). `ManagedProcessManager.stopOwned(owner, reason)` exists (`process_owner.go:39`) but is unexported, ignores an empty owner, and top-level processes have an empty owner (`process_owner.go:14-24`; only `ownedProcessManager.Start` sets `Owner`, line 92). Required new wiring:

1. Stamp `Owner = ExecutionContext.SessionKey` on every top-level process start (`process.go:174`), kept separate from `ProcessOwner` (child scoping).
2. The async manager exposes `CancelOwner(owner, reason)` and `CancelOwnersWithPrefix(prefix, reason)`, fanning out to all drivers; `Resolver.CancelSessionChildren` and `CancelAllSessionChildren` call it (replacing the direct sub-agent loop once subagent is a driver).
3. `Stop` blocks up to two `kill_grace_ms` (`process_manager.go:433-474`), which breaks the "must not block" contract: the manager sends TERM immediately and the driver finishes the KILL step in its own goroutine.
4. Reset additionally calls `DropUnread(owner)` (Q5).

**(a) Session delete.** `deleteCatalogSession` → `stopSessionRuns` → `CancelAllSessionChildren(reference)` (sub-agents only, `session_catalog_files.go:169-182`) → `DeleteSessionConfig` → `releaseSessionChildren(settingPath)` (`session_catalog_files.go:100-`). The last step closes a `ManagedProcessManager` only when no other session shares its storage (`processSessions` refcount, same file), so on a shared storage dir the deleted session's processes keep running. Change: `CancelAllSessionChildren` also calls the async manager with prefix `sessionKey(reference,"")` before `DeleteSessionConfig`; it stays inside `stopSessionRuns` so the existing wait/409 logic for runs still applies. Records and result files follow the existing `deleteState` choice (kept unless `?state=delete`).

**Profile delete.** A profile is a catalog file; `DeleteCatalogFile` (`app/catalog_files.go:88-95`) releases nothing, and `mapCatalogFileError` maps `CatalogInUse` to 409 (`catalog_files.go:119-121`), so a referenced profile is refused. I did not verify which references count as "in use"; the working assumption is that a profile with live session state cannot be deleted, so no handle wiring is needed. WP4 includes a test that deleting an in-use profile definition is refused.

**(c) HTTP endpoint.** No endpoint for handles exists and none is in phase 1 *(decided, Q8)*. The paginated `GET …/async` shape in section 12 is an optional later extension. Phase 1 UI is only the counts in an SSE event.

## 9. Persistence, results, retention and growth control

### 9.1 What grows, and the bound on each

| Thing | Bound |
|---|---|
| Live handles | `max_handles` per owner (16), `max_handles_total` (64): count only `queued+running` |
| Terminal records on disk | `async.keep_finished` count (default 200) and `async.retention_days` (default 14, 0 = no age pruning), whichever drops more |
| Per-kind result files | pruned together with the record (process dir via the existing pruner, `process_manager.go:1002`; subagent manifest + optional transcript via `delete_transcripts`, `subagent_manager.go:140-180`) |
| Undelivered (unread) signals | at most `unread_max` per owner (default 100); beyond it the oldest unread signal is marked read-by-overflow (record keeps `overflow:true`, result stays readable until pruned) and a counter `dropped_unread` is reported in the next wait/list header |
| Unread age | `unread_ttl_days` (default 7): older unread signals expire the same way; the result stays until the record's own retention |
| In-memory index | live handles in full + terminal records as compact index rows (id, kind, owner, state, ended, flags; < 200 bytes each); at most `keep_finished` terminal rows. 200 rows ~ tens of KB. Detail (summary, paths) is read from the record file on demand |
| One response | `async_response_max_bytes` (8 KB); `limit` hard max 50 (default 10) |
| Record file | one small JSON per handle (< 2 KB: no command, no env, no answer, label cut to 80 bytes) |
| Prompt text embedding the list (state-context) | pending/unread only, at most `statecontext.limit` entries (existing process default 20, `native_process_settings.go:21`), counts for the rest |
| SSE / UI payloads | counts + at most N active ids (section 12), never summaries |
| Logs | one line per transition, no result text |

Retention happens per owner and globally; running handles and unread signals younger than the TTL are never pruned by the count rule (a pending record that would be pruned first has its signal read-by-overflow, not silently dropped).

### 9.2 Layout

```text
<tool_storage>/.async/
  index.json            # compact rows for live + recent terminal handles, rewritten atomically, bounded by keep_finished + live
  <id>.json             # unified record (small), referenced by the index
```

Per-kind data stays where it is (process dir, subagent `.runs` and transcripts). The unified record holds: id, kind, owner, state, ok, code, times, 80-byte label, `ref`, `bytes`, `unread`, `overflow`, server identity. `index.json` is an optimization, not the source of truth: it is rebuildable. *(decided, Q13)*

### 9.3 Startup scan cost

Today a start scans every process dir and every `.runs/*.json` (`process_manager.go:813-884`, `subagent_store.go:132-154`), which already grows with history and is bounded only by pruning. Design: at startup read `index.json` only and the records of rows that are not terminal (to run recovery/orphan handling). Terminal history is not opened. If the index is missing or fails its checksum, rebuild once by scanning `.async/*.json` (not the per-kind trees), logging the count; this is the only full scan. List never touches disk for terminal rows except to read the one record behind a page's `items` when a field is not in the index row. `list` is O(page) after an O(N log N) sort of at most `keep_finished + live` index rows. Not measured; I did not benchmark current scan time (unverified).

### 9.4 List semantics *(user concern: the list will grow)*

- `state` default `active` = `queued`, `running` and **unread terminal** (undelivered) handles. Read terminal handles are hidden unless `state` is `done`, `failed`, `stopped`, `lost`, `timed_out` or `all`.
- Newest first (by last state change, then id). Pagination: `limit` (default 10, hard max 50, clamped silently), opaque `cursor` (encodes the last (time,id); stable under inserts; an expired/pruned position just continues from the next older row).
- Filters: `kind`, `state`. Owner is implicit (caller); the HTTP/UI endpoint may filter by owner within the caller's authorization.
- Header `counts:{active,unread,total}` is cheap (maintained counters), so the model sees the size without paging.
- Entries are summaries only; use `status` for one handle.

### 9.5 Consumed vs readable

A signal is "unread" until a `wait` returns it (or `stop`/`status` on that id does *not* consume; only `wait` and `stop` do: `stop` returns the terminal entry itself). Reading a result via `status`, `list` or the file works before and after consumption until the record is pruned. The queue is derived from records with `unread: true`, so it survives restart without a second file; `wait` pops oldest first.

### 9.6 Restart recovery per kind

| Kind | After restart | Result |
|---|---|---|
| process | `running` -> `lost`, orphan group stopped when `stop_orphans` and identity verified (`process_manager.go:813-884`, `process_orphan.go`) | logs kept; `lost` signal queued |
| subagent | queued/running -> `lost` unless another live server owns it (`subagent_manager.go:100-134`) | transcript partial work kept |
| tool_smith (ph. 2) | child interpreter died -> `lost` | none unless it wrote its result file |
| profile_message (ph. 2) | target run is an in-memory `activeRun` -> `lost` | partial reply only in the target's transcript |

No PID is signalled after restart (PID reuse rule, `design_process.md` section 2).

### 9.7 Shutdown policy

No supervisor, so shutdown ends all handles (order and grace as `resolver.go:825-875`: sub-agents first, then processes). They become `stopped` with reason `server_shutdown` and their signals are queued for the next start. The wait is the longer of the stop wait and the longest stop of a live exec or tool_smith child (kill grace, kill deadline and drain), so a child killed at the end of its grace still ends `stopped`. A handle whose driver does not report within it is ended by the manager's close as `lost` with reason `server_shutdown`, so no record is left `running`; a later report of its driver is ignored. A result already being written is waited for, and every change of memory and files (a reset's read marks, a delivery, a delete) is on disk before the close writes its final index and shuts file writes. Only this server's handles are stopped and waited for: a handle another live server owns in the same folder never reports here, so it is left running and does not hold the close. When that other server stops, the next `list`, `status`, `stop`, `wait` or start in this server reads each of its handles' records again: an end that server recorded (a result, its shutdown stop, a delete) is taken as it is, a record still live ends as `lost` with reason `backend_restarted`, as a restart would, and a record this server cannot read (a newer build's version) drops the handle and is left as it is, as recovery at start does. While that other server runs, a `stop` of its handle is refused at once with `handle_foreign`: no driver here knows the run, so a stop could only wait out `stop_wait_ms`. `server.shutdown_timeout_ms` does not cap this wait (it bounds only the HTTP shutdown after it): exiting earlier could leave a shell child's process group running until the next start sweeps it. A second interrupt or SIGTERM during the shutdown ends the process at once, and a host or sub-agent manager whose build ends after the resolver closed is closed before `Resolver.Close` returns. The resolver closes its async hosts (one per tool storage folder) together, so a shutdown waits for the slowest host, not for the sum. A hard crash yields `lost` with reason `backend_restarted`, at the next start.

## 10. Concurrency, locking and goroutines

- One mutex in the manager guards registry map, counters, cap and queue. Per-handle state keeps the driver's lock. Order: manager -> handle; drivers never call the manager while holding their own lock (they signal a completion channel).
- Completion: driver writes the result, calls `manager.complete(id)`: persist record (outside the manager mutex, as `SubagentManager.finish`, `subagent_manager.go:540-553`), then under the mutex mark unread, update counters/index, wake waiters.
- Waiting: `wait` registers a waiter under the lock, then selects on {waiter, timer, ctx.Done()}. On wake it pops up to `wait_max_signals`/byte budget under the lock. Concurrent waits by one owner are allowed; each signal is delivered to exactly one. Waiters are removed on every exit path.
- Goroutines: drivers own their workers (WaitGroup as `subagent_manager.go:48`); the manager has none except an optional periodic pruner (one ticker, stopped by Close). `Close` stops drivers, waits, wakes waiters with "shutting down".
- Index rewrites are debounced and atomic (temp+rename); a crash between record and index leaves a rebuildable state.
- Goroutine-leak tests: extend the existing process lifecycle test; one per driver (start, complete, wait, stop, close).

## 11. Limits and defaults

*(decided, Q11: defaults accepted)* All in config, none a tool parameter. New `tools.native.async` object in `setting.json` (validators in `internal/sessionfs`, schema in `sample/schemes/setting.schema.json`, a `DECISIONS.md` entry, config tests, per the convention of `native_process_settings.go`).

| key | default | meaning |
|---|---|---|
| `max_handles` | 16 | live (queued+running) handles per owner |
| `max_handles_total` | 64 | live handles per server |
| `default_block_ms` | 10 000 | start tools, `block_ms` omitted |
| `max_block_ms` | 120 000 | clamp for `block_ms` |
| `default_wait_ms` / `max_wait_ms` | 10 000 / 120 000 | `async` wait |
| `wait_max_signals` | 10 | most signals per wait |
| `list_default_limit` / `list_max_limit` | 10 / 50 | list page |
| `summary_max_bytes` / `status_summary_max_bytes` | 512 / 4096 | per-entry text caps |
| `response_max_bytes` | 8192 | hard budget per `async` response |
| `keep_finished` / `retention_days` | 200 / 14 | terminal record retention |
| `unread_max` / `unread_ttl_days` | 100 / 7 | undelivered signal bounds |

The cap counts **live handles only**; terminal and unread handles never block a start. At the cap a start is refused with `async_capacity_reached` (error, names the limit and live count, suggests `async` wait/stop); nothing is queued by the outer cap. The subagent `queued` state is its own inner queue and counts as live. The inner caps stay (process `max_running` 8; subagent `max_concurrent` 3, `max_queued` 32; `process_max_running` per child) and are checked after the outer one. Removed: process `wait_timeout_ms`/`max_wait_timeout_ms`, subagent `wait_ms`/`max_wait_ms`. The validator rejects incompatible bounds (e.g. `max_wait_ms < default_wait_ms`, a response budget smaller than one full entry, `unread_max < wait_max_signals`).

## 12. Compaction, transcript, HTTP/SSE and frontend

Compaction:

- The transcript holds start/wait results: id, small entries, `ref` paths. Never output beyond `summary_max_bytes`.
- State-context: replace the process and subagent providers (`process_statecontext.go`, `subagent_statecontext.go`) with one `async` provider listing the caller's **live and unread** handles only (id, kind, state, label), capped like today's `limit`, plus counts for the rest (summary mode exists, `process_statecontext.go:20-24`). Terminal read handles are never embedded in prompts.
- Unread signals stay queued across compaction; the model collects them with `async wait`.

HTTP/SSE and frontend:

- Transcript reader: `async` is an ordinary tool call (`frontend_new/src/transcript-facts.ts:13` only needs `tool_call` parts); no special reader code.
- *(decided, Q8)* Phase 1 UI is one `async_state` SSE event with counts only (`active`, `unread`), sent on change and coalesced, plus the same two numbers in the session snapshot. Events are built with `makeSSEEvent` and registered in the run registry (`httpapi/provider_retry_state.go:53`, `registry.go:549`); a new event name needs the same plumbing. Whether `frontend_new` ignores unknown event names was not verified. No ids, no summaries. A paginated `GET …/async?state=&kind=&limit=&cursor=` endpoint is optional and out of phase 1.
- Frontend touchpoints: none handle `subagent`/`process` by name; `composer-triggers.ts:51` lists `design_process.md` among built-in mentionable docs, so add `design_async_manager.md` and update its test.

## 13. Security and permissions

- Tool allow-lists: the `async` tool is enabled via `sample/tools/native/*.json` and the profile `tools.allow/deny` like any native tool. A profile that denies the start tools but allows `async` has nothing to wait on. A profile that allows start tools but denies `async` cannot collect or stop: the validator rejects a config that enables `process` or `subagent` (and in phase 2 `tool_smith`/`profile_message` async mode) without `async` *(decided, Q7)*.
- Approval: `approval: ask` stays on the start tools (`process`, writer-capable `subagent`); `async` stop is not asked (it only ends work the caller started). Writer sub-agents never become handles in phase 1 (Q6), so no handle needs an approval after its call returned; `detachedApprovalReason` (`subagent_call.go:185`) stays for the existing `allow_detached_writers` path only.
- Nested tool_smith: scripts keep the exclusion list. `async` is added to `nativeToolSmithExcludedTools` (`native_tool_smith_settings.go:41`) in phase 1, since the tool exists from phase 1 and a script could otherwise wait on or stop the model's handles. `profile_message` is added to the same list *(decided, Q9)*; this is a behaviour change because it is callable from scripts today (it is not in the list), so scripts and tests that use `tools.profile_message` must be found first.
- Process env/path policy, stdin limits, API-key stripping: unchanged. No handle id is guessable (random IDs as now).
- Secrets: the unified record contains no command or env.

## 14. Observability

- Structured log lines (slog, existing logger) on start, complete, stop, lost, cap refusal, with id, kind, owner, duration, state; no command text.
- Counters exposed in the same place as other manager stats if any exist (none found for process/subagent; keep to logs in phase 1).
- Each result carries `id` and `kind`; state-context carries pending count.
- `/api` diagnostics endpoint is not proposed.

## 15. Testing strategy

- Manager unit tests (fake driver): first-wins with two handles, already-queued pop, batch of simultaneous completions, no pending, timeout, ctx cancel (no consumption), multiple waiters delivering once, owner isolation, cap refusal, close wakes waiters, consumed vs readable, record-based queue after reopen.
- Driver tests keep their suites (`process_*_test.go`, `subagent_*_test.go`) with the removed `wait` parameters rewritten (about 14 test files mention `wait_ms`/`wait`: the list in section 16).
- Start-model tests: finishes within the timeout (no handle needed), exceeds it (handle), `timeout_ms: 0`, ctx cancel of the start call leaves the work running (new for subagent).
- Survives-run tests: end a runner run with a running handle; handle still running; next run collects it (httpapi level with the fake provider).
- Restart tests: process and subagent `lost` recovery (exist), plus the unified record and unconsumed-signal queue after reopen.
- Shutdown: no leftover children (descendant pid check as in the P-037 tests), goroutine baseline.
- Config/schema parity: native definition manifest vs in-code schema (`process_golden_test.go` style), config validator tests for the new keys.
- `-race` on all touched packages.

## 16. Touchpoint inventory

Everything the migration touches, by area. Section 20 assigns each item to a work package.

- **Go tools** (`internal/tools/native`): new `async_*.go`; `process.go`, `process_manager.go`, `process_owner.go`, `process_statecontext.go`; `subagent.go`, `subagent_call.go`, `subagent_manager.go`, `subagent_statecontext.go`; `sessionfs/native_tool_smith_settings.go` (exclusion list).
- **Go app/httpapi**: `app/resolver.go`, `app/session_catalog_files.go`, `httpapi/{handler.go,mutation_handlers.go,handler_ports.go,catalog_sessions_handler.go}`, an SSE state file like `provider_retry_state.go`, `registry.go`.
- **Config** (`internal/sessionfs`): new `native_async_settings.go`; remove process `wait_timeout_ms`/`max_wait_timeout_ms` and subagent `wait_ms`/`max_wait_ms`; `roots.go` references; validator for Q7.
- **Schemas/samples/docs**: `sample/tools/native/{process,subagent}.json`, new `async.json`, `sample/schemes/setting.schema.json`, `sample/jad/tools/coding/full.json`, `sample/jad/state/default/coder/README.md`, `backend-go/DECISIONS.md`, `design_process.md`/`design_tool_smith.md` notes, `CLAUDE.md` (master guidance that uses `subagent` with ids), `backend-go/README.md` if it lists tools.
- **Frontend** (`frontend_new`): `composer-triggers.ts:51` and its test (add this design doc); SSE handling of `async_state` (counts only; verify unknown-event tolerance).
- **Tests**: 14 files reference `wait_ms`/`wait_timeout_ms` (`app/{subagent_integration,subagent_write_integration,native_process_integration,http_server}_test.go`, `tools/native/{subagent_manager,subagent_partial,subagent_persist,subagent_write,server_identity_lifecycle,statecontext_summary,valid_values_errors}_test.go`, `sessionfs/native_{process,subagent}_settings_test.go`), `process_golden_test.go`; `messages.jsonl` golden impact (`messages-golden-diff`) because tool schemas change.

## 17. Risks

- Hidden spend: handles outlive turns. User stop/reset is the kill switch and is extended to processes (section 8.1); it must be tested per kind.
- Writers remain blocking (Q6), so the "survive the run" promise is partial for them. Stated, not hidden.
- A model that never calls `async wait` leaves signals unread; bounded by `unread_max`/TTL, visible in state-context counts.
- Cap pressure: only live handles count, so finished work never blocks a start; a forgotten long-running handle does, and `async stop`/list make it findable.
- Clean break: prompts, skills or scripts calling `process({action:"wait"})`, `process({id})`, `subagent({id})` or `wait_ms` fail validation; a resumed model may repeat old calls. `CLAUDE.md` guidance for the master must be updated.
- Process owner stamping changes sharing semantics on shared storage dirs.
- New concurrency (waiter fan-out, index rewrites): single-lock design, race tests, rebuildable index.
- Index/record drift after a crash: index is rebuildable; record is the truth.

## 18. Open questions

All earlier questions are decided (section 4). Nothing blocks starting WP0. Q12 was checked in code and is decided (6.2.1); no open question remains.

Verification notes (each is resolved inside the named work package, not decisions):

- Does `frontend_new` ignore unknown SSE event names? (WP4; not verified.)
- Which references make a profile definition "in use" for deletion? (WP4; assumption in 8.2.)
- Existing tool_smith scripts/tests that call `tools.profile_message` must be found before it joins the exclusion list. (WP4.)
- Real startup-scan cost at large history was not measured. (WP1 adds a benchmark.)

## 19. Where the current code contradicts or complicates the decisions

- **User stop/reset reaches only sub-agents today** (`resolver.go:135-147`); processes need owner stamping, an exported stop, and a non-blocking contract (8.2).
- **Session delete on shared process storage** does not stop the deleted session's processes (`session_catalog_files.go:100-`, refcount); the async manager call fixes it (8.2).
- **Subagent call owns its run**: ctx end cancels the run (`subagent_call.go:255-261`); must change.
- **Writer sub-agents** run inside the call (`subagent_call.go:104-113`); kept blocking (Q6), so decision 2 does not cover them in phase 1.
- **profile_message**: the target run is stopped when the caller's ctx ends (`app/profile_message.go:120-125`); busy targets refused; depth-one uses a live caller run (line 74). Phase 2 problem.
- **tool_smith**: tied to the call ctx; no persisted result. Phase 2 problem.
- **Top-level processes have no owner** (`process_owner.go:14-24`).
- **Per-kind managers shared per storage dir** (`resolver.go:526-544`): a cross-kind wait/cap needs a per-resolver manager.
- **Startup scans read full history today** (`process_manager.go:813-884`, `subagent_store.go:132-154`); hence the index (9.3).
- **`ReadOnly` side effects** (6.2.1): children pool, checkpoint on `stop`, parallel start+wait.
- **`design_process.md`** is stale (TypeScript); refresh or supersede when phase 1 lands.

## 20. Implementation plan

Each work package (WP) is one reviewable change that can be approved and implemented on its own. Common gate for every WP: `gofmt -l` clean, `go vet ./internal/... ./cmd/...`, `go test -race` on touched packages, full `go test ./internal/... ./cmd/...` (never `./...`), `git diff --check`, `go.mod`/`go.sum` unchanged, and `messages-golden-diff` when a tool schema or tool-result text changes. Bug-like regressions get a failing-first test. One commit per WP with explicit paths. No hardcoded limits: every number is a config key with a default.

### Phase 1

**WP0. Config and schema** (risk: low)
- Files: `sessionfs/native_async_settings.go` (+ test), `sample/schemes/setting.schema.json`, loader/`roots.go` hook.
- Content: `tools.native.async.*` keys of section 11 with validators (bounds, `max_wait_ms >= default_wait_ms`, response budget fits one full entry, `unread_max >= wait_max_signals`). Old process/subagent wait keys stay for now.
- Tests: defaults, every key, null, bad bounds, schema/validator parity.
- Gate: common gate.

**WP1. Manager core, record/index, `async` tool** (risk: medium-high, concurrency)
- Files: new `internal/tools/native/async_manager.go`, `async_record.go`, `async_index.go`, `async_tool.go`, `sample/tools/native/async.json`; registry/catalog registration; tool_smith exclusion (`async` is allowed for children, scoped to the child's run; amended 2026-10-22).
- Content: driver interface, registry, live-only caps, unread queue with TTL/overflow, waiters, entry encoder with the byte budget, `.async/<id>.json` + `index.json` (atomic, rebuildable), list pagination/cursor/filters/counts, startup from index, retention pruner, `ReadOnly` + `MayChangeFiles` (stop only).
- Tests (fake driver): the manager list of section 15; list growth (10 000 terminal rows: page cost, memory bound, pruning, cursor stability); corrupt index → rebuild; many goroutines wait/complete under `-race`; goroutine baseline after Close; `ChangesState` per action.
- Gate: common gate plus a benchmark of startup from index vs rebuild. No driver is wired yet, so no user-visible change.

**WP2. Process migration** (risk: high, behaviour change)
- Files: `process.go`, `process_manager.go` (driver adapter, exported owner stop, non-blocking TERM), `process_owner.go`, `process_statecontext.go`, `sample/tools/native/process.json`, process tests incl. `process_golden_test.go`, `sessionfs/native_process_settings*.go` (remove wait keys), `app/native_process_integration_test.go`.
- Content: start takes `block_ms` and returns the result or a handle; remove `wait`/`status`/`list`/`stop` actions; stamp the session owner; map states and `ok`/`code`/`summary`/`ref`; keep P-037 group-kill, owner/orphan files and recovery; completion → manager.
- Tests: finishes within block, exceeds block (handle), `block_ms:0`, start-call cancel leaves the process running, survives run end, owner isolation on shared storage, descendant pid check, lost after restart, no leftover `sleep`, goroutine test, existing suite adapted.
- Gate: common gate; no stray processes (check by recorded pids); golden diff reviewed.

**WP3. Subagent migration** (risk: high)
- Files: `subagent.go`, `subagent_call.go`, `subagent_manager.go`, `subagent_statecontext.go`, `sample/tools/native/subagent.json`, `sessionfs/native_subagent_settings*.go` (remove `wait_ms`/`max_wait_ms`), the subagent test files listed in section 16, `app/subagent_integration_test.go`, `app/subagent_write_integration_test.go`.
- Content: `block_ms`; remove `id`/`wait_ms`/`stop`; run ctx cancel no longer cancels the run (reverses `subagent_call.go:255-261`); writer agents stay blocking and never enter the index; child's processes stopped at child end (kept); state and `ok` mapping (Q14); manager adapter over `SubagentManager`.
- Tests: slow child → handle → `async wait` → answer; stop via `async`; writer stays blocking; partial work through `ref`; restart → lost; owner isolation; `allow_detached_writers` path unchanged.
- Gate: common gate; golden diff.

**WP4. Lifecycle wiring** (risk: medium-high)
- Files: `app/resolver.go` (one async manager per resolver, close order), `app/session_catalog_files.go`, `httpapi/{handler.go,mutation_handlers.go,handler_ports.go,catalog_sessions_handler.go}`, SSE state file + registry, `sessionfs` Q7 validator, `native_tool_smith_settings.go` (add `profile_message`).
- Content: user stop/reset/delete cancel all kinds (8.1/8.2); reset drops unread; non-blocking cancel; shutdown order and `server_shutdown` reason; start-time recovery of unified records; `async_state` counts event + snapshot fields.
- Tests: stop/reset/delete matrix per kind (http level, fake provider); shared-storage delete; reset drops unread; in-use profile delete refused; SSE counts; Q7 rejection; `profile_message` not callable from scripts; shutdown leaves no child.
- Gate: common gate; `npm test` and `npm run typecheck` in `frontend_new` if the SSE client changes (never `npm run build`).

**WP5. Prompts, schemas, docs, frontend touchpoints** (risk: low)
- Files: final tool JSON text, `sample/jad/**`, `DECISIONS.md` final entry (decisions of section 4), `design_process.md`/`design_tool_smith.md` notes, this doc status → Implemented, `CLAUDE.md`, `composer-triggers.ts` + test.
- Tests: manifest/in-code schema parity, `messages-golden-diff`.
- Gate: common gate; frontend tests/typecheck if touched.

**WP6. Integration and soak** (risk: low, mandatory)
- Content: end-to-end scenarios: end a turn with a running process and subagent, next turn waits; two handles finish together (one response, `more`); 500 finished handles list/pagination; unread overflow/TTL; restart mid-run; shutdown. Confirm no old parameters remain (`grep wait_ms`).
- Gate: full suite with `-race`, `-count=3` on the manager, process and subagent packages.

### Phase 2

**WP7. tool_smith async** (risk: medium) *(BUILT 2026-10-22: see DECISIONS.md "`tool_smith` is an async tool"; the `list` object was removed from the schema in the same change)*
- Files: `tool_smith.go`, `tool_smith_fn.go`, new driver, `sample/tools/native/tool_smith.json`, settings, tests.
- Content: `block_ms`; driver over `runShellChild` (`bash.go:194`) detached from the call ctx; result file; lost on restart; stop kills the interpreter group; nested bridge keeps its exclusion list.
- Tests: long script → handle → wait; stop; nested call cap; no leftover interpreter; restart.
- Gate: common gate.

**WP8. profile_message async** (risk: high)
- Files: `profile_message.go`, `app/profile_message.go`, `httpapi/profile_run.go`, schema, tests.
- Content: `block_ms`; driver over `StartProfileRun` detached from the caller ctx (reverses `app/profile_message.go:120-125`); busy target still refused; depth-one rule reworked because the caller run may be gone (`RunningProfileRunCaller`, line 74); lost on restart (reply only in the target transcript).
- Tests: cycle/depth, busy, caller run ends before target, stop, restart, target compaction (`compact` option).
- Gate: common gate; profile tests in `app` and `httpapi`.


## 21. Writer sub-agents as handles when no approval can be asked *(ACCEPTED and BUILT 2026-10-22: Q-W1 to Q-W6 as recommended; see DECISIONS.md. The text below is the proposal; where it says "proposed", the build follows it. Differences: the changed files ride on the handle's signal summary and status; the `lost` result needed no change because the manifest already keeps the files.)*

Today a writer (an agent whose tools include write, edit, bash, exec or process; `ChangesFiles()`, `native_subagent_settings.go:618`) never becomes a handle: `subagent_call.go:61-63` sets `handle` only when `!changes`, and `subagent_call.go:98-106` makes the call wait to the end. This section checks the idea "if every write-capable tool is auto-approved, treat the run like a read-only agent" against the code.

### 21.1 Approval facts (file:line)

- Policy is a per-tool string, `approval: "off" | "ask"`, under `tools.native.<write|edit|bash|exec|process|profile_message|tool_smith>` (`approval_settings.go:11-14, 49-60`) and per MCP server (`mcp/manager.go:251-252`). `approval.default` ("off" when absent) fills tools without their own key at config load (`approval_default.go:46-91`). The result is stored once on the tool as `Tool.Approval` (`native/catalog.go:236-326, 435`).
- **There are no pattern, path or command-based approval rules.** The only per-call logic is `CallNeedsApproval` (`tools/approval.go:~117`): `process` without a `command` (wait/inspect) is never asked. Everything else with `Approval == ask` asks on every call (`tools/catalog.go:378-386`). Agent `paths.allow/deny` (`subagent_call.go:47-51`) is a hard deny, not a question. So the user's premise "a background child may hit an ask later because of a pattern or path" cannot happen with today's config.
- Run-dependent, not config-dependent: whether anybody can answer. `NewApprovalRequester` (`approval.go:60-75`) with no `ask` handler allows when `approval.headless == "allow"`, else returns `approval_unavailable`. The handler exists only when the HTTP run supports interaction (`httpapi/executor.go:61-63`).
- Child inheritance: the child's tools are the parent's own `tools.Tool` values (`childToolPools`, `subagent.go:173-190`), so each keeps the session's `Approval`. `allow_write` (`native_subagent_settings.go:255, 318`) only decides whether an agent may list those tools at all. MCP tools of an ask-server cannot be listed by any child (`subagent.go:517-518, 568-577`): children already cannot ask for MCP.
- **A static check is possible and exact**: "agent's catalog contains a tool with `Approval == ask`". Nothing narrows it per call except the `process` case above. Computed once per agent in `newSubagentToolAsync` (`subagent.go:~224`), no per-call work. Headless-allow stays outside the check (treated as "may ask", conservative); see Q-W3.

### 21.2 How a prompt reaches the user today, and denial-on-ask

`childApproval` (`subagent_call.go:119-131`) wraps the *parent's* requester for a writer, adding the agent name and a `parentCallID/callID` id. That requester is built per tool round in `session/runner.go:601-603` over `toolExecution.RequestInteraction`, which is the run's broker (`httpapi/executor.go:62`). The broker holds a single pending question (`approval.go:50-53` comment; `interaction_state.go`), shown on the parent run's SSE stream. If the parent turn ends, the broker and its ctx are gone, so a still-running child's question would be cancelled or unanswerable. It would also compete with the parent's own questions for the one slot. So a background writer must never ask.

Denial-on-ask is already built in: if `childExecution.RequestApproval` is nil and the tool is `ask`, `catalog.go:378-386` returns a blocked `approval_unavailable` ("the run has no way to ask"). For a background writer we simply do not call `childApproval` (`subagent_call.go:58-60`). A clearer code/text ("a background sub-agent cannot ask the user") is a ~10-line custom requester. With the static check this path is only a safety net, because a writer with an ask tool is never started in the background.

### 21.3 Checkpoint interaction

- The checkpoint is per *parent* run: `checkpoint.NewRun` (`runner.go:442-444`), `Start()` / `BeforeCalls` (`runner.go:584`, `runner_tools.go:176` via `CallMayChangeFiles` and `subagent.go:301`), `defer Finish()`. `Run` is "not safe for concurrent use" (`checkpoint/run.go:~45`); the child never touches it, so there is no race.
- A background writer keeps changing files after the parent's `Finish()` has captured the "after" tree (`checkpoint/service.go:532-557`). Effects: turn N's "after" misses the late changes; the next turn's "before" (`Begin`, `service.go:503`) contains them, so they are attributed to nobody; undoing turn N restores N's "before" tree and would erase late changes without a clean diff; a run that restores while the writer runs races with it. The first call (the `subagent` start) still triggers the `first_mutation` checkpoint, since `callMayChangeFiles` stays true for writer agents.
- `changes` and `stopped_processes` accounting is per child run and survives: `recordChange` via `OnFileChange` (`subagent_manager.go:469-472`), manifest `Changes/Files` (`:311-325`), `setProcessesStopped` at child end (`:505-509`). It is reported through the handle's result, not through the call.
- Recommendation: phase 1 documents that changes made after the starting turn ended are not in that turn's checkpoint, and shows the changed-file list in the handle result (`changed_files_max` must be > 0 for it, otherwise the result says nothing about files). Optional phase 2: the writer takes its own `Begin/Finish` pair (records are independent, `service.go:503-557`), but a whole-tree capture still includes concurrent parent edits.

### 21.4 Concurrency

- Existing limits: `max_concurrent` default 3 and `max_queued` (`native_subagent_settings.go:13, 45`, `subagent_manager.go:37, 391-400`); `process_max_running`. They count all runs, not writers.
- Existing locking: `PathLocks` (`path_locks.go`, built once in `resolver.go:233`) serializes single `write`/`edit` operations on one canonical path. It does not cover read-modify-write across calls, and `bash`/`exec`/`process` take no locks. The parent can edit the same files while a background writer runs, and two writers can interleave.
- Proposal: new setting `tools.native.subagent.max_background_writers`, default **1 per session owner** (counter in the manager keyed by owner, checked in `start`). Over the limit the call falls back to today's blocking behaviour and says so in the result (`background: false`, reason). The design also states: the model is told (tool description) that a running writer may edit files and the parent should not touch the same paths until it ends; no per-path locking beyond `PathLocks`.

### 21.5 Stop, reset, delete, child end, restart

- Registered under the session key like read-only handles, so the WP4 fan-out (section 8.2: `CancelOwner`, `DeleteOwnersWithPrefix`) already covers it. Child end still stops the child's own handles and processes (`subagent_manager.go:505-509`).
- Stop/cancel mid-run leaves partial changes. The existing result already warns ("an unfinished run may have left changes behind", `subagentRun.changes`) and gives `partial`, `transcript` and the changed files (`partialInformation`, `subagent_call.go:~170`).
- Reset and checkpoint restore while a writer runs: reset cancels children (`mutation_handlers.go:84-110`) before the workspace is touched. A checkpoint restore must stop the session's running writers first or be refused with 409. This is new work (WP-W3).
- Restart: queued/running becomes `lost` (`subagent_manager.go:32, 100-134`); the manifest keeps `Changes` and the files recorded so far, so the `lost` result can list them. Files changed by its `bash`/`process` children are not listed (as today); orphan processes follow `stop_orphans`. No attempt to resume. Shutdown stops writers like other handles (section 9.7).

### 21.6 Behaviour risk the idea hides

The default is `approval.default = "off"` (`approval_default.go:11-14`), so most sessions have *no* ask tools and every writer would silently become a background handle. That changes current behaviour for existing configs (a model that expected the final answer now gets a run id). Recommend opt-in: `tools.native.subagent.background_writers` (default false), documented in `sample/schemes/`.

### 21.7 Minimal implementation plan

- **WP-W1 settings and static check.** `native_subagent_settings.go` (keys, `background_writers` default false, `max_background_writers` default 1, validation), sample scheme, config tests. `subagent.go`: per-agent `backgroundSafe` = setting on and no tool in the agent's catalog has `Approval == ask`. Risk low.
- **WP-W2 start path.** `subagent_call.go:61-63, 98-106`: `handle` when `(!changes || backgroundSafe)` and under the writer limit; when background, `RequestApproval` stays nil (clear denial text); `subagent_manager.go` writer counter; `subagent.go:23, 28` and the agent list marker (`:373`) updated to say when a writer is handed off. Risk medium (reverses Q6 for opted-in agents).
- **WP-W3 lifecycle.** Result carries changed files and a "changes after the starting turn are not in its checkpoint" note; checkpoint restore/undo refuses or stops running writers; lost-writer result lists recorded files. Risk medium.
- Tests, failing-first: (1) opted-in writer with only `approval: off` tools and `block_ms: 0` returns a run id (fails today: blocks); (2) same agent with one `ask` tool still blocks to the end; (3) background writer calling an ask tool is blocked with the clear code (only reachable by forcing the flag in a test); (4) second writer over `max_background_writers` blocks and says so; (5) `async list/wait` shows the writer, result lists changed files; (6) user stop/reset/delete cancel it and the owner's handles; (7) restart gives `lost` with recorded files; (8) writer outliving its turn does not touch the `checkpoint.Run` (`-race`); (9) a session with `background_writers: false` is unchanged (golden); (10) goroutine baseline.

### 21.8 If the static check were not feasible

It is, so this is only a fallback: keep writers blocking and let the model choose an explicit parameter instead (`subagent {background: true}` accepted only for agents whose tools are all `approval: off`). Same static check, explicit intent, no behaviour change for existing calls.

### 21.9 Open questions for the user

- Q-W1 Opt-in setting (recommended, default off) or on whenever the static check passes?
- Q-W2 Over `max_background_writers`: fall back to blocking (recommended) or return `subagent_busy`?
- Q-W3 `approval.headless: allow` with no client: count as auto (writer goes to background) or stay conservative (blocking)?
- Q-W4 Checkpoint: accept "late changes are not in the starting turn's checkpoint" for phase 1, or require a separate checkpoint per background writer first?
- Q-W5 Should a checkpoint restore stop running writers or be refused with 409?
- Q-W6 Should the parent be told (tool text and result) to avoid the writer's paths, or do we want a per-run path lock for writers (not proposed: larger change)?
