# p6c reviewer r1: native agent tools (subagent, profile_message, remote/external, manager, statecontext)

- phase: p6c   reviewer: r1   HEAD: 3bf8c33   date: 2026-11-01
- files read (index tools were used for the first lookups: get_architecture, search_graph, get_code_snippet for ForNested; its qualified name failed (method on ExecutionContext) so the rest was read with exec `sed`/`cat` by exact range, which is the stated fallback): subagent.go, subagent_call.go, subagent_manager.go, subagent_store.go, subagent_async.go, subagent_external.go, subagent_remote.go, subagent_answer.go, subagent_changes.go, subagent_statecontext.go, subagent_owner*.go, statecontext_counts.go, profile_message.go, profile_message_async.go, profile_message_remote.go. Read as evidence: tools/catalog.go (ExecutionContext, ForNested), async_host.go, remote/{hub,run,client,entry,target}.go, app/{remote_api,subagent,resolver,profile_message}.go, asyncmgr (Register, Complete, Close, Stop), transcript_path.go, execution_scope_test.go, parts of the subagent/remote tests.
- commands: `go build ./...` exit 0; `go vet ./internal/tools/native/` exit 0; `gofmt -l internal/tools/native` no output, exit 0; `go test ./internal/tools/native/ -run 'TestRemoteSubagentCloseLeavesNoGoroutines|TestRemoteProfileMessageCloseEndsFollowersWithoutLeaks' -count=1` printed `ok ... 0.027s` (the `exit=0` I echoed was the exit of `tail`, not of go test; the `ok` line is the evidence). The `.async: directory not empty` flake did not appear.

## 1. Understanding

- `SubagentManager` (subagent_manager.go) owns local runs per storage dir. `start` reserves a slot or queue place, optionally an async handle, then spawns `execute` (launcher under `runLauncher` recover, timeout, process/handle cleanup, `finish`). Manifests (`.runs/sub_*.json`) are written by `subagentStore.save` under one store mutex (temp file, fsync, rename). `recover` marks queued/running runs `lost` unless a live foreign server wrote them.
- `subagentTool` (subagent.go, subagent_call.go): local path `startRun` -> manager; remote path `startRemote` (subagent_remote.go) -> `startRemoteRun` (profile_message_remote.go) -> `remoteRun.follow` on a `remote.Hub` goroutine. External (remote API) path: `WithExternalRun` observer; no handle, call waits, ctx cancel stops the run.
- `childExecutionContext` = `ForNested()` + clear interaction/file hooks + AgentName + narrowed PathScope; `manager.start` then overwrites ProcessOwner, ProcessOwnerLimit, ProcessAsync, AgentDepth, RootRun, OnFileChange. No stale inherited field found (sound).
- profile_message: local (`localTarget`: own `<category>/<profile>` or `<session>/<category>/<profile>` gated by `sessions` globs) and remote (`executeRemote`), inline vs handle (`runInline`/`runHandle`).

## 2. Hypotheses (status)

- A1 remote subagent skips local depth/run gates: VERIFIED (F-01).
- A2 inline remote cancel can hang: VERIFIED by read (F-02).
- A3 nested runs are owned by the parent run id, not the session: VERIFIED (F-03).
- A4 locks across I/O in finish: VERIFIED (F-04); lock-order inversion m.mu/store.mu/run.mu: DISPROVED (run.mu is never held while taking m.mu or store.mu; save takes store.mu then run.mu, drop takes m.mu then store.mu/run.mu; no reverse edge found).
- A5 Close/shutdown returns before work stops: DISPROVED for runs (`active.Wait`), but Wait is unbounded (F-07). Hub.Close waits followers; the untracked Stop goroutine outlives it (F-10).
- A6 panic recovery on goroutines: PARTIAL (F-05).
- A7 `ForNested`/child context leaks parent hooks: DISPROVED.
- A8 manifest recovery corrupts a foreign server's runs: DISPROVED (foreignServerAlive guard, recover skips them).
- A9 transcript dir deletion can aim outside storage: DISPROVED (`transcriptFolderUnder` demands one path element under root and not `.runs`; `insideDirectory` is strict).
- A10 secrets in errors/transcripts: PARTIAL (F-12, F-13). Token scrubbed in remote client (sound).
- A11 data race on `subagentTool.agents` (bindMCP vs readers): unverified/hardening only (section 5b).

