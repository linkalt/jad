# Audit report — phase p6b — reviewer r4

- HEAD: `385cc89`
- Files read: phase prompt; audit plan §§1.1/4/5; `backend-go/CLAUDE.md`; relevant `backend-go/DECISIONS.md` entries; named root prior audits; workflow skill; all 33 non-test source files in `backend-go/internal/tools/native/` p6b scope. Coverage details are in `/tmp/r4/notes.md`.
- Commands: `cd backend-go && go test ./internal/tools/native -run '^(TestToolSmithBridgeForwardsTheExecutionHooksOfTheOuterCall|TestLatestTotalIsCappedAcrossHandles)$' -count=1` — exit 0. No build/vet/gofmt run.
- Date: 2026-10-29

## Understanding note

Phase scope covers in-process native command tools and their helper lifecycles: `exec` builds bounded runner plans and invokes the shared shell-child path; managed `process` persists command state and exposes owner-scoped async handles; `tool_smith` stages saved helpers, starts its nested-tool bridge, and can return an async handle; `AsyncHost` coordinates handles, progress cursors, wait/list/status/stop, and output summaries. Process groups, orphan records and scratch cleanup are platform- and lifecycle-specific. The configured tool model is intentionally not a sandbox: enabled tools run without approval, with the container as the security boundary.

## Hypotheses (status)

- H1 (verified): a nested `tool_smith` bridge drops chain/root/hop state from `ExecutionContext`, affecting nested guard decisions — P6B-R4-01.
- H2 (verified): async latest can advance a handle's output cursor beyond the aggregate-budget-clipped text — P6B-R4-02.
- H3 (no additional finding after source review): exec command/template construction, managed process env/cwd and lifecycle, Tool Smith helper boundaries, scratch handling, process group/orphan cleanup, and async ownership. No additional defect was retained from these checks.
- H4 (performance hypothesis, unmeasured): active agent transcript summaries re-index/scan bounded transcripts on each latest poll; see measurement hypotheses.

## Findings

### P6B-R4-01  Tool Smith's nested execution context drops chain and remote-hop guards
- severity: medium
- lens: B security | A correctness
- location: `backend-go/internal/tools/native/tool_smith_bridge.go:220-242`
- evidence: [read] `startToolSmithBridge` constructs a new `tools.ExecutionContext` by enumerating selected fields (`WorkspaceRoot`, `SessionDir`, `SessionKey`, `AgentName`, `PathScope`, hooks, and process-owner/async fields) but omits `AgentDepth`, `RootRun`, and `RemoteHops`, although all three exist on `ExecutionContext` (`backend-go/internal/tools/catalog.go:63-70`). The nested catalog executes tools using this stored context. Thus a nested tool that consults chain depth/root-run or remote-hop policy sees zero values rather than the outer call's values. The omission is confirmed and is carried-in item P6AM-22.
- suggested fix: use one `ExecutionContext.ForNested(...)`/copy helper that preserves all fields and explicitly overrides call-specific ownership; add a field-walking regression test that fails when any context field is omitted.
- effort: S
- prior art: P6AM-22 (carried-in; confirmed)

### P6B-R4-02  Async latest advances an output cursor past text clipped by the aggregate budget
- severity: medium
- lens: A correctness
- location: `backend-go/internal/tools/native/async_latest.go:40-69, 136-146`
- evidence: [read] `outputLatest` builds `next` from the collector's full produced-byte offsets at lines 136-137, while `latestBlocks` may truncate `text` to the remaining `LatestTotalBytes` at lines 55-57 and then unconditionally stores the returned `next` progress at line 59. If a handle's new output exceeds the remaining aggregate response budget, the omitted suffix is not returned on a later async call: the cursor has advanced over it. This is reachable when an async wait/status/list includes multiple live handles or the first handle's output alone exceeds the total cap. The existing output-latest caps bound size but do not make the consumed offset match the displayed output.
- suggested fix: commit per-handle progress only when the entire latest text was emitted, or have the driver advance its byte cursor only through the output portion actually included; add a test where latest output is clipped by `LatestTotalBytes` and the remainder appears on the next call.
- effort: S
- prior art: new

## Performance hypotheses (measurement-step input)

- H1: `activityLatest` reopens, indexes, and scans the transcript on every latest request even though an offset cursor exists (`backend-go/internal/tools/native/async_latest.go:208-255`).
  workload: 10 active sub-agent/profile-message handles, each with an 8 MiB transcript, polled 20 times through `async list`/`status`/`wait`.
  benchmark: add `BenchmarkAsyncLatestActivityPoll` with temp transcripts at 1 MiB and `LatestScanMaxBytes`, repeated `latestBlocks` calls at unchanged and advancing cursors; report bytes read/allocations and time.
  expected: repeated reads scale with transcript size × poll count (at the configured scan cap), including calls with no newly appended transcript data. This is a hypothesis, not measured.

## Cross-phase leads

- None identified beyond the carried-in P6AM-22 context-copy item, reported above within this phase.

## Checked and sound

- `exec` runner planning rejects unsupported modes and bounds code, stdin and argument payload; code-file delivery uses private per-run scratch, and helper files are resolved within configured/workspace roots and opened as regular no-follow files.
- Managed `process` checks the manager/recovery, scopes a child by `ProcessOwner`, uses the shared child environment allowlist, and reports bounded output. Manager `Close` waits for starts to settle before stopping still-running entries; final async notification is sent before `done` closes, while pruning follows `done`.
- Tool Smith listing and staged helper copies bound files, headers and returned entries and enforce directory containment; the bridge uses a private socket, token check, frame limits, tool allowlist, writer/parallel gates and call cap.
- Async latest scrubs control output, separates stdout/stderr, bounds latest bytes and scan size, and excludes reasoning, tool results and user text from agent activity summaries. Activity cursors are retained if a transcript is too large/unreadable; aggregate output clipping is the exception reported as P6B-R4-02.
- Unix group signalling rechecks process-group existence before delayed kill; Windows has the documented direct-process fallback. Orphan classification checks server identity and process start identity before signalling. Scratch sweep restricts itself to stale private directories.

## Not covered

- No full native package suite, build, vet, gofmt check, race run, benchmark/profile, or Windows execution was performed. Only the two targeted native tests listed in the header were run; exit 0.
- Test source was read only where needed for the async latest cap and bridge context forwarding hypotheses; the full test suite and every failure path are not audited.
- No runtime reproduction was made for either finding; they are static source traces labelled `[read]` and rated medium.
