# Phase p7b (sub-pass 7b) review: reviewer r1

## Header

- Phase: p7b (native tools, transcript, memory, task, hooks, skill, catalog, request_draft)
- Reviewer id: r1
- HEAD sha: 09139f5 (the task brief named 3bf8c33; the tree I read is 09139f5, and `git status` shows only others' uncommitted edits to EXECUTOR.md, MASTER.md, audit_plan.md, and untracked audit files; no backend-go change)
- Date: see the run log in section 7
- Files read (22 non-test files, 7,871 lines; list: the 22 files named in section 1 and the glob set of phasemap p7b):
  - transcript: transcript.go, transcript_cut.go, transcript_detail.go, transcript_path.go, transcript_remote.go, transcript_render.go, transcript_scan.go, transcript_steps.go, transcripts.go, transcripts_list.go
  - memory: memory.go, memory_group.go, memory_json.go, memory_store.go
  - task: task.go, task_store.go
  - hooks: hooks.go, hooks_events.go, hooks_run_events.go
  - skill.go, catalog.go, request_draft.go
  - Read in full by whole-file `sed`: transcript.go, transcript_scan.go, transcript_path.go (lines 1-243), transcripts_list.go (lines 118-465), transcript_remote.go, hooks.go, hooks_events.go, hooks_run_events.go, skill.go, transcript_render.go, transcript_cut.go (lines 1-40), request_draft.go (lines 35-214, 390-470), memory.go (55-266), memory_store.go (1-142, 163-275, 469-476, 572-760), task_store.go (195-206, 249-302, 352-445), task.go (224-300), catalog.go (88-205, 393-481).
  - Read by index only (get_architecture, search_graph, trace via get_code_snippet, zvec-grep): transcript_steps.go (partial, 1-60, 60-200), transcript_detail.go (1-60, 76-145), memory_json.go (113-190), transcripts.go (166-190, 236-248), task.go (77-100).
  - Shared helpers read: app/transcript_profiles.go (Profile, Candidates; whole file), tools/json_bounds.go (names only), sessionfs native_transcript_settings.go (defaults only, via grep).
  - Not read in full: memory_group.go (94 lines, index only), memory_json.go (except 113-190), task.go (except 224-300 and 77-100), task_store.go (only the mutate/load/write path and List), transcript_steps.go (partial), transcript_detail.go (partial), transcripts_list.go (listSessions, listProfiles, listRuns and mayReadRun read; page/runItem read).
- Reference for code: the index (codebase-memory `search_graph`, `get_code_snippet` with qualified names, `zvec-grep`) and `read.sh`-style `sed -n` on exact ranges (fallback used; see section 7).

## 1 Understanding note

The p7b files are the model-facing "read the past" and "store state" tools. Four groups:

1. **transcript and transcripts** read `messages.jsonl` files: the caller's own, another profile's or another session's (`profile:<session>/<category>/<name>`, `subagent:<session>/...`), a file by path (off by default), or a remote JAD server (`remote:<name>/...`, behind `allow_remote` and an allow-list). The gate is in `transcript.go:189` (`allow_children`), `transcript_path.go:71-203` (`resolve`: selector to files, with `profileRefusal`, `deniedFile`, `runOfTarget`), `transcripts.go:238` (`gate`), `transcripts_list.go:161-456` (listing, with `mayReadRun`). Cursors are base64 JSON with the selector inside; each follow-up re-runs the resolver (`transcript.go:172-177`). Remote paths are validated by `transcript_remote.go:59-108` and sent through `remote.Directory`.
2. **memory** and **task** are JSON record stores under a storage dir, serialised by `PathLocks` (memory) or a per-store mutex (task); memory writes go through `atomicWrite` (temp, rename).
3. **hooks** run configured argv commands after a tool call (`pre_tool`, `post_tool`, post-edit checks, run events) with the hooks' own env and timeouts.
4. **skill, request_draft** load library entries and ask the user to accept a draft. **catalog.go** is the wiring: it builds the catalog and wraps tools with hooks.

Entry points traced: `transcript.go` (execute), `transcripts.go` (execute, gate), `transcript_path.go` (resolve), `app/transcript_profiles.go` (`Profile`, `Candidates`, the messenger that answers the tools), `catalog.go` (`BuildCatalogWithAsync`, `buildNativeTools`), `hooks.go` (`WithPostEditHooks`), `hooks_events.go` (`WithToolEventHooks`), `task_store.go` (`mutate`, `write`), `memory_store.go` (`CreateValue`, `UpdateValue`, `totals`, `atomicWrite`).

Prior material read: audit_plan.md sections 1.1, 3.2 (p7b row, phasemap), 3.3 (p7b row), 4, 5; `audit_backend.md` F-11, F-13 rows (F-13 "memory quota scan stats every record" is fixed by commit `5d09534`); `DECISIONS.md:940` (approval removed; `request_draft` keeps `headless_auto_accept_timeout_ms`). I did not open any other `audit_findings/` file.

## 2 Hypotheses

| id | statement | status |
|---|---|---|
| H1 | Child runs (AgentDepth>0) reach transcripts only with `allow_children` on, on every path (`transcript`, `transcripts`, remote, listing). | verified (transcript.go:189, transcripts.go:169, transcript_remote.go:290, transcript_path.go:161-163, transcripts_list.go:416) |
| H2 | A cross-session read needs `allow_other_sessions`: profile, subagent, runs listing, sessions listing. | verified for reads; **disproved for the not-found path**, see F1 |
| H3 | A cross-profile sub-agent read needs `allow_other_profiles`. | disproved as a finding: `subagent:` branch calls `profileRefusal` (transcript_path.go:135); test TestTranscriptSubagentOfAnotherProfileKeepsTheAccessSwitches passes |
| H4 | A forged or cross-session cursor cannot read another selector. | verified: cursor re-resolves its selector (transcript.go:172, :229) with the same gates; negative offsets refused (transcript.go:149, commit 084ae6a) |
| H5 | Remote transcript reads obey `allow_remote`, the remote name allow-list and the entry's profile globs before any network call. | verified (transcript_remote.go:111-119 `check`, :59-108 `selector`, :234-245) and tested (TestRemoteSelectorGrammar, TestRemoteTranscriptCursorIsWrappedAndContinues passes) |
| H6 | `messages.jsonl` reasoning is shown only when asked (`part=reasoning`), never in list mode. | verified (transcript_detail.go:32-39, transcript_steps.go:308-312) |
| H7 | A detail range's memory is bounded by the page size. | **disproved, see F2** (measured) |
| H8 | Memory and task writes are durable across a crash (fsync before rename). | disproved: no Sync in memory_store.go or task_store.go (grep, empty for p7b) |
| H9 | Hook argv cannot be influenced by model input (program fixed, placeholders only in args). | verified (hooks.go:201-217; hookArguments replaces only argv[1:]; test TestHookArgumentsReplacePlaceholdersButNeverTheProgram) |
| H10 | Hook commands and run events never change the call result beyond the report, and blocking applies to pre_tool only. | verified (hooks_events.go:50-75; hooks_run_events.go:55-78) |
| H11 | Memory per-write cost is bounded by the quota scan, not by a stat per record (F-13). | verified fixed (5d09534); residual per-write ReadDir: C-low hypothesis |
| H12 | `request_draft` cannot block forever: timer and context both end the wait. | verified (request_draft.go:117-127) |
| H13 | Transcript reads are bounded in memory by the response size. | disproved for `detail` ranges (F2); list mode is bounded (transcript.go:340-350 caps lines and bytes) |
| H14 | A model cannot read another server's tokens, paths or hop counts beyond the switch. | unverified: remote hops are caller-supplied (commit 64e407b docs it); out of scope for this phase, see section 5 |

## 3 Findings

Severity and lens follow audit_plan.md section 5.1. Evidence tags: [measured] = I ran it and saw the result; [read] = traced in source; [unmeasured] = a hypothesis.

### P7B-R1-01  Transcript error names other sessions when allow_other_sessions is off

- severity: medium
- lens: B security
- location: backend-go/internal/tools/native/transcript_path.go:207-218 (lookupProfile, called at :130 and :189), transcript_path.go:158 (no_such_run notFound); backend-go/internal/tools/native/transcripts_list.go:335-338 (runs listing); backend-go/internal/app/transcript_profiles.go:54-70 (Candidates lists every catalog session when the named session is not a layout session)
- evidence: [measured] probe `TestProbeCandidatesLeakWhenOtherSessionsOff` through `go test -overlay /tmp/r1/overlay.json ./internal/tools/native/ -run TestProbeCandidatesLeakWhenOtherSessionsOff -count=1 -v`, exit 0. Settings: `AllowOtherSessions: false`, `AllowOtherProfiles: false`, `AllowChildren: false`. Call `transcript` with `path: "profile:nosuch/dev/coder"`. Output: `status not_found`, `error_code no_such_profile`, `available: demo, secret-client-session`. The fake `TranscriptProfiles` stands in for `app.profileMessenger`, whose `Candidates` returns every catalog session ID for a non-layout session (app/transcript_profiles.go:65-69). The switch check (`profileRefusal`, transcript_path.go:135 and :194) runs only after `lookupProfile` succeeds, so the not-found branch skips it. The fixture in transcript_select_test.go:113-128 sets `AllowOtherSessions: true`, so no existing test covers this.
- suggested fix: in `lookupProfile` (transcript_path.go:207) and `runs` (transcripts_list.go:335), when the session differs from the caller's and `AllowOtherSessions` is off, pass an empty session list (or name only the caller's profiles) to `notFound`. Smallest change: return `"transcript_session_denied"` before `Candidates` when `session != "" && session != callerSession && !AllowOtherSessions`. Add a test with `AllowOtherSessions: false` in transcript_select_test.go.
- effort: S
- prior art: new. It is the reverse of the closed decision on names in not-found errors (commit 226f648); that commit adds the valid values, not a session oracle.
- who can reach it: the model in any session (the tool is on by default for a session with `native.transcript` on). The leak is names of other sessions, not their content. Medium, not high, because the names are the local catalog and the caller already sees session IDs through `transcripts` only when the switch is on.

