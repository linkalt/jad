# Compaction summary chain

> **Superseded (2026-11-11, T-013).** `compaction.summary` was removed from the Go backend together with its code (the summary request, `NewCompactionSummarizer`, `transcript.CompactionOptions.Summarize`, the digest guard and `ErrCompactionSkipped`). Nothing in this document is built or planned; it is kept as a record of a design that was not pursued. See `backend-go/DECISIONS.md`, "`compaction.summary` removed; `keepbackup.min_turns` replaced by `keepbackup.append_limit` (T-013)".

Status: proposal; the code is unchanged. Paths are relative to `backend-go/internal/`, and line
numbers were read on the tree at `2998c4c`. It builds on `docs/compaction-summary-redesign.md`
(called "the redesign" below) and replaces its §5 and §6.3. Its §7 (summary model) and §8 (input
rendering) still apply.

Labels: **[verified]** means read in the code. **[inferred]** means reasoned from the code, not
run. **[unverified]** means not checked.

## 1. The bug

Setup: `compaction.summary.enabled: true`, `keepbatch.turn.keep: 20`, and the trigger fires while
the session has 10 turns.

- `planCompaction` keeps the newest `KeepTurns` turns (`transcript/compaction_summary.go:83-91`).
  With 10 turns and keep 20, every turn is kept. **[verified]**
- `dropped()` returns `turns[:len(turns)-len(selected)]`, which is empty here (`:27-33`).
  **[verified]**
- With nothing dropped, `summarizeDroppedTurns` returns without calling `Summarize` (`:107`). No
  summary is written. **[verified]**
- The compaction then goes on without a summary. **[verified]**
  - Under `assistant_final`, `retainCompactedTurn` strips the tool pairs and interim messages of
    all 10 kept turns (`transcript/maintenance.go:118-130`, `:528`).
  - That work is lost unsummarized, which is the opposite of what the user enabled the summary for.
- If stripping does not make the history smaller (for example under `full`, or with turns that are
  mostly user text), the size check gives up (`:151-153`). **[verified]**
  - The automatic trigger then fires again after every response, with no relief, until the
    provider refuses the request and the overflow path runs. **[inferred]**
- **Related defect:** when a later compaction keeps the turn that holds an earlier summary, that
  summary is deleted (redesign P7, observed there).
- **Related defect:** a very small dropped set (for example "proceed" and "ok") produces a useless
  summary that is kept for good (redesign P4, observed there).
- **Not found:** the user reported that "the summary will error". No code path that returns an
  error for an empty dropped set was found: the summary is skipped silently. The error seen may be
  the P4 reply or the overflow that follows. **[unverified]**

Root cause: the summary only covers turns older than the kept window, and it runs only when
something falls out of that window. While the window still has room, a compaction strips tool work
and summarizes nothing.

## 2. The idea

**The kept window works as it does today until it is full.** It is full when it holds too many
turns or would be too big. When it is full, **the whole window is summarized** and emptied. Summaries pile up as a list instead of being re-summarized into one,
and a limit merges the oldest.

- **Window not full** (fewer than `keep` turns since the last summary): an ordinary compaction,
  exactly as today. Turns are kept and reduced by `keepbatch.turn.type`, and there is no summary
  and no model call.
- **Window full**, checked when a compaction runs, under either of two triggers:
  - **Turn count:** `keep` turns or more since the last summary.
  - **Size:** after the ordinary reduction, the window would still be at least
    `window_ratio × trigger_tokens` (§4.2).

  Every turn in the window is then summarized into one new summary, and the window starts again
  empty. Nothing is dropped without a summary.
- **Earlier summaries** are kept byte for byte. They are not counted as turns, and they are not
  stripped or summarized again until the limit merges them (§4.4).

Example with `keep: 20`, where the token trigger happens to fire about every 10 turns:

```
turn 10, trigger:  [t1 … t10]              window 10 < 20  → ordinary: [t1 … t10] reduced
turn 20, trigger:  [t1 … t20]              window full     → [S1(1–20)]
turn 30, trigger:  [S1] [t21 … t30]        window 10 < 20  → ordinary: [S1] [t21 … t30] reduced
turn 40, trigger:  [S1] [t21 … t40]        window full     → [S1, S2(21–40)]
…
limit reached:     [S1 … Sk] [full window]                 → oldest pair merged, then append
```

- **Uneven counts:** the window is checked only when a compaction runs, so it can hold more than
  `keep` turns, for example 25 when the trigger fires late. All 25 are summarized, and turns are
  never split across two summaries.
