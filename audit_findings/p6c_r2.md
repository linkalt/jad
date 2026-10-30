# Phase p6c — independent review (reviewer r2)

- **Phase:** p6c — native agent tools: `subagent*`, `profile_message*`, `statecontext_counts.go` (no test files in the finding scope; tests read as evidence).
- **Reviewer:** r2
- **HEAD:** 3bf8c33 (`Record the latest-block, transcript-index and close fixes in DECISIONS.md`)
- **Date:** 2026-11-01
- **Files read:** `backend-go/internal/tools/native/` `subagent.go`, `subagent_call.go`, `subagent_manager.go`, `subagent_async.go`, `subagent_answer.go`, `subagent_changes.go`, `subagent_external.go`, `subagent_remote.go`, `subagent_statecontext.go`, `subagent_store.go`, `subagent_owner*.go`, `profile_message.go`, `profile_message_async.go`, `profile_message_remote.go`, `statecontext_counts.go`, plus `async_host.go` (interplay, read as required by the prompt) and read-only context from `internal/tools/catalog.go` (`ExecutionContext`, `ForNested`), `internal/asyncmgr/{manager,query}.go`, `internal/remote/{hub,client,target,entry}.go`, `internal/app/{resolver,subagent}.go`, `internal/sessionfs/native_subagent_settings.go`.
- **Commands run** (see section 8 for exit codes): `gofmt -l internal/tools/native`, `go vet ./internal/tools/native/`, `go build ./internal/... ./cmd/...`, targeted `go test ./internal/tools/native/ -run ...`.

**Findings: 11 (0 critical, 0 high, 3 medium, 8 low).** Lens: A 6, B 1, C 0 (5 hypotheses), D 4. Medium: P6C-R2-01 (panic boundary), P6C-R2-02 (`Close` without deadline), P6C-R2-06 (changed-file list not byte-bounded, measured 201 KB). Performance hypotheses: 5. Cross-phase leads: 4.

## 1. Understanding note

`subagent` is one native tool (`subagentTool`) that (a) lists agents when `task` is blank, (b) starts a *run* owned by a `SubagentManager` (one per storage directory, shared by sessions, closed by the resolver), (c) starts a run on another JAD when `agent` is `remote:<name>/...`. A run is a goroutine (`SubagentManager.execute`) that calls `SubagentLauncher.Run` (implemented in `app/subagent.go`) with a child `ExecutionContext` derived from the caller's by `ExecutionContext.ForNested()` (`subagent_call.go:42-59`). A run that is not a writer may also be an async handle (`subagentAsyncHandle`), so it outlives the call; runs are owner-scoped by session key (depth 0) or by the parent run id (deeper). State survives a restart as one manifest per run (`subagent_store.go`, `.runs/`), and a run found going at startup becomes `lost`. Nested starts are bounded by `max_depth`, `max_runs_per_root`, `max_runs_per_session`, and are never queued.

`profile_message` is `profileMessageTool`: it resolves a local target (`<category>/<profile>` of the calling session, or `<session>/<category>/<profile>` allowed by `sessions` globs) or a remote target (`remote:<name>/...`), then asks the app's `ProfileMessenger` for a `ProfileMessagePlan`, starts it, and waits `block_ms`. Past `block_ms` the message is an async handle (`pm_...`, kind `profile_message`) owned by the session key (or the child run id). Remote runs (`remoteRun`, kind `remote_subagent` for `remote:` agents) are followed by polling on a `remote.Hub` goroutine and bridged to an async handle by a detached goroutine.

`statecontext_counts.go` holds the shared count/trim helpers used by the task, process and subagent state-context providers.

## 2. Hypotheses (assumed → verified / disproved)

