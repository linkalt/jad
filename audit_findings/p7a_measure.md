# Phase 7a measurement (plan 6.3)

Prepared by the executor from `audit_findings/p7a_merged.md` section 7 (items M1 to M11). Machine: 12 CPUs (Ryzen 5 3600 reported), go1.27.3, `rg` 15.2.0. The machine was shared: `npm run dev` (pid 6389) and a `dario proxy` process were running throughout. No `go build`, `go test` or `go vet` was running at the start of any block (checked with `ps`). Load averages are the 1-minute, 5-minute and 15-minute values from `/proc/loadavg`, read before and after each block. Absolute numbers are context only; the verdicts rest on relative comparisons and on spreads.

Rules applied: each item was run alone, `-run '^$' -bench <name> -benchmem -count=10`. A run is noisy when the spread (max-min)/min exceeds 5%; a noisy run was repeated once, and if still noisy the item stays unmeasured. Probe benchmarks were kept in `/tmp` and attached with `go test -overlay`, so no repo file changed. The `/tmp` scratch was removed afterwards, so the probe source is not kept; the probe shapes are described under each item so they can be rebuilt.

## Summary

| id | hypothesis | command | verdict | key result |
|---|---|---|---|---|
| M1 | `read` reads the whole file before slicing | existing `BenchmarkNativeReadLargeFile` | confirmed | 16 MB 32.5 ms, 64 MB 128.6 ms; `B/op` equals file size |
| M2 | fetch `Create` scans the whole store | probe `BenchmarkP7AFetchStoreCreate` and `BenchmarkP7AFetchDirectoryBytes` | confirmed | directory scan is 8.67 of 8.88 ms at 5000 docs |
| M3 | fetch page re-converts the stored document per cursor page | probe `BenchmarkP7AFetchStorePage` | confirmed (scaling) | 1 MB 10.3 ms, 8 MB 73.5 ms; the 3x factor not separated |
| M4 | grep post-filter per matched file | probe `BenchmarkP7AGrepPostFilter` | confirmed (scaling) | about 5.4 us per matched file end to end; share not isolated |
| M5 | write/edit reads the target to decide unchanged | probe `BenchmarkP7AWriteUnchangedSize` | noisy, unmeasured | 1 MiB cell 9.4% then 10.6% spread |
| M6 | spill planning lists the directory per call | existing `BenchmarkPlanBashSpillLargeDirectory` | confirmed at interval 0 only | interval 0: 16.8 ms per call; 3600 s: 23 us |
| M7 | ripgrep UTF-16 per match line | probe `BenchmarkP7ARipgrepLongLines` | confirmed (small) | 20,000-char line +5% time, +203 KB/op |
| M8 | directory listing reads every entry before the limit | probe `BenchmarkP7ARenderDirectory` | confirmed | cost scales with entries at fixed limit 100: 0.47 ms to 12.6 ms |
| M9 | glob post-filter policy check per listed file | probe `BenchmarkP7AGlobFileCount` | confirmed (scaling) | about 2.0 us per listed file end to end; share not isolated |
| M10 | fetch HTML path copies the body three times | probe `BenchmarkP7AFetchHTMLSize` | confirmed (scaling) | 100 KiB 3.5 ms, 1 MiB 30.0 ms; new lead on UTF-16 allocation |
| M11 | `read_image` stored reference reads the whole object | probe `BenchmarkP7AStoredImageSize` | noisy, unmeasured | 4 MiB cell 9.5% then 63.8% spread; `B/op` equals image size |

Verdict counts (11 items): confirmed 9, not confirmed 0, noisy (unmeasured) 2 (M5, M11).

Mediums confirmed by reproduction: not applicable here; this step measures cost, not correctness. Mediums and performance items are reported in `p7a_merged.md`.

## Hypothesis mapping

Each reviewer hypothesis maps to one item. r2 H01 to M3, r2 H02 to M2, r2 H03 to M4, r2 H04 to M5, r2 H05 to M6. r3 H01 to M7, r3 H02 to M8, r3 H03 to M9, r3 H04 to M10, r3 H05 to M6. r1 H01 and r4 H01 map to M1 and M8, and M11 comes from r2 and r4. All of H01 to H05 are covered.

## Per-item results

### M1 (r1 H01): `read` reads the whole file before slicing

