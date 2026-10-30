# Phase 6b review — native command tools (exec, process, tool_smith, async, shell_async)

- reviewer: r2
- HEAD: `385cc89`. I wrote only this file (no code, config or git state); `git status` also shows `M EXECUTOR.md`, `M MASTER.md`, `M audit_plan.md` and untracked `REVIEWER*.md`, which were already like that before I started and are not mine. Scratch dir `/tmp/r2/` was created empty and removed.
- scope reviewed (non-test): `internal/tools/native/` — exec.go, exec_functions.go, exec_template.go, exec_templates/{js.js,python.py,ruby.rb}, process.go, process_async.go, process_group_{unix,windows}.go, process_manager.go, process_manifest_order.go, process_orphan{,_unix,_windows}.go, process_owner.go, process_statecontext.go, tool_smith.go, tool_smith_async.go, tool_smith_bridge.go, tool_smith_fn.go, tool_smith_list.go, tool_smith_open{,_unix,_windows}.go, tool_smith_prelude.ts, async_host.go, async_latest.go, async_statecontext.go, async_tool.go, shell_async.go, child_groups.go, scratch_sweep.go (5,850 lines).
- navigation: `codebase-memory` `get_architecture` + `search_graph`; whole-file `sed -n` ranges were used for the multi-function files (fallback per rule 3), plus `grep -n` for line numbers.
- commands (all from `backend-go/`): `go build ./internal/... ./cmd/...` → exit 0; `go vet ./internal/tools/native/` → exit 0; `gofmt -l internal/tools/native` → exit 0 (no output). No tests run (see 7).
- priors checked: `audit_plan.md` 1.1/4/5, `backend-go/CLAUDE.md`, DECISIONS entries reached by `git log` for this area (`c3ba56c` "tool_smith run is not sandboxed … pin its scheduling", `5d09534` "render process output outside the writer lock", `176fd7d` Close waits for Complete, `6a1a9c1` O_NOFOLLOW + same-file check, `453d3f5` bounded queued frames, `f442484` wait for queued end signal).

## 1. Understanding note

Two families of tools share one "child process that can outlive the call" mechanism:

- **exec / tool_smith** produce a `shellRunResult` inside `startShellAsync` (`shell_async.go:144`). That function registers a handle with the shared `asyncmgr.Manager`, runs the child on a **detached** context (`context.Background()` + an `outputWatch` value), writes the full result file, completes the handle and closes `done`. `block_ms` only ends the *wait*, never the child.
- **process** uses `ManagedProcessManager` (`process_manager.go`, 1,117 lines): one `managedProcess` entry per process (manifest, command, output collector, two spill writers, `done`), a persisted `manifest.json` + `owner.json` per process directory, recovery that classifies orphans against an owner record, and drivers (`process_async.go`) that map the process to async-handle states.

`tool_smith` additionally starts a **nested-tool bridge**: a unix socket (dir 0700, socket 0600, 128-bit token compared in constant time) serving `tools.<name>()` frames from the TS script, executed through a `tools.Catalog` built from the profile's allow-list minus the always-excluded tools.

Trust boundaries: the model (tool arguments), the script itself (full interpreter privileges, documented), local files (`functions_dir`, process storage), and the environment (allow-list only). Settings come from `sessionfs` (phase 2a, not reviewed here).

## 2. Hypotheses list

| id | statement | status |
|---|---|---|
| HY-1 | The tool_smith bridge loses `ExecutionContext` fields, so nested calls run with different authority than direct calls | **verified** (P6B-R2-01) |
| HY-2 | exec's `cwd`/code file could escape the workspace | disproved: `resolveExistingTarget(… settings.Policy)` at `bash.go:238`; code file always inside the call's `MkdirTemp` dir |
| HY-3 | exec builds a shell command from model text (injection) | disproved: argv only, `code` is one argv element (`exec.go:369-378`); no shell |
| HY-4 | the async "latest" view races the process writer | disproved: stream collectors have their own mutex (`bash_output.go:99-102, 449-464`) |
| HY-5 | the async shell goroutine has a panic/close boundary like the rest of the server (D-1) | **disproved** → finding P6B-R2-02 |
| HY-6 | a detached async run keeps the run-scoped context values (child groups) | **disproved** → finding P6B-R2-03 |
| HY-7 | process storage can grow without an effective bound | **verified** → P6B-R2-04 |
| HY-8 | tool_smith re-reads/re-copies the whole function library per run | **verified** → P6B-R2-05 |
| HY-9 | the agent "latest" block re-reads the whole transcript per async call | **verified** → P6B-R2-06 |
| HY-10 | the process manager's `Close` returns before processes stop (176fd7d class) | disproved for correctness (it waits for `done` per process), but it is sequential → P6B-R2-08 (latency) |
| HY-11 | a recycled pgid can be signalled by the grace timer | disproved: `processGroupAlive` before every hard kill (`process_group_unix.go:28-40`, `process_manager.go:620-644`) |
| HY-12 | tool_smith nested calls can reach the parent's processes | disproved: `ProcessOwner` is copied (`tool_smith_bridge.go:235`) |
| HY-13 | the bridge socket is reachable by another local user | disproved: 0700 dir (`tool_smith.go:99-108`), 0600 socket, token compare (`tool_smith_bridge.go:203-214, 416`) |
| HY-14 | stale scratch sweep covers dirs created in the `/tmp` fallback | **disproved** → P6B-R2-07 |

