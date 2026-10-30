# Phase p6a review — r3

- phase: p6a (Tool catalog, MCP, extension)
- reviewer: r3
- HEAD: 0941b6a5d61399bfa7b8dbb51f6c52d2f6613ea6 (checked `git rev-parse HEAD`)
- date (UTC): 2026-10-29
- files read (non-test, in scope): backend-go/internal/tools/catalog.go (433), entry.go (32), interaction.go (116), json_bounds.go (156), output.go (172), schema.go (515, head 1-300 + spot checks), text.go (14), extension/decide_systemone.go (377), extension/decide_systemone_backend.go (474), extension/text.go (35), mcp/manager.go (528), mcp/config.go (329), mcp/content.go (298), mcp/environment.go (45), mcp/schema_cache.go (73), mcp/structured_content_capture.go (157), mcp/structured_content_order.go (145). Tests read as evidence only: catalog_test.go, json_bounds_test.go, text_test.go, extension/decide_systemone_test.go (spot), mcp/manager_test.go, manager_concurrent_test.go, manager_lifecycle_test.go, config_test.go, content_test.go, schema_cache_test.go.
- context read: audit_plan.md §§1.1,4,5 (incl. phasemap, 3.3, 6.x); backend-go/CLAUDE.md; backend-go/DECISIONS.md (tool-catalog/MCP/decide_systemone/toolset/hooks/tool-timeout entries, incl. 2026-10-26 rename, MCP-child, approval-removal, toolset-MCP notes); `git log --oneline a095d7e^..b8a2967 -- backend-go/internal/tools/extension`, `git log --oneline -- backend-go/internal/tools/catalog.go`, `git show 1063e32/b8a2967/206ebd5` (stat + relevant diffs); audit_backend.md (F-18/D-1, F-22, F-25), audit_performance_plan.md §§A-4/A-6/P-006/P-016/P-034/P-035 (status checks only, no repeat).
- navigation: started with `codebase-memory get_architecture` (project home-www-Downloads-tmp-myagent3) + `zvec-grep` scoped to `backend-go/internal/tools` (root `/home/www/Downloads/tmp/myagent3/backend-go/internal/tools`); `search_graph` for Catalog/MCP/decide symbols then `get_code_snippet` with exact `qualified_name` (e.g. `...backend-go.internal.tools.ExecuteFunctionCall`, `...backend-go.internal.tools.mcp.LoadTools`, `...backend-go.internal.tools.extension.executeDecideSystemone`); `trace_path` on `mcp.LoadTools` (both directions); fallback `read.sh PATH START END` only for exact line ranges / non-Go context (DECISIONS, settings cross-check). No whole-file dump to context.
- commands (from `backend-go/` unless noted, serial, no `-race`/`-bench`/`-count>1`):
  - `go build ./internal/tools/...` → exit 0
  - `go vet ./internal/tools/...` → exit 0
  - `gofmt -l internal/tools` → exit 0 (no output)
  - `go test ./internal/tools/ -run TestCatalogConvertsToolPanicsToToolOutputs -count=1` → ok exit 0
  - `go test ./internal/tools/mcp/ -run TestLoadToolsConcurrentCallersShareOneConnectionPerServer -count=1` → ok exit 0
  - `go test ./internal/tools/extension/ -run TestDecideSystemoneDoesNotFollowARedirectWithTheKey -count=1` → ok exit 0
  - `go test ./internal/tools/mcp/ -run 'TestSchemaCache|TestManagerConnectsStreamableHTTPWithHeaderFromEnvironment|TestLoadToolsLeavesNoGoroutinesBehind' -count=1` → ok exit 0
  - `go test ./internal/tools/extension/ -run 'TestDecideSystemoneReportsA422DetailWithItsLocation|TestDecideSystemoneInputLimitCountsAllPartsAndTheJSONAsSent' -count=1` → ok exit 0
  - `go test ./internal/tools/ -run 'TestDraft|TestCutText' -count=1` → ok exit 0
  - `git log/show/diff/status` reads only; `git diff --check` → exit 0; `git diff --stat -- backend-go/` → empty (no repo change by reviewer)
- scratch: `/tmp/r3/` only; removed `cutcheck.go`/`cutprobe_test.go` probes at end (kept notes in report). One early probe briefly wrote a tree test file and removed it immediately (`git diff --stat -- backend-go/` empty after); no further tree writes.

