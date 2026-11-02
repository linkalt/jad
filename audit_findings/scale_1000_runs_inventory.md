# Scale inventory: design limits for ~1000 concurrently active runs

Read-only audit, HEAD 24f005a. Nothing was measured: every severity and "breaks at 1000" claim comes from reading code. Scope was chosen by the user: all design elements, not only examples. The three parts were run by separate reviewers and are kept verbatim below.

- Part A: session/profile identity, Resolver, transcript/journal, sessionfs, usage cache, compaction, sessiongate, globals (reviewer0).
- Part B: tool dispatch, provider clients, run loop (reviewer2).
- Part C: HTTP/SSE, sub-agents, asyncmgr, remote, logging, serverconfig (reviewer3).

## Part A

### Inventory

| ID | Location | Shared thing | Why it fails at 1000 active runs; trigger | Sev | Design change (structure) | Breaking? | Evidence |
|---|---|---|---|---|---|---|---|
| A1 | `internal/app/resolver.go:71` | One `Resolver.mu` guarding ~15 maps | Short holds (I/O is done outside it), but every resolve, store, manager and build takes it. Contention grows with run starts/sec. | degrades | Split by kind: one lock per map family, or sharded maps keyed by storage path | No | `resolver.go:925-955`, `:653-690` release before I/O |
| A2 | `resolver.go:255-273` | Per-storage maps with no eviction: `stores`, `profileTurnCounts`, `processManagers`, `subagentManagers`, `asyncHosts`, `mcpManagers`, `taskStores`, `memoryStores`, `fetchStores` | Entries removed only on session delete (`session_catalog_files.go:111-167`, `forgetStores` `:161`). `profileTurnCounts` (`catalog.go:343-352`) is never deleted from. Memory grows with session count, not with active runs. | degrades | Idle eviction with reference counts, or LRU for inactive sessions; entries keyed by session, not by path | No | grep `delete(` in `internal/app` = 27 sites; none for `profileTurnCounts` |
| A3 | `resolver.go:458-471` | `sharedSession.runners` map, created per store (`resolver.go:1009`) | Keyed by a fingerprint, so one runner per model/key combo per session. No eviction. Grows with distinct configs. | minor | Bound the map or key by profile; the runner is cheap to rebuild | No | `resolver.go:458`, `:471` |
| A4 | `internal/transcript/jsonl.go:35`, `:296`, `:368` | Journal `mu` held for the whole visit of `ReadRecordsRange` and `ReadCompleteRecordsRange` | A usage scan or read of a long transcript blocks every append to that session. | blocker for a session with a large transcript; not for the 1000-session total | Snapshot end under lock, read `[start,end)` lock-free (the file is append-only) | No, internal | `jsonl.go:264-300`, `:358-420` |
| A5 | `internal/transcript/line_index.go:12-48` | Per-journal `lineIndex` (SHA-256 of every line) kept in memory | Index holds one hash per line for every session that has written since start. Memory is O(lines), with no eviction and no bound. | degrades (memory) | Persist the index or bound it (LRU by session); rebuild from the tail on reopen | No, if rebuilt; additive if persisted | `line_index.go:20-28` |
| A6 | `internal/transcript/turn.go:158-216` | `TurnStore.mu` held in `BeginTurn` and `unregister`; `activeTurns` map | Brief holds, but `Compact`, `Reset` and `Recover` take the same `s.mu` for their whole duration (`maintenance.go:59`, `:669`). A compaction stalls `BeginTurn` for that session. | degrades | Make compaction a copy-then-swap so `s.mu` is held only for the swap | No | `maintenance.go:59-61`; `turn.go:158`, `:213` |
| A7 | `internal/app/maintenance.go:78`, `:142`; `bound_runner.go:32`; `front_compact.go:26`; `state_context_user.go:35`; `checkpoints.go:32` | `shared.runMu` (per-profile channel mutex, `run_mutex.go`) taken by runs, compaction, reset, checkpoint, state context | Runs and mutations of one profile serialize. A compaction waits for a live run of the same profile. Cross-session no effect. | degrades (per profile) | Keep as a per-profile lock, but split run-start from read-only paths so reads never take it | No | `run_mutex.go:9-30` |
| A8 | `internal/sessiongate/gate.go:9-40` | One `Gate.mu` for mutations, restoring map, and the `sessions keyedLocker` | `Begin`/`End`/`WhileFree`/`Held` all take `g.mu`. `WhileFree` runs `fn` while holding `g.mu` (`:52-57`), so a run start's registration is serialized behind the global lock. | degrades | Use a per-key lock for `WhileFree`, or sharded maps by session key | No | `gate.go:9-17`, `:52-57` |
| A9 | `gate.go:91-120` (`keyedLocker`) | Per-session in-process lock with `refs` | Refcount is correct, but the map lock is global. Fine for 1000 sessions; the single map lock is the cost. | minor | Shard the map | No | `gate.go:91-120` |
| A10 | `internal/sessionlock/lock.go:103-134` | Cross-process file lock, polling every `acquirePollInterval` | Waiters poll; 1000 waiters on one file = 1000 pollers. Only matters across processes. | minor | Use blocking `flock` with a context-aware wait, or a notify channel | No | `lock.go:103-134` |
| A11 | `internal/app/resolver.go:500-532` (`newProviderStreamer`) | Provider client per runner key | One client per runner, shared per session store. The default transport is shared; non-default network, OAuth or HTTP debug build a new one each time. | degrades | Pool transports by (network, auth, debug) key | No | `resolver.go:499-531` |
| A12 | `internal/app/locked_config.go:14-55` | `lockedConfigs.mu` and `entries` map, 1 s TTL | TTL bounds size in time, but not in count; a burst of 1000 distinct keys holds 1000 entries for 1 s. | minor | Bound by count too | No | `locked_config.go:15`, `:34-55` |
| A13 | `internal/app/usage.go:114-160`, `:203-213` | Usage cache write: `os.Stat`, hash of 64 KB under the caller's journal lock, then write | Called from inside `scanTranscriptIndex` (`catalog.go:504-625`), i.e. under the journal lock on the usage path. Each advancing scan rewrites `usage.json`. Fsync not read. | degrades | Write the cache after releasing the journal lock; coalesce writes | No (cache schema stays v1; a version bump rescans) | `usage.go:119-135`; `catalog.go:580` |
| A14 | `internal/app/turn_starts.go:32-60`, `:174-180` | `turnStartsCache` per shared session; a full rebuild when generation changes | A compaction or reset resets it (`maintenance.go:90`, `:149`, `front_compact.go:33`), so the next read rescans the full transcript, under `turnStartsMu` and the journal lock. | degrades | Persist the turn index and version it by generation | Additive (new cache file); old caches ignored | `turn_starts.go:41-60` |
| A15 | `internal/app/catalog.go:343-352` (`profileTurnCounts`) | Map under `profileTurnsMu`, no eviction | Grows with every profile listed. | minor | Evict with the store | No | `catalog.go:343-352` |
| A16 | `internal/checkpoint/service.go:183-195` | Package-level `sync.Map` for `gates`, `locks`, `repoSizes` keyed by staging dir | Never deleted from. Grows with every checkpoint staging dir ever used. | minor | Move onto the owning store object; delete on close | No | `checkpoint/service.go:183-195` |
| A17 | `internal/transcript/jsonl.go:35-40`, `:264-300` | One `Journal` per transcript, one `mu`, whole-file reads with `os.OpenFile` per read | Each read opens and repairs the file (`withRepairedFile`), and repair can write. Reads and appends to one session serialize. | degrades | Single-writer append file plus snapshot reads (see A4) | No | `jsonl.go:264-300`, `:296-300` |
| A18 | `internal/sessionfs/catalog_scans.go:18-91` | `s.mu` in a catalog scan cache | Scan lock is held while it reads the directory tree (`:65-91`). A session list with many sessions holds it for the scan. | degrades (listing) | Scan outside the lock; publish the result | No | `catalog_scans.go:65-91` |
| A19 | `internal/sessionfs/catalog_files.go:449` | Package-level `categoryDirectoryMu` | One process-wide mutex for category directory creation. Serializes all session creation. | minor | Per-category lock | No | `catalog_files.go:449` |
| A20 | `internal/app/skipped_sessions.go:13-36` | `s.mu` around a skipped-sessions set | Global set of skipped sessions, locked for reads and writes. Scales with the number of broken sessions. | minor | Keep; move to the session list cache | No | `skipped_sessions.go:20`, `:36` |
| A21 | `internal/app/profile_message_queue.go:38-47` | Per-profile message queue with `s.mu` | Queues per target profile; one lock per queue. Fine per target, but a single hot target serializes all callers. | degrades (hot target) | Bounded queue with per-target channel | No | `profile_message_queue.go:38`, `:47` |

