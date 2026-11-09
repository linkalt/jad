# Compaction summary chain

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

Root cause: the summary only covers turns older than the kept window. Whenever the window covers
the whole history, which is common with a large `keep` and a high `trigger_tokens`, there is
nothing to summarize.

## 2. The user's idea

When the summary is on, `keep` no longer decides what is summarized. **Everything since the last
compaction is summarized**, and summaries pile up as a list instead of being re-summarized into
one:

```
compaction 1:  [turn 1 … turn 30]                         → [S1(1–30)]
compaction 2:  [S1] + [turn 31 … turn 60]                 → [S1, S2(31–60)]
compaction 3:  [S1, S2] + [turn 61 … turn 90]             → [S1, S2, S3(61–90)]
…
limit reached: [S1 … Sk] + new turns                      → oldest summaries merged, see §4.4
```

**What this gives:**

- **There is always something to summarize.** The trigger fires only on token size
  (`input_tokens >= trigger_tokens`), so the new turns are large by definition.
- **The keep-window bug cannot happen.** The keep window is gone for summary users.
- **No chain loss on each compaction.** An old summary is never re-summarized until the limit
  forces a merge. Today, and in the redesign's Option A, the earlier summary is re-summarized each
  time, and facts fade with every pass.
- **The work in progress is summarized as well**, which closes redesign P2 (mid-turn tool work
  stripped without a summary) without a separate active-turn mode.
- **The output stays small and bounded**: at most `max_summaries × max_output_tokens`, plus the
  verbatim tail (§4.2).

## 3. Goals and non-goals

**Goals:**

- With the summary on, a compaction always summarizes the turns since the last compaction, whatever
  `keep` is.
- Earlier summaries are kept verbatim, as separate items, and never silently deleted.
- The number of summaries is limited by config. When the limit is reached, the oldest ones are
  merged.
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
| `all` | This design: summarize everything since the last compaction, and keep a list of summaries. |
| `dropped` | Today's behavior: summarize only the turns older than the `keep` window, as one rolling summary. |

- **Default:** `all` (decision C1). The summary is off by default, so only sessions that turned it
  on are affected, and for them `dropped` has the bug.
- Under `all`, `keepbatch.turn.keep` and `keepbatch.turn.type` are not used, because nothing old
  is kept verbatim. `keepbatch.tools` takes on a new meaning (§4.2).
  - The keys stay valid, so a setting can switch modes.
  - The schema descriptions say they apply only with the summary off or under `scope: dropped`.

### 4.2 What a compaction keeps verbatim (`scope: all`)

The compacted history is:

```
[S1] [S2] … [Sk]                  summaries, oldest first (the list)
[current request]                 only when a run is in flight (PreserveResponseID set)
[active tail]                     the preserved response's calls, as today
```

- **Summaries:** the earlier ones are kept byte for byte, and the new one is appended.
- **Current request (mid-run only):** to continue, the agent needs the request it is working on.
  - The user message that started the active turn, and its steering messages, are kept verbatim
    and in order. They are also rendered into the summary input, so the summary can place the
    steering among the steps.
  - The newest `max(1, keepbatch.tools)` completed tool pairs of the active turn are kept verbatim,
    so the model sees its last real result (the floor of 1 comes from redesign §5.1).
  - Everything else in the active turn (older pairs, interim assistant messages, reasoning) is
    summarized. The new summary is told it covers "the current request so far".
- **Active tail:** the in-flight calls are excluded and kept as today
  (`excludePreservedItems`, `transcript/maintenance.go:216-287`), so they are never summarized.
- **No run in flight** (a final answer, or a manual compaction): everything is summarized,
  including the last turn. Whether to keep the last user message verbatim anyway is decision C2.

**Placement:** the summaries come before the current request. That is also the chronological
order, which fixes the placement weakness of redesign Option A.

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

- **Input:** only the new items (turns since the last compaction, and the summarizable part of the
  active turn). Earlier summaries are **not** re-summarized.
- **Context (decision C4):** the existing summaries are passed as read-only context, so the new
  summary can say "continues the work on X" and does not repeat facts. They are labeled "do not
  summarize this again; write only what is new". This uses up to
  `max_summaries × max_output_tokens` of input, which is small, and the budget comes off
  `max_input_bytes`.
- **Size:** everything since the last compaction is about `trigger_tokens` (200 000 tokens, roughly
  800 KB), well above `max_input_bytes` 200 000. Today `RenderItemsForSummary` would leave out the
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
  kept. It is measured on the new items only. Below it the compaction is a no-op
  (`ErrCompactionSkipped`, logged at info level). Under `all` this only matters for manual
  compaction, since the automatic trigger only fires on a large history.
- **No growth:** the size check (`transcript/maintenance.go:151`) stays. Under `all` it practically
  never fires, because the output is the summaries plus a short tail.
- **The transcript changed:** the digest check stays, and now covers the summarized items of the
  active turn too.

### 4.7 The three callers

| Caller | Today | Under `scope: all` |
|---|---|---|
| Automatic, after a response (`session/runner.go:974`) | keep window, `PreserveResponseID` when calls are pending | §4.2 with the current request kept |
| Overflow recovery (`session/runner.go:878`) | `KeepLatestTurn = true`: keep the whole latest turn | §4.2 too: the current request plus the newest pair(s) kept, the rest of the active turn summarized. This also fixes redesign P3 (keeping the whole turn removes nothing when the turn is the big part). Decision C6. |
| Manual (`app/maintenance.go:102`) | no run in flight, keep window | everything summarized (C2 for the last user message) |

### 4.8 Other readers of the compaction cache

To trace in R0, because they read the items or the turn split:

- `transcript/input_history.go` and `transcript/compaction_memo.go` (history reads and the memo).
- `transcript/state_context.go`: `After` is an item count. With several summary items at the
  start, state-context blocks must still land after them. **[inferred]**
- `app/session_fork.go` / `httpapi/session_fork.go`: a fork copies or replays the compacted
  history and uses `keep`. Under `all`, a fork must carry the `summaries` field across.
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
| `scope` | `all` \| `dropped`, default `all` (C1) | R1 |
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
| R0 | Baseline. Pin today's behavior in tests, including the bug as an expected-failure case: keep 20, 10 turns, no summary, tool work stripped. Golden `messages.jsonl` from the baseline build. Trace the readers in §4.8. | `transcript/*_test.go` |
| R1 | Settings (`scope`, `max_summaries`, `merge_prompt`, `min_input_bytes`). Cache `summaries` and `summarized_turns`. Plan: summary items are excluded from the turn split, and the old-cache rule. `scope: all` plan for "no run in flight". Merge step. | `sessionfs/compaction_summary_settings.go`, `transcript/compaction_summary.go`, `transcript/maintenance.go`, `session/compaction_summary.go` |
| R2 | Mid-run: keep the current request and the newest pair(s), summarize the rest of the active turn. Render priority and `max_tool_output_bytes` (redesign §8). | `transcript/compaction_summary.go`, `transcript/maintenance.go`, `session/compaction_summary.go` |
| R3 | Overflow recovery on the same rule (C6). Manual compaction. Fork carries `summaries`. | `session/runner.go`, `app/maintenance.go`, `app/session_fork.go` |
| R4 | Schema, sample settings, docs, `DECISIONS.md` entry. Golden diff, full suite, manual quality comparison (redesign §10.2, adding 3-5 chained compactions with merges). | `sample/schemes/`, docs |
| later | Optional chunking (C5). Summary model (redesign §7). | |

## 7. Tests

**`transcript`** (through `CompactHistory` with a recording `Summarize`):

1. **The bug:** keep 20, 10 turns, `scope: all`. `Summarize` gets all 10 turns, and the result is
   `[S1]` plus nothing else when no run is in flight.
2. **Second compaction:** `[S1]` plus 30 new turns. `Summarize` gets only the 30 turns (and S1 as
   context). The result is `[S1, S2]`, and S1 is byte for byte the same.
3. **Limit:** `max_summaries: 2`, a third compaction. The merge callback gets S1 and S2, and the
   result is `[S12, S3]`. Ranges and `merged` are right.
4. **A merge failure:**
   - Under `compact`, the list is `[S1, S2, S3]` (one over the limit), and the next compaction
     merges.
   - Under `skip`, the history is unchanged.
5. **Mid-run:** `PreserveResponseID` set. The current user message and steering messages are kept
   in order, the newest completed pair is kept (also with tools 0), the active tail stays
   verbatim, and the rest goes into `Summarize`. `continued_items` is right.
6. **Old cache:** item 0 with the header and no `summaries` field is read as one summary and
   kept. A user message that starts with the header *after* index 0 is not a summary.
7. **`scope: dropped`:** byte for byte the same results as R0's baseline.
8. **Digest:** an item appended during `Summarize` or the merge is refused.
9. **`min_input_bytes`:** a manual compaction of "proceed"/"ok" is a no-op with no model call.

**`session`:**

- An automatic mid-run compaction via `multiRoundStreamer`. The summary request carries the
  earlier summaries as labeled context, and the merge request uses `merge_prompt`. Both are counted
  on the run meter.
- Overflow recovery under `all` removes the active turn's older pairs.

**`sessionfs`:** default, bounds and wrong type of each new key; `min_input_bytes >
max_input_bytes` is refused.

**`app`:** manual compaction under `all`; a fork of a session with two summaries keeps both.

**Final gate:**

- `go test ./internal/... ./cmd/...`, `go vet`, `gofmt -l` and `git diff --check`.
- `messages-golden-diff` against the R0 build.
- Every bug-fix test must fail without the fix.

## 8. Risks

- **Losing verbatim recent context.** Under `all`, the agent continues from summaries, not from
  the last turns as they were. Quality now depends on the summary. Mitigations:
  - the current request and the last result are kept verbatim;
  - summaries are not re-summarized until the limit is reached;
  - a user who prefers verbatim turns keeps `scope: dropped`;
  - the R4 quality comparison measures the effect.
- **Latency inside the run:** up to two model calls per compaction (summary and merge), each
  bounded by `timeout_ms`.
- **Cost:** the input of each summary call is bounded by `max_input_bytes` plus the summaries
  used as context. The merge input is small. Both are counted on the run meter in automatic
  compaction. Manual compaction has no meter today (redesign §7). **[inferred]**
- **Prompt cache:** after each compaction the prefix is `[S1 … Sk]`. It stays stable between
  compactions and changes only when a new summary is appended or merged, which is better than
  today's single rolling item. **[inferred]**
- **Header in the item text:** changing `header` later still breaks the old-cache rule, but not
  caches written with the `summaries` field.

## 9. Decisions needed

- **C1.** Should `scope` default to `all`, or should summary users opt in? Recommended: `all`,
  because `dropped` has the bug and the summary is off by default anyway.
- **C2.** With no run in flight (final answer or manual compaction), should the last user message
  and the final answer also be kept verbatim? Recommended: keep the last final answer verbatim
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
- **C7.** The redesign is still only a proposal. Should its Phase 1 (guard and P7) be built
  separately first, or folded into R1? Recommended: fold it into R1, since §4.3 replaces its P7 fix.
