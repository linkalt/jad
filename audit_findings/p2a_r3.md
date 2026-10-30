# Phase p2a (sessionfs native_* tool settings + layouttest) — reviewer r3
- HEAD: fbe49a89d14f2aa6998a1561afb1a85decde0faa
- date (UTC): 2026-10-28
- files read: backend-go/internal/sessionfs/native_*.go (24 non-test files, 4845 lines), backend-go/internal/sessionfs/layouttest/layouttest.go (355 lines), backend-go/internal/sessionfs/tool_settings.go, backend-go/internal/sessionfs/tool_shape.go, backend-go/internal/sessionfs/removed_keys.go, backend-go/internal/sessionfs/file_settings.go (normalizeNativePathPolicy/decodeConfigObject), backend-go/internal/sessionfs/session.go (140-180, 240-520, 1220-1320 helpers), backend-go/internal/sessionfs/roots.go (resolveFrom); callers/consumers traced read-only: backend-go/internal/asyncmgr/config.go, backend-go/internal/tools/native/fetch.go (allowed_hosts enforcement, containsHost, isPublicFetchAddress), backend-go/internal/tools/native/read_image.go, backend-go/internal/tools/native/request_draft.go; tests read as evidence only; DECISIONS.md entries (subagent, exec, fetch, checkpoint null-rule, approval removal); prior audits audit_backend.md (F-22), audit_performance_plan.md, audit_audit.md, audit_design*.md (skimmed for open ids + fixed items); skill .jad/prompts/skills/feature-development/SKILL.md
- commands: `go vet ./internal/sessionfs/` exit 0; `gofmt -l internal/sessionfs/` exit 0 (no output); `go build ./internal/sessionfs/` exit 0 (all from backend-go/); baseline suite not rerun per prompt
- scratch: /tmp/r3/ (removed at end)

## 1 Understanding note
p2a is the validation vocabulary every later phase trusts: 24 `native_*` non-test files under `internal/sessionfs` plus `layouttest` (355 lines). Each `tools.native.<tool>` section has one `normalizeNative*Settings` entry point called from `loadSessionSettingsFrom` (session.go:377-501) after `validateToolSettingsShape` canonicalizes `tools.native: null` to `{}`. Shared helpers: `decodeConfigObject`, `orderedObjectKeys`, `configuredInteger`/`configSafeInteger` (tool_settings.go, session.go), `normalizeNativePathPolicy` (file_settings.go), `unsupportedToolKey` with edit-distance suggestion (tool_shape.go), `rejectRemovedApproval` (removed_keys.go), `bashBool`/`bashString`/`nullableField` (bash/exec/tool_smith). `nativeBashStyleKeys` is the shared child-process key list reused by bash/exec/tool_smith via `normalizeBashStyleFields`; process duplicates it. Storage dirs (fetch/memory/process/subagent/task) each resolve `~`/relative against cwd or `toolStorageDir` then `Abs`+`Clean`. `layouttest.Write` builds a catalog-layout fixture (root setting + models/profiles/tools/mcp + session) and auto-adds `async: {}` when native enables process/subagent/tool_smith. Never independently reviewed with priority: `native_subagent_settings.go`, `native_profile_message_settings.go`, `native_transcript(s)_settings.go` (be4af1d..a21104b, a57403b, 2828a68..010e616), `native_async_settings.go` (86552a4). Known-risk: validation of every new key (bounds, defaults, unknown keys), 21 `Native*Settings` types (F-22, closed no-fix), duplicated validation patterns.

