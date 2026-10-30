# Phase p6b review — reviewer r3

- phase: p6b (native command tools: exec, process, tool_smith, async, shell_async)
- reviewer: r3
- HEAD: 385cc89153a411437cadc7a4b3263a66dc67588d
- date: 2026-10-29
- files read: backend-go/internal/tools/native/exec.go, exec_functions.go, exec_template.go, exec_templates/js.js, exec_templates/python.py, exec_templates/ruby.rb, process.go, process_async.go, process_group_unix.go, process_group_windows.go, process_manager.go, process_manifest_order.go, process_orphan.go, process_orphan_unix.go, process_orphan_windows.go, process_owner.go, process_statecontext.go, tool_smith.go, tool_smith_async.go, tool_smith_bridge.go, tool_smith_fn.go, tool_smith_list.go, tool_smith_open.go, tool_smith_open_unix.go, tool_smith_open_windows.go, tool_smith_prelude.ts, async_host.go, async_latest.go, async_statecontext.go, async_tool.go, shell_async.go, child_groups.go, scratch_sweep.go; backend-go/internal/tools/catalog.go (ExecutionContext struct); backend-go/internal/tools/native/subagent_call.go (hand-copy site); design_exec.md, design_bash.md, DECISIONS.md (tool_smith/exec entries)
- commands: see section 8

## 1. Understanding note

p6b covers three child-process runtimes plus the handle layer over them. `exec` runs inline code in a configured interpreter (argv/file/stdin delivery, per-runner `fn.<name>` templates, helper library under `functions_dir/<runner>/`); `tool_smith` runs a TypeScript body in a private workdir with a Unix-socket bridge for nested native-tool calls; `process` starts long-running shell commands tracked by `ManagedProcessManager` (manifest + logs on disk, orphan recovery, owner scoping for sub-agents). `shell_async.go` is the shared "run as async handle" wrapper for exec/tool_smith; `process_async.go` is the equivalent reporter/driver for managed processes; `async_tool.go` + `async_latest.go` implement wait/list/status/stop plus the per-handle "latest" news cursors. `child_groups.go` sweeps leftover process groups at run end; `scratch_sweep.go` cleans crashed-server temp dirs at startup. Trust boundaries: model-controlled code/input/cwd/block_ms vs local-user settings (runners, env, env_file, limits); the helper library on disk (shared with the write/edit tools); the bridge socket+token. State that crosses calls: async-manager handles with progress cursors, process manifests/logs, library files.

## 2. Hypotheses list

| id | statement | status |
|---|---|---|
| H1 | tool_smith bridge drops AgentDepth/RootRun/RemoteHops; subagent_call.go is a second hand-copy | verified |
| H2 | async shell goroutine has no recover; panic leaves handle running / kills server | verified (structural) |
| H3 | async exec/tool_smith detaches context values, so kill_children_on_run_end never applies | verified |
| H4 | startToolSmithAsync nils interaction hooks only on the async path (sync/async divergence) | verified |
| H5 | latestBlocks budget-cut advances the cursor past output it never shows | verified (structural) |
| H6 | env allowlist is bypassable via overrides | disproved (overrides are local-user settings, not model input) |
| H7 | orphan recovery can signal a recycled pid/pgid | disproved (owner record + start-time check + alive polling) |
| H8 | process List/prune/activityLatest/scratch staging costs scale with size | hypotheses (see 4) |

## 3. Findings

