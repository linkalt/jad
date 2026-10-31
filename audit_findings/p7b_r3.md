# p7b review — reviewer r3

- phase: p7b (transcript* + memory* + task* + hooks* + skill + catalog + request_draft)
- reviewer: r3
- HEAD: 09139f5416974ef745399e0ae735d6058206e064
- date: 2026-10-30
- files read: backend-go/internal/tools/native/transcript.go (1-481), transcript_path.go (1-243), transcript_remote.go (1-329), transcript_scan.go (1-141), transcript_detail.go (1-145), transcript_steps.go (1-344), transcript_cut.go (1-81), transcript_render.go (1-51), transcripts.go (1-248), transcripts_list.go (1-465), memory.go (1-470), memory_group.go (1-94), memory_json.go (1-447), memory_store.go (1-1062), task.go (1-703), task_store.go (1-520), hooks.go (1-246), hooks_events.go (1-258), hooks_run_events.go (1-78), skill.go (1-242), catalog.go (1-632), request_draft.go (1-591); shared helpers read as needed: path_scope.go scopedPolicy/narrowScope (1-120), path.go resolveExistingTarget/pathPolicyError (23-110), subagent_store.go insideDirectory (189-210), subagent_manager.go RunTranscript/ownedRuns/allRuns/byID (822-862), tools/catalog.go ExecutionContext/ForNested (63-95), app/transcript_profiles.go Profile/Candidates (1-80), app/remote_transcripts.go Transcript/tokenProfiles/remoteTranscriptPath (1-130), sessionfs/native_transcript_settings.go defaults + RemoteAPITranscriptSettings (85-169), sessionfs/native_transcripts_settings.go defaults (27-93), sessionfs/files.go PathWithin (210-240). Tests read as evidence only (names in §6/§7). No audit_findings file other than this one was opened.
- commands: `go vet ./internal/tools/native/` exit 0; `gofmt -l` on the 22 in-scope non-test files exit 0 (no output); `go build ./internal/tools/native/` exit 0; `go test ./internal/tools/native/ -run 'TestTranscriptListsNewestStepFirstAcrossTurns|TestMemoryList|TestTaskList' -count=1` exit 0. vet/build/gofmt/tests run from backend-go/.
- navigation: codebase-memory get_architecture + zvec-grep for orientation; search_code/get_code_snippet/trace_path for symbols; exact line ranges via exec read.sh/sed fallback. search_graph was unavailable (unknown_tool on every call); used search_code to get qualified_names then get_code_snippet instead, and state it here per audit rules. Never dumped a whole Go file into context; counts via wc, greps + bounded ranges only.

## 1 Understanding

p7b is the read-your-own-history half of tools/native plus the small stateful tools. `transcript` reads one messages.jsonl (own transcript by default, `profile:<session>/<category>/<name>`, `subagent:<run id>` (+ 3/4-part forms), `path:<file>` when allow_path, `remote:<name>/...` when allow_remote) in two modes: list (one short line per step, newest first, reasoning never listed) and detail (`step` 3.2 or 3.1-3.4, `part` call/result/reasoning, paged by byte cursor). `transcripts` lists what transcript can read (sessions / profiles of a session / runs of a profile, plus remote). Both enforce AgentDepth>0 child gates, allow_other_sessions/profiles switches, read-path policy via scopedPolicy, HidePaths (remote forces true), cursor identity binding (hash of first line), and remote hop counting (RemoteHops+1). `memory` is a bounded JSON-object store (per-type dirs, UUID filenames, ETag via sha256, stored cursors + legacy cursors, GroupBy) with per-op quota scans. `task` is a single tasks.json store (whole-file load/mutate/write under mu). `hooks` wraps tools with post-edit and pre/post-tool commands (allowlisted env, bounded output, blocking pre_tool) plus run_start/run_end/pre_compact log-only events. `skill` bridges the global skill library (list/read by name). `catalog` builds the per-session tool list and validates definitions. `request_draft` is the RequiresUserDecision prompt (draft bytes/depth/title limits, headless auto-accept, OnInteractionResolved).

Trust boundaries: model-supplied selectors/paths/cursors/filters/limits (transcript/transcripts/memory/task/skill), files on disk (transcripts, memory records, task store, skill dirs, hook programs from session config — local user, not model), remote peers/tokens (remote reader), and owner scoping (SessionKey). Defaults matter: transcript/transcripts AllowOtherSessions/Profiles/Children default true, AllowPath false, HidePaths false locally (true on remote API).