### Lock-order pairs found (Part A)

- **`runMu` then `s.mu`** (run start, compaction): `bound_runner.go:32` → `turn.go:158`; `maintenance.go:78` → `maintenance.go:59`. Consistent everywhere read.
- **`Gate.mu` then `keyedLocker.mu`**: `gate.go:52-57` holds `g.mu` in `WhileFree`; `Lock` takes `l.mu` via `g.sessions`, but does not call under `g.mu`. No reverse path seen.
- **`Resolver.mu` then `turnStartsMu`**: not taken together. No reverse path.
- **`Journal.mu` then usage cache write**: `catalog.go` scan holds `j.mu` and calls `writeUsageCache`. Nested lock with I/O inside. No reverse order found.

Only the sites above were checked; the full lock-order graph was not built.

### Not verified (Part A)

- Measured cost of anything above.
- `usage.go` `writeUsageCacheCheckpoint` after line 144: fsync and rename.
- `session_catalog_files.go` beyond lines 103-175.
- `interactionbroker` and `httpapi` registries (read only at call site).
- `internal/transcript/read_records.go`, `input_history.go`, `session_turn.go`, `run_error_cache.go` bodies.
- `internal/sessionfs/roots.go` and `session.go` package vars.
- `internal/app/config.go` and `remote_listen.go` globals.
- `recovery.go` (`recovering` flag) logic.
- The `sessionlock` non-unix build (`lock_unsupported.go`).
- Whether `acquirePollInterval` is a constant or configurable.
- Every `delete(` site in `internal/app` (27 counted; 4 were read).


