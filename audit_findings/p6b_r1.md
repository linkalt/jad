# Phase p6b review, reviewer r1

- Phase: p6b, native command tools (exec, process, tool_smith, async, shell_async), `backend-go/internal/tools/native/`
- HEAD: 385cc89. Review date: 2026-10-31.
- Navigation: deviation from the prompt. codebase-memory `search_graph` was used once (asyncmgr Close/Complete). zvec-grep and `get_code_snippet`/`trace_path` were not used. Everything else was read with `sed`/`grep` line ranges via `exec`. No indexed evidence is claimed beyond that one call.
- Files read: exec.go, exec_functions.go, exec_template.go, exec_templates/*, process.go, process_async.go, process_group_unix/windows.go, process_manager.go (all), process_manifest_order.go (head), process_orphan*.go, process_owner.go, process_statecontext.go, tool_smith.go, tool_smith_async.go, tool_smith_bridge.go, tool_smith_fn.go, tool_smith_list.go, tool_smith_open*.go, tool_smith_prelude.ts, async_host.go, async_latest.go, async_statecontext.go, async_tool.go, shell_async.go, child_groups.go, scratch_sweep.go. Outside the phase, only to verify: tools/catalog.go (ExecutionContext, ExecuteFunctionCall, ChangesState), bash.go and bash_child.go (runShellChildDetailed, env build), bash_output.go (since), bash_spill.go, asyncmgr/manager.go (Close, Complete, progress), subagent_call.go, subagent_manager.go:560-640, app/resolver.go close path, app/remote_api.go:221, sessionfs exec/tool_smith settings. Tests were read only by grep (sleep counts, field coverage).
- Commands (cwd `backend-go/`):
  - `go build ./internal/... ./cmd/...` exit 0 (CLAUDE.md forbids `./...`)
  - `go vet ./internal/tools/native/` exit 0
  - `gofmt -l internal/tools/native` no output, exit 0
  - 4 throw-away probes via `go test -overlay /tmp/r1/overlay*.json ./internal/tools/native/ -run TestR1... -count=1`: probe1 (template rewrite) exit 1 FAIL as predicted (confirms P6B-R1-02); probe2 (bridge context) exit 0, printed depth=0 root="" hops=0 (confirms P6B-R1-01); probe3 (stage count) exit 0, inconclusive, its fixture file names were malformed; probe4 (400 start/wait cycles, stale running manifests on disk) exit 0, 0 stale, so the race in P6B-R1-06 was not reproduced. Probes were never in the tree; /tmp/r1 removed.
  - No full test run, no -race.

## 1. Understanding note

exec, tool_smith and process each start OS children. exec and tool_smith go through `runShellChildDetailed` (bash.go). When a `block_ms` window runs out they continue as a `shellRun` handle (`shell_async.go`), kept in `AsyncHost.shellRuns` and registered with the asyncmgr as kind `exec` or `tool_smith`. `process` uses `ManagedProcessManager`, which keeps a manifest on disk per process, an owner record for crash recovery, and an `entries` map in memory. Handles are owned by session key, or by `ProcessOwner` inside a sub-agent. The async tool lists, waits for, inspects and stops them. "latest" blocks come from `asyncLatester` drivers whose cursor lives in the manager. tool_smith runs a TypeScript child with a unix-socket bridge (`tool_smith_bridge.go`) that executes allowed native tools in-process through `tools.Catalog.ExecuteFunctionCall`. Scratch dirs (`exec-*`, `tool-smith-*` in TMPDIR) are swept once per process by `scratch_sweep.go`.

## 2. Hypotheses

| id | statement | status |
|---|---|---|
| A1 | bridge ExecutionContext copy drops AgentDepth/RootRun/RemoteHops (P6AM-22) | verified (probe2) |
| A2 | `{template}` substitution runs over argv that already contains the user's code | verified (probe1) |
| A3 | `startShellAsync` goroutine has no recover and no join at host Close | verified by read (only 2 recover sites in package: subagent_manager.go:645, tool_smith_bridge.go:345) |
| A4 | recycled pgid can be signalled by `ChildGroups.sweep` | verified by read |
| A5 | Stop/setLeftRunning can overwrite a final manifest in `manager.manifests` and on disk | read; not reproduced (probe4) |
| A6 | ManagedProcessManager.Close returns while `prune` still removes dirs | verified by read |
| A7 | Stop between `Begin` and `command.Start` leaves the process unstopped | read |
| A8 | env inheritance (A-2) leaks in process/exec/tool_smith | disproved: allowlist plus denies, see section 6 |
| A9 | approval gate on these tools | not applicable (approval system removed, DECISIONS.md "approval system is removed") |
| A10 | async_latest cursor/scrubbing lets output forge the tool envelope | disproved: `sanitizeBashOutput(…, true)` strips control bytes; Information values with newlines are JSON-quoted (`tools/output.go:valueText`) |
| A11 | scratch sweep follows symlinks | disproved: Lstat + IsDir + 0700 + `RemoveAll` on the path only |

## 3. Findings

### P6B-R1-01  tool_smith bridge builds the nested ExecutionContext by hand and drops AgentDepth, RootRun, RemoteHops (confirms P6AM-22)
- severity: medium
- lens: B security (boundary bypass), A correctness
- location: tool_smith_bridge.go:224-238 (copy); tools/catalog.go:27-71 (struct, 15 fields)
- evidence: probe2 [measured]: bridge started with `ExecutionContext{AgentDepth:2, RootRun:"root1", RemoteHops:2, ProcessOwner:"run1"}`; the nested tool saw `depth=0 root="" hops=0 owner="run1"`. Consequences, by read: `transcript.go:189`, `transcripts.go:169`, `transcript_remote.go:290` deny a child only when `AgentDepth > 0`, so a sub-agent with tool_smith and `transcript`/`transcripts` in `tools.native.tool_smith.tools` reaches them as depth 0 and bypasses `allow_children=false`. `transcript_path.go:81` returns the caller's own transcript instead of `subagent:<run>`. `profile_message.go:356` skips `WithChildCall`. `profile_message_remote.go:80`, `transcript_remote.go:279/328` and `subagent_remote.go:127` send `RemoteHops+1` from 0, so the remote hop limit (`max_hops`, remoteapi/server.go:27) restarts at the nested call. Not reachable: nested `subagent` is excluded (`nativeToolSmithExcludedTools`, sessionfs/native_tool_smith_settings.go:41). Reachable by: a model in a child run, or in a run started by a remote peer, when the profile lists these tools for tool_smith.
- other hand copies of ExecutionContext (complete list from `grep ExecutionContext{`): tools/native/subagent_call.go:54 (child context: carries WorkspaceRoot, SessionDir, SessionKey, AgentName, RemoteHops, PathScope; the rest is set in subagent_manager.go:569-577; intentional narrowing but also hand-written); app/remote_api.go:221 and httpapi/executor.go:61 (fresh top-level contexts, correct); app/remote_transcripts.go:122 (fixed caller, fine); tool_smith_async.go:22-25 clears two fields by assignment (fine).
- suggested fix: add `func (e ExecutionContext) ForNested() ExecutionContext` in tools/catalog.go that copies the value and resets only the per-call fields (CallID, ToolName, RawArguments); call it in tool_smith_bridge.go instead of the literal. Add a test using `reflect` to walk every field of `ExecutionContext`, set it non-zero, call `ForNested`, and assert each field is either equal or listed in an explicit "reset" set, so a new field fails the test until classified. Optionally the same pattern (`ForChild`) for subagent_call.go.
- effort: S
- prior art: P6AM-22 (carried in), re-confirmed

### P6B-R1-02  exec rewrites `{template}` inside the model's own code
- severity: medium
- lens: A correctness
- location: exec_template.go:63-67 (loop `strings.ReplaceAll(item, "{template}", path)` over the full argv); exec.go:362-377 (`prepare` puts the code into argv first)
- evidence: probe1 [measured]: for `node -r {template} -e {code}` with code `console.log("{template}")`, the final argv is `-e console.log("/tmp/.../template.js")`; the code text changed. Affects node, ts, ruby (any runner with a template and mode arg). A program that contains a literal `{template}` (string templating, mustache/format strings, docs) is silently altered.
- suggested fix: replace `{template}` in the runner's argv flags before `{code}` is inserted (the path is known: `workDir/template<ext>`), or substitute `{code}` last in `attachExecTemplate`. exec.go:362-377 and exec_template.go:63-67.
- effort: S
- prior art: new (exec rework, unreviewed)

### P6B-R1-03  shell-run goroutine is outside every recover boundary
- severity: medium (read, D-1 class; reachable only through a panic bug in the child path)
- lens: A correctness
- location: shell_async.go:167-186 (goroutine runs `run(runCtx)`, `os.WriteFile`, `Complete`); process_manager.go:378 (`go manager.waitForProcess`, which runs `finish`, `End`, `prune`); async_latest.go: no goroutine
- evidence [read]: The synchronous call is covered by `catalog.go:433`. The detached goroutines are not. `grep recover()` in the package finds only subagent_manager.go:645 and tool_smith_bridge.go:345 (the latter swallows the panic with no log). A panic in `runToolSmithScript`/`runShellChildDetailed`/`finish` takes down the whole server, and the handle would never reach `Complete`. D-1 added recovery for HTTP/tool calls; these goroutines were created by the async rework afterwards.
- suggested fix: wrap the goroutine body in shell_async.go:167 and `waitForProcess` with a `defer recover` that logs and completes the handle as failed ("the program crashed"); log in tool_smith_bridge.go:345.
- effort: S
- prior art: D-1 (fixed class, new sites)

### P6B-R1-04  AsyncHost.Close does not join the shell-run goroutines (176fd7d class)
- severity: low-medium [read]
- lens: A correctness
- location: shell_async.go:167 (untracked goroutine), async_host.go:84 (`Close` is `manager.Close()` only); asyncmgr/manager.go:569-629 (waits at most StopWait, 2 s default)
- evidence [read]: `Manager.Close` calls `shellDriver.Stop`, which only cancels `runCtx`. The child then gets SIGTERM, waits `kill_grace_ms` (default 2 s, `sessionfs/native_bash_settings.go:14`) and a kill deadline, so a stubborn child outlives the 2 s StopWait. The goroutine still runs after Close returns: it writes `<async>/exec/<id>.txt` (shell_async.go:172-175) and calls `Complete` on a closed manager. Same symptom as the known `.async: directory not empty` flake. If the server exits before the SIGKILL timer fires, a child that ignores SIGTERM survives; shell runs started as handles are not in `ChildGroups` and have no owner record (unlike `process`).
- suggested fix: keep a `sync.WaitGroup` (or the shellRuns map plus done channels) in AsyncHost; `Close` waits for the done channels after `manager.Close`, bounded by the kill deadline; a regression test with a SIGTERM-ignoring child that asserts the result file is not created after Close returns.
- effort: M
- prior art: new (same class as 176fd7d)

### P6B-R1-05  ChildGroups.sweep can signal a recycled process group
- severity: medium [read]
- lens: B security / A correctness (B-C3 residual)
- location: child_groups.go:34-57 (`add` stores only the pgid; `sweep` rechecks `processGroupAlive(pgid)`); process_group_unix.go:55 (`kill(-pgid,0)`)
- evidence [read]: `add` runs at the end of each call and stores the group only "while a member is still alive". `sweep` runs at the end of the whole run, which can be many minutes or hours later. If the original group ended meanwhile and the number was reused as the pgid of an unrelated process (for example any process that calls `setpgid(0,0)`), `processGroupAlive` is true for the stranger, which gets SIGTERM and, after `grace`, SIGKILL. `killGroupAfter` and `endProcessGroup` are safe because their window is one grace period; this one is not. `Recover` (process_orphan.go:112-123) already shows the right proof (record process start time), but a reaped leader cannot be checked, so a pid-start-time check does not apply directly. Probability: requires pid wrap; real on hosts with `pid_max=32768` and fork-heavy load. Reach: any user of bash/exec/tool_smith with `kill_children_on_run_end`.
- suggested fix: drop a group from the map as soon as it is observed dead (a periodic `processGroupAlive` check in `add` of later calls, or a poll in sweep scheduled earlier), and record, at `add` time, `processStartTime` of any still-living member read from `/proc/<pgid>/…` if available; or store a pidfd of the leader on Linux. Smallest: keep the map small and re-verify with `getpgid`/`/proc/<pid>/stat` start time recorded at `add` for the leader's zombie-less member. Needs a design choice.
- effort: M
- prior art: B-C3 (recycled pgid), partly fixed for killGroupAfter, open here

### P6B-R1-06  Stop and setLeftRunning write a stale manifest after finish
- severity: low [read, not reproduced]
- lens: A correctness
- location: process_manager.go:495-510 (Stop: `updated` cloned under `entry.mu`, then `manager.manifests[id] = updated` and `persistManifest(updated)` after the lock is released); process_manager.go:716-724 (`setLeftRunning` same shape)
- evidence [read]: `finish` (process_manager.go:600-665) updates `manager.manifests` and persists. If the process exits and `finish` completes between Stop's `entry.mu.Unlock()` and its `manager.mu` write, the map and the file are overwritten with `state: running`. Effects: `Start`'s capacity count (`manifests` with `State==Running`, line 233) and `runningOwnedLocked` count a dead process until restart; `prune` (terminal manifests only) never removes it; `List` filters by the stale state; after a restart `Recover` marks it lost. `setLeftRunning` has a wider window (after the grace waits time out). probe4 (400 start/wait cycles, no concurrent stop) found 0 stale manifests, so only the race remains unproven.
- suggested fix: in Stop and `setLeftRunning`, update `manager.manifests` while holding `entry.mu` and only if `!processTerminal(entry.manifest.State)`, or re-read the entry's manifest under the lock before writing; add a test that stops a process at the moment it exits (loop with `sleep 0`).
- effort: S
- prior art: new

### P6B-R1-07  ManagedProcessManager.Close returns while `prune` still deletes directories
- severity: low
- lens: A correctness (176fd7d class)
- location: process_manager.go:626-632 (`close(entry.done)` then `prune` still running in the `waitForProcess` goroutine); process_manager.go:525-552 (`Close` waits for `entry.done` through `Stop`)
- evidence [read]: `done` closes before the prune by design (comment at 625-626). `Close` therefore returns while the last finisher may still `RemoveAll` old process dirs under `StorageDir`. A caller that removes the storage dir right after (tests with `t.TempDir()`, resolver shutdown) races with it and can see "directory not empty". Also `Close` stops processes sequentially (see H2).
- suggested fix: track the `waitForProcess` goroutine with a WaitGroup that `Close` waits on, or run prune before closing `done` only when `closed` is set.
- effort: S
- prior art: new

### P6B-R1-08  A stop that arrives between Begin and command.Start does not stop the process
- severity: low [read]
- lens: A correctness
- location: process_manager.go:289 (`Async.Begin` registers the handle), 348 (`command.Start`), 482-510 (`Stop` with `command == nil`)
- evidence [read]: From `Begin` to `Start` the handle is live and its driver `Stop` works (`stopAsync` → `Stop`). In that window `Stop` sees `pid <= 0 || command == nil`, sets `StopReason`, marks `LeftRunning`, and returns. `Start` then starts the process anyway; `waitForProcess` later reports `stopped` when it exits on its own, but nothing signals it. Reached by `CancelOwner`/`stopOwned` at a sub-agent run end, or `Manager.Close`'s driver stop (Close waits for `starting` first, so shutdown is safe). The window is the owner-record write and an fsync (a few ms), so narrow.
- suggested fix: in `Start`, after `command.Start()`, check `entry.manifest.StopReason != ""` and signal the group at once; or make `Stop` on a not-yet-started entry mark it so `Start` aborts before spawning.
- effort: S
- prior art: new

### P6B-R1-09  exec listing is unbounded for stray entries and malformed files
- severity: low [read]
- lens: B/C (untrusted size)
- location: exec_functions.go:137-164 (`strays` appended for every non-runner top-level entry of the library dir), 233-260 (`problems` printed whole); tool_smith_list.go:97-136 same for malformed files (bounded by list_max_files)
- evidence [read]: the library lives in the workspace, so the model (or a checked-out repo) controls it. A `functions_dir` with 100 000 junk entries yields a 100 000-line result; `ListMaxResults` cuts only `matched`. exec and tool_smith otherwise cut every list.
- suggested fix: cap `problems` and `strays` at `ListMaxResults` and add "…and N more" to the summary.
- effort: S
- prior art: new

### P6B-R1-10  Stale-scratch sweep does not cover the /tmp fallback directory
- severity: low
- lens: A correctness
- location: scratch_sweep.go:21-31 (`os.TempDir()` only); tool_smith.go:233-250 (`makeToolSmithWorkDir` falls back to the hard-coded `/tmp` when TMPDIR makes the socket path too long)
- evidence [read]: with a long TMPDIR, `tool-smith-*` dirs of a crashed server go to `/tmp` and are never swept (TMPDIR ≠ /tmp). Also `sweepStaleScratchOnce` runs synchronously in `NewExecTool`/`NewToolSmithTool` (see H5), and the prefix match `exec-*`/`tool-smith-*` with mode 0700 and age > 7 h could in theory remove another program's dir of the same name pattern (hardening, unreachable in normal use; sticky bit protects other users' dirs).
- suggested fix: sweep both `os.TempDir()` and `toolSmithShortTempDir`.
- effort: S
- prior art: new

### P6B-R1-11  Three copies of the "resolve, inside check, regular file, no-follow open" sequence
- severity: low
- lens: D maintainability
- location: exec_functions.go:60-83 (`openExecFunction`), tool_smith_fn.go:75-95 (`copyToolSmithFunction`), tool_smith_list.go:222-240 (`readToolSmithListEntry`)
- evidence [read]: identical logic (EvalSymlinks, `toolSmithInside` twice, Stat IsRegular, `openToolSmithFile`) with different problem strings. The next hardening fix (for example a TOCTOU change) must be made three times. Also `executeExecList` (exec_functions.go:210-300) and `executeToolSmithList` repeat the filter parsing and the "truncated/shown/suggestion" information block.
- suggested fix: have `copyToolSmithFunction` and `readToolSmithListEntry` call `openExecFunction`-style `openLibraryFile(dir, realRoot, file) (*os.File, os.FileInfo, string)` (rename, move to tool_smith_list.go).
- effort: S
- prior art: new

### P6B-R1-12  process_manager.go is 1117 lines with mixed duties, and Start repeats the failure block
- severity: low
- lens: D maintainability
- location: process_manager.go (types and JSON helpers 1-110 and 800-870; Start 218-385 (about 170 lines); recovery 870-940; persistence 945-980; log windows 1000-1060; prune 1075-1117); Start failure block at process_manager.go:308-312, 332-337, 350-355 (`finish(entry, ProcessFailed, ProcessManifest{StopReason, ExitCode: null, Signal: null})`, three times); `executeNativeProcessAsync` (process.go:92-240, about 150 lines), `executeAsync` (async_tool.go:118-250, about 130 lines), `executeExecList` (about 115 lines)
- suggested fix: split into process_manifest.go (types/JSON), process_recover.go, process_prune.go; add `failStart(entry, cause)`; split `executeAsync` by action. Do not combine with behaviour changes.
- effort: M
- prior art: new

### P6B-R1-13  Production dead code
- severity: low
- lens: D
- location: exec.go:182-192 (`executeNativeExec` has callers only in tests: child_groups_test.go:170, exec_input_test.go:189, exec_test.go:50), process_manager.go:191 (`Directory()` no caller outside its definition in non-test code). `runShellChild` (bash.go:199) is used only by bash.
- evidence: `grep -rn` in `internal/`, non-test files, shows only the definitions. The tests exercise the wrapper, not the registered Execute closure (same code path, `host == nil`).
- suggested fix: move the wrapper to a `_test.go` file; delete `Directory()` if the tests do not use it.
- effort: S
- prior art: new

### P6B-R1-14  exec file-write failure returns an error with an empty call id
- severity: low
- lens: A correctness
- location: exec.go:366 (`tools.ExecutionError("", …, "spawn_failed", "The code file could not be written")`)
- evidence [read]: every other error in the file passes `callID`. `prepare` has no access to the call id, so the result header carries `"id":""`. The outer wrapper uses the call_id, so the model still matches it; transcripts and the bridge parse a blank id.
- suggested fix: return the system error from `prepare` and render with `callID` at the caller (exec.go:229-232).
- effort: S
- prior art: new

### P6B-R1-15  process start errors expose server paths to the model
- severity: low
- lens: B security (leak)
- location: process.go:195-197 (`process_start_failed` with `err.Error()`); process_manager.go:262, 298-309, 382 (errors wrap `os` errors that include the storage path)
- evidence [read]: for example "create process storage: mkdir /home/…/tool-storage/processes/proc_…: permission denied". The path is the profile's tool storage, not the workspace.
- suggested fix: map errors to a fixed message and log the detail, as for `async_capacity_reached`.
- effort: S
- prior art: new

### P6B-R1-16  Test gaps for the new commits
- severity: low
- lens: D test quality
- location: tool_smith_bridge_test.go (no `AgentDepth/RootRun/RemoteHops` anywhere in tool_smith*_test.go; 6 `time.Sleep`), child_groups_test.go (6 sleeps, fixed 300 ms), exec_template_test.go (no literal `{template}` in code), process_* tests (no stop/exit race, no start-stop window test), shell_async tests (no panic, no Close-with-stubborn-child case)
- evidence: grep counts. Several tests wait by sleeping (`child_groups_test.go:53,76,93,160`), which is machine-load sensitive.
- suggested fix: the field-walking test of P6B-R1-01, a `{template}` case for P6B-R1-02, and polling helpers for the sleeps.
- effort: S
- prior art: new

### P6B-R1-17  Recover forces SIGKILL on a pid it has not re-verified (hardening, unreachable)
- severity: low
- lens: B
- location: process_orphan.go:131-157 (`stopOrphanGroups`: `processExists(pid)` is the only liveness test before the SIGKILL)
- evidence [read]: the start-time proof is made once (classifyOrphan); if the leader dies and its pid is reused by a new group leader inside `grace` (default 2 s), `processExists` is true and the forced signal hits the stranger. Requires pid wrap inside 2 s: negligible on Linux's sequential allocator. Cheap fix: compare `processStartTime(pid)` with the recorded value before the forced signal.
- effort: S
- prior art: B-C3 (residual)

### P6B-R1-18  Retention is applied only when another process finishes
- severity: low
- lens: A/C
- location: process_manager.go:630 (`prune` called from `finish` only); `Recover` never prunes
- evidence [read]: `retention_days` and `max_completed` are enforced when some process ends. A profile that stops starting processes keeps its old manifests and logs (disk) forever; recovered "lost" manifests also count in `manifests` but are not pruned until the next finish. Bounded by disk, not memory.
- suggested fix: call `prune` at the end of `Recover`.
- effort: S
- prior art: new

## 4. Performance hypotheses (for measurement)

- H1: `activityLatest` scans the whole transcript from offset 0 on every async wait/list/status for each live agent handle, then scans again from the cursor, with `json.Unmarshal` per line (async_latest.go:186-205, `indexTranscriptTurns` at transcript_scan.go:95). Up to `latest_scan_max_bytes` (8 MiB default) per handle per call, no ctx (uses Background).
  workload: 10 live sub-agent handles (list_default_limit 10, max 50), transcripts of 4-8 MiB, 20 status calls.
  benchmark: new `BenchmarkActivityLatest` with a generated 8 MiB messages.jsonl and a cursor at 90%.
  expected: cost scales with transcript size, not with news; starting the index at `cursor.off` should cut it to the news size.
- H2: `ManagedProcessManager.Close` stops processes one after another; each can take 2×`kill_grace_ms` (process_manager.go:525-552, `Stop` 482-523).
  workload: `max_running` (default 8, max 1024) children that ignore SIGTERM, kill_grace 2 s.
  benchmark: new test timing `Close` with 8 `trap '' TERM; sleep 100` children.
  expected: ~8×4 s sequentially vs ~4 s if all signalled first (the server shutdown deadline D-3 would cut it).
- H3: `processStreamWriter.Write` holds `entry.mu` while writing the spill file to disk (process_manager.go:200-215); `snapshotEntry`, `Stop`, `finish`, `writeOwnerRecord`-adjacent paths and the latest block wait for that lock.
  workload: a child writing 100 MB/s while async status is polled.
  benchmark: existing `process_lock_benchmark_test.go` and `process_output_contention_test.go` (re-run on a slow disk).
  expected: poll latency grows with disk latency; moving the file write out of the lock removes it.
- H4: per exec call repeated library I/O: `resolveToolSmithFunctionsDir` (EvalSymlinks of root and dir, Stat) runs in `execHelperEnvironment` and again in `execTemplateText`, plus `EvalSymlinks` of the runner dir, `Lstat` of the template, `MkdirTemp`, template write, result-file read (exec_functions.go:33-58, exec_template.go:63-85, exec.go:197-240).
  workload: 1000 exec calls of `true` in bash, library with 200 files.
  benchmark: new `BenchmarkExecTrue`.
  expected: ~15-25 extra syscalls per call, small next to fork/exec; check only if exec latency matters.
- H5: `sweepStaleScratchOnce` does `ReadDir` of the temp dir synchronously while the first catalog is built (scratch_sweep.go:23-25).
  workload: TMPDIR with 100 000 entries.
  benchmark: new test timing `sweepStaleScratch` on a synthetic dir.
  expected: first session start delayed by the scan; one-time.
- H6: `stageToolSmithFunctions` validates and copies every library function for each tool_smith run, even when the script never calls `fn` (tool_smith_fn.go:20-70; each file at most `function_max_bytes`, 64 KiB; up to `list_max_files`, 200 by default, max 10 000).
  workload: 200 functions × 64 KiB per run, 50 runs.
  benchmark: new `BenchmarkToolSmithStage`.
  expected: up to ~13 MB of copying and ~600 syscalls per run at defaults; lazy copy on first `fn` use would make it free for scripts that do not call `fn`.
- H7: `ManagedProcessManager.Start` creates the process directory (`MkdirAll`) while holding `manager.mu` and scans all manifests for the running count (process_manager.go:228-262).
  workload: 1000 stored manifests, 8 starts per second.
  benchmark: new `BenchmarkProcessStart`.
  expected: lock hold grows with stored manifests and disk latency; probably negligible below `max_completed` 200.
- H8: Start and finish persist the manifest with fsync each time (process_manager.go:945-980): 2 fsyncs per start, 1 per finish, plus `Stop` persisting `running+stop_reason`.
  workload: 100 short processes per minute on a slow disk.
  benchmark: `BenchmarkProcessStart` with `strace -c -e fsync` count.
  expected: 3-4 fsyncs per process; durability is needed for crash recovery, so only the second persist could be dropped.

## 5. Cross-phase leads

- tools/native/bash_child.go:163-165: `ChildGroups.add` defers with the group pid; the recycled-pgid concern of P6B-R1-05 is in phase 6a/bash, not in the scope here.
- tools/native/transcript_path.go:81, transcript.go:189, transcripts.go:169, profile_message.go:356, profile_message_remote.go:80, transcript_remote.go:279/328/290, subagent_remote.go:127: all consumers of AgentDepth/RemoteHops that P6B-R1-01 makes reachable at depth 0 (phase 6c/7).
- tools/native/subagent_call.go:54-60: hand-built child ExecutionContext (phase 6c); same ForNested/ForChild helper shape.
- tool_smith async scripts: `startToolSmithAsync` keeps `OnFileChange` and `ProcessOwner` of the starting call (tool_smith_async.go:20-26); a nested write after the call returned invokes the sub-agent run's change recorder after that call ended. Whether `recordChange` is safe after the run's end was not checked (phase 6c, subagent_manager.go:575).
- sessionfs/native_tool_smith_settings.go:41: `nativeToolSmithExcludedTools` excludes `bash`, `exec`, `process`, `async`, `subagent`, `tool_smith`, `task`, but `profile_message`, `write`, `edit` and `fetch` remain allowed by config: design choice, noted because `profile_message` hops depend on P6B-R1-01.
- path policy: `runShellChildDetailed` and `executeNativeProcessAsync` resolve `cwd` with `settings.Policy`, not `scopedPolicy(execution)` (edit.go:92, write.go:94 do apply it). A child with `paths` can start bash/exec/process in an out-of-scope cwd. This may be intended (a shell cannot be path-scoped) but I found no DECISIONS.md entry; worth a decision record (phase 6a/6c).
- app/resolver.go:1031-1039: shutdown order is subagent managers, process managers, then async hosts. Consistent with the comment; process `Close` is sequential (H2).

## 6. Checked and sound

- Process environment (A-2): `buildNativeProcessEnvironment` (process_manager.go:754-790) keeps only the allowlist from `os.Environ()`, adds configured env, and then deletes `JAD_API_TOKEN`, the provider key variable and `env_deny`. `buildNativeBashEnvironment` (bash.go:405-440) applies the same denies; exec/tool_smith add only `extraEnv` (JAD_RESULT, TOOL_SMITH_*, library vars) after that. `PYTHONPATH`/`NODE_PATH` are not in the allowlist, so a host value cannot inject a module path. Duplicates of PATH from `prependEnvironmentList` are collapsed by `os/exec` (last wins).
- Command construction: exec uses argv with no shell for the runner; the code reaches the child as a single argv element, a file, or stdin. `{file}` replacement happens before `{code}` is inserted (exec.go:372-374), so code containing `{file}` is not rewritten (only `{template}`, see 02). NUL is refused for arg mode (exec.go:322).
- exec helper library: `resolveToolSmithFunctionsDir` rejects absolute, `..`, and symlinks leaving the workspace; `openExecFunction` re-checks containment against the library and the workspace root and opens with `O_NOFOLLOW` plus `os.SameFile` (exec_functions.go:60-83, tool_smith_open.go). Runner names are validated by `nativeExecNamePattern` (sessionfs/native_exec_settings.go:53). The runner folder goes on PATH/PYTHONPATH as its resolved real path, so a later symlink swap does not redirect it. That the model can write executables into that folder is the documented design.
- Scratch dirs: `exec-*` dirs are created by `MkdirTemp` (0700) and removed by `defer os.RemoveAll`; sweep uses `Lstat`, requires a directory with perm 0700 and age > 7 h, and cannot follow links.
- Bridge: socket in a 0700 directory, `chmod 0600`, token compared with `subtle.ConstantTimeCompare`; frames are length-capped with a discard path (`readBridgeFrame`); calls are capped by `MaxNestedCalls`, parallel gate and single writer gate; `Close` is idempotent and waits for all goroutines; per-frame panic is recovered (without a log, see 03); nested names are limited to the `allowed` list minus the excluded set and the profile deny list; envelope parsing cannot be forged because content markers carry a per-content token and information values are single-line.
- async_latest: cursor kept in the manager and dropped at Complete; budget logic matches the comment; `outputLatest` cuts at UTF-8 boundaries and sanitizes ANSI/control bytes; the `latest` value goes through `valueText` (JSON-quoted when multi-line) or the JSON `blocks` list; remote runs show only id and age; no raw file paths of remote runs.
- shellRun data flow: `handle.text` is written before `close(done)` and read after it; `reason` is guarded by a mutex; `dropShellRun` before `Complete` leaves `latest` returning "" safely; `sweepShellResults` is throttled and removes only regular files older than the retention.
- Process ownership: `ownedProcessManager` hides foreign ids as not found; `stopOwned` takes `manager.mu` then `entry.mu` (same order as elsewhere); I found no lock-order inversion between `manager.mu`, `entry.mu`, `persistMu`, `recoverMu`.
- Start failure paths: `Begin` failure removes the entry and the dir; manifest persist failure calls `Abort`; spawn failure goes through `finish` so `End` completes the handle exactly once (`finishOnce`).
- gofmt, vet and build clean (section header).

## 7. Not covered

- zvec-grep was not used; codebase-memory `get_architecture`/`trace_path`/`get_code_snippet` were not used (the index was searched once with `search_graph`). All reading was by `sed`/`grep` line ranges, so the "mandatory navigation" rule was not followed beyond that one call.
- No test execution beyond the 4 overlay probes; no `-race` run, so the races in P6B-R1-04/06/08 are code-read only.
- Windows files (process_group_windows.go, process_orphan_windows.go, tool_smith_open_windows.go) read for shape only; behaviour on Windows not examined.
- exec_templates/*.js|py|rb read for env/path use only (dir from `JAD_RUNNER_DIR`), not for correctness of the entry-point rules.
- tool_smith_prelude.ts reply buffering (known deferral 3 in DECISIONS.md) not re-measured.
- process_manifest_order.go beyond the first 60 lines; process_statecontext.go sorted/limit logic read, no finding.
- Earlier-commit history of the exec rework was not walked commit by commit (`git log` was not run); review is of the tree at 385cc89.