## 3. Findings

### P6B-R2-01  tool_smith bridge drops AgentDepth, RootRun and RemoteHops when it builds the nested ExecutionContext
- severity: high
- lens: B security (also A correctness)
- location: `backend-go/internal/tools/native/tool_smith_bridge.go:224-236` (the hand-copied `tools.ExecutionContext` literal); second hand-copy in `backend-go/internal/tools/native/subagent_call.go:54-65`
- evidence: [read] trace.
  The literal copies `WorkspaceRoot, SessionDir, SessionKey, AgentName, PathScope, RequestInteraction, OnInteractionResolved, OnFileChange, ProcessOwner, ProcessOwnerLimit, ProcessAsync` only. `internal/tools/catalog.go:62-71` also has `AgentDepth`, `RootRun`, `RemoteHops`; `Catalog.ExecuteFunctionCall` fills `CallID`, `ToolName`, `RawArguments` (`internal/tools/catalog.go:326-337`), so the three dropped fields reach the nested tool as their zero values. Consequences, each with the consumer:
  1. **`transcript`/`transcripts` child gate is skipped.** `transcript.go:189`, `transcripts.go:169`, `transcript_remote.go:290` refuse only when `execution.AgentDepth > 0 && !AllowChildren`. A script run inside a sub-agent (depth ≥ 1) that lists `transcript` in `tools.native.tool_smith.tools` reads transcripts even with `allow_children: false`. `transcripts`/`transcript` are not in `nativeToolSmithExcludedTools` (`internal/sessionfs/native_tool_smith_settings.go:41-42`), so only the config decides.
  2. **`transcript`'s own-run selector is wrong.** `transcript_path.go:79-83` picks `subagent:<ProcessOwner>` only when `AgentDepth > 0`; through the bridge it takes the parent's own transcript instead of the child's run.
  3. **`profile_message` from a script inside a child fails.** `profile_message.go:355-358` sets `WithChildCall` from `AgentDepth`; without it `internal/app/profile_message.go:70-83` finds no caller transcript path and returns "profile message needs the calling transcript path".
  4. **`max_runs_per_root` is bypassed.** `subagent_call.go:91` passes `rootRun: execution.RootRun`; `subagent_manager.go:491-493` then treats `""` as a new root (`root = id`) and counts `byRoot[root]` (`:501,521`), so every nested `subagent` call from a script starts a fresh root and the per-root cap never binds.
  5. **Remote hop counting restarts at 1.** `profile_message_remote.go:80` and `subagent_remote.go:127` send `execution.RemoteHops + 1`; a script inside a remote-originated run sends `1` instead of `hops+1`, so the hop guard under-counts.
- suggested fix: one helper `func (e ExecutionContext) ForNested() ExecutionContext` in `internal/tools/catalog.go` that copies every field except `CallID`/`ToolName`/`RawArguments` (which the catalog sets), used by `tool_smith_bridge.go:224` and by `subagent_call.go:54` (which then overrides `AgentName`, `PathScope`, `ProcessOwner`). Add `internal/tools/catalog_nested_test.go` walking `ExecutionContext` with `reflect` (`NumField`) and failing when a field is not in the helper's copy list, so the next added field cannot be dropped silently. Add one bridge test asserting a nested tool sees `AgentDepth`/`RootRun`/`RemoteHops` from the creating call.
- effort: S
- prior art: **P6AM-22** (carried in, "confirm"), confirmed with the concrete consumers above; the second hand-copy is new.

