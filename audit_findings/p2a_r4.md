# Phase 2a State — independent review

- reviewer: r4
- HEAD: fbe49a89d14f2aa6998a1561afb1a85decde0faa
- date: 2026-10-27 (container date)
- scope: `backend-go/internal/sessionfs/native_*.go` (24 production files) and `backend-go/internal/sessionfs/layouttest/layouttest.go`; every production source file was inspected through bounded codebase-memory snippets, tests selectively used as evidence. No other `p2a_r*.md` file opened.
- commands: `git rev-parse HEAD` (exit 0; fbe49a89d14f2aa6998a1561afb1a85decde0faa); `go test ./internal/sessionfs -run 'Native|Normalize|SettingsStamp' -count=1` (exit 0; package passed, 0.121s); `git diff --check` (exit 0); `rg -n '[[:blank:]]+$' audit_findings/p2a_r4.md` (exit 1; no matches); `git status --short` (exit 0; pre-existing `M EXECUTOR.md`, `M MASTER.md`, `?? audit_findings/`, `?? nodir/`; untouched). Only this report was written; no commits.

## 1. Understanding

Phase 2a covers normalization and typed models for native tool settings, plus the layouttest fixture builder. Main risks are rejecting every unsupported key and invalid type/range, keeping defaults/null semantics consistent across the 21 Native*Settings types, ensuring paths/env/agent permissions cannot widen tool capabilities, and ensuring the test helper creates valid isolated catalog layouts. Four lenses apply; performance claims remain hypotheses only. Prior audit fixes are not to be repeated.

## 2. Final hypotheses and outcomes

- H01 — unsupported-key validation is present for top-level tool sections, but incomplete for nested memory/task/process statecontext objects (P2A-R4-01).
- H02 — type/default/bound checks were traced across the settings; one accepted async retention range still overflows duration arithmetic (P2A-R4-02, prior M23).
- H03 — path, env, scope, and resource-limit settings were traced; no additional security finding was verified. Remote transcript API settings force path reads off and hide paths; remote access is off by default.
- H04 — F-22 is an intentional shared-config design and is not re-filed; a misleading subagent tool-list comment is P2A-R4-03. Repeated parent JSON decoding is an unmeasured performance hypothesis (H01).
- H05 — fixture writing is deterministic for the standard model case; an empty model map panics during default-profile creation (P2A-R4-04).

## 3. Findings

Four read-based findings are retained: 2 low A correctness, 2 low D maintainability; none in B or C. C performance claims remain unmeasured hypotheses in section 4.

## 4. Performance hypotheses for measurement (no benchmarks run)

No performance cost was measured; the following two hypotheses are for the later serialized measurement step.

- H01: session settings load repeats JSON decoding of the shared `tools` and `tools.native` objects once per native normalizer. `loadSessionSettingsFrom` invokes the normalizers sequentially (`backend-go/internal/sessionfs/session.go:377-494`); each normalizer independently calls `decodeConfigObject` for those parent objects (for example, `native_transcript_settings.go:55-77` and `native_memory_settings.go:84-99`).
  workload: one session with 15+ configured native tools and repeated settings loads after invalidation, compared with one or two enabled tools.
  benchmark: existing `BenchmarkLoadSessionConfig` (`backend-go/internal/sessionfs/settings_load_benchmark_test.go:10-58`), extended to vary the number of native settings blocks.
  expected: load latency and allocations grow with the number of normalizers because common parent objects are re-unmarshaled; no measurement was run.
- H02: subagent settings reload walks every configured agent directory and reads prompt files/documents in full; `loadSubagentFolder` also reads each `agent.json` once with `os.ReadFile` and again through `readSessionObject` (`native_subagent_dir.go:68-161`; `session.go:1168-1177`).
  workload: 100 agent folders with 1 MiB prompt documents, then repeated settings loads after a stamp change.
  benchmark: existing `BenchmarkLoadSessionConfig`, extended with generated `agents_dir` folders and documents.
  expected: file I/O and temporary allocations scale with folder count and total document bytes, with a duplicate settings-file read per folder; no measurement was run.

## 5. Cross-phase leads

- No separate cross-phase defect is filed. Native tool execution consumers are outside 2a; one-hop traces were used only to interpret setting consumers and capability constraints. H01-H02 are phase-local measurement leads.