## Part B

Source: reviewer2 report, verbatim.

# Part B — tool dispatch, provider clients, run loop (HEAD 24f005a)

Scope: `session` batch/runner, `tools` catalog, `native` shell/exec/file, output caps, provider HTTP/transports/retries/timeouts/stream parser, run-loop goroutines/channels/contexts/buffers. Index-first (`get_architecture`, `search_graph`/`search_code`, snippets) + exact-range `read.sh` fallback. No edits.

## Findings

**B-01 per-response workers, no global ceiling — blocker.** `session/tool_batch.go:119-124` (`workers=min(limit,len); go worker(); wg.Wait`), limit via `runner_tools.go:254`, max 64. Shared: worker goroutines + concurrent `ExecuteFunctionCall`. 1000 runs×64 ≈64k goroutines + 64k concurrent tool execs (JSON/schema/file/child each). Trigger: simultaneous tool responses. Fix: global tool scheduler (weighted semaphore/queue), not a number. Breaking: no. Old data: yes. Verified.

**B-02 `Runner.mu` held for whole `Run` — degrades.** `session/runner.go:228-233,404-406` (`Lock; defer Unlock` around stream+tool loop). One Runner=one session: concurrent Runs there serialize; slow provider/retry sleep blocks others. Trigger: concurrent same-session runs. Fix: narrow mutex to persist/round-state (structure). Breaking: no. Verified.

**B-03 admission uncapped by default; child runs bypass cap — blocker.** `httpapi/registry.go:300-311` (`maxRuns 0=no cap`), `:326-344` (check only if `source==""`), `finish` uses `time.AfterFunc(retention)` per run. 1000 begins allowed by default; subagent/remote (`source!=""`) escapes even configured cap. Trigger: 1000 begins or nested storm. Fix: count all running runs in one budget (or two explicit budgets) + bounded retire queue vs timer-per-run. Breaking: default-cap change is behavioral; 429 path exists so API same. Verified.

**B-04 one parked goroutine per run — degrades.** `httpapi/executor.go:30-` (`executeRun` runs `runner.Run` on run goroutine + emitter + `watchAsyncState`), `app/bound_runner.go:31-131` (holds `runMu` through blocking run). 1000 streaming/retrying runs =1000+ stacks + emitter buffers. Trigger: 1000 concurrent streams. Fix: bounded run-slot scheduler. Breaking: no. Verified.

**B-05 provider transport: shared-default vs per-run clone — blocker.** `app/resolver.go:508-536` (nil=default shared; else new `http.Client`), `provider_network.go:18-28` (`DefaultTransport.Clone()`+DialContext), `resolver.go:499-503` (built per `prepareRun`); `httpdebug/transport.go:53-139` per-run logger. Default: 1000 share one pool (idle-pool contention). Non-default (network/oauth/debug): 1000 Transports, zero reuse, FD/timer blowup. Trigger: custom network/oauth/debug at scale. Fix: transport pool keyed by config fingerprint. Breaking: no. Verified.