## 2 Hypotheses

| id | statement | status |
|----|-----------|--------|
| H01 | cross-session gates (AgentDepth/allow_children/other_sessions/other_profiles/allow_path/allow_remote) deny by default-off switches and fail closed when profiles/sessions/runs are absent | verified sound except the narrow gaps filed as P7B-R3-04/05 (remote int-drop is correctness, short-form/profile nuance is hardening) |
| H02 | cursor forgery cannot escalate: transcript cursor binds Selector+Source+Identity, transcripts cursor re-gates Level and re-checks filters | verified sound (transcript.go:250-270 identity+End checks; transcripts.go:parse/gate re-check) |
| H03 | path traversal/symlink escapes are blocked for path: reads and listings | verified sound (splitTranscriptName rejects ./../backslash/NUL; resolveExistingTarget + statTranscriptsFile symlink-inside-folder + deniedFile/scopedPolicy checks traced) |
| H04 | memory quota/type/ref checks cannot be bypassed with crafted type/ref/filter/cursor | verified sound on validation (patterns + prefix + __proto__/prototype/constructor rejects + fingerprint binding); remaining gaps are error-path disclosure (P7B-R3-01) and lock/IO cost (H-perf) |
| H05 | task store concurrency/crash handling is safe (mu + temp+rename) | verified except durability nuance filed as P7B-R3-06 (no fsync; whole-file rewrite under mu) |
| H06 | hooks cannot be driven to command injection/env leak by the model; skill/catalog/request_draft respect their schemas | verified except skill/skill-error disclosure filed as P7B-R3-02 and request_draft child-gate note in §5 |

## 3 Findings

### P7B-R3-01 Task storage errors render the absolute store path to the model
- severity: low
- lens: B security
- location: backend-go/internal/tools/native/task_store.go:365-395 (load wraps os errors with `%v` including the path from s.path()); backend-go/internal/tools/native/task.go:558-563 (taskStorageError renders TaskStoreError.Error() verbatim; non-TaskStoreError branch renders err.Error() verbatim)
- evidence: [read] trace. `load` does `os.ReadFile(s.path())` then `&TaskStoreError{Message: fmt.Sprintf("could not read task storage: %v", err)}` where err is an *PathError containing `/abs/.../tasks.json`; `mutate`/`write` do the same for persist failures (`could not persist task storage: %v`). `taskStorageError` puts `storageError.Error()` (or any other `err.Error()`) straight into `ExecutionError` content the model sees. Memory's loader by contrast returns fixed strings without paths (`memory record was not found`, `memory storage path is not a safe directory`). Needs a corrupt/missing/unreadable tasks.json to trigger; low because the caller already knows its session layout, but it still discloses the absolute state dir.
- suggested fix: map load/persist failures to fixed codes with the path in server logs only (like memory_store.go does), or strip the path with safeErrorPath before rendering. Files: task_store.go load/write, task.go taskStorageError.
- effort: S
- prior art: new

### P7B-R3-02 Skill tool discloses absolute filesystem paths and raw I/O errors to the model
- severity: low
- lens: B security
- location: backend-go/internal/tools/native/skill.go:159-162 (`skill_directories_unavailable` with `Information{Key: reason, Value: err.Error()}`); backend-go/internal/tools/native/skill.go:166,173,180 (`return "", fmt.Errorf("list skills: %w", err)` / `read skill` propagate raw errors as Go errors); backend-go/internal/tools/native/skill.go:197-203 (`Information{Key: directory, Value: found.Directory}` is an absolute path, asserted by skill_test.go:132 `fields["directory"] == filepath.Join(f.first, "summarize")`)
- evidence: [read] trace. `Directories()`/`List`/`Read` failures from contentlibrary (which include the directory/file that failed) reach the model either as an ExecutionError `reason` or as a Go error (runner renders the Go error text). On success the `directory` field is always absolute. Transcript/transcripts by contrast have HidePaths (remote forces true) precisely to avoid this. Low/hardening: the skill dirs are under the global jad root (local-user config, not secret), but they are server layout the model does not need beyond resolving relative skill files.
- suggested fix: return fixed `skill_unavailable` without the raw err text (log it server-side), and either make `directory` opt-in/hidden or document it as intentional. File-only change in skill.go.
- effort: S
- prior art: new

