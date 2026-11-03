# backend-go audit: performance, memory, design, bugs, bloat (2026-11-03)

HEAD `acec40f` on branch `claude/amazing-ritchie-sbqle7`. go1.26.0 linux/amd64, running in a cloud container as root, with PID 1 = `process_api`.

## Scope and method

- **Target:** `backend-go/` (85.3k non-test lines, 1,052 Go files). I read the hot paths directly: `session/runner*.go`, `transcript/{jsonl,line_index,input_history}.go`, `httpapi/{registry,handler}.go`, `tools/native/{process_manager,process_group_unix,bash_spill,bash_child,subagent_manager,subagent_call,subagent_external,memory_store,fetch,exec_templates/ruby.rb}`, `tools/catalog.go`, `tools/mcp/manager.go`, `asyncmgr/{store,query}.go`, `checkpoint/{service,git}.go`, `app/{resolver,subagent,remote_api,session_catalog_files}.go`. I did not read every package line by line. I also grepped the whole tree for common Go anti-patterns: regex compiled per call, unbounded `ReadAll`, `time.Sleep`, `context.Background()`, hard-coded timeouts.
- **Prior work.** `audit_findings/` already holds eight audit phases (p1 to p8), the scale passes and the performance passes. Before filing a finding I grepped those files for its symbol. Findings are split below into **New** and **Re-confirmed, still open** (an earlier ID that is still present at this HEAD). Items those audits marked fixed or REJECTED are not repeated.
- **Evidence labels:** `[ran]` = reproduced by a command in this session. `[read]` = traced in source, not executed. `[unmeasured]` = a performance claim with no benchmark run in this session. I ran no benchmarks.

## Checks run in this session