| id | statement | status |
|---|---|---|
| H-a | The run goroutine has a panic boundary, as D-1 requires | **disproved** — see P6C-R2-01 |
| H-b | `Close` on the run manager returns before work stops (`176fd7d` class) | **partly verified** — the manager waits (`active.Wait`), but no deadline and no bound for the phase's own detached goroutines (P6C-R2-02, -03) |
| H-c | Locks are taken in an inconsistent order somewhere (`m.mu`/`store.mu`/`run.mu`) | **disproved** — order is `m.mu` → `s.mu` → `run.mu` everywhere; no path holds `run.mu` while taking either of the others |
| H-d | A manifest write can overwrite a terminal state (the P6BM-08 shape) | **disproved** — `subagentStore.save` snapshots under `store.mu`, so a later snapshot can never be written earlier |
| H-e | A start racing with `Close` can be missed and leak a handle | **disproved** — `m.closed` check, `m.active.Add(1)` and the map insert are all under `m.mu`, the same lock `Close` takes before `active.Wait` |
| H-f | Slot/queue counters (`running`, `queued`, `byRoot`, `bySession`) can drift | **disproved** — every increment has exactly one decrement (`undoReservationLocked` on start failures, `acquire` on both outcomes, `finish` when the goroutine ends) |
| H-g | `max_runs_per_root` is off by one | **disproved for the documented meaning** — `byRoot[root]-1` counts the runs *below* the root, which is what the setting documents; only a root that already ended is counted as one fewer |
| H-h | A child inherits per-call identity or a parent hook through `ForNested` | **disproved** — `childExecutionContext` clears `RequestInteraction`, `OnInteractionResolved`, `OnFileChange` and sets `AgentName`; the manager overwrites the process/depth/root fields; `TestSubagentChildExecutionContextKeepsTheRunScope` walks the fields |
| H-i | Untrusted sizes: an agent result or file list can exceed its limit | **partly verified** — the answer text is cut, the changed-file list is not (P6C-R2-06) |
| H-j | Remote hop counting can be bypassed by a nested or child call | **disproved** — `RemoteHops` survives `ForNested` and the manager's field overwrites, and both remote starts send `RemoteHops+1` |
| H-k | Cross-session addressing can escape `deny_profiles` or a glob | **disproved** — `localTarget` checks `DenyProfiles` and `SessionPattern` before any lookup, and the app resolves the name through `sessionfs.Resolve` |
| H-l | Persisted-run recovery can be pointed at a file outside the storage directory | **disproved** — `store.read` drops a `dir` outside the root and `transcriptFolderUnder` allows exactly one level, never `.runs` |
| H-m | A restored run can be stopped through the async manager | **disproved** — async `Stop`/`cancel` act on live rows only; see P6C-R2-05 for why the latent nil deref is unreachable |
| H-n | The manager holds a lock across file I/O on a hot path | **verified as a cost** — `store.save` holds `store.mu` across `CreateTemp`+`write`+`fsync`+`rename`; see H01 |
| H-o | `gofmt`/`go vet` findings | **none** (both exit 0) |

## 3. Findings

### P6C-R2-01  The run goroutine's panic boundary covers only the launcher call
- severity: medium
- lens: A correctness
- location: `backend-go/internal/tools/native/subagent_manager.go:585` (`go m.execute(...)`), `:589-632` (the goroutine body), `:643-650` (`runLauncher`, the only `recover()` in the phase), `:685-722` (`finish`)
- evidence: [read] `grep -rn 'recover()' subagent*.go profile_message*.go async_host.go` returns exactly one hit, `subagent_manager.go:645`, inside `runLauncher`. Everything else in the goroutine runs outside any recover: the two process/async sweeps (`spec.asyncHost.Manager().DeleteOwnersWithPrefix`, `stopRunProcesses`), the state switch, and `finish` — which is where all of the run's bookkeeping happens (result cut, `run.store.save`, `byRoot`/`bySession` countdown, the `finished`/`keep` prune, `releaseWriter`, `run.async.end`, `close(run.done)`). An unrecovered panic in a goroutine ends the whole process. The comment in `runLauncher` states the reason for its boundary ("the goroutine has no caller that could recover it"); the same reason applies to the remaining ~40 lines and was not applied to them. The existing test `TestSubagentLauncherPanicBecomesAFailedRun` (`subagent_manager_test.go:423`) covers the launcher half only.
- suggested fix: wrap the body of `execute` once (`defer func(){ if r := recover(); r != nil { finish the run as failed with the panic text } }()`, placed under `defer m.active.Done()` and after the `finish`-once guard) and drop the inner boundary, or keep both. File: `subagent_manager.go`.
- effort: S
- prior art: new instance of accepted item D-1 (`audit_audit.md`, "panic recovery (D-1)"), which was fixed in `tools/catalog.go` and `app/remote_api.go` but not in this goroutine.

