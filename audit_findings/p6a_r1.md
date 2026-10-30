# Phase p6a review: Tool catalog, MCP, extension (reviewer r1)

- HEAD: 0941b6a. Date: 2026-10-26.
- Files read (all non-test): `internal/tools/{catalog,entry,interaction,json_bounds,output,schema,text}.go` (schema.go lines 1-515 read in two parts), `internal/tools/extension/{decide_systemone,decide_systemone_backend,text}.go`, `internal/tools/mcp/{config,content,environment,manager,schema_cache,structured_content_capture,structured_content_order}.go`. Tests read as evidence: mcp `manager_concurrent_test.go`, `manager_lifecycle_test.go`, `schema_dialect_debug_test.go`; extension `decide_systemone_test.go` (test list + 259-275, 375-405); tools `catalog_test.go` (search hit only). Outside the phase, read for evidence only: `app/resolver.go` (582-590, 840-892, 1030-1050), `session/tool_batch.go:85-110`, `session/runner_tools.go:171-275`, `tools/native/tool_smith_bridge.go:55-80,216-240,460-470`, `tools/native/subagent_call.go:50-66`, `sessionfs/extension_decide_systemone_settings.go:80-120`, `sessionfs/native_tool_smith_settings.go:215-230`, `go-sdk@v1.8.0/mcp` (cmd.go, client.go, streamable.go, transport.go, event.go) for SDK limits.
- Navigation: `codebase-memory` search_graph / get_code_snippet / trace_path and `zvec-grep` were used for `catalog.go` (qualified names from search_graph), `DraftJSON*`, `executeRecovered`, `LoadTools`, `SourceOf`, `ResponseTools`, `AnyEntryMatches` callers. `search_graph` returned nothing for the mcp/extension files with a `file_pattern`+`max_degree` filter and `name_pattern` regex failed ("invalid regex"); so the mcp and extension files, the line ranges outside `catalog.go`, and the SDK were read with `exec` (`sed`/`cat -n` on exact files and ranges). Fallback used for those, stated here as required.
- Commands (cwd `backend-go/`): `go build ./internal/... ./cmd/...` exit 0; `go vet ./internal/tools/...` exit 0; `gofmt -l internal/tools` no output, exit 0; `go test ./internal/tools/ ./internal/tools/extension/ ./internal/tools/mcp/ -count=1` exit 0 (ok x3; the mcp package 7.5 s; I ran these three package suites once, not the full suite). Probes: seven throwaway `go test -overlay` files under `/tmp/r1/` (no file in the tree), all removed; `git status` shows no tracked-file change from me and no `zz_*` file in the tree. The two known flaky tests are in `tools/native`, outside this phase, not run.
- Prior art checked: `DECISIONS.md` decide_systemone section (451-466) and MCP lines 45, 312, 788; `audit_plan.md` section 1.1 table; A-6 / P-006 / P-016 / P-034 lines in `audit_design.md`, `audit_audit.md`, `audit_performance_plan.md`. `git log` for `catalog.go` and the decide_systemone range a095d7e..b8a2967 and `git show 1063e32 -- catalog.go` (remote change: one field `RemoteHops`, see P6A-r1-05).

## 1. Understanding

- `tools.Catalog` is immutable (`catalog.go:111-138`): `NewCatalog` rejects missing ID/name, nil validator, zero or two executors, and a duplicate exposed name; `MergeCatalogs` (142) re-runs that across native/extension/MCP, so name collisions fail the session load rather than shadow. MCP names are `mcp_<slug server>_<slug tool>` cut to 53 bytes plus a 10-hex SHA-256 of `server\0tool` (`manager.go:391-416`), so they cannot equal a native name (no native starts with `mcp_`).
- `ExecuteFunctionCall` (310-391): parse call, lookup, decode, `NormalizeArguments`, schema validate, then `Execute`/`ExecuteOutput` inside `recoverToolPanic` (D-1 holds for the executor, 395-409). Any executor error is replaced by a generic result; the error and panic value are dropped (P6A-r1-09).
- MCP `Manager` (`manager.go`): one per session+profile (`app/resolver.go:877`), `mu` guards maps only (A-6 lock part fixed in f5da718), per-server `serverLock` serialises connect, `loadServer`s run in <=8 workers. `connection.callMu` serialises calls per server; `connection.mu` (RW) is held read-side for the whole call so `close()` waits for it. Stdio servers get `defaultEnvironment` (6 names on unix) + `env`/`env_from` only; HTTP gets `headers_from` only on the same origin.
- Untrusted-output path: SDK decodes the frame (stdio limit 16 MiB, SSE event 16 MiB, JSON body unbounded), `structuredContentCapture` re-reads raw `structuredContent` to keep key order, `contentParts` converts, `boundedContent` applies `max_result_bytes` to the converted parts.
- `decide_systemone`: one HTTP POST with bearer key from the process env read per call, no redirects, `LimitReader` body cap, status mapping, 422 `detail` rendering, answers rendered as one JSON line.