- Command: `go test ./internal/tools/native/ -run '^$' -bench 'BenchmarkNativeReadLargeFile$' -benchmem -count=10` (existing benchmark, 50-line range from 1, 16 and 64 MB files).
- Load: before 2.47 0.94 0.57, after 2.52 1.15 0.66.
- Numbers: 16 MB 32.46 ms mean, spread 1.8%, 16.84 MB/op. 64 MB 128.63 ms mean, spread 0.9%, 67.18 MB/op. Time and allocation scale with file size (4x the file gives 3.96x the time).
- 1 MB first run: spread 19.4%, so noisy. Repeat (load before 2.02 1.82 1.18, after 1.71 1.76 1.17): 2.20 ms mean, spread 2.4%, 1.11 MB/op. The repeat is clean, so the 1 MB cell is valid.
- Verdict: **confirmed**. The 50-line read allocates and reads the whole file.

### M2 (r1 H02, r2 H02): fetch `Create` scans the whole store

- Commands: `go test -overlay <probe-overlay> ./internal/tools/native/ -run '^$' -bench 'BenchmarkP7AFetchStoreCreate$' -benchmem -count=10` and the same for `BenchmarkP7AFetchDirectoryBytes$`. The first builds the store with 100, 1000 and 5000 stored `.md` files and calls `Create`, which removes its own output after each iteration. The second calls `fetchDirectoryBytes` alone over 100 and 5000 files.
- Load: `Create` before 1.24 1.10 0.70, after 2.01 1.44 0.84. Directory scan before 1.38 1.51 1.19, after 1.27 1.46 1.18.
- Numbers, `Create`: 100 docs 0.214 ms (spread 1.1%); 1000 docs 1.649 ms (1.2%); 5000 docs 8.88 ms (2.4%), 2.49 MB/op. Directory scan alone: 100 docs 0.160 ms (0.8%); 5000 docs 8.67 ms (1.5%), 2.49 MB/op.
- Verdict: **confirmed**. The directory scan is about 98% of `Create` at 5000 documents, and `Create` cost grows linearly with stored count.

### M3 (r2 H01): fetch page re-converts the stored document per cursor page

- Command: `go test -overlay <probe-overlay> ./internal/tools/native/ -run '^$' -bench 'BenchmarkP7AFetchStorePage$' -benchmem -count=10`. The probe stores a 1 MB or 8 MB document with `CreatePage`, then times `Page` on the next cursor, one page of 1000 characters.
- Load: before 2.01 1.44 0.84, after 1.66 1.40 0.85.
- Numbers: 1 MB 10.28 ms (spread 4.0%), 21.0 MB/op. 8 MB 73.50 ms (3.2%), 167.8 MB/op. Allocations per op are constant (50), so the cost is the whole-document copy, not the allocation count.
- Verdict: **confirmed (scaling)**. Cost per page grows linearly with stored size. The "three conversions" multiplicity is not separated by this benchmark.

### M4 (r2 H03): grep post-filter per matched file

- Command: `go test -overlay <probe-overlay> ./internal/tools/native/ -run '^$' -bench 'BenchmarkP7AGrepPostFilter$' -benchmem -count=10`. The probe writes 10 or 1000 files that each match, then runs `executeNativeGrep` end to end.
- Load: before 1.16 1.17 0.84, after 1.67 1.29 0.90.
- Numbers: 10 files 1.87 ms (spread 1.1%), 59.5 KB/op, 627 allocs. 1000 files 7.14 ms (1.5%), 775.9 KB/op, 9800 allocs. Marginal cost about 5.4 us per matched file end to end.
- Profile (`-cpuprofile`, 1000-file run, load 0.81 1.26 1.14 to 0.90 1.27 1.14, exit 0): the top entries are syscalls (25.5% flat), `filepath.Clean` (22.8% cumulative) and `filepath.Rel` (10.3% cumulative). The profile does not show ripgrep's own CPU (external process) and does not isolate the `resolveExistingTarget` share from the other Go-side work.
- Verdict: **confirmed (scaling)**. The per-file cost is real. How much of it is the post-filter specifically is not isolated.

### M5 (r2 H04): write/edit reads the whole target to decide unchanged