### P7B-R1-02  A detail range holds the full text of every step in it before paging

- severity: medium
- lens: C performance (lens B for the bound on untrusted input: model-chosen range)
- location: backend-go/internal/tools/native/transcript_detail.go:101-145 (detail: walk keeps the whole range, then pages), backend-go/internal/tools/native/transcript_steps.go:60-62 (`keeps`), :164-179 (`result` stores `call.result`/`step.result`), transcript_steps.go:138-162 (`reasoning` keeps the text in full); backend-go/internal/sessionfs/native_transcript_settings.go:12-13 (ScanMaxBytes default 256 MiB, ceiling 4 GiB)
- evidence: [measured] probe `TestProbeDetailRangeHeld` (overlay /tmp/r1/overlay2.json, exit 0). Transcript: 400 turns, each with a 64 KiB tool result (about 25 MiB file). Call `transcript` with `step: "1.1-400.1"`, `ResponseMaxBytes` 16 KiB. Output 16,828 bytes. `runtime.MemStats.TotalAlloc` delta: 240 MiB. Same file, `step: "1.1-1.1"`: delta 1 MiB. Allocation scales with the range, not with the returned page. Single run each; workload and command are above. Model-reachable: the `step` argument is any range the model writes; the cap is `scan_max_bytes` (default 256 MiB, so one call can hold up to about 256 MiB of text, and up to 4 GiB if an operator raises the limit).
- suggested fix: cap the kept bytes in `detail`: stop keeping `text` once the kept total passes `detailPageBytes()+offset`, or count the kept size during the walk and return a `range_too_large` error with the step number to narrow to. Files: transcript_steps.go (`keeps`/`full`), transcript_detail.go (`detail`), transcript.go (error text). Add a test with a range over a large result that asserts the walk stops keeping text.
- effort: M
- prior art: new. Related to F-12 (`read` loads the whole file for a short range), which is an open performance item and a different surface.

