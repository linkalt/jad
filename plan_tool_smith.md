# Tool Smith — Implementation Plan for jad (revised)

Companion to `design_tool_smith.md` (revised; see its "Revision notes"). Sources (T-002): `.jad/subagents/.shared/docs/T-002/planner_plan.md` and `code-explorer_explore.md`, `issue-researcher_findings.md`. Documentation only: this file changes no code or config. The task named no separate report file, so this plan and the design's Revision notes are the deliverable. Items marked *(unverified)* were not read in code.

## Decisions fixed by the user

1. The native-tool bridge is required (`tool_smith` is a dynamic, optimized codemode-style tool; skill plus `exec` cannot do it). No go/no-go gate.
2. External bun/node runtime; no embedded engine.
3. Nested tools: read-only allowlist by default; `bash`, `exec`, `process`, `subagent`, `tool_smith`, `task` always excluded.
4. Functions in the workspace, default `.jad/functions`.
5. Audit: counters only (result `Information`).
6. `run` has exec/bash trust level; no OS sandbox.
7. No nested calls in `messages.jsonl`.
8. Ideas taken from harness2 (design section 16): one-message validation of function files, short index with bodies on demand, visible truncation marker with a floor, name whitelist plus escape check; no silent drops, no hard-coded limits.
9. Every limit is a `tools.native.tool_smith.*` key with default, schema in `sample/schemes/`, config test and `backend-go/DECISIONS.md` entry.
10. (User-accepted after the spike.) `ReadOnly: true`, `MayChangeFiles` true only for calls containing `run`, plus an explicit `tool_smith` exclusion in `childToolPools` (`native/subagent.go:162-170`) so sub-agents never get it.
11. Own bash-style `Child` settings block like `exec` (`native_exec_settings.go`); no dependency on `tools.native.bash`.
12. v1 oversized return value: cut at `result_max_bytes` with a visible truncated marker; no spill (exec spill covers streams only).
13. Bridge channel: Unix socket in a private temp dir, path and token via env vars; no extra fd.
14. No frontend change.

## Verification rules (every phase)

From `/home/www/Downloads/tmp/myagent3/backend-go`:

- `go test ./internal/... ./cmd/...` (never `./...`). Narrow while iterating, e.g. `go test ./internal/sessionfs/... ./internal/tools/... ./internal/session/...`.
- `go vet ./internal/... ./cmd/...`; `gofmt -l internal cmd` prints nothing; `git diff --check` (repo root) clean.
- Golden check: a session without `tool_smith` produces byte-identical `messages.jsonl`. Existing golden test: `backend-go/internal/session/runner_native_tools_golden_test.go` with `testdata/*.messages.jsonl` (named by the spike, not run); config-parity: `sessionfs/layout_settings_cases_test.go:23` and `testdata/layout-settings.cases.json:176`.
- No frontend change (decision 14). Never `npm run build`.
- Never restart, stop or kill the running backend; no git command that changes tree, index or history.

## Phase 1 — Evidence spike (no code) — DONE

Report: `.jad/subagents/.shared/docs/T-002/code-explorer_spike.md`. Findings:

- (a) `runShellChild` is reusable for env (`extraEnv`, `bash.go:158-159`) and file-delivered prelude; no extra fd (`bash_child.go:31-48`), so socket path via env. `bun --preload X {file}` passes the argv parser (not run). Return channel is cut, not spilled (`bash.go:~237-262`; `planBashSpill` `bash.go:212` is streams only). exec keeps its own `Child` copy (`native_exec_settings.go:~62-65`).
- (b) `MayChangeFiles` takes the whole-workspace checkpoint (`session/runner_tools.go:152-170`, `checkpoint/service.go:417`), nested writes covered; `run` serial, `list` parallel (`tools/catalog.go:247-261`). Side effect: `childToolPools` would hand a `ReadOnly` tool to sub-agents (`subagent.go:162-170`), so exclude it.
- (c) `function_call_output` persisted via `appendToolOutputs`; no byte cap found (`transcript/input_history.go:22-74`; rest unread).
- (d) Frontend tool cards are generic (`TranscriptParts.tsx:386-440`): no change.
- (e) Wiring points and test touchpoints are listed under phase 3a-3c. Defaults: design section 15.