(appended incrementally)
### P6B-R3-01 tool_smith bridge drops AgentDepth, RootRun, RemoteHops; subagent_call.go is a second hand-copy
- severity: medium
- lens: B correctness-of-boundary (D fix shape)
- location: backend-go/internal/tools/native/tool_smith_bridge.go:224-236; backend-go/internal/tools/native/subagent_call.go:54-60; backend-go/internal/tools/catalog.go:27-71
- evidence: [read] `ExecutionContext` has 17 fields. The bridge literal copies 11 (WorkspaceRoot, SessionDir, SessionKey, AgentName, PathScope, RequestInteraction, OnInteractionResolved, OnFileChange, ProcessOwner, ProcessOwnerLimit, ProcessAsync) and drops CallID/ToolName (per-call, correctly replaced by `baseCallID` sequence) and RawArguments (per-call, set by the catalog call path — correct to drop), but also drops AgentDepth, RootRun and RemoteHops, which are run-scoped, not per-call. `subagent_call.go:54-60` builds a second literal keeping only WorkspaceRoot, SessionDir, SessionKey, AgentName, RemoteHops — dropping AgentDepth/RootRun (passed out-of-band as start depth/rootRun), plus RequestInteraction, OnInteractionResolved, OnFileChange and the ProcessOwner trio. So a nested tool inside a tool_smith script observes depth 0 / no root run / RemoteHops 0 even when the outer call is a remote child, and the two hand-copies already disagree with each other (bridge keeps PathScope+hooks, child drops them). No `ForNested`/copy helper exists on the struct.
- suggested fix: add `func (e ExecutionContext) ForNested(callID, toolName string) ExecutionContext` in `internal/tools/catalog.go` that copies the struct and clears only per-call fields (CallID, ToolName, RawArguments), use it in both files, and add a field-walking test (reflect over struct fields, fail on unhandled new field).
- effort: S
- prior art: P6AM-22 (carried in) — confirmed; second hand-copy site is new evidence.

### P6B-R3-02 async exec/tool_smith goroutine has no panic boundary; a panic leaves the handle `running`
- severity: medium
- lens: A correctness
- location: backend-go/internal/tools/native/shell_async.go:167-186
- evidence: [read] the `go func()` runs `run(runCtx)` (runToolSmithScript / runShellChildDetailed, i.e. template attach, staging, copy, manifest write) with no `recover()`, then Completes the handle. Any panic in that path crashes the server (the synchronous tool path is protected by `recoverToolPanic` in `internal/tools/catalog.go:407-415`, so this is a divergence, not a pattern). The handle would stay `running` in the async manager.
- suggested fix: `defer` a recover in the goroutine that Completes the handle as failed with the panic text, mirroring `executeRecovered`.
- effort: S
- prior art: new (D-1 covered the call path only).

### P6B-R3-03 async exec/tool_smith runs detach context values, so `kill_children_on_run_end` never covers them
- severity: medium
- lens: A correctness
- location: backend-go/internal/tools/native/shell_async.go:162-164
- evidence: [read] `runCtx, cancel := context.WithCancel(context.Background())` discards all values of the caller's ctx; the code re-attaches only the output watch. `childGroupsFrom(runCtx)` is therefore always nil on the async path, so `bash_child` never registers the group (`groups.add`) and `WithChildGroups` sweep at run end cannot stop descendants of async runs — while the synchronous path (same `run` closure over the caller ctx) does register. Detaching cancellation is intended and documented; dropping the values is not.
- suggested fix: `context.WithCancel(context.WithoutCancel(ctx))` (Go 1.21+).
- effort: S
- prior art: new.

### P6B-R3-04 async tool_smith runs silently lose interaction hooks that sync runs keep
- severity: low
- lens: A correctness (D divergence)
- location: backend-go/internal/tools/native/tool_smith_async.go:21-29
- evidence: [read] `startToolSmithAsync` sets `execution.RequestInteraction/OnInteractionResolved = nil` before building the run closure, so `runToolSmithScript` → `startToolSmithBridge` receives nil hooks on the async path, while the sync path (`host == nil || !canHandle`, tool_smith.go) forwards the outer call's hooks to the bridge. The bridge test pins hook forwarding for the direct path, so async and sync runs behave differently with no documented reason.
- suggested fix: either document that nested calls from async runs cannot interact (and say so in the tool description), or stop nil-ing and let the bridge propagate.
- effort: S
- prior art: new.