### P7B-R1-03  Memory and task writes are not fsynced before rename

- severity: low
- lens: A correctness (crash durability)
- location: backend-go/internal/tools/native/memory_store.go:707-734 (atomicWrite: no Sync before Close and Rename), memory_store.go:736-760 (nextSequence writes .memory-sequence with the same path); backend-go/internal/tools/native/task_store.go:400-439 (write: CreateTemp, Write, Close, Rename; no Sync)
- evidence: [read] `grep -n "Sync()" transcript*.go memory*.go task*.go` returns nothing in these files; the same package's transcript store (backend-go/internal/transcript/front_compact.go:240, state_context.go:78, cursor_cache.go:259) does call `Sync()`. After a power loss, the rename can persist while the data does not, leaving a zero-length `task` document (which `load` refuses as invalid JSON, so the store then reports every call as storage error) or a zero-length memory record (which `load` returns as malformed). Not reachable in normal use; crash only.
- suggested fix: call `file.Sync()` (memory) and `temporary.Sync()` (task) before Close. Smallest change: one line in each write function. Add no new test; the crash case cannot be tested here.
- effort: S
- prior art: new. audit_backend.md P-012..P-014 covers the transcript journal, not these stores.

### P7B-R1-04  Task list clones and sorts every matching task, then keeps `limit`

