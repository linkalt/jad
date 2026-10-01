# Streaming content cache: audit, fixes, and implementation record

Status: **follow-up fixes implemented in the working tree; backend suite 455/455 and focused suite 125/125 passed; generated `messages.jsonl` corpus is byte-identical to `ea77880`; uncommitted**. A three-run local interleaving benchmark (1.024 MB, two items) measured median append/compaction 28.65 ms, replay 3.52 ms, and staged-range retirement 3.54 ms. `git diff --check` and schema JSON validation pass; backend was not restarted.

Audit baseline: commit `70c5de7` (`Store streamed output in one partial content file`). The earlier first-delta, cache-I/O, and commit-handoff fixes are in `ea77880` (`Fix streaming content cache correctness and buffering`). This document records those audit findings plus the follow-up interleaving and crash-orphan fixes currently in progress.

## Current implementation status

- One content-only UTF-16LE `responses.partial` file can hold multiple active output-item ranges. In-memory ranges and ordered per-part contributions reconstruct interleaved updates; staged item ranges are removed and remaining data compacted.
- The writer maintains a PID/hostname owner sidecar. Opening a response store removes partial/compaction files only when a same-host owner process is confirmed exited; provider staging and live/unknown-owner caches are preserved.
- `server.run_content_max_index_segments` defaults to `1024` and bounds range metadata. At the cache contribution-index cap, the run uses a correctness-first normalized-message fallback.
- The configured content budget is not a total-process-RAM cap; the provider/normalizer and snapshot materialization still retain content. Legacy or unknown-owner files cannot be safely scavenged automatically.
- No canonical `messages.jsonl` serialization change is intended. Full backend tests and final compatibility checks are pending.

## 1. Goal and scope

Implement a correct, efficient live Web UI cache without creating a second transcript format.

Required lifecycle:

```text
reasoning/text/tool deltas -> bounded memory -> shared responses.partial content ranges
output_item.done -> durable staging -> switch that item to its staged reader
                  -> remove that item's ranges and compact remaining ranges
response.completed -> canonical messages.jsonl commit -> safe staging cleanup
```

Small output items may never need a partial file. There is at most one long-lived `responses.partial` per response-turn staging directory, and it may contain ranges for multiple interleaved output items. A staged item's ranges are retired only after in-flight snapshots finish; the shared file is deleted when no active partial-backed ranges remain. Compaction may briefly create one `responses.partial.compact` temporary file.

The `responses.partial` file contains **only raw UTF-16LE content**. It has no JSON object, JSONL record, delimiter, event header, or on-disk index. Small in-memory item ranges and ordered per-part contribution segments identify which content belongs to each item and preserve stream order; there is no per-provider-delta record log. A separate tiny owner sidecar protects live files during startup cleanup; it contains no content.

### Non-goals

- Do not change the observable representation of `messages.jsonl`.
- Do not replace Responses output staging, history caches, compaction backups, or request-draft state.
- Do not persist/recover incomplete UI content across a backend process restart.
- Do not claim to bound total provider, SDK, normalizer, completed-message, or process memory.
- Do not introduce per-item/per-chunk spill files to support arbitrary interleaving.
- Do not restart the backend as part of development or validation against the current conversation.

## 2. Current execution path and ownership

Relevant source paths:

| Responsibility | Current implementation |
| --- | --- |
| Provider event persistence and callbacks | `backend/src/responses.ts`, `persistResponseStream` |
| Provider round and session lifecycle | `backend/src/session.ts`, `AgentSession.runUnlocked` |
| Durable output staging and canonical commit | `backend/src/persistence.ts`, `ResponseTurn` |
| Normalized UI parts and item-to-part correlation | `backend/src/plugins/web-ui/normalize-web-ui.ts`, `WebUiNormalizedRun` |
| SSE coalescing | `backend/src/stream-emitter.ts`, `MessageStreamEmitter` |
| Run state, subscriptions, reconnect, interactions | `backend/src/run-registry.ts`, `SessionRun` |
| Current partial-content cache | `backend/src/run-content-cache.ts`, `RunContentCache` |
| Integration wiring | `backend/src/server.ts`, `AgentHttpApplication.executeSessionRun` |
| Root settings and defaults | `backend/src/session-roots.ts` |
| Root configuration schema | `sample/schemes/root-setting.schema.json` |

Current order, simplified:

```text
provider event
  -> persistResponseStream.onEvent
  -> WebUiNormalizedRun.updateForStream
  -> MessageStreamEmitter.push
  -> SessionRun.publishMessage
  -> RunContentCache.update
  -> SSE subscribers

output_item.done
  -> onEvent above
  -> ResponseTurn.stageOutputItem
  -> onOutputItemStaged
  -> cache hands off to staged readers and removes partial

response.completed
  -> ResponseTurn.commit appends the canonical response
  -> pre-cleanup observer installs the committed Web UI baseline
  -> staged readers drain and staging is removed
```

The original audit found cache-ingestion and commit-cleanup ordering defects; those are fixed in `ea77880`. This follow-up addresses the two remaining gaps from that implementation: interleaved-item fallback and crash-orphaned partial files.

## 3. Confirmed audit bugs at `70c5de7`

All three findings were confirmed by isolated reproductions and recorded separately as `bug_report` memory records. They are historical failures of the audit baseline; `ea77880` fixes them and adds repository regressions. The reproduction files mentioned below were temporary audit artifacts, not committed tests.

### B1. High: first chunk omitted; initial oversized snapshot bypasses spilling

Locations:

- `backend/src/run-content-cache.ts:191`: the cache resolves the part from an append index, an explicit part index, or an already-active item.
- `backend/src/run-content-cache.ts:218`: initialization from a later append takes only that append's content.
- `backend/src/plugins/web-ui/normalize-web-ui.ts:405-413`: the first text delta for a new part normally has no append hint.

Observed reproduction:

```text
actual provider deltas: FIRST, SECOND, THIRD
expected partial/reconnect: FIRSTSECONDTHIRD
observed partial/reconnect: SECONDTHIRD
```

A separate single initial delta of 1,048,576 bytes did not spill with a four-byte configured threshold. No partial file was created.

Root cause: `outputIndex` identifies an output item, not its UI part. The first normalized update therefore creates no active content state. A subsequent append starts that state with only the new suffix.

Current tests conceal the mismatch by manually supplying append metadata for the first chunk.