## Phase 2 — Baseline measurement (cheap, no code) — spawn part DONE, token baseline pending

- **Goal:** record what plain tool calls cost today, so phase 8 can show what `tool_smith` saves. Not a gate.
- **Done (`tester_spawn.md`):** warm spawn of a trivial `.ts` (20 runs): bun 1.4.3 min 3 / median 3.5 / max 5 ms; node v26.12.0 min 49 / median 53.5 / max 60 ms; deno absent. First iteration (not a true cold cache) 4 ms and 57 ms. Spawn is negligible; default `ts` = bun.
- **Pending (tester, before phase 8):** run 3-5 reference tasks (multi-file search plus summary, callers of a symbol across files, collect and compare config values) with plain `read`/`grep`/`glob`, fixed model and prompt wording; record model rounds, input/output/total tokens, wall-clock, refused calls, rounds spent on discovery with `glob`/`grep`.
- **Exit criteria:** a table per task, exact task prompts (reused in phase 8).
- **Risks:** small sample is noisy (repeat each task at least twice); costs real model tokens.

## Phase 3 — Config surface and registration (three sequential executor steps, after phase 1)

All paths under `backend-go/` unless noted. Keys and defaults: design section 15 (own bash-style `Child` keys via `nativeBashStyleKeys`; no dependency on `tools.native.bash`). Test commands run from `backend-go/`.

### 3a — Settings normalizer, SessionConfig, name list, tests
- **Files:** `internal/sessionfs/native_tool_smith_settings.go` (new; models `native_exec_settings.go`, `native_skill_settings.go`; exports `nativeToolSmithKeys`); `internal/sessionfs/tool_shape.go:12-14` (`nativeToolNames` add `tool_smith`); `internal/sessionfs/session.go` (`NativeToolSmith` field near :159, `normalizeNativeToolSmithSettings` near :440, struct literal near :579, `applyApprovalDefault` call :468 plus `approval_default.go:66-72`). If `env_file`/paths are accepted, also `native_subagent_paths.go:88` and `layout_load.go:588` (otherwise do not accept them).
- **Rules:** no hard-coded limit; `tools` list rejects bash, exec, process, subagent, tool_smith, task and non-native names; byte keys max `maxNativeToolBytes` (`tool_settings.go:20`).
- **Tests:** new `native_tool_smith_settings_test.go` (pattern `native_skill_settings_test.go`): defaults, unknown key, invalid values and ranges, excluded tool names, approval; update `layout_settings_cases_test.go:23` `goOnlyNativeTools` so the supported-keys error text in `testdata/layout-settings.cases.json:176` still matches; `profile_tools_test.go:249` stays red until 3b adds the pattern (expected).
- **Command:** `go test ./internal/sessionfs/...`
- **Exit:** sessionfs tests green except the profile-pattern test, which 3b fixes (or run 3a+3b tests together); unconfigured sessions unchanged.

### 3b — Schemas, tool definition sample, profile pattern, DECISIONS
- **Files:** `sample/schemes/setting.schema.json` (`tools.native.tool_smith` block next to `exec` ~:1074; sub-agent `tools` description ~:1706 optional); `sample/tools/native/tool_smith.json` (new; id `native.tool_smith`, name, short static description, inputSchema as design 4.1; model `profile_message.json`); `sample/schemes/profile.schema.json:68,76` (add `tool_smith` to both patterns); `.jad/tools/jad/default.json` only if the live session should have the tool; `DECISIONS.md` (new entry: keys, defaults, minimums, Child block, ReadOnly/MayChangeFiles, sub-agent exclusion, cut-not-spill, socket+env). `toolset.schema.json` and `agent.schema.json` need no edit (`native_tool_schema_test.go:109-117` enforces `$ref`).
- **Tests:** `internal/sessionfs/native_tool_schema_test.go`: add `"tool_smith": nativeToolSmithKeys` to the map at :122 and valid/invalid cases near :71-82; `profile_tools_test.go:249` passes. Check tests that read `sample/tools/native/*.json` (`hooks_test.go:315`, `decide_image_test.go:391`, `app/layout_integration_test.go:119-120`) still pass with the extra file.
- **Command:** `go test ./internal/sessionfs/... ./internal/tools/... ./internal/app/...`
- **Exit:** schema keys equal loader keys; profile pattern test green; `git diff --check` clean.

