# Phase p6a review — reviewer r2

- phase: p6a (Tool catalog, MCP, extension)
- reviewer: r2
- HEAD at review: `0941b6a` (worktree clean for the phase files; `git status` not used, read-only)
- files read (non-test): `internal/tools/{catalog.go,entry.go,interaction.go,json_bounds.go,output.go,schema.go,text.go}`, `internal/tools/extension/{decide_systemone.go,decide_systemone_backend.go,text.go}`, `internal/tools/mcp/{config.go,content.go,environment.go,manager.go,schema_cache.go,structured_content_capture.go,structured_content_order.go}` (all 7 + 3 + 7 files of the scope, 3,899 lines)
- tests read as evidence: `internal/tools/*_test.go` (names + selected bodies), `internal/tools/mcp/*_test.go`, `internal/tools/extension/decide_systemone_test.go` (names)
- outside the phase, read only to trace: `internal/session/runner.go:425-470`, `internal/app/resolver.go:983-1043`, `internal/app/service.go:110-128`, `cmd/jad/main.go:85-130`, `internal/httpapi/interaction_handlers.go:145-190`, `internal/tools/native/request_draft.go:55-80`, plus SDK sources in `$GOMODCACHE/github.com/modelcontextprotocol/go-sdk@v1.8.0/mcp/{streamable.go,event.go,transport.go,cmd.go}` (cited per finding)
- navigation: `codebase-memory` `get_architecture`, `search_graph`, `get_code_snippet`, `trace_path`, and `zvec-grep` for orientation; `exec sed -n` was used for exact line ranges and for non-Go files (SDK sources, DECISIONS.md) — stated here as the required fallback.
- commands run (from `backend-go/`): `go build ./internal/... ./cmd/...` → exit 0; `go vet ./internal/tools/...` → exit 0; `gofmt -l internal/tools` → no output, exit 0; `go test ./internal/tools/ ./internal/tools/extension/ ./internal/tools/mcp/ -count=1` → `ok` for all three, exit 0 (2.5 s); two probe programs under `/tmp/r2/` (`go run`, stdlib only) for the number/key-encoding claims, removed at the end.

## 1. Understanding note

The phase is the model-facing tool layer:

- `internal/tools` is the contract: `Catalog` (immutable after `NewCatalog`, `byName` dispatch, duplicate-name rejection), `ExecuteFunctionCall` (call JSON → schema validation with JSON-Schema-to-AJV error shaping → `Execute`/`ExecuteOutput` under a panic recover → `function_call_output` envelope), `MergeCatalogs`/`Filter` for the profile tool filter, `EntryMatches` for `tools.allow`/`deny` entries, `RenderToolResult`/`SchemaError`/`ExecutionError` for the stable text envelope, `json_bounds.go` for the `request_draft` display limits (bytes as `JSON.stringify` would count, container depth).
- `internal/tools/extension` is one tool: `decide_systemone`, a read-only, provider-API tool (`Execute`, not `ExecuteOutput`). `NormalizeArguments` accepts `noul`/`boolean`/case spellings before validation; the request is one POST to `base_url + path` with a bearer key read from the environment at call time, redirects refused, response capped; answers are re-rendered as one compact JSON line with `missing`/`refused`/`unusable` statuses instead of a zero.
- `internal/tools/mcp` is the MCP client: one `Manager` per (setting, profile). `LoadTools` diffs the enabled server set, loads servers concurrently (≤ 8) under **per-server** locks, reuses a connection while its config fingerprint is unchanged, and lists tools on every call. Each `tools/call` is serialised on `connection.callMu`, bounded by the server's `timeout_ms`, converted to `outputPart`s bounded by `max_result_bytes`, with `structuredContent` re-ordered from the wire (JSON-RPC-id correlation through a transport wrapper) so object-key order survives. Schema compilation is cached by schema SHA-256 in a 512-entry LRU.
- Lifecycle: `Manager.Close` flips `closed`, empties the map and closes connections; `LoadTools`/`getConnection` re-check `closed` under `mu`, so no connection is published after `Close`. No goroutine outlives `LoadTools` (`wg.Wait`), every context has a deadline and a `cancel`.

