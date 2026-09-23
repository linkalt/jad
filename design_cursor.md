# Cursor cache — one entry per provider run

## Problem

Every UI read re-reads and re-parses the whole transcript, and the browser pulls the result every
5 seconds:

| Measured (this session) | |
|---|---|
| `messages.jsonl` + `messages.backup.jsonl` | 3.0 MB + 11.7 MB = **14.9 MB**, 841 records |
| `readFile` + `JSON.parse` | **85 ms** |
| normalize + stringify + send | +75 ms → **~160 ms per request** |
| payload | **3.03 MB** |
| poll cadence | **5000 ms** (`session_refresh_interval_ms`), unconditional — no ETag, `no-store`, no gzip |
| cost while idle | **~1.9 s CPU and ~36 MB per minute, per tab** |

Two more facts drive the design:

- The transcript is **append-only between compactions**; a run appends one record per committed
  provider round (measured: 38 records in a single turn) and one per tool output.
- `function_call_output` records carry **no `turn`/`step`**, so a cold reader cannot attribute them
  without walking forward — but the writer knows both at commit time.

## Decision: 1 cursor = 1 provider run

A cursor is the **start offset of a provider run**. Its extent is the next run's start offset, and
for the last run the current transcript size:

```
runs: [
  { "offset": 3820000, "turn": 61, "step": 1 },
  { "offset": 3845620, "turn": 61, "step": 2 },
  ...
]
```

Consequences:

- **No `bytes` field.** Extent = `runs[i+1].offset`, last run = `stat.size`. Appends therefore never
  invalidate an entry.
- **Tool outputs get no entries.** A `function_call_output` is appended after the run that issued the
  call and before the next run, so it already falls inside that run's extent — which is also what it
  means: that run's result.
- **User messages get no entries.** The turn's user message sits immediately before the run with
  `step == 1`; a page includes it by reading backwards from `runs[i].offset` to the previous line
  boundary (the same backwards-read trick `repairJsonlTail` already uses).
- **Turn starts are derived, not stored:** `step == 1` marks a turn boundary.
- The document is rewritten **once per new provider run** (per step, seconds apart) — never per
  appended record.

## Storage: `staging/cursor-cache.json`

Same idiom as `history-cache.json` and `usage-cache.json`: built once, then maintained by reading
only the newly appended bytes, rewritten atomically.

```json
{
  "schema_version": 1,
  "transcript": {
    "device": 2049,
    "inode": 1234567,
    "offset": 3986608,
    "modified_at_ms": 0,
    "changed_at_ms": 0,
    "checkpoint_start": 3976128,
    "checkpoint_sha256": "…"
  },
  "runs": [{ "offset": 3820000, "turn": 61, "step": 1 }]
}
```

- `transcript.offset` — bytes of `messages.jsonl` already covered by `runs`.
- `checkpoint_start` / `checkpoint_sha256` — hash of the last `HISTORY_CHECKPOINT_BYTES` window
  before `offset`, so append-onlyness is verified without re-reading the file.

## Lifecycle

| Event | Action |
|---|---|
| first read, no cache file | full scan → one entry per `response.completed` record (85 ms / 14.9 MB), atomic write |
| new provider run committed | writer pushes one entry (turn/step known there) → atomic rewrite; a few KB |
| tool output / user message appended | **nothing** — falls inside the last run's extent |
| cache valid, file grew | read `[offset, size)`, append entries for new runs, atomic rewrite |
| `size < offset` (repair truncate) | rebuild |
| device/inode changed (compaction replace) | rebuild |
| cache missing or invalid | rebuild (never an error) |
| compaction | cache is removed alongside `history-cache.json`; next read rebuilds |

Validation mirrors `cacheMatches` (`persistence.ts:1566`): device/inode, `size >= offset`, mtime when
`size == offset`, otherwise `hashFileRange(checkpoint_start, offset) === checkpoint_sha256`.

## Readers

**UI view cache (per session, in memory).** `messages = head + tail`, where:
- `head` = the compacted prefix from `compaction-cache.json` `items` / `assistant_final_items`
  (immutable, 60 KB today) — this is what keeps `messages.backup.jsonl` out of the read path;
- `tail` = records from `runs[0].offset`, normalized incrementally.

Incremental normalize rule: reuse everything up to the **start of the last turn** and re-normalize
only that turn, because a tool result attaches backwards to the message that opened the call.

**Paging.** `before=<offset>` — binary search `runs` by offset, slice whole turns. The union of pages
equals a full normalization.

**HTTP.**
- `ETag` fingerprint = transcript (device, inode, size, mtimeNs) + cache offset + run descriptor
  `{turn_id, status, started_at, finished_at}`. **The run's in-flight snapshot is excluded** — it
  changes every few hundred ms and belongs to the SSE stream; the payload only ever contains
  committed content, so the invariant is: *ETag ≡ committed state, SSE ≡ live state*.
- `If-None-Match` ⇒ `304`, no body, no `Content-Length`; explicit handling in `fetchSessions`, plus
  `ETag` + `Cache-Control: no-cache` so any client may revalidate.
- The session **list** stops shipping messages (counts + last message id only); only the viewed
  session's snapshot carries a transcript.
- While attached to a run, the poll backs off (`session_refresh_interval_ms_streaming`), because the
  SSE already delivers committed tool results; it refreshes once on `done`.

## Configuration

| Key | Default | Purpose |
|---|---|---|
| `paths.cursor_cache` | `staging/cursor-cache.json` | cache location, per path profile |
| `server.conditional_requests` | `true` | ETag/304 |
| `session_refresh_interval_ms_streaming` | `30000` | poll cadence while attached to a run |
| `server.view_cache_sessions` | `4` | how many normalized views stay in memory |

## Effects

| Case | Today | After |
|---|---|---|
| idle poll | 160 ms, 3 MB | **304, 0 bytes** |
| one step commits | 160 ms, 3 MB | ~1 ms, delta-sized body |
| load an older turn | impossible | seek + one turn |
| cold start | 160 ms | one 85 ms scan, then incremental |

## Out of scope (follow-ups)

- Strip the provider request echo from stored records (`tools` et al. — 84–91% of bytes).
- Cap parts on the wire (`max_inline_part_bytes`) with lazy full-output fetch.
- Per-message manifest for client-side delta sync (the `turnOffsets` data supports it later).

## Verification

1. **A/B correctness:** normalized output of the cached reader must be identical to today's
   implementation for the same transcript (byte-compare).
2. **Work count:** instrument parsed records/bytes — expect **1 record per step commit**, not 841.
3. **Paging:** union of pages == full normalization.
4. **Invalidation:** simulate an inode change (compaction replace) and a truncation → rebuild, output
   still identical, stale cursors rejected.
5. **HTTP:** idle tab → `304`; a stepping turn on the playground → cheap 200s; attach/stop flows
   unchanged.