- **The keep-20/10-turns case** becomes an ordinary compaction when reducing leaves enough room.
  It no longer tries to summarize nothing. When the 10 turns are still too big after reducing,
  which is where today's compaction loops, the size trigger summarizes them.

**What this gives:**

- **The summary is used only when needed.** Recent turns stay verbatim, as reduced by `turn.type`,
  for as long as `keep` allows, and the model call happens only once per `keep` turns.
- **A summary always has real input:** at least `keep` turns, or a window of at least
  `window_ratio × trigger_tokens`.
- **Room after every compaction.** An ordinary compaction always leaves the context below
  `window_ratio × trigger_tokens`, so the next trigger needs real growth first. Compaction cannot
  repeat every round.
- **No loss along the chain.** An old summary is never summarized again until the limit forces a
  merge. Today, and in the redesign's Option A, the earlier summary is summarized again each time,
  and facts fade with every pass.
- **Bounded size:** at most `max_summaries × max_output_tokens`, plus the window, plus the
  current request.

## 3. Goals and non-goals

**Goals:**

- With the summary on, a compaction keeps the window as today until `keep` turns have
  accumulated. It then summarizes the whole window into one summary.
- Earlier summaries are kept verbatim as separate items. They are never deleted without notice
  and never use up a `keep` slot.
- The number of summaries is limited by config. When the limit is reached, the oldest ones are
  merged.
- An ordinary compaction that cannot bring relief does not loop. It falls back to summarizing the
  window early (§4.2).
- The current request stays usable mid-run: the agent must still see what it was asked and the
  calls it has in flight.

**Non-goals:**

- Changing `messages.jsonl`. Summaries live only in `staging/compaction-cache.json`, as today.
- Changing compaction with the summary off. `keep`, `assistant_final` and `full` stay exactly as
  they are.
- Summaries for sub-agents (`app/subagent.go` passes no `Summary`). This could come with the
  unified run model.
- The summary model and its pricing (redesign §7). It is unchanged and can be built on top.

## 4. Design

### 4.1 Modes

New key `compaction.summary.scope`:

| Value | Meaning |
|---|---|
| `window` | This design. Keep the window as today until it holds `keep` turns, then summarize all of it. Summaries form a list. |
| `dropped` | Today's behavior. Summarize only the turns older than the `keep` window, as one rolling summary. |

- **Default:** `window` (decision C1). The summary is off by default, so only sessions that turned
  it on are affected, and for them `dropped` has the bug.
- `keepbatch.turn.keep`, `keepbatch.turn.type` and `keepbatch.tools` keep their meaning in both
  modes. `keep` additionally becomes the size of a summary batch.

### 4.2 One compaction under `scope: window`

`planCompaction` splits the history into the **summaries** (§4.3) and the **window**, which is
every other turn, the active turn included, with the preserved response excluded as today. It then
picks one of two paths.

**The window is full** when either trigger holds:

1. **Turn count:** the window holds `keep` turns or more.
2. **Size:** the estimated size of the window *after* the ordinary reduction is at least
   `window_ratio × trigger_tokens`. The estimate works as follows.
   - There is no tokenizer. The response that fired the trigger reports `input_tokens`
     (`automaticCompactionTrigger`, `session/runner.go:1169`), and that count is scaled by how much
     the reduction shrinks the history:

     ```
     estimated_tokens = input_tokens × bytes(reduced window + summaries + active tail)
                                     / bytes(current history)
     ```

     Both byte counts come from `compactedInputBytes` (`transcript/maintenance.go:571`), which
     already compares these two sizes at `:151`.
   - The fixed part of a request (instructions, tool schemas) is spread over the history in
     proportion, so the estimate runs a little high and the summary comes slightly early. That is
     the safe side. **[inferred]**
   - The runner passes `input_tokens` to the plan in a new `CompactionOptions.TriggerInputTokens`
     field. With zero tokens (a manual compaction, decision C10), only the turn count applies.
   - `window_ratio` is 0 means the size trigger is off.

   The size trigger covers today's loop in both its forms:
   - An ordinary reduction that removes nothing is still at the trigger, so its estimate equals
     `trigger_tokens`, which is above the ratio. This is today's no-op at
     `transcript/maintenance.go:151`.
   - A reduction that removes some, but not enough, would fire the trigger again a round later.
     The size trigger catches it before it happens.

**A. Ordinary** (the window is not full): today's compaction, applied to the window only.