## 2. Hypotheses

| id | statement | status |
|---|---|---|
| X1 | Panic in a tool is recovered (D-1) | verified for Execute/ExecuteOutput (`catalog.go:395-409`); NOT for NormalizeArguments/Validate/MayChangeFiles (covered only by `session/tool_batch.go:97` on the run path; callers `app/remote_api.go:221` and `tool_smith_bridge.go:468` have no recover around them, see leads) |
| X2 | Tool names can collide across sources | disproved (`NewCatalog:131`, hash suffix, `mcp_` prefix) |
| X3 | MCP server output is size bounded | partly: converted output yes (`boundedContent`), decode-time cost of structured content no (P6A-r1-01), tools/list size/count no (P6A-r1-06) |
| X4 | MCP `Close` returns promptly | disproved (P6A-r1-03, measured) |
| X5 | `tools/list` pagination handled | disproved (P6A-r1-02, measured) |
| X6 | decide_systemone never leaks key in results | disproved at the 400-byte cut (P6A-r1-08, measured) |
| X7 | decide_systemone follows redirects / unbounded body | disproved (`decide_systemone.go:91`, `backend.go:96`) |
| X8 | Schema validation errors are bounded | disproved (P6A-r1-04, measured) |
| X9 | Schema cache grows without bound | disproved (LRU 512 per Manager, `schema_cache.go:359`); one cache per manager, see P6A-r1-06 |
| X10 | MCP env inherits the whole process env | disproved (`environment.go:352-374`) |
| X11 | Cross-origin redirect leaks MCP headers | disproved (`manager.go:496-507` sets headers only for the configured origin) |
| X12 | `ExecutionContext` is propagated intact to nested calls | disproved (P6A-r1-05) |

## 3. Findings

### P6A-r1-01  Structured content from an MCP server costs O(depth x size) to re-encode; 1,000 levels and 1 MiB take 4.6 s
- severity: medium
- lens: C performance (also A: no bound before the work)
- location: `tools/mcp/structured_content_order.go:281-301` (`orderedJSONObject.MarshalJSON` calls `marshalJSON` per child, each of which re-copies and runs `unescapeJSONSeparators`, `content.go:263-298`); `content.go:106-112` (`compactJSON` of the whole value before `boundedContent` can cut it); `content.go:45-53`.
- evidence: [measured] throwaway `go test -overlay` test: object nested `{"a":{"a":...}}` with a 1 MiB string at the leaf, then `preserveStructuredContentOrder` + `compactJSON`. depth 100: parse 2.5 ms, marshal 447 ms; depth 1000: parse 3.9 ms, marshal 4.62 s; depth 5000: parse 8.0 ms, marshal 29.5 s (output about 1 MiB each). Every nested object marshals its child and then the parent copies it again, so each level re-copies the leaf. A frame of this size is below the SDK's 16 MiB limit (`transport.go:124`). The work runs in `executeTool` after the call returned, outside `callMu` but on the run's tool worker, with no context check. Reachable by: the configured MCP server (or a compromised one), through any tool result with `structuredContent`.
- suggested fix: encode the ordered tree once into one buffer (write keys/values recursively into a shared `bytes.Buffer`) instead of per-level `marshalJSON`; and/or refuse structured content larger than `max_result_bytes` or deeper than a small limit before ordering (`mcp/content.go`, `structured_content_order.go`).
- effort: S
- prior art: new

