# Phase 8 merged review: transcripts and media

Phase: p8 (`internal/transcript`, `imagestore`, `imageresize`, `imagedata`, `contentlibrary`; 7,296 non-test lines per `audit_plan.md` 3.2).
HEAD: `632ebed9964f73ca6a3efde654931eff97914071`.
Coordinator: merge per `audit_plan.md` 6.2 and 7.2.
Date: 2026-10-30.

## Coverage gap: r4 dropped

Reviewer r4 was dropped by the user's decision and produced no `audit_findings/p8_r4.md`. This merge uses three reviewers: r1, r2 and r3. Findings that only r4 would have covered are not known, so the merge cannot rule out findings missed by all three. Consensus is 2+ of the 3 reviewers that ran, and the pair counts below cover three pairs.

## Inputs

- `audit_findings/p8_r1.md`: 5 numbered findings (1 medium, 4 low), 6 hypotheses, 7 performance hypotheses.
- `audit_findings/p8_r2.md`: 6 numbered findings (1 medium, 5 low), 15 hypotheses, 11 performance hypotheses.
- `audit_findings/p8_r3.md`: 8 numbered findings (1 medium, 7 low), 12 hypotheses, 8 performance hypotheses.

## Commands run by the coordinator

| Command | Exit | Result |
|---|---|---|
| `git rev-parse --short HEAD` | 0 | `632ebed` |
| `go test ./internal/transcript/ -run TestP8ReproResetCrashWindow -count=1 -v` (in a `/tmp` copy, removed afterwards) | 0 | 6 items served after a simulated crash between `ReplaceRecords` and `removeDerivedFiles` |
| `go test ./internal/transcript/ -run TestP8ReproControlCacheRemoved -count=1 -v` (same `/tmp` copy) | 0 | 0 items served when both cache files are removed (control) |
| `git diff --check` on this file | 0 | clean |

No benchmark, `-race`, profile, `-count>1`, `go test ./...`, network or port was used. No repo file was changed. The coordinator did not run `go build`, `go vet`, `gofmt` or the phase test suite; the reviewers' reported results are cited as reported.

## Table

Severity: the rating after verification. Status: `confirmed` (source read or reproduced), `disproved`, or `unverified`. "Found by" uses the reviewer ids and their numbered finding.

