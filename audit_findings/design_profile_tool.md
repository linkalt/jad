# Design note: profile reset gate

Scope: the reset gate and session lock moved out of `httpapi.Handler` into `internal/sessiongate`,
and `Resolver.ResetProfile` (`internal/app/profile_reset.go`) as the one reset path used by the
HTTP reset route and the native `profile` tool.

## Compact during a live run waits on both paths

A compaction sent while a run of the profile is live **waits for the run**, on the HTTP compact
route and on the native profile tool alike, as DECISIONS.md records (the measured behaviour, kept).
Neither path refuses it. Both take the same sequence through `Resolver.CompactProfile`
(`internal/app/profile_compact.go`): the mutation gate (a second mutation of the profile is
refused at once with 409), the in-process session lock, then the cross-process transcript lock,
which waits for another process's run. `Session.Compact` then waits on the in-process run lock.

- **Difference kept on purpose:** the HTTP route also orders starts behind a compaction through
  the httpapi turn queue (`startTurns`). The app-side function does not, because that queue is
  private to httpapi. The gate already refuses a second mutation at once, so the queue only orders
  starts, not mutations, and a start that races a compaction is still refused by the transcript
  lock.
- **Cross-process gap, accepted.** The gate is per process. A second server process does not see
  this process's gate, so a mutation there is not refused by it. What a second process is still
  guarded by is the cross-process transcript lock: a compaction or a run of the same transcript in
  the other process waits for it, so the transcript is never changed by two processes at once.
  Only the refuse-at-once for a second mutation is per process. Covering that across processes
  would need a cross-process gate, which this change does not add; no existing pattern gives one
  cheaply.
- Test: `TestToolAndHTTPCompactWaitForALiveRunAlike` (`internal/app/profile_compact_live_run_test.go`):
  a live run holds the tool compaction, a concurrent HTTP compact gets 409 at once, and the tool
  compaction finishes after the run ends. The old tool path (no gate) fails it (mutate-check: CAUGHT).

## Known gap: gate-order ordering check is not mechanically caught

`ResetProfile` must clear the mutation gate before it gives back the process lock. The
mutation check that swaps those two lines (`r.gate.End(key)` moved after `processUnlock()`)
is **not caught** by `TestResetHoldsTheGateWhileItWaitsForAnotherProcessAndClearsIt`
(`internal/app/profile_reset_order_test.go`).

Why: the cross-process test checks the gate while the reset waits and after it returns. It
does not start a second mutation in the window between the process-lock release and the gate
clear, so it cannot tell the two orders apart. A deterministic test needs a seam in the release
path, which this change does not add (no production hook).

Decision: recorded as a known gap, checked by review only. A reviewer must confirm that
`ResetProfile` clears the gate (`r.gate.End`) before the deferred `processUnlock()` runs.