**B-06 retry storm, per-run sleeps — degrades (blocker on outage).** `openaiapi/client.go:openStream` (`WithMaxRetries(0)`+own loop, `guard.Stop` during sleep), `retry.go:25-34` (MaxRetries/Initial/MaxDelay, `sleepContext`), SSE `provider_retry` per attempt. 1000 runs retry 429/5xx independently in lockstep; goroutine parked per wait. Trigger: provider outage. Fix: global retry budget/jitter coordinator + breaker (structure). Breaking: no. Verified.

**B-07 per-run staged stream state unbounded — degrades.** `session/runner_response.go:16-151` (`stagedItems map[int64]RawMessage`, `cloneRaw`+`StageOutputItem` per `output_item.done`), `openaiapi/client.go:74-117` (copy+`json.Valid` per event). Long/large responses grow map ×1000 runs. Trigger: large outputs. Fix: per-response event/byte budget with disk spill. Breaking: no (additive). Verified.

**B-08 timer per round + no header timeout — minor.** `openaiapi/idle_guard.go:36-71` (`WithCancel`+`AfterFunc` per `Stream`). 1000 rounds=1000 timers; `ResponseHeaderTimeout=0` so trickling headers hold conns until idle fires. Trigger: slow headers. Fix: header timeout on shared transports + shared deadline wheel. Breaking: no. Verified.

**B-09 per-call JSON/schema roundtrips — degrades.** `tools/catalog.go:345-449` (Unmarshal→decode→normalize→Marshal→validate→marshal output; issue bound `schema.go:54-71` only). 1000×N concurrent calls allocate maps/strings; GC/CPU scales with calls. Trigger: tool-heavy rounds. Fix: streaming decode + buffer pool. Breaking: no. Verified.

**B-10 shell/exec FD+process fan-out, no global budget — blocker.** `native/bash_child.go:66-` (2-3 `os.Pipe` per child + fork), `bash.go:117-147` (no semaphore), `process_manager.go:80-` (`OwnerLimit` only with Owner). 1000×64 bash ≈10ks processes + ~4 FDs each → EMFILE/PID/load. Trigger: model emits bash/process widely. Fix: process-wide exec semaphore covering sync+async+children. Breaking: no (refuse like `async_capacity_reached`). Verified.

**B-11 async capped but detached work piles — degrades.** `native/shell_async.go:156-247` (`WithoutCancel` ctx, `go func`/handle, `NewTimer` per wait), `asyncmgr/config.go:14-` (MaxHandles/Total/UnreadMax/KeepFinished). Caps refuse (good) but 1000 owners pin contexts/goroutines/result files + `Latest*` scans until read. Trigger: many background procs/agents. Fix: unify sync+async in one budget + age/size result GC. Breaking: no; old records load. Verified.

**B-12 MCP one conn + per-server lock — degrades.** `tools/mcp/manager.go:369-467` (`serverLock` across ReadFile+`Connect(WithTimeout)`; single `connections[id]`), `:601-622` per-reconnect client. 1000 callers on one server serialize on dial + share one session (head-of-line; timeouts cascade). Trigger: popular MCP server. Fix: small pool per server + non-blocking dial. Breaking: no. Verified.

**B-13 fetch per-call caps only — minor.** `native/fetch.go:166-` (per-call limit/TimeoutMS, `CreatePage`). 1000 concurrent fetches = sockets + page writes, no global concurrency cap. Trigger: fetch-heavy. Fix: global fetch semaphore + shared pool. Breaking: no. Verified entry caps; store-wide quota inferred.

**B-14 file/read per-call byte cap only — degrades.** `native/read.go:262-331` (`MaxFileBytes` + verified read). Each bounded, but 1000×cap resident + glob/grep/ripgrep scans multiply. Trigger: many reads/scans. Fix: global scan/read byte budget. Breaking: no. Read verified; glob/grep paths inferred.

**B-15 spill per-stream bounded, count unbounded — degrades.** `native/bash_output.go:19-22` (head/tail limits), `bash_spill.go:71-320` (throttled prune), `bash.go:306-340` (result-file cut). 1000 runs × spill/result files fill disk; throttled prune overshoots in bursts. Trigger: verbose commands. Fix: dir quota + refcounted GC. Breaking: no. Verified.