| id | title | severity | lens | location | found by | status |
|---|---|---|---|---|---|---|
| P8-M-01 | Rewritten transcript and cache renames have no parent-directory fsync | medium | A | `transcript/jsonl.go:126`, `maintenance.go:653`, `maintenance.go:793`, `front_compact.go:279` | r1 (P8-R1-01), r2 (P8-R2-03), r3 (P8-R3-05) | confirmed (read) |
| P8-M-02 | Reset publishes the empty transcript before deleting the compaction cache | medium | A | `transcript/maintenance.go:668-671`, `maintenance.go:683-712`, `input_history.go:169-225`, `maintenance.go:820-838` | r2 (P8-R2-01) | confirmed (reproduced for the Reset window); CompactHistory case (b) confirmed by read only |
| P8-M-03 | Library asset reads are unbounded and repeated per request | medium (r1 dissent: low) | B | `contentlibrary/library.go:302-330` (`readAsset`, `os.ReadFile` at :316), `:66-99`, `:101-135` | r1 (P8-R1-02), r3 (P8-R3-03) | confirmed (read); severity contested, see contradictions |
| P8-M-04 | Failed BeginTurn or Commit leaves a staging directory that is never removed | low | A | `transcript/turn.go:170-194`, `turn.go:339-422`, `recovery.go:107-160` | r2 (P8-R2-02) | confirmed (read) |
| P8-M-05 | Image writes have no fsync; dedupe checks size only | low | B, A | `imagestore/store.go:479-481`, `store.go:602-626` | r1 (P8-R1-03) | confirmed (read); impact narrower, see below |
| P8-M-06 | Image store mutex covers the first-write directory scan and file I/O (B-C5) | low | C, A | `imagestore/store.go:479-491`, `app/bound_runner.go:51,68`, `app/subagent.go:90`, `app/session_settings.go:146` | r1 (P8-R1-04) | confirmed (read); prior art B-C5 is accepted-low, not open |
| P8-M-07 | Commit observer runs while the per-turn mutex is held | low | A | `transcript/turn.go:343` (lock), `turn.go:405` (observer), `turn.go:424-434` | r1 (P8-R1-05), r3 (P8-R3-01) | confirmed (read); no deadlock found |
| P8-M-08 | Transcript reads open the file O_RDWR|O_CREATE and may truncate | low | B, A | `transcript/jsonl.go:286-306` (`withRepairedFile`), `jsonl.go:255-284`, `jsonl.go:221` | r2 (P8-R2-04) | confirmed (read); not reproduced |
| P8-M-09 | A completed output item is validated and scanned twice per expansion | low | C | `transcript/input_history.go:280-289`, `jsoncompat.go:22-29`, `jsoncompat.go:284-306` | r2 (P8-R2-05) | confirmed (read); cost unmeasured |
| P8-M-10 | Whole-file SHA-256 helpers duplicate `HashFileRange` | low | D | `transcript/recovery.go:272`, `maintenance.go:796`, `prefix_hash.go:14` | r2 (P8-R2-06), r3 (P8-R3-07) | confirmed (read) |
| P8-M-11 | Cursor-index read does not serialize with transcript rewrites | low | A | `transcript/cursor_cache.go:59-113` (lock at :61), `cursor_cache.go:125` | r3 (P8-R3-02) | confirmed (read); transient, self-healing |
| P8-M-12 | `imagedata.Decode` copies the whole input before the size gate | low | B | `imagedata/imagedata.go:28-40` (copy at :31, gate at :37) | r3 (P8-R3-04) | confirmed (read) |
| P8-M-13 | CompactHistory no-op result still names a backup path and full counts | low | D | `transcript/maintenance.go:90` (`BackupPath` set), `maintenance.go:138` (no-op return) | r3 (P8-R3-06) | confirmed (read) |
| P8-M-14 | Resizer memo holds full replacement data URLs, bounded by entries times size | low | C | `imageresize/apply.go:187-218` | r3 (P8-R3-08) | confirmed (read); operator-bounded |
| P8-M-15 | PNG encode uses `png.BestCompression` | low | C | `imageresize/resize.go:183` | r2 (H11, flagged possible duplicate) | duplicate of prior art P2B2-27; not counted as new |

Distinct findings: 14 (P8-M-01 to P8-M-14). P8-M-15 is a duplicate of prior art and is excluded from the count.

## Consensus (filed by 2+ reviewers)

Three items are consensus. The table's "found by" column shows them.

- **P8-M-01 (medium, lens A).** Found by r1, r2 and r3. Same cause and locations: a rename publishes a file without a parent-directory sync. Median severity: r1 medium, r2 low, r3 low. Verified by source: the publish sites are `jsonl.go:126` (`ReplaceRecords`), `maintenance.go:653` (`backupTranscript`), `maintenance.go:793` (`writeCompactionJSONAtomic`), `front_compact.go:248` and `:279`, and `turn.go:722` (`writeJSONAtomic`, which is the staging-manifest writer, not a transcript publish). Syncs of the directory exist only at `jsonl.go:205`, `jsonl.go:526` and the turn staging calls (`turn.go:293`, `:384`, `:469`, `:479`). The contrast site `cursor_cache.go:267-285` syncs. Severity: kept medium; see contradictions.
- **P8-M-03 (library reads).** Found by r1 and r3. Same cause: `readAsset` reads each file whole with no size cap, and `List`/`Read` walk the tree per request. Verified: `library.go:316` (`os.ReadFile`, unbounded), and the walk in `markdownFiles` (`:232-265`). Median severity: low (r1) and medium (r3). See contradictions.
- **P8-M-07 (observer under lock).** Found by r1 and r3. Same site: the observer is called at `turn.go:405` while `t.mu` is held from `turn.go:343`. Both rate it low. No deadlock: `unregister` takes `s.mu` under `t.mu` (`turn.go:441-444`), and no path found takes `s.mu` then `t.mu`.
- **P8-M-10 (duplicate SHA-256 helpers).** Found by r2 and r3. Same cause: `hashFile` (`recovery.go:272`), `hashTranscriptFile` (`maintenance.go:796`) and `HashFileRange` (`prefix_hash.go:14`) each stream a whole file through `sha256.New`. Both rate it low.