### P6A-r1-02  `tools/list` pagination is ignored: tools after the first page silently do not exist
- severity: medium
- lens: A correctness
- location: `tools/mcp/manager.go:198` (`listTools` -> `session.ListTools(ctx, nil)` and `NextCursor` never read); `manager.go:38-45`, `manager.go:214`.
- evidence: [measured] throwaway test with a Go MCP server `ServerOptions{PageSize: 2}` and 5 tools over Streamable HTTP: `LoadTools` returned 2 tools and `err == nil`. The SDK has `ClientSession.Tools` (`client.go:1551`) that follows cursors. The SDK server default page is 1000, so the gap shows only with servers that use a smaller page; the model gets no error and no warning, only a smaller toolset.
- suggested fix: loop on `NextCursor` in `connection.listTools` (cap pages, e.g. 100), within the same `listCtx`; or use `session.Tools`. `manager.go` only. Add a paging test beside `manager_concurrent_test.go`.
- effort: S
- prior art: new

### P6A-r1-03  `Manager.Close` and connection replacement block until an in-flight tool call returns (up to that server's `timeout_ms`, max 10 min)
- severity: medium
- lens: A concurrency/shutdown (the 176fd7d class: Close waits instead of cancelling)
- location: `tools/mcp/manager.go:47-70` (`callTool` holds `c.mu.RLock()` for the whole `session.CallTool`; `close()` needs `c.mu.Lock()`); callers `manager.go:134-136` (stale connections), `manager.go:295-297` (config changed, under `serverLock`), `manager.go:376-378` (`Close`); `app/resolver.go:1041-1043` (managers closed one after another).
- evidence: [measured] throwaway test: HTTP MCP server whose tool handler waits up to 5 s, config `timeout_ms` 20000; one call started on another goroutine, then `m.Close()`: "Close took 5.000644158s while a call was in flight". The call's own context is not cancelled by `Close` (it is derived from the run context in `executeTool`, `content.go:30`), so a hung server holds shutdown for `timeout_ms` per connection, sequentially across connections and sessions, and `getConnection` for the same server (which holds `serverLock`) stalls every other `LoadTools` for it. Stdio close then adds the SDK terminate sequence (5 s stdin close wait, SIGTERM, 5 s, SIGKILL; `cmd.go:68-110`).
- suggested fix: give `connection` a cancel for in-flight call contexts: `callTool` derives `ctx` from a per-connection context that `close()` cancels before taking `c.mu.Lock()`. `manager.go` only.
- effort: S
- prior art: new (D-3 covered the shutdown deadline, not this wait)

### P6A-r1-04  Schema-validation failure output is unbounded: 4 KB of bad arguments produce a 160 KB tool result
- severity: medium
- lens: A/C (model-supplied input amplified into the transcript and the next provider request)
- location: `tools/catalog.go:350-366` (all `issues` go into `SchemaError`); `tools/schema.go:73-87`, `collectValidationIssues` (112-140), `issuesForKind` (no cap); `tools/output.go:84-91`.
- evidence: [measured] throwaway test: schema `{a: array of integer}`, arguments `{"a":["s",...]}`: 1,000 items -> 4,007 B arguments -> 162,123 B output (5.3 ms); 20,000 -> 80 KB -> 3.27 MB (87 ms); 100,000 -> 400 KB -> 16.4 MB (424 ms). The result is persisted to `messages.jsonl` and re-sent every round, so one malformed call (a model error or a prompt-injected one) can fill the context or exceed provider limits. Reachable by: the model.
- suggested fix: keep the first N issues (for example 20) and add a final "N more omitted" entry, in `ExecuteFunctionCall` before `SchemaError` (`catalog.go:354-366`); or stop collecting in `Validate`.
- effort: S
- prior art: new

