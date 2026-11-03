# Goroutine design round, second review: profile queue + remote follow paths

HEAD `45a9df3`, read-only. Read DECISIONS.md "Goroutine spawn removals" first; skipped the done sites (bash_child reader, non-queued merge `83eb386`) and the decided ones (site 1 async throttle, site 4 `remote_api.go:389`, site 5 `subagent_remote.go:165`, `remoteapi/server.go:490`). Read via grep/sed on exact ranges (index `get_code_snippet`/`search_graph` first for `beginQueued`, `finish`, `follow`, `startRemoteRun`); `internal/app/profile_message_remote.go` does not exist, it is `internal/tools/native/profile_message_remote.go`. No test run (design round).

## Per-run goroutines, what each waits on

| # | Site | Waits on | Count per run |
|---|---|---|---|
| A | `app/profile_message_queue.go:344` `go r.watch()` | `doneC` / `r.ctx.Done()` | 1 per queued message, whole life |
| B | `app/profile_message_queue.go:498` queued attach waiter | `run.Done()` | 1 per queued message, from start |
| C | `native/profile_message_async.go:190` async wrapper | `Started()`/`Done()`, then `Done()` | 1 per handled message (local **and** remote) |
| D | `native/profile_message_remote.go:126` `directory.Go(follow)` | `Client.Follow` long-poll (`remote/run.go:133`), or the `MaxInFlight` slot semaphore (`remote/client.go:174-177`) | 1 per followed remote run, whole run; pairs with 1 held net/http handler on the peer (`remoteapi/server.go:590-604`, no extra goroutine there) |
| E | `native/subagent_remote.go:183` async remote waiter | `run.Done()` | 1 per handled remote subagent run |
| F | `native/profile_message_remote.go:229` `Stop` | `StopRun` (3s cap) | 1 per *stopped* remote run, transient |
| G | `app/profile_message_queue.go:274` `go set.pump` | `StartProfileRun(wait=true)` | **0 extra**: one per target queue, `q.pumping` guard at `:233`, set at `:269` |

So a queued async message holds 2 goroutines while waiting (A+C) and 3 while running (A+B+C); a followed remote run holds 1 (D) plus 1 if async (E).

## Proposals (each: saved / behaviour / proving test)

1. **A+B→1, then A→0 (`:344`, `:498`).** Give `attach` the same body already merged for the non-queued path (`:506`: select `run.Done()` / `ctx.Done()`→`Stop`→wait→`finish`). The `if r.queued` branch at `:497-503` then dies, and `watch`'s only remaining job is the pre-start phase → replace `go r.watch()` with `r.watchStop = context.AfterFunc(r.ctx, r.watch)` (`context.go:326-341` is goroutine-free while pending and only spawns on cancel). Saved: 1 per queued message while waiting, 1 while running. Behaviour: unchanged — `ProfileRun.Stop` is idempotent (`httpapi/registry.go:847-855`), and `watch`/`finish` already tolerate racing each other. Test: hold a target, enqueue 50 messages, assert `runtime.NumGoroutine` delta ≤2 while they wait (≈50 today) plus the existing stop/timeout/queue-expiry tests; mutate-check by deleting the `watchStop()` call.
2. **C+E→hook (1 saved per async message).** Both are pure `<-Done()` waiters that call `Complete`/`dropProfileMessageRun`. Move them to `onStart`/`onEnd` hooks on `profileMessageRun`/`remoteRun`, fired where `attach` closes `startedC` and where `finish`/`follow` already run. Saved: 1 per handled message. Behaviour: hooks must be registered **before** `plan.Start` (a non-queued message can finish before `Start` returns), and `Complete`+`drop` order kept. Test: existing async completion tests plus a goroutine-delta check over 50 handled messages (1/run, not 2).
3. **D: one follower per remote instead of per run.** `Follow` is one long-poll HTTP request per run, so one goroutine each; the per-client `MaxInFlight` semaphore already throttles the wire, not the goroutines. Proposal: a batch wait on the peer, `POST /api/remote/v1/runs/wait {ids, wait_ms}`, held until any listed run ends or the deadline (one select over the entries' done channels); the hub then keeps one follower goroutine per `Client` over its active-run set, with `Follow`'s gap/lost/backoff state moved into a per-run struct. Saved: `runs − remotes` on the client **and** the same count of peer handler goroutines — the only design that cuts the net/http handlers DECISIONS left alone. Behaviour: wake-on-end latency kept, 1 request per remote per cycle; needs a capability/fallback to the per-run path for older peers. No-protocol-change variant: one goroutine per `Client` iterating `PollRun(wait=0)`; same saving, end latency becomes ~1 cycle (min poll gap 100 ms) and one slow run delays the others. Test: 100 runs followed on one fake remote, delta while all run ≤ remotes+slack vs ≈100 today, all outcomes intact, one stopped run, one `ErrLost`.
4. **F (minor, optional).** Route `Stop` through the same per-remote follower so a stop needs no goroutine. Saved 1 per stopped run, transient; otherwise harmless.

## Fan-out and leak risk

- **No unbounded fan-out found**: nothing spawns per poll iteration (`Follow` loops inside one goroutine), the pump is per queue, there is no hub subscriber list, and a queued waiter costs exactly one watch. All growth is linear in live runs.
- **Leak risk, proposal 1:** `finish` must call `watchStop()` *before* `r.cancel()`, else every normal completion cancels `r.ctx` and spawns `watch` (count unchanged, transient spawn per message).
- **Leak risk, proposal 2:** the hook must fire on `startFailed`/finish-without-run too; a hook only wired into `attach` leaves async entries `running` forever — the exact leak the C/E wrappers avoid today.
- **Leak risk, proposal 3:** the shared follower must drop each run on terminal/lost/hub-close and must always close `run.done`, else waiters block forever; needs a close-with-runs-in-flight test.
- Existing code checked for leaks and clean: `attach`'s goroutines all end on `run.Done()`, `pump` exits only through its empty-queue branch (`:155-158`).

## Not covered
Queued-waiter numbers assume `max_queued_per_profile` > 0; no measurement was run (design round), so every "saved" figure is a spawn count, not a measured peak; `httpapi` run-driver and net/http goroutines excluded by DECISIONS.