## 6. Checked and sound

- Top-level normalizers across the 24 source files reject unsupported keys, apply defaults, and parse bounded numeric values; known exceptions are recorded in P2A-R4-01 and P2A-R4-02. Representative schema/loader cases are covered by `TestNativeToolSchemasAgreeWithTheLoader`; that test does not exercise the nested unknown-key cases in P2A-R4-01.
- Bash/exec/tool_smith share `normalizeBashStyleFields`; nested bash stream/spill objects have explicit key checks. Environment keys/values are validated, and the `.env` parser does not evaluate shell substitutions. `tool_smith` excludes shell/process/recursive tools; subagent path restrictions reject shells when paths are configured. These checks are about settings, not the out-of-scope tool execution implementations.
- `allow_remote` is false by default for transcript tools; `RemoteAPITranscriptSettings` sets `AllowPath=false` and `HidePaths=true` (`native_transcript_settings.go:141-168`). The defaults allowing other local sessions/profiles/children are explicitly documented in `DECISIONS.md:1023-1030`.
- Audit-backend F-22 described the shared settings package/type count as intentional and closed with no fix; type count/duplication alone is not filed. The concrete stale tool-list comment is separate (P2A-R4-03).
- `layouttest.Write` uses `t.TempDir`, sorts model IDs before selecting the default, and writes fixture files with 0600; the empty-model edge is P2A-R4-04.

## 7. Not covered / verification limits

- The targeted `go test` command passed existing matching tests; it does not add a test reproducing P2A-R4-01 or P2A-R4-04. The duration-overflow behavior was previously reproduced in the P4 merge copy; I did not reproduce it again.
- No full suite, build, vet or gofmt command was run; the coordinator baseline is not repeated. No benchmark, profile, or `-race` run was made, per the audit constraints.
- All 24 production `native_*.go` files and `layouttest.go` were inspected via bounded source snippets; test files were sampled as evidence rather than exhaustively reviewed. Native tool execution implementations and end-to-end behavior outside `sessionfs` are not claimed as audited.
- Read-only review: no code/config edits or commits. The existing `git status --short` entries were left untouched; the only intended write is this report. No other `p2a_r*.md` file was opened.
- A first report-finalization edit used double-quoted Markdown backticks, so shell substitutions were attempted; a `go test` from the repository root failed because no Go module exists there, and Markdown tokens produced command-not-found errors. `edit.sh` itself completed; the malformed report text was corrected. The valid targeted test ran from `backend-go`; no source or Git changes resulted.

### Review progress (append-only)

- Codebase-memory project `home-www-Downloads-tmp-myagent3` confirmed the current index; its `sessionfs` tree identifies the 24 production `native_*.go` files plus `layouttest/layouttest.go` (tests are supporting evidence, not additional source scope).
- First settings batch traced: async, bash/exec child-style, fetch and glob normalizers. Unknown-key checks are present; the inspected numeric settings use bounded parsing and relevant cross-field constraints. Bash/exec share `normalizeBashStyleFields`, including env, spill and path policy handling. No finding verified from this batch yet.
- Async settings history includes `86552a4` (latest keys/progress cursor); this and the plan-listed subagent/profile-message/transcript commits are being traced rather than assumed correct.


- Second batch reviewed via codebase-memory snippets: `native_grep_settings.go`, `native_memory_settings.go`, `native_mutation_settings.go`, `native_process_settings.go`. The memory and process normalizers reject unsupported/removed keys, bound configured numeric values, and enforce the observed cross-field constraints; mutation backup settings accept null as the documented beside-file default and normalize paths; no defect has been verified from this batch.
- Performance review remains unmeasured; no benchmark, profile or race command has been run.


- Third batch traced: profile-message target/session/remote lists and bounds; read-image path/byte settings; request-draft cross-field time limits; skill byte limit; state-context enum; subagent tool, agent, directory, path, deny, state-context and persistence/resource settings. Object/null handling, removed keys and cross-field checks were examined. The subagent path guard rejects any configured shell alongside a path restriction; tool-name filtering is subsequently applied against the child tool pool (cross-phase trace), so no capability widening was verified here.
- Performance lead, not a finding: subagent prompt documents and `agents_dir` folders are read in full during settings normalization (`native_subagent_settings.go:540-576`; `native_subagent_dir.go:68-101`); I found no prompt-document byte/count limit in these files. I am checking the rest of the loader/test path before finalizing the workload hypothesis.


