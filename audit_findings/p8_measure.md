# Phase 8 measurement (section 6.3)

Phase: 8 (transcript and media). Input: `audit_findings/p8_merged.md` performance list (PM-01 to PM-18). Reviewer coverage: r1, r2, r3 (r4 dropped by user decision; no `p8_r4.md`; that gap is recorded in `p8_merged.md`).

Method: relative only. Each claim compares two workload sizes, or a current benchmark against a `/tmp` overlay, inside one session. Spread is (max - min) / median over the 10 runs; a row above 5% is repeated once; a row still above 5% is `noisy` and not used for a claim. Load average is the 1-minute value from `/proc/loadavg` before and after each block.

Machine: 12 cores, not quiet. A Firefox process was using about 28% CPU at one check (`ps`, 22:27), outside this work. It was not stopped. Each benchmark ran alone from this session, one `go test` at a time, with no build or test in parallel. The machine was not idle, so no absolute number is claimed.

Scratch: overlays `/tmp/p8m/ov/*.go` were applied with `go test -overlay`, mapping virtual `internal/<pkg>/p8_scratch_overlay*_test.go` paths to `/tmp`. Nothing was written under the repo. `/tmp/p8m` is removed at the end of the step.

## Verdict counts

| Verdict | Count | Hypotheses |
|---|---|---|
| confirmed | 6 | PM-04, PM-05, PM-06, PM-07, PM-09, PM-14 |
| not confirmed | 4 | PM-01, PM-02, PM-03, PM-08 |
| noisy | 1 | PM-12 |
| unmeasured | 7 | PM-10, PM-11, PM-13, PM-15, PM-16, PM-17, PM-18 |
| total | 18 | |

## Per-hypothesis results

Numbers are the median of 10 runs (ns/op, B/op, allocs/op). Load is the 1-minute average before and after the block. "Rep" marks a repeat run after a first run above 5%.