- severity: low
- lens: C performance
- location: backend-go/internal/tools/native/task_store.go:249-302 (List: `cloneTaskRecord` per match, then `sort.Slice` over all matches, then slice to limit); backend-go/internal/tools/native/task.go:250-265 (executeNativeTask list branch builds a filter and calls List)
- evidence: [unmeasured] Read. Workload: `max_tasks` default 1,000 (sessionfs/native_task_settings.go:12); each list call (no filter, `status` all) loads and parses the whole document (`load`, task_store.go:365), clones every match, sorts them, and returns `limit` (default 100). Expect a few hundred microseconds at 1,000 tasks; the benchmark that exists is `BenchmarkTaskStoreList100` (task_test.go:225-246).
- suggested fix: select the top `limit` with a bounded insertion or heap, and clone only the returned records. Files: task_store.go. Optional; measure first.
- effort: S
- prior art: new.

### P7B-R1-05  Memory quota scan lists every type directory on every write

- severity: low
- lens: C performance
- location: backend-go/internal/tools/native/memory_store.go:611-655 (totals: `ReadDir` of the root, then `filesKnowing` (578-602) per type directory); called from CreateValue (memory_store.go:163-205) and UpdateValue (230-269)
- evidence: [read] F-13 (stat per record) is fixed by commit `5d09534` (`sizes` map). What remains is one `os.ReadDir` per type directory plus the root on each write, under the store lock. Workload: records bounded by `MaxTotalRecords` and `MaxScanRecords` (sessionfs native_memory settings); each write re-lists them. [unmeasured]. Benchmarks: `BenchmarkMemoryStoreCreateWithExistingRecords` (memory_benchmark_test.go:38-63).
- suggested fix: none needed now; if the benchmark shows it, keep a directory mtime check per type and re-list only changed directories. Expect no change for typical sizes.
- effort: M if acted on
- prior art: F-13 fixed by 5d09534; residual only.

### P7B-R1-06  Hook check reads the watched file twice with no size limit

- severity: low
- lens: C performance
- location: backend-go/internal/tools/native/hooks.go:233-240 (fileDigest: `os.ReadFile` of the whole file), called at hooks.go:123 and :158 for each post-edit check; hooks_events.go does not call it
- evidence: [read] `runHookChildPassed` calls `fileDigest(hook.watched)` before and after the child runs. The watched file is the file the model just edited, so its size is bounded only by what the edit wrote (the write and edit tools have their own limits, not checked here). Each post-edit check on a large file reads it twice and hashes it. [unmeasured].
- suggested fix: compare size and mtime first, hash only on a change, or hash with `io.Copy` into `sha256` instead of `ReadFile`. Files: hooks.go.
- effort: S
- prior art: new.

### P7B-R1-07  Three ordered-JSON parsers, one per tool, with different duplicate-key handling

- severity: low
- lens: D maintainability
- location: backend-go/internal/tools/native/request_draft.go:390-473 (decodeOrderedRequestDraft, decodeRequestDraftNode, sortRequestDraftObjectMembers, plus encoder appendRequestDraftJSON at 274-336); backend-go/internal/tools/native/memory_json.go:113-188 (parseMemoryJSON, parseMemoryJSONValue, with a `positions` map) and the encoder memoryJSONPretty (memory_json.go:280-286); backend-go/internal/tools/json_bounds.go:12 and :120 (DraftJSONBytes, DraftJSONDepth, a third measurement of the draft)
- evidence: [read] Same job (keep object key order from a model JSON, sort indexes, encode in the same shape); `request_draft` looks up duplicates with a linear scan (request_draft.go:427-431), `memory_json` with a map (memory_json.go:136-153). Duplicate-key semantics are the same (last value wins, first position kept) but the code is written twice and the two lookups differ in cost (see P7B-R1-08).
- suggested fix: move the ordered decoder to one helper (for example in tools, next to json_bounds.go) used by both files. Only do this if a third user appears or when the two need to change together; rewriting now is out of scope.
- effort: M
- prior art: new.