### P6C-R2-02  `SubagentManager.Close` waits without a deadline inside the shutdown path
- severity: medium
- lens: A concurrency
- location: `backend-go/internal/tools/native/subagent_manager.go:888-898` (`m.cancel(); m.active.Wait()`), `backend-go/internal/app/resolver.go:1022-1040` (children closed serially, before processes, async hosts and MCP), `backend-go/internal/app/service.go:126-135` (`Service.Close` → `resolver.Close()`, before `Server.Shutdown` applies `server.shutdown_timeout_ms`)
- evidence: [read] `Close` sets `closed`, cancels the manager context and then `m.active.Wait()` — there is no timeout and no kill path (unlike `AsyncHost.waitShellRuns`, `async_host.go:110-122`, which waits at most the run's kill grace, and `ManagedProcessManager.Close`, which is bounded by `kill_grace_ms`). The resolver calls it for every sub-agent manager *before* the process managers and the async hosts, and `Service.Close` calls the resolver before the HTTP shutdown deadline exists, so one child whose launcher does not return costs the whole graceful shutdown. A child is a full session run: it can be blocked in a `fetch`, an MCP tool call, or a nested sub-agent run whose own `Close` has the same property, so "returns promptly on cancel" is not structural — `session.Runner` only checks `ctx` at round boundaries (`runner.go:401,420,463,493,631`).
- suggested fix: bound the wait (`select` on a `time.After(grace)` after `m.cancel()`, log the runs still active with their ids, and return), the same shape as `AsyncHost.waitShellRuns`. File: `subagent_manager.go`.
- effort: S
- prior art: new; related to the accepted shutdown-deadline item D-3 (`DECISIONS.md` "Deleting a session releases its children; shutdown has a deadline"), whose deadline does not cover this close.

### P6C-R2-03  The phase's detached goroutines have no owner, no recover and nothing waits for them
- severity: low
- lens: A concurrency
- location: `backend-go/internal/tools/native/profile_message_async.go:172-186` (bridge goroutine), `backend-go/internal/tools/native/subagent_remote.go:150-158` (bridge goroutine), `backend-go/internal/tools/native/profile_message_remote.go:221-231` (`remoteRun.Stop` starts a goroutine with `context.Background()`), `backend-go/internal/tools/native/async_host.go:96-122` (`Close` waits for `shellRuns` only)
- evidence: [read] Three `go func()` sites in the phase are not registered anywhere: they are not in `AsyncHost.shellRuns`, not in the `remote.Hub` wait group, and carry no `recover`. Two of them wait on `<-run.Done()` and then call `host.Manager().Complete(...)` — i.e. they can still be running after `AsyncHost.Close` has closed that manager (the error is discarded). `remoteRun.Stop`'s goroutine uses `context.Background()` with `stopCallTimeout` (3 s), so it can outlive every close. This is the shape `176fd7d` fixed for the shell runs (`AsyncHost.Close` "returns before work stops"), left in place here; the consequence is bounded (a goroutine that ends within one stop timeout or when its run ends), not a hang, which is why this is low.
- suggested fix: run both bridges through `remote.Directory.Go`-style bookkeeping (or add them to a wait group `AsyncHost.Close` waits on, bounded by `stopCallTimeout`), and give each a one-line `recover`.
- effort: S
- prior art: `176fd7d` (fixed for `shellRuns`); new for these three.

### P6C-R2-04  A restored run's answer is cached cut to the first caller's limit
- severity: low
- lens: A correctness (bounds)
- location: `backend-go/internal/tools/native/subagent_manager.go:356-381` (`loadText` sets `textLoaded` before reading, stores `result.Text` cut to `limit`), called from `backend-go/internal/tools/native/subagent_async.go:135` with the async manager's `StatusSummaryBytes`; `backend-go/internal/asyncmgr/query.go:170-176`
- evidence: [read] `loadText(limit)` is the only reader of a restored run's transcript, and it marks the run loaded and stores the text already cut to that caller's limit. `asyncmgr.Status` passes `cfg.StatusSummaryBytes`, `wait`/`Detail` pass a different limit, so a `status` (or a `list` that details) that arrives first pins the text to the smaller limit for the rest of the process; the later, larger read gets `truncated: true` on a shorter text. Same ordering class as the phase-6a `decide_systemone` "redact before cut" fix.
- suggested fix: keep the uncut text (or the cut plus the limit that produced it) and re-cut per call. File: `subagent_manager.go`.
- effort: S
- prior art: new (ordering class seen in P6AM "redact the key before it cuts").

### P6C-R2-05  `subagentRun.stopWith` dereferences a nil context on a restored run (unreachable today)
- severity: low
- lens: A correctness
- location: `backend-go/internal/tools/native/subagent_manager.go:286-293` (`r.ctx.Err()`), `:341-351` (`restoredSubagentRun` never sets `ctx`), callers `subagent_async.go:105-109`, `subagent_external.go:38`
- evidence: [read] A restored run has `ctx == nil` and `cancel == func(){}`. Both `stopWith` callers reach any run the manager holds by id (`subagentDriver.find`, `External`), restored ones included, so `r.ctx.Err()` would panic. Both are guarded today: `asyncmgr.Manager.Stop` calls `driver.Stop` only for a non-terminal row (`query.go:185-200`) and `cancel` skips terminal and finishing rows (`manager.go:473-497`), and `External` handles are in-memory only (`app/remote_api.go:220`), so a restored id cannot arrive. Filed as *hardening, unreachable*; it becomes a crash the day an id is followed after a restart.
- suggested fix: `if r.ctx != nil && r.ctx.Err() == nil` in `stopWith` (one line).
- effort: S
- prior art: new.

### P6C-R2-06  The changed-file list in a sub-agent result is not byte-bounded (201 KB measured with the defaults)
- severity: medium
- lens: B security (limits on untrusted material reaching the model and `messages.jsonl`)
- location: `backend-go/internal/tools/native/subagent_changes.go:51-67` (`changedFilesInformation`: `strings.Join` of every path, no cut), rendered into the result at `subagent_call.go:267-356` (`renderRun`) and `:209-221` (`partialInformation`); the count default is `backend-go/internal/sessionfs/native_subagent_settings.go:25-28` (`changed_files_max` 100, max 10,000); the paths come from the child's own tool calls (`native/write.go:212`, `native/edit.go:229`, `displayPath` = workspace-relative)
- evidence: [measured] probe test added through `go test -overlay` (scratch only, `/tmp/r2/probe_changes_test.go`): 100 changed files (the default `changed_files_max`) with 2,005-byte relative paths render `changed-files result: 201795 bytes` through the real `changedInformation` + `tools.RenderToolResult`, i.e. **6.2x `result_max_bytes`** (32,768, the cap the *answer* is cut to in `finish`, `subagent_manager.go:686-689`). Nothing else bounds it: `RenderToolResult` has no size cap (`internal/tools/output.go:47-72`), and the async side already cuts the same list to 200 bytes (`subagent_async.go:14-15,59`), so only the tool-result path is open. [read] reachability: a writer child whose agent is marked "can change files" and writes many long-named paths; the result is persisted to the parent's `messages.jsonl` and re-sent on every later round.
- suggested fix: cut the joined list the way the async summary does (share the `changedFilesText` helper) or budget the paths against `result_max_bytes` and report `written_files_omitted`; one file, `subagent_changes.go`.
- effort: S
- prior art: new (the phase-6a/6b bounded-output work covered MCP results, validation reports and process logs, not this information value).

### P6C-R2-07  Files and one function above the size threshold, and a name that does not match its content
- severity: low
- lens: D maintainability
- location: `backend-go/internal/tools/native/subagent_manager.go` (914 lines), `subagent.go` (726 lines), `subagent.go:220-313` (`newSubagentToolRemotes`, 94 lines: schema/limit checks, remotes, agent catalogs, toolset binding, description, self-tool and nesting wiring), `subagent_owner.go` (holds `serverIdentity`, `foreignServerAlive`, `serverAlive` — shared with `process_orphan.go:26,47,106` and `async_host.go:63-72`, so the `subagent_` prefix misleads)
- evidence: [read] `wc -l`; function length from the symbol index (`newSubagentToolRemotes 220-313`). The manager file mixes run lifecycle, limits, owner fan-out, persistence recovery and listing.
- suggested fix: nothing structural (a split would be a rewrite); the smallest useful change is renaming `subagent_owner.go` to something neutral (`server_identity.go`) since three tools use it.
- effort: S
- prior art: new.

### P6C-R2-08  Two comments state rules the code does not apply
- severity: low
- lens: D maintainability
- location: `backend-go/internal/tools/native/subagent.go:99-104` (`backgroundSafe`: "the session turned `background_writers` on, the async tool is there to collect the run, **and no native tool of the agent is limited**" — the body checks only `settings.BackgroundWriters`, `host != nil` and `agent.config.ChangesFiles()`), `subagent.go:668-684` (`normalizeSubagentArguments`: "A negative block is no block", but a negative `block_ms` is clamped to `0`, which `blockFor` passes to `BlockDuration` and means "hand off at once", the opposite of the default block)
- evidence: [read] code beside the comment. Both are restatements of a rule that changed (the "limited tool" rule and the old "no block" meaning).
- suggested fix: correct the two comments (or apply the rule the first comment states, if it is still wanted — that is a decision, not a comment fix).
- effort: S
- prior art: new.

### P6C-R2-09  `agentNames` reads the agent slice without the lock that guards it
- severity: low
- lens: A concurrency
- location: `backend-go/internal/tools/native/subagent.go:707-713` (`agentNames`, no lock), called at `subagent_call.go:85`; the guarded writer is `subagent.go:559-618` (`bindMCP`, `s.mu.Lock()` + `agent.catalog = catalog`), and the field's documented purpose is "`mu` guards the agents' catalogs, which Bind fills in after the tool is built" (`subagent.go:136-139`)
- evidence: [read] `agentNames` ranges over `s.agents`, copying each struct *including* the `catalog` pointer that `bindMCP` writes under `s.mu`; `selectAgent` (same file, `:686-705`) takes the `RLock` for exactly this reason. Today the only caller is the "unknown agent" error path, and the catalog is published after `Bind`, so I could not build a call that lands in the window; a `-race` run with a call issued while the session's MCP binding runs would report it.
- suggested fix: take `s.mu.RLock()` in `agentNames` (as `selectAgent` does).
- effort: S
- prior art: new.

### P6C-R2-10  Test quality: sleep-based synchronization, and three untested gaps this review found
- severity: low
- lens: D maintainability
- location: sleeps used as synchronization where the package already has `eventually` (`subagent_background_writer_test.go:55-65`) and `callReturnsWithin` (`:66-77`): `subagent_manager_test.go:184` and `:207` (sleep 20 ms, then the assertion requires the run to have started: `cancelled.Load() != 1` fails on a loaded machine), `subagent_test.go:469` (`time.Sleep(50ms) // give a third child the chance to start wrongly`, a negative assertion on a fixed sleep), `subagent_remote_test.go:123,269`, `profile_message_remote_test.go:325` (`time.Sleep(300ms)`). Missing failure-path tests: no test panics after the launcher (P6C-R2-01), none bounds `written_files` (P6C-R2-06), none closes the manager with a launcher that ignores `ctx` (P6C-R2-02).
- evidence: [read] the listed lines; `TestSubagentManagerCloseStopsDetachedChildrenAndWaitsForThem` and `TestSubagentManagerCloseWithDetachedRunsLeavesNoGoroutines` exist and do cover the happy close path. The two known-flaky tests (`TestRemoteProfileMessageCloseEndsFollowersWithoutLeaks`, `TestRemoteSubagentCloseLeavesNoGoroutines`) passed for me (section 7).
- suggested fix: replace the four fixed sleeps with the existing `eventually`/`callReturnsWithin` helpers and add the three tests.
- effort: M
- prior art: the p6b batch consolidated test waits in the other native tools (`6e63a51`, P6BM-17/18/19/22); these files were not part of it.

### P6C-R2-11  Duplicated helpers inside the phase
- severity: low
- lens: D maintainability
- location: `subagent_remote.go:61-68` `remoteAgentSettings.allowed` vs `profile_message_remote.go:38-45` `remoteSettings.allowed` (identical); `profile_message_async.go:273-282` `profileMessageDriver.latest` vs `subagent_remote.go:38-43` `remoteSubagentDriver.latest` (same body); `subagent_changes.go:51-67` `changedFilesInformation` vs `:71-87` `changedFilesText` (same join loop, one cut, one not — the cause of P6C-R2-06); `subagent.go:482-496` + `profile_message.go:298-315` (two "list the remotes" description builders); `remoteProblem`/`startError`/`remoteCode`/`remoteMessage` (`profile_message_remote.go:143-186`) reused unchanged by the sub-agent path (`subagent_remote.go:196-229`), which is the good direction.
- evidence: [read] side-by-side comparison of the listed ranges.
- suggested fix: one `allowed([]string, string) bool` and one cut-then-join helper for the file list; leave the description builders alone (they are per-tool wording).
- effort: S
- prior art: new.

## 4. Performance hypotheses for measurement

- **H01: one fsync per changed file, serialized across every child of a storage directory.** `subagent_changes.go:19-35` calls `run.store.save(r)` on **every** changed file, and `subagent_store.go:75-114` holds `store.mu` across `MkdirAll` + `CreateTemp` + `write` + `Sync` + `Close` + `Rename`. The store is per storage directory, so children that share it (`max_concurrent` 3 by default) serialize on that fsync; `setDir` (`subagent_manager.go:321-326`) and `finish` (`:685-700`) add two more per run.
  workload: one writer child writing 100 files (`changed_files_max` default), 3 concurrent writers on one storage dir, spinning disk or a network mount.
  benchmark: proposed `BenchmarkSubagentRecordChange` (100 `recordChange` calls on a manager with a temp storage dir, `-benchmem`) plus the same with 3 goroutines; expected effect: wall time scales with the file count and is multiplied by the number of concurrent children.
- **H02: a whole-transcript scan to find a restored run's answer.** `subagent_answer.go:58-88` reads `messages.jsonl` line by line with `bufio.Reader`, tests `bytes.Contains` then `json.Unmarshal` per candidate line and unmarshals the envelope again inside `SubagentAnswerText`.
  workload: a child transcript of 1 MiB and of 8 MiB (`latest_scan_max_bytes` scale), first `async status`/`wait` after a restart, `keep_finished` 50 restored runs.
  benchmark: proposed `BenchmarkReadTranscriptAnswers` over a fixture with N tool-result lines and one final `response.completed`; expected: linear in transcript bytes, once per restored run per process.
- **H03: `recordChange` is O(files) per change.** `subagent_changes.go:21-26` scans `r.files` under `run.mu` for every change, so n writes cost n²/2 comparisons.
  workload: `changed_files_max` raised to 10,000 (the ceiling) or 1,000, a child doing a bulk edit loop.
  benchmark: proposed `BenchmarkRecordChange` at 100/1,000/10,000 files; expected: quadratic; a `map[string]struct{}` beside the slice removes it.
- **H04: three reads of the remote's entry file per remote call.** `subagent_remote.go:88-101` (and `profile_message_remote.go:73-85`): `remote.ParseSubagent` calls the `Entry` callback, then `Entry(name)` is read again, then `Directory.Client(name)` (`remote/hub.go:59-73`) loads it a third time; each is a file read plus a JSON unmarshal.
  workload: one remote tool call per round in a long session (hundreds of calls), remotes folder on a network mount.
  benchmark: proposed `BenchmarkRemoteEntryLoad` (the existing loader) ×3; expected: microseconds per call, only visible as an extra 2 file reads per call.
- **H05: the run list copies every run of the storage directory on every snapshot.** `subagent_manager.go:756-783` (`ownedRuns`/`allRuns`/`list`) walks `m.runs` and `run.view()` copies the file list for every run, on each state-context snapshot and each `subagent({})` list.
  workload: `keep_finished` 1,000 (the ceiling) × 4 sessions sharing one storage dir → 4,000 runs scanned, `files` copies included.
  benchmark: proposed `BenchmarkSubagentList` (1,000 finished runs) with `-benchmem`; expected: linear in retained runs per snapshot; no index by owner.

## 5. Cross-phase leads (outside this phase's files; not filed as findings)

1. `backend-go/internal/app/subagent.go:203-215` — the child's directory name is `unsafeDirectoryCharacters.ReplaceAllString(request.CallID, "_")`: a provider call id longer than ~250 bytes makes `MkdirAll` fail with `ENAMETOOLONG`, which reaches the parent as `subagent_failed`. Sanitized but not length-bounded (phase 7 / app).
2. `backend-go/internal/remote/hub.go:80-92` — `Hub.Go`'s goroutine wrapper has no `recover`, and this phase's `remoteRun.follow`/`conclude` (`profile_message_remote.go:233-290`) run inside it: a panic there is a whole-server crash with no boundary (D-1 instance outside the phase).
3. `backend-go/internal/asyncmgr/query.go:147-182` — `Status` calls `driver.Detail` on every call, which is what makes P6C-R2-04 (the cached cut) observable and re-reads a restored transcript once; the entry/detail sizing rules live in `asyncmgr`.
4. `backend-go/internal/app/subagent.go:96-100` — `WithChildGroups(ctx)` + `defer sweepChildren()` owns the child's process-group sweep at run end (phase 6b); this phase only calls `stopRunProcesses` for processes the manager can tell apart (`subagent_manager.go:633-641`).

## 6. Checked and sound (one line each)

- No lock-order inversion: `m.mu` → `store.mu` → `run.mu` in every path (`finish`, `dropOldestLocked`, `save`, `recordChange`, `setDir`, `loadText`); no goroutine holds `run.mu` while taking `m.mu` or `store.mu` (paths: `subagent_manager.go:589-722` run goroutine, `:685-722` `finish`, `:152-170` `dropOldestLocked`, `subagent_changes.go:19-35`, `subagent_manager.go:321-326`, `:356-381`).
- A stale manifest cannot replace a terminal one (the P6BM-08 shape): `store.save` takes the snapshot and writes under `store.mu` (`subagent_store.go:73-86`), so writes are ordered by snapshot time.
- `Close` vs `start` cannot lose a goroutine: `m.closed`, `m.active.Add(1)` and the map insert are under `m.mu`, the lock `Close` holds before `active.Wait` (`subagent_manager.go:477-585`, `:888-898`).
- Slot, queue and per-root/session counters balance on every path (`undoReservationLocked`, `acquire`, `finish`); a queued run that is cancelled ends `stopped` and never leaks a slot.
- `max_runs_per_root` matches the documented meaning ("runs below one top-level run"); the `-1` is the root itself, and a root that already ended just relaxes the bound by one.
- A nested run is never queued (`errSubagentNoSlot`), so a parent holding a slot cannot deadlock behind its own child.
- Child `ExecutionContext`: per-call identity cleared, the caller's interaction/`OnFileChange` hooks removed, `PathScope` narrowed by `narrowScope`, and `ProcessOwner`/`AgentDepth`/`RootRun`/`ProcessAsync` re-set by the manager before the child starts (`subagent_call.go:42-59`, `subagent_manager.go:571-578`); `TestSubagentChildExecutionContextKeepsTheRunScope` pins it.
- Remote hop counting: `RemoteHops` survives both `ForNested` and the manager's overwrites and both remote starts send `RemoteHops + 1` (`subagent_remote.go:127`, `profile_message_remote.go:80`); `TestRemoteSubagentHopsAreSentAndLimited` and `TestRemoteProfileMessageHopsAreSentAndTheRemoteLimitIsReported` cover it.
- Owner scoping holds: runs are listed/stopped by owner (`ownedRuns`, `CancelOwner*`), handles are checked by owner in `asyncmgr`, and a nested run's handles are removed at its parent's end (`execute`, `DeleteOwnersWithPrefix(run.id, …)`).
- Cross-session addressing cannot escape the rules: `localTarget` checks `localProfilePattern`/`remote.SessionPattern`, rejects `.`/`..`, applies `deny_profiles` to the target and requires a `sessions` glob match before any lookup (`profile_message.go:377-395`), and the app resolves the name through `sessionfs.Resolve` (`app/profile_message.go:167-177`).
- A remote refusal never leaks the token: `remoteMessage` maps known codes to fixed texts and the loader messages name files/keys only (`profile_message_remote.go:143-190`); `TestRemoteProfileMessageBusyAndTokenErrorsNeverContainTheToken` covers it.
- Recovery is bounded and cannot be aimed outside the storage directory: manifest validation (`subagent_store.go:156-180`), one-level delete rule (`:193-199`), a foreign live server's runs are left alone (`subagent_manager.go:118-133`).
- Secrets: no new secret is printed by this phase; `boundedReason` cuts error text to 400 bytes, `safeRemoteText` to 400 and one line.
- `gofmt -l internal/tools/native` prints nothing; `go vet ./internal/tools/native/` is clean; the two known-flaky tests passed on the run recorded in section 7.

## 7. Not covered, and why

- **No `-race` run** (forbidden by the plan), so P6C-R2-09 and the general concurrency claims are `[read]` only.
- **No benchmark** (`-bench` is forbidden): section 4 lists what to measure instead; every C item is `[unmeasured]`.
- **The remote API server side** (`internal/remoteapi`, token table, hop refusal, `/api/remote/v1` routes) is phase-agnostic infrastructure named in DECISIONS "Remote JAD calling"; I read only what the client side of this phase calls (`remote.Client.StartRun/Follow/StopRun`, `remote.Error`, `Hub.Go`). Token-table behaviour, globs and `max_hops` enforcement were not re-audited here.
- **The app-side halves** (`app/profile_message.go`, `app/profile_message_queue.go`, `app/subagent.go`, `app/remote_api.go`, `app/resolver.go`) were read only where this phase depends on them (plan/messenger contract, launcher, manager lifecycle, shutdown order); they are not reviewed as such.
- **`asyncmgr` internals** (record pruning, wait semantics, `latest` budget arithmetic) belong to phases 6b/7 and were read only as far as needed to judge stop/report ordering.
- **Windows and non-Linux owner behaviour** (`subagent_owner_windows.go`, `_other.go`) read, not exercised.
- **The `tool_smith` bridge and `process` manager** are 6b/7; `stopOwned` and `DeleteOwnersWithPrefix` were traced for ordering only.

## 8. Commands run (from `backend-go/`, shared machine)

| command | result |
|---|---|
| `gofmt -l internal/tools/native` | no output, exit 0 |
| `go vet ./internal/tools/native/` | no output, exit 0 |
| `go build ./internal/... ./cmd/...` | no output, exit 0 |
| `go test ./internal/tools/native/ -run 'TestSubagent' -count=1` | `ok jad/backend-go/internal/tools/native 5.165s`, exit 0 |
| `go test ./internal/tools/native/ -run 'TestProfileMessage\|TestRemote' -count=1` | `ok jad/backend-go/internal/tools/native 4.656s`, exit 0 |
| `go test -overlay /tmp/r2/overlay.json ./internal/tools/native/ -run TestProbeR2ChangedFilesResultSize -count=1 -v` | `PASS`; logged `changed-files result: 201795 bytes for 100 paths of 2005 bytes (result_max_bytes default 32768)`, exit 0 |

The probe was a scratch test added through `go test -overlay` (`/tmp/r2/probe_changes_test.go` + `/tmp/r2/overlay.json`); nothing was written inside the repository, and `/tmp/r2/` was removed at the end. `go build ./...` was not used (the repo's `backend-go/CLAUDE.md` forbids it); `go build ./internal/... ./cmd/...` is the documented equivalent.