### P6B-R2-02  the async shell goroutine has no panic boundary, so a panic kills the server and leaves the handle `running`
- severity: medium
- lens: A correctness
- location: `backend-go/internal/tools/native/shell_async.go:167-186` (`go func(){ … result := run(runCtx) … }`)
- evidence: [read]. There is no `recover()` in the goroutine, and it is the only place where `runToolSmithScript`/`runShellChildDetailed` (exec) execute for a handle. A panic raised there is not caught by the D-1 recover in the session runner (different goroutine) — it aborts the whole server. Contrast the bridge, which recovers per frame: `tool_smith_bridge.go:344-348`. Secondary effect: whatever the panic, `Complete` is never called, so the handle stays `running` in the async manager until retention and its result file is never written.
- suggested fix: `defer func(){ if r := recover(); r != nil { slog…; _, _ = host.Manager().Complete(id, asyncmgr.Result{State: asyncmgr.StateFailed, Reason: "internal error"}); ref := "" } }()` inside the goroutine, wrapping `run`, mirroring `tool_smith_bridge.go:344`.
- effort: S
- prior art: new (D-1 was fixed for the tool-call path only).

### P6B-R2-03  an async exec/tool_smith run drops the run's context values, so `kill_children_on_run_end` never covers its descendants
- severity: low
- lens: A correctness
- location: `backend-go/internal/tools/native/shell_async.go:162-164` (`context.WithCancel(context.Background())`)
- evidence: [read]. Detaching cancellation is intended and documented ("only a stop, the child's timeout or the end of the server ends it"), but the whole value chain is dropped too. `ChildGroups` (`child_groups.go:21-33`) is carried by the run context (`internal/app/bound_runner.go:119`, `internal/app/subagent.go:81`) and read in `bash_child.go:163` (`childGroupsFrom(ctx)`); with an async handle that lookup returns nil, so a leftover process group of an async exec/tool_smith call is never registered and is not swept at run end even with `kill_children_on_run_end` on. The synchronous path does register it.
- suggested fix: keep the values and drop only the cancellation: `runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))` (Go 1.21+; module is `go 1.26`), then add `withOutputWatch`. One test: async exec leaving a descendant group is in `ChildGroups` after the call returns.
- effort: S
- prior art: new.

### P6B-R2-04  process logs can reach ~12.8 GB per tool-storage directory with the default settings
- severity: medium
- lens: C performance (disk; also B "limits on untrusted sizes")
- location: `backend-go/internal/tools/native/process_manager.go:273-274` (two `bashSpillWriter`s at `LogCapBytes`), `backend-go/internal/tools/native/process_manager.go:1056-1110` (`prune`), defaults `backend-go/internal/sessionfs/native_process_settings.go:13,15`
- evidence: [read] arithmetic: `log_cap_bytes` default 64 MiB × 2 streams × `max_completed` default 100 finished processes = 12.8 GiB per storage dir, all written before prune. A child that emits output without being read reaches the cap; nothing warns the model, and 8 running processes × 2 × 64 MiB more is live. Any model tool call can start such processes (`process` tool), so this is reachable without user intent.
- suggested fix: lower the default cap (bash spill default is far smaller) and/or add a per-directory total-bytes budget enforced in `applyBashSpillPolicy`-style before opening the writers; report `stdout_log_capped` as today.
- effort: M
- prior art: new (the 64 MiB default comes from the TS parity table shipped with `e12e430`/`74d2c11`; a DECISIONS entry was not found).

### P6B-R2-05  every tool_smith run copies the whole function library and re-reads every header
- severity: low (performance hypothesis H01)
- lens: C performance
- location: `backend-go/internal/tools/native/tool_smith_fn.go:27-76` (`stageToolSmithFunctions`: `os.ReadDir` at :46, `readToolSmithListEntry` at :54, `copyToolSmithFunction` at :58)
- evidence: [read]. Per call with `code`: one `ReadDir`, then for each of up to `list_max_files` (default 200) files one `EvalSymlinks` + `stat` + open + read of `list_header_bytes` (2 KiB) + a full copy of up to `function_max_bytes` (64 KiB) into the work dir. A 200-file library with 10 KiB helpers = ~2 MiB copied and ~600 syscalls per call, even when the script calls nothing.
- suggested fix (measured later, see H01): copy lazily, or reuse a per-async-dir staged library keyed by (dev, inode, mtime, size) list, or hard-link/copy only on `fn.` miss.
- effort: M
- prior art: new.

