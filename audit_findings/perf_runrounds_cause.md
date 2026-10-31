# RunRounds slowdown: the cause, measured (review of `final_perf.md` "accepted cost")

- reviewer: perf-review (independent re-measurement)
- HEAD: `2ae8f35`; baseline: `632ebed`
- subject: `final_perf.md`, section "RunRounds slowdown: accepted cost (no code change)"
- method: `git archive` copies under `/tmp/review_perf/{old,new}`, `go test -c`, `go tool nm`,
  `go tool objdump`, `go test -run '^$' -bench '^BenchmarkRunRoundsHistory$/(full|memo)/1000-turns'
  -benchmem -count=5`, plus inert-padding layout perturbations in the `/tmp` copies (never in the tree).
- CPU profiles were **not** taken in this pass (my reviewer rules forbid profiles); the record's profile
  evidence is accepted as-is, and the case below does not depend on it.

## Verdict

**The conclusion "accept, no code change" is right for the wrong reason, and must be rewritten.**
The +21% is *not* a cost of the commit's work and *not* unknown. It is a linked-code placement effect,
measured here: identical instruction bytes, and only 32 bytes of inert padding ahead of the package's text
flips the benchmark between ~213 ms and ~262 ms. The number at HEAD is a property of *where*
`internal/transcript`'s jsoncompat code landed in the test binary, not of any extra work on the timed path.

Severity of the record defect: medium (a PERF entry that says "unknown cost, accepted" where the
measurement does not support a cost at all, and where the same claim would block or misattribute a
future optimisation).

## The controlled experiment (primary evidence)

Two commits, six builds. The only difference between builds within a commit is an inert
`//go:noinline func padLayout(p []byte) int` added in a file (`aaa_pad.go`) that sorts before
`jsoncompat.go`, kept alive by one unused test. It touches nothing on the timed path and adds no
allocation. `full/1000-turns` and `memo/1000-turns`, medians of `-count=5`, one machine, back to back,
load about 0.6:

| build | scanJSONValue / objectFieldsValid / canonicalJSONValid address (mod 64) | full/1000 | memo/1000 | allocs/op |
|---|---|---|---|---|
| old `632ebed` | 0x613140/**0**  0x612360/**32**  0x6135e0/**32** | **215.5 ms** | **23.25 ms** | 631316 / 63490 |
| old + 32 B pad | 0x613160/**32**  0x612380/**0**  0x613600/**0** | **263.9 ms** | **27.93 ms** | 631314 / 63489 |
| new `2ae8f35` | 0x613920/**32**  0x612b40/**0**  0x613dc0/**0** | **261.1 ms** | **27.76 ms** | 631316 / 63489 |
| new + 32 B pad | 0x613940/**0**  0x612b60/**32**  0x613de0/**32** | **212.6 ms** | **23.42 ms** | 631311 / 63490 |
| new + 96 B pad | 0x613980/**0**  0x612ba0/**32**  0x613e20/**32** | **213.6 ms** | **22.77 ms** | 631314 / 63489 |
| new + 160 B pad | 0x6139a0/**32**  0x612bc0/**0**  0x613e40/**0** | **264.0 ms** | **27.75 ms** | 631317 / 63489 |

Facts this establishes:

- The regression reproduces (215.5 -> 261.1 ms full, +21%; 23.25 -> 27.76 ms memo, +19%), matching the
  record's and the lead's numbers. Allocations and B/op are identical throughout.
- Within HEAD, adding 32 B (or 96 B) of code that never runs removes the whole regression: 261.1 -> 212.6 ms.
  Within 632ebed, adding 32 B of the same inert code *creates* the regression: 215.5 -> 263.9 ms.
- The only measured variable separating the fast from the slow builds is the 32-byte offset of the
  `jsoncompat.go` text block (`scanJSONValue`, `objectFieldsValid`, `canonicalJSONValid` move together).
  Fast builds: block at mod-64 residue 0 for `scanJSONValue`; slow builds: residue 32. There is no
  exception in six builds, and no other correlated change (same source, same allocator behaviour,
  same bench fixture, same run order).
- Therefore the cause of the moved rows is code placement, not added work. This closes the record's
  "not verified: the cause" and replaces the earlier "layout artefact is an inference only".

Secondary supporting fact: the `jsoncompat.go` block shifted by exactly 0x7e0 (2016 B) between the
commits, and every function in that file shifted by the same amount, i.e. nothing *inside* the file
changed, only what precedes it in the package's text. The net 2016 B comes from real code added in
earlier-sorting files (`ValidateInputItem` 969 B, `(*TurnStore).removeCompactionCaches` 687 B, growth of
other bodies). Only the *displacement* matters; those functions are not on the timed path.

## The record's three sub-claims, checked

1. "The timed path is byte-identical between the two commits." **False as written, immaterial in effect.**
   The memo read path changed: `ReadInputItems`/`InputHistory.Read` -> `readInputItems` ->
   `readInputItemsWithCache` -> `readTranscriptItemsMemo`, and `input_history_memo.go` gained
   `ran := false` / `ran = true` plus a post-call `*memo = transcriptMemo{}` on the missing-file path.
   `readTranscriptItemsMemo` is on the memo timed path (verified: `input_history_memo.go:65-116`).
   Both read paths also changed in `jsonl.go` `withRepairedFile` (jsonl.go:295-319): `O_CREATE` dropped
   and an `errors.Is(err, os.ErrNotExist)` early return added. On an existing transcript (the benchmark's
   case) that is one extra `errors.Is` on a nil error and no changed syscall: no measurable work.
   The record also states "`input_history.go` ... unchanged since 632ebed". **That is wrong**:
   `input_history.go` changed in `07678f5` (`AppendInputItemsAt` refactored onto the new exported
   `ValidateInputItem`). It is append-time only, and the benchmark appends through `journal.Append`, so it
   is off the timed path, but the statement is not true and should not be relied on later.
   The benchmark file itself *is* unchanged.

2. "Extra work in setup that the timer includes / changed fixture size." **Ruled out.** The only setup the
   timer includes is `store.NewInputHistory()` (`input_history_memo.go:30-32`, returns a one-field struct)
   and `history.Release()` (`:43-48`, clears two fields); both are unchanged and allocation-free.
   `journal.size()` and the `os.Truncate` reset are inside `b.StopTimer()`/`b.StartTimer()`.
   The fixture is built in-code from constants (`text = strings.Repeat("x",1500)`, 2 records per turn,
   `toolRounds = 10`) and the benchmark asserts `len(items) >= 2*turns` each round; the identical
   allocs/op (631k, 63.5k) and B/op across all six builds confirm the same item count and sizes.
   There is no on-disk fixture. And the padding control changes no setup at all, yet moves the result by
   21% both ways, which settles the question on its own.

3. "Is a controlled run needed to separate placement from real cost?" **Yes, and it was missing; it is now
   done** (table above). The earlier "moved-package control that did not reproduce a sign flip" was not a
   placement control: moving the benchmark file re-orders test code, not the `internal/transcript` text
   block that contains the hot loop, so it cannot flip the sign. Inserting inert bytes in the package's
   text ordering does.

## What is inference, not measurement

- Attribution to 64-byte alignment of the hot loop specifically, rather than to some other consequence of
  the same shift (iTLB/uop-cache set conflicts of the loop, alignment of `objectFieldsValid` and
  `canonicalJSONValid` which moved with it): the residue flip is the only correlated variable in six builds,
  but three hot functions move together, so the measurement cannot single out one. The reportable level is
  "the placement of the `jsoncompat.go` text block", not "misalignment of `scanJSONValue` alone".
- Whether the *product* binary (`cmd/jad`) is now in the slow or the fast state is **unknown and not
  measurable here**: `go build ./cmd/jad` fails in this container (`/usr/bin/ld: cannot find
  -lhtml_to_markdown_ffi`), so no product link was produced. The 0x7e0 displacement is a property of the
  test build; a product link interleaves other packages and would put the block at yet another residue.
  This is why the honest statement is "not a cost of this commit", not "the product got faster".
- The naive reading "aligned loop start is better, so residue 0 should be fast" does not hold: in the fast
  builds the loop head sits at residue 32 of a 64-byte line, and in the slow builds at residue 0. I did not
  establish the microarchitectural mechanism, and it is not needed for the verdict.

## Recommendation

- **Do not change code for this.** No commit-level cost exists to fix; a "fix" would chase an addressing
  accident.
- **Rewrite the accepted-cost entry** as: cause found, not a cost. Suggested wording: "The +21% move is a
  linked-code placement artefact of the `internal/transcript` text block: identical instruction bytes, and
  32 bytes of inert padding in the package's text order flips the benchmark between 213 ms and 264 ms in
  either commit direction (six builds, `-count=5`). No code change. The commit's own diff adds no work to
  the timed path."
- **Fix the two false statements** in the same section ("the timed path is byte-identical";
  "`input_history.go` unchanged") so a later pass does not reason from them.
- **Process follow-up (the durable point):** `BenchmarkRunRoundsHistory` has about +/-20% run-to-run
  comparability across commits from text placement alone. Any PERF budget on `ReadInputItems` or on the
  RunRounds rows needs either a tolerance of about 25% or a same-binary A/B (both variants in one test
  binary / one process), otherwise later passes will keep re-finding and re-accepting this phantom.
  The revisit trigger "any change to the `internal/transcript` package build layout" should be replaced by
  the same-binary rule, because that trigger fires on every commit and cannot be acted on.

## Not covered

- Product-binary impact (link fails here, see above).
- CPU profiles / mechanism (forbidden for this reviewer; delivered fact does not need them).
- The Compaction rows and the other rows of the pass: not re-measured here.

## Commands run (all exit 0 unless noted)

- `git log --oneline -3`, `git diff --stat 632ebed 2ae8f35`, `git diff 632ebed 2ae8f35 -- backend-go/internal/transcript/ ':!*_test.go'` (306 lines), `git diff --stat ... '*_test.go'` (5 files, +190)
- `git archive <commit> backend-go | tar -x -C /tmp/review_perf/{old,new}` -- 0
- `go test -c -o /tmp/review_perf/{old,new}.test ./internal/transcript/` -- 0
- `go tool nm -size -sort address` on both test binaries; `go tool objdump -s 'transcript\.scanJSONValue$'` on both
- `go test -run '^$' -bench '^BenchmarkRunRoundsHistory$/(full|memo)/1000-turns' -benchmem -count=5 ./internal/transcript/` in old, new, old+pad, new+pad32, new+pad96, new+pad160 -- 0 (six runs)
- `go build -o /tmp/review_perf/old.jad ./cmd/jad` -- **exit 1**, `/usr/bin/ld: cannot find -lhtml_to_markdown_ffi`
- scratch under `/tmp/review_perf/` removed after the run
