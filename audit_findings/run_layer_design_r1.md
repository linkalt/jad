# Run-layer goroutine design, round 1

HEAD 968e14d (backend-go). Read-only: no source edits, no staging, no test runs. Measurement is proposed, not run.

## 1. Per-run go statements (verified at HEAD)

| File:line | Goroutine | Waits on | Ends when | Per |
|---|---|---|---|---|
| httpapi/handler.go:472 | HTTP run driver: `executeRun` | run itself; SSE is served by the request goroutine | executeRun returns | run |
| httpapi/profile_run.go:133 | profile message driver: `executeRun` | run itself | executeRun returns | run (profile) |
| httpapi/async_state.go:52 | async-state watcher: publishes on `wake` or `interval` timer | `done`, `wake` | `done` closed by stop, deferred in executeRun (`executor.go:56`) | run, only if asyncmgr is configured |
| app/remote_api.go:233 | `remoteMessageRun.watch`: timeout timer, then `started.Done()` | started run `Done()` or timeout | after the run ends (or after timeout + Stop) | remote message run |
| app/remote_api.go:364 | `callSubagentTool` wrapper | `ExecuteFunctionCall` return (the start phase only on the handle path) | after start | remote subagent call |
| app/remote_api.go:389 | `<-run.Done(); cancel()` | run end | run end | remote subagent run (kept: site 4 blocker) |
| app/profile_message_queue.go:274 | `set.pump` | queue | queue empty | per target queue, not per run |
| app/profile_message_queue.go:497 | attach completion: `<-run.Done(); finish(conclude)` (queued) or select with `ctx` (non-queued) | run end | run end | profile message |
| app/service.go:96 | storage recovery | RecoverStorages | once at start | process |

Not per run: `time.AfterFunc` in registry.go:844 (retention, fires once per run, no goroutine until it fires), run_content_cache.go:518 and message_stream_emitter.go:102 (timers, goroutine only at fire), asyncmgr `Watch` (map registration, no goroutine), `serveSSE` (request goroutine). `session/tool_batch.go:138` is the leaf/batch layer, out of scope.

Per active run today: HTTP run = 2 (driver + async watcher, plus the request goroutine). Profile message = 2 (driver + async watcher) + 1 (attach waiter) = 3. Remote message = profile driver (133) + async watcher (52) + remote watch (233) = 3. Remote subagent = 1 wrapper (start only) + 1 end waiter (389).

## 2. Classification

- **Required:** 472 and 133 (the run must outlive the request or the tool call on the async path); 364 (it races the blocking call against `observed`/`finished`/`ctx`).
- **Mergeable, needs a hook:** 233 (`remoteMessageRun.watch`) can become an end hook on the run plus a `time.AfterFunc` for the timeout. The result is built on the finishing goroutine, as D1 did. Blocker: `activeRun` has no end hook yet. `finish` (registry.go:~837) closes `done`, so one hook there is enough.
- **Mergeable, deferred:** 497 (the completion waiter) is already what D1 did. The async watcher (async_state.go:52) is the largest per-run item, but see proposal A.
- **Kept:** 389 (site 4 blocker, DECISIONS: cancel must stay tied to run end).
- **Not per run:** 274 (pump, per target queue) and service.go:96 (process).

## 3. Proposals, ranked by goroutines saved per active run

**A. One async-state publisher per manager, not per run. Saves 1 per active run (2 to 1 for an HTTP run).**
- Behaviour change: publish timing for each run keeps its `toolStreamInterval` throttle, but it is run by one shared loop. Order per run is kept. The `read()` call moves off the `Watch` notify, so notify still only signals.
- Blocker to check first: `asyncmgr.Manager.Watch` notify runs under `m.mu` and must not block (manager.go:169). A shared loop needs its own wake channel, not a blocking send. The existing DECISIONS note about `stop` under `m.mu` (manager.go:181-190) still applies.
- API change: `httpapi.AsyncSource.WatchSessionAsync` would need to be registered by the server, not per run. Internal only, no wire change.

**B. End hook on `activeRun` for the remote message watcher (233). Saves 1 per remote message run.**
- Behaviour change: none on the result; `timed_out` is set by an `AfterFunc` that calls `started.Stop()`. The `<-started.Done()` wait after Stop moves to the end hook, so it is bounded by the run ending, as now.
- API change: internal `activeRun` hook only. No wire change.
- Blocker to watch: the hook runs on the finishing goroutine, so it must not hold `r.mu` (finish closes `done` under the lock, then unlocks).

**C. Run the sync profile message on the tool goroutine (133). Saves 1 per sync profile message, not per async run.**
- Behaviour change: the caller already waits, so nothing changes for sync calls. Async/handled calls keep the goroutine.
- Blocker: needs the tool to know whether the call is sync. That is the native/profile_message_async.go split that DECISIONS calls a larger change. Not recommended now.

## 4. Named blockers (not proposed)

- 389: site 4 (DECISIONS).
- 364: the blocking `ExecuteFunctionCall` cannot be merged without an API that returns at start.
- 472: the SSE needs the request goroutine; the driver is required.
- Max concurrent runs default: deferred (DECISIONS). The 1000-run number assumes no cap.

## 5. Measurement design (not run)

- Test: a new `TestScaleRunLayerGoroutines` next to `TestScaleRunAdmission` (`httpapi/registry_scale_test.go:77`), with `scaleSkipUnlessMeasured` as the gate. Env gate: `JAD_SCALE_MEASURE=1`.
- What it counts: take `pprof.Lookup("goroutine").WriteTo(buf, 1)` after the N runs are blocked on a fake runner. Count stacks containing `executeRun` (driver), `watchAsyncState` (async watcher), `remoteMessageRun).watch` (remote watcher), `StartSubagent.func` (wrapper), `attach.func` (completion waiter), `set.pump` (follower). Print one labelled line per N.
- N = 100, 500, 1000. Compare each count to N (expected 2N for HTTP runs, 3N for profile runs).
- Output line format: `run-layer N=<n> driver=<d> async_watch=<a> remote_watch=<r> wrapper=<w> attach=<t> pump=<p> other=<o>`.
- Baseline is taken at the same HEAD before any proposal, so the saving is a measured difference.

## 6. Not verified

- I did not check whether a shared publisher (A) changes the per-run throttle timing under load; that needs a design review.
- `TestScaleRunAdmission` and `TestScaleToolBatchFanOut` were not read in full.
- Goroutine counts are from the code, not measured. The measurement above has not been run.
- Lines are from HEAD 968e14d. Older DECISIONS line numbers (for example 506) have moved.