Consensus count: 4 items (P8-M-01, P8-M-03, P8-M-07, P8-M-10).

## Unique (filed by one reviewer) and verification

Every unique finding and every medium-or-higher finding is verified here. Outcome is `confirmed`, `disproved` or `unverified`.

- **P8-M-02 (medium, r2 only). Confirmed for the Reset window.** Source: `Reset` (`maintenance.go:656-681`) runs `journal.ReplaceRecords()` at `:668`, then `removeDerivedFiles(true, false)` at `:671`. `removeDerivedFiles` (`:683-712`) deletes the compaction cache and its `.backup` (`:703`-area, through the `paths` loop). The read path `readInputItemsWithCache` (`input_history.go:169-225`) prepends `cache.Items` whenever a cache exists, and `cacheSourceIsTranscript` (`maintenance.go:820-838`) returns false because `source_bytes` no longer matches the emptied transcript, so the cache items are served.
  Reproduction, in a `/tmp` copy of the module: 10 turns, `CompactHistory` with `KeepTurns: 3`, then `journal.ReplaceRecords()` alone (the crash point), then `ReadInputItems`. Result: 6 items served. Control with both `compaction-cache.json` and `.backup` removed: 0 items. So a crash in that window serves the deleted conversation until another mutation. Not reproduced: the CompactHistory case (b), where `ReplaceRecords` fails after `writeCompactionCache` succeeded (`maintenance.go:174-181`). By source: `CompactHistory` returns an error and `Compacted=false`, while the cache matches the unchanged transcript, so the next read serves the compacted view. Severity medium is kept for the Reset window: the state persists until the next mutation.
- **P8-M-04 (low, r2 only). Confirmed (read).** `BeginTurn` (`turn.go:151-198`) creates the turn directory at `:170` and returns on a `persistManifest` error (`:189`) or a `syncTurnEntries` error (`:194`) with no removal. The `RemoveAll` sites are `recovery.go:139`, `:151` and `turn.go:410`, all on success paths. Recovery counts the leftover as retained (`recovery.go:107-160`).
- **P8-M-05 (low, r1 only). Confirmed (read), impact narrower.** `writeAtomic` (`store.go:602-626`) has no `Sync` and no directory sync. The dedupe check (`store.go:479-481`) compares size only. r1 says `Open` detects a corrupt file on read. That holds for `Open` (`store.go:520-559`, content sniff against the name). Impact: a power loss can leave an image that is then unreadable.
- **P8-M-06 (low, r1 only). Confirmed (read).** `putPayload` holds `s.mu` across `os.Stat` (`store.go:479`), `directorySize` (`:485`, once per Store) and `writeAtomic` (`:491`). Construction per run confirmed: `bound_runner.go:51` and `:68`, `subagent.go:90`, `session_settings.go:146`, all through `newImageStore` (`app/image_resize.go:17-18`). Prior art: `audit_audit.md:31` records B-C5 as ACCEPT-LOW (merged into D-9), so r1's "still open" is out of date. Cost unmeasured.
- **P8-M-08 (low, r2 only). Confirmed (read), not reproduced.** `withRepairedFile` (`jsonl.go:286-306`) opens with `O_CREATE|O_RDWR|O_APPEND` under `j.mu`, and `ReadRecordsRange` (`:255-284`) uses it. The flags mean a read needs write permission and can create an empty file. I did not chmod a file to reproduce it.
- **P8-M-09 (low, r2 only). Confirmed (read).** `expandCompletedRecord` (`input_history.go:280`) calls `objectFieldsValid(item)`, then `canonicalJSONValid(item)` at `:289`, which re-parses the object (`jsoncompat.go:284-306`). `input_history.go:312` already has `canonicalJSONFields`, the reuse point r2 names. The double scan is real; cost unmeasured.
- **P8-M-11 (low, r3 only). Confirmed (read).** `ReadCursorIndex` (`cursor_cache.go:59-113`) takes only `cursorMu` (`:61`). Writers in `maintenance.go` and `front_compact.go` hold `s.mu`, not `cursorMu`. A concurrent rewrite can make the scan see a shrunk transcript, returning `transcript shrank while rebuilding cursor index` (`cursor_cache.go:125`). Self-healing on the next read.
- **P8-M-12 (low, r3 only). Confirmed (read).** `Decode` (`imagedata.go:28-40`) runs `strings.Map` (a full copy, `:31`) before the length gate (`:37`).
- **P8-M-13 (low, r3 only). Confirmed (read).** `result.BackupPath` is set at `maintenance.go:90`, before the no-op return at `:138`, so the no-op result names a backup path and full counts.
- **P8-M-14 (low, r3 only). Confirmed (read).** `resized` (`apply.go:187-216`) caches each replacement, keyed by source-URL hash, up to `CacheEntries` (default 64, max 4096, `sessionfs/image_resize_settings.go:21-23`). The replacement is bounded by `max_bytes` (default 1 MiB, max 20 MiB, `:12-14`). Operator-bounded.
- **P8-M-15 (duplicate of prior art, r2 H11). Confirmed.** `pngEncoder` uses `png.BestCompression` at `resize.go:183`. This is `P2B2-27` in `audit_findings/p2b2_merged.md` (row 27, low, measured in that phase). Not counted as new.