**B-16 SSE fanout + per-run images — blocker (mem).** `httpapi/registry.go:25-47` (`eventQueue{queue,bytes,maxBytes,held}`), `:249-298` (`subscribers,latest*,content,liveImages`), publish fanout under `mu` (drop/close on overflow), `beginSubscribe` snapshot+hold/release. 1000 runs × subs × `maxBufferedBytes` + decoded `liveImages` + content caches; copies per subscriber. Trigger: watchers + image results. Fix: shared immutable snapshots + image LRU/spill. Breaking: no. Verified.

**B-17 per-round rebuild + re-reads — minor.** `session/runner.go:460-` (rebuild `ResponseTools`+request, re-expand history each round), `bound_runner.go:38-131` (`scanTranscriptIndex`, `prepareRun`, state-context snapshot/append), `executor.go` usage publish per commit. O(history) × rounds ×1000. Trigger: many rounds. Fix: incremental builder + cached index. Breaking: no. Verified.

**B-18 checkpoint/hooks/compaction inline — degrades.** `session/runner.go` (`checkpointRun.Start/BeforeCalls/Finish`, `compactAfterResponse`), `bound_runner.go` hooks. 1000 concurrent git snapshots + transcript rewrites contend on disk/lock. Trigger: mutating tools + auto-compaction. Fix: async/coalesced snapshots + compaction queue. Breaking: no. Inferred (bodies not fully read).

**B-19 nested runs multiply all above — blocker.** `tools/catalog.go:ExecutionContext` (Depth/RootRun/Hops/Owner/Scope) with child `source!=""` escaping cap (B-03). Each of 1000 runs can spawn subtrees; no fan-out/depth budget seen here. Trigger: subagent/tool_smith/remote. Fix: global depth+subtree budget in context. Breaking: behavioral when tripped. Inferred.

## Unbounded resources → current caps

Goroutines: tool workers (per-response ≤64, no global); run goroutines (only `maxRuns` if set; children uncapped); async runs (async MaxHandles/Total refuse); MCP (one/server); idle timers + `AfterFunc(retention)` per run (none). FDs/sockets: default shared pool vs per-run Transport (no pool); MCP one/server; fetch per-call only; bash pipes per-stream bytes only; debug files (`MaxBodyBytes/MaxTotalBytes`, 0=keep-all). Processes: sync exec none; managed only `OwnerLimit`+retention; subagent unknown. Memory: `stagedItems` none; args/outputs per-call only; `eventQueue` per-subscriber bytes (terminal frames always queued); `liveImages`/content inline-part limit only; fetch/read per-call bytes only.

## Not verified

`native/{exec_functions,glob,grep,ripgrep,task,subagent,memory,fetch store quotas}` bodies; `mcp/{content,loadServer}`; `checkpoint` cost; `transcript/{TurnStore,CommitWithObserver,InputHistory}` contention; `imagestore/imageresize`; `responses` notifier wiring; `anthropicadapter` symmetry; `remote/` hop clients; `DefaultTransport` numbers; no load test per task.

## Part C

Source: reviewer3 report, verbatim.

# Part C — HTTP/SSE, sub-agents, asyncmgr, remote/remoteapi, logging, serverconfig/cmd
Concurrency inventory for ~1000 concurrently ACTIVE runs. HEAD 24f005a. Read-only.
Evidence: **verified** = cited lines read via index tools (`get_code_snippet`/`search_code`) + `read.sh` exact line ranges. **inferred** = traced by name/structure.

## Findings