```
[S1 … Sk]  [window turns, reduced by turn.type]  [active tail]
```

The summaries go first and are untouched. Nothing is summarized.

**B. Summary** (the window is full):

```
[S1 … Sk, S(k+1)]  [current request]  [active tail]
```

- **New summary:** it covers every completed turn of the window and the summarizable part of the
  active turn.
- **Current request (mid-run only):**
  - The user message that started the active turn, and its steering messages, are kept verbatim
    and in order. They are also rendered into the summary input, so the summary can place the
    steering among the steps.
  - The newest `max(1, keepbatch.tools)` completed tool pairs of the active turn are kept
    verbatim, so the model sees its last real result (the floor of 1 comes from redesign §5.1).
  - The rest of the active turn (older pairs, interim assistant messages, reasoning) is
    summarized, and the summary is told it covers "the current request so far".
- **Active tail:** the in-flight calls are excluded and kept as today (`excludePreservedItems`,
  `transcript/maintenance.go:216-287`), so they are never summarized.
- **No run in flight** (a final answer, or a manual compaction): the whole window is summarized.
  Whether the last final answer is kept verbatim as well is decision C2.
- **The window restarts empty,** apart from the current request, which becomes the first turn of
  the new window.

**Which trigger fired** is logged with each summary (`turns` or `size`, with the estimate), so the
setting can be tuned from the log.

**The active turn under path A (decision C9).** An ordinary mid-turn compaction under
`assistant_final` strips the active turn's tool pairs without a summary (redesign P2). Under
`window`, the recommended rule is: if path A would strip completed pairs from the **active** turn,
take path B instead. Completed turns keep today's reduction: for them, the final answer stands for
the work, which is what `assistant_final` means.

**Placement:** the summaries come first, and every summary is older than every turn after it. The
order is chronological, which fixes the placement weakness of redesign Option A.

### 4.3 Knowing which items are summaries

Header prefix matching (today `renderSummaryItem`, `session/compaction_summary.go:235`) is fragile:
a user message that starts with the header would be taken for a summary. The cache records them
explicitly instead:

- **Cache field:** a new `summaries` array in `compactionCache` (`transcript/maintenance.go:16-28`).
  It is optional and absent in older caches, like `continued_items`.

  ```go
  type summaryEntry struct {
      Item      int    `json:"item"`       // index in Items
      FromTurn  int    `json:"from_turn"`  // first user turn covered, 1-based, session-wide
      ToTurn    int    `json:"to_turn"`    // last user turn covered
      Merged    int    `json:"merged,omitempty"` // number of summaries folded into this one
      CreatedAt string `json:"created_at"`
  }
  ```

  `SummarizedTurns int` is stored next to it: the running turn count used to number the next range.
- **Plan:** `planCompaction` reads `summaries` and takes those items out of the turn split. They
  never count as a turn, are never stripped, and never go into the new summary's input (§4.5).
- **Old caches** (no field): if item 0 starts with the configured header, it is treated as one
  summary with an unknown range. That covers sessions summarized before this change. Otherwise
  there are no summaries.
- **Text:** the header gets the range, e.g. `Summary of turns 31–60, written when the conversation
  was compacted:`. The configured `header` stays the prefix, so both the model and the old-cache
  rule still recognize it.
- **`messages.jsonl`:** unchanged. The active tail written by `ReplaceRecords` has the same shape
  as today. **[inferred; confirm with messages-golden-diff]**

### 4.4 The limit and merging

New key `compaction.summary.max_summaries`: an integer, default 4, between 1 and 64 (decision
C3).

When a compaction would leave more than `max_summaries`, it merges before it appends:

- **Rule (recommended): fold the oldest two into one.** One extra model call:
  - Input: the two summary texts, oldest first.
  - Prompt: `merge_prompt` (a new key, with a default): "Combine these two summaries of
    consecutive parts of a conversation into one. Keep the user's goals and constraints, decisions,
    files, and open work. Drop what a later part superseded."
  - Output: one item covering `FromTurn` of the first to `ToTurn` of the second, with `Merged`
    increased.
  - Cost: one small request (the input is at most `2 × max_output_tokens`).
  - The oldest summary absorbs the merges, so early facts are compressed more over time and recent
    facts less. That is the right bias for a coding agent.
- **Alternatives:**
  - **Merge all into one:** the same single call, but it rewrites every summary and brings back
    the chain loss.
  - **Drop the oldest:** no call, but it loses the original goals, which usually live in the first
    turns. Rejected.