### P2A-R4-01  Native state-context objects silently ignore unsupported keys
- severity: low
- lens: A correctness
- location: backend-go/internal/sessionfs/native_memory_settings.go:239-247, 245-255; backend-go/internal/sessionfs/native_task_settings.go:172-214; backend-go/internal/sessionfs/native_process_settings.go:272-314
- evidence: [read] These normalizers decode nested state-context objects/selectors and read the known fields, but do not reject leftover keys. For example, `tools.native.task.statecontext` with `"detial_statuses"` instead of `"detail_statuses"` is accepted and silently uses the default detail list; memory selector objects similarly ignore a misspelled `limit`/`filter` key, and process statecontext ignores unsupported keys. This contrasts with `normalizeNativeSubagentStateContext` (`native_subagent_settings.go:379-384`), which explicitly validates its keys. No targeted reproduction/test was run.
- suggested fix: add an explicit allowlist check for the task and process statecontext objects and for each memory selector; add tests asserting an unknown nested key is rejected.
- effort: S
- prior art: new


### P2A-R4-02  Async retention settings still allow duration overflow and mass pruning
- severity: low
- lens: A correctness
- location: backend-go/internal/sessionfs/native_async_settings.go:10,69-70,77-91; backend-go/internal/asyncmgr/config.go:97-99; backend-go/internal/asyncmgr/prune.go:83-115,127-139
- evidence: [read] The settings normalizer accepts both `retention_days` and `unread_ttl_days` up to `maxNativeAsyncValue` (10,000,000); `Config.Validate` rejects negative day counts but has no upper bound. `pruneLocked` multiplies days by `24*time.Hour`, so values above the signed-duration day limit (~106,751) overflow; a 10,000,000-day retention produces a future cutoff and can drop every finished record. The prior P4 merge recorded a reproduction in a copy (M23 / P4-R1-04); current settings, validation and pruning lines remain unchanged. No new reproduction was run.
- suggested fix: cap both day settings at a duration-safe maximum in the native settings normalizer and `asyncmgr.Config.Validate`; retain a boundary regression test for both settings.
- effort: S
- prior art: M23 / P4-R1-04 (confirmed in p4_merged.md; still present in the current tree)


### P2A-R4-03  Subagent tool-field comment incorrectly limits explicit tools to read-only
- severity: low
- lens: D maintainability
- location: backend-go/internal/sessionfs/native_subagent_settings.go:183-186, 159, 649-657; backend-go/internal/sessionfs/native_subagent_persist_test.go:75-86
- evidence: [read] The `NativeSubagentAgent.Tools` comment says the list names read-only tools, but the loader and `SubagentChangeTools` explicitly support `write`, `edit`, `bash`, `exec` and `process`; the in-scope test verifies those values are accepted and mark the agent as changing files. This is a misleading capability contract at the settings type, not a runtime capability finding.
- suggested fix: document that an explicit list may include the supported change tools (and that an absent list defaults to read-only tools), including how `paths` applies.
- effort: S
- prior art: new


### Review progress (append-only)

- All 24 production `native_*.go` files have now been located and inspected through bounded codebase-memory snippets; the nested statecontext omission is retained as P2A-R4-01. The tool-schema test covers representative flat keys across both schemas and the loader; it does not exercise the memory/task/process nested statecontext unknown-key cases.
- Prior-art check: F-22 in `audit_backend.md` was explicitly closed as an intentional shared-config design, so type count/duplication alone is not filed. P4 M23 is not fixed in this tree: current `native_async_settings.go`, `asyncmgr/config.go`, and `prune.go` still allow/multiply oversized retention days; P2A-R4-02 re-confirms it under the prior ID.
- `layouttest.Write` and all fixture methods were traced. The `listed[0]` empty-model panic is retained as the low-severity fixture finding P2A-R4-04.