Fix requirements:

1. Carry explicit output-item identity and UI part identity for every content mutation, including the first.
2. Distinguish actual content mutations from optimization hints for SSE.
3. Account for and ingest the first delta before testing the memory threshold.
4. Handle oversized initial content through bounded segments, not one unbounded encoded copy.
5. Test through the real normalizer, emitter, registry, and cache pipeline.

### B2. High: optional cache filesystem errors can crash Node

Locations:

- `backend/src/run-content-cache.ts:394-427`: synchronous directory creation, opening, and writes can throw.
- `backend/src/stream-emitter.ts:134-137`: a timer invokes `flush` without containing cache errors.

Observed reproduction: making the cache directory an ordinary file caused `EEXIST` during a coalesced flush. An isolated Node subprocess exited with code 1; the exception escaped through `Timeout._onTimeout`.

Root cause: the predecessor cache wrapped update/spill failures in a fallback boundary. The audited implementation retains a disable/fallback helper, but filesystem operations are no longer protected by that boundary. An immediate callback failure can fail the run; a scheduled callback failure is outside the run's try/catch and can terminate the process.

Fix requirements:

1. Catch cache operation failures at the cache boundary, not only at individual call sites.
2. Observe every asynchronous operation and timer-triggered rejection.
3. Switch to a complete, usable fallback snapshot before discarding the partial source.
4. Do not turn an ephemeral cache failure into a provider-run failure or backend crash.
5. Contain close/delete failures separately so cleanup cannot destroy the fallback path.

### B3. Medium: reconnect races staging cleanup at response commit

Locations:

- `backend/src/persistence.ts:3217-3223`: a staged read rejects after the turn is closed.
- `backend/src/persistence.ts:3336-3339`: commit closes the turn and removes staging.
- `backend/src/session.ts:485-494`: `onResponseCommitted` runs after the persistence function returns.
- `backend/src/server.ts:2038-2042`: the run cache switches sources in that later callback.
- `backend/src/run-registry.ts:335-347`: failed replay falls back to content-free metadata.

Observed reproduction: subscribing at the real commit cleanup boundary replayed an empty string instead of `hello`. After commit, the cache contained `hello`, but that correction was not published to the subscriber.

Root cause: the lifetime of a staged reader extends past the lifetime of the staged turn. Fallback metadata is not a valid content snapshot, and changing the cache baseline alone does not correct an already-published blank snapshot.

Fix requirements:

1. Install the post-commit replay source before the turn is closed or staging is removed.
2. Prevent new staged-reader leases after that source switch.
3. Await existing staged-reader leases before deleting staging.
4. If a snapshot's source becomes unavailable, retry a consistent source; never substitute blank content as a successful snapshot.
5. Test both a reconnect at the cleanup boundary and a staged read already in flight.

### B4. Interleaved items disabled spilling in the correctness fallback

At `ea77880`, starting another unstaged output item while a partial item existed disabled the cache and retained the complete normalized message in memory. That preserved the UI snapshot but bypassed the configured cache-content threshold for interleaved streams.

The current working-tree fix keeps one raw content file and maps active item/part identities to in-memory file ranges. Each normalized part also retains an ordered list of coalesced item-contribution segments, so reconnect replay preserves `A1, B1, A2` order when item A and item B interleave in one merged UI part. Once an item is durably staged, its contributions switch to the staged reader and its ranges are removed/compacted. No provider-event envelope or JSONL record is written.

`server.run_content_max_index_segments` bounds range metadata. If the per-part contribution index reaches that limit, the cache deliberately falls back to the complete normalized snapshot rather than corrupting the response or failing the provider run. This rare fallback is a documented exception to the cache content budget, not a total-process-memory guarantee.

### B5. Crash-leftover partial files had no startup scavenger

At `ea77880`, ordinary staging/handoff/retention cleanup removed `responses.partial`, but an abrupt process exit could leave the file in an incomplete turn directory. Persistence recovery retains incomplete staged turns to preserve provider output, so it could not safely delete the whole directory or infer cache ownership.

The current working-tree fix creates a small `responses.partial.owner` sidecar with the local hostname and PID before opening the content file. `SessionResponseStore.open()` scans immediate response-turn directories and, under a per-turn cleanup lock, removes only `responses.partial`, its temporary compaction file, and owner sidecar when the recorded same-host process is confirmed exited. It leaves manifests, staged items, and other response-recovery data untouched. Live, remote, malformed, and unowned caches are preserved; a PID reused by another process may therefore delay cleanup rather than risk deleting live data.

## 4. Recommended design and invariants

### 4.1 Share normalized coalescing and cache admission

The current implementation uses `MessageStreamEmitter` as the admission boundary so the cache and SSE observe the same normalized message revision:

```text
provider event
  -> normalized mutation with explicit partIndex
  -> MessageStreamEmitter coalesces append-only growth
  -> SessionRun updates the cache and publishes that emitted state
  -> wait for cache capacity only when the bounded writer queue is full
```

A new part is emitted and cached immediately. Append-only growth is coalesced up to `server.tool_stream_interval_ms` (default `120` ms); `0` disables that coalescing. Consequently, a reconnect snapshot can trail a provider delta that is still pending in the coalescer by at most this configured interval. Once a coalesced state is emitted, the cache update and SSE publication share its ordering and snapshot boundary.

The cache publishes the live update before awaiting a capacity promise. Normal disk writes are asynchronous and do not gate each delta. If the bounded writer queue reaches its configured content budget, the event callback waits for capacity before accepting more provider events; this is intentional backpressure, not waiting for a completed response.

There are no synchronous file operations on the live cache/read path. Removing the emitter's newest-message snapshot retention or eliminating the bounded coalescing window is a separate optimization; neither is required for correctness of the content file.

### 4.2 Identity is explicit and immutable

Every content operation carries:

```ts
interface OutputContentIdentity {
  responseKey: string;
  outputIndex: number;
  itemId?: string;
  partIndex: number;
  field: "text" | "arguments" | "output";
}

type ContentMutation =
  | {kind: "append"; identity: OutputContentIdentity; delta: string}
  | {kind: "replace"; identity: OutputContentIdentity; content: string};
```

These are proposed interfaces, not existing exports.

Identity must be established from the provider event and normalizer mapping. Do not infer a new part from an already-active part. If a provider omits an index, assign the same stable normalized index used by persistence, or use the correctness fallback; never guess independently in two layers.