### P6A-r1-05  `ExecutionContext` is copied field by field; the tool_smith bridge drops `RemoteHops`, `AgentDepth` and `RootRun`
- severity: medium
- lens: B/D (boundary propagation; the struct has no copy helper)
- location: `tools/catalog.go:26-70` (struct; `RemoteHops` added in 1063e32 touching only this file among tools); `tools/native/tool_smith_bridge.go:224-236` (outside this phase) copies 11 fields and omits those three; second hand copy at `tools/native/subagent_call.go:54-60`.
- evidence: [read] `profile_message.go:355-358` and `transcript_remote.go:279,328` use `execution.RemoteHops`/`AgentDepth`; `transcript.go:189` refuses `AgentDepth > 0` when `allow_children` is false. `NativeToolSmithExcludedTools` (`sessionfs/native_tool_smith_settings.go:41`) excludes bash, exec, process, async, subagent, tool_smith, task only, so `profile_message`, `transcript` and `transcripts` are callable from a script. Inside a script the nested call therefore sees `AgentDepth 0` (child restrictions bypassed) and `RemoteHops 0` (so a remote-started run that uses tool_smith -> profile_message to a remote sends hops 1 again, which resets the loop guard enforced at `remoteapi/server.go:261`). Not reproduced; needs a configured remote and a script. Rated medium per the evidence rule.
- suggested fix: one `func (e ExecutionContext) ForNested() ExecutionContext` (or copy the struct and clear only the per-call fields) in `tools/catalog.go`, used by the bridge and subagent launch; add a test that every field is carried.
- effort: S
- prior art: new

### P6A-r1-06  MCP discovery and the connection lifetime are per session, with no TTL or idle close; descriptions and schemas from the server are unbounded
- severity: medium
- lens: C/B
- location: `tools/mcp/manager.go:105-215` (every `LoadTools` does a `tools/list` per server; `list_changed` is not used to invalidate); `manager.go:235-258` (description and schema copied as received, no length, count or total cap); lifetime `app/resolver.go:877-889` (managers keyed by session+profile are removed only on catalog change or resolver Close, `session_catalog_files.go:107-151`; no idle reaper found with `grep idle app/resolver.go`).
- evidence: [read] A-6 lock part is fixed (f5da718) and the CPU part was measured low (P-006); remaining: one `tools/list` RTT per server per run (also per `GET /tools`, `app/resolver.go:529`), and a stdio MCP process stays alive per session x server until shutdown. A server (or a remote HTTP server) can return any number of tools and 16 MiB descriptions; they go into `ResponseTools()` for every run and are model-facing text from an untrusted source (prompt-injection channel, token cost). Reachable by: the configured MCP server operator.
- suggested fix: cap per-tool description (e.g. 4 KB), schema bytes and tool count per server with a config error (`manager.go:toolsFromList`); consider caching the list until the SDK's tools-changed notification or a TTL, and an idle-close for managers. Measure first (hypotheses H1, H2).
- effort: M
- prior art: re-confirmed A-6 (remainder) / P-006 / P-034; size caps new

### P6A-r1-07  `max_result_bytes` keeps only the first part's text, and only if that part is text and first: an `isError` result loses its message
- severity: low
- lens: A
- location: `tools/mcp/content.go:143-174` (`len(bounded) == 0` condition at 153; `room := (maxBytes-used-noticeBytes-128)/6` at 154); `content.go:62-64` (the error marker is the first part).
- evidence: [measured] throwaway test with budget 2048: (a) `IsError:true` with a 3,000-byte text -> model gets `"MCP server reported a tool error."` and the omitted-notice only, the actual error text is gone; (b) `"hi"` + 3,000-byte text -> `"hi"` + notice (second part dropped whole); (c) one 1 MiB text with budget 1 MiB -> 174,868 encoded bytes kept (about 1/6, the divisor assumes worst-case `\u00XX` expansion). Only matters over the limit (default 1 MiB, min 512) so it is narrow.
- suggested fix: truncate the first part that does not fit when it is text (not only when `len(bounded)==0`) and size `room` from the actual encoded size (`partSize` of the cut text) instead of dividing by 6; `content.go`.
- effort: S
- prior art: new (if this is TypeScript parity, record it as a decision)