## 2 Hypotheses (assumed -> verified/disproved)
- H01 process/task `statecontext` silently ignores unknown keys while subagent rejects them — verified by read, filing.
- H02 memory `statecontext` selector silently ignores unknown keys — verified by read, filing.
- H03 `statecontext: null` means defaults for subagent but an error for process/task/memory — verified by read (tests codify it), filing as inconsistency.
- H04 fetch `allowed_hosts`/`denied_hosts` pattern rejects IPv6 literals the fetch tool otherwise handles — verified by read, filing.
- H05 `read_image` has its own `paths` key and policy while its comment says it uses the read policy — verified by read, filing.
- H06 `request_draft` checks `timeout_ms <= max_timeout_ms` but not `headless_auto_accept_timeout_ms <= max_timeout_ms` — verified by read, filing.
- H07 async `retention_days`/`unread_ttl_days` 10M-day overflow (p4 M23) still present — verified by read, re-confirming.
- H08 five `storage_dir` resolvers are byte-identical logic — verified by read, filing as duplication.
- H09 process env handling duplicates bash helper and trims `env_deny` differently — verified by read, filing.
- H10 unknown-key errors use `unsupportedToolKey` (with suggestion) in 8 tools but plain `configError` in 7 — verified by read, filing.
- H11 `layouttest.withAsync` auto-adds `async` and hides the `needs async` error path — verified by read, filing.
- H12 bash spill `storage_dir` vs deprecated templates exclusivity compares raw JSON including quotes — verified by read, kept as hardening note, not filed.
- H13 exec runner alias collision is case-sensitive but names are lowercase-only — disproved (pattern forces lowercase, `default_lang` lowercases input; no bypass).
- H14 spill readability check without `EvalSymlinks` is a TOCTOU — kept as cross-phase lead (path confinement is phase 7a), not filed.

## 3 Findings
### P2A-R3-01 process/task statecontext silently ignores unknown keys
- severity: medium
- lens: A correctness
- location: backend-go/internal/sessionfs/native_process_settings.go:272, backend-go/internal/sessionfs/native_task_settings.go:172
- evidence: [read] `normalizeNativeProcessStateContext` decodes `tools.native.process.statecontext` then reads only `states`/`limit`/`mode`/`detail_states` with no unknown-key loop; same for `normalizeNativeTaskStateContext` (`statuses`/`limit`/`mode`/`detail_statuses`). Concrete: `{"tools":{"native":{"process":{"statecontext":{"limitt":5}}}}}` decodes, `fields["limit"]` is absent so `configuredInteger` returns default 20, `limitt` is never mentioned and the load succeeds. Contrast `normalizeNativeSubagentStateContext` at native_subagent_settings.go:380 which loops and returns `unsupportedToolKey(... ["states","limit","mode","detail_states"] ...)` with a `did you mean` suggestion (covered by `TestNormalizeNativeSubagentStateContextRejectsBadValues` "unknown key"). No test asserts an error for an unknown process/task statecontext key (grep for `statecontext.*not supported` in `*_test.go` is empty; process/task invalid lists cover only bad values, not unknown keys).
- suggested fix: add the same unknown-key loop to both functions (copy the subagent four-key check). Files: backend-go/internal/sessionfs/native_process_settings.go, backend-go/internal/sessionfs/native_task_settings.go
- effort: S
- prior art: new

### P2A-R3-02 memory statecontext selector silently ignores unknown keys
- severity: medium
- lens: A correctness
- location: backend-go/internal/sessionfs/native_memory_settings.go:223
- evidence: [read] `normalizeNativeMemoryStateContext` decodes each selector (`type`/`filter`/`exclude_any`/`group_by`/`limit`) and never checks for extra keys. Concrete: `{"tools":{"native":{"memory":{"statecontext":[{"type":"task","limitt":5}]}}}}` succeeds with `Limit` defaulting to `min(10, max_list_limit)`; `limitt` is dropped. The invalid-policy test (`TestNormalizeNativeMemorySettingsRejectsInvalidPolicies`) covers bad `group_by`/limits/`filter` depth but no unknown selector key. Same class as P2A-R3-01, different function.
- suggested fix: loop over selector `fields` and reject anything outside `type/filter/exclude_any/group_by/limit` via `unsupportedToolKey`. File: backend-go/internal/sessionfs/native_memory_settings.go
- effort: S
- prior art: new