Medium or higher findings and their verification: P8-M-01 (read, consensus), P8-M-02 (reproduced for the Reset window), P8-M-03 (read, consensus).

### Specific claims requested

- **Directory fsync gap** (r1 R1-01 medium, r2 R2-03, r3 P8-R3-05). **Confirmed by source.** Five publish sites have no directory sync: `jsonl.go:126`, `front_compact.go:279` (and `:248` for the archive), `maintenance.go:653` and `:793`. The syncing comparison is `cursor_cache.go:267-285`. The sync is gated on `durability != process`. `dir_sync_test.go` covers the commit path only (`TestDirectorySyncPerStep` at `:46`, `TestJournalCreationSyncsItsDirectoryOnce` at `:177`). Not reproduced: a crash or power-loss harness is not allowed in this step.
- **Reset/CompactHistory publish order** (r2 P8-R2-01, medium). **Confirmed for Reset, by reproduction.** See P8-M-02. The CompactHistory case is confirmed by source only.
- **Unbounded library reads** (r1 R1-02, r3 P8-R3-03 medium). **Confirmed by source; the medium rating is contested.** `readAsset` (`library.go:302-330`) reads the full file at `:316` with no cap. Reachability: `/api/library` is behind `h.authorized` (`httpapi/handler.go:322-325`), so an unauthenticated peer cannot reach it. The roots are operator-configured. Prior art: D-8 (P-009) was rejected by the user (`audit_performance_plan.md:89`, `audit_audit.md:19`). Result: confirmed as a cost, not as an exposure. See contradictions.

## Contradictions

1. **Directory fsync severity** (P8-M-01). r1 rates medium. r2 and r3 rate low. Evidence: the gap is confirmed at five sites. The effect needs a power loss or kernel crash, since a process crash after `rename` is safe. The compaction and reset caches self-heal from the source hash, so the consequence there is a lost compaction, not lost data. The one user-visible case is front compaction with `delete_mode=permanent` (`front_compact.go:39`, `:131-147`): a power loss can bring back messages the user deleted. Result: medium kept, because of that case. Both positions recorded.
2. **Library read severity** (P8-M-03). r3 rates medium, r1 rates low. r3's medium rests on the unbounded read and on a root symlink being followed. I checked the symlink point: `library.go:201` calls `os.Stat(root)`, which follows a symlinked root. That is a configuration choice by the operator, not an attacker path. The unbounded read needs a large file in an operator-configured root, and the token holder is the only caller. The cost is prior art D-8, rejected by the user. Result: low, pending a user decision. Not promoted in this merge.
3. **B-C5 status** (P8-M-06). r1 says "still open". `audit_audit.md:31` says ACCEPT-LOW. Prior-art record is authoritative; r1's "open" is out of date.
4. **Observer under lock** (P8-M-07). r1 says no deadlock. r3 says the lock stalls staging cleanup. These do not conflict: both agree on the lock and on the absence of a cycle; they differ on consequence. Both low.
5. **Reset line references.** r2 cites `maintenance.go:656-681` for Reset, which matches the source. No conflict.
6. **Library catalog caching** (r2 H13). r2 says the catalog is not cached, and this is documented. r3 agrees. No conflict.
7. **Image store per-run scan** (r1 P8-R1-04, r2 H5, r3 H5). All three agree on the cause and on a shared Store as the fix. Agreement.
8. **Phase boundary, A-8** (r2 H-14). r2 says the phase 8 risk-row item "content cache fallback (A-8)" is in `httpapi/run_content_cache.go` (phase 3). Verified: the file exists at that path and nothing in the five phase-8 packages implements it.