An append hint used to avoid SSE diffing is not the cache's source of truth. An empty append does not create or switch a file owner. Metadata-only updates do not erase content.

### 4.3 State machine

One cache coordinator per `(session, profile, run)` and one shared partial-file generation per Responses turn. The file can index multiple active item/part identities:

```text
per output item:
  IDLE -> BUFFERED                 first content mutation
        -> SPILLED                 combined cache threshold crossed
        -> STAGING                 output_item.done received
        -> STAGED                  durable reader installed; its file ranges retired

shared response file:
  ABSENT -> responses.partial      first active item spills
          -> compacted partial     staged ranges removed; other active ranges remain
          -> ABSENT                last partial-backed item is staged or run is disposed

STAGED sources for a response
  -> COMMITTED_BASELINE            canonical append succeeded, before staging cleanup
  -> staging cleanup              no reader can still depend on that staging

any active state
  -> FALLBACK_MEMORY               cache I/O failure or contribution-index cap
  -> DISPOSED                      run retired; asynchronous cleanup drained
```

When the combined active content stays below threshold and no other item has spilled, staging can complete without creating a partial file. Once spilled, new item deltas share the writer and its bounded admission queue.

Invariants:

1. At most one long-lived `responses.partial` filename/file handle belongs to a response coordinator; compaction may use one transient `.compact` file.
2. The content file is raw UTF-16LE. Item ranges and normalized-part contribution ordering exist only in memory.
3. Every accepted content delta is represented exactly once in the active range or staged source used by replay.
4. Every read uses metadata and content from the same logical snapshot boundary.
5. A staged item switches to its authoritative reader before its ranges are removed; existing snapshot readers drain before compaction.
6. No old file operation can write into a reused filename or delete a newer generation.
7. Staging and canonical transcript durability remain owned by `ResponseTurn`.
8. Snapshot retries and cache failures never trigger a duplicate provider request, tool execution, or canonical append.

### 4.4 Small metadata, not growing duplicate snapshots

Keep these in memory:

- Content-free normalized message/part scheme.
- Stable active item and part identity.
- A baseline message for previous completed response rounds.
- Immutable staged-source descriptors keyed by output item and projected part.
- One item generation and content revision.
- Buffer accounting, current writer state, and reader lease counts.
- Pending interaction, run status, and usage metadata.

Do not keep a second cumulative copy of active text just to drive appends. Do not retain a provider-event or per-delta manifest. Each normalized part has an ordered sequence of coalesced contribution spans; a span identifies an output item's content offset and length, and adjacent appends from the same item extend the current span. The writer keeps in-memory byte ranges per item for the raw shared file. These are bounded replay indexes, not a disk index or per-chunk record format.

`server.run_content_max_index_segments` bounds the contribution/range metadata. The writer compacts redundant file ranges at the configured threshold; if the ordered part index itself reaches the limit, the cache switches to the correctness-first normalized-message fallback.

Staged descriptors must capture stable identity/field information. Avoid readers whose behavior depends on subsequently mutated normalizer maps or an unrelated current part.

## 5. Bounded asynchronous writer

Use a small internal module such as `backend/src/response-partial-writer.ts` for file mechanics. Keep source selection and UI composition in `run-content-cache.ts`.

### 5.1 Writer behavior

- Open `responses.partial` lazily using `node:fs/promises`.
- The coordinator owns the directory, generation, file handle, buffers, and cleanup.
- Buffer small deltas and flush on a configured batch threshold or timer.
- Serialize writes; handle short writes by continuing the remaining bytes.
- Do not perform a flush, open, or `fsync` for every provider delta.
- Do not `fsync` the partial cache: it is ephemeral. Durable staged output retains the existing persistence policy.
- Start an actual disk write as soon as spilling is required, even if the batch timer has not fired.
- Once spilling, append subsequent content bytes; do not rewrite the file on ordinary growth. Item identity is associated with ranges in memory, never with bytes written to disk.
- A batch may contain interleaved item deltas. Keep an in-memory range list per item and compact redundant ranges into the same raw file when `run_content_max_index_segments` is reached; use one transient `.compact` file and atomically replace the partial.
- After an output item is durably staged, switch its contributions to the staged reader, drain snapshot readers, then remove that item's ranges and compact remaining items. Delete the partial when no active ranges remain.
- On an unchanged final arguments snapshot, avoid redundant replacement work. When `output_item.done` is immediately followed by durable staging, switch to that authoritative source instead of rewriting the entire partial just to delete it.

A queue of promises can also be unbounded: serialize file operations, but retain content only inside a budgeted buffer manager. Never enqueue one closure containing a delta for every raw provider chunk.

### 5.2 Memory accounting and configuration compatibility

The existing `run_content_memory_bytes` is documented in **UTF-8 content bytes**, not actual JavaScript heap bytes. The implementation uses it as the active-source spill threshold and as the writer's bounded UTF-8 queue capacity (with the configured batch size as the zero-threshold transport capacity). These are separate stages, not a strict combined physical-memory ledger. Initial provider/normalizer strings, one in-flight input item, UTF-16LE encoding buffers, range metadata, and full reconnect snapshots can temporarily add memory beyond the configured content charge. Do not describe it as an exact process-RAM cap.

The writer queue is bounded; it must never become an unlimited promise closure queue. Initial active item strings are admitted sequentially and released from their active-source slots as the writer accepts them. A future exact physical-buffer cap would need a separately specified encoded-buffer budget and a ledger that includes transient slice/encoding allocations; do not silently redefine the existing UTF-8 setting.

If admission cannot reserve capacity, start spilling/draining immediately, even when retained content is exactly equal to the threshold. Never wait for capacity while the only operation that could release it is an unscheduled spill. Test this exact-threshold case to prevent a budget deadlock.

Conservative incremental UTF-8 charging per incoming string is acceptable: separately counting split surrogate halves may overcharge a pair, causing earlier spilling, but must not undercharge or corrupt it. Do not repeatedly compute the byte length of the entire growing string.

UTF-16LE on disk can require twice the UTF-8 content charge for ASCII. Encoding batches, buffer capacity, string representations, descriptors, and snapshot materialization add overhead. Document and measure this explicitly. Bound encoded batch allocations as well; do not encode an entire oversized provider delta in one allocation.