## 3. Findings

### P6C-R1-01  Remote sub-agent bypasses max_depth and every local run cap
- severity: medium
- lens: A correctness
- location: internal/tools/native/subagent_remote.go:80-184 (no depth check), compare subagent_call.go:70-73 and subagent_manager.go:497-506
- evidence: [read] `startRun` refuses at `depth+1 > max(MaxDepth,1)` and `manager.start` enforces `max_runs_per_root`/`max_runs_per_session`. `startRemote` never reads `execution.AgentDepth`, `RootRun` or `SessionKey` counts; the only brake is the async per-owner handle cap (asyncmgr Register, MaxHandles 16) and the remote's hop limit. A grandchild at the configured max depth that lists `subagent` can therefore still start remote agents. design_remote.md:53 says "Nesting/depth limits apply locally to the local call"; test `TestRemoteSubagentNeverChangesLocalCheckpointOrLimits` (subagent_remote_test.go:214) only checks local slots are unused and has no depth case.
- suggested fix: in `startRemote`, apply the same depth check as `startRun` (share one helper) before registering; add a test with AgentDepth = MaxDepth. Files: subagent_remote.go, subagent_call.go.
- effort: S
- prior art: new

### P6C-R1-02  Cancelling an inline remote sub-agent call blocks until the remote answers or is declared lost
- severity: medium
- lens: A correctness
- location: internal/tools/native/subagent_remote.go:140-146; compare profile_message.go:419-425
- evidence: [read] on `ctx.Done()` the unhandled branch calls `run.Stop(...)` then `<-run.Done()`. `Stop` only fires an async best-effort HTTP stop (profile_message_remote.go:226-234); `Done` closes when `follow` sees a terminal state, `ErrLost` (lost_after_ms, default 60 s, up to 1 h, remote/entry.go:34-36) or the remote timeout (default 600 s). With an unreachable or unresponsive remote, a user stop of the turn waits up to that long. The profile_message twin (`runInline`) returns at once after `Stop`, so the two inline paths diverge. Reachable by: user (stop), against a slow or partitioned remote.
- suggested fix: return `"", ctx.Err()` right after `run.Stop` like `runInline`, or wait with a short bound (stopCallTimeout). File: subagent_remote.go.
- effort: S
- prior art: new