### P7B-R1-08  request_draft dedupes object keys with a linear scan (quadratic in member count)

- severity: low
- lens: C performance
- location: backend-go/internal/tools/native/request_draft.go:427-431 (for each key, `for index := range node.members` compares every earlier member)
- evidence: [read] A draft object with n keys costs O(n^2) string compares. The draft is capped by `max_draft_bytes` (checked before decoding, request_draft.go:70-79), so the worst case is bounded by that setting and a JSON of n short keys. For n=1000 keys that is about 500k compares, which is small. Not reachable as a hang. [unmeasured]
- suggested fix: use the `positions` map pattern from memory_json.go:136-153. Files: request_draft.go.
- effort: S
- prior art: new.

### P7B-R1-09  Memory tool returns raw OS errors, including the absolute storage path, to the model

- severity: low
- lens: B security (secrets and paths in results; divergence from the transcript tools, which hide paths with `hide_paths`)
- location: backend-go/internal/tools/native/memory.go:249-268 (memoryExecutionError: the last branch returns `err.Error()`); memory_store.go:527-539 (ensureRoot), :665-692 (load: `os.ReadFile(path)` errors carry the path)
- evidence: [read] An I/O error (for example permission denied on a record) is returned as `memory_operation_failed` with `err.Error()`, which contains `root/type/name` as the absolute path. The transcript tools expose the path too but gate it with `hide_paths` (transcript.go:419). The model is the reader here and can already list the memory directory through `memory` (`list` returns refs with the same directory), so this is a consistency item, not a new disclosure. [read]
- suggested fix: map non-`MemoryStoreError` errors to a fixed text with a short reason (for example `errors.Is(err, fs.ErrPermission)`) and keep the full error in the log. Files: memory.go.
- effort: S
- prior art: new.

### P7B-R1-10  Detail and list transcript reads rescan the whole file on every call

- severity: low
- lens: C performance
- location: backend-go/internal/tools/native/transcript.go:304 (indexTranscriptTurns over the whole file on every call), transcript_scan.go:95-127; the walk then scans again (transcript.go:362-364 and transcript_detail.go:112-115)
- evidence: [read] Each `transcript` call reads the file twice: once for turn starts (transcript_scan.go), then the selected range again for steps. Bounded by `scan_max_bytes` (256 MiB default). The incremental index `indexTranscriptTurnsFrom` exists and is used only by `async_latest.go:301`. Cursor paging re-reads from the cursor's offset, so page 2 is not a full scan, but page 1 is. [unmeasured]. Benchmark: none in this package for the transcript read.
- suggested fix: none now. If measured, cache the turn index keyed by the file identity and size (the cursor already carries `Identity`). Measure first.
- effort: M if acted on
- prior art: new. Related to F-07 (transcript commit scans), which is phase 8.

### P7B-R1-11  Unused-by-production BuildCatalog paths pass a nil task store

- severity: low
- lens: D maintainability
- location: backend-go/internal/tools/native/catalog.go:50-58 (BuildCatalog, BuildCatalogWithLocks, BuildCatalogWithProcessManager); catalog.go:398 (`NewTaskTool(*config.NativeTask, taskStore)`) and task.go:83-85 (nil store creates a private store)
- evidence: [read] `grep -rn "BuildCatalog(\|BuildCatalogWithLocks(\|BuildCatalogWithProcessManager("` over non-test Go finds no production caller outside catalog.go. Production builds go through `BuildCatalogWithAsync` with resources from the resolver (resolver.go:775-786 shares one task store per storage key). The nil fallback means two catalogs on the same file have two mutexes, so a concurrent test could race a file that production never shares. [read]
- suggested fix: none required; note for the test owners, or remove the no-resource constructors if no test needs them. Check with grep first.
- effort: S
- prior art: new.

### P7B-R1-12  Transcript and transcripts tools are not wrapped by hooks; reads are documented as read-only

- severity: low (note; expected to stay)
- lens: D maintainability (divergence check)
- location: backend-go/internal/tools/native/catalog.go:107-141 (transcript, transcripts built before the hook wrapping at :477); catalog.go:477 (WithToolEventHooksAll covers the list built by buildNativeTools only)
- evidence: [read] `tool_event` hooks fire for every native tool in `buildNativeTools` (catalog.go:477) and for subagent and profile_message (catalog.go:164, :195). `transcript`, `transcripts` and `request_draft` are appended after that wrapping, so a `pre_tool` hook does not see a transcript read. `request_draft` is intentional (it is a question). For the two read-only transcript tools, that is consistent with "read-only" but it is a difference from the other native tools that nobody has documented. [read]
- suggested fix: say it in DECISIONS.md if intended; otherwise wrap them like the rest. Decision challenge, not a defect.
- effort: S
- prior art: new.

