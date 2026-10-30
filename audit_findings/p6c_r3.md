# p6c review — reviewer r3

- phase: p6c (native agent tools: subagent*, profile_message*, statecontext_counts.go)
- reviewer: r3
- HEAD: 3bf8c33
- date: 2026-10-29
- files read: backend-go/internal/tools/native/subagent.go (1-726), subagent_call.go (1-356), subagent_manager.go (1-914), subagent_store.go (1-236), subagent_owner*.go, subagent_external.go (1-45), subagent_remote.go (1-272), subagent_async.go (1-152), subagent_answer.go (1-88), subagent_changes.go (1-87), subagent_statecontext.go (1-99), profile_message.go (1-504), profile_message_async.go (1-301), profile_message_remote.go (1-310), statecontext_counts.go (1-54); plus backend-go/internal/tools/catalog.go (27-130) + catalog_nested_test.go and path_scope.go (narrowScope) as consumer evidence for ForNested; sessionfs native_subagent/profile_message settings only via grep for bounds. Tests read as evidence: subagent_nesting/manager/async/remote, profile_message_remote/session tests (names listed in §6/§7).
- commands: `go vet ./internal/tools/native/` exit 0; `gofmt -l` on the 12 in-scope non-test files exit 0 (no output); `go build ./internal/tools/native/` exit 0; `go test ./internal/tools/native/ -run 'TestNestedStartIsRefusedBeyondMaxDepth|TestSubagentManagerCloseStopsDetachedChildrenAndWaitsForThem|TestProfileMessageRejectsTraversalInASessionTarget' -count=1` exit 0 (0.028s); `go test ./internal/tools/native/ -run 'TestRemoteSubagentHopsAreSentAndLimited|TestRemoteProfileMessageHopsAreSentAndTheRemoteLimitIsReported|TestSubagentCutsALateAnswerToTheResultLimit' -count=1` exit 0 (0.038s). vet/build/gofmt run from backend-go/.
- navigation: codebase-memory search_graph/get_code_snippet/trace_path + zvec-grep for orientation; exact line ranges via read.sh fallback where noted (per prompt §Navigation). No audit_findings file other than this one was opened.

## 1 Understanding

p6c is the agent-messaging half of tools/native: local sub-agent runs (start/wait/stop as async handles, writers that run to their end, nested children with depth + per-root/per-session caps, persisted manifests with `lost` recovery, transcript-backed answers, changed-file tracking, statecontext summaries) plus `profile_message` (local profiles incl. `<session>/<category>/<profile>` addressing, and `remote:<name>/...` targets for both tools followed as async handles with poll/stop/lost semantics). The trust boundaries are: model-supplied ids/texts (agent/profile/task/message/block_ms), session allow-lists (agents, targets, sessions globs, remotes, deny lists), files on disk (manifests, transcripts, remotes folder), the remote peer (tokens, responses, redirects — mostly cross-phase in `remote/`), and owner scoping (SessionKey vs run-id owners). Shutdown/Close must stop detached work and wait for it (the `176fd7d` class); nested cancel flows through parent-derived contexts; panic recovery is in the launcher boundary (D-1).

## 2 Hypotheses

| id | statement | status |
|----|-----------|--------|
| H01 | manager Close/shutdown waits for detached writers/handles without holding the lock | verified sound (manager.go:866-878 Close: lock-set-closed/unlock/cancel/Wait; execute release/finish paths traced) |
| H02 | cross-session addressing validates session + target and does not leak distinguishability | verified sound on validation + unknownProfile indistinguishability; residual glob/deny asymmetry filed as P6C-R3-05 |
| H03 | remote lifetime/cancel + hop/depth gates are enforced | verified locally sound; hop-limit enforcement itself is cross-phase (see §5); id-collision + Detail-truncation filed as P6C-R3-01/02 |
| H04 | answer/changes size limits bound retained state; secrets not leaked in results | verified except Detail-poisoning (P6C-R3-01) and files-prefix overshoot (P6C-R3-07); no secret leak found in scope |
| H05 | owner identity (SessionKey vs ProcessOwner/run-id) isolates sessions and children | verified sound (handleOwner, ProcessOwner stamping, by-owner lists, Cancel* scoping) |
| H06 | manifest recovery is idempotent, crash-safe, storage-confined | verified sound (version/id/state/start checks, insideDirectory + transcriptFolderUnder guards, foreign-server skip, best-effort writes) |

## 3 Findings