### P6C-R1-03  Nested run owner is the parent run id, so session-scoped views and the transcript owner gate misattribute it
- severity: medium
- lens: A correctness
- location: internal/tools/native/subagent_call.go:98-99 and :123 (`owner: owner` from `handleOwner`), subagent_manager.go:208 (field comment says "session key"), subagent_manager.go:742-751 (`RunTranscript` returns `run.owner`); consumer transcript_path.go:186-195 (cross-phase)
- evidence: [read] `handleOwner` returns `execution.ProcessOwner` (the parent's run id) inside a child, and that value is stored as `subagentRun.owner`. Effects: (a) `transcript_path.go:187` does `strings.Cut(owner, "\x00")`; for a nested run that yields `sub_xxxxxxxx`, never equal to the caller's session, so with `allow_children=true` and `allow_other_sessions=false` the session's own grandchild run is refused as "belongs to another session"; (b) `list(owner)`/`CollectSubagentStateContext`/`ownedRuns(sessionKey)` never show nested runs; (c) the manifest (`Owner`) keeps the run id, and `session` (a run field, line 213) is not persisted, so after restart there is no way to recover the session. The session key is available (`run.session`) but only the async owner semantics are used.
- suggested fix: keep two fields: `owner` = session key (what `RunTranscript` and lists use) and an async owner only for the handle; persist `session` in the manifest. Files: subagent_manager.go, subagent_store.go, subagent_call.go (+ check transcript_path.go).
- effort: M
- prior art: new

### P6C-R1-04  `finish` removes manifests under `m.mu`, behind a store mutex that is held across fsync
- severity: medium
- lens: A correctness / C performance
- location: internal/tools/native/subagent_manager.go:152-158 (`dropOldestLocked` -> `m.store.remove`), :703-710 (loop in `finish` under `m.mu`); subagent_store.go:76-92 (`save` holds `s.mu` across MkdirAll, CreateTemp, Write, Sync, Rename) and :149-157 (`remove` takes `s.mu`)
- evidence: [read] the comment at :150-151 says "a slow disk never holds the lock", true for the transcript delete, but the manifest `os.Remove` still runs under `m.mu`, and `remove` first waits for `s.mu`, which any run's `save` (fsync) holds. While a child writes a manifest, every `start`, `acquire`, `release`, `Cancel*`, `list` and `byID` call is blocked behind a finishing run. [unmeasured]
- suggested fix: collect the ids to drop under `m.mu`, remove the manifests after unlocking (same as transcripts). File: subagent_manager.go.
- effort: S
- prior art: new

### P6C-R1-05  A panic in the launcher leaves no log or stack, and the panic value goes into the run reason
- severity: low
- lens: A correctness / B security
- location: internal/tools/native/subagent_manager.go:643-651 (`runLauncher`), :589-631 (`execute` body outside any recover)
- evidence: [read] `recover()` turns the panic into `fmt.Errorf("sub-agent launcher panicked: %v", recovered)`; nothing is logged and no stack is kept. DECISIONS.md:1084 (6a) set the rule that a recovered failure leaves a `slog` line. The value ends in `run.reason`, the manifest, the model-facing result (`boundedReason`, 400 B) and the remote API view. The code after the launcher (`DeleteOwnersWithPrefix`, `stopRunProcesses`, `finish`) is outside the recover: a panic there crashes the process and leaves `done` open (D-1 class, but no concrete panic site found).
- suggested fix: log value and stack with `slog.Error` in the deferred func and put a fixed text in the reason. File: subagent_manager.go.
- effort: S
- prior art: related to 6a decision on logToolFailure (fixed in tools package, not here)

### P6C-R1-06  The slot is given back after `done` closes, and queued runs can be overtaken
- severity: low
- lens: A correctness
- location: internal/tools/native/subagent_manager.go:589-631 (`defer m.release()` runs after `finish` closed `run.done`), :497-509 (`start` takes a free slot immediately) vs :653-672 (`acquire` loop)
- evidence: [read] defers in `execute` run in reverse, so `release()` follows `finish` -> `close(run.done)`. A caller woken by `done` that immediately starts a nested run sees the slot still held: a nested start is never queued (`errSubagentNoSlot`, "busy") even though the slot frees microseconds later. Also a new top-level `start` takes `m.running < limit` before any queued waiter wakes, so queued runs have no FIFO guarantee and can starve while starts keep arriving. [unmeasured]
- suggested fix: release the slot before `finish` (explicit call instead of defer) and make queued waiters take slots in order. File: subagent_manager.go.
- effort: S
- prior art: new

### P6C-R1-07  `SubagentManager.Close` waits without a bound
- severity: low
- lens: A correctness
- location: internal/tools/native/subagent_manager.go:888-897
- evidence: [read] `m.active.Wait()` has no timer; the launcher contract ("must stop promptly") is the only guard, and `app/resolver.go:1031-1033` calls Close synchronously before the process managers are closed. A child blocked in something that needs the process manager (or a launcher that ignores ctx) hangs shutdown. Compare `AsyncHost.waitShellRuns`, which bounds the wait by the kill grace.
- suggested fix: wait with a deadline (config or the longest kill grace) and log the runs still going. File: subagent_manager.go.
- effort: S
- prior art: new

### P6C-R1-08  Concurrent status of a restored run can return an empty answer
- severity: low
- lens: A correctness
- location: internal/tools/native/subagent_manager.go:356-381 (`loadText` sets `textLoaded = true` before the read), subagent_async.go:135-137
- evidence: [read] a second `Detail` call arriving while the first is still reading the transcript sees `textLoaded` true, returns, and `view()` has empty `Text`; `Detail` then returns "" and asyncmgr falls back to the stored summary. Also a failed read is never retried.
- suggested fix: guard the load with a `sync.Once`-style state (loading/loaded) and make the second caller wait. File: subagent_manager.go.
- effort: S
- prior art: new

### P6C-R1-09  Remote-run start can orphan a remote run, and the stop goroutine has no owner
- severity: low
- lens: A correctness
- location: internal/tools/native/profile_message_remote.go:132-134 (`directory.Go` false after `StartRun` succeeded), :226-234 (`go func` in `Stop`)
- evidence: [read] (1) `startRemoteRun` has already got a run id from the remote when `Go` refuses (hub closed); it returns "the server is shutting down" without sending a stop, so the remote work keeps running, and the text says nothing was started. (2) `remoteRun.Stop` starts an untracked goroutine (bounded by `stopCallTimeout` = 3 s) that can outlive `Hub.Close`/`client.Close()`; this is why the goroutine-leak tests need a `before+4` tolerance (subagent_remote_test.go:257-274). Shutdown-only window.
- suggested fix: on the `Go` failure call `StopRun` with the 3 s bound; run the stop through `directory.Go` when it can, else keep it as is and document. File: profile_message_remote.go.
- effort: S
- prior art: new

### P6C-R1-10  Model-facing remote errors name server file paths and env variable names
- severity: low
- lens: B security
- location: internal/tools/native/profile_message_remote.go:89-95 (`remoteProblem`, comment says "without any secret ... names files and keys only"), :122-125; remote/entry.go:262-277, :314
- evidence: [read] `remoteProblem` formats `%v` of the loader error. Those include `read ... remotes/<name>.json` OS paths (`entry.go:314` wraps `os.ReadFile` errors), the token file path ("token file %s cannot be read"), and `token_env` names. They reach the model (`remote_unavailable`) and so the transcript. Reachable by the model in any session with `remotes` enabled; impact is path disclosure only, no token.
- suggested fix: map loader errors to fixed texts (not found, invalid file, token unavailable) and log the detail. Files: profile_message_remote.go (+ remote/entry.go messages).
- effort: S
- prior art: new

### P6C-R1-11  A failed run's reason is stored unbounded and unscrubbed, and sent to remote peers
- severity: low
- lens: B security
- location: internal/tools/native/subagent_manager.go:626 (`err.Error()` as reason), subagent_store.go:35 (`Reason` in manifest); consumer app/remote_api.go:292-294 (cross-phase)
- evidence: [read] for a failed run `finish` stores the full provider/launcher error text as `run.reason`; the model-facing path uses `boundedReason` (400 B), but the manifest on disk and `ExternalRun.Outcome()` get the raw text, which the remote API returns as `Message` (cut to 1024) to the token holder. Provider error bodies can carry request ids, org names or base URLs. [unmeasured: no sample provider error checked]
- suggested fix: `boundedReason` once at `finish` for storage, and give remote peers a fixed code-based message. Files: subagent_manager.go, app/remote_api.go.
- effort: S
- prior art: new

### P6C-R1-12  Crash leaves `run-*.tmp` files in `.runs` forever
- severity: low
- lens: A correctness
- location: internal/tools/native/subagent_store.go:96-98 (temp created, removed by `defer os.Remove`), :120-141 (`load` ignores names that do not match `sub_*.json`)
- evidence: [read] a crash between `CreateTemp` and `Rename` leaves a temp file; nothing deletes it at startup (`recover` and `load` skip it). One small file per crash, never reclaimed.
- suggested fix: remove `run-*.tmp` older than a minute in `load`. File: subagent_store.go.
- effort: S
- prior art: new

### P6C-R1-13  The slot/handle completion logic is duplicated between runHandle and startRemote
- severity: medium
- lens: D maintainability
- location: profile_message_async.go:94-226 (`runHandle`) vs subagent_remote.go:80-184 (`startRemote`); profile_message_async.go:235-266 (`asyncResult`) vs subagent_remote.go:246-278 (`remoteAsyncResult`); profile_message_async.go:269-297 and subagent_remote.go:30-57 (two drivers with the same Stop/Detail/latest); profile_message.go:419-425 vs subagent_remote.go:140-146 (inline path, already diverged, see 02); `blockFor` at profile_message_async.go:64 vs subagent_call.go:~170
- evidence: [read] about 130 lines of the same shape: id generation, `beginProfileMessage`, `Register`, `abandonProfileMessage`, Complete+MarkRead on start failure, `setProfileMessageRun`, goroutine waiting `Done`, double `select` on `ended`/timer/ctx, "still running" rendering, state mapping (Stopped/TimedOut/Lost/Failed). Divergences already present: `runHandle` registers `Queued: true` and calls `seedActivity`, `startRemote` does neither; `runHandle` waits for `Started()` first; inline cancel differs (02).
- suggested fix: one `followHandle(kind, run, ...)` helper and one state-mapping function; keep the two thin callers. Files: profile_message_async.go, subagent_remote.go.
- effort: M
- prior art: new

### P6C-R1-14  Size and shape: two files near/over 700 lines, six functions near 90-105 lines
- severity: low
- lens: D maintainability
- location: subagent_manager.go (914 lines: run struct, manifest conversion, cancel family, loadText, start/execute/finish); subagent.go (726: tool build, MCP binding, agent-list text, child tool selection, argument normalization); functions: `start` subagent_manager.go:481-587 (106), `startRemote` subagent_remote.go:80-184 (104), `startRun` subagent_call.go:61-160 (99), `runHandle` profile_message_async.go:94-188 (94), `newSubagentToolRemotes` subagent.go:~210-302 (93), `renderRun` subagent_call.go:~255-344 (89)
- evidence: [read] line counts from the files; the manager file mixes lifecycle, persistence adapter (`manifest`, `restoredSubagentRun`, `loadText`) and cancellation.
- suggested fix: move manifest/restore/loadText to subagent_store.go (or subagent_run.go), the cancel family to subagent_cancel.go, and the MCP binding plus agent-list text out of subagent.go. No logic change. Files: subagent_manager.go, subagent.go.
- effort: M
- prior art: new

### P6C-R1-15  Dead or test-only exported code and a seven-deep constructor ladder
- severity: low
- lens: D maintainability
- location: subagent_external.go:26-32 `SubagentManager.External` (no caller anywhere, tests included: grep for `External(` is empty); subagent_manager.go:841-861 `CancelOwnersWithPrefix` (no caller anywhere); :785-798 `CancelOwner` and :880-885 `Closed` (tests only); profile_message_remote.go:201 `stopCtx` field (declared, never used); subagent_manager.go:753 `stop` wrapper; subagent.go:155-190 `NewSubagentTool` -> `NewSubagentToolWithMCP` -> `NewSubagentToolWithManager` -> `NewSubagentToolWithProcesses` -> `newSubagentTool` -> `newSubagentToolAsync` -> `newSubagentToolRemotes` (9 positional params)
- evidence: [read] non-test callers: only `catalog.go` calls `newSubagentToolRemotes`; the other six exist for tests (grep of non-test files shows no use). `takeStopReason` (:295) does not take (it only reads).
- suggested fix: delete `External`, `CancelOwnersWithPrefix`, `stopCtx`; collapse the ladder into one options struct used by tests and `catalog.go`. Files: subagent_external.go, subagent_manager.go, profile_message_remote.go, subagent.go.
- effort: M
- prior art: new

### P6C-R1-16  Four near-identical cancel loops, and the host stop drops the stop reason
- severity: low
- lens: D maintainability
- location: subagent_manager.go:785-861 (`CancelOwner`, `CancelUnmanagedOwner`, `CancelUnmanagedOwnersWithPrefix`, `CancelOwnersWithPrefix`)
- evidence: [read] four copies of "select runs by predicate, cancel active ones, count". The two that production uses (`CancelUnmanagedOwner`, `...WithPrefix`) call `run.cancel()`, not `stopWith(reason)`, so a user stop of a non-handle run reports no `user_stop` reason, while a handle stopped through the async manager does (`app/resolver.go:180` vs `:213`, `:184`).
- suggested fix: one `cancelWhere(pred, reason)`; pass the resolver's reason through. Files: subagent_manager.go, app/resolver.go, app/session_catalog_files.go.
- effort: S
- prior art: new

### P6C-R1-17  Hand-rolled membership helpers and stale or garbled comments
- severity: low
- lens: D maintainability
- location: `containsName` subagent.go:362; `containsState` statecontext_counts.go:27; `remoteSettings.allowed` profile_message_remote.go:38; `remoteAgentSettings.allowed` subagent_remote.go:61 (all `slices.Contains`, already imported in profile_message.go:9); comments: `backgroundSafe` subagent.go:96-105 says "no native tool of the agent is limited" but the code checks three booleans only and ends with `if !x {return false}; return true`; `expandMCPWildcard` subagent.go:645-648 has a broken sentence ("A tool whose / a wildcard that matches no tool"); `subagentChildTools` subagent.go:~480 starts with a blank line; `startRun` depth check duplicated in `manager.start` (subagent_call.go:70-73, subagent_manager.go:497-500); `normalizeSubagentArguments` (subagent.go:~560) mutates its input map while `normalizeProfileMessageArguments` copies it.
- evidence: [read]
- suggested fix: use `slices.Contains`, fix the comments, drop the duplicate depth check from one place. Files: as listed.
- effort: S
- prior art: new

### P6C-R1-18  Test quality gaps
- severity: low
- lens: D maintainability
- location: 36 `time.Sleep` calls across subagent*_test.go and profile_message*_test.go (for example subagent_manager_test.go:184,207; profile_message_remote_test.go:325 sleeps 300 ms; subagent_remote_test.go:123); subagent_remote_test.go:223-226 (`running := f.tool.Definition.Name; running == ""` asserts nothing about running); subagent_remote_test.go:257-274 (global `runtime.NumGoroutine()` with a `before+4` allowance)
- evidence: [read] Missing for current code: depth gate on a remote agent (01), cancel of an inline remote call against a hung remote (02), nested-run owner in `RunTranscript`/transcript gate (03), launcher panic is logged (05), concurrent `Detail` on a restored run (08), `External`/`WithExternalRun` inside the native package (only remoteapi server_test with a fake), manifest `.tmp` cleanup (12).
- suggested fix: replace sleeps by channel handshakes; add the listed failure-path tests together with each fix. Files: the test files named.
- effort: M
- prior art: new

### P6C-R1-19  Every cross-profile sub-agent transcript read lists and parses all manifests
- severity: low
- lens: C performance
- location: internal/tools/native/transcript_path.go:149-155 (cross-phase, caller) calling subagent_store.go:224-238 (`subagentRunIDs` -> `load`: ReadDir + ReadFile + Unmarshal of every manifest)
- evidence: [read] `ids` is only used by `notFound`, but is computed unconditionally after `runOfTarget`, so a successful read of one run reads and parses all `keep_finished` manifests (default 50). See H3.
- suggested fix: compute `ids` lazily inside the `!found` branch. File: transcript_path.go.
- effort: S
- prior art: new

## 4. Performance hypotheses (for measurement)

- H1: `recordChange` rewrites and fsyncs the whole manifest per new changed file: file list grows, so bytes written are O(n²) and fsync count is n; all runs of a storage dir share one `subagentStore.mu`. subagent_changes.go:21-34, subagent_store.go:76-110.
  workload: writer child changing 100 / 1000 / 10000 distinct files (changed_files_max 100 default, 10000 max), persist_runs on, durability default; also 3 concurrent writer children.
  benchmark: propose `BenchmarkSubagentRecordChange` (new): persistent manager in `t.TempDir()`, call `run.recordChange` n times, report ns/op and bytes written; also the linear duplicate scan at :21 for n=10000.
  expected: super-linear in n; wall time dominated by fsync; contention across runs.
- H2: `finish` holds `m.mu` across manifest `os.Remove` and the wait for `store.mu` (04). subagent_manager.go:152-158,703-710.
  workload: max_concurrent 8, 8 children finishing while 8 others call `recordChange`; measure p99 of `start`/`list` latency.
  benchmark: new `BenchmarkSubagentManagerFinishUnderSave` (fake launcher, real store).
  expected: lock wait equals one fsync per contending save.
- H3: `subagentRunIDs`/`load` re-reads all manifests per transcript read (19) and per `transcripts` list. subagent_store.go:120-141,224-238, transcript_path.go:155.
  workload: keep_finished 50 and 500, repeated cross-profile `transcript` reads of an existing run.
  benchmark: new `BenchmarkTranscriptSubagentOtherProfile` with N manifests.
  expected: linear in N per call; zero if `ids` becomes lazy.
- H4: `loadText` scans the whole `messages.jsonl` of a restored run, and runs again for each distinct restored run on `Detail`. subagent_manager.go:356-381, subagent_answer.go:60-88.
  workload: 20 restored runs with 50 MB and 500 MB transcripts, `async status` on each.
  benchmark: new `BenchmarkReadTranscriptAnswers` over a generated file (tool-result-heavy lines).
  expected: linear in file size, `bytes.Contains` over every line; reading the tail would be O(answer).
- H5: `CollectSubagentStateContext` takes a full `view()` of every retained run of the owner, copying each `files` slice, only to filter by state and read six fields. subagent_statecontext.go:50-55, subagent_manager.go:274-284, :864-878.
  workload: keep_finished 500, 100-10000 files each, one snapshot per compaction.
  benchmark: new `BenchmarkCollectSubagentStateContext`.
  expected: allocation proportional to runs x files; the sort adds n log n.
- H6: each remote start reads and parses the remote file up to 3 times (`ParseSubagent` entry callback, `directory.Entry`, `directory.Client`->`Load`), and `Client.Do` stats and reads the token file on every request including each poll. subagent_remote.go:91-95, profile_message_remote.go:122, remote/hub.go:46-60, remote/client.go:151.
  workload: 50 remote starts/min; token_file remote with poll_ms 100.
  benchmark: new `BenchmarkRemoteStartDirectoryLoad` against a stub server.
  expected: constant extra file syscalls per start/poll; small unless poll_ms is low.
- H7: the remote API builds a full tool catalog for every `StartSubagent` (cross-phase). app/remote_api.go:197.
  workload: 100 remote sub-agent starts/min, MCP servers configured.
  benchmark: new `BenchmarkRemoteStartSubagentCatalog`.
  expected: per-call catalog construction cost dominates start latency.
- H8: a rejected message (busy, queue full) still registers a handle and writes the record twice (Register + Complete) plus the index. profile_message_async.go:136-157.
  workload: burst of 200 profile_message calls to a busy target with max_queued 0.
  benchmark: reuse async baseline benchmarks in async_baseline_benchmark_test.go with a rejecting plan.
  expected: two atomic file writes per rejected message.

## 5. Cross-phase leads

- transcript_path.go:186-195: owner/session check misreads a nested run owner (03). transcript_path.go:149-155: eager manifest listing (19).
- app/remote_api.go:197 (catalog per call, H7); :292-294 (raw reason to peer, 11).
- app/subagent.go:206 (`MkdirAll(..., 0o755)`) and transcript/jsonl.go:173 (`0o666` before umask): child transcripts are readable by other local users under a default umask 022; check whether intended.
- remote/hub.go:96-100: followers run through `Hub.Go` with no recover (D-1 class); parses untrusted JSON, so a panic there would end the process.
- remote/entry.go:262-277, :314: error texts carry server paths (10).
- remote/client.go:151: token resolved per request (H6).

### 5b. Hardening, unreachable
- `restoredSubagentRun` leaves `ctx` nil (subagent_manager.go:341-354); `stopWith` reads `r.ctx.Err()` (:287), so a stop of a restored run would panic. Only reachable if an async row is live for an id whose manifest is already ended (crash between `store.save` and `async.end` in `finish`, plus asyncmgr not marking it lost). Set `ctx` to a cancelled context.
- `subagentTool.agentNames` (subagent.go) ranges `s.agents` without `s.mu` while `bindMCP` writes `agent.catalog` under it; Bind runs once at catalog build before use (resolver.go:600).
- `newSubagentRunID` has 32 random bits and `start` does not check `m.runs[id]` (collision overwrites a retained run and its manifest; about 1e-8 per start at 50 retained).
- `store.write` fsyncs the file but not the directory after rename.

## 6. Checked and sound

- `childExecutionContext`/`ForNested` (subagent_call.go:36-50, catalog.go:86-92): every run-scoped field carried; per-call identity and parent hooks cleared; manager overrides ProcessOwner/Limit/Async/AgentDepth/RootRun/OnFileChange; guarded by the field-walk test (execution_scope_test.go:100-140).
- Manifest load validation (id from file name, version, state, start time, Dir strictly inside storage root) and transcript delete confinement (`transcriptFolderUnder`).
- Foreign live server guard in `recover` and `foreignServerAlive` (pid + /proc start time).
- `start`/`undoReservationLocked` bookkeeping on the async-begin failure and closed-during-begin paths; `active.Add` happens under the same lock that checks `closed`, so `Close`/`Wait` cannot race an `Add`.
- Lock order: no cycle found among `m.mu`, `store.mu`, `run.mu`, `AsyncHost.mu`; asyncmgr calls drivers outside its lock.
- Remote client: no redirects, token scrubbed from errors, untrusted texts cut and stripped of control characters (`View.clean`, `errorOf`), response size limit, stop calls bounded by 3 s; remote reference parsing uses plain-name patterns, `.`/`..` impossible, the model supplies only the remote name.
- `localTarget` cross-session addressing: session regex + glob match + profile regex + deny list; unknown vs not-allowed answered identically; cycle and same-transcript refusals follow in the messenger.
- External run path: ctx cancel stops and awaits the run (`ownsRun`), observer channel buffered, panic boundary `callSubagentTool` present.
- Answer limits: `finish` cuts once (`cutUTF8`), `loadText` cuts, remote outcome cut by `ResultMaxBytes`, task preview 80 B, result rendering uses a content token.
- Known flaky tests did not fail in my single run.

## 7. Not covered

- Server side of the remote API (remoteapi/server.go) beyond the lines cited; asyncmgr internals except Register/Complete/Close/Stop; `async_latest.go`, `activityLatest`, `transcriptOf` (phase 6b/7); the Windows and non-Linux owner files beyond reading them; no benchmarks or race runs (forbidden); tests were read in part, not all 6.7k lines; no check of real provider error contents (11).