### P2A-R3-03 fetch host allow/deny lists cannot name IPv6 literals
- severity: medium
- lens: B security
- location: backend-go/internal/sessionfs/native_fetch_settings.go:41, backend-go/internal/sessionfs/native_fetch_settings.go:178
- evidence: [read] `nativeFetchHostPattern = ^[a-z0-9](?:[a-z0-9.-]{0,251}[a-z0-9])?$` applied after `ToLower` with no trim; any `:` fails. Concrete: `{"tools":{"native":{"fetch":{"allowed_hosts":["::1"]}}}}` and `{"denied_hosts":["2001:db8::1"]}` both fail with `must contain host names or IP addresses`, while the consumer handles IPv6: `fetch.go:390-430` takes `parsed.Hostname()` (`::1` without brackets), `containsHost` (fetch.go:460, `EqualFold`) would match it, and `isPublicFetchAddress` (fetch.go:466) explicitly classifies IPv6 (`::1`/`fd00::1`/`fe80::1` false, `2606:4700:4700::1111` true, covered by fetch_test.go:220-224). Reachable by: model (chooses the URL host) vs operator (writes the allow/deny lists). An operator cannot deny a specific public IPv6 (`denied_hosts` cannot name it) and cannot pin `allowed_hosts` to an IPv6-only set; with `allowed_hosts` empty the tool allows any public host. `TestNormalizeNativeFetchSettings` rejects `*.example.com` and `" bad.example"` but has no IPv6 case.
- suggested fix: accept IPv6 literals (and bracketed forms, normalized via `net.ParseIP` after stripping brackets) in `nativeFetchHosts`, or document that host lists are DNS/IPv4-only and IPv6 is governed solely by `isPublicFetchAddress`. File: backend-go/internal/sessionfs/native_fetch_settings.go
- effort: S
- prior art: new

### P2A-R3-04 read_image policy comment contradicts the code
- severity: medium
- lens: B security
- location: backend-go/internal/sessionfs/native_read_image_settings.go:10, backend-go/internal/sessionfs/native_read_image_settings.go:12
- evidence: [read] Type comment: `The path policy is not a key of this tool: it is the read tool's policy, so one setting governs both.` Code: `nativeReadImageKeys = ["max_image_bytes","paths"]`, `normalizeNativeReadImageSettings(setting, globalJadRoot, settingPath)` builds its own `Policy` from `fields["paths"]` and never takes the read policy, while `normalizeNativeTranscriptSettings(setting, settingPath, readPolicy)` does take `readPolicy` and has no `paths` key. Loader (session.go:401) passes no read policy to read_image. Concrete divergence: `{"tools":{"native":{"read":{"paths":{"allow":["/safe"]}},"read_image":{}}}}` restricts `read` to `/safe` but leaves `read_image.Policy` empty (its own default), so the model reads an image outside `/safe` via `read_image` (`readImageFile(path, settings.Policy, ...)` at tools/native/read_image.go:68). The operator reading the comment expects one setting to govern both.
- suggested fix: either make read_image share the read policy (take `readPolicy`, drop its `paths` key) or correct the comment and document that read_image has its own policy. Files: backend-go/internal/sessionfs/native_read_image_settings.go, backend-go/internal/sessionfs/session.go
- effort: S
- prior art: new

### P2A-R3-05 async retention TTLs still overflow time.Duration (re-confirmed p4 M23)
- severity: low
- lens: A correctness
- location: backend-go/internal/sessionfs/native_async_settings.go:10, backend-go/internal/asyncmgr/config.go:97, backend-go/internal/asyncmgr/prune.go:84
- evidence: [read] Loader caps `retention_days`/`unread_ttl_days` at `maxNativeAsyncValue` 10,000,000 and `Config.Validate` rejects only negatives, so 10,000,000 passes validation; `prune.go:84,114,138` computes `time.Duration(days) * 24 * time.Hour` which overflows past ~106,751 days (negative duration = cutoff in the future = every signal/record looks expired). p4 M23 measured that 10,000,000 drops everything at once; `git log --oneline -- native_async_settings.go asyncmgr/config.go` since p4 shows only `86552a4` (latest_* keys), no bound change, so the finding is unchanged in this tree.
- suggested fix: cap `retention_days`/`unread_ttl_days` at ~100,000 days in the loader (or reject >106751 in `Validate`) and document 0 as the "never" value. Files: backend-go/internal/sessionfs/native_async_settings.go, backend-go/internal/asyncmgr/config.go
- effort: S
- prior art: re-confirmed p4 M23 (still open, not fixed)