## 1. Understanding note

p6a is the tool-dispatch core everything else hangs off: `tools.Catalog` validates (`SchemaValidator`, `NormalizeArguments`, `RejectedProperties`) and dispatches (`Execute` vs `ExecuteOutput`, `executeRecovered`/`executeOutputRecovered` panic boundary, `Filter`/`MergeCatalogs`/`ResponseTools`/`ChangesState`/`SourceOf`) with `EntryMatches` (`native`+`extension` vs `mcp:<server>[:tool|*]`) deciding profile/hook/sub-agent visibility. `json_bounds` (`DraftJSONBytes`/`Depth`) is the model-draft measuring helper (used by `request_draft` outside phase); `output` is the stable text envelope (`RenderToolResult`, `contentToken`); `schema` is the compiled-validator wrapper (external refs rejected, Draft-7 default, AJV-shaped issues); `text`/`extension/text`/`mcp/content` hold parallel UTF-8/JSON helpers. `extension/decide_systemone` is HTTP-only (no process spawn): per-call env key read, `NormalizeArguments`/`parseDecideSystemoneInput/Questions`, `requestDecisions` (context timeout, `MaxResponseBytes` via `LimitReader`, no-redirect client, 422 `detail` parse, key redaction, 400-char one-line errors), `renderDecideSystemoneAnswers`. `mcp` is the untrusted-server boundary: `normalizeConfig`/`ValidateServerFile` (stdio vs `streamable_http`, `env`/`env_from`/`headers_from`, `timeout_ms`/`max_result_bytes`, fingerprint over transport+limits+resolved secrets), `Manager.LoadTools` (concurrent per-server loads, 8 workers, ordered results, first-failing-in-order error, per-server `serverLock`, fingerprint reconnect, `Close` eviction), `connection` (`callMu` + `RWMutex`, `structuredContentCapture` by JSON-RPC id, `listTools`/`callTool` timeouts), `toolsFromList` (deny, duplicate-name, object-schema, `schemaCache` 512-LRU, `exposedToolName` slug+10-hex), `content` (`boundedContent`/`maxResultBytes`, image/audio/resource mapping, `truncateUTF8`), `environment` (minimal inherit + `()` filter). Recent changes at HEAD: decide rename `a095d7e`, image-drop `9ce1892`, OpenAI-path removal `e7f7198`, configurable `Path` + 422 detail `b8a2967`; catalog only gained `RemoteHops` in `1063e32`; D-1 `206ebd5` added the catalog recover; P-006(a) schema cache, P-016 concurrent listing, P-034 lock-narrowing are present in this tree.

## 2. Hypotheses (assumed → verified/disproved)

- HYP-01 Assumed: MCP discovery still holds a global mutex across connect+list (A-6). → Disproved for global scope: `LoadTools` (mcp/manager.go:108-187) holds `m.mu` only for stale eviction, workers run `loadServer` outside it; `serverLock` is per-server (mcp/manager.go:268-271). Remainder below (per-server I/O under lock) kept as low/C-hypothesis.
- HYP-02 Assumed: panic recovery covers every tool path (D-1). → Partly disproved: `Execute`/`ExecuteOutput` covered (catalog.go:395-408), `NormalizeArguments`/marshal path is not (catalog.go:343-350). Filed P6A-R3-01.
- HYP-03 Assumed: decide endpoint/secrets/redirects/body-caps correct. → Verified for those paths (see §6); unbounded-questions remainder filed P6A-R3-02.
- HYP-04 Assumed: MCP server output fully bounded (size/schema/cache). → Partly disproved: result bytes bounded, schema bytes/cache-memory not. Filed P6A-R3-03.
- HYP-05 Assumed: stdio child lifetime/kill explicit in phase. → Unverified/disproved as in-phase guarantee: only `session.Close()` via SDK (manager.go:62-70,360-380), no `Process.Kill/Wait` in phase, no death assertion. Filed P6A-R3-04.
- HYP-06 Assumed: UTF-8/JSON helpers single-sourced. → Disproved: three cut helpers + two JSON helpers. Filed P6A-R3-06; panic edge filed P6A-R3-05.
- HYP-07 Assumed: `json_bounds` number handling exact. → Minor deviation verified (Inf→null). Filed P6A-R3-07.
- HYP-08 Assumed: tool-name collisions possible across sources. → Disproved: `NewCatalog`/`MergeCatalogs` reject duplicates (catalog.go:117-149), `exposedToolName` (manager.go:391-416) prefixes `mcp_` + digest, native/extension vs mcp disjoint. Sound (§6).

