# Phase p6a merged findings — the model-facing tool layer: `internal/tools`, `internal/tools/extension`, `internal/tools/mcp`

- phase: `p6a`, merged at HEAD `0941b6a` (go1.27.3 linux/amd64). Worktree before and after this merge: `M EXECUTOR.md`, `M MASTER.md`, `M audit_plan.md`, untracked `REVIEWER.md`, `REVIEWER4.md`, `audit_findings/`; `git diff --check` reports only one pre-existing trailing-whitespace line in the modified `EXECUTOR.md:21`, which was there before this step. **No repository file was changed** — no source, config, `sample/` or git state. The only repository file written is this one; probes lived in `/tmp/p6amerge` via `go test -overlay` and are removed at the end.
- inputs: `p6a_r1.md` (231 lines, 14 findings, 7 hypotheses), `p6a_r2.md` (150, 6, 4), `p6a_r3.md` (168, 7, 6), `p6a_r4.md` (126, 9, 4). Total **36 filings** and 21 raw performance hypotheses.
- scope (`wc -l`, non-test): `internal/tools/` 7 files/1,438 lines, `internal/tools/extension/` 3/886, `internal/tools/mcp/` 7/1,575 — 17 files, 3,899 lines. All four reviewers agree; no re-measurement was needed.
- **r4 left its close probe unresolved and ran no build/vet/gofmt** (`p6a_r4.md` §7, `/tmp/r4/notes.md`). Both are resolved here: `go build ./internal/... ./cmd/...`, `go vet ./internal/tools/...`, `gofmt -l internal/tools` and the three package suites are clean (§10), and the shutdown question r4 could not settle is answered by two probes of my own (P6AM-14 in §3, P6AM-15 in §4), which also decide how r4's `P6A-R4-01` must be re-rated.
- **Verification outcome: 36 filings → 22 merged entries; 10 reproduced by execution in this merge, 11 confirmed by reading source at `0941b6a`, 1 refuted outright (`p6a-r2-03`), 1 refuted as a defect while the fact holds (`P6A-R3-07`), 1 relocated out of the phase (`P6A-r1-05`, it is `tools/native` code).** 0 filings were dropped as prior art; 10 entries merge 23 filings. Seven claims are corrected (C1-C7) and four more framing disagreements are recorded (C8-C11).
- fixes and decisions assumed, each checked at this HEAD: `f5da718` + `e11b9f9` (P-016/P-034 concurrent listing, P-006(a) schema cache), `206ebd5` (D-1 executor recover), `1063e32` (`RemoteHops`), and the **user decisions** in `audit_performance_plan.md:420` (P-006: "cache compiled schemas only; no tool-list staleness") and `:757` (P-035: "no action").

## 1. Counts