### P6A-r1-08  decide_systemone redacts the key after cutting the text, so a key straddling byte 400 leaks its prefix
- severity: low
- lens: B
- location: `tools/extension/decide_systemone_backend.go:104` (`decideSystemoneRedact(decideSystemoneErrorMessage(data), key)`), cut at 139-141.
- evidence: [measured] throwaway test: provider error body `{"error":{"message":"xxxx...(380 x) key=<key>"}}`; result tail `...key=sk-abcdefghijkl...`: the cut left the first 12 characters of a 39-character key, `ReplaceAll` finds no whole key. Needs a provider that echoes the key into a long error message (the code comment says "some providers echo", `backend.go:59-60`). The existing test (`decide_systemone_test.go:259-275`) has no key in its body, so it does not cover this.
- suggested fix: redact (and `oneLine`) before the 400-byte cut: pass `key` into `decideSystemoneErrorMessage`.
- effort: S
- prior art: new

### P6A-r1-09  The catalog's recover boundary covers only the executor, and the recovered panic is never logged
- severity: low
- lens: A/D
- location: `tools/catalog.go:381-389` (`executeErr`/`err` replaced by a generic result with no log), `catalog.go:395-409` (`recoverToolPanic` keeps no stack); `catalog.go:331-358` (`jsonschemaDecode`, `NormalizeArguments`, `Validate` outside recover); `CallMayChangeFiles` (259-286) calls `NormalizeArguments` and `MayChangeFiles`.
- evidence: [read] D-1 holds: `session/tool_batch.go:97` recovers the whole call on the run path and logs the stack (`tool_batch.go:10-20`), but `ExecuteFunctionCall` itself discards the tool's error and the panic value, so a failing or panicking tool leaves no log line at all (the test only asserts the text is not in the result, `catalog_test.go:173-203`). Direct callers `app/remote_api.go:221` (goroutine) and `tools/native/tool_smith_bridge.go:468` have no recover of their own for the pre-execution code.
- suggested fix: log the error and, for a panic, `debug.Stack()` at `slog` error level in `ExecuteFunctionCall` (as `toolBatchPanic` does); keep result text unchanged. `catalog.go`.
- effort: S
- prior art: D-1 (fixed for the run path); the log gap is new

### P6A-r1-10  Output escaping done in `mcp` and `json_bounds` is undone by the catalog's `json.Marshal` of a `RawMessage`
- severity: low
- lens: D (dead effort; unverified parity claim)
- location: `tools/mcp/content.go:263-298` (`SetEscapeHTML(false)` + `unescapeJSONSeparators` byte loop); `tools/output.go:152-158`; `tools/catalog.go:419-429` (`encodeFunctionCallOutputValue` -> `json.Marshal(struct{Output json.RawMessage})`).
- evidence: [read + stdlib demo] `json.Marshal` of a struct holding a `json.RawMessage` compacts it with HTML escaping on: demonstrated with `{"j": json.RawMessage({"x":"a<b&c"})}` -> `{"j":{"x":"a\u003cb\u0026c"}}` and the same applies to U+2028/2029. So the bytes written for an MCP result contain `\u003c` regardless of the MCP-side care. The value is JSON-equal, so only byte-level parity with the TypeScript form and the cost of the extra pass are affected. I did not run it through the catalog; confirm with one test before acting.
- suggested fix: either encode the envelope with an encoder that has `SetEscapeHTML(false)` in `encodeFunctionCallOutputValue` (and drop the mcp-side loop if so), or delete the mcp unescape pass and note that the form is `json.Marshal`'s. `catalog.go`, `mcp/content.go`.
- effort: S
- prior art: new

### P6A-r1-11  Duplicate text helpers: seven `cutUTF8` and two `isJSSpace`/`oneLine`, plus `firstNonEmpty` x4
- severity: low
- lens: D
- location: `tools/text.go:4` (`CutText`), `tools/extension/text.go:10,21,26`, `tools/native/skill.go:123,139,144` (identical logic), `tools/mcp/content.go:241` (`truncateUTF8`, different algorithm), `asyncmgr/types.go:192`, `remoteapi/server.go:608`, `webui/hosted_tool.go:204`; `firstNonEmpty`: `tools/extension/decide_systemone_backend.go:274`, `openaiauth/token.go:170`, `claudeauth/token.go:168`, `remote/client.go:275`.
- evidence: grep of `func cutUTF8|func CutText|func truncateUTF8|func oneLine|func isJSSpace|func firstNonEmpty` over `internal/` (non-test). `CutText` and `extension.cutUTF8` index `text[end]` and panic for a negative `maxBytes` (unreachable today: callers pass 400 and a validated setting).
- suggested fix: export `tools.CutText` and `tools.OneLine` and use them in extension and native (3 uses, so past the rule of three); leave the others for their phases.
- effort: S
- prior art: new