- **When the merge call fails:** the same `on_failure` rule applies.
  - `skip`: no compaction this round.
  - `compact`: append without merging, so the list goes one over the limit, and the next
    compaction merges again.
  - In both cases the error is logged through `OnSummaryError`.
- **Order of work:**
  1. Summarize the new turns (call 1).
  2. If needed, merge the oldest pair (call 2).
  3. Take the store lock and check the digest of what was summarized, as today
     (`transcript/maintenance.go:101-103`).
  4. Write the cache.
- **With `max_summaries: 1`:** this is today's rolling single summary, but the merge happens in a
  separate call rather than in the same request.

### 4.5 What the summarizer reads

- **Input:** the window as it is stored at that moment, plus the summarizable part of the active
  turn. Earlier summaries are **not** summarized again.
  - Turns that earlier ordinary compactions reduced arrive in their reduced form: under
    `assistant_final`, that is the user message and the final answer. That is intended, because
    the final answer stands for the work.
- **Context (decision C4):** the existing summaries are passed as read-only context, so the new
  summary can say "continues the work on X" and does not repeat facts. They are labeled "do not
  summarize this again; write only what is new". This uses up to
  `max_summaries × max_output_tokens` of input, which is small, and the budget comes off
  `max_input_bytes`.
- **Size:** the window is at most about `trigger_tokens` (200 000 tokens, roughly 800 KB), well
  above `max_input_bytes` 200 000 when it holds unreduced tool work. Today `RenderItemsForSummary` would leave out the
  *oldest* blocks (`session/compaction_summary.go:213-231`), which are the user's instructions.
  - **This design depends on redesign §8:** render with priority (user and assistant messages
    always, then calls, then tool outputs cut head and tail and replaced oldest first by an
    "omitted" marker) and `max_tool_output_bytes`. Build it in the same phase (R2).
  - **Optional chunking (C5):** split the new items into chunks of `max_input_bytes` and write one
    summary per chunk. This fits the list model naturally, since each chunk becomes a list entry
    and the limit merges them. It costs one model call per chunk inside the run. It is proposed as
    a later option, not the default.

### 4.6 Guards

- **Small input:** `compaction.summary.min_input_bytes` (redesign §6.1, default 2 000, 0 = off) is
  kept. It is measured on the window only. Below it, path B falls back to path A (an ordinary
  compaction), with the reason logged at info level. Under `window`, a full window is rarely this
  small, so the guard mostly covers very small `keep` values.
- **No growth:** the size check (`transcript/maintenance.go:151`) stays as the last guard. Under
  `window`, a path A that would be a no-op is caught earlier by the size trigger. Path B practically
  never fails it, because its output is the summaries plus a short tail.
- **The transcript changed:** the digest check stays, and now covers the summarized items of the
  active turn too.

### 4.7 The three callers

| Caller | Today | Under `scope: window` |
|---|---|---|
| Automatic, after a response (`session/runner.go:974`) | keep window, `PreserveResponseID` when calls are pending | §4.2: path A, B or C |
| Overflow recovery (`session/runner.go:878`) | `KeepLatestTurn = true`: keep the whole latest turn | Always path B: the window is summarized whether full or not, and the current request plus the newest pair(s) are kept. This also fixes redesign P3, where keeping the whole turn removes nothing when that turn is the big part. Decision C6. |
| Manual (`app/maintenance.go:102`) | no run in flight, keep window | path A or B by the window count, as automatic; decision C10 on whether a manual request should force B |

### 4.8 Other readers of the compaction cache

To trace in R0, because they read the items or the turn split:

- `transcript/input_history.go` and `transcript/compaction_memo.go` (history reads and the memo).
- `transcript/state_context.go`: `After` is an item count. With several summary items at the
  start, state-context blocks must still land after them. **[inferred]**
- `app/session_fork.go` / `httpapi/session_fork.go`: a fork copies or replays the compacted
  history and uses `keep`. Under `window`, a fork must carry the `summaries` field across.
- `app/subagent.go`: it names the cache path. Sub-agents have no summary, so there should be no
  change. **[inferred]**
- The frontend: check whether the session view displays the compacted items, and how a list of
  summaries looks there. **[unverified]**

## 5. Config

All the keys are under `compaction.summary`. Each needs a normalizer in
`sessionfs/compaction_summary_settings.go` (`compactionSummaryKeys` at `:71` rejects unknown
keys), `sample/schemes/setting.schema.json` (`additionalProperties: false`) and config tests
(CLAUDE.md rule 3).