### P2A-R3-06 request_draft headless timeout is not bounded by max_timeout_ms
- severity: low
- lens: A correctness
- location: backend-go/internal/sessionfs/native_request_draft_settings.go:78
- evidence: [read] Loader checks `if TimeoutMS > MaxTimeoutMS { error }` but never compares `HeadlessAutoAcceptTimeoutMS` to `MaxTimeoutMS`. Concrete: `{"tools":{"native":{"request_draft":{"timeout_ms":1000,"headless_auto_accept_timeout_ms":1800000,"max_timeout_ms":1000}}}}` loads (all three within 1..1800000) although the headless path (`autoAcceptRequestDraft(ctx, policy.HeadlessAutoAcceptTimeoutMS)` at tools/native/request_draft.go:99) waits 1800x the advertised max. Max defaults to 1800000 so the default tree is unaffected; the gap needs an explicit small `max_timeout_ms`.
- suggested fix: add `if HeadlessAutoAcceptTimeoutMS > MaxTimeoutMS { error }` beside the existing check. File: backend-go/internal/sessionfs/native_request_draft_settings.go
- effort: S
- prior art: new

### P2A-R3-07 five byte-identical storage_dir resolvers
- severity: low
- lens: D maintainability
- location: backend-go/internal/sessionfs/native_fetch_settings.go:213, backend-go/internal/sessionfs/native_memory_settings.go:200, backend-go/internal/sessionfs/native_process_settings.go:249, backend-go/internal/sessionfs/native_subagent_settings.go:416, backend-go/internal/sessionfs/native_task_settings.go:149
- evidence: [read] Each `normalizeNative*StorageDir` is the same 15 lines: default name, `Unmarshal` + empty check with a section-named error, `TrimSpace`, `expandHome`, `Join(cwd|toolStorageDir)` when relative, `Abs`, `Clean`. Only the default (`fetch`/`memory`/`process`/`subagent`/`task` dir name) and the error prefix differ. A sixth variant (bash spill `storage_dir` at native_bash_settings.go:411 plus `stdout_full`/`stderr_full` templates) shares the `expandHome`/`Join`/`Clean` shape. `grep -n StorageDir native_*.go` lists exactly these five; no helper exists. Behavior is correct; the risk is drift (one copy gains a confinement or error-message fix the others miss).
- suggested fix: one `normalizeToolStorageDir(fields, key, defaultDir, cwd, toolStorageDir, settingPath, section)` helper; no behavior change. Files: the five listed plus call sites
- effort: M
- prior art: F-22 notes sessionfs size/duplication with no small fix; this is the concrete instance inside p2a (5x ~15 lines)

### P2A-R3-08 process env handling duplicates the bash helper with a trim divergence
- severity: low
- lens: D maintainability
- location: backend-go/internal/sessionfs/native_bash_settings.go:263, backend-go/internal/sessionfs/native_process_settings.go:202
- evidence: [read] `normalizeChildEnvironment` (bash/exec/tool_smith via `normalizeBashStyleFields`) and the inline process block both seed `{"NO_COLOR":"1","GIT_TERMINAL_PROMPT":"0"}`, overlay `fileEnv`, then `env`, then `env_deny` against `bashEnvNamePattern`. Divergence: bash `env_deny` does `name = TrimSpace(name)` before the pattern check (native_bash_settings.go:299), process does `if err := Unmarshal(rawName,&name); err != nil || !pattern.Match(name)` with no trim (native_process_settings.go:225). Concrete: `{"env_deny":[" FOO"]}` loads for bash/tool_smith/exec (trimmed to `FOO`) but fails for process (`must be an environment variable name`). `env` keys in both reject without trim, so the asymmetry is `env_deny`-only. Tests cover `{"env_deny":["BAD-NAME"]}` for process and `BAD-NAME` for bash but no whitespace case.
- suggested fix: make process call `normalizeChildEnvironment` (or at least trim `env_deny` the same way). Files: backend-go/internal/sessionfs/native_process_settings.go
- effort: S
- prior art: new

### P2A-R3-09 unknown-key errors disagree on suggestions
- severity: low
- lens: D maintainability
- location: backend-go/internal/sessionfs/native_bash_settings.go:141, backend-go/internal/sessionfs/native_fetch_settings.go:96, backend-go/internal/sessionfs/native_glob_settings.go:69, backend-go/internal/sessionfs/native_grep_settings.go:75, backend-go/internal/sessionfs/native_memory_settings.go:116, backend-go/internal/sessionfs/native_task_settings.go:100, backend-go/internal/sessionfs/native_request_draft_settings.go:63
- evidence: [read] Eight sections use `unsupportedToolKey` (edit-distance `did you mean` + sorted candidate list): async, exec, profile_message, read_image, skill, subagent, tool_smith, transcript(s). Seven use plain `configError("%s...%s is not supported")` with no suggestion: bash top-level + `stdout`/`spill` sub-objects, fetch, glob, grep, memory, mutation (write/edit), process, task, request_draft. Concrete: `{"tools":{"native":{"bash":{"timeouut_ms":1}}}}` answers `tools.native.bash.timeouut_ms is not supported` while `{"tools":{"native":{"exec":{"timeouut_ms":1}}}}` answers `did you mean timeout_ms?`. `grep -n unsupportedToolKey native_*.go` vs `grep -n "is not supported" native_*.go` shows the split.
- suggested fix: route the seven plain sites through `unsupportedToolKey` with their existing key lists. Files: the seven listed
- effort: S
- prior art: new