## 4 Performance hypotheses (section 5.3)

- H01: transcript detail range keeps full text of every step in the range, file:line transcript_steps.go:60-62, transcript_detail.go:101-145.
  workload: a 25 MiB transcript with 400 turns of 64 KiB results; range 1.1-400.1 (measured 240 MiB, see P7B-R1-02).
  benchmark: new `BenchmarkTranscriptDetailRange` in transcript_detail_test.go, fixture `detailTranscript` with N turns and a result size parameter; sizes 1, 10, 100 turns.
  expected: allocation scales with the range size; a capped walk makes it constant at the page size.
- H02: transcript read rescans the file per call, twice (index then walk), transcript.go:304 and transcript.go:359-362.
  workload: 256 MiB transcript (default `scan_max_bytes`), list mode, page 1 vs page 2.
  benchmark: new `BenchmarkTranscriptListPage` over `writeTestTranscript` with a generated file of 16 MiB and 256 MiB.
  expected: page 1 cost scales with file size; page 2 (cursor) scales with the bytes after the cursor only for the walk, not for the index.
- H03: memory `totals` lists every type directory on each write, memory_store.go:611-655.
  workload: 5,000 records across 10 types (F-13 workload size).
  benchmark: existing `BenchmarkMemoryStoreCreateWithExistingRecords` (memory_benchmark_test.go:38-63).
  expected: linear in the number of type directories plus entries; should be small after 5d09534.
- H04: task list clones and sorts all matches, task_store.go:249-302.
  workload: 1,000 tasks (max_tasks default), list with no filter.
  benchmark: existing `BenchmarkTaskStoreList100` (task_test.go:225-246), plus a 1,000-task variant.
  expected: O(N log N) with N clones; a top-k selection cuts clones to `limit`.
- H05: hook `fileDigest` reads the edited file twice per post-edit check, hooks.go:233-240.
  workload: edited file of 1, 10 and 100 MiB with one post-edit check.
  benchmark: new small benchmark in hooks_test.go style.
  expected: linear in file size, twice per check.
- H06: request_draft quadratic key dedupe, request_draft.go:427-431.
  workload: draft object with 1,000 and 10,000 keys within `max_draft_bytes`.
  benchmark: new benchmark over `decodeOrderedRequestDraft`.
  expected: quadratic in key count; at 10,000 keys this is visible (about 50M compares).

## 5 Cross-phase leads

- `backend-go/internal/tools/native` remote HTTP route calls `tool.Execute` without `recover` (commit 084ae6a message). Phase 3 (`internal/httpapi`, `app/remote_transcripts.go`) owns the route. file: backend-go/internal/app/remote_transcripts.go:88-123 (remoteBackend.Transcript calls `tool.Execute`).
- Remote hop count is caller-supplied (commit 64e407b docs it). Phase 4 (`internal/remoteapi`) owns hop enforcement. file: backend-go/internal/tools/native/transcript_remote.go:279 and :328 (`execution.RemoteHops+1` forwarded).
- `profileMessenger.Candidates` lists every catalog session through `sessionfs.ListSessionFiles` (P7B-R1-01 root). Phase 2b1 (`internal/sessionfs`) owns the listing. file: backend-go/internal/app/transcript_profiles.go:54-70.
- `read` and transcripts share the "file a model may read" rules through `scopedPolicy`/`transcriptDenied`. Phase 7a owns `path.go`. file: backend-go/internal/tools/native/transcript.go:442-450 (transcriptDenied) and transcript_path.go:173 (deniedFile).
- Transcript store `Sync()` pattern is used in phase 8 but not in the p7b stores (P7B-R1-03). Phase 8 owns `internal/transcript`. file: backend-go/internal/transcript/cursor_cache.go:259.

## 6 Checked and sound

