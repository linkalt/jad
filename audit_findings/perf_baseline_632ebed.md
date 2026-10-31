# Performance baseline at HEAD 632ebed

Purpose: a reference for later comparison. This is not the section 6.6 final pass (`audit_findings/final_perf.md` stays as the blocker record). `audit_findings/baseline.md` is unchanged.

Method: from `backend-go/`, one command at a time, each `go test <pkg> -run '^$' -bench '^<Name>$' -benchmem -count=10`. Nothing else was built or tested by the coordinator during the runs. No `-race`, no profiles. Medians are over the 10 counts. Spread = (max - min) / median. Rule: spread above 5% means repeat once; if still above 5%, mark noisy.

Machine: 12 CPUs. Firefox (`firefox-bin` 28.6% CPU, `Isolated Web Co` 23.4%) was running at the start of the block and was not stopped. It was not re-checked during the runs. Load stayed between 0.66 and 2.69 (1-min) across the block. Per the rule, proceeded because load stayed low.

## Paths (first run, n=10 each)

| Path | Benchmark | Exit | Median ns/op | B/op | allocs/op | Spread | Repeat (median, spread) | Load 1-min before -> after | Verdict |
|---|---|---|---|---|---|---|---|---|---|
| Session list | `BenchmarkListSessions/conditional/4-sessions` | 0 | 163,958 | 49,644 | 323 | 1.48% | - | 1.19 -> 1.86 | stable |
| Session list | `BenchmarkListSessions/conditional/20-sessions` | 0 | 825,876 | 324,168 | 1,556 | 3.29% | - | 1.19 -> 1.86 | stable |
| Session list | `BenchmarkListSessions/conditional/200-sessions` | 0 | 8,110,929 | 2,942,464 | 15,262 | 9.38% | 8,341,013 (1.32%) | 1.19 -> 1.86; repeat 2.16 -> 2.08 | stable after repeat |
| Session list | `BenchmarkListSessions/unconditional/4-sessions` | 0 | 518,654 | 251,308 | 740 | 1.90% | - | 1.19 -> 1.86 | stable |
| Session list | `BenchmarkListSessions/unconditional/20-sessions` | 0 | 2,582,794 | 1,388,835 | 3,602 | 2.31% | - | 1.19 -> 1.86 | stable |
| Session list | `BenchmarkListSessions/unconditional/200-sessions` | 0 | 25,247,368 | 13,547,424 | 35,371 | 8.28% | 25,713,518 (3.00%) | 1.19 -> 1.86; repeat 2.16 -> 2.08 | stable after repeat |
| Journal append | `BenchmarkJournalAppend` | 0 | 11,817 | 688 | 7 | 1.20% | - | 1.86 -> 1.67 | stable |
| Read input items | `BenchmarkReadInputItems1000Records` | 0 | 2,315,087 | 1,769,226 | 28,019 | 1.88% | - | 1.67 -> 1.52 | stable |
| Compaction | `BenchmarkCompactHistory1000Turns` | 0 | 11,881,560 | 7,946,562 | 119,472 | 2.03% | - | 1.52 -> 1.44 | stable |
| Run rounds history | `BenchmarkRunRoundsHistory/full/100-turns` | 0 | 21,224,356 | 13,491,016 | 64,014 | 2.00% | - | 1.44 -> 1.73 | stable |
| Run rounds history | `BenchmarkRunRoundsHistory/full/1000-turns` | 0 | 215,005,898 | 133,990,729 | 631,327 | 3.29% | - | 1.44 -> 1.73 | stable |
| Run rounds history | `BenchmarkRunRoundsHistory/full/5000-turns` | 0 | 1,110,583,734 | 671,013,544 | 3,151,760 | 3.36% | - | 1.44 -> 1.73 | stable |
| Run rounds history | `BenchmarkRunRoundsHistory/memo/100-turns` | 0 | 3,196,710 | 2,152,560 | 6,757 | 2.64% | - | 1.44 -> 1.73 | stable |
| Run rounds history | `BenchmarkRunRoundsHistory/memo/1000-turns` | 0 | 23,092,990 | 15,117,326 | 63,490 | 2.01% | - | 1.44 -> 1.73 | stable |
| Run rounds history | `BenchmarkRunRoundsHistory/memo/5000-turns` | 0 | 113,051,077 | 72,948,484 | 315,535 | 2.75% | - | 1.44 -> 1.73 | stable |
| httpapi fan-out | `BenchmarkPublishFanOut/1-subscribers` | 0 | 1,456 | 341 | 12 | 1.24% | - | 1.73 -> 2.45 | stable |
| httpapi fan-out | `BenchmarkPublishFanOut/10-subscribers` | 0 | 3,124 | 999 | 15 | 7.62% | 3,154 (3.62%) | 1.73 -> 2.45; repeat 0.74 -> 2.69 | stable after repeat |
| httpapi fan-out | `BenchmarkPublishFanOut/100-subscribers` | 0 | 20,474 | 7,296 | 79 | 5.32% | 20,991 (8.03%) | 1.73 -> 2.45; repeat 0.74 -> 2.69 | noisy |
| Async index open | `BenchmarkOpenFromIndex` | 0 | 22,971,324 | 13,827,440 | 20,234 | 3.81% | - | 2.45 -> 2.25 | stable |
| Session lock waiters | `BenchmarkAcquireWaiters/1-waiters` | 0 | 17,549,986 | 58 | 0 | 38.66% | 17,824,368 (43.15%) | 2.25 -> 0.66; repeat 1.06 -> 0.74 | noisy |
| Session lock waiters | `BenchmarkAcquireWaiters/4-waiters` | 0 | 23,817,586 | 66 | 0 | 15.95% | 25,402,266 (19.35%) | 2.25 -> 0.66; repeat 1.06 -> 0.74 | noisy |
| Session lock waiters | `BenchmarkAcquireWaiters/16-waiters` | 0 | 46,832,054 | 152 | 0 | 15.20% | 48,390,498 (18.80%) | 2.25 -> 0.66; repeat 1.06 -> 0.74 | noisy |
| Native read large file | `BenchmarkNativeReadLargeFile/1MB` | 0 | 2,210,661 | 1,105,899 | 282 | 1.58% | - | 0.66 -> 1.06 | stable |
| Native read large file | `BenchmarkNativeReadLargeFile/16MB` | 0 | 32,522,924 | 16,839,286 | 292 | 0.86% | - | 0.66 -> 1.06 | stable |
| Native read large file | `BenchmarkNativeReadLargeFile/64MB` | 0 | 128,920,827 | 67,176,640 | 302 | 1.18% | - | 0.66 -> 1.06 | stable |

