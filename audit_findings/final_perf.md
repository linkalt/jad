# Final performance regression pass (audit_plan.md section 6.6)

Earlier pass blocked: `baseline.md` had no numbers and no fixes had landed. This is the pass run against the 632ebed baseline in `perf_baseline_632ebed.md`, at HEAD 9800d7b. The original section 2 baseline in `baseline.md` still has no numbers, so this is not the baseline the plan asked for.

## Method, verdict rule, load

From `backend-go/`, one `go test <pkg> -run "^$" -bench "^<Name>$" -benchmem -count=10` at a time, serialized. Medians over 10 counts. Spread = (max - min) / median.

Verdict rule: the final run's spread decides. If it is above 5% the row is **noisy**. Otherwise it is **moved** when the first run and the repeat both change by more than 5% in the same direction, and **same** when they do not. Every row that was noisy or moved on its first run was repeated, as the rule requires. Journal append was within noise on the first run and was not repeated.

Load: `uptime` before the first run was 0.48 0.87 1.23. It rose to about 2.5 during the first run. Firefox (`firefox-bin` 28%, `Isolated Web Co` 22%) was running throughout. During the repeat, Chrome also appeared at about 30% CPU, and load reached about 3. Nothing was killed. The two RunRounds controls ran at load 1.0-1.2 (see below).

Row count: the baseline document states 28 rows, but its table lists 25 benchmark rows. The 25 are the ones compared here.

## Verdict counts (25 rows)

same: 8. moved: 6. noisy: 11.

## Results

Chg is the first-run median against baseline. Repeat is the second run, with its own spread (the value that decides the verdict). B/op and allocs/op are shown as base/HEAD from the first run.