### P2A-R3-10 statecontext null means defaults for subagent but an error for process/task/memory
- severity: low
- lens: D maintainability
- location: backend-go/internal/sessionfs/native_subagent_settings.go:370, backend-go/internal/sessionfs/native_process_settings.go:272, backend-go/internal/sessionfs/native_task_settings.go:172, backend-go/internal/sessionfs/native_memory_settings.go:223
- evidence: [read] `normalizeNativeSubagentStateContext`: `if len(raw)==0 || TrimSpace=="null" { return defaults }`, and `{"states":null,"limit":null}` falls back per-field (covered by `TestNormalizeNativeSubagentStateContextReadsStatesAndLimit` "null"/"null states"). `normalizeNativeProcessStateContext`/`normalizeNativeTaskStateContext`: `if len(raw)==0 { return defaults }` only, so `"statecontext":null` hits `decodeConfigObject(null)` and fails (covered by `{"statecontext":null}` in both invalid lists); memory `"statecontext":null` fails via `Unmarshal(null)` + `==nil` check (in its invalid list). Same key name, opposite null semantics, both codified by tests. Operator impact is a confusing error (explicit null is arguably "absent"), not a bypass.
- suggested fix: pick one rule (absent==null==defaults, matching subagent and `normalizeNativeStateContextMode`) and apply to all four; update the three invalid lists. Files: the four listed plus their `*_test.go`
- effort: S
- prior art: new

### P2A-R3-11 layouttest auto-adds async and hides the needs-async error
- severity: low
- lens: D maintainability
- location: backend-go/internal/sessionfs/layouttest/layouttest.go:339, backend-go/internal/sessionfs/session.go:501
- evidence: [read] `withAsync` adds `"async":{}` whenever native enables process/subagent/tool_smith, and both `Write` and `AddSession` apply it, so no layouttest fixture can express `{"native":{"process":{}}}` without async. The loader error `tools.native.<name> needs tools.native.async to collect, list and stop its handles` (session.go:501) is therefore unreachable through the shared fixture; it is covered only by direct `normalize*` unit tests if at all (`grep -rn "needs tools.native.async" internal/sessionfs/*_test.go` finds no layout-based case). A regression that drops the async requirement (or doubles it) would not fail a layout test.
- suggested fix: add an opt-out (e.g. `Spec.NoAsyncDefault`) or one negative layout case that bypasses `withAsync`. Files: backend-go/internal/sessionfs/layouttest/layouttest.go
- effort: S
- prior art: new

## 4 Performance hypotheses (reviewers do not measure; [unmeasured])
- H01: every session load re-decodes `tools` and `tools.native` once per native tool, file: backend-go/internal/sessionfs/session.go:377
  workload: session with N native tools enabled (5 vs 20); 200 sessions listed (each `LoadSessionSettings` + `scanTranscriptIndex`)
  benchmark: existing `BenchmarkListManySessions`, `BenchmarkListSessionsProfiles` (app/sessionfs); proposed `/tmp` variant with N tools vs 1 tool per session
  expected: `decodeConfigObject(tools)` + `decodeConfigObject(tools.native)` + `orderedObjectKeys` run ~20x per session load; scales with tools-per-session, not sessions alone
- H02: `normalizeBashStyleFields` re-validates the shared child-process keys per tool that embeds them, file: backend-go/internal/sessionfs/native_bash_settings.go:164
  workload: sessions enabling bash + exec + tool_smith together (each parses `env`/`env_file`/`spill`/`paths`); large `env_file` (1 KiB vs 1 MiB)
  benchmark: proposed `/tmp` load benchmark with bash-only vs bash+exec+tool_smith, small vs large env_file
  expected: `loadNativeBashEnvFile` + `normalizeChildEnvironment` + `normalizeNativeBashSpill` (including the readability walk over read/grep policies) run 3x per session; env_file read cost triples
