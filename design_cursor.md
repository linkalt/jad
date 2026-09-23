# Cursor cache — one entry per provider run

## Built

Implemented as described below, with these decisions settled during the build:

- **The reader pages by offset instead of caching a prefix.** A snapshot reads the
  turns it needs from the transcript, starting at a turn boundary, so the bytes
  before the window are never read at all. History a compaction removed is folded
  from the backup's tail, cached under the backup's identity plus the transcript's
  generation.
- **The table of turns is never materialized.** `turnStarts()` derives it per
  request from the cursor entries: a step-1 run marks a turn, and the user message
  in front of it (one backwards read) is where its records begin.
- **Revalidation happens before the payload is built.** A `304` costs five `stat`
  calls plus the run descriptor — no transcript read, no usage refresh.
- **`turns` counts user submissions** (from the usage summary), not provider
  rounds: it is the one number the list can produce without touching a transcript,
  and it matches the transcript's own `session_turn` numbering.
- **The scan tolerates JSON whitespace.** The cheap prefilter looks for the
  `"response.completed"` literal rather than this store's compact serialization,
  so a transcript written by something else is still indexed.

Measured on a copy of the session in this repository (transcript 5.4 MB, 124 runs,
`snapshot_turns` 3 / `max_page_turns` 4):

| | before | after |
|---|---|---|
| `GET /api/sessions` | 2,554,719 B / 137 ms | **1,237 B / 3 ms** |
| the same request, unchanged | 2,550,455 B / 124 ms | **304, 0 B / 2 ms** |
| `GET /api/sessions/<ref>` | 2,550,455 B / 132 ms | 1,218,938 B / 63 ms |
| the same request, unchanged | 2,550,455 B / 136 ms | **304, 0 B / 0.8 ms** |
| cursor index, first build | — | 33 ms (one scan) |
| cursor index, nothing changed | — | 0.08 ms |
| cursor index, one round appended | — | skips unchanged bytes, rewrites 5 KB |

The remaining snapshot cost is the newest turns' own bytes (reasoning, tool
arguments, tool results); trimming those is the follow-up listed at the end.

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

**UI view cache (per session, in memory).** `messages = history + tail`, where:
- `history` = the backup's last `compaction.keepbackup.turns` turns (`0` hides them; `20` by
  default). The backup holds raw records, so reasoning and tool calls survive; the compacted request
  items keep only what the model was given, which is why the view no longer reads
  `compaction-cache.json` at all. The block is cached in memory under the backup's identity plus the
  transcript's generation — the window in which a seam can still move without the backup changing.
  It is **served as its own resource** (`GET /api/sessions/:ref/history`) whose validator is that
  identity: a snapshot names it as `history: {etag, count}` instead of carrying it, so a committed
  step ships the live tail only and the static block is fetched once per identity;
- `tail` = records in `[start, end)` of the transcript, where `start` is the oldest turn in the
  window. Nothing before it is read, and a page (`before=`) does not restate the block.

The backup's end is the **compaction seam**: compaction rewrites the transcript with the response it
kept in flight, and the backup still holds that record at its end. The transcript's leading records
are compared with the backup's trailing ones byte for byte, and the matching suffix is left out — the
transcript's copy is the live one. The turn window before the seam is found by walking backwards in
growing chunks, so the cost is proportional to the range that is about to be read, and the backup
needs no index.

A tool result attaches backwards to the message that opened the call, which is why a window always
begins at a turn boundary: the reducer's state at that point is exactly "no calls pending", so a
window normalizes to the same messages as a full read.

**Paging.** `before=<offset>&turns=<n>` — the cursor is a byte offset, and the window is the last
`turns` turns that end before it. Pages tile the transcript exactly (union of pages == full read),
and message ids are derived from byte offsets, so a message keeps its id between a page and a full
read.

**HTTP.**
- `ETag` fingerprint = transcript (device, inode, size, mtime) + backup + compaction cache + usage
  cache + setting + model + run descriptor `{turn_id, status, started_at}`. **The run's in-flight
  snapshot is excluded** — it changes every few hundred ms and belongs to the SSE stream; the payload
  only ever contains committed content, so the invariant is: *ETag ≡ committed state, SSE ≡ live
  state*.
- `If-None-Match` ⇒ `304` before anything is read; 200 responses carry `ETag` +
  `Cache-Control: no-cache`, so a client that stores the body revalidates instead of trusting it.
- The **history block** has its own validator (`GET /api/sessions/:ref/history`): the response ETag
  covers the whole payload and therefore moves on every committed step, while the block only moves
  when history is compacted. Keeping them apart is what stops the static block from being re-sent
  with every step — the client holds it, revalidates it separately, and a `304` there costs a stat
  set.
- The session **list** ships metadata only (name, model, turns, usage, run); the transcript of the
  viewed session is fetched from the snapshot endpoint.
