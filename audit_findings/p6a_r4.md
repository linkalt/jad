# Phase 6a audit review — r4

- Phase: p6a; reviewer: r4; HEAD: `0941b6a`.
- Files reviewed: all 17 scoped non-test Go files: `backend-go/internal/tools/{catalog.go,entry.go,interaction.go,json_bounds.go,output.go,schema.go,text.go}`, `backend-go/internal/tools/extension/{decide_systemone_backend.go,decide_systemone.go,text.go}`, and `backend-go/internal/tools/mcp/{config.go,content.go,environment.go,manager.go,schema_cache.go,structured_content_capture.go,structured_content_order.go}`. Context: phase prompt, audit plan, CLAUDE/skill, DECISIONS.md, specified prior audits, and scoped git history.
- Commands/checks with exit codes:
  - `git rev-parse --short HEAD` — exit 0 (`0941b6a`).
  - `go test ./internal/tools/extension -run '^TestR4' -count=1 -overlay /tmp/r4/overlay.json` — exit 0.
  - Initial `go test ./internal/tools/mcp -run '^TestR4' -count=1 -overlay /tmp/r4/overlay.json` structured-content number probe — exit 0. Subsequent attempts with the MCP close probe failed: one scratch-probe build exit 1 (missing imports), then two probe runs exit 1 because the test did not reach its blocked initialize request. These are harness/probe failures, not product-test failures.
  - Dedicated `go test ./internal/tools/mcp -run '^TestR4ManagerCloseDoesNotCancelPendingConnect$' -count=1 -overlay /tmp/r4/mcp_overlay.json` — still active at report completion (>8 minutes), no exit code or result.
  - `read.sh backend-go/internal/tools/mcp/environment.go 1 22` fallback — exit 0 after both indexes did not return the declaration.
  - `go build ./...`, `go vet ./internal/tools/...`, `gofmt -l internal/tools`, full suite, and benchmarks — not run.
- Date: 2026-10-29.

## 1. Understanding note
Reviewed all 17 scoped non-test Go files plus the requested context/prior-art material. Tool definitions flow through Catalog validation and execution; the MCP manager loads configured peers, discovers tools and maps results; decide_systemone parses model-supplied questions and sends provider requests. Findings below are based on indexed traces/snippets and are marked `[read]`; the two completed overlay probes provide limited targeted evidence. The `environment.go` declaration was the sole source fallback (`read.sh` exact lines 1–22) after both indexes returned nothing useful. No source or git state was changed.

## 2. Hypotheses
- H1 — Catalog validation and executor panic boundaries reject collisions and recover panics: **verified**; no finding.
- H2 — MCP discovery/result handling and lifecycle bound remote work and preserve returned data: **partly disproved** by findings P6A-R4-01, 03, 04, 05, 08, and 09.
- H3 — decide_systemone bounds provider requests and safely validates/redacts provider responses: **partly disproved** by findings P6A-R4-02, 06, and 07.
- H4 — No performance candidates exist until traced: **disproved**; four unmeasured hypotheses are listed in section 4. No benchmark was run.

## 3. Findings

### P6A-R4-09 — MCP discovery has no application-level budget for returned tool definitions or schemas
- **Severity:** Medium
- **Lens:** B/C — remote-server boundary and resource use
- **Evidence:** [read] `backend-go/internal/tools/mcp/manager.go:192-215` accepts the `ListTools` response and passes its entire `Tools` slice to `toolsFromList`. `toolsFromList` at `manager.go:217-266` sizes maps/slices from the remote tool count, marshals each remote schema, compiles it through the cache, and retains each description/schema in the returned tools; it applies no total-count, description-byte, or schema-byte budget. The cache at `schema_cache.go:39-67` bounds entry count only, not the size of an entry or work before insertion.
- **Impact:** A malformed or compromised configured MCP server can return an unusually large list or schema and force memory/CPU proportional to the discovery payload before the tool catalog is usable. The call timeout does not cap bytes or amount of schema compilation.
- **Suggested fix:** Enforce response bytes, total tool count, and per-schema/description size budgets before validation and compilation; include a test for an oversized list/schema.