### P6A-r1-12  decide_systemone: the `max_input_bytes` limit and the "as sent" comment do not match the wire bytes for JSON parts
- severity: low
- lens: D/A
- location: `tools/extension/decide_systemone.go:286-296` (compact, HTML escaping off), `decide_systemone.go:72-78` (`size`), `decide_systemone_backend.go:80` (`json.Marshal` of the request).
- evidence: [measured] a JSON part `{"a":"x<y&z>"}` is `{"a":"x<y&z>"}` in the part but `{"a":"x\u003cy\u0026z\u003e"}` on the wire (`json.Marshal` re-escapes a `RawMessage`; same mechanism as P6A-r1-10, which is the catalog-side cause). `DECISIONS.md` says the limit counts the JSON "as sent". The gap is at most 6x for `<>&`-dense JSON and the request is still bounded by the provider, so narrow.
- suggested fix: encode the request with `SetEscapeHTML(false)` (`backend.go:80`), which also makes the comment true.
- effort: S
- prior art: new

### P6A-r1-13  A grandchild of a stdio MCP server survives shutdown; one child per session x server stays up
- severity: low
- lens: A
- location: `tools/mcp/manager.go:306-309` (`exec.Command`, no process group), SDK `cmd.go:92-101` (SIGTERM/SIGKILL go to the direct child only).
- evidence: [read] a config such as `npx`/`sh -c "..."`/`uvx` starts a wrapper; killing the wrapper leaves the real server. No `Setpgid` anywhere in `tools/mcp`. Not reproduced.
- suggested fix: set `cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}` and kill the group (needs a small wrapper around the transport's close), unix only; weigh against Windows support (`environment.go` has a windows table).
- effort: M
- prior art: new

### P6A-r1-14  Test gaps and quality
- severity: low
- lens: D
- location: `tools/mcp/schema_dialect_debug_test.go` (tests the third-party library, name says debug); `tools/mcp/manager_concurrent_test.go:258-285` (goroutine count `<= before+2` poll, sensitive to other tests); `tools/extension/decide_systemone_test.go:259-275` (name promises "NeverHoldsTheKey", body never contains the key); `tools/mcp/schema_cache.go:67-70` (`len()` used only by `schema_cache_test.go:78`).
- evidence: [read] no test exists for: pagination (P6A-r1-02), `Close` during a call (P6A-r1-03), deep/large `structuredContent` (P6A-r1-01), `isError` text over the limit (P6A-r1-07), key echo at the cut (P6A-r1-08), many schema issues (P6A-r1-04). b8a2967's 422 test exists (`:692`); the new commits a095d7e..b8a2967 do have failure-path tests otherwise.
- suggested fix: add the six tests above when fixing; rename or drop the dialect test.
- effort: S
- prior art: new

### Hardening, unreachable (no reachable path today)
- `tools/output.go:77-82,91-97`: `ExecutionError`/`SchemaError` return `""` when rendering fails (only `crypto/rand` or encoder failure), which gives an empty `function_call_output`.
- `tools/text.go:4-12`: `CutText` panics for negative `maxBytes`.
- `sessionfs/extension_decide_systemone_settings.go:87-90`: `base_url` accepts `http://` (key sent in clear) and userinfo/query; config-only, the file is the operator's.
- SDK `streamable.go:2490` reads an `application/json` response with `io.ReadAll` (no cap); `max_result_bytes` limits the model-facing text, not memory. Outside this repo.

## 4. Performance hypotheses (for measurement)

- H1: `tools/list` per server per run, `mcp/manager.go:105-215`, `app/resolver.go:586`.
  workload: 5 remote HTTP servers, 50 ms RTT, a run every 10 s plus `GET /tools` polls; stdio: 0 RTT but decode+compile per run (P-006 already measured 0.5-2.7 ms CPU).
  benchmark: `BenchmarkLoadToolsServers` (`manager_servers_benchmark_test.go`) with injected latency; add a variant with a list-changed-invalidated cache.
  expected: RTT x ceil(servers/8) saved per run; CPU saved is small.
- H2: schema compile for tools of a large server, `mcp/manager.go:239` via the 512-entry LRU.
  workload: one server with 1,000 tools of 5 KB schemas, new `Manager` per session (cache is per manager, `manager.go:95`), 50 sessions.
  benchmark: `BenchmarkLoadTools` with a 1,000-tool fixture, cold per manager vs a package-level cache.
  expected: compile cost x sessions; memory 512 compiled schemas per manager.
- H3: arguments are parsed 5-6 times per call, `tools/catalog.go:310-391`: `Unmarshal` fields, `json.Valid`, `jsonschemaDecode`, `json.Marshal` (normalized), `Validate` (`json.Valid` + `jsonschema.UnmarshalJSON`), plus `RawArguments` copy at 330, and `IsUserDecisionCall`/`ChangesState`/`CallMayChangeFiles`/`SourceOf` re-unmarshal the call (`catalog.go:221-305`, called from `session/runner_tools.go:177-215,250`).
  workload: `write`/`edit` calls with 1 MB content, 8 calls per response.
  benchmark: new `BenchmarkExecuteFunctionCall` with a 1 MB string argument and a validator for `{content: string}`, plus the 4 predicate methods per call.
  expected: allocation and time scale with 6x argument bytes; decode once and reuse would remove about half.
- H4: MCP result path copies: `content.go:29-55` -> `contentParts` -> `imagePart` base64 -> `partSize` (full `marshalJSON` with the `unescapeJSONSeparators` byte loop) -> `marshalParts` -> catalog `json.Valid` (`catalog.go:379`) -> `json.Marshal` compaction (`catalog.go:419`).
  workload: tool returning five 3 MB PNGs (`max_result_bytes` 16 MiB).
  benchmark: new `BenchmarkExecuteToolImageResult` in `mcp` over the stdio fixture; report allocs/op and ns/op.
  expected: about 5 full passes over the base64 bytes; `partSize` could use the encoded length arithmetic.
- H5: structured content is decoded by the SDK, copied three times (`structured_content_capture.go:95,112,59`) and re-parsed with `json.Decoder.Token` (`structured_content_order.go:180`), then re-encoded (P6A-r1-01).
  workload: 2 MB structured result, depth 3 and depth 200.
  benchmark: new `BenchmarkPreserveStructuredContentOrder` at depth 3/50/200/1000 with a 1 MB leaf.
  expected: depth-linear time per level (measured 0.45 s at 100, 4.6 s at 1000 for 1 MiB).
- H6: `json_bounds.go:13-34`: decode (`any`), a normalized copy, encode, `TrimSuffix`, `[]byte`, replace loop, `string` back: 6 copies of the draft.
  workload: `request_draft` and `/api/.../interaction` bodies up to `max_draft_bytes` (several MB).
  benchmark: new `BenchmarkDraftJSONBytes` on 1 MB and 8 MB objects; compare with a single streaming size pass.
  expected: roughly 6 x the draft in allocations per call.
- H7: `callMu` serialises all calls to one MCP server and the wait is not cancellable (`manager.go:48`).
  workload: 8 parallel calls to one server with 200 ms handlers, then cancel the run.
  benchmark: `BenchmarkParallelToolCalls` (exists, `manager_calls_benchmark_test.go`); add a cancel-latency test.
  expected: latency N x handler time; cancel returns only when the queue drains to the cancelled call's turn.

## 5. Cross-phase leads

- `tools/native/tool_smith_bridge.go:224-236`: the nested `ExecutionContext` omits `RemoteHops`, `AgentDepth`, `RootRun` (P6A-r1-05).
- `app/remote_api.go:221`: `go func(){ catalog.ExecuteFunctionCall(...) }` has no `recover` of its own; the catalog covers the executor only (P6A-r1-09).
- `app/resolver.go:877-889` and `session_catalog_files.go:107-151`: MCP managers live as long as the session entry; no idle close (P6A-r1-06).
- `app/catalog_files.go:81` + `tools/mcp/config.go:62-68`: a catalog write of an `mcp/*.json` file defines a command (`transport.command`/`args`/`cwd`), `env_from` (any backend env variable to the child) and `headers_from` (any env variable to a chosen URL); `ValidateServerFile` checks shape only. So `PUT /api/catalog/mcp/...` is code execution and secret-read capability for whoever holds that token; review in the httpapi/auth phase.
- `app/resolver.go:529` (`ListTools`): an authenticated `GET` on the tools route can spawn stdio MCP servers via `LoadTools`.
- `sessionfs/native_tool_smith_settings.go:41`: `profile_message` (and the remote `transcript` reads) are callable from scripts.
- `sessionfs/extension_decide_systemone_settings.go:87-90`: see hardening note.

## 6. Checked and sound

- Name collisions: `catalog.go:131`, `MergeCatalogs`; MCP names unique per `(server, tool)` with hash (`manager.go:391`); a tool of a denied name is skipped before dedupe (`manager.go:228`), duplicate remote names refused (232).
- Stdio env: `defaultEnvironment` whitelists 6 (unix) names, drops values starting `()` (shell functions), `env_from` is required set at run, `env` and `env_from` cannot overlap (`config.go:173-177`); fingerprints hash secrets and are not logged. Command resolved against the configured PATH (`manager.go:452-468`).
- HTTP MCP headers only on the configured origin and stripped on cross-origin redirect (`manager.go:470-507`); `url.User` and fragments refused (`config.go:206`).
- Connection state machine: replacement under `serverLock`, `closed` flags re-checked after connect (`manager.go:337-343`), no lock-order inversion (`callMu` -> `mu.RLock`; `close` takes only `mu`); `LoadTools` workers are joined (`wg.Wait`) so no goroutine outlives the call; earlier test `TestLoadToolsLeavesNoGoroutinesBehind` passes. Stale-session probe: a dropped HTTP session (404) failed that run's list once and the next run reconnected (measured, 4 runs).
- Structured-content capture: calls are serialised by `callMu`, so `active` is unambiguous; entries removed in `finish`; late response after a timeout finds no entry.
- Schema cache: SHA-256 key, failures not cached, LRU bounded at 512 (`schema_cache.go:337-365`); external `$ref` rejected (measured: compile error), regex lookahead rejected, `$schema` 2020 honoured.
- Schema validation of huge numbers (`1e9999999`, multipleOf, integer) returned in microseconds, no allocation blow-up (measured).
- decide_systemone: no redirect (`decide_systemone.go:91`), body capped with `LimitReader(max+1)` (`backend.go:96`), timeout from settings, key from env per call, `path` constrained in settings, answers never invent a 0 (`unusable`/`missing`), score clamped (`nearestDecideSystemoneLevel`, measured huge score -> last level), error classification as documented, 422 detail and plain string handled, unknown choice refused.
- `entry.go` matching: `mcp:<server>:*` uses `prefix+":"`, so `docs` does not cover `docsx`.
- `output.go`: content token regenerated if the content contains the end marker (`contentToken`), UTF-8 cut in `CutText` correct for positive limits.
- Build, vet, gofmt clean; the three package suites pass.

## 7. Not covered

- `tools/schema.go` error-mapping functions (lines 112-515) were read for flow, not compared case by case with AJV output; `schema_test.go`/`schema_messages_test.go` not read.
- `interaction.go` is type declarations only; its consumers (broker, httpapi) are other phases.
- SDK internals beyond the limits I cite (HTTP transport reconnect/retry behaviour, `MaxRetries`) were not reviewed.
- No stdio-process shutdown test (grandchild survival) was run; P6A-r1-13 is [read].
- No benchmark was run (rule); H1-H7 are unmeasured except the measured data quoted in P6A-r1-01/04/07.
- `decide_systemone_test.go` was read by test names and three snippets, not in full (719 lines).
- Windows-specific MCP paths (`environment.go` windows table, `commandWithEnvironmentPath` with `PATHEXT`) not exercised.