### P6C-R3-01 Restored run answer is permanently truncated to the first Detail limit
- severity: medium
- lens: A correctness
- location: backend-go/internal/tools/native/subagent_manager.go:356-381 (loadText stores cut text + cut flag, once); backend-go/internal/tools/native/subagent_async.go:122-145 (Detail calls loadText(maxBytes) with the detail cap, then cuts for display)
- evidence: [read] trace: `Detail` on an ended restored run calls `run.loadText(int64(maxBytes))`; `loadText` sets `textLoaded=true`, writes `r.result.Text=text` cut to that `limit`, and ORs `r.cut`. Any later read (later Detail with a larger limit, async wait/status paths via `view()`) sees the already-truncated text and `cut=true`, and never reloads (`textLoaded` guard). The stored answer should be cut to `result_max_bytes` (default 32 KiB), but the Detail cap that wins the first-call race is the async detail limit, which is the smaller number in practice. Needs a restart (restored=true) plus a small-limit Detail before a full read to trigger.
- suggested fix: do not let Detail choose the persistence limit: load with the run's `resultMax` (thread it into the driver or the run) or split "load once at resultMax" from "cut for display"; at minimum `Detail` should cut only its return value, never `r.result.Text`/`r.cut`.
- effort: S
- prior art: new

### P6C-R3-02 32-bit run/handle ids are installed without a collision check
- severity: low
- lens: A correctness
- location: backend-go/internal/tools/native/subagent_manager.go:474-522 + 581 (`newSubagentRunID` at 900-906, `m.runs[id] = run` at 581 with no existence check); backend-go/internal/tools/native/subagent_remote.go:108-125 (`rs_` + 4 random bytes, `Register` without pre-check); backend-go/internal/tools/native/profile_message_async.go:140-160 (`pm_` + 4 random bytes, `Register` without pre-check)
- evidence: [read] `start` generates 4 random bytes (32-bit space) and unconditionally stores under it; a repeat id overwrites a live map entry (old run's `finish`/`dropOldestLocked` then operate on the wrong entry, `finished` can hold a duplicate id). Same shape for the two async id kinds. Probability per start ~ n/2^32; no retry loop.
- suggested fix: loop `newSubagentRunID` (and the `rs_`/`pm_` generators) until the id is absent (`m.runs` / `Manager().Status` miss), or check-and-retry once.
- effort: S
- prior art: new

### P6C-R3-03 Per-session / per-root run caps only gate nested starts; top-level fan-out bypasses them
- severity: low
- lens: A correctness
- location: backend-go/internal/tools/native/subagent_manager.go:483-522 (nested-only `maxPerRoot`/`maxPerSession` check at 501; `byRoot`/`bySession` incremented for every run at 521-522)
- evidence: [read] `start` increments `byRoot`/`bySession` for top-level runs too, but the refusal branch runs only when `nested` (`spec.depth > 0`). A session can therefore exceed `max_runs_per_session` (default 32) with top-level starts alone; the only brakes are `max_concurrent` (default 3) + `max_queued` (default 32), which are shared per storage dir across sessions. Conditional and self-limiting, hence low.
- suggested fix: decide whether the caps are "descendants only" (then rename/document `MaxRunsPerSession` and stop counting top-level runs) or "session total" (then check before the `nested` branch). File-only change in `start` + settings text.
- effort: S
- prior art: new (DECISIONS describes the caps as descendants/session-live; behaviour matches "descendants" for root via the `-1`, but the session counter mixes both)

### P6C-R3-04 Remote-entry errors render server file detail into model-visible text
- severity: low
- lens: B security
- location: backend-go/internal/tools/native/profile_message_remote.go:89-97 (`remoteProblem`); reached from profile_message_remote.go:78-88 and subagent_remote.go:115-125
- evidence: [read] non-`ErrNotFound` loader failures are interpolated verbatim (`"Remote %s cannot be used: %v"`). The comment asserts loader errors "name files and keys only" — that is still server filesystem layout (remotes-folder paths) disclosed to the model, which is the untrusted side of this boundary. `ErrNotFound` is already mapped to a name-only message; extend the same mapping (or a bounded reason) to the loader-error branch.
- suggested fix: map loader failures to `remote_unavailable` with the entry name only (log the detail server-side), same as the NotFound branch.
- effort: S
- prior art: new

### P6C-R3-05 Session allow uses globs, deny is exact-only: a whole session cannot be denied
- severity: low
- lens: D maintainability (with B hardening note)
- location: backend-go/internal/tools/native/profile_message.go:377-392 (`localTarget`: `path.Match(glob, session)` allow vs `slices.Contains(p.settings.DenyProfiles, target)` exact deny)
- evidence: [read] allow entries are `path.Match` globs over the session id (`*` allowed); deny entries are exact `category/profile` matches with no glob evaluation, so there is no spelling for "deny session S" or "deny prefix". A bad glob fails closed (denies; safe) but silently. Behaviour is fail-closed, hence low/hardening.
- suggested fix: document the asymmetry on the setting, or evaluate deny entries with the same `path.Match` semantics (exact entries keep working under glob match).
- effort: S
- prior art: new

### P6C-R3-06 `subagentTool.blockFor` dereferences a nil async host; the sibling guards
- severity: low
- lens: D maintainability
- location: backend-go/internal/tools/native/subagent_call.go:168-174 vs backend-go/internal/tools/native/profile_message_async.go:91-99 (nil guard present)
- evidence: [read] subagent `blockFor` calls `s.async.Config()` unconditionally; every current call site guards it indirectly (`handle != nil` implies `s.async != nil` at subagent_call.go:87-100 and subagent_remote.go:103-140), so this is latent, not live. The profile_message twin shows the intended shape.
- suggested fix: add the same `if s.async == nil { return 0 }` guard.
- effort: S
- prior art: new

### P6C-R3-07 Changed-files prefix can push an async summary past the result limit
- severity: low
- lens: A correctness
- location: backend-go/internal/tools/native/subagent_async.go:56-63 (`subagentAsyncResult` prepends up to 200 bytes of `changedFilesText` to an already at-limit summary)
- evidence: [read] `finish` cuts the answer to `resultMax` (manager.go:~640-650), then `subagentAsyncResult` prepends `files + "\n"` (bounded by `subagentAsyncFilesBytes=200`) without re-cutting. Retained async summary can exceed the configured limit by up to ~200 bytes. Narrow and bounded, hence low.
- suggested fix: cut the composed summary to the run's limit after prepending (or count the prefix against the limit before storing).
- effort: S
- prior art: new

### P6C-R3-08 Duplicated cancel/collection loops (four Cancel* variants, two listers)
- severity: low
- lens: D maintainability
- location: backend-go/internal/tools/native/subagent_manager.go:756-862 (`ownedRuns`/`allRuns`; `CancelOwner` 785-799, `CancelUnmanagedOwner` 801-815, `CancelUnmanagedOwnersWithPrefix` 817-839, `CancelOwnersWithPrefix` 841-862)
- evidence: [read] four methods repeat "snapshot under `m.mu`, then `active()`+cancel" differing only in the owner/prefix/async-nil predicate; the two listers differ only in the filter. No behaviour issue found.
- suggested fix: one filtered snapshot helper + one cancel helper; keep the four exported wrappers. File-only refactor.
- effort: S
- prior art: new

### P6C-R3-09 Oversize files and multi-duty ~100-line functions
- severity: low
- lens: D maintainability
- location: backend-go/internal/tools/native/subagent_manager.go (914 lines), backend-go/internal/tools/native/subagent.go (726 lines); backend-go/internal/tools/native/subagent_call.go:56-155 (`startRun`), 267-356 (`renderRun`); backend-go/internal/tools/native/subagent_remote.go:76-180 (`startRemote`); backend-go/internal/tools/native/profile_message_async.go:143-241 (`runHandle`)
- evidence: [read] both files exceed the ~700-line guide; the listed functions each exceed ~80 lines and mix validation, reservation, handle/registration, waiting and rendering. No behaviour claim.
- suggested fix: split render vs start paths (e.g. `render_*` helpers already partially exist) and/or move the Cancel* group (P6C-R3-08) and the store/manifest types out of the manager file. Docs-only if kept.
- effort: M
- prior art: new (size; not behaviour)

## 4 Performance hypotheses

- H01: per-change manifest fsync. `recordChange` (subagent_changes.go:19-35) calls `store.save` (temp file + `Sync` + rename) on every reported file change, from child goroutines. Workload: writer agents with `changed_files_max` up to 10,000 changing hundreds of files. Benchmark: new micro-benchmark driving `recordChange` with N=100/1000 files on tmpfs, or `BenchmarkPersistManifestParallel` (existing) extended with a changes workload. Expected: syscalls + serialization scale O(files); batching/coalescing saves ~1 fsync per change.
- H02: restored-run transcript scan per Detail. `Detail` (subagent_async.go:122-145) → `loadText` → `readTranscriptAnswers` (subagent_answer.go:39-88) scans the whole `messages.jsonl` with `ReadBytes('\n')` (longest line unbounded) even for a small `maxBytes`. Workload: restored run with a large transcript (large tool results), repeated status/detail polls. Benchmark: new fixture with a transcript of S=1 MiB/8 MiB tool-result lines, Detail at small maxBytes. Expected: time scales with transcript bytes, not maxBytes.
- H03: unbounded answer join before the cut. `SubagentAnswerText` (subagent_answer.go:16-51) joins all `output_text` parts, `finish` (manager.go) cuts to `resultMax` afterwards. Workload: child answers totalling megabytes. Benchmark: extend `BenchmarkSubagentListManyFinished`-style fixture or a unit bench over a synthetic completed-response event of 1 MiB/8 MiB. Expected: peak allocation = full answer regardless of limit.
- H04: statecontext copies per run. `CollectSubagentStateContext` (subagent_statecontext.go:38-80) via `list` (manager.go:~847-862) copies every retained run's view including its `files` slice (up to `changed_files_max`), then filters/sorts. Workload: `keep_finished` up to 1000 with large file lists, compaction snapshots. Benchmark: new bench with R=50/500 retained runs × F=100 files. Expected: O(R·F) copy per snapshot.
- H05: per-request description cost. `subagentAgentList` (subagent.go:434-456, bounded by `list_max_bytes` up to 1 MiB) and `profileMessageListText`/`remoteListText` are rebuilt into the tool description per request. Workload: 100s of agents / long session+remote lists. Benchmark: existing description/list tests as a bench, or measure `NewSubagentTool*` + description length at 40/100/600 agents (DECISIONS already characterises 40/100/600 tiers qualitatively). Expected: standing per-request bytes scale with agent count tiers.
- H06: MCP catalog build under the write lock. `bindMCP` (subagent.go:559-618) builds a `tools.NewCatalog` per agent while holding `s.mu` (write), blocking `selectAgent` readers; startup-only. Workload: many agents × many MCP tools. Benchmark: bench `BindToolsets` with A×T matrix. Expected: lock hold time scales with agents × tools.

## 5 Cross-phase leads

- Remote hop-limit enforcement lives outside p6c: p6c sends `RemoteHops+1` (subagent_remote.go:127; profile_message_remote.go:80, 115-119) and renders `remote_hop_limit` (profile_message_remote.go:157-183), but the refusing check is in `remote`/`remoteapi` + server routes. Verify the numeric limit and its test from phase 4; p6c itself does no local pre-check (by design — each server enforces its own).
- Hub shutdown wait for remote follower goroutines (`directory.Go` in `startRemoteRun`, profile_message_remote.go:121-139; follower `ended` channels in subagent_remote.go:150-180, profile_message_async.go:195-230): whether `Close` waits for the best-effort 3 s `stopCallTimeout` stops is a `remote.Directory` property. p6c callers handle both outcomes; the wait itself is out of scope.
- Child model/provider behaviour and the launcher (`SubagentLauncher.Run`, app package), transcript-tool owner checks (`RunTranscript` consumers), and `asyncmgr` capacity/detail limits are consumers of p6c outputs (`subagentAsyncResult`, `asyncResult`, `Result`/`Detail`/`latest`, `subagentRunRef`/`transcriptRef`); no issue found at the p6c seam beyond P6C-R3-01/07.
- Resolver shared-manager aliasing (two paths aliasing one storage dir build two managers whose recoveries can overlap) is recorded in DECISIONS and unchanged here; noted, not filed.
- `ExecutionContext.ForNested` (tools/catalog.go:86-106) now carries depth/root/hops (fix `edcca31`); p6c's `childExecutionContext` (subagent_call.go:42-54) relies on it and the `catalog_nested_test.go` pins (`TestForNestedKeepsEveryRunScopedField`, `TestForNestedKeepsTheThreeRunFields`). The tool_smith bridge half of P6BM-01 is phase 6b; p6c side verified sound.

## 6 Checked and sound

- Shutdown/Close class (`176fd7d` lens): `SubagentManager.Close` (manager.go:866-878) sets closed under lock, releases it before `cancel`+`active.Wait`; `execute`/`acquire`/`release`/`finish` accounting traced (queued vs running slots, early-return path skips `release` correctly, `undoReservationLocked` on failed `begin`); `CancelOwner*` snapshot-then-cancel without holding `mu` across `cancel()`; targeted test `TestSubagentManagerCloseStopsDetachedChildrenAndWaitsForThem` passes.
- Depth + caps: double gate (`startRun` at subagent_call.go:66-72 and `start` nested check at manager.go:494-501); nested-no-queue rule (`errSubagentNoSlot` so a slot-holding parent cannot deadlock on its own queued child); parent-derived context (`parentContext` from `m.runs[parentID]`, manager.go:505-513) so stopping a run cancels descendants (test `TestStoppingARunCancelsItsDescendantsAndLeavesNoGoroutines` passes); `handleOwner` (subagent_call.go:160-165) scopes nested handles to the run id with `ProcessAsync`.
- Writers: `depth==0` + `reserveWriter` + `backgroundSafe` gating (subagent_call.go:73-100); writer-without-handle waits with no timer bound and cancels the run on caller cancel (`await`, subagent_call.go:179-208); `BackgroundWriters`/`releaseWriter` pairing on start-failure and finish paths.
- profile_message: `MaxQueued=0` inline fail-fast when nobody can collect (profile_message.go:345-350); `Blocking = !handled || block>0` cycle semantics; `WithChildCall` from `AgentDepth>0` (profile_message.go:353-356); `unknownProfile` identical for denied vs unknown (profile_message.go:394-399); `localTarget` rejects `.`/`..`, enforces `SessionPattern` + `localProfilePattern`, glob-match sessions (profile_message.go:377-392; traversal test passes).
- Remote: `remote_unavailable`/`unknown_remote`/`remote_not_allowed` pre-checks before any dial (both tools); `startError` maps `profile_busy`/`profile_cycle`/`remote_hop_limit`/auth/session/target distinctly; `conclude` orders answer > shutdown > timeout > lost+stopRequested > lost > mapped failure; `stopOnce` + 3 s best-effort stops on both user stop and timeout/shutdown paths; token-redaction tests exist (`TestRemoteProfileMessageBusyAndTokenErrorsNeverContainTheToken`).
- Persistence: manifest schema checks (version/id/state/start), `insideDirectory` + `transcriptFolderUnder` confinement (store.go:147-183), foreign-live-server skip, `keep_finished` prune + `removeTranscripts` after unlock, best-effort writes that never fail the run; reason bounded to 400 bytes (`boundedReason`, subagent.go:719-726), task preview to 80 bytes (`subagentTaskPreview`, manager.go:908-914), answers cut once at `finish` to `resultMax`.
- Concurrency hygiene: `run.mu`/`m.mu`/`store.mu` discipline with unlock-before-`save` (no store↔run inversion); `byID` pointer discipline; `view()` copies incl. files; `loadText` single-flight via `textLoaded` (modulo P6C-R3-01's limit choice); launcher panic recovered in `runLauncher` (D-1 boundary, manager.go:~625-635; test `TestSubagentLauncherPanicBecomesAFailedRun`).
- Panic (D-1) on goroutines: `runLauncher` recovers; remote/profile followers perform no user-callback work that needs a boundary beyond the launcher's; no `recover` gap found in scope (app launcher internals not covered).

## 7 Not covered

- `remote/` + `remoteapi/` + `app` wiring internals (TLS, redirects, proxy, token loading from env/files, server auth/glob enforcement, hop-limit value, hub `Go`/shutdown waits), the app `subagentLauncher` (model/credential/transcript/rounds/tokens), `asyncmgr` internals (capacity, `Complete`/`MarkRead`/`MarkRunning` persistence across restarts), transcript/statecontext consumers, `sessionfs` settings validation (phase 2a), and anything outside `subagent*`/`profile_message*`/`statecontext_counts.go` except the `ForNested`/`PathScope` reads above.
- Full-suite, `-race`, benchmarks, or profiles (forbidden in this step); only the targeted `-count=1` runs in the header.
- The two known-flaky tests (`TestRemoteProfileMessageCloseEndsFollowersWithoutLeaks`, `TestRemoteSubagentCloseLeavesNoGoroutines`) were deliberately not run; the nearby non-flaky Close test was run instead.

## 8 Command log

- `go vet ./internal/tools/native/` (from backend-go/): exit 0, no output.
- `gofmt -l <12 in-scope non-test files>`: exit 0, no output.
- `go build ./internal/tools/native/`: exit 0.
- `go test ./internal/tools/native/ -run 'TestNestedStartIsRefusedBeyondMaxDepth|TestSubagentManagerCloseStopsDetachedChildrenAndWaitsForThem|TestProfileMessageRejectsTraversalInASessionTarget' -count=1`: exit 0, `ok ... 0.028s`.
- `go test ./internal/tools/native/ -run 'TestRemoteSubagentHopsAreSentAndLimited|TestRemoteProfileMessageHopsAreSentAndTheRemoteLimitIsReported|TestSubagentCutsALateAnswerToTheResultLimit' -count=1`: exit 0, `ok ... 0.038s`.
- Read-only inspection: `git log --oneline` (HEAD 3bf8c33 confirmed), per-file `grep -n`/`wc -l`, `awk` function-length scans. No writes outside `audit_findings/p6c_r3.md`; no `/tmp/r3` scratch left behind (no probes needed beyond the allowed test runs).
