# Phase 6c Audit Review — r4

## Findings

## Understanding note

## Checked and sound

## Not covered

# Phase 6c Audit Review — r4

- phase: p6c; reviewer: r4; HEAD: 3bf8c33
- files read: all 19 in-scope non-test files listed in phase6c_prompt.md; relevant test files read as evidence
- commands: `cd backend-go && go test ./internal/tools/native -run '^(TestRemoteSubagentAnswersWithinBlockMs|TestRemoteProfileMessageAnswersWithinBlockMs)$' -count=1` (exit 0); no build, vet, gofmt, benchmark, full-suite, or race command
- date: 2026-10-29

## 1. Understanding note

The native subagent tool constructs per-agent catalogs and starts managed child runs; local and remote calls may be async, carry ownership/depth/hop context, persist run manifests and expose run status/transcripts. `profile_message` routes local/cross-session or configured remote requests, inline or async, through a messenger plan; async handles and subagent runs are attached to owners and cancelled with lifecycle events. State-context functions project manager state into bounded item/count sections. Reviewed phase decisions and prior-art references for these behaviors.

## 2. Hypotheses

- H1: nested execution context and gating preserve depth/root/hops and prevent bypasses — verified by source trace and focused tests.
- H2: local run-manager Close, cancellation, queue/slot release and manifest recovery settle reviewed paths; remote StartRun/follower-registration race remains P6C-R4-01.
- H3: remote references, allow-lists, hops, timeouts and result text are bounded/scrubbed on reviewed paths; StartRun cleanup race remains P6C-R4-01.
- H4: cross-session profile addressing keeps identity and access scope distinct — verified by source and tests.
- H5: task/answer/change/error outputs are bounded on reviewed paths — verified by source and tests.
- H6: statecontext counts and trim behavior preserve tested count/snapshot semantics — verified by source and tests.
- H7: maintainability and scaling reviewed; two unmeasured performance hypotheses are listed in section 4.


### P6C-R4-01  Remote start can escape cancellation when shutdown races follower registration
- severity: medium
- lens: A correctness | B security
- location: `backend-go/internal/tools/native/profile_message_remote.go:121-134`; called by `backend-go/internal/tools/native/subagent_remote.go:127`
- evidence: [read] `startRemoteRun` first calls `client.StartRun` and receives the remote run ID, then calls `directory.Go` to register the polling/cancellation goroutine. `remote.Hub.Go` rejects the callback after `Hub.Close` sets `closed` (`backend-go/internal/remote/hub.go:80-92`); on that rejection the helper returns `remote_unavailable` without issuing `StopRun`. The successful remote start is already a side effect (`backend-go/internal/remote/run.go:72-91`). Thus a shutdown concurrent with a slow/just-completed start can leave a remote profile message or subagent running without a local follower; this is especially consequential when a configured remote agent can change files. Existing `TestRemoteSubagentCloseLeavesNoGoroutines` (`subagent_remote_test.go:257-274`) checks local goroutine cleanup, not that the remote run was stopped in this registration race.
- suggested fix: if `Directory.Go` returns false after a successful start, make a bounded best-effort `client.StopRun` for the returned ID before returning the shutdown error; add a test that closes the hub between remote start acceptance and follower registration and asserts remote stop.
- effort: S
- prior art: new (remote run lifetime risk from phase prompt)

## 3. Additional findings

No additional confirmed findings beyond P6C-R4-01. Focused evidence checks support the existing bounded-output, owner-isolation, nested-run gating, remote hop, state-context count/trim, and local shutdown behavior; the relevant test snippets are referenced in the review notes. The shutdown race finding remains un-reproduced by a dedicated test.

## 4. Performance hypotheses for measurement

- H1: Persistent manager recovery cost scales with manifest count and per-manifest contents (`backend-go/internal/tools/native/subagent_manager.go:100-108`; `subagent_store.go:211-221`). Workload: 1k–5k retained run manifests on startup. Benchmark: existing `BenchmarkSubagentRecoverManifests` seeds 1k and 5k manifests and constructs/closes a manager. Expected: linear disk reads/decodes and allocations with manifest count; measure before considering changes.
- H2: Per-session subagent state-context snapshot allocates and sorts all retained owner runs before applying the item limit (`subagent_manager.go:864-876`; `subagent_statecontext.go:48-84`). Workload: 1k+ retained runs for one session, small configured limit. Benchmark: add a fixture with 1k/5k manifests or retained runs and compare collection latency/allocations by `Limit`. Expected: O(n) views plus O(n log n) sort even when returning only a few items; unmeasured.

## 5. Cross-phase leads

None recorded.

## 6. Checked and sound

- Nested child context and per-root/session/concurrency gates: source-traced; focused tests exercise run-scope propagation and refusal without queueing excess nested work.
- Local subagent manager close cancels detached children and waits; descendants and child profile-message calls receive cancellation. Owner-bound async handles reject cross-session status/stop operations.
- Remote start/wait/stop/hop, local timeout stop, result path scrubbing and close follower cleanup have coverage. The specific start-success/follower-registration shutdown interleaving in P6C-R4-01 is not exercised by the close tests.
- Cross-session profile-message selection and remote allow-list/refusal are covered; returned error/message text is bounded/scrubbed on the reviewed paths.
- Subagent answer/error partial text, changed-file lists and persisted changed-file recovery have tests for limits and failure outcomes.
- State-context summary counts, detail limits, newest-first ordering and oldest-first trimming have focused tests.

## 7. Not covered and why

- No full Go test suite, build, vet, gofmt, race detector, benchmark or dedicated shutdown-race reproduction was run. One targeted command was run: `cd backend-go && go test ./internal/tools/native -run '^(TestRemoteSubagentAnswersWithinBlockMs|TestRemoteProfileMessageAnswersWithinBlockMs)$' -count=1` (exit 0).
- Platform-specific owner implementation behavior on non-host OSes was read but not executed; no cross-compilation performed.
- Performance hypotheses H1–H2 are unmeasured; proposed benchmark/workload is stated above.