| measure | count |
|---|---|
| reviewer findings filed | 36 (r1 14, r2 6, r3 7, r4 9) |
| merged entries | 22 — every filing carried, none dropped as prior art, none split |
| entries that merge several filings | 10, holding 23 filings: P6AM-08 (3), P6AM-13 (3), P6AM-16 (3), P6AM-01 (2), P6AM-03 (2), P6AM-04 (2), P6AM-05 (2), P6AM-09 (2), P6AM-10 (2), P6AM-15 (2). The other 12 entries hold one filing each. |
| consensus (2+ reviewers) | 10: P6AM-01 (r2, r4), P6AM-03 (r2, r4), P6AM-04 (r1, r4), P6AM-05 (r1, r2), P6AM-08 (r1, r3, r4), P6AM-09 (r3, r4), P6AM-10 (r1, r4), P6AM-13 (r1, r2, r3), P6AM-16 (r1, r2, r3), P6AM-19 (r1, r3) |
| unique (one reviewer) | 12: r1 6 (P6AM-02, P6AM-07, P6AM-14, P6AM-15, P6AM-21, P6AM-22), r2 1 (P6AM-20), r3 2 (P6AM-17, P6AM-18), r4 3 (P6AM-06, P6AM-11, P6AM-12) — P6AM-15 holds two r1 filings and stays a single-reviewer entry |
| severity as merged: critical / high / medium / low | 0 / 0 / **7** / 15 (12 low findings + 2 refuted-as-defect + 1 out-of-phase) |
| severity as filed (36 filings) | critical 0 / high 0 / medium **19** (r1 6, r2 1, r3 3, r4 9) / low 17 |
| the task brief said "r1 5 medium, r3 2 medium" | **both are one short**: r1 filed six mediums (01, 02, 03, 04, 05, 06) and r3 three (R3-01, R3-02, R3-03). Counted from the reports, not the brief. |
| mediums: kept / lowered / moved out of phase | 19 filed → **11 kept** (r1-01, r1-02, r1-03, r1-04, r1-06, r2-01, r3-03, r4-03, r4-04, r4-08, r4-09 — as merged: P6AM-01, 02, 04, 05, 07, 08, 12) / **7 lowered** (r3-01→P6AM-13, r3-02→P6AM-09, r4-01→P6AM-06, r4-02→P6AM-09, r4-05→P6AM-03, r4-06→P6AM-10, r4-07→P6AM-11) / **1 relocated out of the phase** (r1-05 → P6AM-22, `tools/native`) |
| r4's own severity column (9 × medium) | **4 kept medium** (R4-03, R4-04, R4-08, R4-09), **5 lowered** (R4-01, R4-02, R4-05, R4-06, R4-07), with the reason for each in §2 and C5 |
| verification: reproduced in this merge / read-confirmed / refuted / unverified | **10 executed** (P6AM-03, 05, 06, 07, 10, 11, 12, 14, 15, 17) + 1 executed refutation (P6AM-20) + 1 executed correction (C2) / **11 read-confirmed** (P6AM-01, 02, 04, 08, 09, 13, 16, 18, 19, 21, 22) / **2 refuted** (P6AM-18 as a defect, P6AM-20 outright) / **0 unverified** |
| every unique (12) and every medium (7) verified — 19 distinct entries | 19 verified: 16 confirmed as facts, P6AM-18 refuted as a *defect*, P6AM-20 refuted as a *fact*, P6AM-22 confirmed but outside the phase's scope. **0 left unverified.** |
| claims corrected | 7 (C1-C7), plus r4's severity column as a whole (C5) and r3's sibling-comparison in P6A-R3-05 (C2) |
| contradictions recorded | 11 (C1-C11) |
| performance hypotheses | 21 raw (r1 7, r2 4, r3 6, r4 4) → **7 groups** for the measurement step (§9); **none measured here** (the task forbids it; the only numbers quoted are the reviewers' own) |
| findings added by this merge | 0 |
| repeats of prior art | 5: P6AM-01 is the untaken size half of `audit_design1.md` B-S1; P6AM-08's "list per run / no TTL" half is A-6 + the **P-006(b) user decision** (no staleness); P6AM-13 is D-1's remainder; P6AM-19 / P6AM-05 are the A-4/D-3 neighbourhood. None is counted as a new class. |

## 2. Merged findings

`found by` lists the reviewer filings that map here. `sev` is the coordinator rating; `filed` is what the reviewers said, one letter per filing in the order of `found by`. Status: **R** = this merge executed a probe that reproduced it, **S** = confirmed by reading source at `0941b6a`. Locations are the ones verified in this merge (not quoted from the filings).

| id | title | sev | filed | lens | location | found by | status |
|---|---|---|---|---|---|---|---|
| P6AM-01 | `streamable_http` MCP response bodies are uncapped on both SDK read paths, and `max_result_bytes` is applied only after the whole result is decoded and expanded | medium | m,m | B/C | `tools/mcp/manager.go:312-315` (`StreamableClientTransport` built without `MaxEventSize`), SDK `streamable.go:2088` (copied raw, no 0→default resolution), SDK `event.go:73-75,125` (non-positive disables the cap), SDK `streamable.go:2490` (`io.ReadAll(resp.Body)`); cap applied at `mcp/content.go:143-174` after `contentParts` (`:57-117`) and `imagePart` base64 (`:199`) | r2-01, r4-03 | S |
| P6AM-02 | Structured content is re-encoded once per nesting level: O(depth × size), 1 MiB at depth 1,000 costs seconds on a tool worker | medium | m | C/A | `mcp/structured_content_order.go:124-146` (`orderedJSONObject.MarshalJSON` → `marshalJSON` per field), `mcp/content.go:263-298` (`marshalJSON` encodes into a buffer, then `unescapeJSONSeparators` copies the whole value again), `mcp/content.go:45-53` | r1-01 | S (r1 measured) |
| P6AM-03 | Structured content passes every number through `float64`: `1e400` fails the whole call, integers above 2^53 are silently rounded | low | l,m | A | `mcp/structured_content_order.go:38-41` (`decoder.Token()` without `UseNumber`), re-encoded at `:124-146`; failure path `content.go:45-49` → `catalog.go:374-378` | r2-02, r4-05 | R |
| P6AM-04 | `tools/list` pagination is ignored: tools after the first page do not exist | medium | m,m | A | `mcp/manager.go:38-45` (`session.ListTools(ctx, nil)`, single page), `:192-215` (`listed.Tools` only), `:214`; the SDK offers `ClientSession.Tools` (cursor iterator, `client.go:1555`) | r1-02, r4-04 | S |
| P6AM-05 | `Manager.Close` waits for an in-flight tool call (up to that server's `timeout_ms`, 10 min) and then closes connections serially (up to ~15 s each on a hung stdio server) | medium | m,l | A | `mcp/manager.go:47-60` (`callTool` holds `c.mu.RLock()` across `session.CallTool`), `:62-70` (`close` needs `c.mu.Lock()`), `:360-380` (serial loop); SDK `cmd.go:68-110` (5 s stdin wait, SIGTERM, 5 s, SIGKILL); caller `app/resolver.go:1034-1036` (serial over managers) | r1-03, r2-04 | R |
| P6AM-06 | `Manager.Close` does not stop a pending connect: it returns while the connect is still running, and a stdio child started in that window lives until `timeout_ms` | low | m | A | `mcp/manager.go:321-322` (`connectCtx` from the caller's ctx + config timeout), `:337-342` (`m.closed` checked only after `Connect` returns, the late session closed at `:340`), `:360-380` (Close tracks connections, not connects) | r4-01 | R |
| P6AM-07 | A schema-validation failure amplifies the arguments ~40×: 4 KB of bad arguments becomes a 162 KB tool result that is persisted and re-sent every round | medium | m | A/C | `tools/catalog.go:357-365` (all issues into `SchemaError`), `tools/schema.go:97-138` (`collectValidationIssues`, no cap), `:376` (`issuesForKind`) | r1-04 | R |
| P6AM-08 | MCP discovery has no budget for tool count, description bytes or schema bytes; the schema cache is bounded by entry count only and compiled schemas are retained per manager | medium | m,m,m | B/C | `mcp/manager.go:217-266` (description and schema copied as received; no count/byte check before `marshalJSON` + `m.schemas.get`), `mcp/schema_cache.go:39-67` (512-entry LRU, no byte bound); lifetime `app/resolver.go:844-889` (manager per session+profile, closed only at `Resolver.Close`) | r1-06, r3-03, r4-09 | S |
| P6AM-09 | `decide_systemone` bounds input bytes and question count but not question/option bytes | low | m,m | B/C | `extension/decide_systemone.go:206-221` (`input` parts vs `MaxInputBytes`; `len(questions)` vs `MaxQuestions`), `:302-373` (`parseDecideSystemoneQuestions`: trims, never measures; unbounded option count), request marshalled whole at `decide_systemone_backend.go:79-88,244-272` | r3-02, r4-02 | S |
| P6AM-10 | `decide_systemone` cuts the provider error text to 400 bytes *before* redacting the key, so a key across the cut leaks its prefix | low | l,m | B | `extension/decide_systemone_backend.go:104` (`decideSystemoneRedact(decideSystemoneErrorMessage(data), key)`), cut at `:139-141`, replace-all at `:61-66` | r1-08, r4-06 | R |
| P6AM-11 | `decide_systemone` reports a provider probability outside [0,1] as a valid answer and renders a missing `confidence` as an explicit 0 | low | m | A | `extension/decide_systemone_backend.go:299-308` (`Confidence float64`, no presence flag), `:398-411` (`decideSystemoneAnswerFits` checks presence/type only), `:352-393` (emits `probability`/`score`/`confidence` unrounded-range) | r4-07 | R |
| P6AM-12 | The MCP HTTP client follows a cross-origin redirect and only strips the configured headers — a configured server can make the backend POST to an arbitrary internal address | medium | m | B | `mcp/manager.go:470-488` (`CheckRedirect` deletes headers for a non-same-origin target and returns nil), `:496-507` (`RoundTrip` re-attaches headers only for the configured origin) | r4-08 | R |
| P6AM-13 | The recover boundary covers only the executor, the recovered panic and every executor error are dropped with no log line, and `NormalizeArguments`/`Validate` run outside any recover in this package | low | m,l,l | A/D | `tools/catalog.go:374-384` (both branches replace the error with a canned result, no log; the package imports no logger), `:395-409` (`executeRecovered`/`recoverToolPanic`), `:343-356` (normalize + marshal + validate outside it); run path has `session/tool_batch.go:96-106` (recover **with** `debug.Stack()`); uncovered caller `app/remote_api.go:220-229` (goroutine) | r3-01, r1-09, r2-05 | S |
| P6AM-14 | `max_result_bytes` keeps only a first *text* part and only when nothing else fitted: an `isError` result loses the provider's message; a second large part is dropped whole | low | l | A | `mcp/content.go:143-174` (`len(bounded) == 0` gate at `:153`, `room := (max-used-notice-128)/6` at `:154`), error marker first at `:62-64` | r1-07 | R |
| P6AM-15 | Escaping done inside `mcp`/`json_bounds` is undone by the catalog envelope's `json.Marshal`, and `max_input_bytes` therefore counts bytes that differ from the wire | low | l,l | D/A | `tools/catalog.go:419-429` (`encodeFunctionCallOutputValue` marshals a struct holding a `json.RawMessage`), `mcp/content.go:263-298`, `tools/output.go:151-158`; decide side `extension/decide_systemone_backend.go:79-88` (`json.Marshal` of the request) | r1-10, r1-12 | R |
| P6AM-16 | Three UTF-8 cutters, three "marshal without HTML escaping" helpers, two `\u2028/\u2029` replacers (not equivalent) and four `firstNonEmpty`, all in one phase | low | l,l,l | D | `tools/text.go:4` (`CutText`), `extension/text.go:21-34` (`oneLine`, `cutUTF8`), `mcp/content.go:241-254` (`truncateUTF8`); `tools/output.go:151-158` vs `mcp/content.go:263-272` vs `extension/decide_systemone.go:288-296`; `tools/json_bounds.go:35-77` (string-aware) vs `mcp/content.go:274-297` (whole document); `firstNonEmpty` ×4 | r1-11, r2-06, r3-06 | S |
| P6AM-17 | `tools.CutText` panics on a negative limit; `mcp.truncateUTF8` guards it and `extension.cutUTF8` does not (**r3's comparison is wrong, see C2**) | low | l | A | `tools/text.go:4-12` (`end := int(maxBytes)` then `text[:end]`), `extension/text.go:26-34`, contrast `mcp/content.go:241-245` | r3-05 | R |
| P6AM-18 | `json_bounds` maps an unrepresentable number to `null` — **refuted as a defect: it is the documented `JSON.stringify` behaviour** | low | l | D | `tools/json_bounds.go:79-101` (`ParseFloat` +Inf → `nil`), doc `:13-15` ("matching JSON.stringify at the tool boundary") | r3-07 | S (refuted as a defect) |
| P6AM-19 | A stdio MCP server's grandchild survives shutdown: no process group, no `Kill`/`Wait` in the phase, and the lifecycle test asserts nothing about the child | low | l,l | A | `mcp/manager.go:300-310` (`exec.Command`, no `SysProcAttr`), `:62-70` (`session.Close()` only), SDK `cmd.go:92-110` (SIGTERM/SIGKILL to the direct child); `manager_lifecycle_test.go` asserts tools, not processes | r1-13, r3-04 | S |
| P6AM-20 | A control character in a question name makes the result invalid JSON — **refuted: the `%q` is applied to a constant key list, and a rendered name is `json.Marshal`ed** | low | l | A | `extension/decide_systemone_backend.go:452-472` (keys from the literal list at `:459`; the name is the *value* of `"name"`, encoded at `:465-468`); `orderedDecideSystemoneProbabilities` already uses `json.Marshal(entry.Label)` at `:443` | r2-03 | R (refuted) |
| P6AM-21 | Test gaps and two misleading test names | low | l | D | `mcp/schema_dialect_debug_test.go:1-12` (tests the third-party library); `mcp/manager_concurrent_test.go:258-285` (goroutine count poll); `extension/decide_systemone_test.go` name vs body for the key test; missing: pagination, `Close` during a call, `1e400`, `isError` over the limit, key at the cut, many schema issues, a panicking `NormalizeArguments` (which D-1 itself asked for) | r1-14 | S |
| P6AM-22 | The `tool_smith` bridge copies 11 of 17 `ExecutionContext` fields, dropping `AgentDepth`, `RootRun` and `RemoteHops` — **true, but it is `internal/tools/native`, outside p6a** | out of phase (filed medium) | m | B/D | `tools/catalog.go:26-72` (the 17 fields); `tools/native/tool_smith_bridge.go:219-236` (the copy). r2's rebuttal (`subagent_call.go:59` forwards `RemoteHops`) is about a *different* file — see C3 | r1-05 | S |

Severity rule applied: highest evidence, then the median severity of the filings, with every departure named. **Kept at medium:** P6AM-01 (two mediums, and the control that `max_result_bytes` is supposed to provide is absent on one transport), P6AM-02 (medium, reproduced as a *mechanism* and measured by r1), P6AM-04 (two mediums, silent and indefinite loss of tools), P6AM-05 (medium + low, reproduced here: shutdown waits on the server), P6AM-07 (medium, reproduced at 40×), P6AM-08 (three mediums, untrusted payload with no budget), P6AM-12 (medium, reproduced here: a redirect to a second origin is followed). **Lowered:** P6AM-03 (from r4's medium: the rounding is the documented JavaScript shape; only the out-of-range literal is a defect), P6AM-06 (from r4's medium: nothing leaks, the child is closed once the connect returns — see C5), P6AM-09 (both mediums: `DECISIONS.md` documents `max_input_bytes` as counting the input only, and the caller is the model whose own arguments are already bounded by the provider), P6AM-10 (from r4's medium: the class — secret in a bounded error string — was rated low when p5 merged it as P5M-03), P6AM-11 (from r4's medium: requires a provider that answers out of range; no security effect), P6AM-13 (from r3's medium: the process no longer dies on the run path, D-1 is fixed; what is left is diagnostics), P6AM-15/P6AM-16/P6AM-17 (single-class maintainability), P6AM-22 (out of the phase).

`sev` summary: **medium** P6AM-01, 02, 04, 05, 07, 08, 12; everything else low. Per reviewer (entries, not filings): r1 12 entries from 14 filings — 6 consensus, 6 unique, 6 medium (5 kept, 1 moved out of phase); r2 6 entries from 6 filings — 5 consensus, 1 unique, 1 medium (kept), 1 filing refuted; r3 8 entries from 7 filings — 6 consensus, 2 unique, 3 medium (1 kept, 2 lowered); r4 9 entries from 9 filings — **6 consensus, 3 unique** (P6AM-06, 11, 12), 9 medium (4 kept, 5 lowered).

## 3. Verification of the medium findings

Probes are `go test -overlay` test files mapped onto the packages, so nothing was written in the tree; each one calls product code, never a copy. Commands and exit codes are in §10.

### P6AM-01 (medium, B/C) The streamable-HTTP body is uncapped and the size limit is applied late — r2-01, r4-03 — **confirmed by reading, including the SDK lines the reviewers cited**

The chain is: `executeTool` waits for the complete `CallToolResult` (`mcp/content.go:29-55`), `contentParts` builds one part per content block (including a base64 re-encode per image, `:199`), and only then does `boundedContent` measure and cut (`:143-174`). The limit therefore never bounds bytes **received, decoded, copied or base64-expanded** — and on the HTTP transport nothing above it bounds them either. Verified in the SDK at HEAD's module version `go-sdk@v1.8.0`:

- a plain JSON answer is `io.ReadAll(resp.Body)` with no limit (`streamable.go:2490`);
- the SSE path calls `scanEventsLimited(resp.Body, c.maxEventSize)` (`:2623`), `newStreamableClientConn` copies `maxEventSize: t.MaxEventSize` **verbatim** (`:2088`) instead of resolving 0 to a default the way `sse.go:437` does, and `scanEventsLimited` treats a non-positive budget as "no cap" (`event.go:73-75,125-130`). `internal/tools/mcp/manager.go:312-315` sets no `MaxEventSize`, so the streamable path is unbounded on both read paths;
- stdio is bounded, but by `DefaultMaxLineLength` = 16 MiB (`transport.go:124,494`), not by the configured `max_result_bytes` (default 1 MiB, min 512: `mcp/config.go:21-25`).

Who can reach it: whoever writes an `mcp_servers` file, and the server that file names (a third-party HTTP service in the streamable case). The item is a repeat of the **untaken size half of `audit_design1.md` B-S1** ("enforce at HTTP layer too") — the timeout half of B-S1 was CORRECTed in `audit_audit.md:24` and must not be reintroduced: do **not** add `http.Client.Timeout` here, because the SDK's streamable transport holds a long-lived body.

Severity: **medium**. The knob exists, is documented and is silently ineffective per transport, and the failure mode is an allocation chosen by an untrusted server.

### P6AM-02 (medium, C/A) Structured content is re-encoded per nesting level — r1-01 — **confirmed by reading; the 4.6 s figure is r1's measurement, not repeated here**

`orderedJSONObject.MarshalJSON` (`structured_content_order.go:124-146`) calls `marshalJSON` for **each field**, and `marshalJSON` (`content.go:263-272`) encodes into a `bytes.Buffer` and then runs `unescapeJSONSeparators`, which allocates a second buffer and copies the whole encoded value byte by byte (`:274-297`). A nested object therefore re-encodes its whole subtree at every level and copies it again: depth × size, with a constant of about three passes per level. The input arrives from the MCP server as `json.RawMessage` (`structured_content_capture.go:105-113`) and the re-encode runs in `executeTool` after the call returns, with no size or depth bound before it (`content.go:45-53`), on the run's tool worker and with no context check.

r1 measured, with a throwaway overlay test and a 1 MiB leaf: depth 100 → 447 ms, depth 1,000 → 4.62 s, depth 5,000 → 29.5 s (parse cost 2.5-8 ms throughout, so the whole cost is the re-encode). I did not re-run it: the task forbids measurement and the complexity is evident from the code. What would confirm it in the measurement step is `BenchmarkPreserveStructuredContentOrder` at depths 3/50/200/1,000 (§9 PH2).

Severity: **medium**. A configured server can spend seconds of a tool worker per result, and the frame is under the SDK's stdio cap, so nothing stops it.

### P6AM-04 (medium, A) `tools/list` pagination is ignored — r1-02, r4-04 — **confirmed by reading; definitive**

`connection.listTools` makes exactly one request, `c.session.ListTools(ctx, nil)` (`manager.go:44`), and `loadServer` passes only `listed.Tools` on (`:214`). `NextCursor` appears nowhere in the package (checked by grep, non-test and test). The SDK offers the cursor-following iterator `ClientSession.Tools` (`client.go:1555`), so the fix is local. The SDK server's default page size is 1000, so the gap is invisible with the fixtures and with well-behaved servers and silent with the rest: no error, no warning, only a smaller toolset. r1 reproduced it with `ServerOptions{PageSize: 2}` and five tools (two tools loaded, `err == nil`); the reading alone settles the mechanism.

Severity: **medium**: a capability silently disappears, and the model is never told.

### P6AM-05 (medium, A) `Manager.Close` waits for in-flight calls and closes serially — r1-03, r2-04 — **reproduced by execution here**

Probe `/tmp/p6amerge/p6amerge_close_probe_test.go`, overlay onto `internal/tools/mcp`, exit 0. An in-process SDK server whose tool handler blocks until released, a real `Manager` over a `streamable_http` config (`timeout_ms` 20,000), a real `Catalog`; one call starts, and then `Close` is started:

    Close still blocked 500ms after the call started (it waits for the call)
    Close returned after the call finished

The mechanism is `callTool` holding `c.mu.RLock()` across the whole `session.CallTool` (`manager.go:51-52`) while `close` needs `c.mu.Lock()` (`:63-64`): shutdown waits for the server, up to that connection's `timeout_ms` (max 10 min). r1 measured the same thing with a 5 s handler ("Close took 5.000644158s"). Nothing cancels the call's context — it is derived from the run's context in `executeTool` (`content.go:30`), not from the connection — so a hung server holds shutdown for the full timeout, per connection, and `Close` runs the connections serially (`:376-378`), as does `resolver.Close` over managers (`app/resolver.go:1034-1036`). On a stdio server each close then adds the SDK terminate sequence: 5 s for stdin EOF, SIGTERM, 5 s, SIGKILL (`cmd.go:68-110`), i.e. up to ~15 s per server (r2's "~10 s" is the same two windows).

Severity: **medium**. This is the 176fd7d class in its wait form: `Close` returns late rather than early, on the SIGINT path.

### P6AM-07 (medium, A/C) A schema-validation failure amplifies 4 KB into 162 KB — r1-04 — **reproduced by execution here**

Probe `/tmp/p6amerge/p6amerge_tools_probe_test.go`, overlay onto `internal/tools`, exit 0, through the real `Catalog.ExecuteFunctionCall` with a schema `{a: array of integer}` and 1,000 string items:

    items=1000 arguments=4007 bytes output=162135 bytes ratio=40.5x

`collectValidationIssues` appends one issue per failing element with no cap (`schema.go:97-138`, `issuesForKind` at `:376`) and `ExecuteFunctionCall` puts all of them into `SchemaError` (`catalog.go:357-365`). r1's numbers (162,123 B and 5.3 ms; 3.27 MB at 20,000 items; 16.4 MB at 100,000) are the same curve. The result is persisted to `messages.jsonl` and re-sent every round, so one malformed call — a model mistake or a prompt-injected one — can fill the context or exceed a provider's limits.

Severity: **medium**. Model-reachable, durable (it is in the transcript), and it costs 40× the input.

### P6AM-08 (medium, B/C) Discovery has no budget and the schema cache is count-only — r1-06, r3-03, r4-09 — **confirmed by reading**

`toolsFromList` (`manager.go:217-266`) validates the shape of each tool and the object-ness of its schema, then copies `remote.Description` and the whole `inputSchema` into the returned `tools.Tool`, marshals and compiles each schema through `m.schemas.get`, and applies no limit on the tool count, the description length or the schema size. The cache stores up to 512 compiled validators per manager with no byte bound (`schema_cache.go:39-67`), and a manager lives per session+profile until `Resolver.Close()` (`app/resolver.go:844-889`) — so memory is `sessions × 512 × schema size`. `LoadTools` bounds the list only by `current.timeout` (`manager.go:198-201`). The descriptions end up in `ResponseTools()` on every request, so an untrusted server also gets a prompt-injection channel and a standing token cost.

**What is prior art here:** the *frequency* half of r1-06 (one `tools/list` per server per run, no TTL, no `list_changed` handling) is A-6, and caching the list was **explicitly rejected by the user** (`audit_performance_plan.md:420`, P-006: "cache compiled schemas only; no tool-list staleness"). Only the byte/count budget is new. The idle-close half is an app-lifecycle question (D-2 territory), not a p6a defect.

Severity: **medium**, on the budget only.

### P6AM-12 (medium, B) A cross-origin redirect is followed — r4-08 — **reproduced by execution here**

Probe `/tmp/p6amerge/p6amerge_redirect_probe_test.go`, overlay onto `internal/tools/mcp`, exit 0, through the product's `newHeaderHTTPClient`:

    second origin reached=true status=200 authorization-on-second-origin=""

The `CheckRedirect` callback deletes the configured headers when the target is not the configured origin but then returns `nil`, so the request is made anyway (`manager.go:470-488`; the `RoundTrip` wrapper at `:496-507` only decides which headers are attached, never which host is contacted). A redirect target therefore receives a POST whose body the MCP server wrote, and — since the response is parsed as an MCP reply and can come back to the model as tool content — the primitive is a read-capable SSRF into the operator's loopback or private network, unauthenticated but with side effects. The neighbours in this repository treat redirects as a boundary: `decide_systemone` refuses them outright (`extension/decide_systemone.go:91-92`, `ErrUseLastResponse`) and `fetch` re-validates DNS and private ranges per hop (`native/fetch.go`, "checked and good" in `audit_design1.md:37`).

Severity: **medium**, kept from r4. Two reviewers (r1 X11, r2 §6) inspected this callback and asked only whether the *headers* could leak; both called it sound. The exposure is the destination, not the header, and it is now reproduced (C7).

## 4. Verification of the unique findings and the other lows

### P6AM-06 (low, A) A pending connect is not stopped by `Close` — r4-01 — **reproduced by execution here (r4's unresolved probe, now settled)**

Probe `/tmp/p6amerge/p6amerge_connect_probe_test.go`, overlay onto `internal/tools/mcp`, exit 0. An HTTP MCP server that swallows the `initialize` request until released; a real `Manager.LoadTools` started in a goroutine; then `Close`:

    Close returned while the connect was still pending (err=<nil>)
    LoadTools still running: the connect outlives Close
    LoadTools error after release: MCP manager is closed

So `Close` is not quiescent here — the opposite failure to P6AM-05 — and a **stdio** server started in that window would be a live child until `connectCtx` expires (`manager.go:321-322`, config `timeout_ms`, default 30 s). The late connection is closed correctly (`:337-342` calls `current.close()` when `m.closed`), so nothing leaks beyond that window; the defect is that shutdown returns while a process can still be spawned. **Lowered from r4's medium:** no resource survives its own timeout, the error is reported, and the window requires shutdown to race a connect. Fix: a manager-owned context for connects that `Close` cancels, and a wait for pending connects to settle.

### P6AM-03 (low, A) Numbers in structured content go through `float64` — r2-02, r4-05 — **reproduced by execution here**

Probe `/tmp/p6amerge/p6amerge_mcp_probe_test.go`, overlay onto `internal/tools/mcp`, exit 0, calling `preserveStructuredContentOrder` + `compactJSON`:

    {"n":1.0,"m":1e400} -> parse error: read structured content field "m": json: cannot unmarshal number 1e400 into Go value of type float64
    {"id":9007199254740993} -> {"id":9007199254740992}
    {"big":123456789012345678901234567890} -> {"big":1.2345678901234568e+29}
    {"ok":1.5} -> {"ok":1.5}

`readOrderedJSONValue` uses `decoder.Token()` without `UseNumber` (`structured_content_order.go:38-41`), so every number becomes a `float64`. The rounding of the first two in-range cases is what `JSON.stringify(JSON.parse(x))` does, and the function's own comment says it targets JavaScript-compatible text — that part is intended (C4). The **out-of-range literal is not**: `1e400` is valid JSON, JavaScript would print `null`, and here the error travels out of `executeTool` (`content.go:45-49`) into the canned `Tool <name> failed during execution` (`catalog.go:374-378`), so a valid tool result turns into a failure. `decoder.UseNumber()` fixes both (the re-encode writes the original literal). Severity **low** rather than r4's medium: only a number outside `float64`'s range triggers the failure, and no silent corruption happens that JavaScript would not also produce.

### P6AM-09 (low, B/C) No byte budget for questions — r3-02, r4-02 — **confirmed by reading**

`executeDecideSystemone` measures only the `input` parts against `MaxInputBytes` and only the question *count* against `MaxQuestions` (`decide_systemone.go:206-221`); `parseDecideSystemoneQuestions` trims strings, never measures them, and puts no bound on the option count (`:302-373`); `systemoneRequest` copies everything into the outbound map, which `requestDecisions` marshals whole (`decide_systemone_backend.go:79-88`). So `MaxQuestions` (16, max 128) × unlimited instructions/descriptions/options is one request of unbounded size. **Lowered from both mediums:** `DECISIONS.md` (decide section, "Arguments") documents `max_input_bytes` as counting "the text plus each JSON value as sent" — the input only — so this is a missing bound, not a broken contract; the caller is the model, whose arguments are already bounded by the provider response that carried them, and the outbound failure is a provider 4xx that the bounded error path reports. It becomes worth fixing if the model is ever prompt-injected into a memory/CPU squeeze: recommend one `max_questions_bytes` (or fold the questions into the same budget) with a pre-request refusal.

### P6AM-10 (low, B) The key is redacted after the 400-byte cut — r1-08, r4-06 — **reproduced by execution here**

Probe `/tmp/p6amerge/p6amerge_ext_probe_test.go`, overlay onto `internal/tools/extension`, exit 0, with a provider body that echoes the key after 380 filler characters:

    len=403 tail="x key=sk-kkkkkkkkkkkk..."
    contains full key: false; contains a >=8 char key prefix: true

`requestDecisions` calls `decideSystemoneRedact(decideSystemoneErrorMessage(data), key)` (`decide_systemone_backend.go:104`), and `decideSystemoneErrorMessage` has already cut the text to 400 bytes (`:139-141`); `decideSystemoneRedact` replaces only whole occurrences (`:61-66`). r1 measured the same shape (12 of 39 key characters left). **Kept low** (r4 filed medium): the leak needs a provider that echoes the key into a long message, and the class — a secret in a bounded error string — was merged at low in p5 as P5M-03. The fix is to redact inside `decideSystemoneErrorMessage` before the cut, and the existing test named "…NeverHoldsTheKey" does not cover the boundary (it never puts the key in the body, P6AM-21).

### P6AM-11 (low, A) Out-of-range probabilities and a missing confidence — r4-07 — **reproduced by execution here**

Probe (same overlay), calling `renderDecideSystemoneAnswers` with a choice answer that has no `confidence` and a predicate answer with `noul` 1.5:

    {"answers":[{"name":"q1","type":"choice","choice":"a","confidence":0},{"name":"q2","type":"predicate","probability":1.5}]}

`Confidence` is a plain `float64` with no presence flag (`decide_systemone_backend.go:299-308`) so an omitted value is indistinguishable from 0, and `decideSystemoneAnswerFits` checks only type and presence (`:398-411`), so `1.5` (or `-0.2`) is reported as a probability. **Lowered from r4's medium:** it needs a provider that answers outside [0,1] or omits `confidence`, nothing is executed on the strength of the number, and the model reads a number the provider did send. It does contradict the stated intent ("a gap is never read as a probability of 0", `DECISIONS.md` decide section, "Result") for the confidence column only — which is the argument for fixing it cheaply: make `Confidence` a `*float64` and clamp or refuse a probability outside [0,1] with `status: "unusable"`.

### P6AM-13 (low, A/D) The recover boundary and its diagnostics — r3-01, r1-09, r2-05 — **confirmed by reading**

Three statements, all verified:

1. **D-1 holds for the executor.** `executeRecovered`/`executeOutputRecovered` wrap `Execute`/`ExecuteOutput` (`catalog.go:395-409`), and the run path has a second, outer recover with the stack (`session/tool_batch.go:96-106`, `toolBatchPanic` logs `debug.Stack()`).
2. **The recovered panic is invisible.** `ExecuteFunctionCall` replaces the returned error with `ExecutionError(… "tool_execution_failed" …)` and never logs it (`catalog.go:374-384`); the `tools` package imports no logger. So a panicking tool leaves no line anywhere — the batch recover never sees the error because `ExecuteFunctionCall` returns `nil`. The existing test asserts only that the text does not leak (`catalog_test.go:173-203`).
3. **Normalisation and validation are outside any recover in this package** (`catalog.go:343-356`). On the run path the batch worker catches such a panic as a call error with a stack. Two callers are outside that: `app/remote_api.go:220-229` runs `ExecuteFunctionCall` on a bare goroutine (which would end the process), and `tools/native/tool_smith_bridge.go:468` runs it inside the `tool_smith` tool, i.e. under that tool's `executeRecovered` (contained, but the script dies).

**Lowered from r3's medium:** the process no longer dies on the run path, which was D-1's actual subject, and no panicking normaliser exists in the tree today — the reachable path needs a future tool bug. What is left is real and cheap: log the recovered error once (the batch worker's shape shows how), and add the regression test D-1's own recommendation asked for ("a tool whose `NormalizeArguments` panics"), which does not exist.

### P6AM-14 (low, A) `max_result_bytes` drops the part that matters — r1-07 — **reproduced by execution here**

Probe `/tmp/p6amerge/p6amerge_mcp_probe_test.go` (same overlay), budget 2,048:

    isError with long text parts=2 bytes=179  text0="MCP server reported a tool error."
    short then long        parts=2 bytes=148  text0="hi"
    single long text       parts=2 bytes=447  text0="xxxx…" (201 chars)

(a) an `IsError` result whose real message is 3,000 bytes gives the model only the canned marker — the error text is gone; (b) with `"hi"` first, the 3,000-byte second part is dropped whole instead of being truncated; (c) a single 3,000-byte text keeps 201 bytes, because `room := (max-used-notice-128)/6` assumes the worst case of `\u00XX` expansion (`content.go:154`) — about 1/6 of the budget is used. Narrow (only above the limit, default 1 MiB) and therefore low, but it turns an MCP tool error into a content-free message.

### P6AM-15 (low, D/A) The envelope un-does the escaping, and `max_input_bytes` counts different bytes — r1-10, r1-12 — **reproduced by execution here**

Probe `/tmp/p6amerge/p6amerge_tools_probe_test.go`, overlay onto `internal/tools`, exit 0:

    envelope contains \u003c: true, \u0026: true, raw U+2028: false
    envelope = {"type":"function_call_output","call_id":"call","output":{"x":"a\u003cb\u0026c","y":"\u2028"}}

`encodeFunctionCallOutputValue` marshals a struct holding a `json.RawMessage` (`catalog.go:419-429`), and `json.Marshal` compacts a `RawMessage` with HTML escaping **on**, so the `SetEscapeHTML(false)` + `unescapeJSONSeparators` work inside `mcp/content.go:263-298` and the `replaceJSONStringSeparators` pass in `json_bounds.go:35-77` are both undone at the last step (U+2028 is re-escaped too). The value is JSON-equal, so nothing reads wrong; what is lost is byte-level form and one pass of work. The same mechanism on the decide side means `max_input_bytes` — documented as counting "the JSON as sent (compact)" — can under-count by up to ~6× for `<&>`-heavy JSON (`decide_systemone_backend.go:79-88`). Fix: encode the envelope with `SetEscapeHTML(false)` and delete the now-pointless passes, or drop the passes and document the form. Severity low.

### P6AM-16 (low, D) Helper triplication — r1-11, r2-06, r3-06 — **confirmed by reading**

Three UTF-8 cutters (`tools/text.go:4`, `extension/text.go:26`, `mcp/content.go:241` — the third with a different algorithm and the only guard, C2); three encrypt-without-HTML-escaping helpers, two of them byte-identical (`tools/output.go:151-158`, `mcp/content.go:263-272`, `extension/decide_systemone.go:288-296`); two `\u2028/\u2029` replacers that are **not** equivalent by construction (`json_bounds.go:35-77` is string-aware, `mcp/content.go:274-297` treats the whole document); and `firstNonEmpty` in four packages. `extension` and `mcp` already import `tools`, so all of them have one home. The divergence is not hypothetical: C2 is a bug class caused by it. Low, and mechanical — but past the rule of three, so worth one commit.

### P6AM-17 (low, A) `CutText` panics on a negative limit — r3-05 — **reproduced by execution here, with r3's comparison corrected (C2)**

Probe `/tmp/p6amerge/p6amerge_tools_probe_test.go` and the extension overlay, exit 0 both:

    P6AM-1 CutText(-1) panicked: runtime error: slice bounds out of range [:-1]
    P6AM-4 cutUTF8(-1) panicked: runtime error: slice bounds out of range [:-1]
    P6AM-11 truncateUTF8(-1) = ""

`tools.CutText` (`text.go:4-12`) and `extension.cutUTF8` (`text.go:26-34`) both index `text[end]` with a negative `end`; `mcp.truncateUTF8` returns `""` early (`content.go:241-245`). r3 filed `CutText` correctly but claimed the extension sibling "returns early" — it does not (C2). Unreachable today (callers pass 400 and validated settings), so hardening only: one guard, one table case.

### P6AM-18 (low, D) `json_bounds` writes `null` for an unrepresentable number — r3-07 — **confirmed as a fact, refuted as a defect**

`normalizeJSONStringifyNumbers` maps a `ParseFloat` overflow to `nil` (`json_bounds.go:79-101`), and `DraftJSONBytes` documents itself as "matching `JSON.stringify` at the tool boundary" (`:13-15`). `JSON.stringify(JSON.parse("1e999"))` is `"null"`, so the code does what it says, including the odd-looking `-0 → 0.0` beside it. r3's objection is that the measurement then differs from the bytes on the wire — true, but that is the definition this helper implements, and the alternative (measuring raw bytes) would be a different contract. Recorded here so it is not re-filed; if the contract ever changes to "as sent", add the `1e999` table case and change the doc comment in the same commit.

### P6AM-19 (low, A) A stdio grandchild survives, and no test looks at the child — r1-13, r3-04 — **confirmed by reading; not reproduced (no process was spawned under this audit's rules)**

`getConnection` builds `exec.Command(command, args...)` with `Dir` and `Env` and hands it to `CommandTransport` (`manager.go:300-310`); nothing sets `SysProcAttr`, and the SDK's shutdown closes stdin, waits 5 s, sends SIGTERM to `s.cmd.Process`, waits 5 s and SIGKILLs **the direct child only** (`cmd.go:68-110`). A wrapper (`npx`, `uvx`, `sh -c …`) therefore leaves the real server running against a closed stdin, holding its ports and files. `manager_lifecycle_test.go` asserts tool replacement and closure, never that a process exited. The fix needs a process group (unix) and a retained handle; Windows support (`environment.go` has a windows table) is the complication that keeps this at low and effort M.

### P6AM-21 (low, D) Test gaps and two misleading names — r1-14 — **confirmed by reading**

`schema_dialect_debug_test.go` tests `jsonschema/v6`'s own dialect handling, not this repository's code, and its name says "debug"; `manager_concurrent_test.go:258-285` asserts a goroutine count against `before+2` by polling, which is sensitive to whatever else runs in the package; the decide test whose name promises the key is never held does not put a key in the body. There is **no test** for: pagination (no `NextCursor` anywhere, grep), `Close` during a call, a structured-content number that overflows `float64`, an `isError` result over `max_result_bytes`, a key at the 400-byte cut, many schema issues, and the panicking `NormalizeArguments` that D-1 itself asked for. Every one of those is a finding above, so the tests should land with the fixes — and, per D-1's recommendation, the panicking-normaliser test regardless.

### P6AM-22 (out of phase, B/D) The `tool_smith` bridge drops three execution fields — r1-05 — **fact confirmed; the file is `internal/tools/native`, not p6a**

`ExecutionContext` has 17 fields (`tools/catalog.go:26-72`); `tool_smith_bridge.go:219-236` copies 11 and omits `RawArguments` (per-call, set by the caller — correct to omit), `AgentDepth`, `RootRun` and `RemoteHops`. The consequence r1 describes is real in principle: inside a script, a nested call sees `AgentDepth == 0` (so `transcript.go:189`, `transcripts.go:169`, `transcript_remote.go:290` and `profile_message.go:356` cannot tell it is in a child) and `RemoteHops == 0` (so a nested remote call restarts the hop count). Two corrections: this is **`tools/native`**, which the phase prompt puts outside p6a's three packages, so it is a cross-phase lead, not a p6a finding; and r2's rebuttal (H12: "disproved, `subagent_call.go:59` forwards `RemoteHops`") answers about a different file — `subagent_call.go` forwards `RemoteHops` and the manager stamps `AgentDepth`/`RootRun` (`subagent_manager.go:572-573`), but the *bridge* is a separate hand copy that does not (C3). Suggest one `ExecutionContext.ForNested()` helper used by both, plus a test that walks every field — which would have caught it.

## 5. Refuted filings, contradictions and corrections

### C1 (fact) r2-03 reports that a control character in a question name makes the result invalid JSON — **refuted; the `%q` never sees the name**
`marshalDecideSystemoneItem` writes the **keys** from a literal list (`"name"`, `"type"`, `"probability"`, … at `decide_systemone_backend.go:459`) with `fmt.Fprintf(&out, "%q:", key)` at `:469`; the question name is the *value* of `"name"` and is encoded with `json.Marshal` at `:465-468`. r2's probe exercised `fmt.Sprintf("{%q:1}", "a\x01b")` — the Go quoting rule in isolation, not the product path. Two executions here settle it: `%q` does produce `\x01` and therefore invalid JSON for U+0000-U+001F and U+007F (`P6AM-8`), and the real render path with a name of `"a\x01b"` produces `{"answers":[{"name":"a\u0001b",…}]}` which `json.Unmarshal` accepts (`P6AM-9`). Nobody can introduce a key from the model here; the ordered-probabilities marshaller already uses `json.Marshal(entry.Label)` for the labels (`:443`). **Not a finding.** Still worth a test, because the two helpers in this file now differ in how they quote.

### C2 (fact) r3's comparison in P6A-R3-05 is wrong: `extension.cutUTF8` panics too — **only the `mcp` cutter is guarded**
`P6A-R3-05` says `cutUTF8` "returns early". `extension/text.go:26-34` has exactly the same shape as `tools/text.go:4-12` (`if len(text) <= maxBytes` then `text[:end]`), so a negative limit panics in both; `mcp/content.go:241-245` is the only one that returns `""` for `maxBytes <= 0`. Reproduced in two probes (`P6AM-1`, `P6AM-4`). The finding survives (the panic is real, unreachable today), the "inconsistency proves it is an oversight" argument is stronger than r3 stated, and a consolidation (P6AM-16) must fix both.

### C3 (fact) r1-05 is outside the phase, and r2's rebuttal of it answers a different file
`tool_smith_bridge.go` is in `internal/tools/native`, the fourth package, which the phase prompt excludes. r2's H12 ("disproved — `subagent_call.go:59` forwards `RemoteHops`") is about `subagent_call.go`; the bridge is a second, independent hand copy that drops `RemoteHops`, `AgentDepth` and `RootRun` (`:219-236`), and `AgentDepth`/`RootRun` are stamped on the child run by `subagent_manager.go:572-573`, not by `subagent_call.go`, so "the field is set elsewhere" does not repair the bridge. Carried as P6AM-22, a cross-phase lead with the fact confirmed, and **not** counted as a p6a finding.

### C4 (verdict) r3-07 files `json_bounds`'s `Inf → null` as a defect — **it is the documented behaviour**
`DraftJSONBytes` says it matches `JSON.stringify` after parsing (`json_bounds.go:13-15`), and `JSON.stringify(Infinity)` is `"null"`. The odd neighbours (`-0 → 0.0`, `1e400 → null`) are the same rule. r3's real point — the measured size is then smaller than the bytes sent — is a *contract* question, not a bug; recorded as P6AM-18 and closed, so it does not come back a third time.

### C5 (severity column) r4's nine mediums — **four kept, five lowered; and its process gaps are now closed**
Kept: R4-03 (P6AM-01), R4-04 (P6AM-04), R4-08 (P6AM-12), R4-09 (P6AM-08). Lowered: R4-01 → P6AM-06 (the late connection is closed at `manager.go:337-342`; nothing survives its own timeout), R4-02 → P6AM-09 (`DECISIONS.md` documents `max_input_bytes` as input-only), R4-05 → P6AM-03 (the rounding is JavaScript parity), R4-06 → P6AM-10 (same class as P5M-03, merged low), R4-07 → P6AM-11 (needs a misbehaving provider, nothing acts on the value). r4 also ran no `go build`/`go vet`/`gofmt` and left its `Close`-during-connect probe unresolved (§7 of its report); this merge ran all four checks (§10) and reproduced the probe question in both directions (P6AM-14, P6AM-15), which is what separates P6AM-05 from P6AM-06. r4's three uniques (R4-01 in revised form, R4-07, R4-08) stand as facts; only R4-08 keeps medium.

### C6 (prior art) r1-06's "no TTL, list per run" half is a **user decision**, not a finding
`audit_performance_plan.md:420`: "P-006: cache compiled schemas only; no tool-list staleness (part (b) rejected)." r1 files the absence of a cache and a TTL as part of a medium; A-6 is the open performance item, P-006 measured its CPU as small, and the staleness half is closed. What is left — and what r3-03 and r4-09 also report — is the missing byte/count budget, which this merge keeps as P6AM-08 and rates on that alone.

### C7 (verdict) r1 and r2 both cleared the MCP redirect callback by looking at the headers — **the destination is the exposure**
r1's X11 ("cross-origin redirect leaks MCP headers") is correctly disproved, and r2's "checked and sound" list says the headers are "stripped on a cross-origin redirect". Both are true and neither is the point: the request is still made to the redirected host (reproduced, the redirect probe output in §3). r4 is the only reviewer that asked where the request goes. Kept at medium.

### C8 (severity) r4-07's facts hold, its severity does not
`Confidence` with no presence flag and an unchecked `noul` range are both confirmed (P6AM-11). But `DECISIONS.md` makes the "never a made-up 0" promise only for the *primary* number of an unusable answer, and nothing downstream acts on a confidence of 0. Low.

### C9 (prior art) The `callMu` serialisation that r1-03 and r1-07 H7 point at is **by decision**
P-035 (`audit_performance_plan.md:729`) measured it and the user recorded "no action" (`:757`), because the default `ordered` scheduling already runs MCP calls one at a time and the `structuredContentCapture` is single-active by design. P6AM-05 is *not* that finding: it is about `Close` taking the same mutex write-side, which P-035 never covered. Do not read the two as one.

### C10 (framing) r3-01 is D-1's remainder, and D-1's subject was a dead process
r3 is right that `NormalizeArguments`/`Validate` sit outside `ExecuteFunctionCall`'s recover, and right that D-1's fix (`206ebd5`) touched the executor only. But `206ebd5` also put the outer recover on the batch worker (`tool_batch.go:96-106`), which catches normalisation panics on the run path — so the process no longer dies there, which was D-1's whole claim. The remainder is (a) diagnostics in `ExecuteFunctionCall`, (b) the `remote_api` goroutine, (c) D-1's own requested regression test. Low, as merged in P6AM-13.

### C11 (framing) r1-01 and r2-01 describe the same output path in different buckets — **both kept, neither double-counted**
r1 files the per-level re-encode as a performance defect (medium), r2/r4 file the missing wire-level cap as a boundary defect (medium). They are different defects in the same pipeline (`structured_content_order.go` vs the transport), and they have different fixes (encode once vs limit the frame), so they stay as P6AM-02 and P6AM-01. The measurement in §9 PH3 covers the copies both touch.

**No reviewer's facts conflict with another's on a fact.** Four disagreements are about severity or verdict on the same facts (C4, C5, C7, C8); two are factual errors (C1, C2); one is a scope error (C3); the rest are prior art and framing (C6, C9, C10, C11). Every one is resolved against the code at `0941b6a`, not against a reviewer.

## 6. Reviewer-pair agreement

| pair | shared entries | notes |
|---|---|---|
| r1 + r2 | P6AM-05 (different halves: block vs serial), P6AM-13 (no-log half), P6AM-16 | r1 found the blocking `Close`, r2 the serial loop and the SDK terminate windows; r1 and r2 agree that a panic leaves no diagnostic |
| r1 + r3 | P6AM-08, P6AM-16, P6AM-19 | the strongest overlap is the untrusted-server budget (r1 from the description/schema side, r3 from the schema-cache side) and process lifetime |
| r1 + r4 | P6AM-01 (r1 as complexity, r4 as boundary), P6AM-04, P6AM-10 | the three most consequential items in the phase have at least two independent readers except P6AM-02 |
| r2 + r3 | P6AM-13, P6AM-16 | both reached "recover covers the executor only"; they split on `Close` (r2 serial, r3 lifetime) |
| r2 + r4 | P6AM-01, P6AM-03 | r4 rated the number bug medium, r2 low — resolved in C4/P6AM-03 |
| r3 + r4 | P6AM-08, P6AM-09 | identical conclusions on the discovery budget and the decide question bytes, both medium, both lowered/kept for the same reasons |

Three independent statements exist for P6AM-08 and P6AM-13; two for P6AM-01, P6AM-03, P6AM-04, P6AM-05, P6AM-09, P6AM-10, P6AM-16, P6AM-19. Twelve entries are single-reviewer. As recorded in p3 C6 and p5 §6, the retrieval index serves `audit_findings/`, so overlap between reviewers is weak corroboration; the entries this merge trusts most are the ones it executed itself (P6AM-03, 05, 06, 07, 10, 11, 12, 14, 15, 17).

## 7. Prior art handling

| prior item | status there | p6a filings | handling here |
|---|---|---|---|
| A-6 / P-006 / P-016 / P-034 — MCP discovery once per run, one manager mutex | A-6 open; P-006(a) DONE (`e11b9f9`); P-016 + P-034 DONE (`f5da718`); **P-006(b) rejected by the user** | r1-06, r3-03, r4-09, r4's H04 | P6AM-08 for the **byte/count budget only**; the list-per-run frequency stays a measurement question (§9 PH1) and no TTL is proposed (C6) |
| P-035 — `connection.callMu` serialises calls to one server | measured, "no action" by user decision | r1-03, r1 H7, r2 H04 | **not re-filed.** P6AM-05 is `Close` taking the same mutex, which P-035 did not cover (C9). PH6 keeps the throughput question as a hypothesis |
| A-4 / B-S1 — no client/transport timeouts | provider stream fixed; "add `Client.Timeout`" CORRECTed (`audit_audit.md:24`); MCP timeouts closed by contexts (`audit_performance_plan.md:474`) | r2-01 (size half), r4-03 | P6AM-01 is the **untaken size half of B-S1**, now shown uncapped with SDK citations. The fix must not add `http.Client.Timeout` (it would cut the long-lived streamable body); `MaxEventSize` and a limited `RoundTrip` body are the right levers |
| D-1 / `206ebd5` — panic recovery on tool execution | ACCEPT (High), fixed for `Execute`; the recommendation also asked for a panicking-`NormalizeArguments` test | r3-01, r1-09, r2-05 | P6AM-13: executor coverage and the batch recover verified; the **diagnostics** and the missing regression test are new; C10 |
| D-2 — session delete does not release per-session resources | ACCEPT-LOW, open | r1-06 (lifetime half) | noted, not re-filed: manager lifetime is app-level (`app/resolver.go:844-889`) |
| D-3 — `jad` shutdown has no deadline | ACCEPT, open | r1-03, r2-04 | adjacent, not the same: D-3 is the overall deadline, P6AM-05 is one manager waiting on one server |
| P5M-03 / P3-09 — a secret or absolute path in bounded client-visible text | merged low twice | r1-08, r4-06 | P6AM-10 at low, same class, with the key-prefix mechanism as the new evidence |
| `audit_design1.md:37` "do not fix these" list — `fetch` SSRF pinning, per-redirect re-validation | checked and sound | r4-08 | cited as the neighbouring standard MCP does not meet (P6AM-12) |

Nothing in p6a contradicts an earlier audit record. The one place where a *decision* is at odds with the code is C6 (a cache was rejected while the cost it would address is now a measurement question), which is a measurement item, not a defect.

## 8. Ranked list (severity, then evidence, then finders, then effort)

| rank | id | sev | evidence | finders | effort | why here |
|---|---|---|---|---|---|---|
| 1 | P6AM-01 | medium | SDK lines + read | 2 | S | an untrusted server chooses the allocation on one whole transport; the knob that should stop it is inert per transport |
| 2 | P6AM-05 | medium | reproduced here + r1 | 2 | S | shutdown waits up to 10 min for a server, serially per connection and per manager |
| 3 | P6AM-12 | medium | reproduced here | 1 | S | a configured server can make the backend POST to any internal address; the two neighbours refuse or pin redirects |
| 4 | P6AM-04 | medium | read (definitive) + r1 probe | 2 | S | tools disappear silently past the first page |
| 5 | P6AM-07 | medium | reproduced here at 40.5× | 1 | S | 4 KB of bad arguments becomes 162 KB, persisted and re-sent every round |
| 6 | P6AM-08 | medium | read | 3 | M | no count/byte budget on tool definitions or schemas; per-manager retention |
| 7 | P6AM-02 | medium | read + r1 measurement | 1 | S | seconds of a tool worker for one nested result; fix is to encode once |
| 8 | P6AM-14 | low | reproduced here | 1 | S | an MCP error loses its message above the limit |
| 9 | P6AM-10 | low | reproduced here + r1 | 2 | S | the redaction misses a key straddling the cut |
| 10 | P6AM-06 | low | reproduced here | 1 | S | a stdio child can be spawned after `Close` |
| 11 | P6AM-03 | low | reproduced here | 2 | S | `1e400` turns a valid result into a failure |
| 12 | P6AM-13 | low | read | 3 | S | a panicking tool leaves no trace; a normaliser panic outside the batch worker would kill the process |
| 13 | P6AM-19 | low | read | 2 | M | a wrapper's real server survives shutdown |
| 14 | P6AM-09 | low | read | 2 | S | unbounded question payload to the provider |
| 15 | P6AM-11 | low | reproduced here | 1 | S | an out-of-range probability and an invented confidence 0 |
| 16 | P6AM-15 | low | reproduced here | 1 | S | two escaping passes are undone; one documented limit counts other bytes |
| 17 | P6AM-16 | low | read | 3 | S | three cutters, three marshallers, two non-equivalent replacers; C2 already bit |
| 18 | P6AM-17 | low | reproduced here | 1 | S | a negative limit panics in two of the three cutters |
| 19 | P6AM-21 | low | read | 1 | S | seven of the eight missing tests are findings above |
| 20 | P6AM-18 | low | read | 1 | — | refuted as a defect (JS parity); recorded so it is not re-filed |
| 21 | P6AM-20 | low | reproduced (refuted) | 1 | — | refuted as a fact; recorded so it is not re-filed |
| 22 | P6AM-22 | out of phase | read | 1 | S | real, but `tools/native`; one `ForNested()` helper closes it |

## 9. Performance hypotheses for the measurement step (grouped; **nothing measured here**)

21 raw hypotheses (r1 H1-H7, r2 H01-H04, r3 H-01…H-06, r4 H01-H04) → **7 groups**. "existing" means the benchmark is in the tree at `0941b6a` and was checked by name in this merge; "new" means nothing covers it. Every row is `[unmeasured]`; the numbers in the notes column are earlier audits' measurements, quoted, not re-run.

| id | hypothesis | file:line | workload | benchmark | sources |
|---|---|---|---|---|---|
| PH1 | `LoadTools` re-reads and re-normalises each server config, sends one `tools/list`, decodes it, then marshals/hashes/slugs and compiles a schema per tool — per run, per server | `mcp/manager.go:108-215,217-266,268-346,391-416`, `schema_cache.go:39-67`; callers `app/resolver.go:585-600` (every run), `:852-871` (toolset managers) | 1/4/8 servers × 5/50/500 tools; cold vs warm manager; 50 ms injected HTTP latency and 0 for stdio | existing `BenchmarkLoadTools`, `BenchmarkLoadToolsServers`, `BenchmarkConcurrentLoadTools`; add a warm sub-case and a 500-tool × 16 KiB-schema fixture | r1 H1, r1 H2, r2 H01, r3 H-01, r3 H-02, r4 H02, r4 H04 |
| PH2 | Structured content is re-encoded at every nesting level: depth × size, ~3 passes per level (P6AM-02) | `mcp/structured_content_order.go:124-146`, `mcp/content.go:263-298` | one 1 MiB leaf string at depth 3, 50, 200, 1 000, 5 000 | new `BenchmarkPreserveStructuredContentOrder`; report ns/op and B/op per depth, and check the curve is quadratic in depth × size — the shape decides whether r1's 4.6 s is the whole story | r1 H5, r1-01, r4 H01 |
| PH3 | An MCP result is encoded several times before it is stored: `partSize` marshals every part for the limit check, `marshalJSON` + `unescapeJSONSeparators` copy the whole document, `marshalParts` encodes again, `imagePart` re-base64s, the catalog then re-marshals the envelope (P6AM-01, P6AM-15) | `mcp/content.go:143-190,199,263-298`, `mcp/content.go:57-117`, `structured_content_capture.go:78-113`, `tools/catalog.go:390,419-429` | one 1 MiB text result and one 3 MiB PNG result at `max_result_bytes` 1 MiB and 16 MiB; 5 calls per response, 20 responses | new `BenchmarkExecuteToolResult` over the in-process SDK fixture, `b.ReportAllocs` | r1 H4, r2 H03, r3 H-04, r4 H01 |
| PH4 | The model's argument JSON is parsed 2-6 times per call: field unmarshal, `json.Valid`, `jsonschemaDecode`, `json.Marshal` of the normalised object, then `Validate`'s own `json.Valid` + `jsonschema.UnmarshalJSON`; the predicate methods (`ChangesState`, `CallMayChangeFiles`, `SourceOf`, `IsUserDecisionCall`) re-unmarshal the call | `tools/catalog.go:310-391,221-305`; callers `session/tool_batch.go`, `session/runner_tools.go:177-215,250` | 1 KB and 1 MB arguments on `{content: string}`; 4/8 calls per response; also `decide_systemone` with a 200 KB input | new `BenchmarkExecuteFunctionCall/{validate-only,execute}` plus one `BenchmarkPredicateMethods`; compare with `BenchmarkSchemaValidatorValidate` | r1 H3, r2 H02, r3 H-03 |
| PH5 | `DraftJSONBytes` decodes, copies the normalised tree, encodes, trims, converts to `string`, then rebuilds through the separator pass: ~6 copies of the draft | `tools/json_bounds.go:13-34,35-77,79-122` | 1 MB and 8 MB drafts, flat and deeply nested | new `BenchmarkDraftJSONBytes`, compared against a single streaming size pass | r1 H6 |
| PH6 | Calls to one MCP server serialise on `connection.callMu`, and a cancelled run's call only returns when the queue reaches it; `serverLock` serialises config reads and connects for the same server | `mcp/manager.go:47-60,268-271,348-357` | one server, 5/50 ms per call, N = 1/4/8 parallel calls in one response; then cancel the run mid-queue and record the latency to return | existing `BenchmarkParallelToolCalls`, `BenchmarkConcurrentLoadTools`; add a cancel-latency test | r1 H7, r2 H04, r3 H-05 |
| PH7 | Each `decide_systemone` call re-parses and clones its questions and marshals the whole request, then re-renders answers item by item | `extension/decide_systemone.go:120-232,302-373`, `decide_systemone_backend.go:79-88,244-272,352-474` | 1 and 128 questions × 2 and 50 options, against an `httptest` provider | new `BenchmarkDecideSystemoneExecute` over the existing `newDecideSystemoneRig` shape | r3 H-06 |

Order for the measurement step: **PH2 first** (it is the only group where the code's complexity, not a constant factor, is in question, and it decides whether P6AM-02's fix is a rewrite of the encoder or a small buffer reuse), then **PH3** (same fixture, and it also quantifies P6AM-01's "decoded and expanded before the limit"), then **PH1** (the A-6 question the user already ruled on: measure, do not propose a cache), then **PH4** (only if large arguments are common), then **PH6** and **PH7**, and **PH5** last. PH1's stdio rows should carry P-006's existing numbers (0.54 ms / 5 tools, 2.7 ms / 50 tools) as the reference; PH6 should record P-035's (5.384 → 43.07 ms for 1 → 8 calls at 5 ms) so a new run is comparable.

These seven groups were measured on 2026-10-29; the numbers, the verdict per group and the effect on the findings are in **§12-bis**. The table above is left as written so the hypotheses can be compared with what they predicted.

## 10. Verification log and repository state

Repo state: `git rev-parse --short HEAD` → `0941b6a`; `git status --short` before and after: `M EXECUTOR.md`, `M MASTER.md`, `M audit_plan.md`, untracked `REVIEWER.md`, `REVIEWER4.md`, `audit_findings/`; `git diff --check` → exit 2 for a pre-existing trailing-whitespace line in `EXECUTOR.md:21` only. No code, config, `sample/` or git state changed; the only repository file written is this report. Scratch: `/tmp/p6amerge` (seven probe test files, three overlay JSON files, `notes.md`) — removed at the end. No process was killed or restarted, and the running backend was not touched.

| command (from `backend-go/` unless noted) | exit | observed |
|---|---|---|
| `git rev-parse --short HEAD` | 0 | `0941b6a` |
| `go build ./internal/... ./cmd/...` | **0** | no output (r4 did not run this; the tree builds) |
| `go vet ./internal/tools/...` | **0** | no output |
| `gofmt -l internal/tools` | **0** | no output (no file needs formatting) |
| `go test ./internal/tools/ ./internal/tools/extension/ ./internal/tools/mcp/ -count=1` | **0** | `ok internal/tools 0.009s`, `ok …/extension 0.151s`, `ok …/mcp 7.486s` |
| `go test -overlay /tmp/p6amerge/overlay_tools.json ./internal/tools/ -run 'TestP6AM' -count=1 -v` | 0 | `CutText(-1) panicked: runtime error: slice bounds out of range [:-1]`; `envelope contains \u003c: true, \u0026: true, raw U+2028: false`; `items=1000 arguments=4007 bytes output=162135 bytes ratio=40.5x` |
| `… ./internal/tools/extension/ -run 'TestP6AM' -count=1 -v` | 0 | `cutUTF8(-1) panicked: …[:-1]`; `render…` control-char name → valid JSON; `len=403 tail="x key=sk-kkkkkkkkkkkk…"`, full key false, ≥8-char prefix true; `{"answers":[{"name":"q1",…,"confidence":0},{"name":"q2","type":"predicate","probability":1.5}]}` |
| `… ./internal/tools/extension/ -run 'TestP6AMQuoteEscapes' -count=1 -v` | 0 | `%q` of U+0000/U+0001/U+001F/U+007F → `\x00 \x01 \x1f \x7f`, `validJSON=false`; U+0080/U+2028/quote/backslash/nbsp/emoji → valid |
| `… ./internal/tools/mcp/ -run 'TestP6AM' -count=1 -v` | 0 | `{"n":1.0,"m":1e400}` → `json: cannot unmarshal number 1e400 into Go value of type float64`; `9007199254740993` → `…992`; `truncateUTF8(-1) = ""`; `boundedContent`: isError parts=2 bytes=179, short-then-long bytes=148, single long 447 |
| `… ./internal/tools/mcp/ -run 'TestP6AMCrossOriginRedirectFollowed' -count=1 -v` | 0 | `second origin reached=true status=200 authorization-on-second-origin=""` |
| `… ./internal/tools/mcp/ -run 'TestP6AMCloseWaitsForInFlightCall' -count=1 -v` | 0 | `Close still blocked 500ms after the call started`; `Close returned after the call finished` |
| `… ./internal/tools/mcp/ -run 'TestP6AMCloseDoesNotWaitForPendingConnect' -count=1 -v` | 0 | `Close returned while the connect was still pending (err=<nil>)`; `LoadTools still running: the connect outlives Close`; `LoadTools error after release: MCP manager is closed` |
| `git status --short`, `git diff --check` | 0 / **2** | state as above; `--check` flags one **pre-existing** trailing-whitespace line, `EXECUTOR.md:21`, which is part of the worktree state this step inherited and did not touch (no file this step wrote is tracked) |

Read at `0941b6a` (targeted ranges, never a whole Go file, as the navigation rules require): `tools/catalog.go:1-25,26-72,310-391,395-429`; `tools/schema.go:1-30,60-145`; `tools/json_bounds.go:1-125`; `tools/text.go:1-14`; `tools/output.go:60-100`; `tools/catalog_test.go:1-60,165-212`; `tools/extension/decide_systemone.go:196-240,296-378`; `tools/extension/decide_systemone_backend.go:40-145,240-278,280-420,430-474`; `tools/extension/text.go:1-35`; `tools/mcp/content.go:29-56,143-200,236-298`; `tools/mcp/manager.go:20-40,38-70,108-215,217-266,268-346,360-380,470-488`; `tools/mcp/structured_content_order.go:1-60,120-146`; `tools/mcp/config.go:20-40,110-140`; SDK `streamable.go:1990-2010,2075-2100,2480-2500,2620-2630`, `event.go:60-140`, `transport.go:118-130,490-505`, `cmd.go:18-115`, `client.go:1550-1560`; outside the phase, for leads only: `session/tool_batch.go:1-40,85-115`, `app/remote_api.go:200-240`, `app/resolver.go:570-600,840-895,1027-1046`, `tools/native/tool_smith_bridge.go:205-260,450-480`, `tools/native/subagent_call.go:45-95`, `tools/native/subagent_manager.go:570-575`, `tools/native/transcript.go:185-192`; `DECISIONS.md:451-500,300-320,585-600`; `audit_audit.md:20-30,50-80,140-152`; `audit_design1.md:10-40,65-85`; `audit_performance_plan.md:77,79,389,474,495,727,729,757`.

**Not run, by design:** no `-race` sweep and no benchmark (both excluded by the task); no full-suite `go test ./internal/... ./cmd/...` (no source changed, and the three phase packages are the affected scope); no `verify-go.sh` (it exists for a change set; this step changes no code); no live MCP server, no provider call, no credentials, no process spawn; no frontend command; no `messages-golden-diff` (nothing that writes a message changed).

## 11. Coverage gaps and unverified items

### Gaps
- **No `-race` run.** The concurrency claims that remain are read from mutex structure (`callMu`/`mu` ordering in `manager.go`, the `structuredContentCapture` single-active design) plus P-035's existing measurement. P6AM-05's probe exercised the real race in the "waits" direction only.
- **No measurement.** P6AM-02's 4.6 s and P6AM-07's timings are the reviewers'; only the byte ratios were re-derived here (162,135 B and 40.5×, which match). §9's seven groups are untouched.
- **No stdio process was spawned.** P6AM-19 (grandchild survival) is read from `exec.Command` + the SDK's `pipeRWC.Close`; no fixture was run, because killing children is outside this step's rules. P6AM-06's stdio consequence (a child spawned after `Close`) is inferred from the HTTP reproduction plus the same code path.
- **The MCP fixtures are in-process SDK servers over `httptest`.** That is enough for redirects, `Close` ordering and result conversion; it says nothing about a real third-party server's paging, streaming or error shapes.
- **`tools/schema.go:145-515`** (the false-schema, type-union and conditional normalisers, and the AJV message wording) was read for flow only; `schema_test.go` and `schema_messages_test.go` were not read. P6AM-07's location is the collector, not that mapping.
- **`extension/decide_systemone_test.go` (719 lines)** was read only where a finding needed it (the key test and the answer table). The provider wire format rests on `DECISIONS.md` (checked against the docs), not on a live call.
- **SDK internals** beyond the cited lines: retry/reconnect behaviour of the streamable transport, `MaxRetries`, and the standalone-SSE GET path were not reviewed. P6AM-01 covers the two client read paths only.
- **`sample/` and the config schemas** for `mcp_servers` and `decide_systemone` were not read; every finding here is code-side, and no fix proposal needs a new key except P6AM-09's optional one.
- **r4's scratch** (`/tmp/r4`) exists but holds no completed result for its close probe (§7 of its report). Its two other probes cannot be inspected either; its facts were re-derived here, so nothing rests on them.

### Unverified (stays in the list, flagged)
- **P6AM-19**: no process was spawned; the survival of a grandchild is read from the kill semantics of the SDK and the absence of a process group.
- **P6AM-06**: reproduced for `streamable_http`; the stdio variant (a child started after `Close`) is inferred, not observed.
- **P6AM-02**: the mechanism is confirmed by reading; the seconds are r1's measurement.
- **P6AM-08**: no oversized `tools/list` was fed; the absence of a budget is read from `toolsFromList` and the LRU.
- **P6AM-13**: no panicking normaliser exists in the tree, so the "kills the process from `app/remote_api.go`" half is a reachability argument, not a reproduction.
- **P6AM-12**: the second origin is reached and the header is stripped (reproduced); whether a real internal endpoint would accept the POST body, and what a caller could read back through the tool result, is not exercised.

## 12. Needs the user

### Decisions

1. **P6AM-01 — bound the MCP response at the transport, and do not add `http.Client.Timeout`.** **Recommendation: take both halves in one small change.** (a) `MaxEventSize: config.maxResultBytes` on the `StreamableClientTransport` (`manager.go:312-315`) — one line, and it also fixes the SDK's zero value, which this version does not resolve to its own 16 MiB default on the streamable path (`streamable.go:2088` vs `sse.go:437`). (b) Wrap the `HTTPClient`'s transport with a `RoundTripper` that limits `resp.Body` to `maxResultBytes` plus slack and fails on exceeding it — the plain-JSON path is `io.ReadAll` with no cap at all (`streamable.go:2490`), so (a) alone leaves it open. Optional (c): pass `MaxLineLength: maxResultBytes` to the stdio transport so the 16 MiB frame cap matches the configured limit instead of exceeding it. Do **not** add a client timeout: `audit_audit.md:24` corrected that, and it would cut the long-lived streamable body. Effort S; needs a test with an oversized HTTP body and one with an oversized SSE event.

2. **P6AM-05 + P6AM-06 — make shutdown cancel, then close, then wait.** **Recommendation: three small changes in `manager.go`, in this order.** (a) Give `connection` a `cancel` for in-flight call contexts: `callTool` derives its context from a per-connection context that `close()` cancels *before* taking `c.mu.Lock()`, so `Close` stops waiting on the server and waits only on the call to notice. (b) Close the connections concurrently (`sync.WaitGroup`) so one hung stdio server (up to ~15 s of SDK terminate sequence) does not multiply. (c) Track pending connects: one manager-owned context that `Close` cancels, and wait for the connects to settle, so a stdio child cannot be spawned after `Close` returned. The two probes from §3/§4 become `_test.go` regressions (they are written and passing as probes today). Effort S/M. If D-3's overall shutdown deadline (`audit_audit.md`, D-3) is built first, (b) can lean on it instead.

3. **P6AM-12 — refuse a cross-origin redirect.** **Recommendation: refuse it, do not merely strip the headers.** Return an error from `CheckRedirect` when the target is not the configured origin (there is no legitimate reason for an MCP endpoint to redirect to a third host), and keep the header-stripping code for the same-origin rewrite case. `decide_systemone` already does exactly this with `ErrUseLastResponse` (`extension/decide_systemone.go:91-92`), and `fetch` re-validates every hop — this makes the three consistent. Test: the probe in §3 turned into an assertion that the second origin is never reached. Effort S.

4. **P6AM-04 — follow `tools/list` cursors.** **Recommendation: loop on `NextCursor` inside the existing `listCtx`, with a page cap (e.g. 100) and the tool-count cap from decision 6**, or switch to the SDK's `ClientSession.Tools` iterator, which does the loop. Keep the tool order and the existing first-failing-server-in-order error behaviour. Test: an in-process server with `ServerOptions{PageSize: 2}` and five tools. Effort S.

5. **P6AM-07 — cap the validation issues.** **Recommendation: keep the first N (20) in `ExecuteFunctionCall` and append one final "N more issues omitted" entry**, so the model still sees the shape of its mistake and the result stays a few KB. Do not change `Validate`'s contract (the tests and the AJV-shape parity depend on the full list). Test: the 1,000-item probe as a regression asserting a size bound. Effort S.

6. **P6AM-08 — put a budget on discovery.** **Recommendation: yes, but as configuration, not constants** — a per-server `max_tools`, a per-tool `max_description_bytes` (default ~4 KiB) and `max_schema_bytes`, each failing as a `configError` naming the server and tool (the existing shape at `manager.go:225-241`), plus a byte bound on the schema cache (or refuse to cache an oversize schema and compile it per use, which keeps the correctness path intact). This needs a config round: `sample/schemes/`, the loader tests, and `DECISIONS.md`. **Do not** add a list TTL — P-006(b) is rejected (C6). Decide the three defaults with the measurement in PH1 if you want to avoid guessing. Effort M.

7. **P6AM-02 — encode the ordered tree once.** **Recommendation: rewrite `orderedJSONObject.MarshalJSON` and its recursion to write into one shared `bytes.Buffer`/`[]byte` (keys and scalars included) instead of calling `marshalJSON` per field**, which also removes the second copy inside `unescapeJSONSeparators`. Add a cheap pre-check: if the raw `structuredContent` is larger than `max_result_bytes`, or deeper than a small limit, refuse the ordering (the content is going to be cut anyway). Run PH2 first if you want the number in the commit message; the fix does not depend on it. Effort S.

8. **P6AM-14 — truncate the part that does not fit.** **Recommendation: cut the first *text* part whenever a part does not fit (not only when `len(bounded) == 0`), and size `room` from the real encoded size of the cut text** instead of dividing the budget by 6. That keeps an `isError` message and uses the budget it was given. Test: the three cases in §4. Effort S.

9. **P6AM-10 — redact before the cut.** **Recommendation: pass `key` into `decideSystemoneErrorMessage` and redact before `cutUTF8(text, 400)`**, with the straddling-key case as the test (and consider the same ordering for `decideSystemoneDetailText`). Effort S.

10. **P6AM-03 — keep the digits.** **Recommendation: `decoder.UseNumber()` in `preserveStructuredContentOrder`, and map an out-of-range literal to `null` in `orderedJSONObject.MarshalJSON`** so it matches what JavaScript prints instead of failing the call. That keeps the documented JS-compatible shape for the in-range cases (the rounding stays) and removes the only way a valid result can fail. Test: `{"m":1e400}` must produce `{"m":null}`, not a tool failure. Effort S.

11. **P6AM-13 — make a panicking tool leave a trace.** **Recommendation: log the recovered error once, keeping the model-facing text unchanged**, and add the regression test D-1's own recommendation asked for (a tool whose `NormalizeArguments` panics must come back as a `function_call_output`, not crash the test binary). This needs a decision on the shape, because `tools` deliberately imports no logger: either a package-level `slog.Default()` call (the smallest change, and `session` already logs stacks this way) or an optional log hook on `Catalog`. Also decide whether `app/remote_api.go:220-229` gets its own `recover` — I recommend yes, one line, since it is the only goroutine that can take the process down. Effort S.

12. **P6AM-19 — kill the process group.** **Recommendation: unix-only `SysProcAttr{Setpgid: true}` plus a group kill after the SDK's `session.Close()`**, and a lifecycle test asserting the fixture pid is gone. This is the one item that needs a Windows decision (the repository supports a Windows PATH table in `environment.go`); if Windows must stay first-class, gate it on GOOS and note the gap. Effort M.

13. **P6AM-09 — one budget for the questions.** **Recommendation: measure the questions as sent (compact JSON) and refuse before the request**, either against `max_input_bytes` (simplest, one counter) or a new `max_questions_bytes` with the same default. A per-question option count cap (`max_options`, e.g. 32) closes the second half. Config round + test that an oversized instruction is refused without any request. Effort S.

14. **P6AM-11 — make the answer honest.** **Recommendation: `Confidence *float64` and refuse (or mark `unusable`) a probability or score outside its documented range**, so a gap stays a gap and a nonsense number is not echoed as a probability. Effort S, and it fits the existing `unusable`/`missing` design.

15. **P6AM-15 — decide where the escaping policy lives.** **Recommendation: encode the envelope with an encoder that has `SetEscapeHTML(false)` in `encodeFunctionCallOutputValue`**, then delete the `unescapeJSONSeparators` pass in `mcp/content.go` (its work is then redundant) and have `json_bounds` share the same helper. Do the decide side (`systemoneRequest`) with it, which makes the "as sent" comment in `DECISIONS.md` true. If you prefer to keep `json.Marshal`'s form, then delete the `mcp` pass and say so in the comment instead — either way one of the two passes should go. Effort S.

16. **P6AM-16 + P6AM-17 — one consolidation commit, with the guard.** **Recommendation: export `tools.CutText` and `tools.MarshalJSON`, fix `CutText`'s negative limit, delete `extension.cutUTF8`, `extension.marshalDecideSystemoneJSON`, `mcp.truncateUTF8`, `mcp.marshalJSON`, and replace `mcp.unescapeJSONSeparators` with the string-aware `json_bounds` helper** (moved to a shared home with a name that says "inside strings"). Do **not** reach into `tools/native` in the same change (its copies belong to that phase); leave a lead note there. Effort S.

17. **P6AM-21 — the tests that go with the fixes.** **Recommendation: land each test with its fix** (pagination, `Close` during a call, `1e400`, `isError` over the limit, the key at the cut, many issues, the panicking normaliser), rename or delete `schema_dialect_debug_test.go` (it tests the dependency; move it to a comment or drop it), and replace the `before+2` goroutine poll with the existing leak-check shape. Effort S.

18. **P6AM-18 + P6AM-20 — the two refuted items.** **Recommendation: close both as recorded.** For P6AM-20 add the boundary test anyway (a control character in a question name must round-trip), because the file now quotes its keys with `%q` and its labels with `json.Marshal` and only the test keeps that distinction honest. For P6AM-18 add nothing unless the contract changes from "as `JSON.stringify`" to "as sent".

19. **P6AM-22 — the `ExecutionContext` copy.** **Recommendation: add `func (e ExecutionContext) ForNested() ExecutionContext` in `tools/catalog.go` (in this phase) with a test that walks every field**, and change `tools/native/tool_smith_bridge.go` to use it in the native/skill phase (or now, if you are willing to touch that package from this batch). `subagent_call.go` should use it too. This is the only item where the fix spans the phase boundary; the helper itself is p6a's.

20. **Severity confirmations.** Medium: P6AM-01, 02, 04, 05, 07, 08, 12. Everything else low, with nine mediums lowered (five of r4's, two of r3's, one of r1's out of phase) and two filings refuted. **Recommendation: confirm as merged.** The two that could reasonably go the other way: **P6AM-09** (medium, if you treat an unbounded outbound request to a paid provider as a boundary defect rather than a missing bound) and **P6AM-12** (low, if you consider a redirecting MCP server to be inside the trust boundary — I do not, which is why it is medium).

21. **Measurement go-ahead for §9's seven groups, in the order given** (PH2, PH3, PH1, PH4, PH6, PH7, PH5), plus **one `-race` pass on `internal/tools`, `internal/tools/extension` and `internal/tools/mcp`** after the P6AM-05/P6AM-06 batch — that batch changes mutex use in the MCP connection, and the phase has no race coverage today. No benchmark is needed for any *fix* decision except PH2 (which decides the shape of the P6AM-02 encoder) and PH1 (which is the A-6 question the user already ruled on).

22. **What I did not do and would need your word for.** Nothing in this step wrote code. If you want the fixes staged as one batch, the natural split is: (i) the MCP transport/shutdown/redirect batch (decisions 1, 2, 3, 4, 6, 7, 8, 10, 12), (ii) the small correctness batch (`decide` redaction, `*float64` confidence, `UseNumber`, envelope escaping, `CutText`, consolidation — decisions 9, 11, 13, 14, 15, 16), (iii) the tests and the two refuted items (17, 18). Batch (i) needs the config round for decision 6 and the Windows decision for decision 12.

Scratch: `/tmp/p6amerge` (seven probe files, three overlay JSONs, `notes.md`) — removed at the end of this step; nothing to remove from the tree.

## 12-bis. Measurements of the §9 hypotheses (2026-10-29)

Method: one group at a time, nothing else running, `-count=5`, medians reported (min–max where the spread matters). Every probe is an overlay file under `/tmp/p6amerge/probes/` added to the package under test with `go test -overlay`, so no source file in the repository was touched. Three of the seven groups needed no new code: `BenchmarkLoadTools`, `BenchmarkLoadToolsServers`, `BenchmarkParallelToolCalls` and `BenchmarkConcurrentLoadTools` already existed at `0941b6a`; the other four are new probe benchmarks (`zz_p6a_ph2/ph3/ph4/ph5/ph6/ph7`). Machine: AMD Ryzen 5 3600 (6 cores / 12 threads), load average 0.9–2.6 during the runs, one browser using ~45 % of one core (not idle-idle; the numbers are medians of five serialised runs and reproducible to a few percent). `go vet` passed on every overlay set before it was benchmarked.

### PH2 — structured content is re-encoded per level (**confirms P6AM-02**)

`preserveStructuredContentOrder` + `compactJSON` (the production path in `executeTool`) over `{"a":{"a":...}}` with a 1 MiB leaf string, `-benchtime=1x`:

| depth | preserve only | preserve + marshal | med / min–max | B/op | ms per level |
|---|---|---|---|---|---|
| 3 | 3.20 ms | 21.33 ms | 21.06–22.87 | 34.7 MB | 7.1 |
| 50 | 2.33 ms | 237.14 ms | 234.7–242.0 | 381 MB | 4.74 |
| 200 | 2.61 ms | 936.45 ms | 933.7–953.2 | 1.49 GB | 4.68 |
| 1000 | 3.62 ms | **4 983.51 ms** | 4 969.9–5 015.1 | **7.39 GB** | 4.98 |

- **P6AM-02's number, measured here: 4.98 s** at depth 1000 with a 1 MiB leaf (r1 reported 4.6 s; +8 % on this machine, same workload). The finding stands as filed.
- The curve is **linear in depth × size, not quadratic in depth**: ~4.9 ms per nesting level per MiB, i.e. about **7 copies of the document per level** (7.39 GB ÷ 1 000 levels ÷ 1.05 MB), not the ~3 the hypothesis guessed. Parsing (`preserve` alone) is flat at 2.3–3.6 ms — the whole cost is the re-encode.
- Depth 5000 was **not run** (extrapolated: ~25 s and ~37 GB allocated for one 1 MiB result). The allocation figure is the more serious half: the depth comes from the server, and the work happens before `max_result_bytes` is consulted, which is P6AM-01's class of defect. Severity stays **medium**; the fix in decision 7 (encode once into one buffer) removes the per-level copy, and the depth/size pre-check it also proposes is now clearly worth having rather than optional.

### PH3 — copies of one MCP result before it is stored

`contentParts` → `boundedContent` → `marshalParts`, the body of `executeTool` after the round trip (`-benchtime=20x`). "copies" = B/op ÷ encoded payload:

| case | parts | + bounded | + bounded + marshal | copies |
|---|---|---|---|---|
| 1 MiB text, `max_result_bytes` 1 MiB | 0.3 µs | 6.03 ms / 6.72 MB | 6.94 ms / 7.45 MB | 7.1× |
| 16 × 64 KiB text, 1 MiB total | 1.2 µs | 5.52 ms / 5.58 MB | 12.65 ms / 13.76 MB | 13.1× |
| 3 MiB PNG (4.19 MB base64), max 16 MiB | 3.88 ms / 12.6 MB | 24.81 ms / 37.1 MB | 47.39 ms / 61.3 MB | 14.6× |
| 1 MiB structured, depth 3 | 19.03 ms / 29.1 MB | 25.29 ms / 36.3 MB | 26.42 ms / 36.8 MB | 35.0× |

- **Confirmed and quantified**: ~7 copies for one big text part, ~13 when the result has many parts (`partSize` marshals every part only to measure it), ~15 for an image (base64 once in `imagePart`, then marshaled twice more), and the structured case spends 19 of its 26 ms inside PH2's recursion before any limit applies.
- 5 calls against a 3 MiB-image server in one response ≈ 235 ms and ~305 MB of allocation, none of it produced by the model and all of it discarded except 16 MiB.
- No severity change (P6AM-01 stays medium, P6AM-15 low). This is the "decoded and expanded before the limit" half of P6AM-01 with numbers; the fix shape (limit the body at the transport) is unchanged.

### PH1 — `LoadTools` per run, by tool count and schema size

Warm path (the per-run cost `app/resolver.go` pays), in-process server, no injected latency (`-benchtime=10x`):

| tools | 1 KiB schema | 16 KiB schema |
|---|---|---|
| 5 | 0.53 ms / 0.53 MB | 2.16 ms / 2.53 MB |
| 50 | 2.70 ms / 3.33 MB | **18.72 ms / 24.3 MB** |
| 500 | 24.09 ms / 34.9 MB | **157.89 ms / 228.9 MB** |

With 5 ms per HTTP request: 50 tools 7.87 ms, 500 tools 28.90 ms — i.e. **exactly one round trip on top of the CPU cost**: the list is one request, and above ~50 tools the CPU (re-marshal, hash, slug, schema compile/cache lookup per tool) dominates.

- ~**458 KB allocated per tool per run** at a 16 KiB schema (28× the schema) and ~70 KB at 1 KiB (68× — a large fixed per-tool overhead).
- This is the A-6/P-006 question measured, and it does **not** argue for a cache: it argues for the byte budget in decision 6. The measured shape says a `max_schema_bytes` default around 64 KiB is generous (16 KiB per tool already costs 158 ms and 229 MB per run at 500 tools, and two servers' worth would be worse), and that `max_tools` at a few hundred is where the per-run cost stops being free.
- New observation, not a new finding (it belongs to P6AM-08): at 500 × 16 KiB the per-run allocation is 229 MB, which on a 12-thread box is a visible spike on **every run**, not a leak.

### PH4 — how many times the argument JSON is walked

`ExecuteFunctionCall` on `{"content":"..."}`, plus the four predicate methods the batch code calls per call:

| arguments | `Validate` alone | `ExecuteFunctionCall` | 4 predicates |
|---|---|---|---|
| 1 KiB | 6.69 µs | 24.4 µs (20.3 KB) | 13.9 µs (4.4 KB) |
| 1 MiB | 3.11 ms | 11.27 ms (**16.8 MB**) | 8.23 ms (3.17 MB) |

- 3.6× `Validate` alone, and ~**16 copies of the argument bytes per call** at 1 MiB — more than the 2–6 the hypothesis estimated, because every `json.Unmarshal` into `map[string]any`/`any` re-materialises the strings.
- The predicate methods cost a further 8.2 ms at 1 MiB (four re-unmarshals of the whole call). This stays a **hypothesis with a number**, not a finding: the input is bounded by `max_input_bytes`, CPU is the only cost, and the absolute numbers at realistic (≤64 KiB) argument sizes are tens of microseconds. It also gives the shape for `decide_systemone`'s 200 KiB input (measured under PH7: 3.64 ms, 3.77 MB).

### PH6 — calls to one server serialise; a queued cancel waits

Existing benchmarks (`-benchtime=10x`), 5 ms per call:

| N (5 ms each) | 1 | 4 | 8 |
|---|---|---|---|
| `BenchmarkParallelToolCalls` | 5.53 ms | 21.96 ms (3.97×) | 43.88 ms (7.93×) |
| `BenchmarkConcurrentLoadTools` (callers) | 5.47 ms | 6.21 ms | 6.65 ms |

- **Calls serialise exactly**: N calls cost N × one call (3.97× at 4, 7.93× at 8). This reproduces P-035 (5.384 → 43.07 ms) to within 3 %; the decision "no action" stands (C9).
- **The `LoadTools` half of r1 H7 is refuted**: 8 concurrent callers finish in 6.65 ms, i.e. one round trip, so the warm path is overlapped or coalesced and is *not* serialised by `manager.mu` into 8 round trips.
- New probe, cancel latency: with 7 calls of 50 ms queued, a call whose own context is cancelled 20 ms in **returns 334 ms after the cancel** (333.5–334.5 ms over 5 repeats) — the queue is not context-aware. **But** when all 8 calls share one context (the run-stop case), all return **~1.0 ms** after the cancel (0.35–1.22 ms), because every queued call aborts on its own turn and the queue drains at once. So the measured cost is real only when one call's own deadline expires while its siblings keep running; **no new finding, no action** — this narrows r1 H7 rather than confirming it.

### PH7 — `decide_systemone` per call

In-process fake provider (no artificial latency), `-benchtime=20x`:

| case | time | allocation |
|---|---|---|
| 1 question × 2 options | 146 µs | 37 KB |
| 1 × 50 options | 281 µs | 117 KB |
| 128 × 2 options | 3.06 ms | 1.48 MB |
| 128 × 50 options | 19.39 ms | 11.82 MB |
| 1 question, 200 KiB input | 3.64 ms | 3.77 MB (19× the input) |

- ~24 µs per question at two options; the 128 × 50 row is **115× the size of the call** in allocation, from parsing each question's options and re-rendering each answer (the existing `MaxQuestions` cap and the missing byte cap are what bound this).
- Supports P6AM-09 with numbers (200 KiB of input costs 3.6 ms and 3.8 MB with no byte budget in front of it). Severity unchanged; the option-count half of decision 13 (`max_options`) now has a cost to point at.
- The render path is not a hotspot at realistic sizes; nothing here justifies changing the request shape.

### PH5 — `DraftJSONBytes`

| draft | time | allocation |
|---|---|---|
| 1 MiB, flat | 6.95 ms | 10.5 MB |
| 1 MiB, depth 100 | 7.21 ms | 10.6 MB |
| 8 MiB, flat | 54.64 ms | 83.9 MB |
| 8 MiB, depth 100 | 55.63 ms | 84.0 MB |

- **~10 copies** of the draft per call, linear in size, no depth term (~150 MB/s). Confirms r1 H6; 55 ms per interaction request at `max_draft_bytes` 8 MiB is the cost of the size accounting alone. No finding, no severity change; a single streaming size pass would take it to ~1 copy if it ever matters.

### All seven groups at a glance

| group | verdict | headline number | effect on the findings |
|---|---|---|---|
| PH2 | confirmed | 4.98 s at depth 1000 (7.39 GB allocated) | P6AM-02 stands; the depth/size pre-check in decision 7 is now clearly wanted |
| PH3 | confirmed | 7 / 13 / 15 copies for text / many-parts / image | P6AM-01's "expanded before the limit" half quantified; no severity change |
| PH1 | confirmed | 158 ms and 229 MB per run at 500 × 16 KiB | data-driven defaults for decision 6 (`max_schema_bytes` ≈ 64 KiB, `max_tools` a few hundred) |
| PH4 | confirmed (small) | 3.6× `Validate`, 16 copies at 1 MiB | stays a hypothesis; no finding |
| PH6 | calls serialise (stands); `LoadTools` half refuted; cancel narrowed | 7.93× at 8 calls; run-stop drains in ~1 ms | P-035 "no action" unchanged; no new finding |
| PH7 | confirmed (small) | 19.4 ms / 11.8 MB at 128 × 50 | supports P6AM-09; no severity change |
| PH5 | confirmed | 55 ms and 84 MB for an 8 MiB draft | no finding |

### Severity and decision changes caused by the measurements

- **No severity changes and no new findings.** The two strongest candidates were checked and declined: PH1's 229 MB per run is a spike inside P6AM-08 (already medium), and PH6's 334 ms queued-cancel is real but does not affect a run stop (measured at ~1 ms) and is bounded by one call's deadline, so it stays inside P6AM-05's scope rather than becoming its own item.
- **Decision 6 (P6AM-08) now has defaults from measurement**, not guesses: `max_schema_bytes` ≈ 64 KiB, `max_tools` a few hundred (500 × 16 KiB costs 158 ms and 229 MB per run).
- **Decision 7 (P6AM-02) gains the number 4.98 s** for the commit message and a stronger reason to ship the depth/size pre-check with the encoder rewrite; the fix itself is unchanged.
- **Decision 13 (P6AM-09)** can cite 3.64 ms / 3.77 MB for 200 KiB of input and 19.4 ms for 128 × 50 questions.
- **Decision 21 is done**: the seven groups are measured and the `-race` pass has run (below).
- **P6AM-05/P6AM-06's fix shape is unaffected** — the measurement adds no mutex/ordering evidence beyond what the probes already showed, and `-race` is clean.

### The one `-race` pass

`bash .jad/exec/bash/verify-go.sh --race "./internal/tools ./internal/tools/extension ./internal/tools/mcp"` from the repository root (`-race -count=3` on those three packages):

| step | exit | observed |
|---|---|---|
| `gofmt -l .` | **0** | no file listed |
| `go vet ./internal/... ./cmd/...` | **0** | no output |
| `go test -count=1 ./internal/... ./cmd/...` | **0** | all packages ok (whole suite, not just the three) |
| `go test -race -count=3 ./internal/tools ./internal/tools/extension ./internal/tools/mcp` | **0** | `ok tools 1.079s`, `ok …/extension 1.903s`, `ok …/mcp 73.079s`; **0** occurrences of `DATA RACE` in the log |
| `git diff --check` | **2** | the same single pre-existing trailing-whitespace line, `EXECUTOR.md:21`, inherited with the worktree; nothing this step wrote is tracked |

The race detector found nothing in the two concurrency paths the phase cares about (`connection.callMu`/`mu` and the `structuredContentCapture`), which is consistent with P6AM-05/06 being **blocking**, not data races.

### Still unmeasured

- **PH1 cold path at 500 tools / 16 KiB** (only the warm per-run path was measured; cold adds connect and the first compile).
- **PH2 at depth 5000** — extrapolated only (~25 s, ~37 GB allocated); not run, to keep the step inside its time budget.
- **PH3 with a real HTTP round trip**: the numbers are the local conversion half, so the SDK's own decode of a 3 MiB body is not included; the end-to-end figure is higher, not lower.
- **PH4 inside a real 8-call response** (the predicate methods were measured directly, not through `session/tool_batch.go`).
- **PH6 with one call's deadline expiring inside a real batch** (the probe called `ExecuteOutput` directly rather than through `runToolBatch`).
- **PH7 against a slow or real provider** (local fake provider, no artificial latency).
- **The fixes themselves**: no patched build was measured, so the expected gains (PH2 ~7× fewer copies, PH3 ~2, PH5 ~1) are projections from the copy counts, not results.