## 3. Findings

### P6A-R3-01 NormalizeArguments/marshal path sits outside the panic boundary
- severity: medium
- lens: A correctness
- location: backend-go/internal/tools/catalog.go:343-350 (NormalizeArguments + `json.Marshal`), boundary at backend-go/internal/tools/catalog.go:395-408 (`executeRecovered`, `executeOutputRecovered`, `recoverToolPanic`)
- evidence: [read] `ExecuteFunctionCall` (catalog.go:310-391, fetched via `get_code_snippet ...ExecuteFunctionCall`) calls `tool.NormalizeArguments(object)` and `json.Marshal` directly; only `tool.Execute`/`ExecuteOutput` go through `recoverToolPanic`. A panicking normalizer (nil-map write, bad assertion on odd model JSON) escapes `ExecuteFunctionCall` as a Go panic to the batch worker instead of a `tool_execution_failed` output. Prior art D-1/`206ebd5` fixed `Execute` only (verified `TestCatalogConvertsToolPanicsToToolOutputs` still passes, exit 0); this is the uncovered remainder. No test feeds a panicking `NormalizeArguments`.
- suggested fix: wrap normalization+marshal in the same recover (e.g. call a `normalizeRecovered` helper or move `NormalizeArguments` inside `executeRecovered` path) so any normalizer panic becomes `ExecutionError(StatusError,"tool_execution_failed",...)`; add a regression test with a panicking normalizer that must return a `function_call_output`, not crash the test binary.
- effort: S
- prior art: D-1 (audit_audit.md ACCEPT, fixed `206ebd5`) — remainder, new

### P6A-R3-02 decide_systemone bounds input bytes and question count but not question bytes
- severity: medium
- lens: B security
- location: backend-go/internal/tools/extension/decide_systemone.go:201-220 (`executeDecideSystemone`: `size` sums only `input` parts vs `MaxInputBytes`; `len(questions)` vs `MaxQuestions`), question parsing at backend-go/internal/tools/extension/decide_systemone.go:298-375 (`parseDecideSystemoneQuestions` trims but never measures)
- evidence: [read] `input` loop (lines 204-211) and count check (218-219) are the only pre-request guards; `instructions`/`choices[].value|description`/`levels[].label|description` strings are unbounded per question (schema requires `minLength:1` only). A model (untrusted caller of this boundary) can send `MaxQuestions` (default 16, max 128) × arbitrarily large strings; `systemoneRequest` (decide_systemone_backend.go:244-272) marshals them unbounded and `requestDecisions` POSTs them. No `MaxResponseBytes`-like cap on the request side. Reachable by the model in any run with the tool enabled.
- suggested fix: measure questions as sent (e.g. sum `len(instructions)+labels+descriptions` or compact-marshal `questions` and enforce against `MaxInputBytes` or a new `max_questions_bytes`), returning `input_too_large`/`invalid_question` before the HTTP call; cover with a test that oversized instructions are refused without a request.
- effort: S
- prior art: new (settings caps `timeout_ms`/`max_*` from `extension_decide_systemone_settings.go:1-132` cover counts/bytes for input+response only)