If a future requirement is an exact physical buffer cap, add an explicitly named encoded-buffer budget with its own documented semantics, or approve a configuration-semantics migration. Do not silently redefine the existing UTF-8 setting as UTF-16LE storage bytes.

### 5.3 Backpressure and oversized deltas

An incoming provider delta can itself exceed the budget. Its original provider/normalizer-owned string already exists and is outside the cache's accounting scope.

Process it in bounded segments:

1. Admit a segment that fits the available cache budget.
2. Schedule/continue the writer.
3. Publish the live normalized update without waiting for all disk writes.
4. Await capacity before admitting remaining segments or accepting another provider event.
5. Treat the entire logical mutation as one snapshot boundary: a snapshot must not expose an accidentally half-admitted provider delta as a completed revision.

A snapshot requested during a large mutation waits for that mutation's admission barrier, without occupying the writer queue. This lets the writer continue releasing capacity and avoids deadlock.

Zero-threshold mode means immediate spill, not an infinite busy loop: use the configured positive write-batch size as the effective staging-buffer capacity in that mode. This is a documented transport-buffer exception, not permission for an unlimited queue.

Fallback mode has different guarantees: if disk fails, preserve a usable in-memory snapshot, even if it temporarily exceeds the normal cache budget. Warn once per affected response and report fallback state in diagnostics.

## 6. Consistent reconnect snapshots

### 6.1 Linearization boundary

A snapshot captures one logical message revision, including metadata, content sources, interaction state, and the position in the published run-event sequence.

A monotonic in-memory run-event counter may be used for ordering. It is not a disk offset, chunk index, or persisted event log. Existing queued-subscriber behavior can be retained if it correctly implements the same boundary.

For a partial-backed item:

```text
prior admitted writes -> snapshot operation -> later writes
```

Before the operation is enqueued, ensure its mutation-admission gate is complete. Flush the pending pre-boundary content, then read the entire partial file asynchronously while later writes are blocked by the writer. Do not parse per-chunk records. Capture the matching metadata before allowing later mutations to alter that snapshot's shape.

An alternative snapshot representation can combine a stable file read with a frozen tail, but the first implementation should use the simpler flush/read barrier; it avoids file-length bookkeeping and offset indexes.

### 6.2 Subscriber event ordering

1. Register the subscriber as initializing at the snapshot boundary.
2. Capture interaction and usage at that same boundary.
3. Queue only live events after the boundary for that subscriber.
4. Deliver the hydrated snapshot.
5. Drain the queued events in order, then switch to live delivery.

Never replay a snapshot that already includes a delta and then apply that delta again. In particular, a cache revision admitted before its coalesced SSE event is published needs explicit treatment: flush the coalescer at the subscription boundary or filter queued events by their covered revision.

Bound initializing subscriber queues using the existing SSE buffering policy or equivalent configurable limits. If a client exceeds the bound, disconnect/resynchronize that client; do not stall the entire provider because one client cannot receive its replay.

Coalesce concurrent snapshot requests for the same revision where practical. Count and bound distinct snapshot jobs so repeated reconnects cannot create an unbounded operation queue. At capacity, share an equivalent existing job if possible; otherwise reject/resynchronize the excess subscriber with the established reconnect/backoff behavior instead of queuing another job indefinitely. Full snapshot allocation is proportional to visible content and is outside the active-content buffering budget; this limitation must remain explicit.

### 6.3 Snapshot failure

Retry against a newer stable source when the old generation is retired. A failed read is not an empty content value. If retry cannot succeed, use a complete fallback snapshot or report replay unavailability and force resynchronization. Never publish stripped metadata as a successful full `ui.message`.

Cancellation detaches the subscriber, but must not delete a file still needed by another subscriber.

## 7. Output-item staging handoff

`response.output_item.done` carries authoritative output. It is not itself proof that the backend's staging write has succeeded.

Recommended sequence:

```text
receive output_item.done
  -> normalizer records authoritative item and final metadata
  -> cache enters STAGING; new snapshots wait outside writer queue
  -> existing partial readers may finish
  -> ResponseTurn.stageOutputItem completes under existing durability policy
  -> install immutable staged replay descriptor
  -> open new snapshot gate against STAGED source
  -> cancel unnecessary pending partial appends
  -> await in-flight partial writer and partial reader leases
  -> close and delete responses.partial
  -> retire generation; admit next item
```

Do not force all pending partial deltas to disk after the authoritative item is durable; those bytes would be written only to be deleted. An in-flight write still must finish before closing/deleting or reusing the filename.

Snapshot gates must not sit in a writer operation awaiting a future handoff that itself needs the same writer. Wait for stage completion before enqueueing the read operation, or use independent source leases.

If staging fails, keep the current partial/fallback source available for live diagnostics until the run's failure policy retires it. A cache warning must not hide a real persistence failure.

If the staged authoritative value differs from streamed content, snapshots after handoff use the staged value. Update the normalized live part to that same authoritative value so cache replay and live SSE converge. Apply this to reasoning and text as well as function arguments.

## 8. Response commit handoff: fix the cleanup race

Add an awaited lifecycle hook inside `ResponseTurn.commit`, **after** the canonical transcript append succeeds, but **before** closing the turn and deleting staging.

Illustrative integration API:

```ts
interface ResponseCommitObservers {
  beforeStagingCleanup?: (committed: CommitResult) => Promise<void>;
}

// Proposed overload; preserve callers that omit observers.
commit(event: ResponseCompletedEvent, observers?: ResponseCommitObservers): Promise<CommitResult>;
```

Forward the observer through `PersistResponseStreamOptions`, session callbacks, and the server. Do not move the existing usage/compaction callback earlier without tracing its dependencies; the new pre-cleanup observer has a narrower role.

Recommended Web UI observer:

1. Freeze a full normalized baseline for this response, using authoritative staged items already reflected by the normalizer.
2. Atomically bind future snapshot requests to that baseline. Reusing immutable strings already retained by the normalizer does not require a new content file or re-reading the transcript.
3. Disallow new staged-reader leases for that response.
4. Await existing staged-reader leases and cache-source transition work.
5. Return permission for persistence cleanup.

Only then may persistence close the turn and remove staging. An in-flight subscriber should either finish its old leased read or replay the new baseline. It should never discover that its only source has vanished.

### Post-commit observer errors

The canonical append is the point of no return. A Web UI observer failure after that append must not make the provider request retry or pretend the response was not committed.

Design an explicit outcome:

- Try to install the complete in-memory fallback baseline and drain readers.
- If cleanup safety still cannot be established, preserve the committed staging directory for deferred cleanup, close resources safely, and return the successful commit with a diagnostic warning.
- Keep cleanup ownership tracked until readers release it; do not delete a leased committed directory during recovery/scavenging.

Document this behavior and test it. Do not rely on an uncaught observer exception propagating through `persistResponseStream`'s failure path.

A no-Web-UI/headless commit must retain the current default behavior. The observer must not change canonical bytes, hashes, append ordering, or idempotence.

## 9. Concurrent items and multiple content fields

A single unframed file does need in-memory identity/range metadata to reconstruct interleaved items. The implemented design keeps **all disk bytes raw content** and puts the smallest practical replay index in memory:

- Key each active source by response, output index, and normalized part index. Use the normalizer's stable identity; do not infer item identity from content text.
- Record byte ranges for contiguous item runs as batches append. Ranges for one item are read/concatenated in order; adjacent writes extend a range when their on-disk byte positions are contiguous.
- For a normalized part, record ordered contribution spans `(item key, offset within that item's content, UTF-16 code units)`. Consecutive deltas from one item extend the same span; interleaved items create another span. This reconstructs a merged part such as `A1, B1, A2` without storing a JSONL event log.
- On snapshot, read active item content from the partial file and staged item content from its authoritative reader, then compose the part in contribution order.
- After `output_item.done` is durably staged, switch snapshots to the staged reader, drain existing reads, remove that item's ranges, and compact remaining content. Delete the shared file if no partial-backed items remain.
- Compact writer ranges when redundant segments reach `server.run_content_max_index_segments`. The per-part contribution index uses the same setting as a hard metadata bound.

If the per-part contribution index reaches that bound, enter the correctness-first normalized-message fallback for the rest of that response rather than corrupting output or failing the provider run. This rare fallback may exceed the cache content threshold; `run_content_memory_bytes` is not a total-process-memory cap. Normal interleaving below the configured metadata bound remains spill-backed and does not trigger the old immediate fallback.

## 10. Configuration proposal

All operational limits and timing choices belong in root server configuration and normalization. Do not embed configurable defaults in writer implementations or timers.

| Setting | Default | Meaning |
| --- | ---: | --- |
| `server.run_content_memory_bytes` | `102400` (existing) | UTF-8 active-source spill threshold; zero requests immediate spill. The writer queue is separately bounded with the same setting. This is not a total RAM cap. |
| `server.run_content_write_batch_bytes` | `32768` (implemented) | Target UTF-8 content bytes per asynchronous append batch; also supplies bounded transport capacity for zero-threshold mode. |
| `server.run_content_flush_interval_ms` | `25` (implemented) | Maximum intentional post-spill batching delay; zero schedules immediate asynchronous flushing. |
| `server.run_content_max_index_segments` | `1024` (implemented) | Maximum in-memory range/contribution segments per response, maximum `65536`; writer compacts redundant ranges, and the cache uses correctness-first fallback if ordered contribution metadata reaches the cap. |
| `server.run_content_max_snapshot_jobs` | `4` (proposal only) | Suggested future ceiling on distinct pending/in-flight snapshot jobs per run; not currently in the root schema or implementation. |
| `server.tool_stream_interval_ms` | `120` (existing) | SSE coalescing; independent of cache flush scheduling. |
| `server.sse_max_buffered_bytes` | `1048576` (existing) | Client buffering bound; snapshots continue to use the existing subscriber buffering policy. |
| `server.run_state_retention_ms` | `30000` (existing) | Finished run retention, not partial-item file retention. |

The implemented writer/index defaults are configurable and should be benchmark-tuned for deployment storage. The snapshot-job ceiling remains an unimplemented proposal.

Implemented schema/normalizer bounds:

- `run_content_memory_bytes`: safe integer from 0 through 16 MiB.
- `run_content_write_batch_bytes`: even safe integer from 2 bytes through 16 MiB; short writes are completed before readers observe the batch.
- `run_content_flush_interval_ms`: safe integer from 0 through 60,000 ms; zero uses asynchronous microtask flushing without a recurring timer.
- `run_content_max_index_segments`: safe integer from 1 through 65,536; default 1,024.
- Older root settings that omit the new writer/index fields continue to receive defaults. `AgentServerOptions`, `RootServerSettings`, defaults, normalization, and the root schema are wired together.

The proposed `run_content_max_snapshot_jobs` ceiling and diagnostic exposure of effective capacities are not implemented in this follow-up.

## 11. Error handling, cancellation, and cleanup

### Cache I/O failures

- The public cache operation resolves with a fallback outcome rather than an unobserved rejection.
- Preserve a complete normalized snapshot supplied by the caller before clearing partial/staged source state.
- Log one primary warning per failed response plus bounded cleanup diagnostics.
- Contains failures from mkdir/open/write/read/close/unlink, including errors after partial writes.
- Cache read failure is distinct from actual canonical staging/commit failure.

### Timer and disposal safety

- Every timer dispatch calls an observed asynchronous operation, with an explicit catch/fallback path.
- Store/cancel the flush timer when staging, cancellation, or retirement supersedes it.
- Dispose asynchronously: stop admissions, close gates, drain writer/reader leases, close the file, remove the owned partial, and settle pending capacity/snapshot waits.
- Adapt registry retention callbacks to observe disposal promises; do not introduce unhandled rejections in `onRetire`, server close, or registry clear.
- A late write/read/delete operation checks its generation and cannot touch a newer item's state.

### Abort policy

Stop accepting provider deltas after abort. Do not force pointless partial flushes. Let an already-started write/read finish safely, then close/remove the file when its replay policy no longer needs it. Preserve the established terminal/error event contract; any change to replaying failed partial messages is a separate product decision.

### Startup/recovery

The cache is ephemeral and is not resumed after process restart. Each writer creates `responses.partial.owner` before opening the content file; it records only hostname/PID, not content. `SessionResponseStore.open()` scans immediate response-turn subdirectories under the active profile's staging directory. For each candidate it takes `responses.partial.cleanup-lock`, confirms the recorded same-host PID is gone, and removes only `responses.partial`, `responses.partial.compact`, and the owner sidecar. Stale cleanup-lock files are reclaimed by PID and atomic rename; a writer checks the lock before and after lease acquisition.