| Key | Type, default, bounds | Phase |
|---|---|---|
| `scope` | `window` \| `dropped`, default `window` (C1) | R1 |
| `window_ratio` | number, 0.5, 0 = off, otherwise above 0 and below 1 (C8) | R1 |
| `max_summaries` | int, 4, 1..64 (C3) | R1 |
| `merge_prompt` | string, default text in §4.4 | R1 |
| `min_input_bytes` | int, 2000, 0 = off, ≤ `max_input_bytes` | R1 |
| `max_tool_output_bytes` | int, default = `max_item_bytes`, 100..1 000 000 | R2 |

**Compatibility:**

- Existing settings stay valid.
- A session summarized before the change is read through the old-cache rule (§4.3).
- `scope: dropped` reproduces today's results exactly; the regression tests pin that.

## 6. Phases

| Phase | Content | Main files |
|---|---|---|
| R0 | Baseline. Pin today's behavior in tests, including the bug as an expected-failure case: keep 20, 10 turns, no summary, tool work stripped, and a repeated no-op. Golden `messages.jsonl` from the baseline build. Trace the readers in §4.8. | `transcript/*_test.go` |
| R1 | Settings (`scope`, `window_ratio`, `max_summaries`, `merge_prompt`, `min_input_bytes`). Cache `summaries` and `summarized_turns`. Plan: summaries are split from the window and excluded from the turn count, plus the old-cache rule. Paths A and B with no run in flight, both triggers, and `TriggerInputTokens` passed from the runner. Merge step. | `sessionfs/compaction_summary_settings.go`, `transcript/compaction_summary.go`, `transcript/maintenance.go`, `session/compaction_summary.go` |
| R2 | Mid-run path B: keep the current request and the newest pair(s), summarize the rest of the active turn. The active-turn rule (C9). Render priority and `max_tool_output_bytes` (redesign §8). | `transcript/compaction_summary.go`, `transcript/maintenance.go`, `session/runner.go`, `session/compaction_summary.go` |
| R3 | Overflow recovery always takes path B (C6). Manual compaction (C10). Fork carries `summaries`. | `session/runner.go`, `app/maintenance.go`, `app/session_fork.go` |
| R4 | Schema, sample settings, docs, `DECISIONS.md` entry. Golden diff, full suite, manual quality comparison (redesign §10.2, adding 3-5 chained compactions with merges). | `sample/schemes/`, docs |
| later | Optional chunking (C5). Summary model (redesign §7). | |

## 7. Tests

**`transcript`** (through `CompactHistory` with a recording `Summarize`):

1. **Window not full:** keep 20, 10 turns. `Summarize` is not called, and the result is today's
   ordinary compaction of the 10 turns.
2. **Window full:** keep 20, 20 turns (and 25 turns). `Summarize` gets all of them, and the result
   is `[S1]` when no run is in flight.
3. **After a summary:** `[S1]` plus 10 turns gives path A, with S1 first and byte for byte the
   same, and not counted as a turn. `[S1]` plus 20 turns gives `[S1, S2]`, and `Summarize` gets
   only the 20 turns, with S1 as context.
4. **Size trigger:**
   - Keep 20 and 10 turns whose ordinary result is not smaller: path B, where today the
     compaction is a no-op. This is the reported case.
   - Keep 20 and 10 turns reduced to an estimate of 0.6 × trigger, with ratio 0.5: path B. Reduced
     to 0.4 × trigger: path A.
   - At exactly the threshold: path B.
   - `window_ratio: 0`: only the turn count applies, which reproduces the no-op.
   - `TriggerInputTokens: 0`: only the turn count applies.
   - At the runner level, after a path A compaction the next trigger needs real growth, so no two
     compactions run in consecutive rounds unless that much was added.
5. **Active-turn rule (C9):** mid-run with path A, where the active turn has completed pairs that
   would be stripped: path B is taken instead.
6. **Limit:** `max_summaries: 2`, a third compaction. The merge callback gets S1 and S2, and the
   result is `[S12, S3]`. Ranges and `merged` are right.
7. **A merge failure:**
   - Under `compact`, the list is `[S1, S2, S3]` (one over the limit), and the next compaction
     merges.
   - Under `skip`, the history is unchanged.
8. **Mid-run path B:** `PreserveResponseID` set. The current user message and steering messages are kept
   in order, the newest completed pair is kept (also with tools 0), the active tail stays
   verbatim, and the rest goes into `Summarize`. `continued_items` is right.