- Command: `go test -overlay <probe-overlay> ./internal/tools/native/ -run '^$' -bench 'BenchmarkP7AWriteUnchangedSize$' -benchmem -count=10`. The probe writes identical content over an existing file of 64 KiB and 1 MiB, so the unchanged path runs every iteration.
- Load: first run before 1.67 1.29 0.90, after 2.46 1.52 0.99. Repeat before 3.52 1.94 1.17, after 2.82 1.95 1.20.
- Numbers: 64 KiB 97.9 us (spread 4.4%) and repeat 96.9 us (4.8%). 1 MiB 840 us (9.4%) and repeat 870 us (10.6%).
- Existing `BenchmarkNativeEditWithBackupOn128KiBFile` (434 us, spread 1.3%, 580 KB/op) was run as context. It alternates a real change each iteration, so it does not exercise the unchanged path and is not used for the verdict.
- Verdict: **noisy, unmeasured** (repeat still above 5%).

### M6 (r2 H05, r3 H05): spill planning lists the directory per call

- Command: `go test ./internal/tools/native/ -run '^$' -bench 'BenchmarkPlanBashSpillLargeDirectory$' -benchmem -count=10` (existing; 3000 spill files, `interval_0s` and `interval_3600s`).
- Load: before 2.13 1.11 0.65, after 1.60 1.08 0.65.
- Numbers: interval 0 16.84 ms per call (spread 3.4%), 4.14 MB/op, 36,277 allocs. Interval 3600 23.3 us per call (spread 2.8%), 24.1 KB/op, 229 allocs, about 720x less.
- Verdict: **confirmed at interval 0 only**. The benchmark does not exercise the "interval has elapsed" case, so the default-interval cost is not measured. The 23 us result shows the default is not a per-call listing cost.

### M7 (r3 H01): ripgrep UTF-16 per match line

- Command: `go test -overlay <probe-overlay> ./internal/tools/native/ -run '^$' -bench 'BenchmarkP7ARipgrepLongLines$' -benchmem -count=10`. One file with one matching line, 200 or 20,000 characters, through `runRipgrep`. The probe uses ASCII characters, so the UTF-16 unit count equals the character count.
- Load: before 0.82 1.11 0.81, after 1.18 1.17 0.84.
- Numbers: 200 chars 1.569 ms (spread 1.5%), 20.3 KB/op. 20,000 chars 1.648 ms (1.4%), 223.7 KB/op. +5% time and +203 KB/op.
- Existing `BenchmarkRunRipgrep` (short lines) at 1.58 ms (spread 1.3%) is context.
- Verdict: **confirmed, small**. The per-line cost is real but about 5% at 20,000 characters.

### M8 (r3 H02, r4 H01): directory listing reads every entry before the limit

- Command: `go test -overlay <probe-overlay> ./internal/tools/native/ -run '^$' -bench 'BenchmarkP7ARenderDirectory$' -benchmem -count=10`. The probe calls `renderDirectory` with limit 100 on a directory of 100 or 10,000 regular files.
- Load: before 0.72 1.13 0.81, after 0.82 1.11 0.81.
- Numbers: 100 entries 0.467 ms (spread 1.7%), 175 KB/op. 10,000 entries 12.64 ms (2.6%), 2.59 MB/op. Limit is fixed, so the cost grows with the directory size, not the limit.
- Verdict: **confirmed**.

### M9 (r3 H03): glob post-filter policy check per listed file

- Command: `go test -overlay <probe-overlay> ./internal/tools/native/ -run '^$' -bench 'BenchmarkP7AGlobFileCount$' -benchmem -count=10`. The probe runs `executeNativeGlob` over 128 or 1024 files.
- Load: before 0.84 1.44 1.16, after 1.38 1.51 1.19.
- Numbers: 128 files 2.170 ms (spread 1.1%), 165 KB/op. 1024 files 4.00 ms (1.3%), 538 KB/op. About 2.0 us per listed file end to end. The existing `BenchmarkExecuteNativeGlob` (128 files) gives 2.148 ms (spread 1.4%), consistent with the probe.
- Verdict: **confirmed (scaling)**. The policy-check share was not isolated (no profile taken).

### M10 (r3 H04): fetch HTML path copies the body three times