- H03: subagent agent-folder load stats and reads every agent file at session load, file: backend-go/internal/sessionfs/native_subagent_dir.go:47
  workload: `agents_dir` with A agents (10 vs 200 folders), each with `agent.json` + `prompt.md` + `prompt_documents`
  benchmark: proposed `/tmp` load with A folders; existing session-load benchmarks as harness
  expected: `os.ReadDir` + `os.Stat` per entry + `os.ReadFile` per `prompt_documents` per session load; scales with agents, paid even when the run never calls subagent
- H04: memory `statecontext` filter validation splits and regex-checks every filter key per load, file: backend-go/internal/sessionfs/native_memory_settings.go:282
  workload: selector with F filter keys (8 vs 64) and D depth (2 vs 8); 128 selectors (the cap)
  benchmark: proposed `/tmp` load with F/D at caps vs minimal selector
  expected: `strings.Split` + per-part regex per key per selector on every session load; scales with F*D*selectors
- H05: transcript/transcripts normalize copies the read policy per call, file: backend-go/internal/sessionfs/native_transcript_settings.go:56
  workload: sessions with large `read.paths` allow lists (10 vs 1000 rules); transcript tool built per remote transcript request (phase 4 N34 area)
  benchmark: existing transcript benchmarks (`BenchmarkScanTranscriptIndex*`); proposed `/tmp` policy-copy micro-benchmark
  expected: `Policy: readPolicy` copies the slice header only (cheap) but downstream `indexTranscriptTurns` re-scans per request; the settings cost is the re-parse of `tools`/`native` (see H01), not the copy
- H06: `unsupportedToolKey` edit-distance per unknown key is O(candidates * keylen^2), file: backend-go/internal/sessionfs/tool_shape.go:105
  workload: config with a typo in a large candidate list (native 20 names vs subagent agent keys 13); error path only
  benchmark: none proposed (error path, not hot); no new benchmark needed
  expected: negligible on the happy path; only typo loads pay it
- H07: `withAsync` map copy per fixture write, file: backend-go/internal/sessionfs/layouttest/layouttest.go:339
  workload: test-only; N sessions added via `AddSession` (10 vs 1000)
  benchmark: none (test helper, not production)
  expected: one small map alloc per session write; no production effect

## 5 Cross-phase leads (outside p2a, with file:line)
- `backend-go/internal/tools/native/fetch.go:390-430`: `allowed_hosts`/`denied_hosts` exact-match (`containsHost` at :460) vs `isPublicFetchAddress` (:466) SSRF floor; whether an IPv6 allow/deny gap (P2A-R3-03) is exploitable depends on resolver + `isPublicFetchAddress` behavior for zone IDs (`%25`) and mapped IPv4 — verify in phase 7a with `httptest` fakes.
- `backend-go/internal/tools/native/read_image.go:68`: `readImageFile(path, settings.Policy, ...)` enforcement of the read_image policy vs the read policy (P2A-R3-04); confirm which policy the operator docs promise — phase 7a (path confinement).
- `backend-go/internal/tools/native/request_draft.go:99`: `HeadlessAutoAcceptTimeoutMS` used only when `RequestInteraction == nil`; whether `MaxTimeoutMS` is meant to bound it is a phase 7b question (interaction broker `maxInt64(question.TimeoutMS, question.MaxTimeoutMS)` at interactions/broker.go:80 ignores headless).
- `backend-go/internal/sessionfs/native_bash_settings.go:402`: spill readability via `bashPathWithin(cwd,...)` without `EvalSymlinks`; symlink escape is a phase 7a (`path.go`/`mutate_path.go`) check, not a settings-parser defect.
- `backend-go/internal/asyncmgr/prune.go:84,114,138`: duration overflow consumer of P2A-R3-05/M23 — the fix belongs with the async manager (phase 4) as well as the loader.
- `backend-go/internal/tools/native/subagent*.go` + `sessionfs/native_subagent_*.go`: `prompt_documents`/`agents_dir` read whole files at load with no size cap (`loadSubagentPrompt`, `loadSubagentFolder`); large-file DoS is operator config today, model cannot set it — phase 6c to confirm no model-reachable path sets these.
- `backend-go/internal/app/resolver.go` + `sessionfs/session.go:501`: the `needs async` error is the only coupling between p2a validation and the async manager (phase 4); layouttest masking (P2A-R3-11) means phase 4 tests should keep one direct negative case.