### P6B-R3-05 latestBlocks budget cut advances the cursor past output it never shows
- severity: low
- lens: A correctness
- location: backend-go/internal/tools/native/async_latest.go:40-69 (esp. 57-64)
- evidence: [read] `outputLatest` returns text already cut to `LatestBytes` with a `…+N bytes earlier` marker and `next` = full produced offsets; `latestBlocks` then cuts `text` again to the remaining `LatestTotalBytes` budget via `cutUTF8` with no marker, yet still calls `SetProgress(id, next)`. The dropped tail is newest output the caller never saw and will never be re-offered. The per-stream cut is honest; the budget cut is silent.
- suggested fix: on budget cut, keep the cursor (like the `budget <= 0` break / skip path) or append a truncation marker.
- effort: S
- prior art: new.

### P6B-R3-06 process_manager.go is 1117 lines of mixed duties; env builders are duplicated
- severity: low
- lens: D maintainability
- location: backend-go/internal/tools/native/process_manager.go:1-1117; backend-go/internal/tools/native/process_manager.go:753-784 vs bash.go:382-420
- evidence: [read] one file owns lifecycle (Start/Stop/Wait/Close), disk format (persist/read/prune), log windows, id/time helpers and env construction. `buildNativeProcessEnvironment` re-implements `buildNativeBashEnvironment`'s allowlist-then-overrides-then-deny shape with a subtly different deny order (bash folds JAD_API_TOKEN/apiKeyEnv into the deny set before applying overrides; process deletes after — same result today, two places to keep in sync).
- suggested fix: no move now (two call sites only); if touched, extract one `buildNativeEnv(base, allowlist, overrides, apiKeyEnv, denied)` helper.
- effort: S
- prior art: new.
## 4. Performance hypotheses (measurement step input)

- H1: `activityLatest` re-opens, stats, re-indexes and re-scans up to `LatestScanMaxBytes` (default 8 MiB) of transcript per live agent handle on every async wait/list/status that attaches a latest block, backend-go/internal/tools/native/async_latest.go:187-265
  workload: W live subagent handles with T-byte transcripts, polled every wait round
  benchmark: new `BenchmarkActivityLatest_NHandles` (fixture: N handles with transcripts of T bytes; existing related: `BenchmarkSubagentListManyFinished`)
  expected: cost scales with W x T per poll
- H2: `stageToolSmithFunctions` re-reads every library header and re-copies every valid function per tool_smith run, backend-go/internal/tools/native/tool_smith_fn.go:17-57
  workload: library of F functions x frequent tool_smith calls
  benchmark: new `BenchmarkStageToolSmithFunctions` (fixture: F saved functions)
  expected: cost scales with F per run
- H3: `ManagedProcessManager.List` clones + sorts all manifests and `prune` sorts completed on every finish, backend-go/internal/tools/native/process_manager.go (List, prune)
  workload: P completed processes retained (MaxCompleted/retention)
  benchmark: existing `BenchmarkProcessListManyFinished`, `BenchmarkProcessListWhileStarting`
  expected: O(P log P) per List/finish
- H4: `ManagedProcessManager.Close` stops running processes sequentially, each up to 2x killGrace, backend-go/internal/tools/native/process_manager.go:525-552
  workload: R running processes x kill_grace_ms at session close
  benchmark: new `BenchmarkManagerCloseManyRunning` (fixture: R sleepers)
  expected: shutdown latency scales with R x grace (consider concurrent stop)
- H5: `SnapshotWithLogs` re-renders the full collector windows on every wait poll, backend-go/internal/tools/native/process_manager.go (SnapshotWithLogs, snapshotEntry)
  workload: frequent block_ms polling of a chatty process
  benchmark: existing `BenchmarkProcessStartToExit` / new poll-loop bench
  expected: repeated full-window render per poll

## 5. Cross-phase leads (outside p6b, not findings)