### P6A-R4-08 — MCP HTTP client follows cross-origin redirects, enabling server-directed SSRF
- **Severity:** Medium
- **Lens:** B — remote-peer boundary and SSRF
- **Evidence:** [read] `backend-go/internal/tools/mcp/manager.go:470-488` installs a redirect callback that removes configured headers when the target is not same-origin, but returns `nil` and follows the redirect anyway (up to ten hops). The `RoundTrip` wrapper at `manager.go:496-505` likewise controls header attachment, not destination access.
- **Impact:** A configured or compromised MCP HTTP peer can redirect the backend's requests to loopback/private-network destinations. Removing bearer/custom headers mitigates credential forwarding but does not prevent the backend from making the cross-origin request; this creates a server-directed SSRF path from an MCP response.
- **Suggested fix:** Reject cross-origin redirects (or restrict them to an explicit trusted-origin allowlist) and test redirects to a private/loopback destination.

### P6A-R4-07 — decide_systemone accepts out-of-range provider probabilities as valid answers
- **Severity:** Medium
- **Lens:** A — correctness of provider-result validation
- **Evidence:** [read] `backend-go/internal/tools/extension/decide_systemone_backend.go:301-318` stores any decoded `noul` number and marks it present. `decideSystemoneAnswerFits` at `decide_systemone_backend.go:398-411` checks only presence/type for non-choice answers; `renderDecideSystemoneAnswers` at `decide_systemone_backend.go:352-393` emits the value as `probability`, rounded but not range-checked (`roundDecideSystemone` at line 427). In the same path, `Confidence` is decoded to a plain `float64` with no presence flag (`decide_systemone_backend.go:301-318`) and valid choice/score answers always emit it, so a missing confidence becomes a reported `0`.
- **Impact:** A malformed or compromised provider response such as `noul: 1.5` or `noul: -0.2` is reported as a valid probability even though probabilities must be in [0,1]; likewise a missing confidence is presented as an explicit zero. Both can mislead model decisions and users. Tests cover missing primary numbers and type mismatches but not probability ranges or omitted confidence (`decide_systemone_test.go:431-475,495-510`).
- **Suggested fix:** Validate probability/confidence semantics and presence before rendering; reject or explicitly clamp invalid values, and test out-of-range values and omitted confidence.

### P6A-R4-06 — decide_systemone truncates provider error text before redacting the API key
- **Severity:** Medium
- **Lens:** B — secret handling across the provider boundary
- **Evidence:** [read] `backend-go/internal/tools/extension/decide_systemone_backend.go:116-143` reduces the extracted provider error to 400 bytes plus `...`. `requestDecisions` at `decide_systemone_backend.go:101` then calls `decideSystemoneRedact` on that already-truncated string; `decideSystemoneRedact` at `decide_systemone_backend.go:61-66` replaces only a complete key occurrence.
- **Impact:** If an upstream error echoes the API key across the 400-byte cutoff, truncation leaves a key prefix in the text and the later whole-key replacement cannot match it. The prefix is returned in the tool error, contrary to the intended no-key-leak behavior. Existing key-echo checks do not place the key across the cutoff (`decide_systemone_test.go:259-275,431-475`).
- **Suggested fix:** Redact before truncating and add a test with the key straddling the byte limit.

### P6A-R4-02 — decide_systemone has no byte budget for its question/option payload
- **Severity:** Medium
- **Lens:** B/C — model-input boundary and resource usage
- **Evidence:** [read] `backend-go/internal/tools/extension/decide_systemone.go:302-373` accepts each question field and appends every supplied choice/level without a per-field, option-count, or aggregate-byte limit. `executeDecideSystemone` at `decide_systemone.go:207-225` measures only `input` parts and checks `MaxQuestions` only after fully parsing questions. `systemoneRequest` copies all question/option data into criteria (`decide_systemone_backend.go:244-272`), and `requestDecisions` marshals the full request before sending it (`decide_systemone_backend.go:79-88`).
- **Impact:** A model-supplied call can consume memory/CPU parsing and marshaling arbitrarily large instructions, option labels, or descriptions and can send an oversized request to the configured provider, despite the configured `MaxInputBytes` limit.
- **Suggested fix:** Apply a total serialized question-payload byte budget and per-question option-count/field-length limits before constructing the outbound request.