### 3c — Catalog registration, sub-agent exclusion, configured check, approval preview
- **Files:** `internal/tools/native/catalog.go` (`:81` short-circuit chain add `config.NativeToolSmith == nil`; `buildNativeTools` block near the skill block ~:354-366 using `requireNativeDefinition` with `ReadOnly: true` and `MayChangeFiles` true only for calls containing `run`, as a stub tool returning a not-implemented result until phase 4; `nativeToolConfigured` switch :391-430 add `case "tool_smith"`); `internal/tools/native/subagent.go:162-170` (`childToolPools`: skip the `tool_smith` tool ID like `subagentToolID`); `internal/tools/approval.go:150-175` (preview branch for `tool_smith`, showing the `run` body cut; read the default branch first).
- **Tests:** `internal/tools/native/catalog_names_test.go:12-16` (new case); new catalog test (pattern `catalog_skill_test.go`): registered when configured, absent otherwise; sub-agent read-only pool never contains `tool_smith`; `ChangesState` true for `run` and false for `list`; golden `internal/session/runner_native_tools_golden_test.go` unchanged.
- **Command:** `go test ./internal/tools/... ./internal/session/... ./internal/sessionfs/...`; then `go vet ./internal/... ./cmd/...` and `gofmt -l internal cmd`.
- **Exit:** configured sessions list the tool; unconfigured sessions byte-identical; no sub-agent exposure.
- **Risks (phase 3):** missed config requirement (schema, test, DECISIONS); tool-list order changes the request prefix for configured sessions only.

## Phase 4 — `run`: child execution

- **Goal:** `run` executes a body in a child and returns its value, without the bridge.
- **Worker:** executor, after phase 3.
- **Files:** new `backend-go/internal/tools/native/tool_smith.go` plus an embedded prelude file.
- **Behaviour:** private 0700 per-run dir with wrapper, prelude and `input.json`; spawn via the exec child runner (env allowlist, kill grace, timeout, ctx cancel); return value through a result file (as `$EXEC_RESULT`, `DECISIONS.md:261`); cut at `result_max_bytes` UTF-8 safe with `truncated` and a visible marker (no spill); `RenderToolResult`; forgiving `NormalizeArguments` (string form, aliases `script`/`code`/`body`); `interpreter_unavailable`; scheduling flags per decision 10; approval title "Run script".
- **Tests:** success, syntax error, thrown error, timeout (`timed_out`), truncation, missing interpreter, ctx cancel (no child left), missing `function` (`invalid_input` with example), string form, aliases, temp dir removed on all paths.
- **Exit criteria:** tests pass; works with `tools` unset.
- **Risks:** none expected for `runShellChild` (env path is enough); leaked processes; secrets via env (use the bash allowlist).

## Phase 5 — Bridge and nested dispatch