- While attached to a run, the poll backs off (`session_refresh_interval_ms_streaming`), because the
  SSE already delivers committed tool results; it refreshes once on `done`.

## Wire cap and re-fetch

A snapshot carries every part of every message it covers, and one tool output can be hundreds of
kilobytes. `server.max_inline_part_bytes` (default 16 KiB) keeps a bounded prefix of each part inline
and reports the rest:

```json
{"type": "tool_result", "output": "…first 16 KiB…", "status": "success",
 "truncated": {"bytes": 4194304}}
```

The prefix is cut on a code point boundary, and a capped message carries the cursor of its turn
(`cursor: <offset>`), plus `text`/`reasoningContent` recomputed from the kept text — shipping the
full value beside a cut part would defeat the cap.

`GET /api/sessions/:ref/messages/:id` returns the message whole:

- **with `?cursor=<offset>`** — the turn is re-read from the transcript through the cursor index, so
  the result is the same projection the snapshot would have produced, uncut;
- **without it** — the message is served from the history block the view already holds (a backup
  message has no turn, which is why it publishes no cursor).

Nothing is capped on the way out: returning another cut copy would leave the client where it
started. The client keeps what it fetched in a per-session overlay keyed by message id, because the
next snapshot still sends the capped copy and ids are stable (they come from the transcript offset).

Measured on the live session (880 KB snapshot, cap 2 KiB): 22 of 46 messages capped, 81 parts,
424 KB of part text — and cap + re-fetch reproduced the uncapped snapshot byte for byte. With the
backup-tail history the same session's snapshot is 2.46 MB (cap 16 KiB, 20 capped parts), and one
message's capped reasoning (16,276 of 38,500 B) came back whole — 38,254 chars — from
`/messages/<id>`.

## Configuration

| Key | Default | Purpose |
|---|---|---|
| `paths.cursor_cache` | `staging/cursor-cache.json` | cache location, per path profile |
| `server.conditional_requests` | `true` | ETag/304 |
| `server.snapshot_turns` | `24` | newest turns in a snapshot; `0` returns everything |
| `server.max_page_turns` | `50` | largest page one cursor request may return |
| `server.max_inline_part_bytes` | `16384` | inline text per part; `0` sends parts uncut |
| `compaction.keepbackup.turns` | `20` | backup turns rendered as history; `0` hides them |
| `session_refresh_interval_ms_streaming` | `30000` | frontend poll cadence while attached to a run |
| `transcript_page_turns` | `5` | turns fetched per "load earlier messages" click |

## Effects

| Case | Before | After |
|---|---|---|
| idle poll | 160 ms, 3 MB | **304, 0 bytes, ~1 ms** |
| one step commits | 160 ms, 3 MB | the window's own bytes, index update is sub-ms |
| load an older turn | impossible | seek + one turn |
| cold start | 160 ms | one scan of the transcript, then incremental |

## Out of scope (follow-ups)

- Strip the provider request echo from stored records (`tools` et al. — 84–91% of bytes).
- Per-message manifest for client-side delta sync (the `turnOffsets` data supports it later).
- The history block is fetched whole once per identity, so a session whose backup is large pays that
  once per compaction instead of once per step; `compaction.keepbackup.turns` and
  `server.max_inline_part_bytes` bound it until it gets a cursor of its own.

## Verification

1. **A/B correctness:** the projection matches `normalizeWebUiSession` over the same records, and a
   session without compaction matches the pre-change reader exactly (`cursor-cache.test.ts`).
2. **Work count:** an unchanged transcript rewrites nothing; an appended round adds one entry and
   reads only the appended bytes.
3. **Paging:** union of pages == full normalization, and message ids stay stable across both.
4. **Invalidation:** a replaced (inode change) or truncated transcript rebuilds; a torn trailing
   record is left for the next read; records without a session turn are ignored.
5. **HTTP:** idle tab → `304` with an empty body; an append → `200` with a new ETag; a cursor page →
   the older turns; invalid cursors → `400` (`server.test.ts`).
6. **Cap:** a snapshot cuts only oversized parts, keeps them valid UTF-8, reports full byte sizes and
   recomputes derived fields (`part-cap.test.ts`); a capped turn message and a capped compacted
   message both come back whole from the message endpoint (`server.test.ts`).
7. **History source:** compacted history comes from the backup with reasoning and tool calls intact
   while the compacted items do not carry them; a preserved response is trimmed at the seam and
   rendered once; `keepbackup.turns` caps the block and `0` hides it (`cursor-cache.test.ts`).
8. **Split validators:** a snapshot carries the tail plus `history: {etag, count}`, never the block;
   `/history` serves the same messages the view holds, answers `304` to its own fingerprint, and a
   capped block message hydrates whole from it (`server.test.ts`). Verified in the browser against a
   copy of the live session: one `1359954 B` snapshot and one `1673643 B` history fetch, then 300-byte
   `304` revalidations, with the block rendering ahead of the tail and hydration updating it in place.