The scavenger leaves `manifest.json`, staged output items, final records, usage/history caches, and all other provider-recovery data untouched. It preserves live, remote, malformed, and unowned cache files because their inactivity cannot be proven safely; old partial files created before owner sidecars may require manual cleanup. Cleanup errors log a warning and do not prevent the response store from opening. Never recursively delete partial names across the whole profile or staging root.

### Privacy

Keep files inside the selected profile's response-turn directory, create them with restrictive permissions, and do not derive filesystem paths directly from provider item IDs. No content in logs by default. Do not leak master-profile baselines, replay readers, or partial files into another profile.

## 12. Server integration and ordering

The following sketch uses the current integration boundaries and omits usage, interaction, and ordinary error handling. `MessageStreamEmitter` carries `partIndex` separately from the optional append hint. Its callback updates `RunContentCache` and publishes the exact same dispatched message; the returned capacity promise is awaited only if the asynchronous writer reaches its configured limit.

```ts
const emitter = new MessageStreamEmitter(intervalMs, async (
  message,
  append,
  outputIndex,
  partIndex,
) => run.publishMessage(message, append, outputIndex, partIndex));

onResponseTurnStarted = async (turn) => {
  await run.beginResponseContentCache(
    turn.turnDir,
    turn.turnDir,
    normalizedRun.streamingMessage(),
  );
};

onEvent = async (event) => {
  const update = normalizedRun.updateForStream(event);
  if (update) {
    emitter.push(update.message, update.append, update.outputIndex, update.partIndex);
  }
  await emitter.waitForEmission();

  if ((event.type === "response.completed" || event.type === "response.incomplete") &&
      activeResponseKey !== undefined && isRecord(event.response)) {
    emitter.flush();
    await emitter.waitForEmission();
    const complete = normalizedRun.complete(event.response);
    await run.commitResponseContentCache(activeResponseKey, {...complete, status: "streaming"});
    activeResponseKey = undefined;
  }
};

onOutputItemStaged = async (turn, outputIndex, readers) => {
  emitter.flush();
  await emitter.waitForEmission();
  await run.handoffStagedOutputItem(turn.turnDir, outputIndex, readers);
};
```

`persistResponseStream` awaits each `onEvent` callback before staging/committing the corresponding event. Thus the first delta is admitted with its explicit normalized part index, provider admission waits when cache capacity is exhausted, and `response.completed` switches reconnect snapshots to a complete in-memory baseline before `ResponseTurn.commit` removes staging. This pre-cleanup source handoff is Web-UI transport state; it does not change the canonical transcript event or its serialized `messages.jsonl` line.

## 13. Step-by-step implementation and status

Steps 1–6 and the commit/staging handoff are implemented in `ea77880`; the current working tree adds spill-backed interleaved item ranges and owner-checked startup cleanup. The explicit `run_content_max_snapshot_jobs` ceiling remains a future proposal. Focused run-registry, persistence, and server tests pass 125/125; the full backend suite and final performance measurements are pending.

### Step 1: write failing regression tests before refactoring

Files: `backend/test/run-registry.test.ts`, `backend/test/stream-emitter.test.ts`, `backend/test/server.test.ts`, and focused new writer tests if needed.

- Reproduce B1 through real normalizer output, without manufacturing first-chunk append hints.
- Reproduce B2 with injectable filesystem failures and one isolated subprocess crash-survival test.
- Reproduce B3 with an explicit commit-cleanup test gate and real staged readers.
- Capture current canonical transcript fixtures for successful, tool, reasoning, and incomplete responses.

Exit criterion: the new cases fail for the audited code for the expected reasons.

### Step 2: introduce explicit content identity and operations

Files: `normalize-web-ui.ts`, `incremental-web-ui.ts` only where types are shared, and corresponding tests.

- Add first-delta and final-item identity for reasoning, text, and tool arguments.
- Generate cache mutations independently of SSE append hints.
- Handle authoritative done events for all supported projected fields.
- Keep existing public SSE event shapes unchanged.

Exit criterion: B1 passes for initial/single/oversized chunks and every new part type.

### Step 3: contain cache failures immediately

Files: `run-content-cache.ts`, `run-registry.ts`, `stream-emitter.ts` only if timer observation still needs changes.

- Reinstate the complete-snapshot fallback boundary.
- Cover write/read/close/delete failures and timed operations.
- Do not swallow real Responses staging errors.

Exit criterion: B2's subprocess remains alive and live/terminal behavior stays correct.

### Step 4: add the pre-cleanup commit handoff

Files: `persistence.ts`, `responses.ts`, `session.ts`, `server.ts`, `run-content-cache.ts`.

- Add the optional awaited observer at the correct post-append/pre-cleanup point.
- Add staged-reader leases and committed-baseline binding.
- Define safe observer-failure behavior after canonical append.
- Keep existing post-response usage/compaction callbacks in their current semantic position.

Exit criterion: B3 passes; canonical bytes and commit idempotence are unchanged.

### Step 5: introduce the bounded asynchronous partial writer

Files: new `response-partial-writer.ts`, `run-content-cache.ts`, configuration files.

- Replace synchronous filesystem calls with serialized asynchronous operations.
- Implement batched appends, short-write handling, capacity accounting, and observed timers.
- Preserve UTF-16LE code units exactly.
- Implement bounded segmentation for an oversized delta and immediate-spill mode.

Exit criterion: no sync filesystem calls remain in cache streaming/replay paths; fake slow storage cannot grow the queue beyond its configured policy.

### Step 6: integrate cache admission with normalized SSE coalescing

Files: `server.ts`, `run-registry.ts`, `stream-emitter.ts`, normalizer integration.

- Carry the explicit normalized `partIndex` through the emitter for first deltas, replacements, and item-done updates.
- Update the cache exactly once when a coalesced normalized state is dispatched, immediately before publishing its corresponding SSE state.
- Return cache-capacity promises from the dispatch; the server awaits them only when the writer reaches its configured content budget.
- Flush and await pending emission before staged-item handoff and before switching to the pre-cleanup completed-response baseline.
- Preserve prompt delivery before the first assistant message and across profile switches/reconnects.

Exit criterion: every dispatched SSE revision has the same cache revision, no coalesced delta is appended twice, the first part chunk is retained, and writer pressure stops further provider-event admission without delaying normal live delivery.

### Step 7: snapshot barriers and queue protection