### P6B-R2-06  each `async` call re-reads and re-indexes every live agent transcript (up to 8 MiB each)
- severity: low (performance hypothesis H02)
- lens: C performance
- location: `backend-go/internal/tools/native/async_latest.go:208-260` (`activityLatest`: `indexTranscriptTurns` at :238, `scanTranscript` at :244), reached from `async_tool.go:194` (`wait`), `:230` (`list`), `:255` (`status`) via `latestBlocks` (`async_latest.go:40-70`)
- evidence: [read]. `indexTranscriptTurns` walks the file from offset 0 up to `latest_scan_max_bytes` (8 MiB default, `internal/asyncmgr/config.go:72`) and `scanTranscript` walks it again; the on-disk cursor only decides which steps are *reported*, not what is parsed. An agent polling `async wait` in a loop with two live children of ~1 MiB transcript pays ~2 MiB of parse per call, and the parse grows with the run.
- suggested fix (measured later, see H02): cache the turn index per (path, size) in the host, or index only the appended tail using the cursor offset.
- effort: M
- prior art: new; the commit that added it is `3e33c97` (never reviewed).

### P6B-R2-07  the stale-scratch sweep misses the `/tmp` fallback work dirs
- severity: low
- lens: D maintainability (A: leftover state)
- location: `backend-go/internal/tools/native/scratch_sweep.go:22-23` (`sweepStaleScratch(os.TempDir(), …)`) vs `backend-go/internal/tools/native/tool_smith.go:175, 243, 256` (`makeToolSmithWorkDir` falls back to the literal `/tmp` when the socket path would be too long)
- evidence: [read]. With `TMPDIR` set to something other than `/tmp` (common on macOS/containers), `tool-smith-*` dirs created by the fallback are never swept after a crash; the same code sweeps only one directory. `exec-` dirs are always created under `os.TempDir()` (`exec.go:219`), so they are covered.
- suggested fix: sweep both `os.TempDir()` and `toolSmithShortTempDir` (dedup when equal) inside the same `sync.Once`.
- effort: S
- prior art: new (`f4d2d30` added the sweep).

### P6B-R2-08  manager `Close` stops up to `max_running` processes one after another, each with two kill graces
- severity: low
- lens: A correctness (shutdown latency)
- location: `backend-go/internal/tools/native/process_manager.go:525-551`
- evidence: [read]. `Close` collects the running ids and then calls `Stop` sequentially; each `Stop` (`:479-521`) may wait `kill_grace_ms` (2,000 ms default) for the SIGTERM and again for the SIGKILL, plus one manifest fsync + rename. With the default `max_running` 8 a session whose children ignore SIGTERM adds up to ~32 s to `resolver.Close`, per storage dir, after the HTTP shutdown deadline has already passed (`internal/app/shutdown.go:14-36` bounds only the HTTP server).
- suggested fix: signal every group first, then wait for the `done` channels concurrently with one shared deadline (the pattern `stopOrphanGroups` already uses, `process_orphan.go:139-158`).
- effort: S
- prior art: new (D-3 covered the HTTP half only).

### P6B-R2-09  duplicated intent: listing helpers exists twice (exec and tool_smith) with two redaction/formatting paths
- severity: low
- lens: D maintainability
- location: `backend-go/internal/tools/native/exec_functions.go:220-336` (`executeExecList`) and `backend-go/internal/tools/native/tool_smith_list.go:72-175` (`executeToolSmithList`); shared pieces already exist (`toolSmithFilter`, `toolSmithMatchesFilters`, `resolveToolSmithFunctionsDir`, `openToolSmithFile`)
- evidence: [read]. Both build `filters`, resolve the library, list files, cut at `list_max_files`/`list_max_results`, collect `problems`, and render `truncated`/`shown`/`suggestion` with the same wording, in ~70 lines each; the divergences already visible are the malformed-entry label ("malformed entries" vs "malformed files") and the description of `scan_cut`. Since exec and tool_smith differ in file selection (per-runner subfolders vs flat `.ts`), the shared part is the render/limit block, not the scan.
- suggested fix: extract one `renderLibraryListing(entries, problems, limits, callID)` helper and keep two thin scanners; no behaviour change.
- effort: S
- prior art: new.