- Child-run gate on every path: `transcript.go:189`, `transcripts.go:169`, `transcript_remote.go:290`, `transcript_path.go:161-163`, `transcripts_list.go:416` (H1).
- Cross-profile subagent read refuses `allow_other_profiles`: `transcript_path.go:135`, test passes (H3).
- Cursor forgery: base64 JSON re-validated, `Bytes<0` refused (`transcript.go:146-153`, commit 084ae6a; `transcript_detail.go:79-93` pageOfText clamp), selector re-resolved with the same gates (`transcript.go:172`, `:229`), identity compared (`transcript.go:315`, `:329`). Test `TestTranscriptCursorIsStaleAfterACompactionOrReset` passes.
- Remote gates before any network call: `check` (transcript_remote.go:111-119), `selector` with NamePattern, SessionPattern, run-id pattern and profile globs (:59-108). Test `TestRemoteSelectorGrammar` passes; app test `TestRemoteTranscriptCursorIsWrappedAndContinues` passes.
- Remote output rewrite touches only the path and cursor lines (`transcript_remote.go:185-230`).
- Reasoning is not listed (transcript_steps.go:308-312) and is shown only with `part=reasoning` (transcript_detail.go:32-39).
- Hook argv: program never replaced by placeholders (`hooks.go:201-217`); pre_tool blocking stops later hooks and post_tool (`hooks_events.go:70-76`); payloads bounded (`hooks_events.go:182-225`).
- `request_draft` waits end by timer or context (`request_draft.go:117-127`); size, depth and title refused before the interaction (`request_draft.go:61-83`). Auto-accept on headless timeout is recorded in DECISIONS.md:940, so it is not a finding.
- `skill` names and output are bounded (`skill.go:202-241`, `cutUTF8`).
- `catalog.go` wraps each tool once: `buildNativeTools` (catalog.go:477), subagent and profile_message (:164, :195); no double wrap.
- Task validation (`task.go:224-300`) and cursor-free list limits.
- `midCut` and `pageOfText` keep rune boundaries and clamp negative offsets (`transcript_cut.go:14-39`, `transcript_detail.go:79-93`).
- `looksLikeTranscript` requires a regular file (`transcript.go:462-481`).
- F-13 (memory stat per record) is fixed by `5d09534` (remembered sizes).
- `go build ./...`, `go vet ./internal/tools/native/`, `gofmt -l internal/tools/native`: clean.

## 7 Not covered, and why

- **Not read in full:** memory_group.go (94 lines; read by index only), memory_json.go (except 113-190 and the encoder names), task.go (except 77-100 and 224-300; `page`, `renderTask`, `collectTaskSummary` by index only), task_store.go (except 195-206, 249-302, 352-445), transcript_steps.go (1-200 read; 200-344 by index only: `visit`, `message`, `call` read, `selectTranscriptSteps` by index), transcript_detail.go (1-60 and 76-145 read; 60-76 by index), transcripts_list.go (listSessions, listProfiles, listRuns, mayReadRun, runItem read; `statTranscriptsFile`, `contextFailure`, `tooMany` by index). Reason: budget and the rule to read exact ranges. Index results for these were consistent with the read parts.
- **Not traced end-to-end:** the `app` side of `Profile` beyond `Candidates` and `Profile` (`sessionfs.Resolve`, `LoadSessionSettings`, `existingSubagentManager`); the remote client (`remote.Directory.Client`, `ReadTranscript`) and its redirects, caps and token scrubbing (phase 4). Those are read only as far as the call sites in p7b.
- **Not measured:** P7B-R1-04, -05, -06, -10 are `[unmeasured]`. Only P7B-R1-02 has a measurement (one run).
- **Tests not run:** the full `tools/native` suite (the brief forbids `go test ./...` and the full suite was not requested). The known flaky tests TestRemoteProfileMessageCloseEndsFollowersWithoutLeaks and TestRemoteSubagentCloseLeavesNoGoroutines were not in scope for p7b.
- **Not covered by design:** race detector, benchmarks, profiles (forbidden for reviewers).

## Commands and exit codes