| Path | Row | Base ns/op | HEAD ns/op | Chg | Spread | Repeat ns/op | Repeat chg | Repeat spread | B/op base/HEAD | allocs base/HEAD | Verdict |
|---|---|---|---|---|---|---|---|---|---|---|---|
| Session list | conditional/4-sessions | 163958 | 164047 | +0.05% | 1.16% | 169278 | +3.24% | 5.24% | 49644/49648 | 323/323 | noisy |
| Session list | conditional/20-sessions | 825876 | 819359 | -0.79% | 2.41% | 835078 | +1.11% | 2.66% | 324168/324160 | 1556/1556 | same |
| Session list | conditional/200-sessions | 8341013 | 8334484 | -0.08% | 12.72% | 8815481 | +5.69% | 6.49% | 2942464/2942074 | 15262/15262 | noisy |
| Session list | unconditional/4-sessions | 518654 | 515333 | -0.64% | 1.97% | 537284 | +3.59% | 4.88% | 251308/251191 | 740/740 | same |
| Session list | unconditional/20-sessions | 2582794 | 2446434 | -5.28% | 7.64% | 2692771 | +4.26% | 4.47% | 1388835/1389238 | 3602/3602 | same (changes opposite sign) |
| Session list | unconditional/200-sessions | 25713518 | 26101806 | +1.51% | 4.56% | 26722514 | +3.92% | 1.41% | 13547424/13547432 | 35371/35370 | same |
| Journal append | BenchmarkJournalAppend | 11817 | 11947 | +1.10% | 3.60% | not repeated | - | - | 688/688 | 7/7 | same |
| Read input items | 1000 records | 2315087 | 2324309 | +0.40% | 7.33% | 2455697 | +6.07% | 5.53% | 1769226/1769220 | 28019/28019 | noisy |
| Compaction | 1000 turns | 11881560 | 12489292 | +5.11% | 6.22% | 13225512 | +11.31% | 3.10% | 7946562/7946555 | 119472/119472 | moved |
| Run rounds history | full/100-turns | 21224356 | 26461768 | +24.68% | 6.19% | 27189001 | +28.10% | 7.25% | 13491016/13490909 | 64014/64013 | noisy |
| Run rounds history | full/1000-turns | 215005898 | 258490762 | +20.22% | 7.58% | 273230055 | +27.08% | 3.42% | 133990729/133990493 | 631327/631328 | moved |
| Run rounds history | full/5000-turns | 1110583734 | 1355153012 | +22.02% | 5.01% | 1406098366 | +26.61% | 2.24% | 671013544/671013696 | 3151760/3151750 | moved |
| Run rounds history | memo/100-turns | 3196710 | 3622670 | +13.32% | 6.65% | 3789578 | +18.55% | 2.36% | 2152560/2152567 | 6757/6757 | moved |
| Run rounds history | memo/1000-turns | 23092990 | 27488939 | +19.04% | 1.76% | 29301362 | +26.88% | 4.27% | 15117326/15117210 | 63490/63490 | moved |
| Run rounds history | memo/5000-turns | 113051077 | 136899051 | +21.09% | 6.88% | 140968209 | +24.69% | 3.04% | 72948484/72948213 | 315535/315534 | moved |
| httpapi fan-out | 1-subscribers | 1456 | 1427 | -1.99% | 1.40% | 1474 | +1.24% | 5.22% | 341/341 | 12/12 | noisy |
| httpapi fan-out | 10-subscribers | 3154 | 2964 | -6.02% | 5.87% | 3346 | +6.07% | 10.16% | 999/998 | 15/15 | noisy |
| httpapi fan-out | 100-subscribers | 20991 | 20487 | -2.40% | 8.22% | 21840 | +4.04% | 15.45% | 7296/7287 | 79/79 | noisy |
| Async index open | BenchmarkOpenFromIndex | 22971324 | 22611018 | -1.57% | 8.03% | 22844402 | -0.55% | 9.06% | 13827440/13787241 | 20234/20234 | noisy |
| Session lock waiters | 1-waiters | 17824368 | 17980854 | +0.88% | 47.77% | 16789063 | -5.81% | 27.61% | 58/63 | 0/0 | noisy |
| Session lock waiters | 4-waiters | 25402266 | 23243200 | -8.50% | 25.69% | 24592072 | -3.19% | 22.19% | 66/72 | 0/0 | noisy |
| Session lock waiters | 16-waiters | 48390498 | 47546918 | -1.74% | 11.06% | 46846942 | -3.19% | 9.88% | 152/148 | 0/0 | noisy |
| Native read large file | 1MB | 2210661 | 2210288 | -0.02% | 7.43% | 2124970 | -3.88% | 0.83% | 1105899/1105937 | 282/282 | same |
| Native read large file | 16MB | 32522924 | 33332290 | +2.49% | 5.19% | 31897732 | -1.92% | 2.09% | 16839286/16838838 | 292/291 | same (changes opposite sign) |
| Native read large file | 64MB | 128920827 | 131911191 | +2.32% | 1.44% | 124881290 | -3.13% | 1.41% | 67176640/67176689 | 302/303 | same (changes opposite sign) |

## Moved and noisy rows

Moved (6): Compaction 1000 turns (+5.1%, then +11.3%); RunRounds full/1000 (+20%, +27%), full/5000 (+22%, +27%), memo/100 (+13%, +19%), memo/1000 (+19%, +27%), memo/5000 (+21%, +25%).

Noisy (11): Session list conditional/4 and conditional/200, Read input items, RunRounds full/100 (repeat spread 7.25%; its direction and size match the other RunRounds rows), all three httpapi fan-out rows, OpenFromIndex, and the three session lock waiters rows.

## What the RunRounds move is, and is not

The moved RunRounds rows are real at HEAD relative to 632ebed. Control: the RunRounds 100 and 1000 turn rows were run at 632ebed (extracted with `git archive` to /tmp) and at HEAD, back to back, under load about 1.1. Two orderings gave the same result. Ordering 1: full/100 base 20.19 ms, HEAD 25.20 ms (+24.8%); memo/100 +15.4%; full/1000 +23.4%; memo/1000 +21.5%. Ordering 2: +24.2%, +15.3%, +23.2%, +22.2%. B/op and allocs/op are identical on both sides.