## 2. Hypotheses list

| id | statement | status |
|---|---|---|
| H1 | MCP discovery is serialised behind one manager-wide lock (A-6) | **disproved** — `Manager.mu` guards only maps/`closed`; `getConnection` runs under `serverLock(id)` and the loop uses ≤ 8 workers (P-034/D-4 fix verified, manager.go:108-187,268-346) |
| H2 | MCP output can be unbounded before the result limit applies | **verified** (finding p6a-r2-01) |
| H3 | MCP structured-content ordering loses number fidelity / fails the call | **verified** (finding p6a-r2-02) |
| H4 | Extension renders an answer key with a non-JSON escaper | **verified** (finding p6a-r2-03) |
| H5 | Tool panic escapes the recover boundary (D-1) | **disproved** for `Execute`/`ExecuteOutput` (catalog.go:395-409, test `TestCatalogConvertsToolPanicsToToolOutputs`); the diagnostic loss is finding p6a-r2-05 |
| H6 | `Manager.Close` can block on a hung stdio server | **verified** (finding p6a-r2-04) |
| H7 | Tool name collisions across native/extension/MCP are silent | **disproved** — `NewCatalog` rejects duplicates (catalog.go:131-133); MCP names carry a 10-hex digest of server+tool (manager.go:391-416) |
| H8 | The schema cache grows without bound | **disproved** — LRU bounded at 512, failures not cached (schema_cache.go:11-37,39-67) |
| H9 | The structured-content capture map leaks entries for unanswered calls | **disproved** — `finish` removes every `byID` entry pointing at the finished call, and only one call is active at a time (`callMu`) (structured_content_capture.go:39-60) |
| H10 | Extension tools spawn a process (env inheritance / cwd / kill) | **disproved for this phase** — `internal/tools/extension` makes one HTTP request; no `os/exec`. Process spawn in the phase is MCP stdio only (manager.go:288-302, `commandWithEnvironmentPath` 452-468, allowlist env `environment.go:5-19`) |
| H11 | Panic paths exist in the schema/validation helpers called outside the recover | **unverified/negative probe** — `kind.UniqueItems.Duplicates` is a `[2]int` (SDK `schema.go:76`), so schema.go:473-479 cannot index out of range; no other unchecked index found |
| H12 | `ExecutionCall.RemoteHops` (remote commit `1063e32`) is dropped on the child path | **disproved** — `internal/tools/native/subagent_call.go:59` forwards `execution.RemoteHops`; the catalog change was a struct field only |

## 3. Findings