### P6A-R4-05 — Structured-content reordering loses precision in large JSON numbers
- **Severity:** Medium
- **Lens:** A — correctness
- **Evidence:** [read] `backend-go/internal/tools/mcp/structured_content_order.go:38-45` reads scalar values with `json.Decoder.Token()` without `UseNumber`; JSON numbers therefore become `float64`. `orderedJSONObject.MarshalJSON` at `structured_content_order.go:124-145` re-marshals those values, and `executeTool` at `content.go:46-55` uses this reordered value in the result.
- **Impact:** Valid JSON integers above the exact `float64` range (for example `9007199254740993`) can be emitted as a different number (`9007199254740992`), silently corrupting MCP structured data.
- **Suggested fix:** Preserve numeric tokens (for example, use `Decoder.UseNumber`) while ordering keys, and add a round-trip test for a large integer.

### P6A-R4-03 — `max_result_bytes` limits output only after an MCP response has been decoded and expanded
- **Severity:** Medium
- **Lens:** B/C — untrusted boundary and resource usage
- **Evidence:** [read] `backend-go/internal/tools/mcp/content.go:29-55` waits for `current.callTool` to return the complete result before calling `boundedContent`. `contentParts` at `content.go:57-117` converts every block and serializes structured content; `imagePart`/`filePart` at `content.go:199-207` base64-encode full payloads. Only then does `boundedContent` at `content.go:143-174` measure and truncate parts. The structured-content wrapper also copies response data in `structured_content_capture.go:78-113`.
- **Impact:** A configured small output limit does not bound response bytes received, decoded, copied, parsed, or base64-expanded. A buggy or compromised configured MCP server can therefore impose memory and CPU proportional to a much larger response before the result is truncated.
- **Suggested fix:** Enforce a response/frame byte ceiling at the HTTP/stdio transport boundary and avoid copying or expanding content until it is within the processing budget.

### P6A-R4-04 — MCP tool discovery ignores paginated tools/list cursors
- **Severity:** Medium
- **Lens:** A — correctness
- **Evidence:** [read] `backend-go/internal/tools/mcp/manager.go:38-45` issues one `c.session.ListTools(ctx, nil)` request. `loadServer` at `manager.go:192-215` passes only that response's `listed.Tools` to `toolsFromList`; there is no cursor loop or subsequent list request.
- **Impact:** An MCP server that returns tools across multiple `tools/list` pages exposes only the first page to the catalog, silently making later tools unavailable.
- **Suggested fix:** Follow each returned continuation cursor until exhausted (while bounding total pages/tools), and test a server with at least two pages.

### P6A-R4-01 — MCP manager shutdown does not cancel or await an in-flight connection attempt
- **Severity:** Medium
- **Lens:** A — correctness and concurrency
- **Evidence:** [read] `backend-go/internal/tools/mcp/manager.go:318-345` starts `client.Connect` using a context derived from the load caller and timeout, then checks `m.closed` only after `Connect` returns (and closes the late connection only at that point). `Manager.Close` at `manager.go:360-379` marks the manager closed and closes only connections already in `m.connections`; it tracks neither pending connections nor their contexts.
- **Impact:** If `Close` races a slow MCP `Connect` while its caller context remains live, `Close` returns while the connection attempt (including a stdio child or HTTP initialization) can continue until its own context ends. This makes manager shutdown non-quiescent and delays stopping a server started during shutdown.
- **Suggested fix:** Track and cancel pending connection contexts, and have `Close` wait for pending connection work to settle before returning.

## 4. Performance hypotheses for measurement

- H01: MCP result work and intermediate allocations scale with the complete remote response before `max_result_bytes` is applied, including base64 expansion. Location: `backend-go/internal/tools/mcp/content.go:29-55,57-117,143-174`; `structured_content_capture.go:78-113`.
  workload: 16 concurrent tool calls, each returning an 8 MiB text result or 6 MiB binary result while configured for a 1 KiB output cap.
  benchmark: proposed `BenchmarkExecuteToolLargeResult`, using an in-process fake MCP server that returns fixed text/binary fixtures; run parallel calls and record `b.ReportAllocs` and elapsed time.
  expected: transient memory/allocations scale with response size and concurrency despite the fixed output cap; binary content adds roughly 4/3 expansion during base64 conversion. **Unmeasured.**