### P6A-R3-03 MCP tools/list payload and compiled-schema memory are unbounded
- severity: medium
- lens: B security
- location: backend-go/internal/tools/mcp/manager.go:192-215 (`loadServer`/`listTools` — no byte cap, only `context.WithTimeout`), backend-go/internal/tools/mcp/manager.go:236-241 (`marshalJSON(remote.InputSchema)` + `m.schemas.get(schema)` with no size check), backend-go/internal/tools/mcp/schema_cache.go:39-67 (`get` compiles any bytes, 512-entry LRU with no byte/memory bound)
- evidence: [read] `loadServer` bounds `listTools` by `current.timeout` only; `toolsFromList` rejects non-object schemas and invalid schemas but never checks `len(schema)`; `schemaCache.get` (fetched via `get_code_snippet`) hashes and compiles whatever the server sent, caching up to 512 validators. A malicious/buggy MCP server (untrusted boundary per checklist) can answer `tools/list` with e.g. a 10 MB `inputSchema` per tool × dozens of tools; each `LoadTools` (per run) compiles and retains them (512 × large = GBs). `maxResultBytes`/`MaxResponseBytes` bound call results/decide responses, not this path. `TestLoadToolsCompilesEachSchemaOnce`/`TestSchemaCache*` cover correctness/eviction, not size.
- suggested fix: cap `tools/list` bytes (e.g. `io.LimitReader`-style at SDK boundary or reject `len(schema) > N` before `CompileSchema`, reusing `maxResultBytes` or a new `max_schema_bytes`), and bound cache memory (byte cap + eviction, or refuse-to-cache oversize with per-use compile); add a test with an oversize schema that must fail config-style, not compile/cache.
- effort: M
- prior art: new (P-006(a) cached validators for CPU/allocs, did not add a size bound; A-6/P-034 cover latency/locks, not bytes)

### P6A-R3-04 stdio server lifetime relies on SDK Close with no in-phase kill/Wait or death assertion
- severity: low
- lens: A correctness
- location: backend-go/internal/tools/mcp/manager.go:300-310 (`exec.Command` + `CommandTransport{Command: cmd}`, `cmd` not retained), backend-go/internal/tools/mcp/manager.go:62-70 (`connection.close` → `session.Close()` only), backend-go/internal/tools/mcp/manager.go:360-380 (`Manager.Close` closes sessions, no `Process.Kill/Wait`)
- evidence: [read] `getConnection` builds `cmd` (`Dir`, `Env`, `args` — no shell, good) and hands it to the SDK; neither `connection` nor `Manager` stores `cmd.Process`, calls `Wait`, or asserts exit. If `ClientSession.Close()` does not kill/reap on this SDK version/platform, a replaced (fingerprint change, manager.go:285-310) or closed manager leaves an orphaned child holding fds/ports. `manager_lifecycle_test.go:50-134` asserts tool replacement/closure, never that the child PID is gone. Cannot rate above low without SDK-behavior proof; no network/real-credential probe attempted.
- suggested fix: retain the child handle (or add a `closeFunc` that kills after `session.Close()` with a grace + `Kill` + `Wait`), and extend `manager_lifecycle_test.go` to assert the fixture process exits after `Close`/fingerprint replacement.
- effort: M
- prior art: new (phase prompt lists process lifetime/kill as never-independently-reviewed; no prior-art id claims a kill guarantee)

### P6A-R3-05 CutText panics on negative maxBytes while sibling cutters guard
- severity: low
- lens: A correctness
- location: backend-go/internal/tools/text.go:4-11 (`CutText`: `end := int(maxBytes)` with no `<=0` guard, then `text[:end]`), cf. backend-go/internal/tools/extension/text.go:26-33 (`cutUTF8` returns early) and backend-go/internal/tools/mcp/content.go:241-253 (`truncateUTF8` returns `""` when `maxBytes <= 0`)
- evidence: [read] `CutText` checks `len(text) <= maxBytes` then slices; for `maxBytes < 0` the length check is false and `text[:-1]` panics (`slice bounds out of range`). In-tree callers pass validated non-negative caps today (`tool_smith_bridge.go:500` with `NestedResultMaxBytes`), so unreachable in normal use — hardening only. Inconsistency with the two guarded siblings confirms the missing guard is an oversight, not a contract.
- suggested fix: mirror siblings (`if maxBytes <= 0 { return "", maxBytes < 0 || len(text) > 0 }` or simply `return "", true` for `<=0` matching `truncateUTF8` semantics chosen) in `tools/text.go`; add the negative/zero table case to `text_test.go`.
- effort: S
- prior art: new