### P6B-R2-10  test quality: several bridge tests synchronise on `time.Sleep`
- severity: low
- lens: D maintainability
- location: `backend-go/internal/tools/native/tool_smith_bridge_test.go:577-580` (two 100 ms sleeps to assume `Close` is blocked), `:302` (200 ms sleep before asserting a late nested write), `:612` (200 ms inside the nested tool), `process_block_cancel_test.go:44` (200 ms)
- evidence: [read]. `TestToolSmithBridgeDoesNotCountQueuedCalls` relies on a 100 ms window to conclude `Close` is waiting; on a loaded machine (four reviewers + build) the assertion can pass or fail for the wrong reason. The rest of the file mostly uses channels, so this is local.
- suggested fix: replace the "is it blocked?" sleeps with a channel the blocked handler closes (`started` already exists), and read the second frame's reply instead of sleeping before `close(release)`.
- effort: S
- prior art: new.

## 4. Performance hypotheses (for the measurement step)

- H01: `tool_smith` re-stages the whole function library per call, `tool_smith_fn.go:27-76`
  workload: library of 200 helpers × 10 KiB (`list_max_files` 200, `function_max_bytes` 64 KiB), one `tool_smith` call with `code` per turn
  benchmark: new `BenchmarkToolSmithStageFunctions` (fixture: 200 generated `.ts` files in `t.TempDir()`, call `stageToolSmithFunctions`) — closest existing: `BenchmarkPersistManifestParallel` (different path)
  expected: cost scales with library size × files, flat per call; bytes copied 2 MiB/call at the fixture size
- H02: each `async` response re-indexes every live agent transcript, `async_latest.go:236-249`
  workload: 2 live sub-agent handles, 1 MiB `messages.jsonl` each, `async wait` polled every second
  benchmark: new `BenchmarkActivityLatest1MiB` (fixture: generated transcript, `activityLatest` with a cursor at the end) — no existing benchmark for the latest builder (gap noted in `audit_plan.md` §2)
  expected: ~2 MiB parsed per call, linear in transcript size, independent of how much is new
- H03: exec/tool_smith helper env resolution and template write per call, `exec_functions.go:33-56` (`resolveToolSmithFunctionsDir` → 2 × `EvalSymlinks` + `stat`) and `exec_template.go:36-66` (template file written per call)
  workload: 50 exec calls per turn in the same workspace
  benchmark: `BenchmarkExecTemplate` (new) or `BenchmarkProcessStartToExit` for the syscall baseline
  expected: 3-6 extra syscalls per call; small absolute cost, relevant only at high call rates
- H04: `async`/`process` detail and status render the full output windows each call, `process_async.go:139-146` (`detailText` → `Snapshot` → render) and `process_manager.go:680-701`
  workload: 8 running processes with 750 B head + 1,200 B tail per stream, `async list` in a poll loop
  benchmark: existing `BenchmarkProcessListManyFinished`; add `BenchmarkProcessDetailRunning`
  expected: linear in the number of live processes per call
- H05: `prune` sorts every terminal manifest on every process completion, `process_manager.go:1056-1110`
  workload: `max_completed` 100 manifests, 20 processes finishing per minute
  benchmark: existing `BenchmarkPersistManifestParallel` does not cover prune; new `BenchmarkPrune100Completed`
  expected: O(n log n) per completion, ~100 items — small, listed for completeness
- H06: `process` output writer holds `entry.mu` across an unbuffered spill write, `process_manager.go:214-229` + `bash_spill.go:140`
  workload: 32 KiB chunks from a chatty child while status pollers run
  benchmark: **existing** `BenchmarkProcessWriteWithStatusPollers` (`process_output_contention_test.go:30`, added with `5d09534`) — run it as the measurement, do not file as a defect

## 5. Cross-phase leads (outside 6b, not filed)

- `internal/tools/native/transcript.go:189`, `transcripts.go:169`, `transcript_remote.go:290`, `transcript_path.go:79-83` — the `AgentDepth`/`AllowChildren` gate that P6B-R2-01 bypasses (6c/7b).
- `internal/tools/native/profile_message.go:355-358` and `internal/app/profile_message.go:70-83` — `IsChildCall` path (6c / phase 1).
- `internal/tools/native/subagent_manager.go:491-521, 572-573` — root-run quota and `AgentDepth` assignment (6c).
- `internal/tools/native/profile_message_remote.go:80`, `subagent_remote.go:127` — hop arithmetic (6c/phase 4).
- `internal/sessionfs/native_process_settings.go:13,15` — `log_cap_bytes` 64 MiB and `max_completed` 100 defaults (phase 2a). A lower default or a total-budget key would be a settings change.
- `internal/tools/native/bash_child.go:163` and `internal/app/bound_runner.go:119` — `ChildGroups` plumbing (7a/phase 1).
- `internal/asyncmgr/config.go:72` — `latest_scan_max_bytes` 8 MiB default sizes H02 (phase 4).