| ID | Command (`go test -run '^$' -bench ... -benchmem -count=10`) | Load before/after | Numbers | Verdict | Reason |
|---|---|---|---|---|---|
| PM-01 | `./internal/transcript`, overlay `BenchmarkP8SuccessiveCompactions` | 1.04 / 1.94 | 1 compaction 13.1 ms; 10: 26.5 ms; 50: 85.5 ms (spreads 1.2 to 1.9%) | not confirmed | Total grows linearly, about 1.5 ms per extra compaction (13.4 ms for 9 more, 59 ms for 40 more). No quadratic term. The workload uses `BackupMode: "replace"`, so accumulation of the backup is not exercised; the accumulating mode was not measured. |
| PM-02 | `./internal/transcript`, overlay `BenchmarkP8LargeItemCommit` | 1.73 / 1.94 | 1 MiB item 24.4 ms, 10.8 MB/op; 10 MiB 228.5 ms, 105 MB/op (2.9%, 2.4%) | not confirmed | 10x the bytes gives 9.4x the time: linear. No super-linear cost at these sizes. Re-parse count is from source only; no per-stage split. |
| PM-03 | same run as PM-02 | same | same | not confirmed | The commit cost scales linearly with the 10 MiB item. The hash-twice and read-three-times counts are source facts; their cost share was not separated, so no further claim. |
| PM-04 | `./internal/transcript`, overlay `BenchmarkP8AppendStateContext` | 1.09 / 1.30 | 100 turns 0.72 ms; 1000: 7.04 ms; 5000: 37.0 ms (2.1 to 2.4%) | confirmed | Time grows with transcript length: 9.8x for 10x turns (100 to 1000), 5.3x for 5x (1000 to 5000). Runs under `s.mu`, as the source shows. |
| PM-05 | `./internal/imagestore`, `BenchmarkDirectorySize` (existing) | 0.79 / 1.22 | 100 files 0.187 ms; 1000: 2.03 ms; 10000: 25.6 ms (1.6 to 2.2%) | confirmed (scan cost only) | The scan is linear, about 2.6 us per file at 10000 (prior P-007: about 2.5 us). The claim that the first write per Store repeats each run was not measured. `BenchmarkPutPayloadParallel` store-16KB was noisy after repeat (40.8%) and is not used. |
| PM-06 | `./internal/contentlibrary`, `BenchmarkLibraryList1000MarkdownFiles` (existing); overlay `BenchmarkP8LibraryReadSizes` | 1.01 / 1.23 (Read); 2.50 / 2.35 (List rep) | List 13.3 ms (5.0%) and rep 13.8 ms (2.4%); Read 4 KiB 0.21 ms; 1 MiB 68.3 ms (2.0%, 0.9%) | confirmed | List is linear in file count and Read is linear in file bytes (about 330x for 256x bytes). Per-request re-read is from source; the cost is the measured linear term. |
| PM-07 | overlay `BenchmarkP8NextSessionTurn` | 1.40 / 1.47 | 1000 turns 2.77 ms; 5000: 13.9 ms (1.4 to 1.6%) | confirmed | 5.0x for 5x turns: linear in transcript length on the fallback path. |
| PM-08 | overlay `BenchmarkP8ReadCursorIndexNoGrowth` | 1.47 / 1.40 | 1000 turns 0.353 ms; 5000: 0.637 ms; 33.6 KB and 11 allocs both (0.9 to 1.0%) | not confirmed | `ReadCursorIndex` returns early when `currentSize == state.offset` (`cursor_cache.go:59-100`), so no-growth does not re-hash. The measured cost is flat across sizes, which matches that path. The claim as written does not hold for the no-growth case. |
| PM-09 | `./internal/transcript`, `BenchmarkCompactHistory1000Turns`, `BenchmarkFrontCompactRecoverable1000Turns`, `BenchmarkCompactHistoryWithCache` (existing) | 1.08 / 1.21; 1.21 / 1.23; 1.23 / 1.50 | CompactHistory 12.0 ms (1.7%); FrontCompact 3.14 ms (2.0%); WithCache 1000 cached 32.2 ms (4.7%), 5000 cached 146.7 ms (3.5%) | confirmed (cost) | Compaction cost scales with transcript length (4.6x for 5x cached turns). The lock-hold part (time under `TurnStore.mu`) is not separated from the rest, so that claim is not measured. |
| PM-10 | `BenchmarkCompactHistory1000Turns`, `BenchmarkCompactHistoryWithCache` (existing) | as PM-09 | as PM-09 | unmeasured | The four-pass split is from source. The benchmarks give totals only; per-pass cost was not measured. |
| PM-11 | `./internal/transcript`, `BenchmarkReadInputItems1000Records` (existing) | 1.91 / 1.79 | 2.34 ms, 1.77 MB, 28.0k allocs (2.1%) | unmeasured | Only one workload size exists for this benchmark, so no relative measurement. The double-validation share was not isolated. Prior P-027 (3.054 ms, 44.02k allocs) is not re-measured here. |
| PM-12 | `./internal/transcript`, `BenchmarkRunRoundsHistory` (existing) | 2.35 / 1.93 (rep) | memo: 100 turns 3.22 ms (3.9% rep), 1000: 23.2 ms (5.8% rep, noisy), 5000: 114.6 ms (2.5% rep). full 1000 rep 7.2% (noisy) | noisy | The memo row at 1000 turns stayed above 5% after repeat. The slice-copy cost (r1 H-P1) is expected to be small and was not isolated from the rest of the read. Not used for a claim. |
| PM-13 | none run (`ReadRecordsRange` lock hold needs a concurrent reader and appender) | n/a | n/a | unmeasured | No benchmark exists for this case and none was written. `BenchmarkReadHistoryCompactedLargeTranscript` (10, 50, 250 MB: 0.149, 0.154, 0.214 ms) measures a different path. |
| PM-14 | `./internal/transcript`, `BenchmarkLineIndexMemory` (existing) | 1.93 / 1.83 (rep) | 5000 lines 1.85 ms, 692 KB, 89 allocs (2.6%); 20000 lines 5.68 ms, 2.69 MB, 582 allocs (4.9%) | confirmed | Memory grows linearly with lines (3.9x B/op for 4x lines); no bound in the code. Time 3.1x for 4x. |
| PM-15 | overlay `BenchmarkP8ApplyManyImages` (16 and 100 distinct 1200 px PNGs, cache 64) | 1.80 / 1.69 | 16 images 103.9 ms, 1.5 KB, 18 allocs (1.5%); 100 images 648 ms, 8.6 KB, 179 allocs (2.4%) | unmeasured | Per-image time is the same at 16 (6.5 ms) and 100 (6.5 ms), but 16 images should hit the 64-entry memo, and 1.5 KB per op is too few allocations for re-decoding. The thrash mechanism is not explained by these numbers, and hit/miss counts were not measured. |
| PM-16 | none run (resizer memo bytes need a heap measure) | n/a | n/a | unmeasured | Only `B/op` was measured elsewhere; the memory held by memo entries was not measured. |
| PM-17 | overlay `BenchmarkP8ApplyManyImages` (same run as PM-15) | same | same | unmeasured | Shows a per-image cost but not the share of `png.BestCompression`. That needs a stage split or a pprof. pprof was not run: the plan allows it only where a cost is shown, and the stage split is not established. Prior P2B2-27 names BestCompression. |
| PM-18 | `./internal/imageresize`, `BenchmarkApplyHistoryWithoutImages` (existing) and `./internal/imagestore`, `BenchmarkRehydrateHistoryWithoutImages` (existing) | 1.83 / 1.51 (Apply rep); 2.60 / 2.71 (Rehydrate) | Apply 200/1000/5000 pairs: 0.191, 0.955, 5.20 ms (1.9, 2.3, 5.1 rep); Rehydrate 0.195, 0.964, 5.41 ms (1.8, 2.6, 3.1%) | unmeasured | The claim is about re-scanning history with images. The benchmarks measure the no-image path, which returns early when no image marker is present (`apply.go:72-74`), and it scales linearly (not a with-image measure). The with-image cost was not measured. The Apply 5000 row is 5.1% after repeat, so it is not used alone. |

