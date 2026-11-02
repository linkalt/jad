# Scale inventory: coverage-gap pass (step 1)

Read-only, HEAD abefc5f. Trace base dc32287 (the file's creation, 2026-11-02 14:47:16 +0800). Commits after it are traced below. Nothing measured. Closes the "Not verified" gaps of the scale inventory (scale_1000_runs_inventory.md). Corrections to earlier rows are stated in each part.


## Commit trace since dc32287 (29 commits)

Scope: the commits whose hunks were checked directly are described by their cited-line effect. Commits that touch no cited file are "no cited row". Commits that touch a cited file but were not checked line by line are marked "not verified" and are listed under Unverified shifts. Statuses are unchanged.

| Commit | Subject | Cited rows |
|---|---|---|
| 1959d19 | scale: add env-gated measurement tests for the top 1000-run blockers | no cited row (no cited file in this commit) |
| 534f1a0 | tools/native: add a bash process seam so spawn and pipe failures test without a  | B-20 cites `bash_child.go:72` (`runBashCommand`); hunks at new 19-24, 96, 100, 114, 123 move it; line not touched |
| b92b089 | transcript, app: usage scan reads a snapshot outside the journal lock | A23 cites `read_records.go:13-24`, `:17-23` (not touched; hunk at new 26-41 sits after them). Its `catalog.go` and `jsonl.go` hunks do not hit a cited line |
| c8f74a7 | tools, session, app: process-wide leaf tool budget (B-01, partial) | B-29 `runner.go:396-513` etc. (hunks at new 138-140 and 656; touched by 656 only through the `executeToolCalls` call); C-26 `subagent.go:46-119` (hunk at new 96 is inside the range) |
| c6dac0e | tools, app: guard leaf-budget opt-in on wrappers; deadlock completion tests for  | no cited row (no cited file in this commit) |
| 67c4af7 | DECISIONS: record the B-01 goroutine goal as not met; worker set not built | no cited row (no cited file in this commit) |
| 9548409 | tool_batch: correct the goroutine comment; every call has its own goroutine, onl | no cited row (no cited file in this commit) |
| 536d113 | bash_child: drop the reader waiter goroutine; the last reader closes readersDone | B-20 cites `bash_child.go:72`; `runBashCommand` stays at 72 (checked before and after); hunks at 160-168 and 288-294 are below it. No shift, no touch |
| 83eb386 | profile_message: merge the non-queued completion and watch goroutines into one | not verified: touches a cited file; see unverified shifts |
| 45a9df3 | DECISIONS: record the deferred goroutine spawn removals | no cited row (no cited file in this commit) |
| a3ce7cb | profile_message: replace the queued watch goroutine with context.AfterFunc; one  | not verified: touches a cited file; see unverified shifts |
| 968e14d | DECISIONS: record the deferred site C spawn removal and its ordering hazard | no cited row (no cited file in this commit) |
| a9828ca | httpapi: add a labelled run-layer goroutine baseline measurement (JAD_SCALE_MEAS | no cited row (no cited file in this commit) |
| 4bd4b47 | DECISIONS: record the deferred proposal A async-state publisher and its load-mea | no cited row (no cited file in this commit) |
| bf06c05 | DECISIONS: record the deferred proposal B remote watcher end hook and its seam | no cited row (no cited file in this commit) |
| 551a06e | DECISIONS: close B-01 for this round; record the measured state and the deferred | no cited row (no cited file in this commit) |
| 33caf30 | httpapi: add a labelled async-state load measurement, per-run loop vs shared tic | no cited row (no cited file in this commit) |
| 971d13d | asyncmgr: add a labelled lock-hold measurement for the notify and SnapshotLive p | no cited row (no cited file in this commit) |
| ba7b6ca | asyncmgr: add a labelled SnapshotLive hold measurement per owner (JAD_SCALE_MEAS | no cited row (no cited file in this commit) |
| 3586902 | httpapi: add a labelled publish-latency measurement, per-run loop vs shared publ | no cited row (no cited file in this commit) |
| b7b5f4b | DECISIONS: defer seam A on its stop condition; close the reopened B-01 goal as n | no cited row (no cited file in this commit) |
| c21d26e | httpapi: run a profile run's OnEnd hook after the transcript lock is released | C-24 cite `registry.go:694-704` shifted: `cacheContent` moved 691 to 694 (checked before and after); cited lines not edited |
| cc43f1f | app: time out a remote message run with a timer and end it in OnEnd, not a watcher goroutine | A34 cite `remote_api.go:401` shifted: `SetOnClosed` moved 377 to 401 (checked before and after); `:36-48` not edited |
| 83a3581 | DECISIONS: record seam B as implemented (httpapi OnEnd, remote timer), limited t | no cited row (no cited file in this commit) |
| f17923b | DECISIONS: record the measured N=1000 remote watcher saving | no cited row (no cited file in this commit) |
| 648d46a | remote_message: test that arm after the run ends starts no timer | no cited row (no cited file in this commit) |
| 783b849 | DECISIONS: record the reproduced run-layer baseline (202/1002/2002) | no cited row (no cited file in this commit) |
| 65202d4 | audit: correct the A28 row (maps are keyed by storage folder, not session) | no cited row (no cited file in this commit) |
| 9858032 | audit: note the commits that changed rows' cited code since dc32287 | no cited row (no cited file in this commit) |

## Unverified shifts and open items (dc32287..HEAD)

Status: open. Nothing in this list has been checked against a cited line. Each item needs a decision or a line-tracking check before the table is final.

**Unverified shifts** (cited code in a changed file; commit not checked line by line):
- `534f1a0`, `b92b089`, `c21d26e`, `cc43f1f`: the shifts listed in the table were checked for the specific cited lines named in each row. Other cited rows in the same files are not checked. `536d113` was checked for `bash_child.go:72` only.
- `c8f74a7`: shifted rows not checked one by one: A22, A24, A28, A30, A37, A38, B-20, B-21, B-22, B-23, B-28, B-30, C-20, C-23, C-24, C-26, bounds rows 17, 25, 26, 27. Only B-29 and C-26 were checked directly.
- `83eb386`, `a3ce7cb`: touch `profile_message_queue.go`, which A33 cites (`:37-50`, `:67-89`, `:49`, `:87`). Not checked line by line.
- `9548409`: touches `tool_batch.go`. No doc row cites that file; listed as no cited row by path.

**Over-claimed impact notes (not changed in this edit):**
- A33: the note says "impacted by a3ce7cb and 83eb386". Their hunks are at `profile_message_queue.go:315-557`, outside the cited `:37-50`, `:49`, `:67-89`, `:87`. Over-claimed.
- A34: the note says "impacted by cc43f1f". Its hunks are at `remote_api.go:221-326`, outside the cited `:36-48`, `:84-103`, `:401`, `:411`. Over-claimed.

**Row 17 (open):** `read_records.go:29-40`, `:33-39` (A23). At HEAD those lines are `ReadRecordsSnapshot`; the row's context is `ReadCompleteRecordsRange` (HEAD 44-56). Needs a decision on which function the row means.

**checkpoint/service.go cites (open):** `service.go:146-210`, `:180-320`, `:320-470`, `:470-530`, `:520-720` (B-28 and related text) describe `checkpoint/service.go`, not `internal/app/service.go` (136 lines at HEAD). They cannot be resolved against the changed `app/service.go` and were not checked against any commit.

## Part A gaps (reviewer0)

# Gap closure, Part A (HEAD abefc5f, read-only)

Scope: the gap list from Part A's "Not verified". Index tools first; `read.sh` for exact ranges (named below).
Paths are relative to `backend-go/`. Part A's header said HEAD 24f005a; line numbers below are at abefc5f.

Row IDs continue from A22.

| ID | Location | Shared thing | Failure at 1000 active runs; trigger | Severity | Design change | Breaking? | Evidence |
|---|---|---|---|---|---|---|---|
| A22 | `internal/transcript/session_turn.go:12-16` (`s.mu` at `:16`), scan at `:27` (`journal.ReadLines`, which repairs the tail under the journal lock, `jsonl.go:246-248`) | `TurnStore.mu` held across `journal.ReadLines` for the whole transcript in `NextSessionTurn` | Full-transcript scan under the store lock, blocking `BeginTurn`, `AppendInputItemsAt`, `ReadRecordsRange`, and maintenance on that session. The scan also repairs the tail. Trigger: a method with no production caller. | no finding (dead in production) | None needed; delete or leave. The app path uses its own closure (`bound_runner.go:44-54`, `scanTranscriptIndex`), which is an index read, not a scan. | No | `grep NextSessionTurn` gives only `session/runner.go:201` (fallback when the option is nil) and the definition. `session/runner.go:529` and `app/bound_runner.go:96` always pass a non-nil closure, so the store method is unused on the app path. Retracts the part of A4/A17 that claimed the full scan runs per run start. |
| A23 | `internal/transcript/read_records.go:13-24`, `:29-40` | `s.mu` held only to read the `recovering` bool; then `journal.ReadRecordsRange` runs unlocked on the store | None. The lock is short; the scan itself is the journal's lock (A4). The `recovering` flag makes reads fail fast during `Recover`, not wait. | no finding | None. | No | `read_records.go:17-23`, `:33-39`. Confirms A4 is the real holder, and the `recovering` gate does not add a second wait. impacted by b92b089: `ReadRecordsSnapshot` (`:29-40`) is a second read path that holds no journal lock; the `recovering` check is the same. |
| A24 | `internal/transcript/run_error_cache.go:45-80`, `:86-91` | `s.mu` held across read, sort, and atomic rewrite (`writeJSONAtomic`) of the whole cache file on every failed run | Bounded size (`maxEntries`, config default 50, max 10000). Under the store lock, one file write per failure. Trigger: a burst of failed runs in one session. Blocks that session's `BeginTurn` for the write. | minor | Write outside `s.mu` (read-modify-write needs its own small lock). | No | `run_error_cache.go:50-72`. Config bound: `session_turn.go` consts `defaultRunErrorCacheMaxEntries=50`, `maxConfiguredRunErrorCacheMaxEntries=10_000`. `RecordRunError` is called from `bound_runner.go:257-261`. Not a memory leak: the file is capped. |
| A25 | `internal/transcript/input_history.go:39-88` (`AppendInputItemsAt`; lock `:43`, journal append `:80`), `:130-140` (`ReadInputItems`) | `AppendInputItemsAt` holds `s.mu` for the whole append (canonicalization and `journal.AppendAt` at `:80`; the journal fsyncs under strict durability). `ReadInputItems` does not take `s.mu`; the compaction-cache read uses the journal lock | Appends hold the store lock during fsync. A full `ReadInputItems` (no handle) expands the whole transcript with no memo; each call rebuilds from byte 0. Trigger: any caller of the handle-less reader in a loop. Production callers: `state_context.go:55,156` only. | degrades (per session) | Do the canonicalization before `s.mu`; keep the lock only around the journal append. | No | `input_history.go:43,80`; `state_context.go:55,156` call `readInputItems(nil)`. `InputHistory` (memo, `input_history_memo.go`) is the per-run path and is bounded by handle lifetime (its own doc comment). |
| A26 | `internal/transcript/input_history_memo.go:65-110` (`readTranscriptItemsMemo`) | `withRepairedFile` holds the journal lock for the whole incremental expansion, and `result = make([][]byte, len(memo.items))` copies the whole item slice on every read | Per-run cost is O(total items) per read, even when only the tail changed. Memory per handle is the expanded transcript, released by `Release()`. Trigger: long transcript, many rounds of a run (B-17 in Part B). Memory is per run, not global. | degrades | Return a slice view of the retained items (safe because items are read-only per the doc comment), not a copy. | No | `input_history_memo.go:65-110`; doc comment at `:14-22` says items are shared. |
| A27 | `internal/app/session_catalog_files.go:103-167` (`releaseSessionChildren`) | Delete ordering: map entries are removed under `r.mu`; the slow `Close()` calls run after `r.mu.Unlock()` (`:147`) | None found. Ordering is correct: maps are cleared under the lock and managers are closed outside it. `forgetStores` (`:161-170`) also runs under `r.mu` but only deletes map keys. | no finding | None. | No | `session_catalog_files.go:103-167`, `:147` (unlock before the `Close` loops). Confirms the delete path does not hold `r.mu` across I/O (answers the gap-list concern about line 175). |
| A28 | `internal/app/resolver.go:83`, `:88` (`subagentManagers`, `asyncHosts`); writes at `:696` and `:786` | `asyncHosts` and `subagentManagers` are never removed from their maps. No `delete` on either map anywhere in `backend-go`. Entries are keyed by storage folder: the tool storage folder for `asyncHosts`, and the subagent storage dir for `subagentManagers`. Deleting a session does not touch them: `DeleteSessionConfig` (`session_catalog_files.go:70-88`) and `releaseSessionChildren` (`:103-157`) leave both maps alone | Bounded by the number of distinct storage folders, not by sessions, so a session delete does not add entries. Each entry holds an `asyncmgr.Manager` and a `SubagentManager`. The gap is a session with a custom `tool_storage_dir` that is deleted and never used again: its manager stays for the process lifetime. Not measured. | degrades (memory), small | Evict on session delete when no other session uses the folder, the same way `releaseSessionChildren` evicts mcp/process/store entries. | No | Verified by reading at HEAD 783b849, not measured. Corrects A2, which said these were removed on session delete; they are not. |
| A29 | `internal/app/session_catalog_files.go:103-167` vs `resolver.go:77` | `profileTurnCounts`: confirmed never deleted (only write at `catalog.go:352`, read at `:345`). Not reached by `forgetStores` or `releaseSessionChildren` | Grows with every profile path ever listed. Trigger: profile churn. | minor | Key on profile, drop on session delete. | No | `catalog.go:343-352`. Confirms A15. |
| A30 | `internal/app/locked_config.go:14-58`; `resolver.go:343`, `:411` | `lockedConfigs.entries` is removed by `take` (`:55-58`, deletes on every call) and by `drop` (`:45`) on run end. An unused entry is removed on the next `take` for that key | Bounded in time (1 s) and removed on every normal path; not a leak. A burst of distinct keys (A12) is still count-unbounded for one second. | no finding (count bound stays minor, A12) | None needed. Optional count cap. | No | `locked_config.go:34-58`; `resolver.go:337-344` (drop), `:411` (take). Retracts the leak concern; keeps A12 as minor. |
| A31 | `internal/app/list_settings_cache.go:33-60`; `catalog.go:221` | `listSettingsCache.entries` is pruned by `keepOnly(listed)` on every catalog list and dropped on session file delete (`session_catalog_files.go:82`) | Bounded by the number of session files on disk. Not a leak. | no finding | None. | No | `catalog.go:221`; `session_catalog_files.go:82`. |
| A32 | `internal/app/skipped_sessions.go:30-40`, `catalog.go:210` | `skipped.reasons` is deleted when a session resolves again (`resolved`) | Bounded by the number of broken session files. | no finding (A20 stays minor) | None. | No | `skipped_sessions.go:36-39`; `catalog.go:210`. |
| A33 | `internal/app/profile_message_queue.go:37-89` | `profileQueueSet.edges` is refcounted (`:37-50`, delete at `:49`); `queues` entries are deleted when `refs==0 && !pumping && len(waiting)==0` (`:82-89`) | Bounded by in-flight waiters. Not a leak. | no finding (A21 is about hot-target serialization, not memory) | None. | No | `profile_message_queue.go:37-50`, `:67-89`. impacted by a3ce7cb and 83eb386: the queued and non-queued wait paths changed; the `edges` and `queues` refcounts are unchanged at HEAD. |
| A34 | `internal/app/remote_api.go:36-48` (`remoteRunIndex`) | Four maps (`runs`, `byLocal`, `byID`, `byManager`). Removal is by `forget` (on run finish via the run's identity) and `forgetManager` (manager `SetOnClosed`) | Bounded by live remote runs as long as each run ends with `forget` or its manager closes. A run whose manager never closes and never reports `Done` stays. Trigger: remote runs that never finish (no run timeout reaching `forget`). Not reached by `releaseSessionChildren`. | minor | Also index by session so session delete can drop them. | No | `remote_api.go:36-48` (maps), `:84-103` (forget / forgetManager), `:401` (`SetOnClosed`), `:411` (`b.runs.forget`). Config bound on run lifetime: `run_timeout_ms` / `run_retention_ms` (see `internal/sessionfs/remote_api_settings.go:19`); I did not confirm those reach `forget` for every path. impacted by cc43f1f: the remote message path changed; the four maps and their forget paths did not. |
| A35 | `internal/app/usage.go:114-213` (`writeUsageCache` at `:203`, `writeUsageCacheCheckpoint` at `:114`) | Usage cache body: `statFile` (`:119`), `HashFileRange` 64 KB tail (`:135`), `json.Marshal`, `CreateTemp` (`:161`), write, `file.Sync()` when durability is not `process` (`:172`), `Close`, `os.Rename` (`:180`), and a directory `Sync` only when durability is `strict` (`:183`) | Cost is one fsync of a small file per advancing scan (plus one dir fsync under strict). The default durability is `turn` (`sessionfs/session.go:742`), so the file fsync runs by default. Runs inside the journal lock because `scanTranscriptIndex` calls it (A13). Temp file is removed by the deferred `os.Remove` (`:166`) on every path. Trigger: each `profile_message`/compaction scan that advances the offset. | degrades (throughput under the journal lock) | Write after releasing the journal lock (A13); keep the fsync. | No (cache schema stays v1) | `usage.go:114-180`, `:166` defer, `:183` dir sync; `session.go:742`. Corrects the gap: fsync is present, not missing. |
| A36 | `internal/app/usage.go:203-213` (`writeUsageCache` error path) | Failure is logged once per run of failures (`usageCacheFailed` Swap, `:209`) and never fails the caller | No risk: best-effort by design. | no finding | None. | No | `usage.go:203-213`. |
| A37 | `internal/app/service.go:27-30`, `:84-120`, `:150-160` (`recovering` chan) | Start-time storage recovery runs in one background goroutine; `Close` cancels it and waits (`service.go:124`). Nothing waits on `recovering` at request time | Recovery does not block listen or runs. Its cost is O(sessions) in one goroutine (C-20). Not a 1000-run issue. | no finding (C-20 stays minor) | None. | No | `service.go:91-108`, `:113-119`. |
| A38 | `internal/transcript/recovery.go:44-55` (store `recovering` bool) | Set under `s.mu` for the whole `Recover` (`:49`, reset in `defer` `:52-54`). Other operations (`BeginTurn` `turn.go:160`, appends `input_history.go:45`, reads `read_records.go:20,35`, maintenance `maintenance.go:61,671`, front compact `front_compact.go:53`, `session_turn.go:18`) return an error when it is set | Fail-fast, not wait. A caller that hits it gets `turn store recovery is in progress`. The app only hands out a store after `Recover` (`resolver.go:1008`), so the window is for a second caller of the same store during its build. | no finding | None. Note the error is returned to the user and is not retried. | No | `recovery.go:44-54`; `resolver.go:1004-1012`. |
| A39 | `internal/app/config.go:22-23`, `:85` (`delete(resolverEnvironment, apiTokenEnvironmentName)`), `:146`, `:172` | Package-level: `const` env names; `listenHostPattern` and `listenHostNumericLabel` are compiled regexes, read-only after init | No shared mutable state. The `delete` at `:85` is on a clone. | no finding | None. | No | `config.go:22-23`, `:146`, `:172`, `:85`. |
| A40 | `internal/app/remote_listen.go:101-103` (`maxUnixSocketPath`, `var netListen = net.Listen`) | `netListen` is a package-level function var (test seam) | Read-only after init in production. | no finding | None. | No | `remote_listen.go:101-103`. |
| A41 | `internal/sessionfs/roots.go:25-28`, `:466-469`; `internal/sessionfs/session.go:16-19` | Package vars: `rootIDPattern`, `sessionIDPattern`, `profileNamePattern`, `environmentNamePattern` (compiled regexes); `rootSettingKeys`, `rootEntryKeys` (slices) | Read-only after init. No shared mutable state. | no finding | None. | No | `roots.go:25-28`, `:466-469`; `session.go:16-19`. |
| A42 | `internal/sessionfs/catalog_files.go:449` (`categoryDirectoryMu`) | Package mutex, already noted as A19 | Confirmed as A19. Scope: held across category directory creation only. | minor (A19) | Per-category lock. | No | `catalog_files.go:449` (from the Part A index). |
| A43 | `internal/sessionlock/lock_unsupported.go:1-16` | Build-tagged fallback for platforms not in the unix/windows set. `tryAcquire` and `release` return an error on every call | On such platforms `Acquire` returns the error on the first try (no polling), so there are no waiters and nothing to scale. This is a platform gap, not a scale issue. | no finding | None. | No | `lock_unsupported.go:10-15`; `lock.go:103-133` (`Acquire`: the error path returns before the wait loop). |
| A44 | `internal/sessionlock/lock.go:17`, `:110` (`acquirePollInterval`, used by `Acquire` at `:103`) | `const acquirePollInterval = 25 * time.Millisecond`; no override anywhere in the package (only tests and the benchmark use it) | Not configurable at runtime. Confirms A10: each waiter polls every 25 ms, so 1000 waiters on one file is 40k wakeups/s. | minor (A10) | Blocking `flock` wait, or make the interval a field set by the caller. | No | `lock.go:17`, `:110`; `grep acquirePollInterval` gives only `lock.go`, `lock_test.go:171,190`, `lock_benchmark_test.go:27,54`, `wake_latency_benchmark_test.go:42`. |
| A45 | `delete(` sites in `internal/app` (27 total) | See the per-site table below | Summarized below. | see below | see below | No | `/tmp/gaps-A/deletes.txt`. |

## The 27 `delete(` sites in `internal/app` (non-test)

Bounds a map that would otherwise grow? Yes = map is bounded by the delete path; No = the map grows without a bound on that path.

| # | Site | Map | Bounds a growing map? |
|---|---|---|---|
| 1 | `config.go:85` | clone of env (local) | not a shared map |
| 2 | `skipped_sessions.go:37` | `skipped.reasons` | yes (resolved sessions) |
| 3 | `locked_config.go:45` | `lockedConfigs.entries` (drop) | yes |
| 4 | `locked_config.go:54` | `lockedConfigs.entries` (take) | yes |
| 5 | `list_settings_cache.go:48` | `listSettingsCache.entries` (drop) | yes |
| 6 | `list_settings_cache.go:58` | `listSettingsCache.entries` (keepOnly) | yes |
| 7 | `session_catalog_files.go:111` | `r.mcpManagers` (session delete) | yes, on session delete |
| 8 | `session_catalog_files.go:119` | `sessions` (inner, processSessions/storeSessions) | yes |
| 9 | `session_catalog_files.go:122` | `r.processManagers` | yes, on session delete (only if no other session uses it) |
| 10 | `session_catalog_files.go:123` | `r.processSessions` | yes |
| 11 | `session_catalog_files.go:131` | `sessions` (inner, storeSessions) | yes |
| 12 | `session_catalog_files.go:135` | `r.storeSessions` | yes |
| 13 | `session_catalog_files.go:139` | `r.taskStores` | yes, on session delete |
| 14 | `session_catalog_files.go:141` | `r.memoryStores` | yes, on session delete |
| 15 | `session_catalog_files.go:144` | `r.fetchStores` | yes, on session delete |
| 16 | `session_catalog_files.go:167` | `r.stores` (forgetStores) | yes, on session delete |
| 17 | `profile_message.go:236` | `callerRunTargets.runs` (forget) | yes (on run end, `bound_runner.go:126`) |
| 18 | `profile_message_queue.go:49` | `profileQueueSet.edges` | yes (refcounted) |
| 19 | `profile_message_queue.go:87` | `profileQueueSet.queues` | yes (refcounted) |
| 20 | `remote_api.go:114` | `remoteRunIndex.byLocal` | yes (on run forget) |
| 21 | `remote_api.go:115` | `remoteRunIndex.runs` | yes |
| 22 | `remote_api.go:117` | `remoteRunIndex.byID` | yes |
| 23 | `remote_api.go:120` | `remoteRunIndex.byManager` (inner) | yes |
| 24 | `remote_api.go:122` | `remoteRunIndex.byManager` | yes |
| 25 | `resolver.go:693` | `r.asyncBuilds` (build in flight) | yes (always removed) |
| 26 | `resolver.go:783` | `r.subagentBuilds` (build in flight) | yes (always removed) |
| 27 | `resolver.go:960` | `r.stores` (failed build) | yes (failed build only) |

Bounds that are missing (no delete on a map that grows with sessions):
- `r.profileTurnCounts` (`resolver.go:77`, written at `catalog.go:352`): **no delete at all** (A15/A29).
- `r.asyncHosts` (`resolver.go:88`, written at `:689`): **no delete** (A28).
- `r.subagentManagers` (`resolver.go:83`, written at `:779`): **no delete** (A28).
- `r.runners` per shared session (`resolver.go:1016`): not in `internal/app` deletes (A3, unchanged).

## Not verified

- No runtime measurement; every claim is from reading code at abefc5f. No tests, benchmarks, or load runs were executed.
- `internal/transcript/read_records.go` `ReadRecordsRange` body beyond the lock: this sits on `journal.ReadRecordsRange` (A4), which I did not re-read at abefc5f.
- `NextSessionTurn` is confirmed unused on the app path by grep, but the `session` package tests may use it; I did not check test-only callers.
- `InputHistory` memo: memory bound is per handle; I did not measure the expanded transcript size per run.
- `acquirePollInterval` is not configurable: confirmed by grep and the package layout, not by a test.
- `writeUsageCacheCheckpoint` fsync behaviour: the sync/skip conditions are from the code; the default is `turn` (`sessionfs/session.go:742`), which I read but did not trace through a deployment config.
- `remote_api.go` removal: I did not confirm that every remote run path reaches `forget` or `forgetManager` (for example, a run whose manager never closes and whose `Done` never fires). A34 is marked minor for that reason.
- `session_catalog_files.go` beyond 175: now read in full (1-218); the delete ordering finding is A27.
- `recovery.go` path in the gap list does not exist under `internal/app`; the `recovering` flag is in `internal/transcript/recovery.go`, and the start-time recovery chan is in `internal/app/service.go`. I covered both.
- Measured cost of anything above: none.

## Part B gaps (reviewer2)

# Scale gaps-B close-out — read-only (HEAD abefc5f)

Scope: Part B "Not covered" gaps only:
native glob/grep/ripgrep/task/memory bodies, fetch store quotas,
mcp content+load, imagestore+resize, checkpoint git handling,
session Runner below executeRun, process_manager per-call locking.
Index-first (`get_architecture`, `search_graph`/`search_code`, `get_code_snippet`)
+ exact-range `read.sh` fallback. No edits, no benchmarks, no load tests.
`trace_path` tried for `Runner.Run` → "function not found" (wrong qn); fell back to `search_graph` + `read.sh`.

Related inventory: `audit_findings/scale_1000_runs_inventory.md` (row IDs only).
Part B ended at B-19; this continues at B-20.

## Findings (B-20 … B-30)

**B-20 glob/grep/ripgrep per-call process+FD+goroutine fan-out, no global budget — degrades (blocker if model fans out).**
`backend-go/internal/tools/native/glob.go:204-353` (`executeNativeGlob`),
`grep.go:103-185` (`executeNativeGrep`),
`ripgrep.go:79-252` (`runRipgrep`), `254-333` (`readRipgrepEvents`).
Shared: OS processes + pipes + reader goroutines + timers; no process-wide semaphore.
`glob` builds a `bashOutputCollector` bounded by `MaxBytes` (`glob.go:228-230`) then `runBashCommand` (same child path as `bash_child.go:72-` 2–3 `os.Pipe` + fork per B-10; impacted by 534f1a0 (pipes now come from the `newBashPipe` seam, `bash_child.go:21`) and 536d113 (the reader waiter goroutine is gone; the last reader closes `readersDone`, `bash_child.go:288-292`));
`grep` delegates to `runRipgrep`; `runRipgrep` does 2×`os.Pipe` (`:122-135`), `command.Start`, 3 goroutines (stdout reader, stderr drain, `Wait`) (`:142-158`), `NewTimer` per timeout/kill/drain (`:166-188`) and a `for` select loop until all close.
Per-call caps only: `MaxResults`/`MaxMatchesPerFile`/`MaxBytes`/`MaxLineChars`/`MaxFileBytes`/`TimeoutMs`
(`sessionfs/native_grep_settings.go:10-22`, `native_glob_settings.go:11-18`; defaults grep 200/64KiB/240/8MiB/10s, glob 500/64KiB/1000/10s; max 10k/100k results).
`readRipgrepEvents` enforces `MaxResults`/`MaxBytes`/`MaxMatchesPerFile` while parsing (`ripgrep.go:302-328`)
with `truncateJSString(raw, MaxLineChars)` per match (`:309`) and `resultMu` per update.
Why 1000 fails: each bounded call still forks `rg` (or glob helper) + 2 pipes + 3 goroutines + timers.
1000 concurrent grep/glob × (1 proc + ~2 FDs + 3 goroutines) ≈1000 procs + ~2000 FDs + 3000 goroutines,
plus per-match `utf16.Encode`-style truncation allocs ×`MaxResults`. Trigger: model emits grep/glob widely in one round (B-10/B-14 path).
Fix: process-wide exec semaphore covering sync+async+grep/glob/ripgrep children (same budget as B-10), or pool `rg` workers; keep per-call caps.
Breaking? No — refuse like `async_capacity_reached`.
Evidence: **verified** (`get_code_snippet` `executeNativeGlob`/`executeNativeGrep`/`runRipgrep` + `read.sh` `ripgrep.go:254-335`, grep/glob settings).
Confirms/extends B-10 (shell fan-out) and B-14 (glob/grep inferred → now verified).

**B-21 task store serializes all ops on one per-store `mu` held across file I/O — degrades.**
`backend-go/internal/tools/native/task_store.go:207-213` (`Create` via `mutate`),
`249-251` (`List` `mu.Lock`), `303-354` (`Update`/`Delete`/`mutate` `mu.Lock` across `load`+`write`),
`352-420` (`load` `ReadFile`+`Unmarshal`+dup check; `write` `marshal`+`MaxStorageBytes` check+`MkdirAll`+`CreateTemp`+`Write`+`fsync`-less rename).
`task.go:249-313` per-call caps only (`MaxTags`/`MaxDocuments`/`MaxTitleChars`, `ListLimit`/`MaxListLimit`/`MaxListBytes`).
Shared: `TaskStore.mu` (one per `storageDir`; shared via `Resolver.taskStores` `resolver.go:809-825`).
Why 1000 fails: every `Create`/`Get`/`List`/`Update`/`Delete` sharing a storage dir serializes on `mu`;
`mutate` holds it across full-file read, JSON parse, and atomic rewrite. 1000 concurrent task calls on one store queue on `mu`;
`List` also holds it while filtering/sorting in memory. Trigger: many runs sharing a session/team task dir doing list/add/update.
Fix: snapshot `load()` under lock, validate/mutate copy, `write` under short lock with generation check; or RWMutex (readers `Get`/`List` concurrent) + per-file size cache.
Breaking? No (file format unchanged).
Evidence: **verified** (`read.sh` `task_store.go:352-420`, `grep` `mu/Lock` + `task.go` caps).

**B-22 memory store `withLock` (shared `PathLocks`) held across quota scans + per-record I/O — degrades.**
`backend-go/internal/tools/native/memory_store.go:158-180` (`Create`/`CreateValue` via `withLock` + `totals` + `MaxRecordsPerType`/`MaxTotalRecords`/`MaxTotalBytes`/`MaxTypes`),
`225-332` (`Update`/`List` via `withLock`), `326-530` (`list` holds lock across `files()`+`sort`+per-record `readRecordText`+`parse`+`match` bounded by `MaxScanRecords`/`MaxScanBytes`/`MaxListLimit`/`MaxListBytes`),
`665-730` (`totals()` `ReadDir` root + per-type `filesKnowing` + `Stat` unknowns, aborts with `quota scan exceeded` over `MaxScanRecords`/`MaxScanBytes`),
`memory_group.go:22-` (`GroupBy` same lock across full scan).
`withLock` (`memory_store.go:523-532`) is `s.locks.Lock("memory:"+s.root)` where `s.locks` is Resolver-shared `mutationLock` (`resolver.go:276,837`, `path_locks.go:22-45` refcounted per-key `mu` + global `mu` for map).
Shared: one `PathLocks` entry per memory root, shared across catalogs/sessions with same root.
Why 1000 fails: `List`/`GroupBy`/`Create`/`Update` sharing a root serialize; each holds the per-root lock while doing `ReadDir` + up to `MaxScanRecords` file reads/parses (default scan budgets in `sessionfs/native_memory_settings.go`).
1000 concurrent memory ops on one store queue; lock hold time = disk I/O, not just pointer swaps. Trigger: tool-heavy rounds using shared memory type.
Fix: list names under lock, read/parse outside lock with generation check (same as fetch/task); keep `MaxScan*`/`MaxList*` as is; shard `PathLocks` map if global `mu` shows up.
Breaking? No.
Evidence: **verified** (`read.sh` `memory_store.go:523-560,665-730,326-530`, `path_locks.go:1-80`, `resolver.go:276,837`).
Confirms P7B-R3-05 shape (lock across I/O) on HEAD abefc5f.

**B-23 fetch store quota exists but `Create/CreatePage` holds `mu` across full-dir scan + writes; no global concurrency cap — degrades (quota part is no-finding vs B-13 inferred).**
`backend-go/internal/tools/native/fetch_store.go:64-91` (`FetchStore{mu, transports, cacheMu}`),
`90-136` (`Create` `s.mu.Lock` across `MkdirAll`+`fetchDirectoryBytes`+`atomicFetchWrite` ×2),
`177-198` (`CreatePage` calls `Create` then slices page),
`280-300` (`fetchDirectoryBytes` `ReadDir`+`Info` per file),
`202-240` (`rememberURLs`/`cachedPage` under separate `cacheMu`),
`backend-go/internal/tools/native/fetch_transport.go:41-85` (`pool.get` bounded by `MaxKeepAliveHosts`, LRU evict + idle sweep under `pool.mu`),
`fetch.go:87-112,563-576` (pool reused when `KeepAliveMS>0`, else new `http.Transport` per call).
Quota: per-document `storedBytes > MaxStorageBytes` reject + aggregate `usedBytes+storedBytes > MaxStorageBytes` reject (`fetch_store.go:110-122`; defaults `sessionfs/native_fetch_settings.go:23-56`).
Shared: one `FetchStore.mu` per `storageDir` (shared via `Resolver.fetchStores` `resolver.go:849-865`); one `fetchTransportPool` per store.
Why 1000 fails: quota is per-store, not per-call-only — so B-13 "store-wide quota inferred" is **confirmed present** (no finding on that sub-point).
Remaining failure: `Create` serializes all writers of one store while scanning the whole dir (`O(files)` `Stat`s) and doing two atomic writes; 1000 concurrent fetches storing to one dir queue on `mu` and each rescans.
Transports: `MaxKeepAliveHosts` bounds idle pooled conns per store, but 1000 stores ×`maxHosts` or `KeepAliveMS==0` (no reuse) still = 1000 dials + sockets + page writes with no process-wide fetch semaphore. Trigger: fetch-heavy rounds sharing a fetch dir.
Fix: keep quota; move `fetchDirectoryBytes` to cached size counter (like memory `sizes`) + write outside `mu` with rename-then-account; add global fetch semaphore (same as B-13 fix).
Breaking? No (quota/error text unchanged).
Evidence: **verified** (`read.sh` `fetch_store.go:1-120,120-342`, `fetch_transport.go:1-120`, `fetch.go` pool lines).
Partially retracts B-13 "inferred" qualifier (quota verified); confirms B-13 "no global cap".

**B-24 MCP content handling: per-call full-buffer copies + repeated marshals, bounded only by `max_result_bytes` — degrades (mem/CPU).**
`backend-go/internal/tools/mcp/content.go:28-` (`executeTool` `WithTimeout(current.timeout)` + `callTool` then `structuredContentForResult`+`boundedContent`),
`contentParts` builds `[]outputPart` (one per block; `ImageContent` kept as `data:` base64 string, `EmbeddedResource` blob kept),
`boundedContent` cuts to `maxBytes` but calls `partSize` (`tools.MarshalJSON` per part) in a loop and `fitTextPart` which loops `MarshalJSON`+`CutText` with shrink-by-ratio (`content.go:162-230`),
`imagePart`/`filePart` `base64.StdEncoding.EncodeToString` (new ~4/3 string per image/file),
`compactJSON` re-marshals `StructuredContent`.
Shared: nothing global — allocations are per-call, but 1000 concurrent calls each hold up to `max_result_bytes` (default? `config.maxResultBytes`) ×2–3 copies (SDK result + `[]outputPart` + marshalled `json.RawMessage`) plus base64-expanded images.
Why 1000 fails: 1000 ×`max_result_bytes` resident + `partSize` double-marshal CPU scales with parts; `fitTextPart` loop re-marshals per shrink step. Large MCP images/texts ×1000 = GBs. Trigger: tool-heavy rounds returning images/blobs.
Fix: stream/cut without re-marshal per part (measure once, slice once); cap decoded image bytes separately from `max_result_bytes`; share immutable snapshots.
Breaking? No (result shape unchanged; additive spill).
Evidence: **verified** (`read.sh` `content.go:1-120,120-288`).
Extends B-12 (MCP conn) to content path (was "not verified").

**B-25 MCP load: per-server `serverLock` across config read + dial + multi-page `tools/list`; `LoadTools` fans out 8 workers per call — degrades.**
`backend-go/internal/tools/mcp/manager.go:214-285` (`LoadTools` prunes stale under `m.mu`, then `workers=min(len(enabled),8)` goroutines each `loadServer`),
`286-370` (`loadServer` → `getConnection` → `listTools` → `toolsFromList`),
`369-470` (`getConnection` `serverLock.Lock` across `ReadFile(config)`+`normalizeConfig`+`Connect(WithTimeout)`+`MaxEventSize` setup; `m.mu` only for map insert),
`52-120` (`listTools` `RLock` + `collectToolPages` bounded by `maxTools`/`maxTools+1` pages + loop detection),
`toolsFromList` validates `maxDescriptionBytes`/`maxSchemaBytes`, `CompileSchema` via `schemaCache`, builds `[]tools.Tool` (one closure per tool capturing `current`).
Shared: one `*sync.Mutex` per server ID (`serverLocks` map under `m.mu` `469-480`); one `connection{mu,callMu}` per server (single `ClientSession`).
Why 1000 fails: 1000 callers loading the same popular server serialize on `serverLock` across dial (network RTT + TLS) + paged `ListTools`; `callTool` (`120-140` `callMu.Lock` + `RLock`) serializes all calls on one session (head-of-line; timeouts cascade — as B-12 said).
`maxConcurrentServerLoads=8` bounds workers per `LoadTools` call, but 1000 concurrent `LoadTools` = 8000 workers + 1000× per-server tool slices + schema compiles. Trigger: cold start / popular MCP server.
Fix: single-flight dial + small conn pool per server (as B-12 fix); cache `toolsFromList` by `fingerprint` outside `serverLock`.
Breaking? No.
Evidence: **verified** (`read.sh` `manager.go:120-180,214-285,286-470,480-600`).
Confirms B-12 (was verified for conn; load path now fully read).

**B-26 imagestore: decoded bytes + per-run `Store` (quota not global) + first-write scan under `mu` — degrades (mem + I/O). NO-FINDING on cross-run `mu` contention.**
`backend-go/internal/imagestore/store.go:55-90` (`Options{Dir,MinBytes,MaxFileBytes,MaxTotalBytes}` defaults `sessionfs/image_store_settings.go:10-18` 16KiB/20MiB/1GiB),
`96-180` (`Externalize`/`Rehydrate`: `Rehydrate` rescans every history item for `keyedRef` and `base64.AppendEncode`s each hit — new ~4/3 string per image per round),
`443-500` (`putPayload`: `base64.DecodeString` (full alloc) + `http.DetectContentType` + `sha256` + `s.mu` across `Stat`+`directorySize`+`writeAtomic`),
`479-491` (`mu` covers `directorySize` + file write),
`629-647` (`directorySize` `ReadDir`+`Info` per file),
`app/bound_runner.go:58,74-75` + `app/image_resize.go:15-22` (`newImageStore` per run; `PersistInputWithImages` also per-run).
Shared: `Store.mu/total/scanned` are per-`Store` (per run), **not** cross-run — P8-R1-04 re-confirmed: no global imagestore lock contention at 1000 runs (**no finding** on that hypothesis).
Why 1000 still fails: quota (`MaxTotalBytes`) and `scanned` cache are per-run, so 1000 runs each do their own `directorySize` scan of the same dir (1000×`ReadDir`) with no sharing; each decoded image (`up to MaxFileBytes` 20MiB default, 100MiB max) is resident per concurrent `put`/`Open`/`Rehydrate`; transcript keeps refs but wire/request holds data URLs. Trigger: image-generating tools × many runs.
Fix: one shared `Store` per image dir in `Resolver` (as P8-R1-04 suggests) to share `mu`/`total`/`scanned`; enforce `MaxTotalBytes` globally; `Rehydrate` only for items actually sent.
Breaking? No (file layout unchanged).
Evidence: **verified** (`read.sh` `imagestore/store.go:1-120,440-560,159-260,629-648`, `bound_runner.go:58,74-75`).
Retracts cross-run lock-contention worry; confirms per-run scan + mem blowup.

**B-27 imageresize: full decode per image per run, per-run memo only — degrades (mem/CPU).**
`backend-go/internal/imageresize/resize.go:60-` (`parseDataURL` `base64.DecodeString` + `DecodeConfig` + `MaxPixels` gate + `image.Decode` full pixel buffer + EXIF/orient + shrink loop + JPEG re-encode),
`apply.go:54-` (`Apply` scans full `history` for `input_image` marker each round, `json.Unmarshal` per item/part, `resized()` per URL),
`apply.go:150-` (`resized` `sha256(url)` + `mu`-guarded `cache/order` bounded by `CacheEntries` (default 64, max 4096 `sessionfs/image_resize_settings.go:21-23`; defaults 1568px/1MiB/64M pixels)),
`app/image_resize.go:9-14` (`newImageResizer` per run).
Shared: `Resizer.mu/cache/order` per run (per `boundRunner.Run`), not global.
Why 1000 fails: each image that needs resize is `base64.Decode` + `image.Decode` (W×H×4 bytes; default `MaxPixels` 64M ≈256MiB RGBA, max 400M ≈1.6GiB per image) with no process-wide decode budget; 1000 concurrent resizes = CPU + GBs; per-run memo (`CacheEntries` 64×`MaxBytes` 1MiB ≈64MiB per run) ×1000 = 64GiB if all full.
`Apply`+`Rehydrate` rescan full history every round (see B-29), so same image decoded once per run (memo helps within run only).
Trigger: image-heavy histories (user/MCP/native images) × many rounds.
Fix: global in-flight decode budget (semaphore by estimated pixels) + shared memo by image hash across runs of same session; lower `MaxPixels` default or spill.
Breaking? No (request bytes only; transcript unchanged).
Evidence: **verified** (`read.sh` `imageresize/resize.go:1-100`, `apply.go:1-223`, settings).
New vs B-16 (`liveImages`): this is the pre-send decode path.

**B-28 checkpoint: per-call git fork storm + per-staging-dir `gate` + scans; global maps never evicted — degrades (blocker if shared workspace / storm).**
`backend-go/internal/checkpoint/git.go:159-210` (`run` `exec.CommandContext(timeout)`+`WaitDelay 5s`+2 `limitedBuffer`s),
`213-260` (`prepare`: `Stat HEAD`, `init --bare`, `privateDir` chmods, stale `.lock` removal, `info/attributes`+`exclude` writes),
`263-340` (`exceedsFiles` `ls-files --others` with `stopAfter`, `snapshot` `add --all`+`write-tree`+`ls-files -z`, `repositoryBytes` `count-objects -v`, `diff`/`checkout`),
`service.go:146-210` (`Service{gate *Mutex, mu *Mutex, size *repoSize}` per staging dir; `gates/locks/repoSizes sync.Map` `183-195` never `Delete` — confirms A16),
`320-500` (`capture`: `prepare`+`repositoryBytes`+`MaxTotalBytes` gate+`exceedsFiles`+`snapshot`+`repositoryBytes` ≈6–8 forks per `Begin`/`Finish`),
`565-720` (`reclaim` `gc --prune=now` under `gate`; `Begin`/`Finish`/`Restore` all `gate.Lock` across all git + `readAll`/`writeAll` metadata atomic writes).
`run.go:43-86` (`Run{service,turnID}` per run; `Start` on `run_start`, `BeforeCalls` on first changing call, `Finish` always).
Shared: `gate` per `StagingDir` (normally per session, so 1000 distinct sessions do **not** serialize on one gate — no global lock finding), but processes/FDs/disk are global.
Why 1000 fails: with checkpoints on + mutating tools, each run forks ~6–8 `git` children twice (Begin+Finish) = ~12k–16k procs for 1000 runs + `count-objects`/`ls-files` scans of workspace + private repo growth to `MaxTotalBytes`/`MaxFiles`; `gc` holds one staging `gate` across full repack.
Trigger: 1000 mutating runs starting/finishing together, or one shared workspace (gates collapse to one).
Fix: async/coalesced snapshots + snapshot queue (as B-18 suggested); cache `repositoryBytes`/`exceedsFiles` across back-to-back captures; bound concurrent `git` with process-wide semaphore; evict `gates/locks/repoSizes` on session delete.
Breaking? No (refs/metadata format unchanged).
Evidence: **verified** (`read.sh` `git.go:120-385`, `service.go:180-320,320-470,470-530,520-720`, `run.go:1-94`).
Confirms/extends B-18 (was inferred) + A16 (never evicted).

**B-29 Runner hot paths below `executeRun`: per-round full-history rescan + compaction trigger parse + `CompactHistory` + state-context rebuild — degrades.**
`backend-go/internal/session/runner.go:400-513` (impacted by c8f74a7: `RunOptions.LeafBudget` added at `:138-140`, and the leaf calls of `executeToolCalls` now pass through it at `:656`; the per-round costs below are unchanged. `Run` holds `r.mu` whole run (B-02); per round: `ToolCatalog.ResponseTools()` rebuild (`460-464`), `buildRoundRequest` (`465`), `runResponse` + `compactAfterResponse` (`486`), `enforceToolLimits`/`executeToolCalls` (`507-510`)),
`520-573` (`persistRunInput` once; `buildRoundRequest`: `inputHistory.Read()` + `ImageStore.Rehydrate(history)` + `ImageResizer.Apply` + `BuildRequestWithStateContext`),
`574-600` (`compactAfterResponse`: `automaticCompactionTrigger(event)` `716-760` parses `response.completed`+`usage.input_tokens` JSON per round, then `CompactHistory` (`shared.compactPreservingUsage` via `bound_runner.go:101-111`) + `OnUsageChanged`),
`605-700` (`enforceToolLimits` sums `usedTokens` from `responseTokens(event)` per round; `executeToolCalls`: `checkpointRun.BeforeCalls` + `executeToolRound` + `ImageStore.ExternalizeAll` + `appendToolOutputs`),
`app/bound_runner.go:56-131` (`scanTranscriptIndex` for turn, `prepareRun`, `collectStateContextSnapshot` reading process/task/memory/subagent/async stores + `AppendStateContext` after compaction).
Shared: `Runner.mu` (per session, B-02); transcript `TurnStore.mu`/journal `mu` (A4/A6); stores read by state-context.
Why 1000 fails: `Rehydrate`+`Apply` each scan **full** `history` (`bytes.Contains` per item + `Unmarshal` per image item) every round even though `InputHistory` is incremental (`runner.go:443-452` comment); `ResponseTools()` rebuilt every round; `automaticCompactionTrigger` re-parses event JSON every round; on fire, compaction rewrites transcript (blocks `BeginTurn` per A6) + `RefreshStateContext` re-reads 5 stores + appends. O(history)×rounds×1000 + compaction storms. Trigger: long histories + many tool rounds + `TriggerTokens` reached together.
Fix: incremental builder (reuse `InputHistory` delta for images too; cache `Rehydrate`/`Apply` by item hash); cache `ResponseTools()` until catalog generation changes; coalesce compactions.
Breaking? No.
Evidence: **verified** (`read.sh` `runner.go:396-513,520-575,605-700,716-800,900-1057`, `bound_runner.go:1-141`).
Extends B-17 (per-round rebuild) below `executeRun` + B-18 (compaction inline).

**B-30 process_manager per-call locking is narrow (no lock across fork) — NO FINDING on lock-hold; O(n) scans remain minor.**
`backend-go/internal/tools/native/process_manager.go:144-161` (`mu`, `persistMu`, `recoverMu`),
`250-330` (`Start`: `Recover()` then `mu` only for `closed`+`running` count+`OwnerLimit` check+`id`+`MkdirAll`+`entries/manifests` insert; fork (`command.Start` `380-390`) + `persistManifest` (`persistMu`) + `snapshotEntry` after `mu` released),
`429-496` (`SnapshotWithLogs`/`List`/`Wait`/`Stop`: `mu` only to clone/copy maps; `List` `466-491` clones under `mu`, sorts outside),
`974-1070` (`Recover` `recoverMu` + `ReadDir` once; fast path `recovered==true` under `mu`),
`process_owner.go:30-45` (`runningOwnedLocked` O(entries) under `mu`; `stopOwned` copies under `mu`, signals outside).
Shared: `ManagedProcessManager.mu` per storage dir (shared via `Resolver.processManagers` `resolver.go:710-729`); `MaxRunning` per manager (default 8, max 1024 `sessionfs/native_process_settings.go:12-21`) + `OwnerLimit` per owner.
Why not a 1000-run lock failure: holds are pointer/map ops + `MkdirAll`/`newManagedProcessID`; `command.Start` and `waitForProcess` (`650-`) run outside `mu` (avoids B-10-style stall); no `mu→entry.mu` inversion beyond documented `recordLiveManifest` order.
Remaining (minor, already in B-10/B-11): `Start` running-count + `runningOwnedLocked` + `List` are O(processes) under `mu`; 1000 owners × processes = linear scans per op; no process-wide cap beyond per-manager `MaxRunning` (1000 managers ×8 = 8000 procs). Trigger: many processes per shared manager + frequent `List`/`Start`.
Fix: none for locking (keep); for budget use B-10 process-wide semaphore.
Breaking? N/A.
Evidence: **verified** (`read.sh` `process_manager.go:250-330,330-430,466-492,974-1070`, `process_owner.go:1-60`).
Resolves "per-call locking" gap as **no finding**; budget part stays with B-10/B-11.

## Not verified (this close-out)

- Measured cost of any path above (no benchmarks/load per task).
- `native/exec_functions.go`, `subagent` bodies beyond `subagent_manager` (Part C) — not re-read here.
- `transcript/TurnStore.Compact/CommitWithObserver/InputHistory` bodies beyond `runner.go` call sites (A4/A6 cited, not re-read).
- `mcp/schema_cache.go` eviction/bounds beyond `toolsFromList` call (seen `m.schemas.get`, not sized).
- `checkpoint` `MaxFiles`/`MaxTotalBytes` defaults and `commitObjectAllowance` sizing beyond `service.go` gates/scans.
- `imagestore` `ValidName`/`partBounds`/`plainItem` edge costs; `imageresize` JPEG quality loop worst-case.
- `fetch` DNS/`validateFetchURL` per-hop costs beyond `fetch.go:389-` signature (not re-read).
- Full lock-order graph (pairs noted in inventory, not rebuilt).
- `go build/vet/test` not run (read-only audit; no test command per scale-audit rules).

## Method

- `get_architecture` (project `home-www-Downloads-tmp-myagent3`) + `zvec-grep` scoped searches (fetch/MCP/image/checkpoint/runner).
- `search_graph` for `executeNativeGlob/Grep/runRipgrep`, `Runner` (trace attempt failed: `Runner.Run` qn not found → fallback).
- `get_code_snippet` for `executeNativeGlob`, `executeNativeGrep`, `runRipgrep` (full).
- `exec` `read.sh PATH START END` exact ranges for all other bodies (fallback per reviewer rules); `grep -n` counts redirected to `/tmp/gaps-B/` (not pasted in full).
- No `read.sh` whole-file reads; no edits; HEAD `abefc5f` verified via `git rev-parse`.


## Part C gaps (reviewer3)

# Scale audit — Part C gap closure (step 1)

Read-only. HEAD abefc5f (= 24f005a + one commit adding `audit_findings/` docs only; verified with
`git log --oneline 24f005a..abefc5f` and `git diff --name-only 24f005a..abefc5f` → 4 markdown files,
no code changes). Row IDs continue from C-21. Evidence: **V** = cited lines read via index tools
(`search_graph`/`search_code`/`get_code_snippet`/`trace_path`, `zvec-grep`) or `read.sh` exact ranges;
**I** = inferred from call sites/structure. Fallbacks used and stated per row: `read.sh`/`grep`/`sed`
for exact ranges and for symbols the index did not hold (`TaskStore.List` was absent from
`search_graph`; located with `grep -n` then read with `read.sh`). No tests, benchmarks or load tests
run; no files outside `/tmp/gaps-C/` touched.

Scope per assignment: Part C rows C-17..C-20 confirm/retract; webui.Increments; interactionbroker;
memorytrace sampling loop (trace.go:210-264); session.Runner hooks **only** where they call into
httpapi (targeted reads of runner call sites only — the runner itself was not re-reviewed);
sub-agent task path (native/task.go + the subagent call path) and per-call scans added since the
inventory; anthropicadapter per-request client/transport (B-05 question).

## Part 1 — C-17..C-20 confirmation pass (same format, verdict in ID column)

| ID | Location | Shared thing | Why it fails at 1000 / trigger | Sev | Design change (breaking? old data) | Evid |
|----|----------|--------------|-------------------------------|-----|-----------------------------------|------|
| C-17 **[confirm, refined]** | `internal/logging/logging.go:46-88`; `internal/httpapi/request_observer.go:78-124`; `internal/httpapi/executor.go:30-55` | One log destination: `os.File` O_APPEND (or stderr) behind the stdlib slog handler mutex (`$GOROOT/src/log/slog/handler.go:202,319-320` — `commonHandler.mu` serializes every `Handle`, correcting the row's "no app lock") | 1000 runs produce per-run `run started`/`run finished`/`run aborted` **info** lines (executor.go:36,55) + request lines. Refinement: ordinary request records are `LevelDebug` (request_observer.go:115-119), so at the default `info` level the per-request volume claim holds only at debug; the single-fd serialization and "slow disk stalls every writer" claim stands at every level. Trigger: log volume / slow disk. | degrades | Buffered/async writer, batched flush. Not breaking. | V |
| C-18 **[confirm, refined]** | `internal/remoteapi/tokens.go:243-300` | `TokenTable.mu` + per-auth O(tokens) `subtle.ConstantTimeCompare` scan + throttled file re-read (`refresh`) | 1000 remote auths each pay the linear scan. Refinement: the scan runs **outside** the RLock (`Authenticate` copies the slice header under `RLock`/`RUnlock` at :277-283, then scans :285-291), so the lock hold is O(1); the row's "RLock + linear scan" wording is imprecise but the O(tokens)/req cost and `refresh`'s throttled re-read+parse (`:243-267`, under `mu`) are as described. Trigger: remote API auth at scale. | minor | Hash-prefix bucket over the same constant-time compare. Not breaking; token format unchanged. | V |
| C-19 **[confirm]** | `internal/serverconfig/defaults.go:54-59` | Config ceilings for `server.max_concurrent_runs` | `MaxMaxConcurrentRuns = 1024` is the hard ceiling; `DefaultMaxConcurrentRuns = 0` (**no cap by default**, reinforcing B-03). 1000 active runs is inside the ceiling but any configured cap cannot exceed 1024. Trigger: config near max. | minor | Raise/unbind the ceiling (config value). | V |
| C-20 **[confirm, location refined]** | `internal/app/service.go:92-113` (`startStorageRecovery`); `internal/app/storage_recovery.go:55-85,87-145`; `cmd/jad/main.go:60-115` | Startup recovery scan: one background goroutine doing a **serial** roots × sessions × profiles walk | The goroutine is started from `NewService` (service.go:85-87, gated on `recover_*_at_start`, default true), not from main.go directly — main.go:60-115 only does config/logger/listener wiring (verified). `RecoverStorages` does `os.ReadDir` per root + `hasSessionSetting`/`LoadSessionSettings` per session + per extra profile (`storage_recovery.go:55-145`), ctx-cancellable between sessions. With many sessions the scan is slow (serial) but never blocks listen. Trigger: cold start, large root. | minor | Parallelize per-session with bounded workers. Not breaking. | V |

## Part 2 — new rows (C-21 onward)

| ID | Location | Shared thing | Why it fails at 1000 / trigger | Sev | Design change (breaking? old data) | Evid |
|----|----------|--------------|-------------------------------|-----|-----------------------------------|------|
| C-21 | `internal/webui/types.go:234-334` (`Increments`, `partsEqual`, `appendedText`); fed per run from `internal/httpapi/registry.go:621-657` (`activeRun.latestMessage`) and `internal/httpapi/message_stream_emitter.go:16-77` (per-run `lastEmitted`/`pending`), wired once per run in `internal/httpapi/executor.go:70` (`newMessageStreamEmitter(..., run.publishMessage)`) | CPU, no lock: the previous/next message pair of **each run** is diffed twice per delta — `push` → `appendOnlyPartIndex` (emitter.go:142-160) and `emitLocked` → `publishMessage` → `messageFrames` (registry.go:661-677) — and `publishMessage` may rebuild the frames a **third** time under `run.mu` when `latestMessage` changed (registry.go:639-642) | Each `Increments` call is O(prefix length) for a streaming part: `partsEqual` compares full `Text`/`*Arguments` of every unchanged part (types.go:264-272) and `appendedText` does `strings.HasPrefix(after, before)` (types.go:326). A stream of D deltas on text length L costs O(D·L) per run ×2-3 calls → quadratic in each stream's text, ×1000 concurrent streams of pure CPU/GC burn on run goroutines. Trigger: long streaming text at scale. | degrades | Diff once per `push` and hand the frames to `publish` (kill the second `Increments` in `messageFrames`); make the append check O(delta) by tail-comparing at the known part index instead of re-scanning prefixes. Not breaking. | V (all snippets read; confirms p3 PH4 "doubling is exact, cost in the comparison" — measurement in `audit_findings/p3_merged.md` was **not** re-run) |
| C-22 | `internal/interactions/broker.go:18-160,274-284`; allocated per run at `internal/httpapi/registry.go:356` (`interactionbroker.NewBroker(run.setPendingInteraction)`) | **No finding.** No global map and no shared lock: one `Broker` per run holding at most one `entry *pendingEntry`; one waiter = the tool goroutine parked on the buffered `settled` channel (Request :126-131); one `time.AfterFunc` per question (:113); `notify`/`Reply` run `onChange` outside `b.mu` (:134-160, :274-284) | 1000 runs with pending questions cost 1 parked goroutine + 1 timer each — already counted in B-04 (parked run goroutines) and C-14 (per-finish timers, same timer-per-event profile). A second concurrent interaction in one run is refused by design ("an interaction is already pending for this run", :109-112). The reply endpoint reaches the broker through `runRegistry` (C-02), not a broker registry. | no finding (minor) | None needed. | V — closes Part C's "interactions broker not verified" gap; confirms B-04's parked-waiter accounting |
| C-23 | `internal/memorytrace/trace.go:210-264` (sampling loop: `ensureSamplingLocked`/`sampleGeneration`/`stopSamplingLocked`/`updatePeaksLocked`/`activeIDsLocked`), `:86-135` (`New`/`FromEnvironment`/`BeginRun`), `:273-322` (`recordLocked`/`write`/`processMemory`); one `Trace` per process (`internal/httpapi/handler.go:129,244`); fed per run from `internal/session/runner.go:412-413,417,479,549,557,672` (targeted reads) | One per-process `Trace.mu`, one ticker goroutine **per process** while ≥1 run is active (not per run): `ensureSamplingLocked` starts a single ticker on the first `BeginRun`, `stopSamplingLocked` stops it when the last run ends (trace.go:203-205). Default interval 250 ms (`serverconfig/defaults.go:22`). Opt-in only: `JAD_PROVIDER_MEMORY_TRACE=1` (trace.go:19,111); disabled → zero-Handle, all methods no-op | When **enabled** with 1000 active runs: every `Mark` (≈4 per round per run: input-normalized, history-loaded, request-built, provider-response-complete, tool-results-persisted) and every 250 ms tick calls `runtime.ReadMemStats` (global heap stop) + `/proc/self/statm` read (trace.go:293-312), takes the single `Trace.mu`, walks **all** active runs for peaks (`updatePeaksLocked` O(n), :257-262), sorts all run IDs (`activeIDsLocked` O(n log n), :264-271), and writes one JSON line (with all n `RunIDs` per tick) to stdout (`fmt.Println`, :284-291, 86-100). Mark frequency × O(n) work under one lock, plus a STW-style `ReadMemStats` per mark, is the failure. Trigger: `JAD_PROVIDER_MEMORY_TRACE=1` at scale. | no finding at default; **degrades (STW amplifier)** when enabled | Read memory once per tick and let Marks reuse it; O(1) peak maintenance (per-run peak only at run end from tick min/max); cap or omit the per-tick `RunIDs` list. Not breaking. | V |
| C-24 | `internal/httpapi/executor.go:74-124` (RunOptions hooks), `:156-169` (`publishRunUsage`) → `internal/app/bound_runner.go:232-254` (`ReadRunUsageSummary`) → `scanTranscriptIndex` (A13 path, `app/catalog.go`); `internal/httpapi/registry.go:694-704` (`cacheContent`; impacted by c21d26e, which added an `onEnd` field to `activeRun` at `:298-300` and does not change this path); `internal/httpapi/live_images.go:15-34` (`keepLiveImages`); `internal/httpapi/async_state.go:34-92` (`watchAsyncState`) | The runner→httpapi hook bridge: per response commit and per usage change the hook re-enters the shared per-session usage-scan path; per run one async-state watcher goroutine + one `WatchSessionAsync` registration | `OnResponseCommitted` and `OnUsageChanged` both call `publishRunUsage` → `ReadRunUsageSummary` → `scanTranscriptIndex` (bound_runner.go:246) — i.e. the A13 cost (journal-lock transcript scan + `usage.json` rewrite) is paid **per commit and per usage change per run**; ×1000 runs this multiplies A13 into a global CPU/IO load and lengthens the per-session journal-lock holds. Per delta `cacheContent` appends to the per-run content file (fine); per tool result `keepLiveImages` decodes inline images into `run.liveImages` (bounded by output size, B-16); `watchAsyncState` adds 1 goroutine/run and each publish calls the resolver's async listing (`read()` → asyncmgr `List`, C-06 O(handles) under `mu`) + `slices.Equal` over IDs. Trigger: 1000 runs streaming/committing. | degrades | Debounce usage publishes (commit only, or coalesce per turn); reuse the A14 turn-start cache for the summary; make the async watcher diff without full listing. Not breaking. | V (hook chain read end-to-end; `scanTranscriptIndex` internals taken from A13, not re-read) — extends A13 + B-17 |
| C-25 | `internal/tools/native/task_store.go:196-203` (`TaskStore.mu`, no cache field), `:249-302` (`List`), `:352-430` (`mutate`/`load`/`write`); `internal/tools/native/task.go:627-679` (`CollectTaskStateContext`/`collectTaskSummary`), `:546-556` (`taskNotFound` → `List` all), `:295,355,373,386,400,438` (per-call store ops); run-start caller `internal/app/state_context.go:116` (`collectRunStartStateContext`, traced from `app.Run`) | Per-storage-dir `TaskStore.mu` over the on-disk task document. **No in-memory cache**: every `List`/`Get`/`Create`/`Update`/`Delete` does `os.ReadFile` + `json.Unmarshal` + full validation + O(n) filter + O(n log n) sort under the mutex; every mutation rewrites the **whole** document (marshal + CreateTemp + Rename). `collectTaskSummary` reads every task (`Limit: math.MaxInt32`, task.go:656) | Every run start builds state context → `CollectTaskStateContext` → `source.List` → full task-file parse (per run start, per store); a task-tool `list`/`get`/`update` call does the same, and a failed get (`taskNotFound`) triggers another full `List` (task.go:548). 1000 run starts sharing one tool storage dir serialize on `TaskStore.mu`, each paying a full parse; adds latency to every run start and every task call. Trigger: run-start storm or task-tool-heavy rounds on a shared store. | degrades | Cache the parsed document keyed by (size,mtime) with reload-on-change; keep tasks pre-sorted; write-behind mutations. Not breaking; storage format unchanged. | V — `TaskStore.List` body was **absent from `search_graph`** (only the `TaskStateSource` interface method indexed); located with `grep -n` and read with `read.sh` (fallback, per rules). **No scans added since the inventory**: `git diff 24f005a..abefc5f` is audit docs only; these predate the inventory and were unlisted (reviewer2 Not-verified: `native/task` bodies) |
| C-26 | `internal/app/subagent.go:46-119` (`subagentLauncher.Run`), `:176-232` (`childConfig`/`openStore`), `:156-174` (`stateContextFor`); `internal/app/bound_runner.go:195-211` (`stateContextEntries`). impacted by c8f74a7: `subagent.go` passes `LeafBudget` into the child run options (`:96`); the per-call child setup described here is unchanged; `internal/tools/native/subagent.go` (tool shell) | Per subagent call: full child-run setup on shared infrastructure — `config.WithModel()` load, `resolver.providerAPIKey` + `resolver.newProviderStreamer` (C-01 `Resolver.mu` + B-05 per-call provider client; non-default network/oauth/debug ⇒ **new Transport per call**), `openStore` (new dir + `transcript.NewJournal` + `NewTurnStore` + optional Seed transcript copy), `stateContextFor` → `store.ReadStateContextEntries()` (parent-transcript read per call when `agent.StateContext`) | Each subagent tool call pays O(setup I/O) + one provider client build + one parent state-context read, then a full child `runner.Run` (all Part B costs) whose `source != ""` escapes the run cap (B-03) and whose nesting multiplies everything (B-19). 1000 runs spawning subtrees amplify client builds, FDs and transcript stores. Trigger: subagent tool used at scale. | degrades (amplifier of B-19) | Pool provider clients per agent config (fixes B-05 here too); memoize the state-context snapshot per turn; global depth/fan-out budget (B-19). Not breaking. | V (Run/openStore/stateContextFor read; `WithModel`/`ReadStateContextEntries` bodies not read — I). Closes reviewer2's `native/{task,subagent}` not-verified entry for the call path; confirms C-11..C-13 manager-side rows. **Nothing added since the inventory** (same git evidence as C-25) |
| C-27 | `internal/anthropicadapter/handler.go:28-33,93-155`; `internal/anthropicadapter/sdk.go:15-26,61-82`; `internal/anthropicadapter/stream_handler.go:16-107`; startup `cmd/anthropic-adapter/main.go:46` | **No finding (B-05 symmetry question).** One `http.Client` (default shared transport) + one `anthropic.Client` built **once** at `NewHandler` (`main.go:46` passes nil → `&http.Client{}`); per request only `option.RequestOption` slices are built (`sdkRequestOptions`: headers, `WithRequestTimeout`, `WithResponseInto`) — no per-request client, transport or dialer | 1000 concurrent adapter requests share one connection pool — the same profile as B-05's default path (idle-pool contention only; no FD/timer blowup). Per-request extras are bounded: one `context.WithTimeout` timer per request, a retry loop with `sleepContext` timers bounded by `Upstream.StreamRetry.MaxRetries` (stream_handler.go:27-90), and one drain goroutine + 25 ms timer per completed stream (stream_handler.go:94-107). Trigger: none structural at 1000. | no finding | None needed; if anything, bound total in-flight upstream requests at the adapter. | V — resolves reviewer2's "anthropicadapter symmetry" not-verified item; confirms B-05's shared-default-transport analysis for the adapter |

## Lock-order / concurrency notes (this pass)

- `messageStreamEmitter.mu` is held across `publishMessage` (`emitLocked` → `publish`, emitter.go:123-133);
  `publishMessage` takes `activeRun.mu` inside. Order: emitter.mu → run.mu. `emitter.flush`/`discard` take
  emitter.mu only. No reverse path seen (the runner calls `push`/`flush` serially per run).
- `interactions.Broker.mu` is a leaf: `notify`/`Reply` fire `onChange` after unlock (broker.go:134-160).
- `memorytrace.Trace.mu` is a leaf (write happens after unlock, trace.go:241-243 etc.).
- `TaskStore.mu` is a leaf (held across file I/O though — the whole load/write).

## Not verified (this pass)

- No benchmarks, tests, or load tests run (forbidden for this step); all severities are structural.
  The p3 measurement (`audit_findings/p3_merged.md`) is cited as prior art only and was not re-run.
- `app/catalog.go scanTranscriptIndex` body not re-read (C-24 relies on A13's evidence).
- `webui.NewStreamingRun.Update` per-delta projection cost (C-21 sizes only the `Increments` diff).
- `sessionfs.SessionConfig.WithModel` cost per subagent call and `transcript.ReadStateContextEntries`
  body (C-26 marks those legs I).
- `executeNativeTask` action-handler bodies beyond the store call sites (grep-located, task.go:295-548).
- `remoteapi` sweep/evict, `run_content_cache` write path, `message_stream_emitter.emitLocked`
  internals beyond the diff path — left to C-02..C-04/p3 coverage.
- `WatchSessionAsync` resolver-side registration map (only the httpapi side of C-24 read).
- Non-unix `processMemory` fallback (C-23) — `/proc/self/statm` path is linux; darwin/Windows use
  `MemStats.Sys` only (branch visible in trace.go:293-312, not exercised).
- Whether `DefaultMaxConcurrentRuns = 0` interacts with any other admission path beyond
  `beginFrom` (B-03 already covers the registry).