## 6 Checked and sound (one line each)
- `validateToolSettingsShape` (tool_shape.go:29) canonicalizes `native: null` to `{}` before per-tool normalizers, rejects unknown `tools`/`native`/extension keys with suggestions — sound.
- `orderedObjectKeys` + `decodeConfigObject` pair rejects non-object sections (`must be an object`) for every tool — sound (explicit `null` section errors, matching the checkpoint null-rule for sections).
- `configuredInteger`/`configSafeInteger` (tool_settings.go:130) rejects floats/NaN/Infinity/strings and enforces min/max for every numeric key sampled — sound.
- `nativeBashStyleKeys` reuse via `normalizeBashStyleFields` for bash/exec/tool_smith (same defaults, same `max_bytes`/`spill`/`env` semantics) — sound; process is the intentional outlier (own shell/timeout model).
- `normalizeNativeBashSpill` (bash_settings.go:333) requires `{call_id}`, rejects unknown placeholders, rejects same-file stdout/stderr, gates unreadable targets behind `allow_unreadable` — sound (raw-JSON compare at :394 is cosmetic only).
- `normalizeNativeExecLangs` alias-collision check + `default_lang` resolution to table name (exec_settings.go:218) — sound; lowercase-only pattern removes the case-collision hypothesis (H13 disproved).
- `normalizeNativeToolSmithTools` (tool_smith_settings.go:199) refuses `bash/exec/process/async/subagent/tool_smith/task` from scripts and unknown/duplicate names — sound.
- `normalizeProfileMessageDenyProfiles`/`normalizeSessionGlobs`/`normalizeRemoteNames` duplicate rejection + pattern checks — sound (duplicates error here, unlike fetch/memory dedupe — noted in P2A-R3-09/10 context).
- `subagentNamePattern` vs `profileMessageIDPattern` dot gap noted in p1 merge §M15 is intended (agent names no dots, profile ids allow dots) — not re-raised.
- `checkSubagentPaths` (subagent_paths.go:70) refuses `paths` with shell tools and without write/edit — sound.
- `loadSubagentDirAgents` skips dotfiles, follows dir symlinks by design, records `taken` to refuse double definitions (override only for folder-over-folder) — sound.
- `loadSubagentPrompt` duplicate-path and missing-file errors, `prompt.md` prepended to `PromptDocuments` — sound.
- `remote_api_settings.go:116` 0600..0666 (p1 N26) is outside p2a (p2b2) — not re-reported.
- F-22 (74 exported types, 21 Native*Settings) is closed with no fix — not re-reported; P2A-R3-07 is the bounded duplicate instance, not a reopen.

## 7 Not covered and why
- `*_test.go` read only as evidence; full suite not rerun per prompt (baseline cited from phase1_prompt: 34 ok, 0 FAIL). Targeted runs here: none (read-only review; `go vet`/`gofmt -l`/`go build` on `internal/sessionfs` all exit 0 from backend-go/).
- `git show` for every 3.3 commit not exhaustively re-read; sampled `86552a4` (latest_* keys), `f0e8b78` (profile_message sessions), `1bf45fe`/`cc2023d`/`236ceb9` (remote transcript/subagent/profile_message), `57c6b84`/`aa8e078` (transcript v2), `0fda7ef` (transcripts registration), `4b50184` (max_queued_per_profile), `7e393ec` (6h cap) stats + key diffs. Full `be4af1d..a21104b` range not re-diffed file-by-file.
- No `-race`, `-bench`, profiles, `-count>1`, `go test ./...`, network, or fixed ports per machine rules. `staticcheck`/`golangci-lint` not installed per baseline; `go vet` only.
- `sessionfs` loader/stamp internals (`session.go` beyond the cited ranges, `layout_*`, `catalog_*`, `check.go`, `settings_stamp.go`), `session/` runner, `httpapi`/`remoteapi` servers, `transcript` store, `tools/native` executors — touched only as callers/consumers for traces (≤3 hops), no speculative reading.
- Measurement of H01-H07 not done (reviewers do not measure; serialized step to follow).