| ID | Location | Shared thing | Why it fails at 1000 / trigger | Sev | Design change (breaking? old data) | Evid |
|----|----------|--------------|-------------------------------|-----|-----------------------------------|------|
| C-01 | `app/resolver.go:71,75-112` | `Resolver.mu` one mutex over ~13 maps (`stores`, `processManagers`, `subagentManagers`, `asyncHosts`, `mcpManagers`, `taskStores`, `memoryStores`, `fetchStores`, `*_sessions`, `*Builds`, `runners`) | Every manager lookup (`sharedSubagentManager`, `sessionAsyncManager`, `asyncManagers`, `subagentManagerList`) and every `Resolve`/`runners` access takes this one lock. 1000 runs doing tool calls+compaction+usage reads+list all serialize. Trigger: concurrent resolve/tool/list. | degrades (blocker if holds lengthen) | Sharded `sync.Map`/per-key RWMutex by storage dir; narrow lock only for build coord. Not breaking; loads unchanged. | V |
| C-02 | `httpapi/registry.go:300-311,361-445` | `runRegistry.mu` global mutex over `runs map[string][]*activeRun` | `SessionRuns` (list) & `runningInSession` (stop) iterate **all** keys under `mu` (O(total runs)); `current`/`stop` hold `mu` while calling `run.state()` (per-run `mu`). 1000 keys ⇒ every `/run/stream`,`/run/stop`,list scans 1000 entries under a global lock. Trigger: list/stop/status at scale. | degrades | Shard run map by session key; cap counter already `atomic.Int64`. Not breaking; in-memory. | V |
| C-03 | `httpapi/registry.go:660-720` (`publish`/`publishMessage`) | `activeRun.mu` held while fanning to `subscribers map[*eventQueue]`, each `queue.push` takes `q.mu` | Per-publish fan-out O(subscribers) with `run.mu`→`eventQueue.mu` nesting; every delta/usage/async/hook event. 1000 runs streaming deltas re-enter `run.mu` constantly. Trigger: streaming at scale. | degrades | Copy subscriber slice under lock, push outside. Not breaking. | V |
| C-04 | `httpapi/run_content_cache.go:237-260` (`SnapshotPrefix`) + `registry.go:560-610` | Per-run content file + `c.mu`; **per-subscriber full read** | `beginSubscribe`/`finish` calls `SnapshotPrefix(length)` which `ReadAt`s the whole streamed text under `c.mu`. Late subscriber to ~1MB output reads 1MB under the cache lock; 1000 reattach re-read full content. Trigger: late subscribe on large output. | degrades | Page the snapshot (index segments stubbed via `MaxIndexSegments`); stream from offset. Not breaking. | V |
| C-05 | `asyncmgr/manager.go:187-253` (`changedLocked`/`notifyLocked`) | `changed chan` closed+recreated on **every** mutation; `watchers map[int]` scanned per change | `notifyLocked` does `close(m.changed); m.changed=make(chan)` on every Register/Complete/track; `changedLocked` iterates all watchers to notify owner. 1000 handles ⇒ every change allocates a channel and scans 1000 watchers under `mu`. Trigger: any async state change. | degrades | Per-owner broadcast (per-owner signal chan / sync.Cond). Not breaking. | V |
| C-06 | `asyncmgr/manager.go:96-105`; `query.go:73-145` (`List`); `manager.go:381-450` (`Complete`) | Single `Manager.mu` over `rows`/`owners` + full-map sort | `List` iterates all rows (or owner live+unread) and `sort.Slice` under `m.mu`; `Complete` takes `mu` for track/untrack/prune/enforceUnread. 1000 handles ⇒ list/status/complete is O(n log n) under one lock. Trigger: async list/poll/completion storm. | degrades | Per-owner buckets / sorted index; cursor-bounded List. Not breaking. | V |
| C-07 | `asyncmgr/store.go:67-185` (`writeRecord`/`writeIndex`) | `ioMu` serializes **all** writes; `writeIndex` rewrites whole `index.json` (O(all rows) marshal+write) per change (timer-coalesced) | Every Register/Complete writes a record under `ioMu`; `flushIndex` marshals every row and rewrites full index. 1000 handles changing ⇒ ioMu is a global write bottleneck; index rewrite O(n) each flush. Trigger: handle churn. | degrades/blocker (throughput) | Append journal + periodic compaction; batch index writes. Not breaking; index v1 loads. | V |
| C-08 | `remote/hub.go:45-73` (`Directory.Client`/`Hub.client`) | `Hub.mu` + `clients map`; **per-call full read** of entry file + `reflect.DeepEqual` | `Directory.Client` calls `Load(d.dir,name)` (read+parse entry file) on **every** call, then `Hub.client` takes `h.mu` and `reflect.DeepEqual(existing.entry,entry)`. 1000 remote calls re-read entry file + DeepEqual under hub lock each. Trigger: remote subagent/profile_message calls. | degrades | Cache entry by mtime/hash; cheap fingerprint; short RLock. Not breaking; entry format unchanged. | V |
| C-09 | `remote/client.go:78-175` (`slots chan`, `Do`) | Per-client `slots chan` = `MaxInFlight`; `control chan` | All calls to one remote serialize through a fixed slot channel; 1000 runs queue on `slots<-{}` (bounded by `MaxInFlight`). Long `Wait` status polls can hold ordinary slots. Trigger: many concurrent remote calls. | degrades (bounded by design) | Ensure long polls use `Control`/dedicated budget. Config-bound, not structural. | V |
| C-10 | `remoteapi/server.go:159-175,429-530` (`begin`, `sweepLocked`, `evictFinishedLocked`) | `Server.mu` over `runs`/`requests`; full-map scans in begin/sweep/evict; **goroutine per run** | `begin` holds `s.mu` across `sweepLocked()` (all runs) + live-count loop (all runs). 1000 remote runs ⇒ every start scans all runs under one lock; +1000 watcher goroutines. Trigger: remote run start. | degrades | Bucket by token; min-heap for eviction; completion callback not goroutine. Not breaking (ids unchanged). | V |
| C-11 | `tools/native/subagent_manager.go:60-71,968-1065` (`ownedRuns`/`allRuns`/`list`/`CancelUnmanagedOwnersWithPrefix`) | `SubagentManager.mu` over `runs`/`writers`/`byRoot`/`bySession`; **O(n) full-map scans** | `ownedRuns`/`allRuns`/`CancelUnmanaged…` iterate every run to filter; `list` sorts. 1000 subagent runs ⇒ every list/scan/cancel O(n) under single lock. Trigger: subagent list/status, session stop, prune. | degrades | Index by session/owner (`map[string]map[string]*subagentRun`); counters already present. Not breaking. | V |
| C-12 | `tools/native/subagent_manager.go:295-340,450-475` (`subagentRun.mu`, `textOnce`, `loadText`/`readText`) | Per-run `mu`; `textOnce` reads **whole** transcript on first read | `readText`/`loadText` reads entire child transcript once per run (resident for lifetime); `view()`/`manifest()` take `run.mu`. 1000 runs ⇒ up to 1000 full transcripts resident + per-view lock. Trigger: subagent transcript read / list. | degrades | Lazy/paged text; cap retained text (a `cutUTF8` cut exists). Not breaking. | V |
| C-13 | `tools/native/subagent_store.go:148-171` + `app/resolver.go:771-800` | `os.ReadDir` + full read/parse of **every** manifest at build; `subagentBuilds` done-chan makes 1000 first-touches of one dir wait on one build | `NewPersistentSubagentManager`→`recover`→`store.load()` reads+parses all manifests (O(manifests)) at build, correctly outside `Resolver.mu`, but 1000 concurrent first-touches block on one `build.done`. Trigger: cold start / first run per storage dir. | minor (amortized) | One-time per dir; acceptable. Optionally pre-warm. Not breaking; manifests load unchanged. | V |
| C-14 | `httpapi/registry.go:712-720` (`time.AfterFunc(retention,retire)`) | **Per-run timer** on every finish | Each finished run schedules a `time.AfterFunc`; 1000 endings ⇒ 1000 timers. Minor timer-wheel/GC load. Trigger: completion storm. | minor | Janitor goroutine + min-heap by retireAt. Not breaking. | V |
| C-15 | `httpapi/start_turn.go:13-70` (`turnQueue.mu` over `map[string]*list.List`) | One mutex over per-key FIFO; map grows with keys | `enter`/`leave` take `q.mu` per run start/compaction. 1000 concurrent starts on distinct keys ⇒ contended but short holds; map size = active transcripts. Trigger: start burst. | minor | Fine at 1000; shard if needed. Not breaking. | V |
| C-16 | `sessiongate/gate.go:91-135` (`keyedLocker.mu` + `keyedLockEntry.mu`) | Map mutex + per-session entry mutex; `lock()` takes `l.mu` then `entry.mu` (map released first) | `lock()` bumps refs under `l.mu`, releases, then `entry.mu.Lock()`. 1000 sessions ⇒ contended `l.mu` (map insert/delete); per-session work on own `entry.mu`. Trigger: concurrent starts, many sessions. | minor | Sharded map; order consistent (no nesting). Not breaking. | V |
| C-17 | `logging/logging.go:48-85` + `httpapi/request_observer.go:78-124` | One log **file** fd; slog+`os.File.Write`(O_APPEND) serialized at kernel; every request + run start/finish logs | 1000 runs ⇒ ~2000 run lines + request lines to one append fd. No app lock (os.File safe) but single fd serializes writes; slow disk stalls logging. Trigger: log volume. | degrades | Buffered/async writer, batched flush. No secrets logged (good). Not breaking. | V |
| C-18 | `remoteapi/tokens.go:243-300` (`refresh`, `Authenticate`) | `TokenTable.mu`(RWMutex) + per-auth **O(tokens) scan** w/ `subtle.ConstantTimeCompare`; periodic full-file re-read | `Authenticate`→`refresh()` (throttled; re-read+re-parse token file) then RLock + linear scan w/ constant-time compare per token. 1000 auths ⇒ O(tokens)/req (small N) + throttled re-read. Trigger: remote API auth. | minor | Hash-prefix bucket; keep constant-time scan over bucket. Not breaking; token format unchanged. | V |
| C-19 | `serverconfig/defaults.go:58-62` (`MaxMaxConcurrentRuns=1024`) | Config ceiling on concurrent runs | `server.max_concurrent_runs` hard ceiling is 1024; 1000 active runs is at the edge of the allowed cap. Registry cap (`running atomic.Int64`) is lock-free. Trigger: config near max. | minor | Raise/unbound ceiling (config value, not structural). | V |
| C-20 | `cmd/jad/main.go:70-105` + `app/storage_recovery.go:55-120` | Startup **background** scan: `os.ReadDir` per root + per-session settings load, builds managers | Recovery walks every root+session (O(sessions)) loading settings and building managers in one background goroutine (`s.recovering`); does not block listen but is serial/slow with many sessions. Trigger: cold start, large root. | minor | Parallelize per-session (bounded workers); already ctx-cancellable. Not breaking. | V |