- **Goal:** scripts call allowlisted native tools via `tools.<name>(args)`.
- **Worker:** executor, after phase 4.
- **Files:** `tool_smith.go` plus a bridge file in the same package if needed; nested catalog in the `childToolPools` style (`internal/tools/native/subagent.go:150-166`); dispatch through `Catalog.ExecuteFunctionCall` (`internal/tools/catalog.go:318-404`).
- **Behaviour:** one multiplexed per-run Unix socket in the private temp dir (path and token via env, ndjson frames with ids); allowlist default `read`, `grep`, `glob` minus the always-excluded set; mutex for non-read-only nested calls, semaphore `max_parallel_calls` for reads, `max_nested_calls`; envelope parsed (`internal/tools/output.go:20-60`) to `{ok, status, summary, content, info}`, no throw on tool failure; `nested_result_max_bytes`; nested approval via wrapped `RequestApproval` (`subagent_call.go:58-73,191`); counters (calls, per-tool, nested writes, bytes, duration) in `Information`; `fn.<name>` lazy import with name validation.
- **Tests:** allowlist, excluded tools refused, no recursion, approval deny returns `blocked` to the script, concurrent writes serialized, read cap, call cap, wrong token rejected, malformed frame, socket and dir removed on success/timeout/cancel, no goroutine leak; `go test -race ./internal/tools/...` if available.
- **Exit criteria:** tests pass; no nested items in `messages.jsonl`; checkpoint covers nested writes (spike (b)).
- **Risks:** deadlock between mutex and approval waits; goroutines blocked after child exit; approval floods (capped by `max_nested_calls`); bridge is policy, not exclusive (documented).

## Phase 6 — `list`

- **Goal:** compact discovery of `functions_dir`.
- **Worker:** executor, after phase 4; parallel with phase 5.
- **Behaviour:** plain-text line per function (`name(sig) — description`); a malformed file is reported with all its problems in one message, never skipped silently; read only `list_header_bytes` per file, at most `list_max_files` files and `list_max_results` lines, a visible `truncated` marker with counts (`M of K shown`) plus filter hint; substring filters; empty versus error distinct; no cache; name whitelist and escape check for `functions_dir` (`..`, absolute, symlinks, outside workspace), shared with the `fn` import.
- **Tests:** header parse, caps, filter, empty versus error, malformed header; `go test ./internal/tools/...`.
- **Exit criteria:** tests pass; output is one short line per function.

## Phase 7 — Regression, config and golden tests

- **Goal:** safety across config, tool and transcript.
- **Worker:** tester discovers commands; reviewer reads results.
- **Scope:** all cases above; config and schema validation for every key; golden diff empty for unconfigured sessions; one fixture where a `tool_smith` call yields a single `function_call_output` and no nested items; no frontend tests (decision 14).
- **Commands:** the verification rules.
- **Exit criteria:** all green; golden diff empty.

## Phase 8 — Skill, review, live trial

- **Workers:** executor (final `SKILL.md`; tool description about 5 static lines); reviewer (design versus code, trust statement, approvals, goroutines, config completeness); tester (real-model trial as in `DECISIONS.md:265`: the same tasks and prompts as phase 2, rounds and tokens versus the baseline, including `duplicate_calls` if counted, large-result behaviour and `list` cost versus `glob`/`grep` discovery).
- **Exit criteria:** findings resolved or recorded; numbers compared with the phase 2 baseline; `DECISIONS.md` records decisions and deferrals (within-run memo, spill, persistent runtime, hard sandbox).
- **Risks:** no measured gain over the baseline (report it; tuning, not removal of the bridge); prompt-cache impact (keep the definition static).

## Order

1 done; 2 spawn done, token baseline pending (before 8); 3a, 3b, 3c in sequence; then 4, 5 in sequence; 6 after 4, beside 5; 7 after 4-6; 8 last (needs 2).

## Open questions

All three earlier questions are decided (decisions 10, 12 and bun as default `ts` interpreter, node documented as alternative).

## Unchecked

- Whether bun accepts `--preload` in an `interpreter` argv (not run); rest of `AppendInputItemsAt` and `validateInputFields` (size checks); default branch of `tools/approval.go` preview; env_file/paths wiring for a new bash-style tool; whether `httpapi` special-cases tool names; behaviour of `decide_image_test.go:391` with an extra sample file.
- Token baseline (phase 2 pending part).
