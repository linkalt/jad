# Design note: profile reset gate

Scope: the reset gate and session lock moved out of `httpapi.Handler` into `internal/sessiongate`,
and `Resolver.ResetProfile` (`internal/app/profile_reset.go`) as the one reset path used by the
HTTP reset route and the native `profile` tool.

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