## Reviewer-pair agreement

Counts use the normalised key (cause and file:line). Each count is over numbered findings. r2's hypotheses that are not numbered findings are not counted, and P8-M-15 (prior art duplicate) is not counted.

| Pair | Both filed | First only | Second only | Severity differs (both filed) | Verdict differs |
|---|---|---|---|---|---|
| r1 and r2 | 1 (P8-M-01) | 4 (r1: P8-M-03, P8-M-05, P8-M-06, P8-M-07) | 5 (r2: P8-M-02, P8-M-04, P8-M-08, P8-M-09, P8-M-10) | 1 (P8-M-01: r1 medium, r2 low) | 0 |
| r1 and r3 | 3 (P8-M-01, P8-M-03, P8-M-07) | 2 (r1: P8-M-05, P8-M-06) | 5 (r3: P8-M-10, P8-M-11, P8-M-12, P8-M-13, P8-M-14) | 2 (P8-M-01: r1 medium, r3 low; P8-M-03: r1 low, r3 medium) | 0 |
| r2 and r3 | 2 (P8-M-01, P8-M-10) | 4 (r2: P8-M-02, P8-M-04, P8-M-08, P8-M-09) | 6 (r3: P8-M-03, P8-M-07, P8-M-11, P8-M-12, P8-M-13, P8-M-14) | 0 (P8-M-01 low in both; P8-M-10 low in both) | 0 |

Per-reviewer numbered findings: r1 5 (P8-M-01, P8-M-03, P8-M-05, P8-M-06, P8-M-07). r2 6 (P8-M-01, P8-M-02, P8-M-04, P8-M-08, P8-M-09, P8-M-10). r3 8 (P8-M-01, P8-M-03, P8-M-07, P8-M-10, P8-M-11, P8-M-12, P8-M-13, P8-M-14).

The verdict count is 0 for every pair: no reviewer marked a shared item checked-and-sound while another filed it as a bug. The contradictions above cover severity and consequence wording only.

## Disproved claims

No reviewer finding was disproved. Hypotheses that were checked and rejected, re-verified here:
- r1 H8: `containsLineHash` is not dead code. It is used by `containsHashText` (`jsonl.go:437`), reached from recovery (`recovery.go:146`).
- r1 H12: `/api/library` is not reachable without a token (`handler.go:322-325`).
- r1 H10, r2 H-07, r3 HYP-12: no lock-order inversion; `s.mu` is taken before `j.mu` and `t.mu` before `s.mu`, and no path takes `s.mu` then `t.mu`.
- r2 H-14: the A-8 risk-row item is not in phase 8 (see contradictions, item 8).

## Counts by severity (after verification)

| Severity | Count | Items |
|---|---|---|
| critical | 0 | |
| high | 0 | |
| medium | 3 | P8-M-01 (consensus), P8-M-02 (r2), P8-M-03 (r1, r3; r1 dissent: low, medium per user decision) |
| low | 11 | P8-M-04, P8-M-05, P8-M-06, P8-M-07, P8-M-08, P8-M-09, P8-M-10, P8-M-11, P8-M-12, P8-M-13, P8-M-14 |
| duplicate of prior art | 1 | P8-M-15 (P2B2-27) |

Total distinct findings: 14 (3 medium, 11 low). Consensus: 4. Unique: 10 (P8-M-02, P8-M-04, P8-M-05, P8-M-06, P8-M-08, P8-M-09, P8-M-11, P8-M-12, P8-M-13, P8-M-14). Shared with 2+ reviewers: 4 (P8-M-01, P8-M-03, P8-M-07, P8-M-10). Counting: 10 + 4 = 14.

## Performance measurement list (unmeasured)

All items are hypotheses. None was measured in this step. Each names an existing benchmark or a proposed one.