- Command: `go test -overlay <probe-overlay> ./internal/tools/native/ -run '^$' -bench 'BenchmarkP7AFetchHTMLSize$' -benchmem -count=10`. The probe runs the real fetch tool against an HTML body of 100 KiB or 1024 KiB through a fake requester, with a new store each iteration.
- Load: before 2.51 1.54 1.00, after 2.13 1.53 1.01.
- Numbers: 100 KiB 3.54 ms (spread 4.8%), 3.13 MB/op, 998 allocs. 1024 KiB 30.04 ms (3.5%), 30.0 MB/op, 1029 allocs.
- Profile (`-memprofile`, 1 MiB body, `-memprofilerate=1`, exit 0): `utf16Length` 41% cumulative, `unicode/utf16.Encode` 20% flat, `sliceFetchCharacters` 20% cumulative, `retrieveFetchDocument` 55% cumulative. The UTF-16 length and encode work accounts for most of the allocated bytes, which is more than the three copies in the hypothesis.
- Verdict: **confirmed (scaling)**. The multiplicity stated in the hypothesis is not separated. The UTF-16 allocation share is a new lead, listed below.

### M11 (r2 P7A-10, r4 P7A-06): `read_image` stored reference reads the whole object

- Command: `go test -overlay <probe-overlay> ./internal/tools/native/ -run '^$' -bench 'BenchmarkP7AStoredImageSize$' -benchmem -count=10`. The probe stores a noise PNG of 64 KiB or 4 MiB with `imagestore.Externalize`, then calls `readImageFile` on the stored reference. The first attempt failed with exit 1 because the probe used a non-image payload that the store rejects; it was fixed to a real PNG, and the rerun exited 0.
- Load: first run before 1.66 1.45 0.99, after 3.52 1.94 1.17. Repeat before 1.71 1.76 1.17, after 2.44 1.93 1.25.
- Numbers: first run 64 KiB 18.6 us (spread 9.6%), 4 MiB 385 us (9.5%). Repeat 64 KiB 18.9 us (6.8%), 4 MiB 645 us (63.8%).
- Allocation (not time): `B/op` is 58 KB and 3.15 MB, equal to the image size, which is consistent with a whole-object read. This is evidence about allocation only.
- Verdict: **noisy, unmeasured** (repeat still above 5%).

## Verification gates

| command | exit code | result |
|---|---|---|
| `go vet -overlay <overlay>` (fetch probe) | 0 | passed on the first attempt |
| `go vet -overlay <overlay2>` (second probe) | 1, then 0 | first attempt exit 1 because the overlay JSON was not written yet ("no such file"), then exit 1 from a `*testing.B` argument to `executeFetchTestTool` (`*testing.T`); fixed, rerun exit 0 |
| `go test -bench` for M1, M6, M9, M7, M5, M8, M4, M10, M2, M3, M11 (and repeats) | 0 | M11 first attempt exit 1 (setup, non-image payload), fixed, rerun exit 0 |
| `go test -race -count=1 ./internal/tools/native/ ./internal/imagestore/` | 0 | `ok` for both (108.7 s and 1.2 s) |
| `go test -cpuprofile` and `-memprofile` (grep and fetch profiles) | 0 | profiles used as evidence only |

The race run used the repo tree without overlays. No `go test ./...` and no `go build ./...` was run.

## Corrections and disclosures

- My first summary read the edit benchmark `B/op` as 298 because it has an extra `MB/s` column. The correct value is 580,043 B/op (spread 1.3%, rerun as `E_edit`). The 298 figure is withdrawn.
- Probe names collided with existing benchmark outputs (`M5.out`, `M7.out`, `M9.out`). The existing benchmarks were rerun under unique names (`E_edit`, `E_glob`, `E_rg`), and all reported numbers come from the unique-named runs.
- The M4 per-file cost was first written as 6.4 us; the corrected marginal from the two measured sizes is about 5.4 us.

## New leads (not measured as hypotheses)

- M10 allocation is dominated by UTF-16 length and encode work in the fetch path, more than the three copies in r3 H04. Worth a targeted check on the fetch and page paths before any fix.
- M4 and M9 both show a per-file cost that is real but not isolated to the policy check. A profile with call-graph attribution would settle it.

## Limits

- Machine shared and not quiet. Load peaked at 3.52 (1 minute) during M5 and M11, which is why those two are noisy.
- Probe source was removed with `/tmp`. Rebuilding requires the probe shapes described above.
- M6 is measured for interval 0 and 3600 only; the "interval has elapsed" case is not exercised.
- No absolute claims; relative comparisons only.