Files: `run-content-cache.ts`, partial writer, `run-registry.ts`, server SSE integration.

- Capture consistent metadata/content/event boundaries and prevent duplicate application of already-covered coalesced deltas.
- Retry retired sources; never emit blank metadata as a full snapshot.
- Existing SSE subscriber buffering remains the client-queue bound. A separate maximum number of concurrent snapshot jobs is a future proposal, not implemented here.

Status: the cache read barrier and staged-reader lifetime protections are implemented. Consider a distinct snapshot-job ceiling only if reconnect fan-out measurements show it is required.

### Step 8: finish staging retirement and asynchronous cleanup

Files: `run-registry.ts`, `server.ts`, partial writer/cache, staging recovery only if necessary.

- Cancel superseded pending partial writes after durable handoff.
- Drain active writes/readers before close/delete/reuse.
- Observe async disposal at retention, clear, abort, and shutdown boundaries.
- Exercise spill-backed interleaving, staged-range compaction, index-cap fallback, and crash/reopen scavenging with live-owner preservation.

Exit criterion: one partial file at most, no open handles/timers after disposal, no generation contamination.

### Step 9: measure, tune configuration, and document

- Run the test and benchmark matrix below.
- Fix correctness or measured performance failures, then repeat.
- Update `backend/design.md` with the final implemented behavior and limitations, not just the proposal.
- Add a link to this plan if it remains useful after implementation.
- Do not commit unrelated working-tree changes.
- Deploy/restart only during a separately approved operational window.

## 14. Test matrix and compatibility contract

### Content and encoding

- First delta followed by at least two more deltas; exact `FIRSTSECONDTHIRD` replay.
- One oversized initial delta, empty deltas, and configured zero/tiny thresholds.
- New reasoning, tool, and assistant parts in the same response.
- Non-ASCII, quotes, backslashes, newlines, NULs, emoji, and split surrogate pairs.
- Lone surrogates preserved exactly as JavaScript code units; no UTF-8 replacement-character corruption.
- A delta whose segmentation crosses the writer's batch boundary.
- Authoritative final output differs from accumulated deltas.
- Consecutive same-type items that project into one merged UI part.
- Completed items with no prior deltas.

### Ordering and lifecycle

- One thousand or more deltas still use one content-only partial file; there are no per-delta files/records. Staged item ranges are removed/compacted, and the file is deleted when the last partial-backed item is staged.
- Under-threshold items never create a partial.
- Snapshot before/after batch flush, while a write is running, during staging, and during commit cleanup.
- A snapshot covering content still waiting in the SSE coalescer; no duplicate delta afterward.
- A staged read held open while commit attempts cleanup.
- Repeated reasoning -> tool -> assistant reuse with different content lengths.
- Multiple output items interleaved into one merged text part replay in exact emitted order; staging one item retires only its own ranges.
- Range-index compaction at its configured threshold and correctness fallback at the contribution-index cap.
- Store reopen removes a dead-owner partial/compaction file, preserves a live-owner cache, and leaves durable staged items untouched; stale cleanup-lock recovery is also covered.
- Rapid attach/detach, many simultaneous reconnects, and a slow subscriber.
- Separate master/named profiles and multiple sessions running concurrently.
- Request-draft interaction before any message, while spilled, and during reconnect.
- Abort, retention expiry, registry clear, and server close with pending operations.

### Fault injection

- mkdir/open errors; partial/zero/failed writes; read failure; close/unlink errors.
- Timer-origin failures and disposal-origin failures do not create unhandled rejection/exception.
- Slow filesystem proves bounded admissions and effective backpressure.
- Staging failure remains a genuine run failure; cache failure does not become one.
- Post-canonical-append observer failure preserves commit success and safe cleanup ownership.

Use a narrow injectable filesystem/writer adapter for deterministic tests. Test gates or fake clocks are preferable to timing sleeps. Production defaults stay in configuration, not test-only switches in production paths.

### Golden transcript compatibility

`messages.jsonl` is the compatibility contract, and no tracked `messages.jsonl` fixture exists. For this follow-up, generated a temporary deterministic corpus with staged tool-call and assistant-message responses from baseline `ea77880` and the working tree. The two `messages.jsonl` outputs were byte-identical at 614 bytes. No transcript paths are modified in the worktree.

This is a focused compatibility spot check, not a substitute for a broader checked-in golden corpus. Future persistence changes should compare identical synthetic streams containing deterministic timestamps/IDs, staged outputs, tool calls/results, reasoning, multiple rounds, incomplete responses, and already-present commits. Every unrelated byte-level difference is a regression.

A passing UI replay test does not prove transcript compatibility. Verify hashes, append count, ordering, recovery, and compaction/history tests independently.

## 15. Performance measurement plan

### Audit measurements: observed, not targets

The audit's direct-cache synthetic measurements on local storage were:

| Chunks | Content bytes | Append time | Replay time |
| ---: | ---: | ---: | ---: |
| 500 | 64,000 | 3.69 ms | approximately 0 ms, in memory |
| 2,000 | 256,000 | 9.65 ms | 0.45 ms |
| 8,000 | 1,024,000 | 17.80 ms | 1.45 ms |

Temporary harness: `/tmp/audit-70c5de7.mjs`. These are single-run, cache-only numbers with manually explicit append metadata; they do not measure the real provider/SSE path, concurrent runs, memory pressure, or slow disks. They do not invalidate the separately reproduced real-normalizer first-chunk bug.

The audited baseline backend build and 448 tests passed; 73 focused run/server/emitter tests also passed. That demonstrates a coverage gap, not correctness of the three defective paths.

### Working-tree spot checks (observed, not acceptance targets)

The measurements below predate the current interleaved range/compaction and startup-scavenger changes; rerun these workloads before making performance claims for the current implementation.

Ran the normalized content path on Node `v26.10.0`, Linux `x64`, using a local temporary filesystem. These are single-run synthetic measurements, not concurrent or slow-disk tests:

| Path | Chunks | UTF-8 content | Cache admission/write | Replay | SSE events / approximate JSON bytes | Partial file |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| Normalizer + 120 ms emitter coalescing + async cache | 500 × 2,048 | 1,024,000 bytes | 14.67 ms | 2.11 ms | 2 / 1,026,224 bytes | 2,048,000 bytes |
| Normalizer + 120 ms emitter coalescing + async cache | 1,000 × 1,024 | 1,024,000 bytes | 11.80 ms | 0.80 ms | 2 / 1,025,200 bytes | 2,048,000 bytes |
| Normalizer + cache admission per delta (no emitter) | 500 × 2,048 | 1,024,000 bytes | 14.14 ms | 2.16 ms | n/a | 2,048,000 bytes |

