# Phase 7b audit review — r2

- phase: p7b (native tools: transcript*, memory*, task*, hooks*, skill.go, catalog.go, request_draft.go)
- reviewer: r2
- HEAD: `09139f5` ("Refuse five IANA special-purpose ranges in the fetch address check (P7A-03)"), date 2026-10-30
- basis: `audit_plan.md` sections 1.1, 3.2 (phasemap `p7b`), 3.3 (p7b row), 4.3, 5, 5.1-5.3; `backend-go/DECISIONS.md` (transcript v1/v2, transcripts, remote call, memory, task, hooks, request_draft entries)
- scope measured now: **22 non-test files, 7,871 lines** (the task prompt says "12 files, about 5,300 lines" and plan 3.2 says "12 files, 5,342"); the phasemap globs are the source of truth and resolve to 22 files at this HEAD, so all 22 were reviewed. The extra files are `transcript_cut.go`, `transcript_render.go`, `transcript_scan.go`, `hooks_run_events.go`, `memory_group.go`, `memory_json.go`, `task_store.go` (all matched by `transcript*`, `hooks*`, `memory*`, `task*`).

## Files read (all 22, non-test)

| file | lines | how read |
|---|---|---|
| transcript.go | 481 | 1-250, 250-481 (whole file, two ranges) |
| transcript_steps.go | 344 | whole file |
| transcript_detail.go | 145 | whole file |
| transcript_path.go | 243 | whole file |
| transcript_remote.go | 329 | whole file |
| transcript_scan.go | 141 | whole file |
| transcript_cut.go | 81 | whole file |
| transcript_render.go | 51 | whole file |
| transcripts.go | 248 | whole file |
| transcripts_list.go | 465 | whole file |
| memory.go | 470 | whole file |
| memory_store.go | 1062 | 90-300, 300-470, 470-660, 660-880, 880-1062 (whole file in ranges) + `grep -n ^func` outline |
| memory_json.go | 447 | 113-200, 287-300, 330-420 + outline; 200-287 and 420-447 only by outline (see section 7) |
| memory_group.go | 94 | whole file |
| task.go | 703 | 224-330 + outline (see section 7) |
| task_store.go | 520 | 196-260, 352-435 + outline (see section 7) |
| hooks.go | 246 | 1-200 (1-130, 130-200), 200-246 |
| hooks_events.go | 258 | 1-95, 115-258 |
| hooks_run_events.go | 78 | whole file |
| skill.go | 242 | 1-120, 150-242 |
| catalog.go | 632 | 1-140, 140-240, 240-340, 340-482, 482-560, 553-632 + `grep -n ^func` outline |
| request_draft.go | 591 | 57-145 + outline (see section 7) |

Shared helpers traced (not filed as p7b findings unless the defect is in p7b code): `app/transcript_profiles.go` (the `TranscriptProfiles`/`TranscriptSessions` port, 1-79), `app/remote_transcripts.go` (remote-side `tokenProfiles` and `remoteBackend.Transcript`, 1-123), `app/resolver.go` (`sharedTaskStore`/`sharedMemoryStore`, 768-826), `contentlibrary.ReadSkill` (skills.go:102-123), `sessionfs` native-transcript/-memory/-task settings defaults.

## Commands and exit codes

| command | exit |
|---|---|
| `go build ./internal/tools/native/` | 0 |
| `go vet ./internal/tools/native/` | 0 |
| `gofmt -l internal/tools/native` (no output) | 0 |
| `go test ./internal/tools/native/ -run 'Memory' -count=1` | 0 (`ok … 0.024s`, log `/tmp/r2/memory_tests.log`) |
| `go test ./internal/tools/native/ -run TestProbeMemoryJSON -count=1 -overlay /tmp/r2/overlay.json` | 0 (observations only, log `/tmp/r2/probe1.log`) |
| `go test ./internal/tools/native/ -run TestProbeMemoryStoreLosesAQuotedRecord -count=1 -overlay /tmp/r2/overlay2.json` | 0 (observations only, log `/tmp/r2/probe2.log`) |
| `go test ./internal/tools/native/ -run TestProbeMemoryJSONStaysValidJSON -count=1 -overlay /tmp/r2/overlay3.json` | **1** (this is the intended result: the assertion fails, see P7B-R2-01) |
| read-only `sed`/`grep`/`git log`/`git show` (all of the above reading) | 0 |

No benchmark, no `-race`, no profile, no `go test ./...`, nothing written outside `/tmp/r2/` and this file.

## 1. Understanding note

The p7b tools are a read-mostly set that the model drives through tool arguments, with the caller's `tools.ExecutionContext` (`SessionKey` = `<session>\x00<profile>`, `AgentDepth`, `ProcessOwner`, `WorkspaceRoot`, `RemoteHops`, `CallID`) as the only identity. Two separate surfaces read files that belong to a session:

- `transcript` (schema `transcriptInputSchema`, `transcriptTool.resolve` at transcript_path.go:71) maps a selector (`""`, `profile:`, `subagent:`, `path:`, `remote:`) to a `TranscriptFiles{Messages, Backup}` and then reads it with a read-only line scanner that never repairs an incomplete last line (`scanTranscript`, transcript_scan.go:42). Access layers, in order: `allow_children` for any sub-agent (transcript.go:229), prefix grammar (`splitTranscriptName` refuses `""`, `.`, `..`, `\`, NUL), `allow_path` for `path:`, `allow_other_sessions`/`allow_other_profiles` via `profileRefusal`, run ownership (`owner != execution.SessionKey` needs `allow_children` and, cross-session, `allow_other_sessions`), the read path rules (`scopedPolicy` + `deniedFile`, symlinks resolved, workspace not required because state lives outside it), and `remote:` behind `allow_remote` + the per-tool `remotes` list + the entry's `profiles` globs.
- `transcripts` (transcripts.go, transcripts_list.go) lists what `transcript` can read; it re-implements the same gates per level (`gate`, `listSessions`, `listProfiles`, `listRuns`, `mayReadRun`, `runItem`) and stats files without opening them.

Both tools are built per catalog from `config.NativeTranscript`/`NativeTranscripts`, and the port that finds another profile's files is `app.profileMessenger` (sessionfs-resolved, no file created). On the remote side the same tool code runs under `tokenProfiles` with `HidePaths`. Cursors are the state carrier for paging: a base64 JSON blob that pins the selector, the source, a file identity hashed from the file's first line, a byte range and (v2) the step range.

The other files: `memory` is a JSON-record store with quotas, filters, a cursor registry on disk and hand-written JSON read/write; `task` is a single `tasks.json` document read-modify-written under a mutex; `hooks` wraps other tools (`pre_tool`/`post_tool` events, `post_edit` checks) and starts child processes with bash's environment rules; `skill` reads `SKILL.md` out of configured skill directories; `request_draft` is the only interactive question left and auto-accepts after a headless timeout (a recorded user decision); `catalog.go` is the registration/wiring of all native tools.

Global observation that shapes the review: every p7b file is synchronous. `grep` for `time.Sleep`, `go func`, `make(chan`, `sync.` over the 22 files returns only `task_store.go:197` (`sync.Mutex`). So no goroutine, channel or timer lifetime can be at fault here; the risks are trust boundaries, cursor arithmetic and per-call I/O.

## 2. Hypotheses (drafted before verification)

| id | statement | status |
|---|---|---|
| H1 | `path:` reads in `transcript` bypass `allow_other_sessions` by naming another session's file | disproved: `resolveExistingTarget` confines to the workspace and the state root is outside it; `allow_path` defaults false (transcript_path.go:91-107, test `TestTranscriptPathIsOffByDefault`) |
| H2 | `transcripts` `sessions` level enumerates all sessions while `allow_other_sessions` is false | disproved: `listSessions` (transcripts_list.go:136-158) returns only the caller's session and adds a note |
| H3 | a forged local cursor can reach a selector the switch or the remote globs forbid | partly verified: every selector gate re-runs because `resolve` is called on resume (transcript.go:268), and the file identity blocks forgery; but the `remote:` cursor branch skips the entry's `profiles` globs (P7B-R2-04) |
| H4 | `write`/`edit` get hooks twice (`withConfiguredHooks` plus `WithToolEventHooksAll`) | disproved: `withConfiguredHooks` is `WithPostEditHooks` (hooks.go:244), a different hook kind from the tool-event hooks (hooks_events.go:95) |
| H5 | a model-controlled string in `memory` data can corrupt the record file | **verified, measured** (P7B-R2-01) |
| H6 | list paging of a large transcript re-scans the whole file per page | **verified by trace** (P7B-R2-02) |
| H7 | `transcripts` run listing can be pointed at a file outside the sub-agent storage | disproved: `insideDirectory` checks on `target.SubagentDir` and on the symlink-resolved run dir (transcripts_list.go:434-438), tests `TestTranscriptsRunTranscriptOutsideTheStorageIsNotFollowed`, `TestTranscriptsSymlinkedTranscriptOutsideItsFolderIsNotFollowed` |
| H8 | `skill` name reaches a file path unvalidated | disproved: `contentlibrary.ReadSkill` requires `skillNamePattern` and a length bound (skills.go:106) |
| H9 | `task` store loses updates because its mutex is per instance | verified in the narrow cross-process case only (P7B-R2-06) |
| H10 | the transcript detail mode can be made to hold the whole file in memory | verified but bounded by `scan_max_bytes`; listed as a hypothesis (H02), not a finding |
| H11 | cursor identity (hash of the first line only) can be confused between two transcripts | verified, narrow (P7B-R2-11) |
| H12 | memory filter/cursor validation can be used to read outside the type directory | disproved: `target` (memory_store.go:657-663) and `checkType` (memory_store.go:498) pin `type` + `<type>-…json` names, and every read requires a regular non-symlink file |

## 3. Findings

### P7B-R2-01  `quoteMemoryString` writes every JSON escape with one backslash too many: a quote in memory data stores an unparseable record, and a backslash/newline/tab silently rewrites the value and grows it on every update
- severity: critical
- lens: A correctness (data loss), D maintainability (test gap)
- location: `backend-go/internal/tools/native/memory_json.go:367-397` (`quoteMemoryString`; the offending literals are lines 373-395), used by `writeMemoryJSON` (`memory_json.go:287-360`) which backs `memoryJSONPretty` (`:280`), `memoryJSONCompact` (`:273`) and `memoryJSONCanonical` (`:422`); written to disk by `memory_store.go:180` (`CreateValue`) and `memory_store.go:252` (`UpdateValue`); read back by `memory_store.go:665` (`load`) and `:694` (`readRecordText`)
- evidence: **[measured]** probe test overlaid into the package (`/tmp/r2/probe3_test.go`, `go test ./internal/tools/native/ -run TestProbeMemoryJSONStaysValidJSON -overlay /tmp/r2/overlay3.json`, exit 1): `{"s":"a\"b"}` is written as `"{\n  \"s\": \"a\\\\\"b\"\n}\n"`, which `json.Valid` rejects. End-to-end probe (`/tmp/r2/probe2_test.go`, exit 0, log `/tmp/r2/probe2.log`) through the public store API with `memoryTestSettings`: `store.CreateValue("note", …{"note":"the user said \"hi\" twice"}…)` returns `err=<nil>` and writes `{"note": "the user said \\"hi\\" twice"}`; `store.Get` then returns `memory record is malformed` — the record is gone and nothing reports it. Non-quote escapes are valid JSON but change the value: probe `/tmp/r2/probe1.log` shows `back\slash` → `back\\slash`, `line1<LF>line2` → `line1\nline2` (literal backslash-n), `tab<TAB>here` → `tab\there`; and a write/read/write loop on `{"note":"line1\nline2"}` grows the record `30 → 32 → 36 → 44 → 60 → 92` bytes over six rounds (one more backslash pair per round, doubling), i.e. a record that is updated repeatedly eventually hits `max_record_bytes` and stops being updateable. Root cause: every case in `quoteMemoryString` uses a two-character raw-string literal (`\\"`, `\\\\`, `\\n`, `\\u%04x`), so a backslash is emitted before the escape instead of as part of it. `go test ./internal/tools/native/ -run Memory -count=1` passes (exit 0), so no existing test stores a quote, backslash, newline, tab or control character (test-quality gap: `memory_test.go`, `memory_json_test.go` (18 lines) assert nothing about round-tripping such text).
- reproduction (6 lines, run as a `/tmp` test overlaid with `go test -overlay`; the scratch files were removed after the run):
  ```go
  value, _ := parseMemoryJSON([]byte(`{"s":"a\"b"}`))   // any quote, backslash, newline or tab
  out, _ := memoryJSONPretty(value)
  if !json.Valid([]byte(out)) { t.Fatalf("wrote invalid JSON: %q", out) }   // fails today
  ```
  The end-to-end form through the exported API is `store.CreateValue("note", value, data, memoryTestSettings(root))` followed by `store.Get("note", created.Ref, …)`, which returns `memory record is malformed`.
- suggested fix: replace the hand-written escaping in `quoteMemoryString` with the encoder this repo already standardised on (`tools.MarshalJSON`, DECISIONS "One marshaler and one cutter for the bytes the model and `messages.jsonl` see") or `encoding/json`'s string encoder, and drop the duplicated switch; add a regression test that writes and re-reads a record whose values contain `"`, `\`, `\n`, `\t`, U+001B and U+2028 and asserts `json.Valid` plus byte-identical values (`memory_json_test.go`). A one-off repair for records already corrupted is not possible in general (the value is unrecoverable); the `warnings` line in `list` already surfaces them as `unreadable record`.
- effort: S (the fix is a few lines; the test is the real work)
- prior art: new. This is memory code that audit_backend.md covered only for performance (F-13, fixed `5d09534`); the same escaping literals are at `84d4deb^:internal/tools/native/memory_store.go:1364-1376`, so the file split did not introduce it.

### P7B-R2-02  `transcript` list paging re-reads and re-walks the whole selected range for every page (work is pages × bytes)
- severity: medium
- lens: C performance
- location: `backend-go/internal/tools/native/transcript.go:332` (resume takes `cursor.Offset`), `:363` (`scanTranscript` from `startOff`), `:397` (the next cursor stores `Offset: startOff`, the *range start*, not the offset of the step it resumes at)
- evidence: **[read]** trace plus the cursor shape. A cursor is `{Selector, Source, Identity, Offset: startOff, End: endOff, From: next.id}`. `startOff` is the offset of the first turn of the original call (`:355`) and is never advanced, so page k scans `[startOff, endOff)` again, re-walks every step and then throws away everything newer than `next` (`:372` `selected[:sort.Search(…resume)]`). Page cost is constant and equal to the whole range, so paging a transcript costs O(pages × range). Workload: this repository's own `.jad/state` had 323 transcripts over 20 KB, 57.7 MB in total (DECISIONS, "Measured"), and `tools.native.transcript.scan_max_bytes` defaults to 256 MiB (`sessionfs/native_transcript_settings.go:12`), so a model paging a 20 MB transcript with `max_list_entries` 100 pays a 20 MB read and walk per page, and the same bytes are read again for every page. Unmeasured (reviewers do not benchmark); see H01 for the measurement.
- suggested fix: on resume set `startOff` to the turn start that holds `next` (`turns[i].off` for the turn of `next.id.turn`, which the step already carries as `transcriptStep.turnOff`), so a page starts at its own turn. Step numbering still restarts per turn (`transcriptWalk.advance`/`add`, transcript_steps.go:92-125), so the documented "numbers do not change and no step repeats or is skipped" property is preserved. Files: `transcript.go` only.
- effort: S
- prior art: new (DECISIONS "Order (amended 2026-10-28)" records the *semantics* of fixing the byte range, not this cost)

### P7B-R2-03  `transcript` list mode materialises one step per record of the whole range to return at most 100 lines
- severity: medium
- lens: C performance
- location: `backend-go/internal/tools/native/transcript_steps.go:63-70` (`transcriptWalk.steps`, `calls` held for the whole walk), `:126-137` (`add` appends every step), `transcript.go:363-390` (walk the range, then `selectTranscriptSteps`, then keep the newest `max_list_entries`)
- evidence: **[read]** the walk builds a `*transcriptStep` (≈150-200 bytes plus a preview string of up to `preview_bytes` = 200 and an `argsLine` of up to `value_bytes` = 80 per call) for **every** step of the range, plus a map entry per call, and only then trims to `MaxListEntries` (default 100) and `ResponseMaxBytes` (default 64 KiB). A 20 MB transcript with tens of thousands of steps therefore allocates several MB and holds it for the call to print 100 lines; the default `turns` is the whole transcript. Combined with P7B-R2-02 this is paid per page.
- suggested fix: in list mode keep only the newest `min(MaxListEntries+1, …)` matched steps a page can print (a bounded slice, or counting from the end) instead of all of them, and cap `walk.calls` to the calls whose result can still change a printed line; the detail mode's `keepFrom/keepTo` already shows the pattern. Files: `transcript_steps.go`, `transcript.go`. Measurement in H02.
- effort: M
- prior art: new

### P7B-R2-04  A `remote:` cursor re-check skips the remote entry's `profiles` allow-list that the same tool enforces for a `path:` selector
- severity: low
- lens: B security (boundary consistency)
- location: `backend-go/internal/tools/native/transcript_remote.go:284-300` (cursor branch of `transcriptTool.executeRemote`: only `t.remotes.check(name)`), `:329-350` (`transcriptsTool.executeRemote`, same shape), versus `:59-108` (`remoteReader.selector`, which enforces `entry.Allows(profiles)` at `:104`) and `:216` (path branch)
- evidence: **[read]** a call whose `cursor` matches `splitRemoteCursor` and whose name passes `check` is forwarded to the remote with only the inner cursor; the local `entry.Allows` glob check and the `remoteShapes` grammar check do not run. The remote then re-resolves the selector in the forwarded cursor against *its own* token globs (`app/remote_transcripts.go:28-39` `tokenProfiles.Profile`), so the exposure is bounded by the remote token, as DECISIONS records ("the remote's token must also have the `transcripts` action; its globs decide the profiles"). Reaching a profile outside the *local* globs also needs a cursor whose `Identity` matches the remote file, and `Identity` is the SHA-256 of that file's first line (`transcript_scan.go:130-141`) checked at `transcript.go:315`/`:329`, which the caller cannot obtain for a file it may not read. So this is defence-in-depth, not a demonstrated bypass; it is filed low because the two branches of the same call disagree about which local rule applies.
- suggested fix: apply `entry.Allows` to the profile or run segment of the inner cursor when the remote is known to be this code base, or (simpler and opaque-cursor-safe) refuse `cursor` on a remote whose entry has a non-empty `profiles` list, with a message naming the entry. Files: `transcript_remote.go`.
- effort: S
- prior art: new

### P7B-R2-05  `transcripts` prints a false "persist_runs is off" note on the `own` fallback, and that whole branch is unreachable in production
- severity: low
- lens: D maintainability
- location: `backend-go/internal/tools/native/transcripts_list.go:350-365` (`case own:` builds `TranscriptTarget{Session, Profile, Runs}` with `SubagentDir: ""` and `PersistRuns: false`, then `:365` emits "persist_runs is off for this profile …"; the run manifests are loaded from `target.SubagentDir` at `:369`), same fallback in `listProfiles` at `:172-179`
- evidence: **[read]** the `own` branch is only reached when `t.profiles == nil`, and both production builders pass a non-nil port (`catalog.go:136` passes `profiles` from `messenger.(TranscriptProfiles)`; `app/remote_transcripts.go:106` passes `tokenProfiles`). With `profiles != nil` the first `case` always wins, so the fallback's note can never be accurate: it is dead code that, if it ever ran, would state a setting it does not know.
- suggested fix: delete the `own`/nil-port fallbacks together with their notes, or make them say "persist_runs is unknown here"; a nil port already answers every other selector with `profile_unavailable`. Files: `transcripts_list.go`.
- effort: S
- prior art: new

### P7B-R2-06  `TaskStore` (and `FetchStore`) serialize with a per-instance mutex while `MemoryStore` uses the shared `PathLocks`, so two servers on one tool-storage directory can lose a task
- severity: low
- lens: A correctness (concurrency), D divergence from the neighbouring pattern
- location: `backend-go/internal/tools/native/task_store.go:196-205` (`TaskStore{mu sync.Mutex}`), `:352-364` (`mutate` = load, change, write whole document), `:207-232`, `:303-336`; contrast `memory_store.go:130-157` (`locks *PathLocks`) and `app/resolver.go:788-808` (`sharedMemoryStore` passes `r.mutationLock`) with `app/resolver.go:768-786` (`sharedTaskStore` does not)
- evidence: **[read]** every task mutation is a read-modify-write of `tasks.json` (`load` → `operation` → `write` with temp file + rename) guarded only by one `TaskStore`'s mutex. Within one process the Resolver hands out one store per storage key (`resolver.go:768-786`), so the mutex is sufficient; two server processes (or two Resolvers) configured with the same `tools.native.task.storage_dir` interleave and the later writer drops the earlier writer's tasks. The memory store was given exactly this protection ("Resolver-owned PathLocks serialize operations across catalogs sharing a root", memory_store.go:127-139) and the task store was not.
- suggested fix: give `TaskStore` an optional `*PathLocks` like `NewMemoryStore(root, shared ...*PathLocks)` and pass `r.mutationLock` in `sharedTaskStore`; same for `FetchStore` if it writes shared state. Files: `task_store.go`, `app/resolver.go`.
- effort: S
- prior art: new

### P7B-R2-07  `catalog.go` registration: the tool list exists in four places and the wiring is 20 copy-pasted blocks behind seven telescoping constructors
- severity: low
- lens: D maintainability
- location: `backend-go/internal/tools/native/catalog.go:50-95` (seven wrappers `BuildCatalog` → `BuildCatalogWithLocks` → `…WithProcessManager` → `…WithResources` → `…WithSubagents` → `…WithMessenger` → `…WithAsync`, each adding one parameter), `:245-481` (`buildNativeTools`, 237 lines, 20 blocks of `if config.NativeX != nil { requireNativeDefinition; NewXTool; tool.Definition = definition; loaded = append(…) }`), `:118-143` (the two transcript tools that must sit *after* `buildNativeTools` and before the subagent tool), `:482-528` (`nativeToolConfigured`, a second 20-arm switch over the same names); the name list it must agree with is `sessionfs.NativeToolNames()` (`sessionfs/tool_shape.go:20`, list in `nativeToolNames`), and the config fields are a third copy. Adding or renaming one native tool means editing four places, and forgetting `nativeToolConfigured` silently skips the "configured but missing from tool_definition_paths" error.
- evidence: **[read]** `grep -n '^func' catalog.go` and the two ranges above; no test failure results from the duplication today, so this is a change-cost finding, not a defect.
- suggested fix: one table of `{name, id, schema, isConfigured, build}` entries that `buildNativeTools`, `nativeToolConfigured` and the pre-flight check all iterate; and collapse the wrapper chain to one exported builder plus a small `options` struct when the next parameter is added (do not churn the seven exported names that tests and `app` call).
- effort: M
- prior art: new

### P7B-R2-08  `memory` derives the tool status from a substring of the store's error text
- severity: low
- lens: D maintainability
- location: `backend-go/internal/tools/native/memory.go:262` (`if strings.Contains(storeError.Error(), "not found") { status = tools.StatusNotFound }`), against `MemoryStoreError.Kind`, which already exists (`memory_store.go:35-47`, switched on by `memoryValidValues` at `memory.go:272`)
- evidence: **[read]** the four not-found errors are `memory_store.go:672` and `:682` ("memory record was not found", which do carry `Kind: memoryErrorRef`) and `:842`/`:847` ("memory cursor was not found", no kind). The model-visible status is derived from the *message text* while the kind that already distinguishes these cases is ignored; any reworded message silently turns a `not_found` into a generic error.
- suggested fix: add `memoryErrorNotFound` to the `Kind` enum, set it at all four places, and switch on it in `memoryExecutionError` (`memory.go:255-269`). Files: `memory_store.go`, `memory.go`.
- effort: S
- prior art: new

### P7B-R2-09  A memory record is pretty-printed twice on every create and update
- severity: low
- lens: C performance, D maintainability
- location: `backend-go/internal/tools/native/memory_store.go:513-525` (`checkData` calls `memoryJSONPretty` only to measure the encoded size) and `:176` / `:244` (`CreateValue`/`UpdateValue` call `memoryJSONPretty` again to write it)
- evidence: **[read]** two full encode passes per write, the first discarded. `max_record_bytes` defaults to 64 KiB, so a full-size record costs 128 KiB of encoding per call. Unmeasured; see H04.
- suggested fix: have `checkData` return the encoded text and pass it to `atomicWrite`, so the measurement and the write are one encode. Files: `memory_store.go`.
- effort: S
- prior art: new (F-13 measured the quota scan, not the encode)

### P7B-R2-10  `pruneCursors` reads the whole cursor directory and stats every entry on every page that creates a cursor
- severity: low
- lens: C performance
- location: `backend-go/internal/tools/native/memory_store.go:793-829`, called from `:889` inside `createCursor`
- evidence: **[read]** one `os.ReadDir` plus one `os.Stat` per cursor file per cursor creation, i.e. per memory `list` page that stops early. Bounded by `max_cursors` plus whatever has expired, so the cost is small; listed because it is the same shape as F-13's per-write scan in the same store and the store already caches record sizes.
- suggested fix: prune at most once per `cursor_ttl_ms`/N calls, or only when `len(entries) > max_cursors`, before the `ReadDir` when the cached count says there is room. Files: `memory_store.go`.
- effort: S
- prior art: new

### P7B-R2-11  A transcript cursor is tied to the file's *first line* only, so two transcripts that begin with the same line share an identity
- severity: low
- lens: A correctness (cursor arithmetic)
- location: `backend-go/internal/tools/native/transcript_scan.go:130-141` (`transcriptIdentity`: SHA-256 of at most the first 4 KiB, cut at the first LF), checked at `transcript.go:315` and `:326`
- evidence: **[read]** the identity is meant to detect a compaction or a reset ("appends do not change it"); a reset that rewrites `messages.jsonl` with the same opening user line (the common case for a re-run of the same prompt) keeps the identity, so a cursor from before the reset is accepted and its `Offset`/`End` are applied to different content. The `cursor.End > complete` guard (`transcript.go:329`) catches only a shorter file. The consequence is a wrong page window, not a cross-session leak (`resolve` re-runs and every access gate re-applies). Also, a file whose first line exceeds 4 KiB hashes only that prefix, so two such files can collide outright.
- suggested fix: include the file size class and the mtime or the inode in the identity (`file.Stat` is already read at `transcript.go:288-296`), or hash the first and last complete lines. Files: `transcript_scan.go`, `transcript.go`.
- effort: S
- prior art: new

### P7B-R2-12  `skill` returns Go errors for library failures while its neighbours return an `ExecutionError`
- severity: low
- lens: D maintainability, A correctness of the model-facing text
- location: `backend-go/internal/tools/native/skill.go:159-161` (`skill_directories_unavailable` is a proper `ExecutionError`), `:166`, `:173`, `:180` (`return "", fmt.Errorf("list skills: %w", err)` and `read skill: %w`)
- evidence: **[read]** an unreadable skill directory therefore reaches the model as the catalog's canned `tool_execution_failed` result with no reason (DECISIONS, phase 6a: `ExecuteFunctionCall` replaces the error with that canned result and logs it), while the same failure three lines above is reported with a code. One function, two conventions.
- suggested fix: return `tools.ExecutionError(callID, tools.StatusError, "skill_unavailable", …)` for the three library errors, as `:159` already does. Files: `skill.go`.
- effort: S
- prior art: new

## 4. Performance hypotheses for the measurement step (section 5.3)

- H01: list paging re-scans the whole range per page, `transcript.go:363` with `:397`
  workload: one `messages.jsonl` of 1 MB / 10 MB / 57 MB (this repo has 323 transcripts over 20 KB, 57.7 MB total); page through with `max_list_entries: 100` (default) until the end.
  benchmark: none exists; proposed `BenchmarkTranscriptListPaging` with a generated fixture of N steps (write it to `/tmp`, not the repo), measuring page 1, page 2, page 10, page 100 and total bytes read (`io` counting wrapper) at each size.
  expected: per-page cost flat at the full range and total work quadratic in pages; after the fix, per-page cost proportional to the remaining window and total work linear in bytes.
- H02: list mode holds a step for every record of the range, `transcript_steps.go:63-70`, `:126-137`, `transcript.go:363-390`
  workload: a 20 MB transcript of ~100k records; one call without `turns`.
  benchmark: the same generated fixture as H01, with `-benchmem`; compare allocations against `max_list_entries` 100 and 5000.
  expected: allocation and peak heap scale with the record count while output is fixed at 100 lines (≈200 B/step ⇒ tens of MB at 100k steps).
- H03: every catalog build re-reads and re-validates all tool definition files and compares every `inputSchema` with `reflect.DeepEqual`, `catalog.go:553-588` (`loadDefinitions`), `:529-551` (`requireDefinition`), `:592-610` (`sameJSON`)
  workload: 20 native definition files plus `extension/decide_systemone`, once per run (`app/resolver.go:438` `buildToolCatalog` in `prepareRun`).
  benchmark: none; proposed `BenchmarkBuildCatalogWith20Definitions` over the real `sample/tools` files (`/tmp` copy if the repo must not be read from a benchmark), with `-benchmem`.
  expected: a per-run constant of a few ms, dominated by one `jsonschema.CompileSchema` and 20 × (`jsonschema.UnmarshalJSON` + `reflect.DeepEqual`); a cache keyed by file name+mtime+size would make it zero after the first run.
- H04: double pretty-print per memory write, `memory_store.go:513-525` vs `:176`/`:244`
  workload: one record at `max_record_bytes` (64 KiB default) with 1000 records present.
  benchmark: existing `BenchmarkMemoryStoreCreateWithExistingRecords` (`memory_benchmark_test.go:38`), `-benchmem`.
  expected: roughly one extra full encode per create/update (the fix removes ~half of the encode work, which is a small part of the create total measured for F-13).
- H05: cursor prune per page, `memory_store.go:793-829`, called at `:889`
  workload: `max_cursors` at its default and at 512, a `list` whose page stops early.
  benchmark: none; proposed `BenchmarkMemoryStoreListCreatesCursor`.
  expected: O(max_cursors) `os.Stat` per page, small but avoidable.
- H06: `transcripts` resolves every candidate profile and loads every run manifest per call, `transcripts_list.go:188-206` (one `profiles.Profile` → `LoadSessionSettings` per profile) and `:371-377` (whole run-storage `load()` plus `allRuns()`)
  workload: a session with 50 profiles; a profile with 200 run manifests.
  benchmark: none; proposed `BenchmarkTranscriptsListProfiles50` and `BenchmarkTranscriptsListRuns200`.
  expected: linear in profiles/runs with one settings load per profile; the run half is bounded by `scan_max_entries` (2000) only after the load.

## 5. Cross-phase leads (not p7b findings)

- `backend-go/internal/app/transcript_profiles.go:16-50` (phase 1): `Profile` calls `sessionfs.LoadSessionSettings` for *every* profile it is asked about, and `transcripts` asks once per listed profile (`transcripts_list.go:188-206`), so a profiles listing costs one settings load per profile. Not p7b code; see H06.
- `backend-go/internal/app/transcript_profiles.go:54-79` (phase 1): `Candidates`/`Sessions` do not filter by session scope themselves; the p7b tools apply the switches *after* the call (`transcripts_list.go:136-158`, `:175-186`). Any future caller of the port must remember that. Sound today.
- `backend-go/internal/app/remote_transcripts.go:28-55` (phase 1): `tokenProfiles.Candidates` hides other sessions entirely and `Profile` maps every refusal to `errRemoteProfileHidden`, which is what makes the remote's `notFound` texts indistinguishable. Relied on by `transcript_path.go:207-220`; sound.
- `backend-go/internal/contentlibrary/skills.go:102-123` (phase 8): the only thing standing between a model-supplied skill name and a path; `skillNamePattern` + length bound hold.
- `backend-go/internal/tools/native/path_scope.go` and `mutate_path.go` (`scopedPolicy`, `resolveExistingTarget`, `withinAny`, `insideDirectory`, `PathWithin`): reviewed in 7a; `transcript`/`transcripts` reuse them unchanged for `subagent:`/`profile:`/`path:` and for the read path rules.
- `backend-go/internal/remote/client.go:319-335` (`scrub`) and `run.go:181-196` (`ReadTranscript`): the token/URL scrubbing the remote transcript error texts rely on is phase 4 code.

## 6. Checked and sound

- No goroutine, channel, timer or sleep exists anywhere in the 22 files (`grep` for `go func`, `make(chan`, `time.Sleep`, `sync.`), so no p7b lifetime or shutdown finding.
- `transcript` access gates, in the order they run: `allow_children` for a sub-agent (`transcript.go:190`), `splitTranscriptName` refusal of `""`/`.`/`..`/`\`/NUL (`transcript_path.go:20-30`), `allow_path` with the workspace-confined `resolveExistingTarget` (transcript_path.go:91-107), `profileRefusal` for another session/profile (defined at `transcript_path.go:222-232`, called at `:135` and `:194`), the run owner rules (transcript_path.go:161-172), `deniedFile` with symlinks resolved (called at transcript_path.go:173 and `:196`, defined at transcript.go:452-459), and `remote:` behind `check`+`selector`. Every one of them re-runs on a cursor resume because `resolve` is called again (`transcript.go:270`), and no cursor field can skip a gate: `Selector`, `Source`, `Identity`, `Offset`, `End`, `From`, `To`, `Part`, `Bytes` are all validated (`decodeTranscriptCursor`, transcript.go:146-152) and `Identity` must equal the current file's hash (`:315`, `:329`).
- `transcripts` re-implements the gates correctly per level, including the two that the top-level `gate` does not cover: `listSessions` returns only the caller's session when `allow_other_sessions` is false and says so (`transcripts_list.go:136-158`), and `listProfiles` filters to the caller's profile when `allow_other_profiles` is false (`:179-186`). `mayReadRun` mirrors the `transcript` owner rule (`:411-418`).
- Symlink confinement in the listing (`statTranscriptsFile` transcripts_list.go:94-125, `runItem` `:420-457` with the `insideDirectory` check at `:438`) and in the reader (`deniedFile`, transcript.go:452-459).
- The read-only scanner never rewrites a transcript: an incomplete last line is not visited (`transcript_scan.go:75-78`); no `os.Open` with a write flag exists in the p7b files.
- The turn index uses `session_turn` when present and only falls back to counting, so a compacted transcript is numbered as documented (`transcript_scan.go:98-126`).
- `midCut` keeps head and tail on rune boundaries within the budget and falls back to the bare marker (`transcript_cut.go:17-43`).
- Memory: `checkType` + `target` + regular-non-symlink checks confine every ref to `<root>/<type>/<type>-…json`; filter keys are validated per dotted segment and refuse `__proto__`/`prototype`/`constructor` (`memory_store.go:320-339`); cursor tokens are a 64-byte pattern mapped to a file name (`:831-873`); atomic writes are temp+rename at 0600 (`:707-733`); the F-13 size cache matches its documented stale case and is not re-reported.
- Task: id pattern `T-\d{3,}`, safe-integer bound, per-type text limits, `taskNoExtra` per action, `validStoredTask` + duplicate-id rejection on load, temp+rename write with a size cap (`task_store.go:365-435`).
- Hooks: argv-only execution with no shell, `{file}`/`{dir}`/`{rel}` substitution per argument (`hooks.go:201-217`), the bash environment rules reused (`buildNativeBashEnvironment` with `EnvDeny` and the API-key hide), separate timeouts/kill grace/kill deadline per hook, output cut by the shared collector, and a hook payload that re-cuts itself until it fits `max_payload_bytes` (`hooks_events.go:182-204`). A blocking `pre_tool` failure returns the same `blocked` envelope an unapproved call got and runs nothing after it (`hooks_events.go:128-147`). Post-edit checks run in order per matching hook (`hooks.go:57-66`).
- `WithPostEditHooks` and `WithToolEventHooks` are different hook kinds, so `write`/`edit` are not double-wrapped (hypothesis H4 disproved).
- `request_draft` measures `max_draft_bytes` and `max_draft_depth` on the raw argument JSON before anything else, counts the title in UTF-16 code units, and the headless auto-accept is a recorded user decision (DECISIONS "The approval system is removed", 2026-10-23) — not a finding.
- The remote read path never puts the token, the URL or the query into the model-visible text (`remoteReadError`, `transcript_remote.go:157-180`), and remote output is treated as data: only `cursor:` lines, the first `summary:` and `| path` fields are rewritten (`:194-241`).

## 7. Not covered, and why

- **File-count discrepancy.** The prompt and plan 3.2 say 12 files / 5,342 lines for 7b; the phasemap globs resolve to 22 files / 7,871 lines at `09139f5`. I reviewed all 22. If the coordinator intended only the 12 named in the prompt, `transcript_cut.go`, `transcript_render.go`, `transcript_scan.go`, `hooks_run_events.go`, `memory_group.go`, `memory_json.go` and `task_store.go` are the extra ones; P7B-R2-01 (memory_json.go), -02/-11 (transcript_scan.go/transcript.go) and -05/-06 (transcripts_list.go/task_store.go) are the findings that depend on them.
- **Partly read, by outline only:** `task.go:330-703` (the add/update/delete branches and the state-context collectors; I read the list branch and the validators), `task_store.go:25-196` and `:435-520` (record marshalling, ranking), `memory_json.go:200-287` and `:420-447` (array-like key ordering, canonicalization), `request_draft.go:145-591` (the answer renderer, the ordered-JSON writer and `changedRequestDraftPaths`). The probes exercised the memory JSON writer and reader end to end; the others were only outlined.
- **Test quality** was judged only as evidence for the findings above; the ~4,900 test lines of the 7b area (transcript_test.go, transcript_select_test.go, transcript_list_test.go, transcripts_list_test.go, transcripts_runs_test.go, memory_test.go, task_test.go, hooks_test.go, …) were not audited as a deliverable.
- **No measurement.** Per section 4.4 reviewers do not run benchmarks; every performance item above is `[read]`/`[unmeasured]` until the measurement step (section 6.3).
- **Not exercised at runtime:** the remote transcript path (needs a second JAD and a token table; forbidden for a reviewer), the `path:` selector against a real state root inside a workspace, and Windows behaviour of the hooks/process paths used here.
- **Concurrency:** no `-race` run (forbidden). The only shared state in p7b is the store mutexes/locks analysed in P7B-R2-06.