### p6a-r2-01  MCP result size is only limited after the whole result is in memory; the streamable-HTTP path has no cap at all
- severity: medium
- lens: B security (untrusted MCP server output)
- location: `backend-go/internal/tools/mcp/manager.go:311-315` (`mcpSDK.StreamableClientTransport` created without `MaxEventSize`), `backend-go/internal/tools/mcp/manager.go:470-488` (`newHeaderHTTPClient`, no body limit), `backend-go/internal/tools/mcp/content.go:143-174` (`boundedContent`, the only size guard), `backend-go/internal/tools/mcp/content.go:57-117` (`contentParts`, builds every part first), `content.go:199-202` (`imagePart` re-base64-encodes)
- evidence: [read] `max_result_bytes` (512 B … 16 MiB, config.go:27-29) is applied to the *converted* `outputPart` list, never to the wire bytes: `executeTool` (content.go:29-55) calls `boundedContent(result, current.maxResultBytes)` only after the SDK has decoded the whole response and `contentParts` has built one part per content block. Per transport, verified in the vendored SDK:
  - stdio: capped — `newIOConn` → `newIOConnLimited(rwc, DefaultMaxLineLength)` with `DefaultMaxLineLength = 16 MiB` (`transport.go:124,493-503`, `cmd.go:46`).
  - streamable HTTP: **uncapped** on both read paths — a plain JSON answer is `io.ReadAll(resp.Body)` with no limit (`streamable.go:2489-2491`), and the SSE path calls `scanEventsLimited(resp.Body, c.maxEventSize)` where `maxEventSize` is the transport option backend-go leaves at zero, and the SDK documents `maxEventSize <= 0` as "disables the cap" (`streamable.go:2088,2623`, `event.go:71-76,125-130`).
  A misbehaving or compromised `streamable_http` server therefore makes the backend allocate a response of its choosing (a 1 GiB body becomes `io.ReadAll`'s 1 GiB plus the decoded `CallToolResult`), long before anything checks 16 MiB. Who can reach it: anyone who can write an `mcp_servers` file or run the server that file names (a local user, or a third-party server trusted by config). Even on stdio the peak is ~2.7× the limit for one base64 image block (16 MiB base64 → `[]byte` → `imagePart` re-encodes it 4/3 larger) and the parts slice is unbounded in *count* until the bound loop drops them.
- suggested fix: (a) set `MaxEventSize: config.maxResultBytes` on `StreamableClientTransport` (one line, manager.go:311-315); (b) wrap `HTTPClient`'s transport with a `RoundTripper` that wraps `resp.Body` in an `io.LimitReader` of `maxResultBytes + slack` (and fails on exceeding it), next to `headerRoundTripper`; (c) optionally pass `MaxLineLength` on stdio too so the frame cap matches the configured limit. Do not add `http.Client.Timeout` (that half of B-S1 was correctly rejected — audit_audit.md:24).
- effort: S–M
- prior art: `audit_design1.md` B-S1 second bullet ("add request/response size caps … enforce at HTTP layer too") — the timeout half was CORRECTed in `audit_audit.md:24`, the size half was never implemented and is now shown uncapped with SDK citations; new evidence, so not a repeat.

### p6a-r2-02  `structuredContent` is re-decoded through `float64`, so a valid number can fail the whole tool call
- severity: low
- lens: A correctness
- location: `backend-go/internal/tools/mcp/structured_content_order.go:22-36,38-90` (`preserveStructuredContentOrder`, `readOrderedJSONValue` use `decoder.Token()` without `UseNumber()`), consumed at `backend-go/internal/tools/mcp/content.go:45-52`
- evidence: [read] + probe. `json.Decoder.Token()` yields `float64` for every number; the values are re-encoded by `orderedJSONObject.MarshalJSON` (structured_content_order.go:126-146). A probe in `/tmp/r2/t.go` replicating the decode loop verbatim:
  `{"id":9007199254740993}` → `{"id":9007199254740992}`; `{"big":123456789012345678901234567890}` → `{"big":1.2345678901234568e+29}`; **`{"n":1.0,"m":1e400}` → error `json: cannot unmarshal number 1e400 into Go value of type float64`**.
  The in-range cases match what `JSON.stringify(JSON.parse(x))` does (JS also loses the integer), which is what the function's comment targets — so the precision part is intended. The overflow case is not: JavaScript yields `Infinity` for `1e400` (would print `null`), while here the error propagates out of `executeTool` (content.go:47-50), through `ExecuteFunctionCall` (catalog.go:374-378), and the model receives only `Tool <name> failed during execution` — for a tool result that was valid JSON on the wire. Any MCP server can trigger it by returning `{"m":1e400}` (e.g. a computed statistic).
- suggested fix: `decoder.UseNumber()` in `preserveStructuredContentOrder` and return the `json.Number` (re-marshalling then writes the original literal, and no error path); if JS fidelity matters, map an out-of-range literal to `nil` in `orderedJSONObject.MarshalJSON` instead of failing.
- effort: S
- prior art: new.

### p6a-r2-03  A question name with a control character makes the `decide_systemone` result invalid JSON
- severity: low
- lens: A correctness
- location: `backend-go/internal/tools/extension/decide_systemone_backend.go:461-472` (`marshalDecideSystemoneItem`, line 469 `fmt.Fprintf(&out, "%q:", key)`)
- evidence: [read] + probe. `%q` is Go quoting, not JSON quoting. Probe `/tmp/r2/q.go`: `fmt.Sprintf("{%q:1}", "a\x01b")` → `{"a\x01b":1}` → `json.Unmarshal` error `invalid escape sequence \x in string`; same for U+007F. The name comes straight from the model (`questions[].name`, schema `{"type":"string","minLength":1}` in decide_systemone.go:47, trimmed but not filtered by `normalizeDecideSystemoneArguments`). Effect: the one-line result the model is told to read is not parseable, and the name is echoed back only in that line; the request side is unaffected (`map[string]any` + `json.Marshal`). Sibling code in the same file already does it correctly: `orderedDecideSystemoneProbabilities.MarshalJSON` uses `json.Marshal(entry.Label)` (line 447-451).
- suggested fix: encode the key with `json.Marshal(key)` (or `tools.MarshalJSON`) instead of `%q`.
- effort: S
- prior art: new.

### p6a-r2-04  `Manager.Close` closes MCP connections serially, each able to block ~10 s on a hung stdio server
- severity: low
- lens: A correctness (shutdown that does not finish promptly)
- location: `backend-go/internal/tools/mcp/manager.go:360-380` (`Close`, sequential `for … current.close()`); blocking chain `manager.go:62-70` → SDK `cmd.go:66-113` (`pipeRWC.Close`: close stdin, wait 5 s, SIGTERM, wait 5 s, SIGKILL); called serially per manager at `internal/app/resolver.go:1041-1043`, from the unbounded `Service.Close` (`internal/app/service.go:126`, `cmd/jad/main.go:118`)
- evidence: [read] the SDK's `pipeRWC.Close` waits `terminateDuration` (default 5 s, `cmd.go:22-25,42-45`) after closing stdin, then SIGTERM plus another 5 s before SIGKILL. `Manager.Close` iterates the map one connection at a time, and `Resolver.Close` iterates managers one at a time, on the SIGINT path *before* `ShutdownHTTPServer`. So K stdio servers that ignore stdin EOF (node/python servers that do not exit) add up to 10 s × K to process exit; nothing bounds it ([unmeasured] — no hung-server fixture was run). No data loss and no goroutine leak; the cost is user-visible shutdown latency. Contrast the `176fd7d` class: here Close does wait, which is why it is slow.
- suggested fix: close connections concurrently (`sync.WaitGroup` over the slice in `Manager.Close`), and/or bound the whole close with one context/deadline in `Resolver.Close`.
- effort: S
- prior art: new (D-3 in `audit_audit.md` covered the *deadline on run shutdown*, not this serial loop).

### p6a-r2-05  A recovered tool panic loses every diagnostic
- severity: low
- lens: D maintainability (silent failure) / A
- location: `backend-go/internal/tools/catalog.go:371-384` (both branches discard the error), `catalog.go:395-409` (`executeRecovered`/`recoverToolPanic`)
- evidence: [read] `executeRecovered` returns `fmt.Errorf("tool panicked: %v", recovered)`; `ExecuteFunctionCall` overwrites the result with `ExecutionError(..., "tool_execution_failed", "Tool <name> failed during execution")` and never logs, returns or records the error value (lines 374-378 for `ExecuteOutput`, 381-384 for `Execute`). The comment at catalog.go:392-394 states a panic would otherwise end the process, so the recover is doing the containment job (verified by `TestCatalogConvertsToolPanicsToToolOutputs`) — but after it fires, an operator has no evidence that a tool panicked, unlike every other failure in the phase, which is at least named to the model. Reachable by any tool bug (native, extension, MCP conversion).
- suggested fix: keep the generic model-facing result, but log the recovered error once (the packages around `tools` already have a logger; if `tools` must stay logger-free, return the error instead of a canned string and let the runner log it).
- effort: S
- prior art: new.

### p6a-r2-06  Three copies of each small JSON/UTF-8 helper across the three packages
- severity: low
- lens: D maintainability
- location: cut-to-bytes: `internal/tools/text.go:4-14` (`CutText`), `internal/tools/extension/text.go:26-34` (`cutUTF8`), `internal/tools/mcp/content.go:241-254` (`truncateUTF8`); marshalling without HTML escaping: `internal/tools/output.go:151-158` (`MarshalJSON`), `internal/tools/mcp/content.go:263-272` (`marshalJSON`), `internal/tools/extension/decide_systemone.go:288-296` (`marshalDecideSystemoneJSON`); `\u2028`/`\u2029` un-escaping: `internal/tools/json_bounds.go:35-77` (`replaceJSONStringSeparators`, string-aware) vs `internal/tools/mcp/content.go:274-297` (`unescapeJSONSeparators`, whole-document)
- evidence: [read] the three `cut*` functions differ only in signature (`int64` vs `int`) and in the byte test (`0xc0 == 0x80` vs `utf8.RuneStart`); the three marshallers are byte-identical in body. `extension` already imports `tools`, and `mcp` imports `tools`, so all three share one home (`internal/tools`). Effect: a fix to one (the mcp variant's separator pass, say) does not reach the others; `truncateUTF8` additionally re-validates the whole slice per iteration. The two separator implementations are not equivalent by construction (one tracks string state), which invites a future divergence.
- suggested fix: keep `tools.CutText` and `tools.MarshalJSON`; delete `extension.cutUTF8`, `extension.marshalDecideSystemoneJSON`, `mcp.truncateUTF8`, `mcp.marshalJSON`, and rewrite `mcp.unescapeJSONSeparators` as a call to the `json_bounds.go` variant (or move that helper to `output.go` and give it a name that says "inside strings").
- effort: S
- prior art: new.

## 4. Performance hypotheses for measurement (section 5.3)

- H01: MCP `tools/list` is still paid on every `LoadTools`, i.e. every run start and every `GET …/tools`, even when the connection fingerprint is unchanged. `backend-go/internal/tools/mcp/manager.go:108-215` (A-6 remainder; the connect is now reused, the list is not, and no `notifications/tools/list_changed` handling exists).
  workload: 5 and 50 tools per server, 1–8 servers, one `prepareRun` per message and one `SessionTools` poll per UI refresh.
  benchmark: existing `BenchmarkLoadTools` (manager_benchmark_test.go) and `BenchmarkLoadToolsServers` (manager_servers_benchmark_test.go); add a `warm` sub-case after a connection exists.
  expected: scales with (runs × servers); P-006 measured the list decode as the dominant allocation (0.54 ms / 5 tools, 2.7 ms / 50 tools) — the remaining cost is the round trip and the JSON decode of the tool list.
- H02: the model's argument JSON is parsed two to three times per call. `backend-go/internal/tools/catalog.go:341-357` (`jsonschemaDecode` then `Validator.Validate` re-decodes the same bytes, schema.go:71-73), and `CallMayChangeFiles` (catalog.go:259-286) re-parses and re-runs `NormalizeArguments` for the same call.
  workload: a `write`/`edit` call carrying 20–200 KB of `content`, 5 calls per response; also decide_systemone with a 200 KB input.
  benchmark: existing `BenchmarkSchemaValidatorValidate`; propose `BenchmarkExecuteFunctionCall/{validate-only,execute}` with a 100 KB string property.
  expected: linear in argument bytes, ×2–3; matters only for large arguments, and the second parse in `CallMayChangeFiles` is per call of a subagent-like tool.
- H03: each MCP result is encoded several times before it is stored. `backend-go/internal/tools/mcp/content.go:187-197` (`partSize` marshals every part once for the limit check), `content.go:263-272` + `274-297` (`marshalJSON` encodes, then `unescapeJSONSeparators` copies the whole document byte by byte), `content.go:176-185` (`marshalParts` encodes again), plus `structured_content_order.go:22-36` re-parsing `structuredContent` and `orderedJSONObject.MarshalJSON` re-encoding it field by field.
  workload: 1 MiB MCP result (the default `max_result_bytes`), 5 parallel calls per response, 20 responses per run.
  benchmark: existing `BenchmarkParallelToolCalls` (manager_calls_benchmark_test.go), with a 1 MiB text+structured fixture.
  expected: ~3–4 copies of the result size per call and one per-byte pass; scales with result bytes, not with history.
- H04: all calls to one MCP server serialise on `connection.callMu` (`backend-go/internal/tools/mcp/manager.go:47-53`), so a response with N calls to a slow server costs N round trips. (Documented in `DECISIONS.md` as intentional for shared connections; listed as a hypothesis, not a defect.)
  workload: one server with 50 ms latency, N = 2, 4, 8 calls of read-only-looking MCP tools in one response.
  benchmark: existing `BenchmarkParallelToolCalls` with an injected per-call delay (as `BenchmarkConcurrentLoadTools` already does).
  expected: wall time linear in N per server; the ceiling is 1 in-flight call per server regardless of `tool_call_scheduling`.

## 5. Cross-phase leads (outside the phase; not counted as findings)

- `internal/httpapi/interaction_handlers.go:161` — error text `"draft the payload is %d bytes; the limit is %d"` (garbled; the sibling message at :167 reads correctly). The `tools.DraftJSONBytes`/`DraftJSONDepth` half is in phase and sound.
- `internal/tools/native/request_draft.go:63-77` — the same draft JSON is parsed twice (bytes, then depth) and both limits are checked only after the whole draft is already in memory. One pass could return both; the `MaxDraftBytes` guard cannot prevent the allocation it measures.
- `internal/sessionfs/extension_decide_systemone_settings.go` — the endpoint validation the extension relies on (`BaseURL + Path`, decide_systemone_backend.go:84) lives here, not in the phase. Worth a check in its own phase that `base_url` rejects a query/fragment/userinfo (`https://h/x?k=1` would otherwise be spliced with the path) and that plain `http://` is intended for non-loopback hosts.
- `internal/tools/native/*` — result truncation and spill sizing (the prompt's "output.go" item): `internal/tools/output.go` has no truncation logic at all; spill/limits are in `native` (`bash_child`, `read`), outside the phase.
- `internal/app/resolver.go:983-1043` — the serial manager-close loop that makes p6a-r2-04 user-visible; the same pattern applies to process/subagent/async managers.

## 6. Checked and sound (one line each)

- D-1 panic boundary: `Execute` and `ExecuteOutput` are both invoked through a `recover` (catalog.go:395-409) that sits on the batch-worker path; a panicking tool becomes a `tool_execution_failed` result instead of ending the process (`TestCatalogConvertsToolPanicsToToolOutputs`); validation/encoding paths hold no panic I could construct (H11).
- A-6 / P-034 / D-4: `Manager.mu` no longer covers connect or list; per-server locks plus `maxConcurrentServerLoads = 8`, results assembled in configured order, first failure in configured order returned (`TestLoadToolsReportsTheFirstFailingServerInConfiguredOrder`, `…KeepsConfiguredServerOrder`, `…ListsServersConcurrentlyWithinTheBound`).
- Tool-name collisions: `NewCatalog` rejects a duplicate exposed name, empty id/name, a missing validator and a double executor (catalog.go:117-138); an MCP name is `mcp_<slug>_<slug>_<10 hex of sha256(server\0tool)>` (manager.go:391-416), so two servers cannot collide.
- MCP size of the *tool list*: `toolsFromList` rejects an unnamed tool, a duplicate name and a non-object `inputSchema` (manager.go:217-266); `max_result_bytes` is enforced for the converted result with a UTF-8-safe cut and an explicit omission notice (content.go:143-174, `TestBoundedContentTruncatesUTF8AndIncludesOmissionNotice`).
- Schema cache: SHA-256 keyed LRU bounded at 512, one compile per distinct schema, failures not cached, one validator shared by concurrent callers (schema_cache.go:11-67, four tests).
- Structured-content capture: correlated by JSON-RPC id through a transport wrapper that observes before the SDK handles the message, one active call per connection (`callMu`), entries removed on `finish` and on response (structured_content_capture.go:1-157).
- MCP process spawn: argv passed as a slice (no shell), `cmd.Dir` = the server's validated cwd, env = a fixed allowlist (`HOME`, `PATH`, …) plus `transport.env`/`env_from`, with `env` and `env_from` forbidden to name the same variable (config.go:159-206, environment.go:5-19); secrets are never logged — `configError` names the variable, not its value; `safeErrorType` collapses failures to `Error`/`McpError` (manager.go:427-437).
- MCP HTTP boundary: endpoint must be http(s) with a host, no userinfo, no fragment (config.go:203-207); configured headers are applied only to the endpoint's own origin and are deleted on a cross-origin redirect (manager.go:470-528); `MaxConnsPerHost` is unbounded (noted in B-S1, Low, unchanged).
- `decide_systemone` boundary: redirects refused (`ErrUseLastResponse`, decide_systemone.go:91-96); response read through `io.LimitReader(MaxResponseBytes+1)` (backend.go:96-101); the key is redacted from body text, from wrapped transport errors and `Error()` (backend.go:45-91); 401/403/429/4xx are typed, and a 422 `detail` list is rendered as `loc: msg` (backend.go:113-207); arguments are checked (blank text, null JSON, duplicate name/option, choices-vs-levels) before any request; `MaxInputBytes`/`MaxQuestions` are read from settings; a missing key yields `api_key_missing` with no request.
- Read-only scheduling: `decide_systemone` sets `ReadOnly: true`, MCP tools do not (so they serialise), `ChangesState`/`CallMayChangeFiles`/`IsUserDecisionCall`/`SourceOf` behave as documented for unknown names (catalog.go:221-305).
- `EntryMatches`: a bare entry matches only `native`/`extension` (so a profile cannot name an MCP tool by its model-facing name), `mcp:<server>:*` matches on the `mcp:<server>:` prefix (so `zvec` does not cover `zvecx`), `mcp:` entries never match a native tool (entry.go:11-25).
- `json_bounds.go`: numbers are normalised the way `JSON.stringify` prints them, including `-0 → 0` and an out-of-range literal → `null`; depth counts containers, not scalars (schema-level checks are unaffected by either).
- Test quality for the new commits: the four `decide_systemone` commits carry tests for the configured path, the 422 detail, timeout, cancellation, redirect-with-key, key echo in a body and in a transport error, oversized response, missing key, `unusable` vs zero, and a clamped score — the failure paths the finding area needs; MCP has lifecycle, concurrency, first-error and no-goroutine-leak tests. Missing: a test for a number that overflows `float64` in structured content (p6a-r2-02), a test for a control character in a question name (p6a-r2-03), and a test that `Close` during `LoadTools` finishes promptly (p6a-r2-04).
- `go build`, `go vet`, `gofmt -l`, and the three package test runs are clean; no TODO/FIXME in the phase files.

## 7. Not covered, and why

- Benchmarks and `-race`: forbidden by the rules; every cost above is `[read]`/`[unmeasured]` and left to the measurement step.
- A hung stdio MCP server: no fixture was run, so p6a-r2-04 stays `[read]` (the SDK source is cited, the wall time is not measured).
- SDK internals beyond the lines cited for p6a-r2-01 (only the client read paths and `CommandTransport` were opened); the SDK's own `tools/list_changed` handling was not traced, so H01's "no cache" claim rests on the repo side (no such handling in `internal/tools/mcp`).
- Process-level consequences of the 16 MiB stdio frame cap exceeding `max_result_bytes` defaults are not measured.
- The settings half of `decide_systemone` (`internal/sessionfs/extension_*`) and the `sample/` schema: outside the phase (section 5).
- The runner's scheduling of batches (`internal/session/tool_batch.go`) was read only as far as needed to confirm the panic boundary sits on a worker goroutine and that `callMu` binds per server.