| # | Command | Exit | Note |
|---|---|---|---|
| 1 | `git rev-parse --short HEAD`; `git status --short`; `git log` on p7b files | 0 | HEAD 09139f5 |
| 2 | `cd backend-go && go build ./...` | 0 | |
| 3 | `go vet ./internal/tools/native/` | 0 | |
| 4 | `gofmt -l internal/tools/native` | 0 | empty list |
| 5 | `go test ./internal/tools/native/ -run '^TestTranscriptSubagentOfAnotherProfileKeepsTheAccessSwitches$' -count=1` | 0 | pass |
| 6 | same, `TestTranscriptsSessionsLevel` | 0 | pass |
| 7 | same, `TestTranscriptNotFoundNamesTheNearestCandidatesUpToTheCap` | 0 | pass |
| 8 | same, `TestTranscriptDetailPagesABigResultWithoutLosingAByte` | 0 | pass |
| 9 | same, `TestMemoryQuotaSeesEveryRecordSizeAfterCreateUpdateAndOutsideChanges` | 0 | pass |
| 10 | same, `TestNativeTaskStoreStateContextAndConcurrentIDs` | 0 | pass |
| 11 | same, `TestHooksRespectTheAgentPathScope` | 0 | pass |
| 12 | same, `TestBlockingPreToolHookStopsTheCallAndReportsWhy` | 0 | pass |
| 13 | same, `TestTranscriptScanStopsOnCancelAndLeaksNoGoroutine` | 0 | pass |
| 14 | same, `TestRemoteSelectorGrammar` | 0 | pass |
| 15 | same, `TestRemoteTranscriptCursorIsWrappedAndContinues` | 0 | `[no tests to run]` in this package; the real test is in `internal/app` |
| 16 | `go test ./internal/app/ -run '^TestRemoteTranscriptCursorIsWrappedAndContinues$' -count=1` | 0 | pass |
| 17 | `go test -overlay /tmp/r1/overlay.json ./internal/tools/native/ -run TestProbeCandidatesLeakWhenOtherSessionsOff -count=1 -v` | 0 | probe, PASS; leak confirmed (P7B-R1-01) |
| 18 | `go test -overlay /tmp/r1/overlay2.json ./internal/tools/native/ -run '^TestProbeDetailRangeHeld$' -count=1 -v` (range 1.1-400.1) | 0 | probe, 240 MiB (P7B-R1-02) |
| 19 | same probe, `step: "1.1-1.1"` | 0 | 1 MiB |
| 20 | `grep -n "Sync()"` over p7b files | 1 | no matches (grep exit 1 means no match) |
| 21 | `git diff --stat -- backend-go` | 0 | empty: no source change |

Probes lived only under `/tmp/r1/` and were applied with `-overlay`; they were not written to the tree (confirmed: `ls ... | grep zz_probe` returned 0 files). The probe sources and overlay JSON were deleted at the end of the run as the reviewer rules require, so rows 17-18 are a record of the run, not commands to re-run as written. The probe code is short: a fake `TranscriptProfiles` whose `Candidates` returns two session names (row 17), and a 400-turn transcript with 64 KiB results read with `step: "1.1-400.1"` under `runtime.MemStats` (row 18).

## 7a Findings summary

| id | severity | lens | title |
|---|---|---|---|
| P7B-R1-01 | medium | B | Transcript not-found error names other sessions when allow_other_sessions is off (measured) |
| P7B-R1-02 | medium | C (B bound) | Transcript detail range holds full text of every step in range (measured, 240 MiB) |
| P7B-R1-03 | low | A | Memory and task writes are not fsynced before rename (read) |
| P7B-R1-04 | low | C | Task list clones and sorts every match (unmeasured) |
| P7B-R1-05 | low | C | Memory quota scan lists type directories per write; residual of F-13 (unmeasured) |
| P7B-R1-06 | low | C | Hook check reads watched file twice unbounded (unmeasured) |
| P7B-R1-07 | low | D | Three ordered-JSON parsers with different duplicate-key handling (read) |
| P7B-R1-08 | low | C | request_draft quadratic key dedupe, bounded by max_draft_bytes (read) |
| P7B-R1-09 | low | B | Memory tool returns absolute storage path in I/O errors (read) |
| P7B-R1-10 | low | C | Transcript reads rescan the file per call (unmeasured) |
| P7B-R1-11 | low | D | No production caller of BuildCatalog; nil task store path is test-only (read) |
| P7B-R1-12 | low | D | transcript/transcripts not hook-wrapped; undocumented divergence (read; decision challenge) |

Counts: 0 critical, 0 high, 2 medium, 10 low. Lens: B 2 (01, 09), C 6 (02, 04, 05, 06, 08, 10), A 1 (03), D 3 (07, 11, 12).

Rating note: P7B-R1-02 is medium because the cost is measured, but it is bounded by an operator setting and needs a large transcript; it is not a hang. P7B-R1-01 is medium because it is a cross-session oracle reachable by the model; it discloses names only.