- `backend-go/internal/tools/native/subagent_remote.go` / `profile_message_remote.go` send `RemoteHops+1`: with P6B-R3-01, a tool_smith script inside a remotely-started run resets the count, weakening loop/depth guards owned by 6a/remote code.
- `backend-go/internal/tools/catalog.go:407-415` (`recoverToolPanic`): the recover boundary P6B-R3-02 asks to mirror lives here; no change proposed to this file.
- `backend-go/internal/sessionfs/native_*.go` (env_file parsing/merging) and `path.go`/`path_scope.go` (cwd/policy resolution) are trusted by p6b env/cwd handling but belong to other phases; env-file-at-startup semantics assumed, not re-audited.
- `backend-go/internal/asyncmgr` (manager Complete/MarkRead/Progress/List semantics) assumed correct; cursor-advance bug P6B-R3-05 is on the p6b side of that contract.

## 6. Checked and sound

- Env inheritance (A-2): `buildNativeBashEnvironment` (bash.go:382) and `buildNativeProcessEnvironment` (process_manager.go:753) allowlist ambient env; overrides come only from local-user settings (env/env_file), never model input; JAD_API_TOKEN + apiKeyEnv always stripped. No model-reachable bypass found.
- Orphan/kill + recycled pgid (B-C3): `classifyOrphan` requires owner record + dead server + pid start-time match; `killGroupAfter`/`endProcessGroup`/sweep re-check `processGroupAlive` before SIGKILL; stop path polls liveness instead of sleeping into a recycled id. Windows stubs signal nothing, matching "no process groups" design.
- Exec command construction: no shell; argv array with {file}/{code} substitution, NUL rejected for arg-mode runners, stdin-mode runners refuse input; `resolveBashExecutable` PATH lookup is standard.
- Bridge frame handling: size cap discards without buffering, oversize id parsed from prefix only, token compared with `subtle.ConstantTimeCompare`, malformed frames fail only their own call, writer serialization for writes, Close waits for handlers (no 176fd7d-class leak).
- Scratch sweep: Lstat (no follow), dirs only, exact 0700, prefix + 7h age, failures ignored; socket path kept under sun_path via /tmp fallback; workdirs 0700/0600.
- Secret handling: bridge token + socket exist only inside the per-run workdir and are removed at Close before the result renders; no secret scrubber exists for command output (documented behavior — output is the answer), so not filed.
- `subagent_call.go` depth-cap check happens before the hand-copy; noted only as the second ForNested site (P6B-R3-01).

## 7. Not covered and why

- `async_latest_test.go` (377 lines) and other `*_test.go` used only as evidence; no systematic test-quality audit beyond the sleep-sync spot pattern (insufficient basis for a D finding).
- Full transcript-step indexing (`transcript_steps.go`, out of phase) internals behind `indexTranscriptTurns`/`scanTranscript` assumed; only the cursor contract was reviewed.
- No benchmarks run (reviewers do not measure); numbers above are hypotheses for the measurement step.
- Flaky-test check: TestRemoteProfileMessageCloseEndsFollowersWithoutLeaks / TestRemoteSubagentCloseLeavesNoGoroutines not run (full-suite tests, out of scope); nothing observed suggests recurrence.

## 8. Commands run (exit codes)

- `go build ./internal/tools/native/` → 0
- `go vet ./internal/tools/native/` → 0
- `gofmt -l internal/tools/native/` → 0 (clean)
- `go test ./internal/tools/native/ -run TestToolSmithBridgeForwardsTheExecutionHooksOfTheOuterCall -count=1` → 0 (pass)
- `go test ./internal/tools/native/ -run TestLatestTotalIsCappedAcrossHandles -count=1` → 0 (pass)
- Note: prompt allows `go build ./...`; per backend-go/CLAUDE.md the repo root has a cgo shim that breaks `./...`, so the package-scoped build above was used instead. No `-race`/`-bench`/`./...`, no network, no fixed ports, no files outside the report + `/tmp/r3/` (empty, nothing to remove).