| ID | Hypothesis | Source | Location | Benchmark | Status |
|---|---|---|---|---|---|
| PM-01 | Compaction backup copies the whole accumulated backup per compaction; total I/O is quadratic in compactions | r2 H1 | `maintenance.go:594-654` | `BenchmarkCompactHistory1000Turns` at 1, 10, 50 successive compactions (new sub-benchmark) | unmeasured |
| PM-02 | A committed response re-parses and re-canonicalises each staged item (about four parses, two re-encodes) | r2 H2 | `turn.go:224-320`, `render.go:28-120`, `turn.go:584-625` | `BenchmarkTurnLifecycleUniqueResponse` with 1 MiB and 10 MiB items (new sub-sizes) | unmeasured |
| PM-03 | The canonical final line is hashed twice and read three times per commit | r2 H3 | `turn.go:526-583`, `jsonl.go:440-530` | `BenchmarkTurnLifecycleUniqueResponse` at 10 MiB, or a new large-line commit benchmark | unmeasured |
| PM-04 | `AppendStateContext` expands the whole transcript to count items, under `s.mu` | r2 H4, r3 H2 | `state_context.go:40-86`, `:151-175` | new `BenchmarkAppendStateContext` at 100, 1000, 5000 turns | unmeasured (prior P-005, deferred) |
| PM-05 | First image write per Store scans the whole image directory; repeats each run | r1 H-P5, r2 H5, r3 H5 | `imagestore/store.go:456-499`, `:629-644` | `BenchmarkDirectorySize` and `BenchmarkPutPayloadParallel` (existing), new first-write benchmark | unmeasured (prior P-007: about 2.5 us per file) |
| PM-06 | Library `List` and `Read` re-read and re-parse every file per request | r1 H-P6, r2 H7, r3 H6 | `contentlibrary/library.go:66-160`, `:302-330` | `BenchmarkLibraryList1000MarkdownFiles` (existing), plus a `Read` case | unmeasured (prior P-009: rejected by the user) |
| PM-07 | `NextSessionTurn` parses the whole transcript per call (fallback path only) | r2 H6, r3 H1 | `session_turn.go:12-73` | new direct-call benchmark at 1000 and 5000 turns | unmeasured |
| PM-08 | `ReadCursorIndex` re-hashes the checkpoint window on every read, even with no growth | r3 H3 | `cursor_cache.go:160-189` | new no-change `ReadCursorIndex` benchmark at two sizes | unmeasured |
| PM-09 | `CompactHistory` and `FrontCompactFromMessage` hold `TurnStore.mu` across full-transcript reads and copies | r3 H4, r2 H9 | `maintenance.go:45-190`, `front_compact.go:34-153` | `BenchmarkCompactHistory1000Turns`, `BenchmarkFrontCompactRecoverable1000Turns` (existing) | unmeasured |
| PM-10 | One compaction makes about four passes over the transcript under `s.mu` | r2 H9 | `maintenance.go:75-80`, `:192-232`, `:336-445`, `:796-810`, `:594-654` | `BenchmarkCompactHistory1000Turns`, `BenchmarkCompactHistoryWithCache` (existing) | unmeasured |
| PM-11 | The read path validates each record and each output item twice | r2 H10, P8-M-09 | `jsoncompat.go:22-29`, `input_history.go:280-289` | `BenchmarkReadInputItems1000Records` (existing; prior P-027: 3.054 ms, 44.02k allocs) | unmeasured |
| PM-12 | `readTranscriptItemsMemo` copies the memo slice on every read (slice headers only) | r1 H-P1 | `input_history_memo.go:96-108` | `BenchmarkRunRoundsHistory`, `BenchmarkRunRoundsHistoryCompacted` (existing) | unmeasured; r1 expects a small cost |
| PM-13 | `ReadRecordsRange` and `ReadCompleteRecordsRange` hold `j.mu` for the whole read, blocking appends | r1 H-P2 | `jsonl.go:255-281`, `:345-418` | new `BenchmarkJournalReadWhileAppending` (10 MB journal, one reader, one appender) | unmeasured; deferred as low by user decision |
| PM-14 | `lineIndex.hashes` has no bound; memory grows with transcript lines | r1 H-P4 | `line_index.go:70-105` | `BenchmarkLineIndexMemory` (existing) | unmeasured |
| PM-15 | Resizer decodes outside the lock; with more than 64 distinct images the FIFO thrashes | r1 H-P7 | `imageresize/apply.go:187-216` | new `BenchmarkApplyHistoryManyImages` (100 images, 64 entries) | unmeasured; deferred as low by user decision |
| PM-16 | Resizer memo holds up to entries times replacement bytes | r3 H8 | `imageresize/apply.go:187-218` | apply benchmark with benchmem at two settings | unmeasured |
| PM-17 | Each run re-decodes, rotates per pixel, and re-encodes every large image; `png.BestCompression` dominates | r2 H8, H11 | `imageresize/apply.go:67-210`, `orient.go:75-109`, `resize.go:183` | new `BenchmarkApplyHistoryWithLargeImages` (1 MiB png, 3 MiB jpeg, 3 MiB webp) | unmeasured; the PNG cost is prior art P2B2-27; deferred as low by user decision |
| PM-18 | `Rehydrate` plus `Apply` re-scan history bytes every provider round | r3 H7 | `imagestore/store.go` (`Rehydrate`), `imageresize/apply.go:67-124` | `BenchmarkRehydrateHistoryWithoutImages` (existing), apply benchmark | unmeasured; prior P2B2 PH11 gives 0.11 ms per 100 KB |