### P2A-R4-04  Empty model catalogs make the layout fixture panic
- severity: low
- lens: D maintainability
- location: backend-go/internal/sessionfs/layouttest/layouttest.go:69-72, 195-202
- evidence: [read] When `Spec.Models` is a non-nil empty map and `Spec.Profiles` is nil, `Write` builds an empty `modelIDs`/`listed` slice and indexes `listed[0]` for the default profile. This panics rather than reporting an invalid fixture through `t.Fatal`; `NoSession` does not bypass default-profile generation. The `Spec.Models` contract does not state that an empty map is forbidden. No reproduction was run.
- suggested fix: either reject an empty model list with an explicit `t.Fatal` before indexing or skip the default profile when no models are requested; add a fixture test for the empty-map case.
- effort: S
- prior art: new


- Independent pass, batch 1 (codebase-memory): inspected `native_async_settings.go`, `native_bash_settings.go`, `native_exec_settings.go`, `native_fetch_settings.go`, `native_glob_settings.go`, `native_grep_settings.go`, `native_profile_message_settings.go`, `native_subagent_settings.go`, `native_transcript_settings.go`, and `native_transcripts_settings.go`. Their leaf decoders reject unsupported keys, apply explicit typed bounds/defaults, and validate cross-field limits; subagent nested `statecontext` remains the scoped unknown-key gap already filed. No additional defect retained from this batch.

- Independent pass, batch 2 (codebase-memory): inspected `native_memory_settings.go`, `native_process_settings.go`, `native_task_settings.go`, and shared mutation normalization in `native_mutation_settings.go`. The memory/task/process state-context decoders accept recognized fields but do not reject extra nested fields; the subagent variant does reject them. This confirms P2A-R4-01 and exposes the same omission in memory selector objects; no separate cause is added. Integer ranges and cross-field caps for list/storage/output settings are present.

- Independent pass, batch 3 (codebase-memory): inspected `native_read_image_settings.go`, `native_request_draft_settings.go`, `native_skill_settings.go`, and `native_tool_smith_settings.go`. Exact-key checks, bounded numeric values, and the shared bash-style normalization are in use; no defect retained from these files. Subagent prompt-document file loading is under additional size-boundary review before classification.

- Independent pass, batch 4 (codebase-memory): inspected `native_bash_env.go`, `native_env_file.go`, `native_statecontext_mode.go`, `native_subagent_deny.go`, `native_subagent_dir.go`, and `native_subagent_paths.go`. The subagent path/tool consistency check rejects shell tools when path allow/deny rules are claimed; agent/tool names and folder ordering are validated. Separately retained for the final pass: configured env and prompt documents use unbounded `os.ReadFile`/whole-content accumulation at `native_bash_env.go:13`, `native_subagent_settings.go:568`, and `native_subagent_dir.go:145`; assess as local-config resource input, not remote access.

- Independent pass, batch 5 (codebase-memory): re-traced all of `layouttest/layouttest.go` (`Write`, `AddSession`, `AddSessionOn`, `AddSessionInRoot`, and helpers). Default-profile construction unconditionally selects `listed[0]`, confirming the existing empty-catalog panic P2A-R4-04; fixture writes otherwise stay in the temporary root for ordinary inputs. No other fixture defect retained.

### P2A-R4-05  Configured env and prompt files have no byte ceiling
- severity: low
- lens: B security
- location: backend-go/internal/sessionfs/native_env_file.go:15-30; native_bash_env.go:12-18; native_subagent_settings.go:540-576; native_subagent_dir.go:143-151
- evidence: [read] A local workspace/session configuration author can point `env_file` or `prompt_documents` at arbitrarily large files. `loadNativeBashEnvFile` uses `os.ReadFile` and then converts/splits the full content; subagent prompt documents and an agent folder `prompt.md` are also read in full, accumulated, and joined. The phase code has no per-file or aggregate byte/count cap on these reads. This can produce unbounded allocations and load-time resource exhaustion in the backend when that local configuration is loaded; no remote reach is claimed. No reproduction or test was run.
- suggested fix: add configuration-owned byte budgets for env files and aggregate subagent prompt input, and read through a bounded reader; test rejection at/above the limit for both inline prompt documents and folder prompts.
- effort: M
- prior art: new

## Review completion addendum