- H02: Tool discovery/schema compilation cost scales with the total remote tool/schema payload; count-only schema-cache limits do not cap per-entry work. Location: `backend-go/internal/tools/mcp/manager.go:192-266`; `schema_cache.go:39-67`.
  workload: one server with 500 tools and 16 KiB valid input schemas per tool (about 8 MiB of schema text), repeated for cold-cache discovery.
  benchmark: proposed `BenchmarkLoadServerLargeToolCatalog`, backed by an in-process fake `ListTools` server and generated schema fixtures; record allocations and CPU time.
  expected: discovery allocations and CPU scale with tools/schema bytes, with a single large schema able to dominate its cache entry. **Unmeasured.**
- H03: decide_systemone question parsing, copying, and request marshaling scale with unbounded question-field bytes, independently of `MaxInputBytes`. Location: `backend-go/internal/tools/extension/decide_systemone.go:302-373`; `decide_systemone_backend.go:79-88,244-272`.
  workload: one question with an 8 MiB instruction field and a small ordinary `input` field, at `MaxInputBytes=1`.
  benchmark: proposed `BenchmarkDecideSystemoneLargeQuestion` with an in-process fake provider `RoundTripper`, generated input fixture, and allocation reporting; no external network.
  expected: parse/copy/marshal allocations and outbound body size grow linearly with question payload bytes even though the ordinary input budget is small. **Unmeasured.**
- H04: prior-art A-6—if the outer run lifecycle constructs/loads MCP discovery per run, repeated `tools/list` and schema work may dominate run startup. Location: `backend-go/internal/tools/mcp/manager.go:192-266`.
  workload: compare 100 simulated runs with 8 configured servers and 200 tools each, contrasting fresh-manager-per-run with a reused manager.
  benchmark: proposed `BenchmarkMCPLoadToolsAcrossRuns` using a fake MCP server; first verify the actual manager lifecycle at the out-of-scope caller.
  expected: with fresh per-run discovery, startup work scales with runs × servers × tool/schema payload; manager reuse should amortize connection/discovery work if supported by the caller. **Unmeasured; caller lifecycle not independently verified.**

## 5. Cross-phase leads

- Existing open performance item A-6 (per-run MCP discovery) remains a measurement/architecture lead, not a new finding. Its frequency depends on the outer run caller, which is outside p6a; verify that lifecycle before treating repeated discovery as a confirmed cost. Relevant in-scope location: `backend-go/internal/tools/mcp/manager.go:192-266`.
- No other cross-phase defect was confirmed; no out-of-scope production code was inspected for this report.

## 6. Checked and sound

- Catalog construction rejects duplicate model-facing names and validates definitions/executors; `MergeCatalogs` and `Filter` rebuild through the same constructor.
- Catalog function-call execution and output execution each have panic-recovery boundaries.
- Schema compilation rejects external references and runtime validation uses the compiled schema.
- MCP configuration validation/defaults and environment allowlisting were reviewed; existing registered connections are closed by `Manager.Close` (pending connects are the separate finding P6A-R4-01).
- The schema cache is bounded by entry count; the lack of a per-entry/schema-discovery budget is separately reported as P6A-R4-09.

## 7. Not covered and why

- No full Go test suite was run (prohibited by the phase prompt). `go build ./...`, `go vet ./internal/tools/...`, and `gofmt -l internal/tools` were not run because they were not needed for this read-only audit.
- No benchmark was run; section 4 lists workload-specific hypotheses only.
- No live external MCP peer, real provider credentials, or actual private/loopback redirect target was used. Redirect behavior was reviewed statically; runtime SSRF behavior was not exercised.
- MCP close-probe execution is unresolved: initial temporary harness attempts either failed to build (missing scratch imports) or failed to reach the blocked initialize request; the later dedicated probe remained active with no exit code. This is not reported as a passing test or as independent reproduction.
- Outer run lifecycle/callers that determine whether MCP discovery repeats per run were not inspected; this is the A-6 cross-phase lead above.
