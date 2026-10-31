# Low-severity batches (planning only, HEAD 556fa05)

Planning only: no source edits, no commits, no gate run. Excluded: the RunRounds slowdown (final_perf.md, deferred by user) and P8-M-03 (done in 556fa05). Rows the merges mark fixed or rejected are listed in section 4, not batched.

## 1. Scope and method

Scanned: the main severity table of each merge (p1, p2a, p2b1, p2b2, p3, p4, p5, p6a, p6b, p6c, p7a, p7b, p8), plus p7a_approval.md and the deferral notes in final_perf.md. All 13 merge files were readable. Rows with severity `low`, `low (deferred)` or `low (decision)` were kept.

| merge | low-class rows |
|---|---|
| p1_merged.md | 28 |
| p2a_merged.md | 17 |
| p2b1_merged.md | 11 |
| p2b2_merged.md | 20 |
| p3_merged.md | 15 |
| p4_merged.md | 20 |
| p5_merged.md | 19 |
| p6a_merged.md | 14 |
| p6b_merged.md | 19 |
| p6c_merged.md | 19 |
| p7a_merged.md | 13 |
| p7b_merged.md | 27 |
| p8_merged.md | 11 |
| total (unique ids) | 233 |

Status key: **verified** = cited code read at HEAD in this pass; **carried** = verified in the earlier pass, not re-read; **stale** = no longer applies (evidence given); **disputed** = code contradicts the claim; **refuted (merge)** = the merge itself marks it refuted; **unverified** = not read in this pass.

This pass read about 40 cited locations by hand at HEAD. Other rows were matched only by file or symbol presence, which does not check the defect, so they stay unverified.

## 2. Batches (existing 1-10, unchanged in scope)

Batch rules: same package and file together; behaviour and pure cleanup never share a batch; golden-affecting alone. Items marked verified were read at HEAD in this pass. No new batches are added: the other low rows are unverified, and I did not invent batch items for them.

| batch | item | file and symbol | one-line fix | risk | output or config change | proving test |
|---|---|---|---|---|---|---|
| 1 | P7A-12 (part) | internal/tools/native/bash_output.go:186 bashStreamCollector.renderWithProducedBytes: no caller (process_manager.go:462 uses the bashOutputCollector method at :262) | delete the method | low | none | go vet and go test ./internal/tools/native/; grep for leftover references |
| 1 | P6CM-19 (part) | internal/tools/native/subagent_external.go:26 SubagentManager.External: no production caller; one test caller at subagent_persist_test.go:425 | delete External; the test reads the map through its own accessor | low | none | the same package tests |
| 2 | N20 (part) | cmd/openai-auth/browser.go and cmd/claude-auth/browser.go: openBrowser (two copies) | one helper in a shared package (choose the package) | low | none | go build ./cmd/...; package tests |
| 2 | P8-M-10 | internal/transcript/recovery.go:272 hashFile; maintenance.go:834 hashTranscriptFile; prefix_hash.go:14 HashFileRange | one streaming helper; the other two call it | low | none (same digest) | existing hash tests, plus a test that the three agree on one file |
| 2 | P5M-14 | internal/anthropicadapter/response.go:182 itoa (marshals an int with encoding/json) | strconv.Itoa | low | none (same text for ints) | table test for 0, negative and large values |
| 3 | P2A-21 | internal/sessionfs/native_process_settings.go:230 env_deny loop: no TrimSpace (the bash reader trims at native_bash_settings.go:296) | trim the name as the bash reader does | low, behaviour change | a name with surrounding spaces is accepted | config test: ' FOO' accepted, blank rejected |
| 3 | P2B2-08 | stale: see section 3 | none | - | - | - |
| 3 | P2B2-20 (= N26) | internal/sessionfs/remote_api_settings.go:116 listen_unix_mode (range 0o600..0o666, so 0o607 passes) | refuse group or other write bits, with a clear message | low, behaviour change | modes with group or other write refused | test: 0o607 refused, 0o640 accepted; default 0o600 unchanged |
| 3 | P2B1-09 | internal/sessionfs/layout_files.go:189-192 parseLayoutFile (trailing data refused only if the next token decodes) | require io.EOF from a second decode | low, behaviour change | files with trailing bytes refused | test: a file with extra data after the object refused, a clean file accepted |
| 4 | P2B2-16 | internal/sessionfs/tool_shape.go:105-127 unsupportedToolKey / closestToolKey: for a key of length 0 or 1 the distance bound is 1 or 2, so the text reads 'did you mean ?' | print the suggestion only when one was found | low, behaviour change | error text for those keys | test: no 'did you mean' for an empty or one-letter key |
| 4 | P5M-08 (= N29) | cmd/anthropic-adapter/main.go:57 prints config.Upstream.BaseURL verbatim (userinfo and query included) | print scheme, host and path only | low, behaviour change | startup log text | test on the redaction helper: no userinfo or query in output |
| 4 | P5M-13 | internal/openaiauth/token.go:179 truncate cuts at a byte offset (can split a rune) | cut at a rune boundary | low, behaviour change | long issuer descriptions (rare) | test: multi-byte input stays valid UTF-8 after the cut |
| 5 | N28 | internal/logging/logging.go:72 os.OpenFile with 0o600: an existing log keeps its mode (0644 stays 0644) | Chmod 0o600 after open (decided: enforce 0600) | low, behaviour change | mode of existing logs | done (batch 5): logging.go:73-77 Chmod; TestNewTightensAnExistingLogFileToPrivate |
| 5 | M31 | internal/remote/entry.go:262-277 token file: os.Stat then os.ReadFile (TOCTOU) | open once, Stat the descriptor, read from it | low | none for normal files | done (batch 5): entry.go Token reads the open descriptor; TestTokenFileIsReadFromTheCheckedFile |
| 5 | N11 | internal/remote/client.go:96-109 DialContext: owner checked on the socket path before dial, not on the peer reached | keep pre-dial check, add post-dial SO_PEERCRED check (decided) | low, behaviour change | socket owner checks | done (batch 5): client.go:112 checkPeerOwner, socket_unix.go:36; peer uid forced foreign in test (cannot run as another uid) |
| 6 | N12 | internal/app/remote_listen.go:63-71: staging dir .js* plus socket name s lengthens the path, so long socket paths fail | shorter staging name, or an up-front length check with a clear error | low, behaviour change | long socket paths | test: a 100+ byte path gets the chosen result |
| 6 | N27 | internal/app/remote_listen.go:47-80,93-98: two ListenUnix calls on one path both succeed; close unlinks the other's socket | no-replace publication, and an own-inode check on close | low, behaviour change; effort M | a second listen is refused | test: second ListenUnix fails; close leaves the other socket |
| 6 | M29 | internal/remote/entry.go:302-315 List skips non-regular entries (item.Type().IsRegular()), but Load at :322-344 follows a symlink | make List apply the same rule as Load | low, behaviour change | List output | test: List includes a symlinked entry |
| 7 | M25 | internal/asyncmgr/query.go:160-168 Status reads r.ok after m.mu.Unlock(); state and changed are captured under the lock | capture ok under the lock too | low, pure | none | go test -race ./internal/asyncmgr/ |
| 8 | M33 | internal/asyncmgr/store.go:46-62 writeFileAtomic: no temp.Sync() before the rename (the checkpoint writer syncs) | temp.Sync() before close | low, behaviour change (durability, cost) | none in output | no direct test without a seam; acceptance by review |
| 9 | P8-M-13 | internal/transcript/maintenance.go:90 sets result.BackupPath and the counts before the no-op return at :138 | set them only when a rewrite happens | low, behaviour change | the no-op result changes; check whether it reaches messages.jsonl | test: no-op CompactHistory has an empty BackupPath; run messages-golden-diff |
| 10 | P8-M-12 | internal/imagedata/imagedata.go:28-40 Decode: strings.Map copies the whole input (:31) before the length gate (:37) | count non-whitespace bytes without copying, then gate, then copy | low, behaviour change (none for valid input) | none for valid input | test: padded valid input decodes; oversized input still ErrTooLarge |
| 10 | N18 | internal/app/catalog.go:234 list path calls sharedStore, which creates the messages directory (internal/app/resolver.go:959 MkdirAll) for sessions that never ran | create the directory on run, not on list | low, behaviour change | listing no longer creates directories | DONE 099989f: test app/catalog_test.go TestListingANeverRunSessionCreatesNoMessagesDirectory; run-path test gap closed a9ab2c1 (app/run_messages_directory_test.go TestFirstRunCreatesTheMessagesDirectoryThatResolveLeftAbsent) |

Dependencies: M31 before M29 (batch 5 before 6); P8-M-10 before P8-M-13 (batch 2 before 9); P2B2-08, P2A-21 and N26/P2B2-20 need sample/schemes/ and config-test updates in batch 3. Batch 9 is the only golden-affecting batch.

## 3. Status of every low-class row

One row per unique id. `file` is the merge file where the row first appears. Status values are defined in section 1. Rows in batches 1-10 are listed with their batch number.