| Check | Result |
|---|---|
| `go vet ./cmd/... ./internal/...` | exit 0 |
| `gofmt -l cmd internal` | no files |
| `go build ./cmd/... ./internal/...` | **failed** at first: `ld: cannot find -lhtml_to_markdown_ffi` (the README's per-host `go generate ./internal/tools/native` had not been run). After running it, the build exits 0. The generated `html-to-markdown-rs_cgo_link.go` is git-ignored. |
| `go test -count=1 ./internal/... ./cmd/...` (before `go generate`) | 31 packages ok. `remote` FAIL (1 test, see B4). `tools/native`, `session`, `app`, `cmd/jad`: build failed (missing FFI library). |
| `go test -count=1 ./internal/tools/native ./internal/session ./internal/app ./cmd/jad` (after `go generate`) | `session`, `app` and `cmd/jad` pass. `tools/native` FAIL with 3 tests. The 3 failures were re-run 5 times on their own and failed every time (B1, B2). |
| `git diff --check` | clean (no tracked file changed apart from this report) |

The 4 failing tests:

| Test | Cause | Finding |
|---|---|---|
| `remote` `TestClientRefusesARealForeignPeerAfterTheDial` | `socat` is not installed, and the test has no skip for it | B4 |
| `native` `TestExecFnIsCallableThroughItsEntryAndKeepsEveryExportAsAProperty/ruby` | the Ruby template prints methods in an unspecified order | B2 |
| `native` `TestProcessAsyncStopIsNonBlockingAndEndsTheGroup` | a process group that holds only zombies is reported as alive | B1 |
| `native` `TestCloseSignalsEveryRunningProcessAtOnce` (1.02-1.04 s against a 1 s bound) | consistent with B1 (see B1), not separately traced | B1 |

## Summary

| ID | Sev | Kind | Title | Status |
|---|---|---|---|---|
| B1 | medium | bug | `processGroupAlive` counts zombies, so in a container without a reaping init every group end waits the full kill grace | new, [ran] |
| B3 | low-med | bug | `Journal.AppendAt` is not atomic: a write error mid-batch leaves part of a batch that is then counted as a turn | new, [read] |
| D1 | medium | design/perf | Process manifests: unconditional fsync, no directory fsync, one global lock, and the fsync runs under `entry.mu`, which stalls the child's output pipe | new, [read] |
| D2 | low-med | design | Sessions in one workspace share a sub-agent manager, and one session's `keep_finished` evicts the other sessions' runs | new, [read] |
| D3 | low | design (rule 3) | Process tool hard-codes drain-after-exit; other runtime values are hard-coded and undocumented | new, [read] |
| B2 | low | bug | Ruby `fn` template error text depends on `singleton_methods` order, so the test fails on Ruby 3.3 | new, [ran] |
| B4 | low | test bug | Foreign-peer test skips for missing sudo but not for missing `socat` | new, [ran] |
| B5 | low | bug | fetch decodes `Content-Encoding: deflate` only as zlib, so raw-DEFLATE servers fail | new, [read] |
| P1 | low-med | perf | SSE loop flushes once per frame and never drains already-queued frames first | new, [read] [unmeasured] |
| P2 | low-med | perf | Every round re-marshals every tool definition, and the request re-validates them | new, [read] [unmeasured] |
| P3 | low | perf | Bash spill compiles 2 regexes per call and prunes the spill folder synchronously on the call path | new, [read] [unmeasured] |
| P4 | low | memory | Sub-agent resume copies the seed transcript through one whole-file `os.ReadFile` | new, [read] |
| P5 | low | perf/bloat | Memory `list` copies each record 4 times and holds the store-wide lock for the whole scan | new, [read] [unmeasured] |
| X1 | low | bloat | Misplaced doc comment on `ReplaceRecords`; `Append` has none | new, [read] |
| R1 | medium | perf | Request build clones and re-marshals the whole history every round | re-confirmed open: p2b2 **PH1** (measured 3.2× one marshal) |
| R2 | low-med | perf | `response.completed` is decoded 4-6 times per round | re-confirmed open: p2b2 **PH2** (measured 2.97 ms per 105 KB event) |
| R3 | low-med | perf | Stream events are validated twice and every value is cloned | re-confirmed open: p2b2 **PH3** |
| R4 | medium | memory | Per-session caches are never evicted for live sessions; `lineIndex` grows with transcript lines | re-confirmed open: p8 **PM-14**, scale **A5**, **A28** |

---

## Bugs

### B1 (medium) `processGroupAlive` treats a group of zombies as alive `[ran]`

- **Where:** `internal/tools/native/process_group_unix.go:54-56` (`syscall.Kill(-pgid, 0) == nil`). It is used by `endProcessGroup` (`process_manager.go:704-721`), `killGroupAfter` and `cancelKillIfGroupGone` (`process_group_unix.go:30-44`), and `child_groups.go:33,59`.
- **Mechanism:** `kill(-pgid, 0)` succeeds while any member of the group exists, zombies included. A background child of the managed shell (`sleep 30 &`) is reparented to PID 1 when the shell exits. If PID 1 does not reap orphans, the child stays a zombie in the group indefinitely. `endProcessGroup` then sees a live group: it sends SIGTERM, polls every 10 ms for the whole `kill_grace_ms` (default 2 s), sends a SIGKILL that does nothing, and only then lets `finish` run.
- **Evidence:** the test failed, and the group it named holds only zombies reparented to PID 1:

  ```
  process_async_test.go:161: process group 14900 is still alive after the stop (P-037 group kill)
    PID  PPID  PGID STAT COMMAND
  14901     1 14900 Z    sleep
  14902     1 14900 Z    sleep
  ```

  This container's PID 1 (`process_api`) also kept an unreaped `sh` zombie for more than 8 minutes. `TestCloseSignalsEveryRunningProcessAtOnce` took 1.02-1.04 s against a 1 s bound. That matches a 500 ms stop grace plus a second 500 ms `endProcessGroup` poll on a zombie-only group; I did not trace it separately.
- **Impact:** `CLAUDE.md` rule 0 says JAD should run inside a container. Many container inits do not reap (no `tini`, no `--init`). There, every managed process or bash call that leaves a killed or orphaned descendant pays an extra full kill grace before its result or async signal arrives, and stop and close paths take longer than their bounds.
- **Fix:** make the backend a child subreaper (`prctl(PR_SET_CHILD_SUBREAPER)`) and reap reparented descendants, so the zombies never reach PID 1. Alternatively, have `processGroupAlive` ignore members in state `Z` (read `/proc/<pid>/stat`). Add a regression test that runs with a non-reaping parent.

### B2 (low) Ruby `fn` template prints singleton methods in an unspecified order `[ran]`

- **Where:** `internal/tools/native/exec_templates/ruby.rb:25` (`constant.singleton_methods.join(', ')`).
- **Evidence:** `ruby -e 'module Many; def self.a = 1; def self.b = 2; end; p Many.singleton_methods'` prints `[:b, :a]` on Ruby 3.3.6. The test expects the error text to contain `a, b`, so `TestExecFnIsCallableThroughItsEntryAndKeepsEveryExportAsAProperty/ruby` fails deterministically (5 of 5 runs).
- **Impact:** the model-visible error text depends on the Ruby version, and the suite is red on a current Ruby.
- **Fix:** `constant.singleton_methods.sort.join(', ')`.

### B3 (low-medium) `Journal.AppendAt` writes a batch record by record with no rollback `[read]`

- **Where:** `internal/transcript/jsonl.go:158-217`; the multi-record caller is `input_history.go:80` (`AppendInputItemsAt`, also reached from `session/runner_tools.go:149`).
- **Mechanism:** each record gets its own `writeAll(file, append(line, '\n'))` (`:200`). If record k+1 fails (ENOSPC, EIO), records 1..k stay complete in `messages.jsonl`. The caller gets an error and nil offsets. `repairTrailingPartialLine` only removes an incomplete last line, so a complete prefix of a batch is never removed. Readers count it as a batch: `session_turn.go:36-42` uses `seenBatches`. A client retry then duplicates the first k input items and bumps the turn count. The `input_history.go:19` doc comment calls the append "atomic-in-process", which does not hold on that error path. `TestJournalAppendAtRecoversFromEveryTornPrefix` only covers a torn single record after a crash.
- **Fix:** build all lines into one buffer and write it once. On a write error, `Truncate` the file back to the starting offset (already known as `offset` at `:195`). This also replaces N write syscalls with one and removes the per-record `append` copy. Add a test with a writer that fails on the second record.

### B4 (low) Foreign-peer test has no skip for a missing `socat` `[ran]`

- **Where:** `internal/remote/client_unix_test.go:101-133`.
- **Evidence:** the test skips when `sudo` or the `daemon` user is missing, then runs `sudo -n -u daemon socat …`. `socat` is not installed here, so the listener never binds and the test fails with `the foreign listener never bound its socket` after 5 s, instead of skipping.
- **Fix:** add `exec.LookPath("socat")` next to the `sudo` check, and `t.Skip` when it is missing.

### B5 (low) fetch accepts `deflate` but decodes only the zlib-wrapped form `[read]`

- **Where:** `internal/tools/native/fetch.go:651-656` (`zlib.NewReader`). The request advertises `Accept-Encoding: gzip, deflate, br` (`:592`).
- **Impact:** a server that sends raw DEFLATE (a long-standing interoperability quirk) makes the fetch fail with a zlib header error. I did not test this against a live server.
- **Fix:** peek the first two bytes and fall back to `flate.NewReader` when they are not a zlib header, or stop advertising `deflate`.

## Design flaws

### D1 (medium) Process manifest writes: fsync semantics and lock scope `[read]`

- **Where:** `process_manager.go:1058-1089` (`persistManifest`). It is called under `entry.mu` at `:411-414` (Start, via `recordLiveManifest`) and `:808-813` (`setLeftRunning`). `processStreamWriter.Write` (`:225-239`) takes the same `entry.mu`, and also writes the spill file while holding it (`:237`).
- **Problems:**
  1. `temporary.Sync()` runs on every manifest write regardless of `persistence.durability`, but the parent directory is never fsynced after the `os.Rename`. So the fsync costs a full sync and still does not make the rename durable.
  2. One manager-wide `persistMu` serializes all manifest writes, including their fsyncs, across every process of the manager.
  3. Because the fsync runs under `entry.mu`, the child's stdout/stderr copier blocks in `processStreamWriter.Write` until the disk sync finishes. A chatty child then blocks on its pipe. On a slow or btrfs disk, a stop (`setLeftRunning`) or start stalls the child's output.
- `asyncmgr/store.go:46-65` (`writeFileAtomic`) has the same "fsync file, never the directory" pattern.
- **Fix:** take the snapshot under `entry.mu` and write outside it. The terminal-state ordering that P6BM-08 needs can use a per-entry generation counter or a dedicated per-entry write lock that the stream writer never takes. Follow the durability setting, and fsync the directory when durability is on.

### D2 (low-medium) A shared sub-agent manager applies one session's `keep_finished` to every session `[read]`

- **Where:** `subagent_manager.go:902-937` (`finish`): `for len(m.finished) > spec.keep { dropOldestLocked() }`. `dropOldestLocked` (`:185-202`) drops the globally oldest run whatever its owner. The manager is keyed by storage folder (`app/resolver.go:737-758`). The default `storage_dir` is `.subagents` under the cwd or tool storage folder (`sessionfs/native_subagent_settings.go:18,412-424`), so sessions in one workspace share it.
- **Impact:** a session with `keep_finished: 1` evicts another session's finished runs on every finish. With `delete_transcripts` on, `removeDropped` also deletes those runs' transcript folders, which is data loss for the other session. Retention is documented per session (`DECISIONS.md` "Persisted runs"), not per storage folder.
- **Fix:** keep the finished list per owner and trim by that owner's `keep`, or refuse two sessions that share a storage folder with different `keep_finished` values (the way `app/resolver.go:976-981` refuses incompatible staging settings).

### D3 (low) Configurable values that are hard-coded (`CLAUDE.md` rule 3) `[read]`

- `process_manager.go:375`: `command.WaitDelay = bashDrainAfterExit` (500 ms). `tools.native.bash`, glob and hooks expose `drain_after_exit_ms` (`bash.go:268`, `glob.go:241`, `hooks.go:143`), but `tools.native.process` has no such key (`sessionfs/native_process_settings.go` has only `kill_grace_ms`). A long-lived process whose descendant holds the pipe loses output after a fixed 500 ms.
- Not in config and not in `DECISIONS.md`:
  - `shell_async.go:23` `shellSweepEvery = 10m`
  - `mcp/manager.go:160` `maxConcurrentServerLoads = 8` (also see scale gap 480: N concurrent `LoadTools` = 8N workers)
  - `anthropicadapter/stream_handler.go:91` `drainTimeout = 25ms`
- `closeGrace`, `stopCallTimeout`, `killWait`, `acquirePollInterval` and `ripgrepKillGrace` are recorded in `DECISIONS.md` and are not counted here.
- **Fix:** add `tools.native.process.drain_after_exit_ms` with the bash default, and update `sample/schemes/` and the config tests. Either move the other three into config or record them as decisions.

## Performance and memory

### P1 (low-medium) SSE: one Flush per frame `[read]` `[unmeasured]`

- **Where:** `httpapi/handler.go:781-799`.
- Every event is written and then `flusher.Flush()`ed on its own, even when `eventQueue` already holds more frames. During token streaming that is one write+flush (one TCP push) per delta frame per subscriber.
- **Fix:** after `nextOrIdle` returns a frame, drain whatever is already queued with a non-blocking pop (a `tryNext`), write all of it, then flush once. Keep the write deadline per chunk as today.
- **Measure** with the existing `BenchmarkRunRouteStreamingDeltas` and `BenchmarkSSEDeltaEventEncoding`. I found no prior finding about per-frame flush (grepped `audit_findings/` for "flush").

### P2 (low-medium) Tool definitions re-encoded every round `[read]` `[unmeasured]`

- **Where:** `session/runner.go:463` calls `Catalog.ResponseTools()` (`tools/catalog.go:246-274`) every round. That `json.Marshal`s every listed tool, which re-validates and compacts each `InputSchema`. Then `BuildRequestWithStateContext` marshals the `[]json.RawMessage` again (`runner.go:381-382`), which validates and compacts every tool a second time, and the whole request is marshalled once more at `:389`.
- A definition only changes when lazy loading flips `Listed()`, so the encoded form can be cached per tool when the catalog is built. Only the listed/unlisted filter needs to run per round. The cost grows with tool count × schema size × rounds.

### P3 (low) Bash spill: regex compiles and a synchronous prune on every call `[read]` `[unmeasured]`

- **Where:** `bash_spill.go:108` calls `bashSpillPruneRegexp` (`:249-265`) for each of 2 streams per bash call. That is 2 `regexp.Compile` calls per call, even when `retention_days` is 0 or the prune is throttled away. When due, `pruneBashSpillFiles` does a `ReadDir` plus a per-entry `Info` on the tool call path.
- **Fix:** compile once per (template, cwd, session) and cache it. Skip the compile when retention is off. Run the due prune in a goroutine.
- `tool_smith_list.go:332` also compiles a regex per listed function (minor).

### P4 (low) Sub-agent resume copies the seed transcript through memory `[read]`

- **Where:** `app/subagent.go:235-250` (`copyFileExclusive`): `os.ReadFile(src)` and then one `Write`. Peak memory equals the transcript size (tens of MB for a long run), and the copy is never fsynced.
- **Fix:** `io.Copy` from the opened source into the `O_EXCL` destination, and `Sync` when durability is on.

### P5 (low) Memory `list`: four copies per record, one store-wide lock `[read]` `[unmeasured]`

- **Where:** `memory_store.go:330-520`.
- `readRecordText` (`:748-759`) returns `string(os.ReadFile(...))`. The loop then converts it back with `[]byte(text)` three times (`parseStoredMemoryJSON`, `memoryETag`, `rawText`; `:476,489`), builds a Go map, and re-encodes the candidate only to size it.
- The whole scan, up to `MaxScanRecords` file reads, runs inside `withLock` (`:523-531`), which also serializes `Create`/`Update` on that root.
- The cursor position search is a linear scan over a sorted slice (`:433-441`); `sort.SearchStrings` would do.
- **Fix:** keep the `[]byte` from `ReadFile`. Read the directory listing under the lock, then read the files outside it (records are written atomically).

## Bloat

### X1 (low) Misplaced doc comment `[read]`

`transcript/jsonl.go:80-83`: the comment for `Append` ("Append validates and compacts…") sits above `ReplaceRecords`, so `Append` (`:150`) has no doc comment and `ReplaceRecords` has two.

## Re-confirmed, still open (present at `acec40f`)

- **R1 = p2b2 PH1.** `runner.go:314-391`: per round, `isJSONObject` + `cloneRaw` per history item, `json.Marshal(input)` (`:360`), then `json.Marshal(requestFields)` (`:389`) re-validates and compacts `input`. Measured in p2b2 §12-bis at 3.2× one marshal and 5.6× the transcript bytes in allocation. Fix: write the request with a `bytes.Buffer` (the items are already valid compact JSON from the journal) and skip the per-item clone.
- **R2 = p2b2 PH2.** `makeCompletedEvent` (`:899`), `responseTraceDetails`, `responseTokens` (`run_token_limit.go:28`), `responseFunctionCalls` (`runner_tools.go:72`) and `automaticCompactionTrigger` (`:719`) each decode the same completed event. Measured at 2.97 ms and 3.49 MB per 105 KB event. Fix: decode the response once into one struct (output, usage, status, id) and pass it on.
- **R3 = p2b2 PH3.** `decodeJSONObject` (`runner.go:818-866`) runs `json.Valid` and then a token decode, and clones every value. Measured at 5.2 µs and 51 allocs per delta event.
- **R4 = p8 PM-14, scale A5/A28.** `Resolver.stores` is only dropped on session delete (`session_catalog_files.go:160-170`) or a failed build. Each cached journal keeps `lineIndex.hashes` with one SHA-256 per transcript line (`line_index.go:24-30`). `asyncHosts` and `subagentManagers` are never dropped. Memory therefore grows with the number of distinct sessions and transcript lines touched since start.

## Not verified / limits

- No benchmarks were run. Every `[unmeasured]` row needs a relative measurement before a fix is prioritized (`audit_plan.md` §6.3 method).
- B5 was not exercised against a real raw-DEFLATE server. D1's pipe stall was not reproduced. It follows from the lock scope, and its size depends on disk fsync latency.
- I did not read these packages in full: `sessionfs` (except settings constants), `checkpoint` (only prune/reclaim/Finish), `webui`, `contentlibrary`, `imagestore`, `imageresize`, `remoteapi`, `openaiapi`, `anthropicadapter` (only the client and stream constants).
- The `race` detector was not run.