Verdict legend: confirmed = relative trend beyond noise; not confirmed = measured and contrary to the claim; noisy = a row above 5% after repeat, not used; unmeasured = no benchmark run or no relative comparison possible.

## Commands and exit codes

Every benchmark block above was run as `go test <flags> <pkg> -run '^$' -bench <regex> -benchmem -count=10` through `/tmp/p8m/run.sh` (rc = go test exit code). Overlay blocks add `-overlay=/tmp/p8m/ov*.json`.

| Block | Exit code |
|---|---|
| PM-05 DirectorySize | 0 |
| PM-05 PutPayloadParallel (first, and rep) | 0, 0 |
| PM-06 LibraryList (first, and rep) | 0, 0 |
| PM-11 ReadInputItems1000Records | 0 |
| PM-12 RunRoundsHistory (first, and rep) | 0, 0 |
| PM-14 LineIndexMemory (first, and rep) | 0, 0 |
| PM-18 ApplyHistoryWithoutImages (first, and rep) | 0, 0 |
| PM-18 RehydrateHistoryWithoutImages | 0 |
| PM-09 CompactHistory1000Turns, FrontCompactRecoverable, CompactHistoryWithCache | 0, 0, 0 |
| PM-02/03 TurnLifecycleUniqueResponse | 0 |
| PM-13 ReadHistoryCompactedLargeTranscript | 0 |
| PM-06 LibraryReadSizes (overlay) | 0 |
| PM-01 SuccessiveCompactions (overlay) | 0 |
| PM-02 LargeItemCommit (overlay) | 0 |
| PM-04 AppendStateContext (overlay) | 0 |
| PM-07 NextSessionTurn (overlay) | 0 |
| PM-08 ReadCursorIndexNoGrowth (overlay) | 0 |
| PM-15/17 ApplyManyImages (overlay) | 0 |

Failed or non-benchmark commands (not measurements):
- Several first attempts ran from a wrong working directory and failed with `go.mod file not found` (rc 1, no benchmark output). Those outputs were discarded and the blocks rerun from `backend-go`.
- One `ls`/`git status` check (rc 1) used repo paths from the wrong directory. Rerun from the repo root: `git status --short` on the four package directories printed nothing (rc 0), so no repo file changed by this step.
- `go test -race -count=1`: skipped. No hypothesis depends on a race result, and no test was run for this step.
- `pprof`: not run (see PM-17).
- Graph lookups for `Library.Read` and `Apply`/`ReadCursorIndex` returned "symbol not found" and were replaced by `read.sh` line ranges. Source for PM-08 and PM-18 comes from those ranges.

## Noisy rows (above 5% after repeat, not used)

- `BenchmarkPutPayloadParallel/store-16KB`: 9.4% first, 40.8% rep.
- `BenchmarkRunRoundsHistory/full/100-turns`: 19.9% first, 5.2% rep.
- `BenchmarkRunRoundsHistory/memo/1000-turns`: 15.7% first, 5.8% rep (drives PM-12).
- `BenchmarkRunRoundsHistory/full/1000-turns`: 7.8% first, 7.2% rep.
- `BenchmarkApplyHistoryWithoutImages/5000-tool-pairs`: 7.1% first, 5.1% rep.

Rows that went above 5% on the first run and fell below it on repeat (used with the repeat): PM-05 DirectorySize (none; first run within 2.2%), PM-06 List (5.0% first, 2.4% rep), PM-12 memo/100 (11.2% first, 3.9% rep), PM-14 rows (5.2% and 5.6% first; 2.6% and 4.9% rep), PM-18 Apply 200/1000 (4.0 and 3.0% first; 1.9 and 2.3% rep).

## Unmeasured and why

- PM-10: per-pass split not measured; only totals exist.
- PM-11: one workload size only, so no relative measure.
- PM-13: no concurrent reader/appender benchmark exists; none was written.
- PM-15: the thrash mechanism does not match the allocation counts; cannot be claimed.
- PM-16: memo memory needs a heap measure, not run.
- PM-17: stage split needs a profile, not run.

Prior numbers (P-005, P-007, P-009, P-027, P2B2-27) are cited as context only and not re-measured.