Overlap with prior measurements (P-005, P-007, P-009, P-027, P2B2-27) is noted per row. None of those prior numbers is re-measured here. Measurement list size: 18.

## Cross-phase leads (from reviewers, not findings here)

- `session/runner_response.go:122` calls the observer under the turn mutex (P8-M-07). Phase 2b2.
- `app/bound_runner.go:51,68`, `app/subagent.go:90`, `app/session_settings.go:146`: per-run image store and resizer (P8-M-06). Phases 1 and 6c.
- `app/turn_starts.go:36`: per-run `ReadCursorIndex` (P8-M-11). Phase 1.
- `session/runner.go:185-200` and `:459`: two full history expansions per run (r2 section 5). Phase 2b2.
- `contentlibrary` ConfigError messages embed absolute paths, which reach the 422 body (r2, filed as `P3-R2-04` in phase 3). Not counted here.
- `httpapi/mutation_handlers.go:59-100`: the only network route to Reset, Compact and FrontCompactFromMessage. Phase 3.
- `internal/tools/native/read_image.go` and `image_file.go`: imagestore callers (r1 XP-5). Phase 7a.

## Checked and sound (merged across reviewers)

Each line was checked by at least two reviewers and re-read by the coordinator:
- Commit ordering and idempotent recovery (`turn.go:339-422`, `recovery.go:96-160`).
- Lock order: no path takes `s.mu` then `t.mu`; `unregister` runs under `t.mu` only.
- `appendFileIfAbsent` uses the in-memory line index (F-07 fix, `jsonl.go:440-532`).
- Compaction cache hash skipped on size mismatch (F-02 fix, `maintenance.go:820-838`).
- P-012, P-013, P-014 fsync reductions (`turn.go:486-508`, `:626-654`, `jsonl.go:153-213`).
- Turn IDs cannot escape the staging directory (`validTurnID`, `turn.go:725-739`).
- Image opens are link-safe (`store.go:520-559`); EXIF walk is bounds-checked (`orient.go:11-74`); the decompression-bomb guard runs before decode (`resize.go:80-90`).
- Library walk skips symlink entries (`library.go:222-315`); the route is gated (`handler.go:322-325`).
- `imagedata` size bound is an upper bound for both alphabets (`imagedata.go:37-40`).
- Build, vet and gofmt for the five packages: each reviewer reports exit 0; not re-run by the coordinator.

## Not covered

- r4 was dropped (see the coverage gap). Findings only r4 would have covered are unknown.
- The coordinator did not run `go build`, `go vet`, `gofmt`, or the phase test suite. Reviewer results are cited as reported.
- Crash and power-loss behaviour was reasoned from source. No fault injection was run.
- The CompactHistory mirror case of P8-M-02 (`ReplaceRecords` failing after the cache write) was not reproduced.
- P8-M-08 (read-only transcript) was not reproduced; its analysis is from the flags in source.
- The measurement list is unmeasured by policy of this step.