## Lock-order pairs

- **`runRegistry.mu` → `activeRun.mu`**: `runRegistry.current` (`registry.go:361-374`) holds registry `mu` while `run.state()` takes run `mu`; also `runRegistry.stop` (`:419-426`). Reverse? `activeRun.finish` (`:690-720`) releases `run.mu` **before** `registry.retire()` (takes registry `mu`); `SessionRuns`/`runningInSession` release registry `mu` before run `mu`. **No reverse — consistent.**
- **`activeRun.mu` → `eventQueue.mu`**: `publish`/`publishMessage`/`beginSubscribe` (`registry.go:560-585,660-680`) hold run `mu` while `queue.push` takes `q.mu`; `pendingSubscribe.finish` (`:596-615`) calls `queue.release` after run `mu` released. **No reverse — consistent.**
- **`asyncmgr ioMu` → `mu`**: `writeRecordUnless` (`store.go:73-84`) takes `ioMu` then may take `mu` via `skip`; `flushIndex` (`store.go:175-185`) takes `mu` to snapshot rows, **releases `mu`**, then `ioMu`. **No inversion** (nothing holds `mu` while taking `ioMu`); documented order (`manager.go:74-75`) holds.
- **`SubagentManager.mu` → `subagentRun.mu`**: `RunTranscript`/`byID` (`:946-963`) take `m.mu`, release, then `run.mu`; `ownedRuns`/`allRuns` hold `m.mu` while reading `run.owner` field only. **Consistent.**
- **`Hub.mu`** is leaf in the read path (no nesting with transport locks).
- **No reverse-order/inversion found.** The dominant risk is *contention* on single global locks (C-01,02,05,06,10,11), not deadlock.

## Not verified
- `webui.Increments` internals (from `messageFrames`) — out of Part C.
- `session.Runner` run loop / tool-batch workers / compaction & usage-read paths below `executeRun` — outside Part C; only `executeRun` wiring read.
- `app/resolver.go` `Resolve`/`runners` full body & hold time — C-01 severity (degrades vs blocker) depends on it.
- `process_manager.go` recover/list scan (`os.ReadDir` :992) and `ManagedProcessManager` per-call locking (phase 6b) — recovery read noted, not audited.
- `checkpoint` git-subprocess handling (phase 4) — not read.
- `message_stream_emitter.emitLocked` per-flush full-message copy cost at 1000 runs — copy seen (`:120+`) but not sized.
- Benchmark/measurement numbers — none run (reviewers don't benchmark); severities are structural hypotheses.
- `memorytrace.Trace` sampling loop cost at 1000 runs (`trace.go:210-264`) — lock surface read, not interval cost.

## Method
Index tools for discovery; `read.sh` for exact ranges/non-Go reads. No tests/benchmarks/`-race`; no files modified.