Path count: 9 representative paths (one per hot path in `audit_plan.md` section 2), 28 benchmark rows.

Note on the stable-after-repeat rows: the first-run spread was above 5%, the repeat was at or below 5%. The first-run median is the value in the table, and the repeat is shown beside it. The later comparison should use the repeat, since it was the steadier of the two.

## Noisy items

- `BenchmarkAcquireWaiters` (all three sizes): spread 15-39% on the first run, 19-43% on the repeat. The machine load also dropped from 2.25 to 0.66 across the first block. Treat the absolute value as not reliable until measured on a quieter machine.
- `BenchmarkPublishFanOut/100-subscribers`: spread 5.32% on the first run and 8.03% on the repeat. Still above 5%.

## Commands and exit codes

First run (one command each, from `backend-go/`, all exit 0):
- `go test ./internal/app/ -run '^$' -bench '^BenchmarkListSessions$' -benchmem -count=10` : 0
- `go test ./internal/transcript/ -run '^$' -bench '^BenchmarkJournalAppend$' -benchmem -count=10` : 0
- `go test ./internal/transcript/ -run '^$' -bench '^BenchmarkReadInputItems1000Records$' -benchmem -count=10` : 0
- `go test ./internal/transcript/ -run '^$' -bench '^BenchmarkCompactHistory1000Turns$' -benchmem -count=10` : 0
- `go test ./internal/transcript/ -run '^$' -bench '^BenchmarkRunRoundsHistory$' -benchmem -count=10` : 0
- `go test ./internal/httpapi/ -run '^$' -bench '^BenchmarkPublishFanOut$' -benchmem -count=10` : 0
- `go test ./internal/asyncmgr/ -run '^$' -bench '^BenchmarkOpenFromIndex$' -benchmem -count=10` : 0
- `go test ./internal/sessionlock/ -run '^$' -bench '^BenchmarkAcquireWaiters$' -benchmem -count=10` : 0
- `go test ./internal/tools/native/ -run '^$' -bench '^BenchmarkNativeReadLargeFile$' -benchmem -count=10` : 0

Repeats (one each, same command form, exit 0):
- `BenchmarkAcquireWaiters` in `./internal/sessionlock/` : 0
- `BenchmarkPublishFanOut` in `./internal/httpapi/` : 0
- `BenchmarkListSessions` in `./internal/app/` : 0

No benchmark failed to run. All 12 `go test` invocations exited 0 and each reported `PASS` and `ok`.

## Scope and limits

- Name check: all nine benchmark names were confirmed with `grep -rl "func <Name>("` before the runs.
- Only the nine representative paths were run. The other candidates in `audit_plan.md` section 2 (`BenchmarkBuildRequestWithLargeHistory`, `BenchmarkLoadToolsServers`, `BenchmarkCommitLargeTranscript`, `BenchmarkSSEDeltaEventEncoding`, and the rest) were not run and are not in this baseline.
- Raw `go test` output was kept in `/tmp` and removed after parsing, as the rules require. The medians above are the record.
- This baseline does not cover the 6.6 regression pass. That still needs a baseline at the chosen commit and the post-fix state.