### P6A-R3-06 Parallel UTF-8-cut and JSON helpers (3 + 2) with mixed duties
- severity: low
- lens: D maintainability
- location: backend-go/internal/tools/text.go:4 (`CutText`), backend-go/internal/tools/extension/text.go:21-33 (`oneLine`, `cutUTF8`), backend-go/internal/tools/mcp/content.go:241-270 (`truncateUTF8`, `marshalJSON`/`unescapeJSONSeparators`), cf. backend-go/internal/tools/output.go:151-159 (`MarshalJSON`), backend-go/internal/tools/mcp/content.go:263-297 and backend-go/internal/tools/json_bounds.go:35-77 (`replaceJSONStringSeparators` vs `unescapeJSONSeparators`)
- evidence: [read] three byte-safe truncators do the same job with different edge handling (§P6A-R3-05); two `MarshalJSON` variants both `SetEscapeHTML(false)`+trim-newline but only the MCP one unescapes `\u2028/\u2029`, while `json_bounds.go` has its own separator replacer. No file in phase exceeds ~700 lines and no function exceeds ~103 lines (longest: `issuesForKind` schema.go, `LoadTools`/`getConnection` manager.go, `parseDecideSystemoneQuestions`), but the helpers' divergence already caused one bug class (§P6A-R3-05). Native package repeats `cutUTF8`/`oneLine` further (outside phase — cross-phase lead §5).
- suggested fix: keep one exported cutter (e.g. `tools.CutText` fixed per §P6A-R3-05) and one JSON helper with explicit separator policy, and call them from `extension`/`mcp`; no broad rewrite (only the 5 in-phase sites).
- effort: S
- prior art: new

### P6A-R3-07 json_bounds normalizes unrepresentable numbers to null, changing the measured value
- severity: low
- lens: D maintainability (correctness nuance, narrow)
- location: backend-go/internal/tools/json_bounds.go:79-122 (`normalizeJSONStringifyNumbers`), esp. 84-101 (`json.Number` → `ParseFloat` error path: `Inf→nil`, `0→0.0`, else error)
- evidence: [read] valid JSON `1e999` decodes as `json.Number("1e999")`; `ParseFloat` returns `+Inf` with an overflow error, which the code maps to `nil` (JSON `null`) before measuring/encoding. The byte count then measures `null`, not the sent text, and a draft containing such a number is measured as smaller than sent. `DraftJSONBytes`/`Depth` are the `request_draft` guard (outside phase), so impact is a guard-accuracy nuance, not a crash; no test covers overflow numbers (`json_bounds_test.go:8-36` covers normal/parse-then-stringify only).
- suggested fix: preserve the input on overflow (return the original `json.Number`/string form for measuring, or reject with an error like other parse failures) and add the `1e999`/`-1e999` table case.
- effort: S
- prior art: new

## 4. Performance hypotheses (unmeasured — for §6.3, one at a time, relative)

- H-01: MCP config re-read + normalize per run, mcp/manager.go:268-346
  workload: 1–8 servers × many runs (run start resolver.go:586, SessionTools, sub-agent toolsets), 5–50 tools
  benchmark: existing BenchmarkLoadTools, BenchmarkLoadToolsServers, BenchmarkConcurrentLoadTools (manager_*_benchmark_test.go), cold vs warm, 1 vs 4 servers
  expected: extra stat+read+parse per server per run; scales with servers, not tools
- H-02: toolsFromList per-tool marshal/hash/slug per run, mcp/manager.go:236-266 + mcp/manager.go:391-416 + mcp/schema_cache.go:39-67
  workload: 50-tool server, repeated runs
  benchmark: existing BenchmarkLoadTools/warm/50-tools (P-006 gate row)
  expected: scales with tools; hash+slug+marshal per tool even on cache hit
- H-03: ExecuteFunctionCall multi-parse per call, backend-go/internal/tools/catalog.go:322-355
  workload: 1 KB vs 100 KB model arguments × many calls per response
  benchmark: proposed new /tmp benchmark calling Catalog.ExecuteFunctionCall with 1 KB/100 KB args
  expected: 2–3 parses per call; scales with argument bytes
- H-04: boundedContent double-marshal per part, backend-go/internal/tools/mcp/content.go:143-190 + :255-270
  workload: large MCP result near maxResultBytes (100 KB–1 MB parts)
  benchmark: proposed new /tmp benchmark over boundedContent/marshalParts
  expected: 2× marshal per part; scales with result bytes