### P7B-R3-03 Remote transcript/transcripts reads silently drop numeric arguments passed as int/int64
- severity: low
- lens: A correctness
- location: backend-go/internal/tools/native/transcript_remote.go:234-247 (forwardRemoteArguments switches only string/json.Number/float64); callers at transcript_remote.go:275 (transcriptRemoteKeys includes `context`) and 322 (transcriptsRemoteKeys includes `limit`)
- evidence: [read] trace. Local paths accept int/int64 via safeInteger (read.go:155-180 handles int/int64 plus json.Number/float64). The forwarder has no int/int64 (or bool) case, so a `context`/`limit` supplied as int is omitted from the remote query without error; the remote then runs with its default. JSON-decoded model arguments usually arrive as json.Number/float64 so this is latent in the normal path, but direct Go callers, tests, and any decoder that yields int hit it. No error is returned.
- suggested fix: add `case int: query.Set(key, strconv.Itoa(value))`, `case int64:`, and bool if ever needed, mirroring safeInteger's accepted set. File-only change in transcript_remote.go.
- effort: S
- prior art: new

### P7B-R3-04 Transcript/transcripts child + cross-profile gates are sound locally but the short subagent id path relies on per-directory managers
- severity: low
- lens: B security (hardening) + D maintainability
- location: backend-go/internal/tools/native/transcript_path.go:108-181 (own `subagent:<id>` branch uses t.runs.RunTranscript + owner/SessionKey check with AllowChildren/AllowOtherSessions only); backend-go/internal/tools/native/transcript_path.go:183-199 (long forms add profileRefusal AllowOtherProfiles); backend-go/internal/tools/native/subagent_manager.go:830-838 (RunTranscript is global to the manager) + 843-862 (ownedRuns/allRuns; `a manager serves one storage directory`); backend-go/internal/tools/native/transcripts_list.go:411-417 (mayReadRun checks owner+AllowChildren/AllowOtherSessions, not the profile)
- evidence: [read] trace. For the default layout ToolStorage is per session+profile (`.../default/dev/coder/tools`, layout_load_test.go:113), so t.runs (caller's dir) cannot contain another profile's live run and the short form fails closed with `no_such_run`. The long form additionally enforces AllowOtherProfiles via profileRefusal. If two profiles ever share one subagent storage_dir (explicit storage_dir override, normalizeNativeSubagentStorageDir), they share one manager and a short `subagent:<id>` from the other profile would pass with only AllowChildren, while the long form would demand AllowOtherProfiles. Same shape in mayReadRun (session-gated, profile comes from the top-level gate). Narrow, configuration-dependent, fail-closed by default — hence low/hardening, not a live bypass.
- suggested fix: document that a shared subagent storage_dir also shares run visibility, or add an explicit profile check on the own-branch owner mismatch (compare the run's agent/profile when known). Files: transcript_path.go, transcripts_list.go comment.
- effort: S
- prior art: new (P6B-R2-01 fixed the ExecutionContext half with ForNested; this is the remaining storage-sharing nuance)

### P7B-R3-05 Memory List/GroupBy and Create/Update hold the store lock across directory scans and per-record I/O
- severity: low
- lens: A correctness and concurrency (also C performance)
- location: backend-go/internal/tools/native/memory_store.go:469-477 (withLock), 276-469 (list holds it across files() + per-record readRecordText/parse/match for up to MaxScanRecords/Bytes), 611-655 (totals does ReadDir per type + Stat for unknown files while holding it), 208-271 (Get/Update/Create all go through withLock+totals); backend-go/internal/tools/native/memory_group.go:28-90 (GroupBy holds it across the same scan)
- evidence: [read] trace. `withLock` is `s.locks.Lock("memory:"+s.root)` (in-process PathLocks, not an fcntl). Every Create/Update/List/GroupBy therefore serializes all memory ops of that root for the whole scan+read+write window. Correct (no race), but a List with MaxScanRecords/Bytes set high blocks Creates/Updates for the scan duration, and totals() re-ReadDirs every op (the sizes cache only skips Stat, not ReadDir — the F-13 class, fixed in 5d09534 for Stat only). No deadlock found (no nested withLock, RecentRefs takes it once).
- suggested fix: do not change locking without measurement; smallest step is snapshot the name list under lock then read files without holding it (with generation check), or document the head-of-line blocking in settings. Measure with H01/H02 first. Files: memory_store.go, memory_group.go.
- effort: M
- prior art: re-confirmed F-13 shape (fixed 5d09534 skipped Stat for known files; this files the remaining ReadDir-per-op + lock-held-across-I/O)

### P7B-R3-06 Task mutate holds mu across whole-file load+write and persists without fsync
- severity: low
- lens: A correctness (durability) + C performance
- location: backend-go/internal/tools/native/task_store.go:352-363 (mutate: mu.Lock across load+operation+write), 365-399 (load reads the whole tasks.json + validates every id), 400-433 (write marshals whole doc, CreateTemp+Write+Close+Rename with no Sync/Sync-dir); backend-go/internal/tools/native/memory_store.go:707-735 (atomicWrite same temp+rename without Sync) and 736-760 (nextSequence read-modify-write of .memory-sequence under the same in-process lock only)
- evidence: [read] trace. Correct under one process (mu serializes), crash-safe against partial JSON (rename is atomic), but a crash between Write and Rename or before the OS flushes can lose the last write (no file or dir fsync, unlike transcript journal P-012..P-014 which fsync). Cost is O(tasks) read+parse+marshal per Create/Update/Delete/List (whole-file). Cross-process races (two servers on one dir) are not excluded (mu is in-process; no file lock). Narrow in the normal single-server case, hence low.
- suggested fix: add file Sync before Close + dir Sync after Rename in task write (and memory atomicWrite if durability is wanted there), or document best-effort durability; for cost, gate with H03 measurement before changing. Files: task_store.go, memory_store.go.
- effort: S (fsync) / M (incremental format)
- prior art: new for task/memory (transcript journal fsync P-012..P-014 does not apply here)

### P7B-R3-07 Cross-session reasoning is readable by default through transcript detail
- severity: medium
- lens: B security
- location: backend-go/internal/tools/native/transcript_steps.go:138-162 (reasoning kept with summary+content text), 308-338 (selectTranscriptSteps: `reasoning never listed` but kept for detail); backend-go/internal/tools/native/transcript_detail.go:29-56 (detailBlock returns reasoning for part=reasoning; part defaults to both, detail called from transcript.go:detail); backend-go/internal/tools/native/transcript.go:222-230 (profileRefusal only checks AllowOtherSessions/Profiles); backend-go/internal/sessionfs/native_transcript_settings.go:90 + native_transcripts_settings.go:73 (AllowOtherSessions/Profiles/Children default true)
- evidence: [read] trace. List mode deliberately hides reasoning; detail with `step`+`part=reasoning` (or `part=both` default? detailBlock for reasoning requires part==reasoning, so explicit) returns the full reasoning text of any step the caller can address. With defaults (all three allows true) any session's model can read another session/profile's reasoning chain-of-thought via `profile:<session>/<category>/<name>` + `step`. Reasoning often contains private planning the list view intentionally withholds. The switches do gate it (setting all three false blocks it), but the default is open and there is no separate reasoning switch. Downgraded to medium (not high/critical) because it needs allow_* true (the default) and only discloses transcripts the operator already lets the model list/read — but reasoning is more sensitive than the listed previews.
- suggested fix: add a separate `allow_reasoning` / `expose_reasoning` switch (default false for other sessions/profiles, true for own), or redact reasoning for otherSession/otherProfile with a distinct `reasoning_denied` code. Files: native_transcript_settings.go + transcript_detail.go detailBlock + transcript.go resolve/profileRefusal.
- effort: S
- prior art: new (audit_plan §3.3 names `messages.jsonl reasoning exposure` as the risk area for this phase)

### P7B-R3-08 Oversize helpers and duplicated whole-file patterns (maintainability)
- severity: low
- lens: D maintainability
- location: backend-go/internal/tools/native/memory_store.go (1062 lines), task.go (703), catalog.go (632), request_draft.go (591), transcripts_list.go (465), transcript.go (481); backend-go/internal/tools/native/task_store.go:352-433 + memory_store.go:707-760 (two temp+rename writers with different perm/sync choices: 0600 vs 0755-dir/0600-file, no shared helper); backend-go/internal/tools/native/transcript.go:442-460 + transcripts_list.go:121-135 (two deniedFile/fileDenied EvalSymlinks wrappers)
- evidence: [read] all exceed the ~700-line guide (memory_store by ~50%) or mix validation+IO+rendering in single functions (task executeNativeTask 224-450, memory list 276-469). No behaviour claim. Duplication is shallow (two atomic writers, two deny wrappers) but they already diverge (perms, fsync, Within handling).
- suggested fix: split render/list/detail helpers or move store/cursor types out of the tool files only if touched anyway; share one atomicWrite+fsync helper if P7B-R3-06 is fixed. Docs-only if kept.
- effort: M
- prior art: new (size; not behaviour)

## 4 Performance hypotheses for measurement

- H01: memory totals re-ReadDirs every op. file: backend-go/internal/tools/native/memory_store.go:611-655 (totals) via Create/List/GroupBy
  workload: T types x F files (10x100 vs 64x1000) with warm sizes cache
  benchmark: new micro-bench driving Create/List/GroupBy on tmpfs at the two sizes, or extend memory_benchmark_test.go
  expected: time scales with T*F per op (ReadDir per type) even when sizes are cached; Stat saved by 5d09534, ReadDir not
- H02: memory List/GroupBy read every record while holding the lock. file: backend-go/internal/tools/native/memory_store.go:276-469 + memory_group.go:28-90
  workload: R records of S bytes (100x1KiB vs 1000x4KiB) at MaxScanRecords/Bytes caps, concurrent List+Create
  benchmark: new bench with N records timing List + blocked Create, or memory_benchmark_test.go extension
  expected: List time scales with scanned bytes, Create stalls for the scan window
- H03: task whole-file rewrite per op. file: backend-go/internal/tools/native/task_store.go:352-433 (mutate/load/write)
  workload: N tasks (100 vs 5000) with Create/Update/List
  benchmark: new bench over TaskStore with N tasks (Create+List), existing catalog_task_test.go as fixture pattern
  expected: op time + bytes scale with N (read+parse+marshal whole file per op)
- H04: transcript index + scan per read. file: backend-go/internal/tools/native/transcript.go:285-360 (indexTranscriptTurns + scanTranscript per execute) + transcript_scan.go:95-130
  workload: transcript S bytes (1MiB vs 20MiB) with small turns/step/detail windows (already covered by p1 N34: 15.5ms/9MB at 1MiB, 280ms/161MB at 20MiB — re-verify here for the tool path, not the remote backend)
  benchmark: existing BenchmarkReadLines/CommitLargeTranscript family + new detail-at-small-window bench
  expected: cost scales with file bytes even for a one-step detail (full index each call)
- H05: transcript detail joins full step text before paging. file: backend-go/internal/tools/native/transcript_detail.go:101-145 (walk keeps full args/result/reasoning, blocks joined then pageOfText cuts)
  workload: one step with 1MiB/8MiB result/reasoning, detail at small MaxDetailBytes
  benchmark: new bench over detail() with large-result fixture
  expected: peak allocation = full step regardless of page size
- H06: per-request skill/memory/task description/list rebuilds. file: backend-go/internal/tools/native/skill.go:198-242 (renderSkillList per list call), memory.go:404-423 (collectMemorySummary per state-context), task.go:627-680 (collectTaskSummary), catalog.go:buildNativeTools per Resolve
  workload: 100s of skills/records/tasks; catalog rebuild per Resolve
  benchmark: new bench driving list/collect at 10/100/600 items
  expected: standing per-request bytes scale with item count

## 5 Cross-phase leads

- Remote hop-limit + token-scope enforcement lives outside p7b (remote/remoteapi + app/remote_transcripts.go tokenProfiles MayAgent/MayProfile + server routes). p7b sends RemoteHops+1 (transcript_remote.go:249-288) and renders remote_hop_limit (160-183) but never enforces the numeric limit locally — by design; verify from phase 4. No local pre-check found or added.
- ExecutionContext.ForNested now carries AgentDepth/RootRun/RemoteHops (tools/catalog.go:86-90, pinned by catalog_nested_test.go:93-98; tool_smith_bridge.go:225-229 uses ForNested). The p6b gap (bridge dropped the three run fields) is fixed at this HEAD — checked and sound here; transcript/transcripts/profile_message child gates that read those fields are therefore effective through nested calls.
- App Resolver shared managers/locks (app/resolver.go sharedSubagentManager/existingSubagentManager, shared PathLocks via BuildCatalogWithLocks) determine whether two profiles share one subagent/memory/process dir (see P7B-R3-04/05). Per-profile ToolStorage default keeps them separate; sharing is explicit config. No change requested here.
- Hook command construction/quoting/timeouts are configured by the local user (hooks settings), not the model; model influence is only via file paths that trigger already-configured commands. Deeper command-injection review belongs to the settings-validation phase (2b2 hooks_settings), not this tool-execution pass.

## 6 Checked and sound (one line each)

- Transcript cursor identity binding sound: list/detail both compare Identity hash + End>complete as stale (transcript.go:285-360); forged Selector with wrong Identity fails as cursor_stale.
- Transcript/transcripts traversal + symlink confinement sound: splitTranscriptName rejects empty/./../backslash/NUL, parseTranscriptsPath length-gated, resolveExistingTarget EvalSymlinks+policy+Workspace checks, statTranscriptsFile requires regular file with symlink-inside-folder, deniedFile/fileDenied re-resolve + scopedPolicy Within/deny (path.go:23-110, transcripts_list.go:100-135, transcript.go:442-460).
- Remote path grammar + token indistinguishability sound locally: selector() validates Name/Session/Profile patterns + Allows(profile) + default_session fill, check() gates allow/remotes/directory, remoteReadError maps forbidden/unauthorized/hop_limit/no_session without echoing tokens (transcript_remote.go:59-183); file-path hiding enforced server-side via RemoteAPITranscriptSettings HidePaths=true (native_transcript_settings.go:166-167, verified by remote_transcripts_test.go:117-130).
- Subagent short-form scoping sound for the default layout: short id resolves only in the caller's per-directory manager (RunTranscript/byID global-to-manager, managers keyed by StorageDir per session+profile); long forms add profileRefusal; residual sharing nuance filed as P7B-R3-04 hardening only.
- Memory type/ref/filter/cursor validation sound: type/filename/sequence/path-segment patterns + recordType- prefix + __proto__/prototype/constructor rejects + fingerprint binding for stored/legacy cursors (memory_store.go:25-31, 336-360, 498-525, 657-664, 903-935).
- Memory atomicWrite temp+O_EXCL+rename + type-dir safe checks (ensureRoot/typeDirectory Lstat+symlink reject) sound against symlink/file swaps; residual fsync + cross-process sequence nuance filed as P7B-R3-06 low.
- Task id/status/priority/limits validation sound (task.go:451-525 taskActionFor/StatusValue/PriorityValue/taskStringArray/validTaskText/taskNoExtra; task_store.go:445-457 validStoredTask + duplicate-id + NextID bounds); task_not_found available_ids listing is ids only, no file paths.
- Hooks env/privilege sound: buildNativeBashEnvironment allowlists PATH/HOME/LANG/LC_*/TMP*/SYSTEMROOT/WINDIR/ComSpec/PATHEXT and denies JAD_API_TOKEN/apiKeyEnv/EnvDeny (bash.go:382-412 via hooks.go:120-177); run events are log-only and cannot change results (hooks_run_events.go:36-78); blocking pre_tool stops later commands (hooks_events.go:150-181).
- Catalog definition check sound: requireDefinition pins id+source+name+exact inputSchema JSON equality (catalog.go:580-610 sameJSON via DeepEqual) + duplicate-id + missing-definition config errors; remoteDirectory validates every named remote exists (catalog.go:615-632).
- Request_draft limits sound: draft bytes (DraftJSONBytes vs MaxDraftBytes), depth (DraftJSONDepth vs MaxDraftDepth), title UTF-16 length (request_draft.go:57-110); RequiresUserDecision + IsUserDecisionCall wiring covered by request_draft_test.go + runner_native_request_draft golden test.
- P6B-R2-01 class fixed: ForNested carries the whole run scope and is covered by TestForNestedKeepsEveryRunScopedField + TestForNestedKeepsTheThreeRunFields; bridge + subagent childExecutionContext both use it.
- Prior art not re-raised: F-13 Stat-saving verified present (filesKnowing skips Stat for known names); transcript whole-file-read (F-12) not present (SectionReader scans); fetch-store per-fetch stat (P7A-04) is phase 7a, not repeated.

## 7 Not covered and why

- Full line-by-line read of every test file (tests used only as evidence for the behaviours above; test-quality lens covered via the named suites, not an exhaustive pass).
- sessionfs native_* settings bounds/caps beyond the defaults cited (phase 2a/2b territory; used only for default-true and HidePaths facts).
- Fuzzing/property testing of midCut/pageOfText/cutUTF8 rune-boundary handling (unit tests cover tiny budgets and split runes; no new fuzz run per reviewer rules).
- Benchmarks, profiles, -race (forbidden to reviewers; hypotheses in §4 are for the measurement step).
- Networked remote round trips (httptest fakes only in existing tests; no live remotes per machine rules).