- Coverage: the independent pass covered all 24 production `native_*.go` files in `internal/sessionfs` plus `internal/sessionfs/layouttest/layouttest.go`. The five batch notes above list each production file. No code outside the phase was independently audited.
- Configuration validation: the flat tool-setting decoders generally reject unsupported keys, type-check values, apply range caps and check cross-field constraints. `native_tool_schema_test.go` exercises both schemas and the loader across a broad representative table; it does not cover unknown keys inside the memory/process/task state-context objects filed in P2A-R4-01. The subagent version has an explicit nested-key check. The 21-type count / repeated per-setting decoding pattern is not filed by itself: F-22 was closed as an intentional shared-config design.
- Security/performance boundary: file-path settings are configuration-authorized local reads, not remote inputs. The unbounded env/prompt file reads are filed above as a conditional local resource-exhaustion issue; H09 below is unmeasured and has no benchmark result.
- Cross-phase lead: runtime enforcement of `NativeSubagentAgent.Tools` and referenced `Toolset` contents is outside p2a (phase 6 / loader integration); no claim about runtime reachability is made here.
- Not covered: no reproduction for the read-based findings, no benchmark/profile or `-race`, and no audit of the downstream native tool consumers, path-policy implementation (`file_settings.go`), or settings loader (`session.go`); these are outside p2a.

- H09: Configured env/prompt file ingestion allocates in proportion to total bytes and file count: `native_bash_env.go:12-18`, `native_subagent_settings.go:540-576`, `native_subagent_dir.go:106-161`.
  workload: a session configuration with several prompt documents or agent folders totaling 10 MiB versus 100 MiB, plus a comparably sized env file.
  benchmark: proposed `BenchmarkNativeSettingsLoadExternalFiles` in `internal/sessionfs`, using temp env/prompt files and the normalizer entry points; compare both workloads and report allocations.
  expected: load time and bytes allocated scale with total input; prompt loading temporarily retains file contents and a joined prompt, and env parsing retains the file plus split lines and the environment map. No benchmark run.

## Completion summary

- Findings: 5 total — critical 0, high 0, medium 0, low 5. By lens: A 2, B 1, C 0, D 2. Performance hypotheses: 3 total, unmeasured (H09 is the added external-file workload hypothesis).
- Commands observed: targeted `go test ./internal/sessionfs ... -count=1` for native schema/settings/statecontext/layout tests (exit 0; package ok, 0.085s); targeted subagent prompt-document tests (exit 0; package ok, 0.007s); `git rev-parse HEAD` (exit 0; `fbe49a89d14f2aa6998a1561afb1a85decde0faa`); `git status --short` (exit 0); targeted `git log -n 12 --oneline -- ...` (exit 0); `git diff --check` (exit 0). An exploratory `rg` using root-level `p1_merged.md`/`p4_merged.md` paths returned exit 2; the files were then read from `audit_findings/`.
- Not run: full suite, vet, build, `-race`, benchmarks or profiles; measurements are deferred and no performance result is claimed. No reproduction was run for any finding.
- Read-only state: no source/config edits or commits. `git status --short` showed `M EXECUTOR.md`, `M MASTER.md`, `?? audit_findings/`, `?? nodir/`; these were left untouched except for this requested report. No other `p2a_r*.md` was opened.

- Exact targeted tests: `go test ./internal/sessionfs -run '^(TestNativeToolSchemasAgreeWithTheLoader|TestNativeAsyncSettings|TestNormalizeNativeSubagentStateContext|TestNormalizeNativeProcessSettings|TestNormalizeNativeTaskSettings|TestNormalizeNativeMemorySettings|TestNativeStateContextSummaryMode|TestLayoutSetModelDefaultAddsTheKeyAfterModelsAndKeepsTheMode)' -count=1` (exit 0); `go test ./internal/sessionfs -run '^TestNormalizeNativeSubagentSettings(ReadsPromptDocuments|RejectsBadPromptDocuments)$' -count=1` (exit 0).

- Trace follow-up: `loadNativeBashEnvFile` is reached through `loadConfiguredEnvFile` from in-scope bash/exec/process/tool_smith normalizers; the graph also shows `normalizeHooksChild` as an out-of-phase caller. That hooks path is only a cross-phase lead and was not reviewed.