Cause: not isolated. The timed loop calls `NewInputHistory`, `ReadInputItems`, `history.Read`, `journal.Append` and `Release`. The benchmark uses `DurabilityProcess`, so the new directory sync (only for non-process durability) does not run there. `input_history.go` and the benchmark file are unchanged since 632ebed. The changed transcript code is in `jsonl.go` (`ReplaceRecords`), `front_compact.go`, `maintenance.go` (`backupTranscript`, `Reset`, `CompactHistory`, `writeCompactionJSONAtomic`) and `turn.go` (`writeJSONAtomic`). None of these is on the timed path I checked. The slowdown is real, but its source is unknown. It may be a build or environment difference, or a changed callee I did not trace.

Compaction (+5% first run, +11% repeat) is moved, but its cause is not isolated. `CompactHistory` changed (cache removal on the replace-failure path and a backup sync), so it may be related. I did not check whether the benchmark reaches those new lines.

## Limits

- Load was not idle. Firefox and Chrome were running, and load reached about 3 during the repeat. The RunRounds controls ran at lower load (about 1.1).
- Only the 9 representative paths were run, as in the 632ebed baseline. Other candidates in `audit_plan.md` section 2 were not run.
- This pass cannot show whether the RunRounds slowdown was caused by the fixes. The controls show HEAD and 632ebed differ, not which commit or environment factor causes it.
- No race, profile, or power-loss check was run, per the rules.

## RunRounds slowdown: placement, not a cost (cause established by an address table)

Status: cause identified as placement, not a cost; the cause is established by an address-controlled table, not a disassembly diff. Decider approved the rewrite.

Commits compared: 632ebed and 2ae8f35. Benchmark: `BenchmarkRunRoundsHistory` full and memo, 1000 turns. Method: `go test -run '^$' -bench ... -benchmem -count=5`, medians. Machine: Ryzen 5 3600, load about 1.0, go1.27.3. Allocations are equal in all rows.

| Build | `scanJSONValue` address mod 64 | full/1000 | memo/1000 |
|---|---|---|---|
| 632ebed | 0 | 215.2 ms | 22.9 ms |
| 632ebed + 32 B inert pad | 32 | 261.9 ms | 27.7 ms |
| 2ae8f35 | 32 | 259.4 ms | 27.5 ms |
| 2ae8f35 + 32 B pad | 0 | 212.2 ms | 22.8 ms |
| 2ae8f35 + 96 B pad | 0 | 209.2 ms | 22.6 ms |
| 2ae8f35 + 160 B pad (reviewer2's construction) | 0 | 212.7 ms | 23.0 ms |

- The pad is an inert, never-called function, added only in `/tmp` copies. A variable-only pad did not move the text.
- reviewer1 measured a 160 B pad at mod 32 with a different construction. The effective shift differs by construction. Timing still follows the measured mod.
- Earlier figures (632ebed 212 ms, 2ae8f35 260 ms on full/1000; 22.8 and 27.6 ms on memo/1000) are consistent with this table.

Verified:
- The timed read path changed in two places: `readTranscriptItemsMemo` (`input_history_memo.go`; a `ran` flag and memo reset on a missing transcript) and `withRepairedFile` (`jsonl.go`; `O_CREATE` dropped and an `errors.Is` check).
- `input_history.go` changed at 07678f5, append-time only (`AppendInputItemsAt`, plus `ValidateInputItem`).
- None of these adds per-call work on the timed path.

Correlation, not mechanism: time tracks the measured `scanJSONValue` address mod 64 across six builds, not source or pad size. Alignment of the hot loop is an inference, not isolated by a disassembly diff.

Not shown: whether the cost exists in the product binary. `go build ./cmd/jad` fails on a missing `-lhtml_to_markdown_ffi`, so product-binary impact is unmeasured.

Audit trail: reviewer1 deleted `/tmp/perfA` and `/tmp/perfB_*` with an over-broad glob (scratch only, no repo effect). Its report is `audit_findings/perf_runrounds_cause.md`. reviewer2's re-run is in its transcript.

Decision: accepted cost. No code change.

Revisit triggers:
- Any change to `scanJSONValue` or `jsoncompat.go`.
- Any change to the `internal/transcript` package build layout.
- A PERF budget on `ReadInputItems`.
- A same-binary A/B check (old and new code in one binary, or both paths in one benchmark process, so placement is shared), at about 25% tolerance on medians at fixed machine and load. That tolerance would NOT catch another ~22% placement shift, so placement effects are not ruled out by it.