## 6. Checked and sound

- exec cannot build a shell command from model text: argv only, one element per placeholder substitution, `{file}`/`{code}` replaced in configured flags (`exec.go:369-378`); NUL and `arg_max_bytes` refused for arg mode (`exec.go:351-357`).
- exec `cwd` is confined: `runShellChildDetailed` calls `resolveExistingTarget(run.cwd, …, settings.Policy, …)` (`bash.go:238-247`) before `os.Stat`, and the code/result files live in a fresh `MkdirTemp("", "exec-")` removed by `defer` (`exec.go:219-222`).
- exec refuses `input` for a stdin-mode runner and `filters` together with `code` (`exec.go:320-330, 356-360`); `input` is size-checked before it is written.
- Process environment inherits only the bash allow-list; `JAD_API_TOKEN`, the active provider key (`apiKeyEnv`) and `env_deny` entries are removed, and the result is sorted for determinism (`process_manager.go:753-786`).
- Orphan handling cannot hit a recycled pid: a stop needs an owner record naming a dead server *and* a matching process start time (`process_orphan.go:104-131`), and every hard kill is preceded by `processGroupAlive` (`process_group_unix.go:28-40`, `process_manager.go:620-644`).
- Bridge boundaries: private dir check (`tool_smith.go:99-108`), socket chmod 0600 (`tool_smith_bridge.go:203-214`), constant-time token compare (`:416`), frame size bounded without buffering the oversize remainder (`:375-397`), per-frame panic recover (`:344-348`), `Close` is `sync.Once` and waits for handlers (`:258-283`, no 176fd7d-class return-early).
- Nested-call concurrency is bounded twice: `max_nested_calls` frames and a `max_parallel_calls` gate plus a single writer gate (`tool_smith_bridge.go:334-348, 434-446`).
- Function-library confinement: `EvalSymlinks` + `toolSmithInside` + `O_NOFOLLOW` + `SameFile` and a size cap (`exec_functions.go:60-82`, `tool_smith_fn.go:79-130`, `tool_smith_open.go`); listing refuses names that are not `^[a-z][a-z0-9_]*$` (`tool_smith_list.go:232-235`).
- Async "latest" cursor arithmetic is clamped and monotonic: `since` (`bash_output.go:450-464`) clamps the offset to `[0, produced]` and reports `lost`; a block that does not fit leaves the cursor unmoved (`async_latest.go:40-70`).
- Latest blocks never expose reasoning: only `call` and `assistant` steps are reported (`async_latest.go:250-258`), and text is passed through `sanitizeBashOutput` (control characters/ANSI) exactly as the normal result path does (`bash_output.go:334-370`).
- An async tool_smith handle drops the interaction handlers before it runs, so a question cannot be asked after the call returned (`tool_smith_async.go:26-31`).
- `managedProcess.owner` is written before the entry is published and never changes, so the unlocked reads in `runningOwnedLocked`/`stopOwned` are safe (`process_owner.go:14-63`; `process_manager.go:275-296`).
- `finish` is `sync.Once`-guarded, closes `done` after `async.End`, and prunes after the close, matching the `176fd7d`/`f442484` contracts (`process_manager.go:631-712`).
- Process manifests keep one documented key-order exception only (`process_manifest_order.go`), consistent with the golden tests.

## 7. Not covered / why

- Nothing was executed: no targeted test was run in this pass, so every finding is `[read]`. `go build`/`go vet`/`gofmt` were run (exit 0) to confirm the tree I read compiles.
- No measurement (benchmarks/profiles are the coordinator's serialized step); H01-H06 are sized hypotheses only.
- `exec_templates/*.js|python.py|ruby.rb` were read for boundary behaviour (name validation, no eval of model text) but not reviewed as code for style; they are not Go and are only exercised through a real interpreter (the tests do run `bun`/`python3`/`ruby`, which I did not run).
- Approvals: the approval gate was removed in `d459b5c`; there is no approval code left in this phase to review (checked by grep for `Approval` in the phase files).
- `asyncmgr` internals (`Wait`/`List`/`Stop`/`Register`/`Progress` semantics, capacity) are phase 4 and were treated as a contract.
- Windows-only code (`process_group_windows.go`, `tool_smith_open_windows.go`, `process_orphan_windows.go`) was read and is trivial; it was not compiled or tested on this machine.
- Deep concurrency of the sub-agent manager (`subagent_manager.go`) and of the profile-message runs is phase 6c; only the call sites touched by P6B-R2-01 were read.