The emitter cases intentionally coalesced each synchronous burst into two UI events (initial message and one appended suffix). The numbers include normalizer and admission overhead but exclude HTTP/SSE transport and real provider latency. `responses.partial` is UTF-16LE, hence twice the ASCII UTF-8 content size. Each run verified replay length and that disposal removed the partial file. Do not use these spot checks as universal performance targets.

### Follow-up interleaved writer measurement (observed, not a target)

On Node `v26.10.0`, Linux `x64`, ran three local repetitions with two alternating output items, 500 × 1,024-byte chunks per item (1,024,000 UTF-8 bytes total), 100 KiB writer capacity, 32 KiB batches, 25 ms flush delay, and `run_content_max_index_segments=256` to exercise compaction. Median append/admission plus range-compaction time was **28.65 ms**, replay **3.52 ms**, and staged-range retirement/compaction **3.54 ms**. The raw file was 2,048,000 bytes before retirement and removed after the last item was staged. This measures the writer only on local temporary storage; it is not a full HTTP/provider benchmark, a concurrent-load result, or evidence of slow-disk behavior.

### Benchmark workloads

Use a repository benchmark harness with CLI/configured workload values, not hardcoded production limits:

1. Real normalizer -> admission -> SSE emitter -> registry pipeline, approximately 500 deltas per output item.
2. Small and large reasoning/tool/text items; proposed sweeps from 64 KiB through 10 MiB.
3. Batch and flush interval sweeps, including immediate mode.
4. Concurrent-run sweep, for example 1, 4, and 16 independent profile/session runs.
5. Reconnect during growth and handoff, with one and many subscribers.
6. Artificial write latency, short writes, and throttled storage through the test adapter.
7. Large authoritative done items to detect unnecessary full-file rewrites.
8. Measure spill-backed interleaving, range compaction, and the contribution-index-cap fallback separately; report each mode distinctly.

Record Node/platform/filesystem, warmup, workload seed, repetitions, configuration, and median/p95/p99 rather than one timing. Measure:

- Normalized-delta-to-SSE latency, separating configured coalescing delay from cache delay.
- Event-loop delay, CPU time, throughput, write count, bytes written, and open/close count.
- Snapshot latency and materialization size.
- Maximum charged cache content, encoded batch capacity, and queued job counts.
- RSS/heap separately, attributing provider/normalizer output and completed snapshots where possible.
- Cache-file count and handle/timer leaks after handoff/retention.

### Acceptance criteria

Correctness and boundedness are hard gates:

- All three bug reproductions pass through production integration paths.
- No content loss, duplication, cross-profile leakage, or stale/blank successful snapshot.
- No per-delta file or record parsing; one long-lived content-only partial and at most one transient `.compact` file.
- No synchronous filesystem calls on live cache/replay paths.
- No unbounded delta-write closure queue; per-client SSE buffering uses the existing `sse_max_buffered_bytes` policy. A distinct cap on concurrent cache snapshot jobs remains a future measured improvement, not a completion claim.
- The active-source spill threshold and writer queue stay bounded under slow storage, subject to documented representation overhead and the explicit index-cap fallback.
- Canonical `messages.jsonl` remains byte-identical for the golden corpus.

Performance gates must be set from a repeatable baseline on the deployment environment. Do not assert a universal millisecond target from the local audit numbers. Require normal live SSE not to wait for cache flushes, demonstrate reduced write calls for small deltas after spilling, and show bounded event-loop impact under concurrency. Treat any throughput/latency regression outside run-to-run noise as a reason to investigate and repeat.

## 16. Suggested change grouping and rollout

Prefer separately reviewable commits:

1. Regression tests, explicit first-delta identity, and cache-failure containment.
2. Pre-cleanup commit observer and safe staged-reader handoff.
3. Asynchronous bounded writer plus configuration/schema changes.
4. Decoupled ingestion, consistent reconnect barriers, and async retirement.
5. Performance harness, measured tuning, final documentation.

Each commit should build and pass its relevant tests. Avoid leaving the branch with a writer API change but old synchronous callers. Keep full backend tests, relevant frontend stream/profile tests, and golden transcript comparison as final gates.

Before deployment, review zero-threshold behavior, the contribution-index-cap fallback, remote/unknown-owner preservation, subscriber queue policy, and the configured range threshold. Monitor cache fallback counts, write/backpressure latency, snapshot failures, retained committed staging, and orphan cleanup. Never restart the backend currently serving the conversation merely to validate this plan.

## 17. Implementation completion checklist

- [x] B1: first chunk and oversized initial content are admitted and replayed.
- [x] B2: optional cache I/O failures are contained, including timer-origin failures.
- [x] B3: committed baseline is installed before staged sources are invalidated.
- [x] Identity/content mutations carry the normalized part index independently of SSE append hints.
- [x] Disk data remains raw UTF-16LE; interleaved items use in-memory item ranges and ordered part contributions, not per-delta disk records.
- [x] Batched asynchronous writes, short-write handling, cleanup ownership, and staged-range compaction are implemented.
- [x] The UTF-8 active-source spill threshold, writer queue bound, and their non-total-RAM limitations are documented.
- [x] Snapshot barriers, staged-reader handoff, and commit cleanup ordering are covered by focused tests.
- [x] Startup scavenging removes only partial/compaction files with confirmed dead local owners and preserves provider staging/live owners.
- [x] `run_content_max_index_segments` is wired through root settings, schema, server options, and documentation.
- [x] Re-ran typecheck and focused/full backend tests after cleanup-lock changes; the full backend suite passed 455/455.
- [x] Compared a temporary deterministic corpus from `ea77880` and the working tree; `messages.jsonl` was byte-identical at 614 bytes, with no tracked or changed transcript paths.
- [x] Recorded a three-run local interleaved writer/compaction microbenchmark (median append/compaction 28.65 ms, replay 3.52 ms, staged-range retirement 3.54 ms); concurrent/slow-storage measurements remain future work.
- [ ] Consider a separate configurable maximum snapshot-job count if reconnect fan-out measurements justify it.
- [x] No backend restart or unrelated worktree modification during this work.