| id | merge | severity | status | evidence (HEAD 556fa05) or reason |
|---|---|---|---|---|
| N06 | p1 | low | HOLDS: Restore never calls startPendingReclaim (checkpoint/service.go:679-717); the flag is set by storeRecord (:836-837) and started only from Begin (:582) and Finish (:627) |
| N08 | p1 | low | HOLDS: q.try is held from :231 across StartProfileRun (:258) to :277; BeforeRun runs the compaction synchronously (httpapi/profile_run.go:126-127; app/profile_message.go:145-155) |
| N09 | Deferred: the cost is per sub-agent run and only matters for sub-agent-heavy use. | low | perf, needs measurement | S |
| N10 | p1 | low | HOLDS (remote/entry.go:141,145) |
| N11 | p1 | low | done (batch 5; closed) | client.go:112 post-dial checkPeerOwner; socket_unix.go:36. Closing evidence: TestClientRefusesARealForeignPeerAfterTheDial (backend-go/internal/remote/client_unix_test.go:101) runs a daemon-owned socat listener, chowns its socket to this user with one 'sudo -n chown', and checks the post-dial refusal 'answered by another user'. Fails with the checkPeerOwner call removed (mutate-check: 'request failed: EOF'), passes with it. |
| N12 | p1 | low | done (batch 6) | 27f8f28; app/remote_listen.go:63-65 maxUnixSocketPath check (104 bytes), refusal test in remote_listen_unix_test.go |
| N13 | p1 | low | HOLDS (reach narrow, not reproduced): the select at app/remote_api.go:233-244 has no priority; observe runs before await (tools/native/subagent_call.go:146) |
| N14 | p1 | low | HOLDS (service.go:112-127, resolver.go:907) |
| N15 | p1 | low | MOVED (resolver.go:1035-1036) |
| N17 | Deferred: the lock is held across process-kill waits; needs a concurrency test showing a stall. | low | perf, needs measurement | S |
| N18 | p1 | low | done (batch 10, 099989f) | app/catalog.go:234 list calls sharedStore; openSharedSession no longer MkdirAll (app/resolver.go:958-959); transcript/jsonl.go:226-228 RepairTail and :298-300 withRepairedFile open without O_CREATE, missing journal read as empty; input_history_memo.go:67-111 memo reset; run path MkdirAll at app/bound_runner.go:41 before PersistInputWithImages (append at session/runner.go:199, transcript/input_history.go:89). Tests: transcript/missing_journal_test.go TestMissingJournalStaysAbsentWhenRepairedOrRead, app/catalog_test.go TestListingANeverRunSessionCreatesNoMessagesDirectory. Changed transcript/recovery_test.go:270 existence check to allow an absent journal. |
| N19 | p1 | low | HOLDS (dead list checked at HEAD) |
| N20 | p1 | low | verified | cmd/openai-auth/browser.go:11, cmd/claude-auth/browser.go:11 |
| N21 | p1 | low | HOLDS (misplaced comment at app/resolver.go:36-38; resolver.go is 1175 lines and catalog.go 632 at HEAD, merge said 1122 and 768) |
| N22 | p1 | low | HOLDS in part: gaps stand for N13, N27, N06 (restore gc), N32 (mid-restore cancel); tests exist for N01, N02, N03, N04, N05, N12, N16, N18 (REFUTED items); sleep counts match (6,4,4,2,2) |
| N23 | p1 | low | MOVED (resolver.go:929, :710) |
| N24 | Deferred: concurrent transcript reads from a remote client are not yet seen. | low | perf, needs measurement | S |
| N25 | p1 | low | HOLDS (remoteapi/server.go:351-358) |
| N26 | p1 | low | verified | remote_api_settings.go:116 (same as P2B2-20) |
| N27 | p1 | low | done (batch 6) | 2ebad42; app/remote_listen.go Link publish and unixListener.info own-inode Close; tests in remote_listen_unix_test.go |
| N28 | p1 | low | done (batch 5) | logging/logging.go:72-77 Chmod 0o600 after open |
| N29 | p1 | low | verified | cmd/anthropic-adapter/main.go:57 (same as P5M-08) |
| N30 | Deferred: lock contention on concurrent turn starts is not measured. | low | perf, needs measurement | S |
| N31 | p1 | low | stale | tools/native/transcript.go:146-152 rejects Bytes < 0 |
| N32 | p1 | low | HOLDS: the handler passes r.Context() (httpapi/checkpoint_handlers.go:122) to Restore, which reaches git via exec.CommandContext (checkpoint/git.go:167) |
| N33 | p1 | low | perf, needs measurement | R |
| N34 | Deferred: tool rebuild per call matters only when remote transcript latency is a problem. | low | perf, needs measurement | R |
| N35 | Deferred: the re-render cost matters only near `max_bytes`. | low | perf, needs measurement | R |
| P2A-05 | p2a | low | HOLDS (native_process_settings.go:272-313, native_task_settings.go:172-213) |
| P2A-06 | p2a | low | HOLDS (all five drift points checked key by key against sample/schemes/setting.schema.json and sessionfs loader; see verdict table) |
| P2A-07 | p2a | low | closed: helper nativeEnvelope in b271a02; read settings converted in 9e779d7. tools.extension (extension_decide_systemone_settings.go:58,125) keeps its current behaviour by decision: a null extension node is absent, a null decide_systemone is refused at section level. Not aligned with the P2A-08 refusal, because loaded tools.native null is rewritten to {} (tool_shape.go:50-58) and aligning extension would make it the only node that refuses on the loaded path. Residual: no test pins the null-extension skip; a test is not added by decision. Item 1: the copy counts in the earlier analysis were not reproduced at b271a02, and the null-skip drift (sections guard literal null differently) is unchanged and open under P2A-08. |
| P2A-08 | p2a | low | closed, not reproduced on the loaded path (native/tools half). The 9-vs-10 refuse/skip split appears only when section normalizers are called directly. On the loaded path, validateToolSettingsShape (tool_shape.go:50-58) rewrites tools.native null to {} before any normalizer runs, and loadSessionSettingsFrom calls it at session.go:255. Parity: legacy-final:backend/src/config.ts:2452 is `(tools as JsonRecord).native ?? {}`. Top-level tools null is refused by the same shape check (tool_shape.go:36-38). No code change, no commit, no tests. |
| P2A-08b | p2a | low | closed (on the loaded-path evidence recorded here; no code change, no tests, no commit). Probed through layout_load.go:79 -> session.go:251 (throwaway tests, deleted). memory.allowed_types null: Go refuses ("must contain safe type names"); TypeScript also refuses (legacy-final config.ts:2993-2995). memory.statecontext null: Go refuses; TypeScript refuses (config.ts:2152-2153). process.statecontext null: Go refuses ("must be an object"); TypeScript refuses (config.ts:2137-2138). task.statecontext null: Go refuses ("must be an object"); TypeScript refuses (config.ts:2122-2123). No loaded-path mismatch for these four TypeScript-backed keys. bash.kill_children_on_run_end "true": Go refuses ("must be on or off"); Go-only key, no TypeScript counterpart (grep of legacy-final config.ts empty), so no parity to check. subagent.statecontext null: accepted Go-only default; loads and resolves to States [queued running], Limit 20, Mode items, same as the control. No TypeScript counterpart (legacy config.ts has no subagent section). Not a gap; not to be refused. Limit: one bad value per key was probed (null, or a string for kill_children); other wrong types (numbers, objects, other strings) were not probed. |
| 4c51997-gap | 4c51997 | low | accepted gap (decider, option 1; no tests, no production change, no commit). Gap: the non-PathError text change in claudeauth and openaiauth Store.Read/Update (store.go Read and the mkdir/backup-read errors, now wrapping the real error instead of %!w(<nil>)) is not pinned by a Store-level test. Reason: no non-PathError is reachable through os.ReadFile or os.MkdirAll on these paths; probe on go1.27.3 gave *fs.PathError for a NUL-byte path, a directory, and mkdir under a regular file. Store has no seam, and the decider declined a seam without a reachable failure. The helper branch is pinned by fserr_test.go:41-46 (TestUnwrapPathErrorLeavesAnErrorWithoutAPathUnchanged). Revisit trigger: a read path that can return a non-PathError (network filesystem, wrapped custom error). |
| P2A-10 | p2a | low | HOLDS (native_fetch_settings.go:37,201) |
| P2A-11 | p2a | low | HOLDS (native_read_image_settings.go:12-13,54-59) |
| P2A-12 | p2a | low | HOLDS (layouttest.go:341-355) |
| P2A-13 | p2a | low | HOLDS (layouttest.go:199-202) |
| P2A-14 | p2a | low | HOLDS (native_exec_settings.go:288-300) |
| P2A-15 | p2a | low | HOLDS (native_subagent_settings.go:336-356) |
| P2A-16 | p2a | low | HOLDS (native_transcript_settings.go:90-109) |
| P2A-17 | p2a | low | HOLDS (native_subagent_settings.go:89-129) |
| P2A-18 | p2a | low | perf, needs measurement | S |
| P2A-19 | p2a | low | confirmed at cac77d1 (doc comment native_subagent_settings.go:183-186 contradicts :159, :649-660) | S |
| P2A-20 | p2a | low | HOLDS (native_request_draft_settings.go:72-74) |
| P2A-21 | p2a | low | closed (batch 3, a7ad56b) | native_process_settings.go:223-232 trims env_deny names as native_bash_settings.go:299 does; test TestNormalizeNativeProcessEnvDenyTrimsNames failed before the fix |
| P2A-22 | p2a | low | HOLDS (tool-list case table covers 14 tools; async, task, memory, skill and request_draft absent; the subagent key test is TestSubagentSchemasAgreeWithTheLoader, not the name merge gives) |
| P2B1-04 | p2b1 | low | HOLDS (layout_profile.go:40, model_effort.go:68-78, layout_load.go:102-107) |
| P2B1-05 | p2b1 | low | HOLDS (settings_stamp.go:51-53) |
| P2B1-06 | p2b1 | low | HOLDS (layout_settings.go:120,174-203; catalog_files.go:492; session.go:727) |
| P2B1-07 | p2b1 | low | closed, accepted (DECISIONS.md:1166: config symlinks are supported; O_NOFOLLOW deliberately not added; no code change, no commit, no fail-first test; existing test TestReadConfigTextFileReadsASymlinkedConfigFile pins it) |
| warning loop (cmd/jad/main.go:74-76, test-gap batch 2) | be71753 | low | dropped (decider): loader warnings already tested by TestLoadWarnsOnUnknownRootSettingKeysWithoutRefusing; no seam, no test |
| P2B1-08 | p2b1 | low | HOLDS (roots.go:399-470, roots.go:502-510, layout_root.go:63-73) |
| P2B1-09 | p2b1 | low | closed (batch 3, a7ad56b) | layout_files.go:188-190 second decode must return io.EOF; TestLayoutFileRefusesDataAfterTheObject failed before the fix |
| P2B1-10 | p2b1 | low | HOLDS (roots.go:220-235, layout_root.go:123-129) |
| P2B1-11 | p2b1 | low | HOLDS (session.go 1308 lines; LayoutSettingKey layout_files.go:216 no caller) |
| P2B1-12 | p2b1 | low | HOLDS (layout_profile.go:40, layout_load.go:102-107) |
| P2B1-13 | p2b1 | low | HOLDS (layout_files.go:196-211) |
| P2B1-14 | p2b1 | low | perf, needs measurement | S |
| P2B2-06 | p2b2 | low | closed dc3f4b5 (rule side): deny rules in allowedByNativeReadPolicy now match the resolved path too; test TestCollectSessionFilesDenyRuleMatchesTheResolvedWorkspacePath failed at 8fa87bb and the removal mutation was CAUGHT. Previously visible denied files under a symlinked workspace root are now hidden; intended. Allow clause unchanged. |
| P2B2-07 | p2b2 | low | perf, needs measurement | S |
| P2B2-08 | p2b2 | low | stale | backend-go/internal/sessionfs/tool_settings.go:168 bounds default_limit with maxSafeInteger, but output is capped by max_bytes (tools/native/read.go:582-590, called at :424), file size by max_file_bytes (:291-292), listing by visibleCount (:361-365) |
| P2B2-09 | Deferred by user decision. | low | perf, needs measurement | S |
| P2B2-11 | p2b2 | low | HOLDS (runner.go:181-201, input_history.go:223-233) |
| P2B2-12 | p2b2 | low | HOLDS (storage_paths.go:88-113, tool_storage.go:10-31) |
| P2B2-13 | p2b2 | low | HOLDS (session_files.go:225-266) |
| P2B2-14 | p2b2 | low | perf, needs measurement | R |
| P2B2-15 | p2b2 | low | HOLDS (tool_batch.go:119, serverconfig/defaults.go:54; B-C8) |
| P2B2-16 | p2b2 | low | done (batch 4) | tool_shape.go:106 prints the suggestion only when one exists; test TestUnsupportedToolKeyWithoutSuggestion (tool_shape_test.go) failed on the old code with 'did you mean ?' |
| P2B2-17 | p2b2 | low | HOLDS (files.go:143-165, file_settings.go:264-278) |
| P2B2-18 | p2b2 | low | HOLDS (runner.go:921-933, tool_batch.go:18; app/subagent.go is 223 lines) |
| P2B2-19 | p2b2 | low | closed, accepted (no code change): configSafeInteger accepts 1e3 and 2.5e1 because they are integers, which the JSON schema also admits. Refusing them (option 1) would reject schema-valid configs across about 89 keys with no security impact. The three tool-limit keys are already refused (71c2417). The cost of a very long literal is P2B2-14; bound literal length there if needed. Known divergence, accepted: `image_store` (`image_store_settings.go`, `json.Unmarshal` into `int64`) refuses `1e3` while `configSafeInteger` and `sessionFileInteger` (`file_settings.go:259-273`, `float64` parse) accept it. All three accept `1024`. This is a strictness difference, not a safety one; unifying the leaf parsers was reverted in 7dcc35d and is not reopened here. |
| P2B2-20 | p2b2 | low | closed (batch 3, a7ad56b) | remote_api_settings.go:122-127 refuses any group or other write bit; TestRemoteAPIRefusesAnyGroupOrOtherWriteBit failed before the fix |
| P2B2-21 | p2b2 | low | HOLDS (extension_decide_systemone_settings.go:84-89) |
| P2B2-22 | p2b2 | low | HOLDS (file_settings.go:178, files.go:205; documented contract) |
| P2B2-23 | p2b2 | low | refuted (merge) | S (partly refuted) |
| P2B2-24 | p2b2 | low | HOLDS (four bounded closures; 22 decodeConfigObject files) |
| P2B2-25 | p2b2 | low | split in two. Sleep half: closed 87fe381 (three sleeps replaced with gates or removed; the failure test still sleeps, since nothing observable marks when the first error is recorded). Run split: deferred per decider (runner.go Run is 247 lines, 397-643; no behaviour gain, large move). |
| P2B2-26 | p2b2 | low | HOLDS (remote_api_settings.go:123-131; file_settings.go:151 [index]) |
| P3-06 | p3 | low | HOLDS (async_state.go:97-108, provider_retry_state.go:50-62, registry.go:519-523) |
| P3-07 | p3 | low | HOLDS (registry.go:332, :844; retention by time only) |
| P3-08 | p3 | low | HOLDS (registry.go:770-778 disposeContent; only caller retire :488) |
| P3-09 | p3 | low | HOLDS (library_handlers.go:48-56; session_catalog_files.go:27-29; native_config_file.go:42,45,52) |
| P3-10 | p3 | low | HOLDS (httpdebug/transport.go:28,31,99-104,338-379) |
| P3-11 | p3 | low | HOLDS (catalog_files_handlers.go:12-24, etag without omitempty) |
| P3-12 | p3 | low | HOLDS (mutation_handlers.go:79-95; front_compact_handler.go:35-48) |
| P3-13 | p3 | low | HOLDS (request_observer.go:126-162) |
| P3-14 | p3 | low | HOLDS (registry.go:123-171 oversized snapshot not charged) |
| P3-15 | p3 | low | REFUTED (failed-start guard finish() stores error+done frames; beginSubscribe replays r.terminal, registry.go:541-545) |
| P3-16 | p3 | low | HOLDS (run_content_cache.go:442-476) |
| P3-17 | p3 | low | HOLDS (start_turn.go:90-115; process_lock.go:5-16 no-op without locker) |
| P3-18 | p3 | low | refuted (merge) | S (refuted as a defect) |
| P3-19 | p3 | low | HOLDS (7 MaxBytesReader sites; 4 dead helpers; handler.go 1012, registry.go 877) |
| P3-20 | p3 | low | HOLDS (27 time.Sleep in 12 files, merge said 24/10; stale comment vs SSEWriteTimeout handler.go:54-56) |
| M03 | p4 | low | REFUTED for removals (service.go:778-796 parentIsPlain, EvalSymlinks); checkout half holds (git.go:373-386) |
| M12 | p4 | low | HOLDS (manager.go:473-485 cancel overwrites r.after) |
| M16 | p4 | low | fixed in 62bdf4b, docs 897e503, tests d590792; on-disk format unchanged |
| M17 | p4 | low | HOLDS (service.go:179,183 package maps; cited :161-170 stale; never deleted) |
| M18 | p4 | low | HOLDS for ErrNotLive, MatchGlobs, CancelOwnersWithPrefix; remote/entry.go:59 is a field, not dead |
| M19 | p4 | low | HOLDS (5 cut-helper copies, moved): asyncmgr/types.go:192; remoteapi/server.go:608,619; tools/native/skill.go:152; webui/hosted_tool.go:204; remote/client.go:293 (safeText, moved from :276) |
| M20 | p4 | low | HOLDS (remoteapi/server.go:454-471; precedence hard to read) |
| M21 | p4 | low | HOLDS (manager.go:231-238 forgetLocked keeps owner when dropped>0) |
| M22 | p4 | low | confirmed at cac77d1 (MarkRunning unlocks before writeRecord, asyncmgr/manager.go:346-360; guard bounds but does not close the window) | S |
| M23 | p4 | low | HOLDS (prune.go:11-12 DaysDuration; config.go:97-99 negatives only; cap 10,000,000) |
| M24 | p4 | low | HOLDS (tokens.go:253-259, :281-283; run.go:148-154) |
| M25 | p4 | low | done (batch 7) | asyncmgr/query.go:160 captures ok under the lock; race test manager_test.go TestStatusReadsOkUnderTheLockWhenTheRecordIsGone (old code: DATA RACE at query.go:167) |
| M26 | p4 | low | closed 8fa87bb (test TestWaitTimeoutCancelledTakesNoSignal with test-only hook beforeWaitTimeout; mutation popLocked above ctx.Err: CAUGHT) |
| M27 | p4 | low | HOLDS (remoteapi/server.go:281-300, backendError :509-510) |
| M28 | p4 | low | HOLDS (checkpoint/run.go:12-39; tools/catalog.go:281; runner_tools.go:183) |
| M29 | p4 | low | done (batch 6) | f6940e4; remote/entry.go List drops the IsRegular filter (Load rule); TestListIncludesASymlinkedEntryAsLoadDoes |
| M30 | p4 | low | HOLDS (remoteapi/server.go:419-421) |
| M31 | p4 | low | done (batch 5) | remote/entry.go:262-290 open once, Stat and read the descriptor |
| M32 | p4 | low | HOLDS (manager_test.go:~460-473 150ms wait with no handshake) |
| M33 | p4 | low | done (batch 8, review-only) | asyncmgr/store.go:53-55 temp.Sync() before Close; no sync or file seam in asyncmgr, so no test. Mutation not run (no seam). Gate exit 0. |
| P5M-03 | p5 | low | REFUTED for the backend half (openaioauth Message() has no path); adapter half holds (credential.go:30-32,124) |
| P5M-04 | p5 | low | CHANGED: capped at upstream.max_event_bytes by 62b0d17 (stream_sse.go:36-39) |
| P5M-06 | p5 | low | refuted (merge) | S (refuted as a defect) |
| P5M-07 | p5 | low | CHANGED: openaiauth Store.Update makes dir 0700 first (e8e22c7; store.go:174-178) |
| P5M-08 | p5 | low | done (batch 4); test gap closed a9ab2c1 (TestDisplayUpstreamURLFallsBackForAMalformedURL) | cmd/anthropic-adapter/main.go:57 prints displayUpstreamURL(...) (scheme, host, path); helper at main.go:~85; test TestDisplayUpstreamURLDropsUserinfoAndQuery failed to compile before the helper existed, mutation to raw URL CAUGHT |
| P5M-09 | p5 | low | CHANGED in part: claudeauth checks state before error (login.go:231-243); late expectState still holds (:79-83) |
| P5M-10 | p5 | low | CHANGED: isPermanentTransportError excludes TLS and scheme errors (1490418; openaiapi/retry.go) |
| P5M-11 | p5 | low | CHANGED: scaledDuration clamps before conversion (1490418; openaiapi/retry.go:126-130) |
| P5M-12 | p5 | low | closed 4c51997: 12 sites now use fserr.UnwrapPathError (errors.As on *fs.PathError), messages and %w unchanged; one intended change: an error without a PathError returns itself, which removes a nil dereference that errors.Unwrap allowed. Mutation (return err in the helper) CAUGHT. |
| P5M-13 | p5 | low | done (batch 4) | openaiauth/token.go:179-190 cuts at a rune start via utf8.RuneStart; test TestTruncateKeepsMultiByteTextValid failed on the old code (\xc3 split), mutation CAUGHT |
| P5M-14 | p5 | low | verified (batch 2) | anthropicadapter/response.go:182-185 json.Marshal |
| P5M-15 | p5 | low | refuted (merge) | S (benign) |
| P5M-16 | p5 | low | HOLDS (claudeauth/refresh.go:61,74 clock read after the exchange) |
| P5M-17 | Accepted by user decision: a maintainability cost, not throughput; the trigger is a maintainability event, not a metric. | low | perf, needs measurement | S |
| P5M-18 | p5 | low | refuted (merge) | S (documented) |
| P5M-19 | p5 | low | HOLDS (openaiauth/config.go:85 hostPattern accepts any bare host) |
| P5M-20 | p5 | low | refuted (merge) | S (refuted as a defect) |
| P5M-21 | p5 | low | HOLDS (cmd/claude-auth/main.go:127 has --copy-code; openai-auth has none) |
| P5M-22 | p5 | low | refuted (merge) | S (no defect) |
| P6AM-03 | p6a | low | perf, needs measurement | R |
| P6AM-06 | p6a | low | CHANGED: Close cancels connectCtx and waits on connects (mcp/manager.go:491,500); getConnection checks m.closed first (:383-388) |
| P6AM-09 | p6a | low | CHANGED in part: question bytes now count toward max_input_bytes (extension/decide_systemone.go:217-221); no per-question option cap |
| P6AM-10 | p6a | low | CHANGED: redacts before the 400-byte cut (extension/decide_systemone_backend.go:121-144) |
| P6AM-11 | p6a | low | CHANGED: Confidence is *float64 and out-of-range probabilities are refused (extension/decide_systemone_backend.go:299-308,416-424,454) |
| P6AM-13 | p6a | low | CHANGED: recovered tool failures are logged (tools/catalog.go:373,409,415); panicking-normalizer test exists (catalog_test.go:368) |
| P6AM-14 | p6a | low | CHANGED: a non-fitting text part is cut wherever it sits and the budget uses the encoded size (mcp/content.go:167-185, fitTextPart :217-230) |
| P6AM-15 | p6a | low | CHANGED: envelope uses MarshalJSON with SetEscapeHTML(false) (tools/catalog.go:481-491; tools/output.go:151-160) |
| P6AM-16 | p6a | low | HOLDS (partly): duplicates remain (extension/text.go:20 oneLine; extension/decide_systemone_backend.go:285 firstNonEmpty; tools/native/skill.go:152 cutUTF8); mcp copies were removed |
| P6AM-17 | p6a | low | CHANGED: CutText clamps negative limit (tools/text.go:6); extension.cutUTF8 gone |
| P6AM-18 | p6a | low | refuted (merge) | S (refuted as a defect) |
| P6AM-19 | p6a | low | CHANGED: stdio process group kill on close (mcp/manager.go:154; process_group_unix.go:14,26); TestManagerStopsStdioGrandchildOnClose (manager_process_group_unix_test.go:69) |
| P6AM-20 | p6a | low | refuted (merge) | R (refuted) |
| P6AM-21 | p6a | low | CHANGED: schema_dialect_debug_test.go gone; 1e400 test (structured_content_order_test.go:60); pagination and in-flight Close tests exist |
| P6BM-05 | p6b | low | HOLDS: cutBlockTail keeps the tail (async_latest.go:57-63, :82-93), cursor still advances (:63); the head-cut helper is skill.go:152
| P6BM-07 | p6b | low | CHANGED: AsyncHost.Close now waits for shell runs (native/async_host.go:96-97, :105-120) |
| P6BM-08 | p6b | low | CHANGED: recordLiveManifest refuses writes over a terminal state (native/process_manager.go:808-830) |
| P6BM-09 | p6b | low | CHANGED: Close waits on finishing after the stops (native/process_manager.go:618-620), covering prune after done (:764-765) |
| P6BM-10 | p6b | low | CHANGED in part: start honours a stop that came first (process_manager.go:402-408); the pid<=0 path in Stop still sets LeftRunning (:548-549) |
| P6BM-11 | p6b | low | HOLDS: problems and strays are uncapped (native/exec_functions.go:246-255), only shown is capped (:272-274) |
| P6BM-12 | p6b | low | CHANGED: staleScratchDirs also sweeps /tmp (native/scratch_sweep.go:35-41) |
| P6BM-14 | Deferred: capped by `list_max_files` and `function_max_bytes`; matters only at the cap. | low | perf, needs measurement | S |
| P6BM-15 | Deferred: matters only when a live handle has a long transcript on the async path. | low | perf, needs measurement | S |
| P6BM-16 | p6b | low | perf, needs measurement | S |
| P6BM-17 | p6b | low | HOLDS: openLibraryFile reached from exec_functions.go:61, tool_smith_fn.go:83, tool_smith_list.go:290 |
| P6BM-18 | p6b | low | HOLDS: process_manager.go is 1235 lines at HEAD (merge: 1117); env builder at :860 duplicates the bash one (bash.go:382-386) |
| P6BM-19 | p6b | low | partly stale | Directory() does not exist; executeNativeExec is test-only (exec_test.go:91) |
| P6BM-20 | p6b | low | CHANGED: exec.go:354 now uses callID; the empty id is gone |
| P6BM-21 | p6b | low | HOLDS: process.go:172 still returns err.Error() as process_start_failed |
| P6BM-22 | p6b | low | HOLDS: sleeps still in tests (bridge 4, run 2, child_groups 2, async 1 at HEAD; merge counts differ) |
| P6BM-23 | p6b | low | HOLDS: stopOrphanGroups re-checks existence only (native/process_orphan.go:131-157) |
| P6BM-24 | p6b | low | CHANGED in part: Recover now prunes (process_manager.go:1044-1050); finish still prunes after done (:765) |
| P6BM-25 | p6b | low | HOLDS (fact): async tool_smith nils interaction hooks (native/tool_smith_async.go:22-26) |
| P6CM-09 | p6c | low | CHANGED in part: sub_ ids are drawn through freeRunIDLocked with a collision check (subagent_manager.go:521-537, :547); rs_ (subagent_remote.go:108) and pm_ (profile_message_async.go:170) ids have no check |
| P6CM-10 | p6c | low | HOLDS: remoteProblem embeds the loader error with %v (profile_message_remote.go:85-91) |
| P6CM-11 | p6c | low | HOLDS: boundedReason bounds length but does not scrub (subagent.go:721-728); reason stored in the manifest (subagent_manager.go:368) |
| P6CM-12 | p6c | low | HOLDS: finish closes run.done before the deferred release runs (subagent_manager.go:650-700, :743-773) |
| P6CM-14 | p6c | low | HOLDS: no recover in subagent_remote.go, profile_message_async.go, profile_message_remote.go, async_host.go (grep empty) |
| P6CM-15 | p6c | low | HOLDS: the limit check is nested-only (subagent_manager.go:562) while byRoot/bySession increment unconditionally (:582-583) |
| P6CM-16 | p6c | low | CHANGED in part: four cancel helpers still exist (subagent_manager.go:873-911); the reason now reaches non-handle runs via cancelActive (:911-922) |
| P6CM-17 | p6c | low | HOLDS: the two handle drivers and renderers exist (profile_message_async.go:135-229 vs subagent_remote.go:80-184) |
| P6CM-18 | p6c | low | HOLDS (carried): write uses CreateTemp run-*.tmp with defer Remove (subagent_store.go:96-98); load skips non-sub_ names (:146) |
| P6CM-19 | p6c | low | done (d4d38f9) | SubagentManager.Closed deleted (was internal/tools/native/subagent_manager.go:941, 7 lines). Its two readers now test through the resolver: app/subagent_manager_lifecycle_test.go:18-63 (cancel on Close via blockingLauncher, refused start after Close, launcher count, lines 54-61) and app/subagent_manager_build_test.go:226-239 (len(orphans) != 1 first, then refused start and launcher count on the orphan from subagentOrphanCatalog:360). Route (ii) config via subagentSampleTool (subagent_integration_test.go:357) and layoutConfig. Mutations: (a) skip close loop resolver.go:1028-1030 -> lifecycle CAUGHT at :54 (cancel wait); (b) drop m.closed=true subagent_manager.go:947 -> lifecycle CAUGHT at :58 (refused start); (c) drop orphan Close resolver.go:750-752 -> orphan test CAUGHT at build_test.go:234 and :237. Gate: go vet ok; go test -count=1 native rc 0, app rc 0; verify-go.sh --race ./internal/tools/native/ ./internal/app/ rc 0 (gofmt, vet, test, race, diffcheck all 0); git diff --check rc 0. The 'Directory()' sub-item is refuted (merge) |
| P6CM-20 | p6c | low | HOLDS (first half): normalizeSubagentArguments clamps negative block_ms to 0 (subagent.go:681-682); second half stale |
| P6CM-21 | p6c | low | CHANGED: agentNames takes s.mu.RLock (subagent.go:707-709) |
| P6CM-22 | p6c | low | CHANGED: restoredSubagentRun sets a cancelled ctx (subagent_manager.go:375-380) |
| P6CM-23 | p6c | low | CHANGED: blockFor now checks s.async nil (subagent_call.go:178-181) |
| P6CM-24 | p6c | low | CHANGED: summary is cut to resultMax minus the files line (subagent_async.go:56-63) |
| P6CM-25 | p6c | low | HOLDS: path.Match glob for sessions vs exact DenyProfiles (profile_message.go:373-395) |
| P6CM-26 | p6c | low | accepted per decider: file split not approved without a neutrality argument; sizes confirmed at HEAD (988, 728). |
| P6CM-27 | p6c | low | HOLDS (count differs): 12 time.Sleep in the phase's test files at HEAD (merge: 34); the merge's named files show 0 sleeps |
| P6CM-28 | Deferred: matters only with many sub-agent manifests in one profile. | low | perf, needs measurement | S |
| P7A-04 | p7a | low | CHANGED: ripgrep and glob children use the bash allowlist (native/ripgrep.go:101,353-365; glob.go:236) |
| P7A-05 | p7a | low | HOLDS: validateFetchURL applies no port policy; the port is only canonicalised and dialled (native/fetch.go:388-444, :439, :553-573) |
| P7A-06 | p7a | low | CHANGED: OpenWithin refuses by size before reading (imagestore/store.go:520-535; native/image_file.go:25) |
| P7A-07 | p7a | low | perf, needs measurement | confirmed (repro, exit 0) |
| P7A-08 | p7a | low | perf, needs measurement | confirmed (read), not reproduced |
| P7A-09 | p7a | low | HOLDS: the redirect loop re-validates each hop (native/fetch.go:295-318) with no scheme-downgrade check (not reproduced, no network) |
| P7A-10 | p7a | low | CHANGED: the local read verifies the opened file with SameFile and IsRegular (native/path.go:126-154); the FIFO swap is rejected |
| P7A-11 | p7a | low | verified | tools/native/path_locks.go:22-26 nil receiver locks nothing shared |
| P7A-12 | p7a | low | verified | bash_output.go:186 method has no caller; fetch.go:626 is readFetchResponseTruncating |
| P7A-13 | Deferred: fix would keep the UTF-16 length in the stored metadata. | low | perf, needs measurement | confirmed (read); perf unmeasured (M3) |
| P7A-14 | p7a | low | perf, needs measurement | confirmed (read); perf unmeasured (M2) |
| P7A-15 | p7a | low | perf, needs measurement | confirmed (read); perf unmeasured (M4) |
| P7A-16 | p7a | low | HOLDS: scopedPolicy is called only by edit, write, transcript_path and transcripts_list (native/path_scope.go:14; edit.go:92; write.go:94; transcript_path.go:78; transcripts_list.go:133); read, grep, glob and read_image do not consult it |
| P7B-MRG-03 | p7b | low (deferred) | perf, needs measurement | **confirmed, reproduced** (251.7 MB vs 1.38 MB). **Deferred as low by  |
| P7B-MRG-04 | p7b | low (deferred) | perf, needs measurement | **confirmed, reproduced** (flat 5.2-5.3 MB per page). **Deferred as lo |
| P7B-MRG-05 | p7b | low (deferred) | perf, needs measurement | **confirmed, reproduced** (0.73 → 2.10 MB for 40 → 400 turns, output f |
| P7B-MRG-06 | Deferred: part of the P7B-MRG-04 measurement. | low | perf, needs measurement | **confirmed by reading**; its cost is part of the MRG-04 measurement |
| P7B-MRG-08 | p7b | low | HOLDS: the cursor branch checks only the remote switch (native/transcript_remote.go:251-264); entry.Allows runs only on the path branch (:104, via selector :269-270) |
| P7B-MRG-09 | p7b | low | HOLDS: memory and task writes Close and Rename with no Sync (native/memory_store.go:762-786, task_store.go:406-429); Sync is absent from both stores |
| P7B-MRG-10 | p7b | low | HOLDS: taskStorageError returns the store error text (native/task.go:558-563); storage errors carry %v of the OS error (task_store.go:365-395) |
| P7B-MRG-11 | p7b | low | HOLDS in part: raw Go errors returned (native/skill.go:159-161); the absolute directory field (:197-203) is not re-read |
| P7B-MRG-12 | p7b | low | HOLDS: memoryExecutionError returns raw err.Error() for non-store errors (native/memory.go:268); store OS errors pass through unwrapped (memory_store.go:726-786) |
| P7B-MRG-13 | p7b | low | HOLDS (latent): forwardRemoteArguments has no int or int64 arm (native/transcript_remote.go:234-247); model args decode with UseNumber (tools/json_bounds.go:14) |
| P7B-MRG-15 | p7b | low | HOLDS: the persist_runs note is behind !target.PersistRuns (native/transcripts_list.go:370-371); own sets Runs (:357) |
| P7B-MRG-16 | p7b | low | HOLDS: TaskStore.mu is per instance (native/task_store.go:197); resolver shares one store per key in-process (app/resolver.go:768-786) |
| P7B-MRG-17 | p7b | low | HOLDS: seven BuildCatalog constructors chain through each other (native/catalog.go:50-88); the 25-arm count is not re-checked |
| P7B-MRG-18 | p7b | low | HOLDS: memory.go:262 maps status by substring 'not found' though MemoryStoreError.Kind exists (memory_store.go:35-46) |
| P7B-MRG-19 | Deferred: memory writes are not seen in a profile. | low | perf, needs measurement | **confirmed by reading**; unmeasured |
| P7B-MRG-20 | p7b | low | perf, needs measurement | **confirmed by reading**; unmeasured; small (bounded by `max_cursors`) |
| P7B-MRG-21 | p7b | low | HOLDS: transcriptIdentity hashes the first line only, capped at 4 KiB (native/transcript_scan.go:130-141) |
| P7B-MRG-22 | p7b | low | HOLDS: skill returns Go errors (native/skill.go:159-161) where its neighbour returns ExecutionError (:166) |
| P7B-MRG-23 | p7b | low | perf, needs measurement | **confirmed by reading**; unmeasured (`BenchmarkTaskStoreList100` exis |
| P7B-MRG-24 | Deferred: lock wait across the scan matters only under concurrent writes. | low | perf, needs measurement | **confirmed by reading**; unmeasured. F-13 (`5d09534`) fixed the Stat  |
| P7B-MRG-25 | Deferred: matters only for a hook that watches a large file. | low | perf, needs measurement | **confirmed by reading** (both calls at `hooks.go:123` and `:158`); un |
| P7B-MRG-26 | p7b | low | HOLDS: request_draft.go:403 and memory_json.go:163 are two ordered-JSON decoders; json_bounds.go:12,120 is a third measurement |
| P7B-MRG-27 | p7b | low | perf, needs measurement | **confirmed by reading** (the loop at `:427-431` is as read); unmeasur |
| P7B-MRG-28 | p7b | low | accepted per decider: file splits not approved; sizes confirmed at HEAD (memory_store.go 1116, task.go 703, catalog.go 632, request_draft.go 591). |
| P7B-MRG-29 | p7b | low | accepted per decider: wrappers kept. Correction: they are not dead, because BuildCatalog and the other wrappers are called from about 20 test files (catalog_*_test.go). Removal is a test-wide rewrite, not a maintainability fix. |
| P7B-MRG-30 | p7b | low (decision) | refuted (merge) | **confirmed by reading**; a documented-or-fix decision, not a defect |
| P7B-MRG-31 | p7b | low (deferred), partly fixed in `632ebed` | perf, needs measurement | **measured** (about 240 MB of the 294.7 MB at HEAD, 400 turns). **Defe |
| P8-M-04 | p8 | low | carried | earlier pass (low_batches.md section 5) |
| P8-M-05 | Deferred: a durability question, not throughput; belongs to the image-store durability review (low_batches.md section 8). | low | perf, needs measurement | confirmed (read); impact narrower, see below |
| P8-M-06 | p8 | low | perf, needs measurement | confirmed (read); prior art B-C5 is accepted-low, not open |
| P8-M-07 | Deferred: the observer runs under the turn lock; matters only if an observer is slow. | low | perf, needs measurement | confirmed (read); no deadlock found |
| P8-M-09 | p8 | low | perf, needs measurement | confirmed (read); cost unmeasured |
| P8-M-10 | p8 | low | verified (batch 2) | recovery.go:272, maintenance.go:834, prefix_hash.go:14 |
| P8-M-11 | p8 | low | HOLDS: ReadCursorIndex takes cursorMu (transcript/cursor_cache.go:61-63) but rewrites do not (maintenance.go:656,823; jsonl.go:126; front_compact.go:248,284); cache is self-checking (cursor_cache.go:160-169) |
| P8-M-12 | p8 | low | done (batch 10, fc10205) | imagedata/imagedata.go:37-47 count before copy at :31-36 |
| P8-M-13 | p8 | low | done (batch 9) | transcript/maintenance.go:189-195 CompactedItems and BackupPath set after the backup succeeds, past the no-op return (:179-181 area) and after backupTranscript; RetainedItems at :193 already on rewrite. Test: maintenance_test.go TestCompactHistoryNoOpWhenRecentTurnDoesNotReduceHistory asserts 0/0/empty BackupPath. Fail on HEAD: observed CompactedItems:1 RetainedItems:1 BackupPath:/tmp/.../backup.jsonl. Mutation (HEAD placement restored): CAUGHT. Golden: scratch fixture, HEAD worktree vs working tree, messages.jsonl no-op (160 masked bytes) and rewrite backup (479 masked bytes, masked field __agent_session_input_batch_id); both MATCH. messages-golden-diff named tool unavailable (cmd/agent-backend absent; cmd/jad only, no stub-provider scenario), so fallback used. Reader: no session-side reader of BackupPath; httpapi and app/maintenance read Compacted, CompactedItems, RetainedItems only. Sign-off on golden substitution: given by Agent 1 (delegate) under the user standing instruction. Gate: verify-go.sh --race ./internal/transcript/ ./internal/app/ exit 0 (gofmt, vet, test, race, diffcheck all 0). go test ./internal/transcript/: exit 0. git diff --check: exit 0. |
| P8-M-14 | p8 | low | HOLDS: resized caches the full replacement data URL, bounded by CacheEntries (imageresize/apply.go:187-218) |
| P8-M-15 | p8 | low | HOLDS (duplicate, not counted): png.BestCompression at imageresize/resize.go:183 |

## 4. Unverified items (not batched)

These rows are not batched: the defect was not checked at HEAD in this pass, so they cannot be placed in a reviewable batch yet. Next step: read each cited location and promote it to verified, disputed or stale.

Count: 155 rows. Reason for all: not read at HEAD in this pass.

## 5. Performance rows (perf, needs measurement)

Listed separately, not batched. Each needs a benchmark or profile before any fix.

- N09 (p1): Every sub-agent run builds its own transport when `provider_network` is not tcp
- N17 (p1): Session delete holds the resolver-wide `settingsMu` across sequential process-kill waits
- N24 (p1): Remote transcript reads have no per-token concurrency bound and rescan the file per page
- N30 (p1): `readTurnStarts` holds `turnStartsMu` across a journal read
- N33 (p1): The history block re-reads and re-decodes the whole `messages.backup.jsonl` on every snapshot, keeping only th
- N34 (p1): Every remote transcript request rebuilds the tool and re-indexes the whole transcript; nothing bounds one requ
- N35 (p1): The state-context trim re-renders the whole document after every single removed item
- P2A-18 (p2a): Each `agent.json` of an `agents_dir` is read and parsed twice, so the key allow-list and the values can come f
- P2B1-14 (p2b1): `ConfigError` text carries absolute server paths to API clients through the catalog and session write validato
- P2B2-07 (p2b2): The workspace listing walk takes no `context.Context` and the schema maxima are 200× the defaults, so a listin
- P2B2-09 (p2b2): The session-file listing reads every `<id>.json` in every root in full with no entry or aggregate-byte budget,
- P2B2-14 (p2b2): `configSafeInteger` runs `big.Rat.SetString` on the whole numeric literal, so a long one costs seconds of CPU
- P5M-17 (p5): The two auth packages are the same ~1.2k lines twice, and the copies already diverge — **open by user decision
- P6AM-03 (p6a): Structured content passes every number through `float64`: `1e400` fails the whole call, integers above 2^53 ar
- P6BM-14 (p6b): Every `tool_smith` run re-reads every library header and re-copies every valid function, whether or not the sc
- P6BM-15 (p6b): Each async wait/list/status re-indexes the whole transcript of every live agent handle from offset 0, up to `l
- P6BM-16 (p6b): `ManagedProcessManager.Close` stops running processes one at a time, each up to 2× `kill_grace_ms`
- P6CM-28 (p6c): A cross-profile transcript read lists and parses every manifest before it knows it needs them
- P7A-07 (p7a): Spill throttle map grows per call when a legacy `stdout_full` template puts `{call_id}` in the directory
- P7A-08 (p7a): edit commits over a same-inode in-place change made after its read
- P7A-13 (p7a): fetch page converts the whole stored document to UTF-16 three times per page
- P7A-14 (p7a): fetch `Create` stats every stored file under the store mutex
- P7A-15 (p7a): grep re-resolves every matched file after the search
- P7B-MRG-03 (p7b): Detail range keeps the full text of every step in the range before paging
- P7B-MRG-04 (p7b): Transcript list paging re-scans and re-walks the whole selected range on every page
- P7B-MRG-05 (p7b): List mode materialises a step for every record of the range
- P7B-MRG-06 (p7b): Transcript reads scan the file twice per call (turn index, then walk); the index is rebuilt on every page
- P7B-MRG-19 (p7b): Memory record is pretty-printed twice per create and update
- P7B-MRG-20 (p7b): `pruneCursors` stats every cursor file on each cursor creation
- P7B-MRG-23 (p7b): Task list clones and sorts every match, then keeps `limit`
- P7B-MRG-24 (p7b): Memory quota scan lists every type directory per write; List/GroupBy hold the store lock across the scan
- P7B-MRG-25 (p7b): Hook post-edit check hashes the edited file twice with no size bound
- P7B-MRG-27 (p7b): `request_draft` dedupes object keys with a linear scan (quadratic)
- P7B-MRG-31 (p7b): Transcript `walk.result` decodes, rewrites (data-URL regex) and status-parses every result line in the range,
- P8-M-05 (p8): Image writes have no fsync; dedupe checks size only
- P8-M-06 (p8): Image store mutex covers the first-write directory scan and file I/O (B-C5)
- P8-M-07 (p8): Commit observer runs while the per-turn mutex is held
- P8-M-09 (p8): A completed output item is validated and scanned twice per expansion

## 6. Refuted in the merge (not batched)

- P2B2-23 (p2b2): S (partly refuted)
- P3-18 (p3): S (refuted as a defect)
- P5M-06 (p5): S (refuted as a defect)
- P5M-15 (p5): S (benign)
- P5M-18 (p5): S (documented)
- P5M-20 (p5): S (refuted as a defect)
- P5M-22 (p5): S (no defect)
- P6AM-18 (p6a): S (refuted as a defect)
- P6AM-20 (p6a): R (refuted)
- P7B-MRG-30 (p7b): **confirmed by reading**; a documented-or-fix decision, not a defect

## 7. Stale, disputed and partly stale

- M22: confirmed at cac77d1 (asyncmgr/manager.go:346-360): stale running write after an intervening Complete; the guard does not cover it
- N31: stale; tools/native/transcript.go:146-152 rejects Bytes < 0
- P6BM-19: partly stale; Directory() does not exist; executeNativeExec is test-only (exec_test.go:91)
- P6CM-09: disputed; subagent_manager.go:519-532 checks m.runs[id] before use
- P6CM-18: carried; earlier pass (low_batches.md section 5)
- P6CM-19: done in d4d38f9. SubagentManager.Closed removed; the two app tests were rewritten through the resolver (see the table row for the evidence and mutation results). The 'Directory()' sub-item is refuted (merge)
- P8-M-04: carried; earlier pass (low_batches.md section 5)

- **P7A-07 (moved out of the perf table, HEAD 4da6954):** the spill throttle loop at `tools/native/bash_spill.go:282-290` already drops every key older than `interval` on each call (the comment at 280-281 says so), so the map is bounded by the interval; a `{call_id}` template grows it only inside one interval. Not a performance row. Removed.
- **P8-M-09 (moved out of the perf table, HEAD 4da6954):** the row said the input is validated twice. `transcript/input_history.go:287-289` validates the response once through `objectFieldsValid`, which the comment says skips re-validation of bytes already checked by `objectFields`. The "twice" claim does not hold. Stale.
- **P6AM-03 (moved to the correctness batch, HEAD 4da6954):** an MCP `tools/call` structured-content number outside float64 range or above 2^53 fails the call (`tools/mcp/structured_content_capture.go:114`, `withoutStructuredContent`). This is a correctness defect, not a cost. Track it as a correctness item; trigger: any such call returns an error.

## 8. Planned item: atomic writers (separate review, not a low batch)

Source: N20 (p1_merged.md), the atomic-writer sub-item. Status at HEAD 4aa04e1: verified, deferred by decision (no code change now).

- **Why it is not a low batch.** The writers differ on purpose, so a shared helper is a behaviour change, not a cleanup. Checked: `openaiauth/store.go:224` and `claudeauth/store.go:210` create the temp file with `os.CreateTemp` (mode 0600); `transcript/turn.go:698` and `transcript/maintenance.go:802` take a `sync` or durability flag; `tools/native/mutate.go:335` pins a parent directory; `checkpoint/service.go:356` takes a durability string; `asyncmgr/store.go:46` has no sync (see M33, batch 8); `imagestore/store.go:602` and `tools/native/memory_store.go:761`, `fetch_store.go:301` each have their own rules.
- **Count.** Ten writer functions were read at HEAD (the list above). Twenty-two files call `os.Rename` (`grep -rln os.Rename internal cmd`, non-test). The N20 row says twelve; the count here is the one re-checked.
- **Planned review.** One item per writer family, each with its own test for the durability, mode and directory rules it keeps. Start from the transcript writers, which are golden-adjacent. A shared helper is allowed only after the mode, sync and directory-sync rules are written down per writer.
- **Dependencies.** M33 (batch 8, `asyncmgr/store.go`) is one writer in this set; fix it inside this review or before it, not as a shared-helper change.

- **P5M-17 (moved out of the perf table, maintainability review):** the two auth packages duplicate `writeAtomic` (`backend-go/internal/claudeauth/store.go:211`, `openaiauth/store.go:225`). The copies differ on purpose in mode and sync handling. Review trigger: a diff that changes one copy and not the other, or a third auth package. Not a performance row.
- **P8-M-05 (moved out of the perf table, image-store durability review):** `imagestore/store.go:602-626` (`writeAtomic`) has no `Sync` before rename, and the dedupe at `:481` checks size only. Review trigger: a crash test that loses an image whose write returned success. Belongs with the atomic-writer review in this section.

## 9. Limits

- Only about 40 locations were read at HEAD. The rest is unverified, not checked.
- Line numbers in the merges drift; the evidence column gives HEAD lines where read.
- Name drift: P7A-12's `readFetchResponse` is now `readFetchResponseTruncating`; P8-M-10 names `hashFile`, which exists.
- All 13 merge files were readable. p7a_approval.md and final_perf.md were read for their decisions.

## 10. Performance decisions (Phase B, HEAD 4da6954)

Recorded from the Phase A table and the Phase B run. Medians are `go test -bench -benchmem -count=5` on this machine, load about 1.3 to 1.8. Numbers are HEAD-only unless a before and after is given.

- **P7B-MRG-23 (task list): accepted (cost kept).** The pin test `TestTaskStoreListPinsOrderAndWindow` and `BenchmarkTaskStoreListManyMatches` (commit `2ae8f35`) are kept. `load()` dominates at 18 ms for 3000 tasks (17.9 of 18.3 ms); the filter and sort are about 0.5 ms. A window-only clone change measured 18.28 ms after against 18.34 ms before, so it was not kept. Caching the decoded document would need proof of a single writer per file, which is not established.
- **P7A-14 (fetch Create scan): accepted (no code change).** The directory scan stays under `s.mu`. Lock hold grows with the stored file count: 4.4 ms at 100 docs, 22 ms at 5000 (`BenchmarkPhaseAFetchCreateCount`, scratch, removed). Moving the scan out of the lock would let concurrent creates both pass the quota check and exceed `max_storage_bytes`. An incremental byte total is not exact either: several `FetchStore` instances share one directory, each with its own mutex, and the total is lost on restart.
- **Accepted (cost kept).**
  - **P2A-18** (`agent.json` read and parsed twice in `sessionfs/native_subagent_dir.go:116,120`): the cost is one extra parse per agent folder at load; the two reads can disagree only if the file changes between them, which is the existing behaviour. Accepted: a small one-off cost per folder, and a single read would change the load path.
  - **P2B2-07** (workspace listing walk takes no context; schema maxima 200 times the defaults): the walk is bounded by the configured `max_entries` (code maximum 1,000,000) and `max_depth` (code maximum 64), so the cost is bounded by configuration. Accepted: a context parameter is an API change on the listing path, and the bound is set by the operator.
  - **P2B2-14** (`configSafeInteger` runs `big.Rat.SetString` on the whole literal): the cost is paid once per configured integer at config load, not per request. Accepted: config load is startup-only and the function is small; a length cap would be a new limit to configure.
  - **P5M-17** (the two auth packages duplicate about 1.2k lines): a maintainability cost, not a throughput cost. Accepted: the copies differ on purpose in mode and sync handling; merging is a refactor, not a performance fix (see the atomic-writers review in low_batches.md section 8).
  - **P7A-15** (grep re-resolves every matched file after the search): measured earlier at about 5.4 µs per matched file, bounded by the match cap. Accepted: a few milliseconds at the match cap.
  - **P7B-MRG-20** (`pruneCursors` stats every cursor file on each cursor creation): bounded by `max_cursors`; small. Accepted.
  - **P8-M-06** (image store mutex covers the first-write directory scan and file I/O, B-C5): prior art, already accepted as low in the earlier review. Accepted, not reopened.
  - **N33** (history block re-reads the whole backup on each snapshot): measured 0.82 ms at 100 turns and 8.0 ms at 1000 turns for `readBackupTail` (scratch benchmark, removed). Accepted: the cost is linear in the backup size, the backup is bounded by the compaction policy, and a cache would need the same single-writer proof as P7B-MRG-23.
  - **P7B-MRG-27** (`request_draft` key dedupe is quadratic): measured 41.8 µs for 100 keys and 7.08 ms for 2000 keys (scratch, removed). Accepted: the input is a draft written by the model in one call, and the quadratic cost only shows at thousands of keys.
- **Deferred rows:** each has a specific trigger in the table after this list (HEAD 4da6954). The generic triggers that were here are removed.
- **P6AM-03, P7A-07, P8-M-09, P6BM-16 (sequential Close), P2B1-14, P7A-08 (partly):** stale or not a perf item at HEAD (see Phase A table).



### Trigger table for the 35 performance rows (HEAD 4da6954)

Each trigger below names the code location, the workload or config that exercises it, and the metric and threshold that would justify measuring. Every cited `file:line` was read at HEAD `4da6954`. Nothing here is measured again. A threshold marked "measured" comes from an earlier scratch benchmark; every other threshold is a proposed bound, to be confirmed by the measurement itself before any change.

| Row | Status (section 10) | Cited file:line at HEAD | Trigger | Stale reference fixed |
|---|---|---|---|---|
| N09 | deferred: the cost is per sub-agent run; matters only for sub-agent-heavy use | `app/subagent.go:62`, `app/resolver.go:489-493` (`newProviderStreamer`) | A sub-agent run with `provider_network` not `tcp`: measure when 20 or more such runs in one session spend more than 5% of their wall time in transport setup (dial and TLS). | `resolver.go:481-515` in the findings -> `resolver.go:489-493` |
| N17 | deferred: the lock is held across process-kill waits; needs a concurrency test that shows a stall | `app/session_catalog_files.go:74-77` (`DeleteSessionConfig` takes `settingsMu`, then `sessionfs.DeleteSessionFile`) | A session delete while the session has a running process: measure when a concurrent settings call (for example `app/session_settings.go:23`) waits longer than the summed `kill_grace_ms` of the processes the delete stops. | `session_catalog_files.go:47` (a different function) -> `:74-77` |
| N24 | deferred: no concurrent remote transcript reads seen yet | `app/remote_transcripts.go:88` (`Transcript`), `tools/native/transcript.go:306` (`indexTranscriptTurns`) | Eight or more concurrent remote transcript requests on a transcript near `tools.native.transcript.scan_max_bytes`: measure when aggregate bytes read per second exceed 256 MiB (proposed bound). | `transcript.go:296-306` -> `:306`; `remoteapi/server.go:647` was a closing brace, dropped |
| N30 | deferred: lock contention on concurrent turn starts not measured | `app/turn_starts.go:41` (lock), `:56` (`ReadCompleteRecordsRange` under the lock) | Four or more concurrent turn starts while the transcript has grown by 1000 or more records since the last scan: measure when p99 wait on `turnStartsMu` exceeds 10 ms. | none (`turn_starts.go:41-92` range kept, function is `:32-109`) |
| N33 | accepted (section 10): linear in the backup size; the backup is bounded by the compaction policy; a cache would need the single-writer proof of P7B-MRG-23 | `app/history.go:242` (`readBackupTail`), `app/catalog.go:398` (caller) | Catalog snapshots of a session with `BackupTurns` at 1000 turns or more: measure when `readBackupTail` takes more than 10 ms per snapshot (8.0 ms measured at 1000 turns). | `app/history.go:222-240` added for `readHistoryBlock` |
| N34 | deferred: tool rebuild per call matters only when remote transcript latency is a problem | `app/remote_transcripts.go:104-106` (tool built per call), `tools/native/transcript.go:306` (index per call) | Ten or more remote transcript requests per second on a transcript of 10,000 lines or more: measure when tool construction plus indexing exceeds 5 ms per request. | `transcript.go:296-306` -> `:306`; `remote_transcripts.go:88-122` -> `:104-106` |
| N35 | deferred: the re-render cost matters only near `max_bytes` | `app/state_context.go:207` (trim loop), `:234` (re-render) | A state context whose rendered size is above `statecontext.triggers.<name>.max_bytes` by 100 or more trimmed items: measure when the loop runs more than 100 iterations or one build takes over 10 ms. | none |
| P2A-18 | accepted (section 10): one extra parse per agent folder at load; a single read would change the load path | `sessionfs/native_subagent_dir.go:116` (raw read), `:120` (`readSessionObject`, second read and parse) | An `agents_dir` with 100 or more agent folders at load: measure when `loadSubagentFolder` over all folders exceeds 50 ms. | `native_subagent_dir.go:105-140` -> `sessionfs/native_subagent_dir.go:116,120` (`:105` is a comment) |
| P2B2-07 | accepted (section 10): the walk is bounded by configured `max_entries` and `max_depth`; a context parameter is an API change; the operator sets the bound | `sessionfs/files.go:22` (`CollectSessionFiles`), `sessionfs/file_settings.go:13-14` (caps 64 and 1,000,000), `:49-50` (defaults 12 and 5000) | A workspace listing with `max_entries` raised above 100,000 and the walk ignoring the request context: measure when a listing keeps running more than 1 s after the client disconnects. | none |
| P2B2-09 | deferred: decided as defer (user decision) | `sessionfs/session_files.go:59` (`listSessionFiles`), `:79` (budget check), `app/catalog_files.go:41` (`ListCatalogFiles`) | A profiles catalog with 1000 or more session files: measure when the listing takes over 500 ms or hits the 64 MiB budget (`truncated` true). | `session_files.go:25-57` -> `:59-95`; `app/catalog_files.go:43` -> `:41` |
| P2B2-14 | accepted (section 10): paid once per configured integer at config load; a length cap would be a new limit | `sessionfs/tool_settings.go:195-213` (`configSafeInteger`), `:204` (`big.Rat.SetString`) | A config file with one integer literal of 10,000 digits or more: measure when config load takes over 100 ms. | `tool_settings.go:190-208` -> `:195-213` |
| P6BM-14 | deferred: capped by `list_max_files` and `function_max_bytes`; matters only at the cap | `tools/native/tool_smith_fn.go:27` (`stageToolSmithFunctions`), `:79` (`copyToolSmithFunction`); cap `sessionfs/native_exec_settings.go:102` (`ListMaxFiles`), `:104` (`FunctionMaxBytes`) | A `tool_smith` run with a library at `list_max_files` (200) files, each near `function_max_bytes` (64 KiB): measure when staging takes over 200 ms per run. | `:77` was a comment -> `:79` |
| P6BM-15 | deferred: matters only with a live handle that has a long transcript on the async path | `tools/native/async_tool.go:194` (`liveHandleIDs` then `latestBlocks`), `async_latest.go:41` (`latestBlocks`), `:112` (`liveHandleIDs`), `asyncmgr/config.go:72` (`LatestScanMaxBytes` 8 MiB) | Twenty or more live handles each with a transcript over 8 MiB, one async wait per second: measure when bytes scanned per call exceed 160 MiB (20 x 8 MiB). | `async_latest.go:213,249` and `subagent_manager.go:152-168` (not this path) -> `:41-70` and `:112-123` |
| P6CM-28 | deferred: matters only with many sub-agent manifests in one profile | `tools/native/subagent_store.go:228` (`subagentRunIDs`), `:233` (`load` of every manifest), `transcript_path.go:155` (cross-profile call) | A profile with 500 or more sub-agent manifests and a cross-profile transcript read: measure when `subagentRunIDs` takes over 50 ms before the target is known. | `subagent_manager.go:100-133`, `subagent_store.go:120-141` -> `subagent_store.go:228-240` |
| P7A-13 | deferred: deferred; the fix would keep the UTF-16 length in the stored metadata | `tools/native/fetch_store.go:158` (`fetchWindow`), `:188` (`utf16Length` of the stored markdown on each page) | A fetch page of a stored document over 8 MiB: measure when one page read takes over 10 ms (22.6 ms measured at 8 MiB). | `ripgrep.go:353-417` (unrelated) -> `fetch_store.go:158,188` |
| P7A-14 | accepted (section 10): the scan stays under `s.mu`: moving it out would let concurrent creates exceed `max_storage_bytes` | `tools/native/fetch_store.go:91` (lock), `:116` (`fetchDirectoryBytes` under the lock) | 5000 or more stored fetch documents: measure when the `Create` lock hold exceeds 20 ms (22 ms measured at 5000). | none |
| P7A-15 | accepted (section 10): a few milliseconds at the match cap | `tools/native/grep.go:123-131` (resolve loop per matched group) | A grep at the match cap across 1000 or more files: measure when the resolve loop takes over 50 ms per call. | none |
| P7B-MRG-03 | deferred (user decision): deferred by user decision (240 MB per 400-turn page measured) | `tools/native/transcript_detail.go:101` (`detail`), `transcript_steps.go:167-180` (`result` keeps full text) | Detail over a 400-turn range with 64 KiB results: measure when one call's peak heap exceeds 100 MiB (240 MB per 400-turn page measured). | `transcript_steps.go:60-62` -> `:60` (keep predicate), text retention at `:167-180` |
| P7B-MRG-04 | deferred (user decision): deferred by user decision (flat 5.2-5.3 MB per page measured) | `tools/native/transcript.go:306` (index rebuilt per page), `:368` (`scanTranscript` over the range) | A list page over a transcript of 400 turns: measure when bytes scanned per page exceed 2x the page output bytes (5.2 MB flat per page measured). | `transcript.go:332` (a `hasResume` assignment) -> `:306,368` |
| P7B-MRG-05 | deferred (user decision): deferred by user decision (0.73 to 2.10 MB for 40 to 400 turns measured) | `tools/native/transcript_steps.go:201` (`visit` materialises each record), `transcript.go:368` | List mode over a 400-turn range: measure when materialised steps exceed 10x the steps emitted in the page. | `transcript.go:363-390` -> `:368` and `transcript_steps.go:201` |
| P7B-MRG-06 | deferred: part of the P7B-MRG-04 measurement | `tools/native/transcript.go:306` (turn index), `:368` (second walk), `transcript_scan.go:95-97` (`indexTranscriptTurns`) | Transcript reads of 400 turns or more: measure when the index build takes over 10% of total read time. | `transcript.go:304` (an error return) -> `:306` |
| P7B-MRG-19 | deferred: deferred; memory writes not seen in a profile | `tools/native/memory_store.go:83` (compact), `:176` (`CreateValue` pretty), `:300` (update pretty), `memory_json.go:317-323` (`memoryJSONPretty`) | A memory create or update of a record at the policy's max bytes: measure when the pretty-print step takes over 1 ms per write. | `memory_store.go:513-525` (not the encoder) -> `:176,:300` |
| P7B-MRG-20 | accepted (section 10): bounded by `max_cursors`; small | `tools/native/memory_store.go:847` (`pruneCursors`), `:863` (`os.Stat` per cursor) | `max_cursors` raised to 1000 or more: measure when one cursor creation takes over 10 ms in `pruneCursors`. | `memory_store.go:793-829` (`load`, not the prune) -> `:847-883` |
| P7B-MRG-23 | accepted (section 10): the load dominates (17.9 of 18.3 ms at 3000 tasks); a window-only change did not help | `tools/native/task_store.go:249` (`List`, load then clone and sort of all matches) | 10,000 or more tasks with a small `limit`: measure when `List` takes over 50 ms (18 ms measured at 3000). | `task.go:250-265` (argument parsing) -> `task_store.go:249` |
| P7B-MRG-24 | deferred: lock wait across the scan matters only under concurrent writes | `tools/native/memory_group.go:28` (`GroupBy` under `withLock`), `memory_store.go:665` (`totals`), `:613` (mkdir per write) | A `GroupBy` over `MaxScanRecords` records with concurrent memory writes: measure when a writer waits over 10 ms for the store lock. | `memory_store.go:609-655` -> `:613` and `:665` |
| P7B-MRG-25 | deferred: deferred; matters only for a hook that watches a large file | `tools/native/hooks.go:123` and `:158` (two `fileDigest` calls), `:233` (`fileDigest`) | A post-edit hook watching a file of 1 MiB or more: measure when digesting takes over 10 ms per edit (5.45 ms measured at 8 MiB). | none |
| P7B-MRG-27 | accepted (section 10): the input is one model-written draft; quadratic cost only at thousands of keys | `tools/native/request_draft.go:427-433` (linear key scan) | A `request_draft` object with 2000 or more keys: measure when decoding takes over 10 ms (7.08 ms measured at 2000). | `request_draft.go:425-432` -> `:427-433` |
| P7B-MRG-31 | deferred (user decision): deferred by user decision (about 240 MB of 294.7 MB at 400 turns measured) | `tools/native/transcript_steps.go:167-168` (`replaceDataURLs(transcriptText)`), `transcript_render.go:9` (data-URL regex), `transcript_steps.go:186` (`transcriptResultStatus`) | A list over a 400-turn transcript with 64 KiB results: measure when one `result()` call takes over 1 s of CPU in total. | none (added the missing location) |
| P8-M-06 | accepted (section 10): prior art, accepted as low earlier (B-C5); not reopened | `imagestore/store.go:479` (lock), `:484-487` (first-write `directorySize`), `:491` (`writeAtomic`) | Image writes into a directory of 5000 or more files: measure when a writer waits over 20 ms on `s.mu`. | `app/bound_runner.go:51` (a brace) -> `:479,:484-487` |
| P8-M-07 | deferred: the observer runs under the turn lock; matters only if an observer is slow | `transcript/turn.go:343` (lock), `:405` (observer call under the lock), `:424` (`callCommitObserver`), `session/runner_response.go:122` (`CommitWithObserver`) | A commit observer that does I/O on a turn: measure when a concurrent turn method waits over 10 ms for `t.mu`. | none |

Rows P7B-MRG-03, P7B-MRG-04, P7B-MRG-05 and P7B-MRG-31 were deferred by user decision before this table. The table is their trigger; the decision is unchanged.

## 11. Closeout of the remaining items (HEAD b7bff35 onward)

- **Windows build (user-excluded).** `GOOS=windows go build ./internal/... ./cmd/...` fails at `internal/tools/native/fetch.go:358-359` (`htmltomarkdown.Convert`, `ConversionOptions`, `Ptr` undefined). The same packages fail with `CGO_ENABLED=0` on Linux, so the cause is the FFI library, not the target OS. Excluded by the user for this round; not a permanent failure. Reopen on a host with the library.
- **Product binary (user-excluded).** `go build ./cmd/jad/` exits 0 in this environment. `cmd/agent-backend` is not in the tree, so the golden skill's build step has no target.
- **Golden scenario byte diff: exception (not closed by a server diff).** No server-level byte diff was run. The skill's step 2 builds `cmd/agent-backend`, which is not a Go target in this tree; the only build that could host it is the product binary, which the user excluded. Substitute, verified at HEAD: `TestRunnerNativeWriteEditMessagesJSONLMatchesNodeGolden`, `TestRunnerNativeRequestDraftMessagesJSONLMatchesNodeGolden` and `TestRunnerStreamsAndPersistsGoldenResponse` compare the runner's `messages.jsonl` output byte for byte against embedded Node goldens. They pass (`go test ./internal/session/ -run 'Golden|MessagesJSONL' -count=1 -v`, exit 0, 3 PASS). Exception wording for the record: "Golden byte diff at server level not run: the server build is part of the excluded product binary. Runner-level golden tests pass at HEAD." Reopen if a server binary is added to the tree.
- **P2B2-19 (decision kept).** An attempt to refuse `1e3` on the leaf nodes was committed as `1d460dd` and reverted as `7dcc35d`. The attempt ignored the decision in row 139 (exponent notation is accepted by `configSafeInteger`, which the schema admits). The reverted code is not in HEAD. Tool limits still refuse exponents (`plainToolInteger`, pinned by `TestToolLimitsRefuseExponentNotation`), so the divergence described in row 139 remains a documented decision.
- **Begin gate (test gap).** Searched `backend-go/internal` (`*_test.go` and `*.go`) and `audit_findings/` for "begin gate", "begin-gate", "begingate": no match. Closed as not found.
- **Runner helpers, direct tests (accepted).** Coverage at `b7bff35`: `Run` 81.8%, `persistRunInput` 87.5%, `buildRoundRequest` 82.4%, `compactAfterResponse` 86.7%, `enforceToolLimits` 73.3%, `executeToolCalls` 85.7%. The uncovered `enforceToolLimits` lines (`runner.go:608-612`, `621-625`) are the store and refresh error returns from `appendToolOutputs` and `refreshAfterCompaction`, which need store failure injection. Accepted; the limit behaviour itself is covered by `run_token_limit_test.go` and `runner_tools_test.go:312`.
- **-cpu / -count split (accepted).** `TestManyConcurrentSubscribersSeeAnUninterruptedStream` passed 100 of 100 with `-count=100 -v` under load. A split re-run adds no evidence. Closed.
- **Named return on `Runner.Run` (accepted).** Nothing defers on `err` or `result` in that range; the named return is used by the `return result, err` paths. Rewriting it would be churn with no behaviour change.
- **Root working docs (decided this round).** `audit_findings/p2b2_merged.md` and `audit_plan.md` committed as docs (P2B2-08 stale verdict checked against `read.go:426` and `tool_settings.go:168`; audit plan section 6.6 backs `final_perf.md`; the R1 profile `jad/reviewer0` matches the reviewer runtime state). `audit_findings/p4_r4.md` was an empty placeholder skeleton in the working tree; restored to the committed review. `EXECUTOR.md` and `MASTER.md` are orchestration role prompts, not audit records, and are excluded and left modified.
- **Performance rows (decision, per row).** The section 5 table has 38 rows, not 21. Nothing was measured. Per row: **closed** where code shows the cost is already fixed or not a performance item; **accepted** where section 10 records the cost and its reason; **deferred** where the trigger table (after section 10) gives a specific trigger. **Closed (stale at HEAD):** P6BM-16 (`ProcessManager` `Close` stops in parallel with a finishing wait, `process_manager.go:571-622`). **Closed (not a performance item):** P2B1-14 (absolute paths in `ConfigError` are a disclosure hardening item, tracked as a finding), P7A-08 (a same-inode write race in edit, unreproduced, same-user actor, ms window; accepted as a correctness risk, not a cost). **Accepted and deferred:** the other 35 rows have a section 10 entry with its reason and trigger. Each deferred row has its own trigger in the table; no trigger is a blanket trigger.

## 12. Final classification (HEAD 9c521b3)

- **Gate:** `verify-go.sh` at 9c521b3: gofmt, vet, test and diffcheck all exit 0. `go test ./internal/sessionfs/ ./internal/session/`: both ok.
- **Committed as records (`29cb1ed`):** the phase 7a, 7b and 8 review, merge, measurement and approval files, `m16_design_r4.md`, `perf_baseline_632ebed.md`, `low_unverified_verdicts.md` (cited by `backend-go/DECISIONS.md:1194`).
- **Committed as docs (`9dbc728`):** `audit_plan.md` (section 6.6 backs `final_perf.md`; R1 `jad/reviewer0` matches the runtime log), `audit_findings/p2b2_merged.md` (P2B2-08 stale verdict).
- **Excluded, not audit records:** `REVIEWER.md`, `REVIEWER4.md` (reviewer operating prompts; no record cites them). `EXECUTOR.md`, `MASTER.md` (orchestration role prompts; modified in the tree, uncommitted, not audit records).
- **Restored:** `audit_findings/p4_r4.md` was an empty placeholder skeleton in the working tree; reverted to the committed review.
- **Open after this round:** Windows build (user-excluded), product binary (user-excluded). Nothing else.