- H-05: per-server serverLock across connect I/O, backend-go/internal/tools/mcp/manager.go:269-346 + :348-357
  workload: N concurrent LoadTools sharing one manager+server (same session/profile)
  benchmark: existing BenchmarkConcurrentLoadTools (1/4/8 callers, 5 ms injected)
  expected: same-server callers serialize (~N×latency); distinct servers do not
- H-06: decide per-call clone/marshal/render, backend-go/internal/tools/extension/decide_systemone.go:120-199 + decide_systemone_backend.go:244-474
  workload: 1 vs 128 questions × options (max settings) against httptest stub
  benchmark: proposed new /tmp benchmark over newDecideSystemoneTool+Execute
  expected: scales with questions × options

## 5. Cross-phase leads (not filed, with file:line)

- `session/runner_tools.go` + `session/tool_batch.go` (outside p6a): `ExecuteFunctionCall` Go-error returns (`nil, fmt.Errorf`, catalog.go:336,340,349,354,362) vs tool-output returns — whether a decode/normalize/validate-internal error fails the run vs yields a `function_call_output` is decided by the caller; confirm the contract.
- `session/tool_batch.go:29-38` `tool_call_scheduling="ordered"` + `catalog.go:246-260` (`ChangesState`: every MCP tool counts as state-changing, `ReadOnly=false`): serializes MCP calls by default, which is why `connection.callMu` (manager.go:47-60) + single-`active` capture (structured_content_capture.go:24-76) never overlap today; any move to concurrent scheduling needs the capture-by-id redesign (P-035, recommend-no-action — do not change here).
- `internal/app/resolver.go:586,603` (`MergeCatalogs` native+MCP, then profile `Filter`): filter does not stop `LoadTools` from starting servers (DECISIONS notes); server-start cost on filtered/hidden tools is an app-lifecycle question, not a p6a defect.
- `internal/sessionfs/extension_decide_systemone_settings.go:47-132` path/base_url/env/timeout validation (p2b2): `BaseURL+Path` join (decide_systemone_backend.go:84) is safe given those checks; any `Path` bypass would be a p2b2 finding, not p6a.
- `internal/tools/native/*` repeats `cutUTF8`/`oneLine` (e.g. `native/skill.go:143-144`, `hooks_run_events.go:58`, `subagent_manager.go:376,688,909`): §P6A-R3-06 consolidation should span phases only with owner approval.
- MCP `CommandTransport` kill/`Wait` semantics live in `github.com/modelcontextprotocol/go-sdk` (outside repo): §P6A-R3-04 needs an SDK-behavior check before any kill change.

## 6. Checked and sound (one line each)