9. **Old cache:** item 0 with the header and no `summaries` field is read as one summary and
   kept. A user message that starts with the header *after* index 0 is not a summary.
10. **`scope: dropped`:** byte for byte the same results as R0's baseline.
11. **Digest:** an item appended during `Summarize` or the merge is refused.
12. **`min_input_bytes`:** a tiny window that met only the size trigger falls back to path A, with no model
    call.

**`session`:**

- An automatic mid-run compaction via `multiRoundStreamer`. The summary request carries the
  earlier summaries as labeled context, and the merge request uses `merge_prompt`. Both are counted
  on the run meter.
- Overflow recovery under `window` takes path B and removes the active turn's older pairs.

**`sessionfs`:** default, bounds and wrong type of each new key. `min_input_bytes >
max_input_bytes` is refused. `window_ratio` of 1 or above, or below 0, is refused.

**`app`:** manual compaction under `window`; a fork of a session with two summaries keeps both.

**Final gate:**

- `go test ./internal/... ./cmd/...`, `go vet`, `gofmt -l` and `git diff --check`.
- `messages-golden-diff` against the R0 build.
- Every bug-fix test must fail without the fix.

## 8. Risks

- **A cliff after each summary.** Right after path B, the agent has only summaries and the
  current request. A turn later it again has a growing verbatim window. Quality just after a
  summary depends on the summary. Mitigations:
  - the current request and the last result are kept verbatim;
  - C2 can keep the last final answer;
  - summaries are not re-summarized until the limit is reached;
  - a user who prefers verbatim turns keeps `scope: dropped`;
  - the R4 quality comparison measures the effect.
- **Latency inside the run:** only on path B, with up to two model calls (summary and merge), each
  bounded by `timeout_ms`. Path A has no model call.
- **Tool work lost on path A:** completed turns lose their tool pairs under `assistant_final`
  before they are ever summarized, as today. C9 protects only the active turn.
- **Cost:** the input of each summary call is bounded by `max_input_bytes` plus the summaries
  used as context. The merge input is small. Both are counted on the run meter in automatic
  compaction. Manual compaction has no meter today (redesign §7). **[inferred]**
- **Prompt cache:** after each compaction the prefix is `[S1 … Sk]`. It stays stable between
  compactions and changes only when a new summary is appended or merged, which is better than
  today's single rolling item. **[inferred]**
- **Header in the item text:** changing `header` later still breaks the old-cache rule, but not
  caches written with the `summaries` field.

## 9. Decisions needed

- **C1.** Should `scope` default to `window`, or should summary users opt in? Recommended:
  `window`, because `dropped` has the bug and the summary is off by default anyway.
- **C2.** When path B runs with no run in flight (after a final answer, or a manual compaction),
  should the last final answer also be kept verbatim? Recommended: keep the last final answer verbatim
  (one item, cheap, and the next user message often refers to it), and summarize the rest.
- **C3.** What should `max_summaries` default to, and what should its maximum be? Recommended: 4,
  maximum 64.
- **C4.** Should earlier summaries be passed as read-only context to the new summary call?
  Recommended: yes.
- **C5.** Should chunking, one summary per `max_input_bytes` chunk, be built now or later?
  Recommended: later, after the R4 comparison shows whether render priority is enough.
- **C6.** Should overflow recovery use the same rule (summarize the active turn's older pairs) in
  place of `KeepLatestTurn`? Recommended: yes, because that is the case where keeping the whole
  turn fails.
- **C8.** What should `window_ratio` default to? Lower means summaries come more often and leave
  more room; higher means more verbatim context and more compactions between summaries.
  Recommended: 0.5. Should it be a share of `trigger_tokens`, or a fixed `window_tokens`?
  Recommended: the share, so that one setting works for any model's trigger.
- **C9.** Should a mid-turn path A that would strip the active turn's completed pairs take path B
  instead? Recommended: yes. This is where unsummarized loss hurts most, in a long tool-heavy run.
- **C10.** Should a manual compaction always force path B (summarize now), or follow the window
  count? It has no triggering response, so the size trigger needs a token count. It could use the
  last stored response usage (`app/compaction_usage.go`, not traced) or skip the size trigger.
  Recommended: follow the turn count only, and add a `summarize: true` request option later if
  wanted.
- **C7.** The redesign is still only a proposal. Should its Phase 1 (guard and P7) be built
  separately first, or folded into R1? Recommended: fold it into R1, since §4.3 replaces its P7 fix.