- D-1 for `Execute`/`ExecuteOutput`: `executeRecovered`/`executeOutputRecovered` + `recoverToolPanic` (catalog.go:395-408) convert panics to `tool_execution_failed`; `TestCatalogConvertsToolPanicsToToolOutputs` passes.
- `NewCatalog`/`MergeCatalogs` duplicate-`Name` rejection (catalog.go:117-149) + `Filter` copy semantics + `ResponseTools` strict:false contract; no native/extension/MCP collision by construction (`mcp_` prefix + digest).
- `EntryMatches`/`AnyEntryMatches` (entry.go:1-32): `native`+`extension` share native entries, `mcp:<server>:*` prefix-colon prevents `docs`/`docsx` confusion; hook/sub-agent/profile callers use the same helper.
- MCP concurrency fixes present: concurrent `LoadTools` workers with ordered results + first-failing-in-order error (manager.go:120-187); `m.mu` only for maps/stale eviction; per-server `serverLock` sharing verified by `TestLoadToolsConcurrentCallersShareOneConnectionPerServer` (passes).
- `structuredContentCapture` begin/finish + `byID` cleanup (structured_content_capture.go:28-76) safe under `callMu` (manager.go:47-60); list path ignores non-`tools/call`; timeout/late-read leaves no entry (finish scans by pointer).
- `toolsFromList` guards: nil/blank/duplicate remote names, `DenyTools`, object-schema-only, invalid-schema-as-config-error (manager.go:217-266); `exposedToolName` slug+10-hex digest, truncation before digest preserves uniqueness.
- `normalizeConfig`/`ValidateServerFile` (config.go:51-329): `stdio` command/args/cwd checks (`Stat` isDir at run, skipped at write time), `env` vs `env_from` overlap refused, `env_from`/`headers_from` secrets required at run only, `timeout_ms` 1–600000, `max_result_bytes` 512–16 MB, `streamable_http` URL https/http no-userinfo/no-fragment, fingerprint covers transport+limits+resolved secrets (rotation reconnects).
- `defaultEnvironment` minimal inherit + `()` filter (environment.go:1-45); `commandWithEnvironmentPath` PATH-scoped lookup (manager.go:452-468); `exec.Command` no shell (no injection); `environmentSlice` sorted (stable fingerprint).
- MCP HTTP: `newHeaderHTTPClient` (manager.go:470-528) adds headers only for endpoint origin, strips on cross-origin redirect, 10-redirect cap; `RoundTrip` double-checks origin — sound.
- decide HTTP: no-redirect client (decide_systemone.go:91-93, `ErrUseLastResponse`), `TestDecideSystemoneDoesNotFollowARedirectWithTheKey` passes; per-call `WithTimeout(TimeoutMS)` + `LimitReader(MaxResponseBytes+1)` + oversize error (backend 79-111); `BaseURL+Path` validated in settings; `Authorization: Bearer` + `Accept/Content-Type` only.
- decide secrets: key read per call via `environment()` (decide_systemone.go:79-83,201-232), never stored; `decideSystemoneRedact` on transport/HTTP errors (backend 61-66), one-line 400-char cut (`oneLine`+`cutUTF8`), `TestDecideSystemoneErrorTextIsOneBoundedLineAndNeverHoldsTheKey` passes; 422 `detail` (`decideSystemoneDetailText`, backend 147-175) bounded by same cut, `TestDecideSystemoneReportsA422DetailWithItsLocation` passes.
- decide validation order (decide_systemone.go:201-251): blank/null-JSON refused, duplicate question names/options refused, choice/score shape mismatches refused before any request; `TestDecideSystemoneRefuses*`/`InputLimit*` pass.
- `SchemaValidator` (schema.go:29-73): empty-schema refused, external refs rejected via `rejectExternalSchemaLoader`, Draft-7 default; `Validate` distinguishes decode-error vs issues; false-schema/type-union/conditional normalizers preserve AJV shape.
- `DraftJSONBytes`/`Depth` semantics (json_bounds.go:13-156): parse-then-stringify with `UseNumber`+`SetEscapeHTML(false)`+`\u2028/\u2029` handling matches `JSON.stringify` intent; `request_draft` (outside phase) enforces the caps.
- `RenderToolResult`/`AppendContent`/`contentToken` (output.go:32-172): 16-byte `crypto/rand` token with collision retry, `utf8`/`base64` encoding normalization; `valueText` newline quoting stable.
- `preserveStructuredContentOrder` (structured_content_order.go:1-145): duplicate-key last-wins with first-position + JS array-index sort, trailing-data rejected; `compactJSON` bounded by caller.
- `CutText` happy path (text.go:4-11) and `truncateUTF8`/`cutUTF8` happy paths correct for split runes; `boundedContent` notice path fits `min 512` cap.
- Prior-art re-checks: A-6 global-mutex/list-per-run now concurrent + schema-cached (P-006a/P-016/P-034 DONE at this HEAD); remaining per-server lock and config re-read kept as H-01/H-05, not re-filed as defects. P-035 (`callMu`+single-active) still by-design serial — not re-filed. A-4 `Timeout` field absence mitigated by per-call `WithTimeout` contexts in this phase.

## 7. Not covered and why

- `schema.go:300-515` conditional/false-schema helpers beyond spot read (time-boxed; core `Compile`/`Validate` paths covered, edge `normalizeFalseSchemaArrayIssues`/`addConditionalValidationIssues` only skimmed).
- Live MCP server round trips / real stdio process kill behavior (no network/real processes per rules; SDK internals outside phase; covered by `httptest`/fixture tests only).
- Full `decide_systemone_test.go` (719 lines) — representative redirect/timeout/422/limit tests run targeted (exit 0 each); not every table case re-read.
- Session/runner/tool-batch callers of `ExecuteFunctionCall` (outside phase by phasemap) — noted as leads only.
- Benchmarks/profiles/`-race` (forbidden to reviewers); all C items left as hypotheses in §4.
- `audit_findings/` other reviewers' files (independence rule — not opened).

