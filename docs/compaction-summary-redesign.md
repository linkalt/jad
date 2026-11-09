# Compaction summary redesign

Status: proposal. Code is unchanged. Paths are relative to `backend-go/internal/`. Line numbers were read on the current tree.
Labels: **[verified]** means read in the code. **[observed]** means a scratch test showed it; those tests ran in a `/tmp` copy of `backend-go` and were deleted. **[inferred]** means reasoned from code without running it. **[unverified]** means not checked.

## 1. Background: how compaction and the summary work today

- The automatic trigger runs after every completed response. `runRound` calls `compactAfterResponse` (`session/runner.go:792`). That function checks `input_tokens >= trigger_tokens` (`session/runner.go:1169-1214`), whose default is 200 000 (`sessionfs/session.go:30`). When the response asked for tools, it sets `PreserveResponseID` to the response id (`session/runner.go:974-984`). **[verified]**
- The summarizer is wired in `automaticCompactionOptions` (`session/runner.go:988-1001`). It passes `NewCompactionSummarizer(ctx, r.streamer, r.settings, ...)`, and `ctx` is the run context that carries the run's cost meter (`session/runner.go:612-616`, `:715`). **[verified]**
- Overflow recovery uses the same options and also sets `KeepLatestTurn = true` (`session/runner.go:878-893`). **[verified]**
- Manual compaction holds `runMu`, so no run is in flight. It passes no `PreserveResponseID` and no `KeepLatestTurn`, and it uses `manualCompactionSummarizer` (`app/maintenance.go:102-128`, `app/compaction_summary.go:58-70`). **[verified]**
- `planCompaction` (`transcript/compaction_summary.go:39-93`) does the following:
  - It removes the preserved response's items from the history (`:59-64`, `transcript/maintenance.go:216-287`).
  - It splits the rest into turns: a user message starts a turn, a steering message continues the turn before it, and the first item always opens a turn (`:69-82`).
  - It selects the newest `KeepTurns` turns (`:83-91`).
- `dropped()` returns `turns[:len(turns)-len(selected)]` (`:27-33`). `summarizeDroppedTurns` summarizes only that set and records a digest of it (`:99-126`).
- `CompactHistory` (`transcript/maintenance.go:74-214`) works in these steps:
  - It puts the summary item first (`:105-108`).
  - Each selected turn goes through `retainCompactedTurn` (`:118-130`, `:528-563`), which keeps user messages, the `keptToolPairs` pairs and, under `assistant_final`, the final assistant message.
  - The preserved response becomes the active tail. Under `assistant_final`, that tail is reduced to its `function_call` items (`:131-149`, `:300-369`).
  - If the result is not smaller, it gives up (`:151-153`).
- `keptToolPairs` counts `keepbatch.tools` across all kept turns, newest first. It only applies under `assistant_final` (`transcript/maintenance.go:484-526`). **[verified]**

## 2. Problem statement

**P1. The summary covers only turns older than the kept window. This is intentional.**
- `dropped()` is `turns[:len(turns)-len(selected)]` (`transcript/compaction_summary.go:29`).
- `keepbatch.turn.keep` must be between 1 and 1000 (`sessionfs/session.go:882-888`), and the default is 1 (`:832`). So at least the newest turn is always selected and never summarized. **[verified]**

**P2. A mid-turn compaction strips the active turn's tool work without summarizing it.**
- During a tool-heavy run, the active turn is the last selected turn. It is not exempt: `KeepLatestTurn` is false on this path (`session/runner.go:974-984`), so `retainCompactedTurn` runs on it (`transcript/maintenance.go:118-122`).
- Only the newest `keepbatch.tools` pairs survive (`:488-526`), and under `assistant_final` interim assistant messages are dropped too (`:552-557`). None of this is in `dropped()`, so none of it is summarized.
- **[observed]** Setup: one older turn, then an active turn with completed pairs `call_2..4` and an in-flight `call_5`, with `assistant_final`, keep 30, tools 1 and `PreserveResponseID` set. Result: Summarize got **0 items**. `call_2..3` disappeared, `call_4` (the newest completed pair) and `call_5` (the active tail) remained, and the earlier turn's `call_1` was also gone.
- **[observed]** Under `full`, every pair goes, including `call_4`, whether tools is 0 or 5, because `keptToolPairs` returns nothing outside `assistant_final` (`:490`). `full` keeps assistant messages and reasoning, though (`:558-560`).
- With the defaults (tools 0, `sessionfs/session.go:865-871`), even the newest completed pair is stripped. **[inferred from :490 + :546-550]**
- At 250K tokens, most of the context is tool output, so most of the context is exactly what gets stripped. That share is the user's observation; it was not measured here. **[unverified]**

**P3. Setting `KeepLatestTurn = true` in `compactAfterResponse` was rejected.**
- With that flag, the last selected turn skips `retainCompactedTurn` (`transcript/maintenance.go:110-121`). The active turn would keep every pair, and only older turns would be reduced.
- When 250K of tool output sits in the active turn, compaction then removes almost nothing, or the size check (`:151-153`) rejects it. The trigger fires again on the next round, and the run reaches the provider's limit and the overflow path. **[inferred]**
- That flag is right for overflow, where the run must not lose work it is still doing. It is not a size reduction.

**P4. A tiny dropped set produces a useless summary that is then kept for good.**
- **[observed]** After an earlier compaction, a compaction with keep 1 handed Summarize exactly two items: `user: "proceed"` and `assistant: "ok"`.
- Nothing checks the size. `summarizeItems` only refuses a rendered text that is entirely whitespace (`session/compaction_summary.go:74-77`).
- The reply ("I don't have a transcript to summarize...") is stored as item 0 of `compaction-cache.json` (`transcript/maintenance.go:105-108`, `:165-193`) and is sent with every later request.
- The reported production case (keep 30, the `"proceed"` turn) is the user's report; I reproduced the same mechanism, not that session. **[unverified for that session]**

**P5. The summary always uses the session's model and client.**
- `summaryRequest` copies `model.Model`, `model.Request` and `PromptRole` (`session/compaction_summary.go:128-160`). The streamer is `r.streamer` (`session/runner.go:998`) or the manual `runStreamer` (`app/compaction_summary.go:68-69`).
- Only nine keys are accepted (`sessionfs/compaction_summary_settings.go:71`). Any other key is rejected (`:121`), and the schema has `additionalProperties: false` (`sample/schemes/setting.schema.json:1135`). **[verified]**
- There is a reusable pattern for a per-use model. Sub-agents have their own `model` (`sessionfs/native_subagent_settings.go:172-179`, `:503-510`). `resolveSubagentModels` turns it into a model file (`sessionfs/layout_load.go:211-250`, called at `:113`). `resolveFallbackModels` also checks the reasoning effort (`:125-149`). `childConfig` loads the model with `WithModel` (`app/subagent.go:232-247`). **[verified]**

**P6. Summarizing in-turn tool work makes each summary input large.**
- The input limits are `max_input_bytes` 200 000 (between 1 000 and 10 000 000) and `max_item_bytes` 4 000 (between 100 and 1 000 000) (`sessionfs/compaction_summary_settings.go:47-50`, `:141-146`). **[verified]**
- `RenderItemsForSummary` (`session/compaction_summary.go:199-233`) applies them like this:
  - Each item is cut to its first `max_item_bytes` (`cutText` keeps the head, `:303-312`).
  - An earlier summary is placed first and capped at `max_input_bytes/2`.
  - Then the **oldest blocks are left out entirely** ("[N older items left out]"), including user messages.
- Prompt caching:
  - `summaryRequest` builds `ModelSettings` without `PromptCacheKey` (`:154-159`). So the session's `prompt_cache_key: session` (`app/compaction_summary.go:50`) is not sent, because `BuildRequest` only sets the key when it is present (`session/runner.go:466-470`).
  - A fixed `request.prompt_cache_key` in the model file *is* copied (`:130-134`). **[verified]**
  - Even with a key, the summary request's prefix (summary prompt + rendered text) shares nothing with the session's requests, and a chained summary changes the start of the input each time. **Assume no cache hits.** **[inferred]**
  - Whether the Anthropic adapter's `cache.automatic` would apply depends on adapter config. **[unverified]**

**P7. An earlier summary in the kept window is silently deleted.** This was found while verifying; it is not in the original list.
- The summary item is a `developer` message at index 0, so it forms a turn of its own (`transcript/compaction_summary.go:78`) and uses up one `keep` slot.
- When that turn is selected (fewer than `keep` real turns), `retainCompactedTurn` drops it. It is not a user message, not an assistant final, and under `full` not an assistant message or reasoning (`transcript/maintenance.go:542-561`).
- **[observed]** Keep 30, both modes: the summary from the first compaction was gone after a second compaction, and Summarize was not called.
- With `role: user` it would survive, because user messages are always kept (`:542-544`). **[inferred]**
- Without a fix, the second mid-turn compaction in the same long turn would delete the progress summary written by the first.

## 3. Decisions already made (by the user)

1. Automatic compaction stays runner-driven, through the trigger in `compactAfterResponse`.
2. The summarizer should read the transcript including earlier tool work. The extra token cost is accepted.
3. No agent-driven nudge. Telling the agent it is over the limit compacts nothing, because only the runner rewrites the transcript.

## 4. Goals and non-goals

Goals:
- During a mid-turn compaction, summarize the active turn's completed tool pairs instead of discarding them.
- Never summarize a dropped set too small to be worth summarizing.
- Never lose an earlier summary.
- Allow an optional cheaper `compaction.summary.model`, priced and capped like the run.
- Bound the summarizer input predictably, and leave out tool output before user instructions.

Non-goals:
- Changing `messages.jsonl` contents. It is the golden contract; the summary lives only in the compaction cache.
- Summarizing the *kept* older turns' pairs, which `assistant_final` reduces to the final answer by design (see open question Q3).
- Sub-agent summaries (`app/subagent.go:186-194` passes no `Summary`).
- Agent-driven compaction.
- Changing how turns are split.

## 5. Design: summarizing completed tool pairs in the active turn

### 5.1 Boundary rule (common to all options)

Applies when the compaction has `PreserveResponseID != ""` (mid-turn) and the summary is on. The **active turn** is the last turn of `plan.turns` after the preserved items are excluded.
- **In flight:** the preserved response (`activeTail`, `transcript/maintenance.go:216-265`). Its calls have no outputs yet. It stays verbatim, reduced to its `function_call` items as today (`:131-141`).
- **Latest pair(s):** the newest `max(1, keepbatch.tools)` *completed* pairs of the active turn stay verbatim. A completed pair is a call whose output is present (same rule as `keptToolPairs`, `:511-523`). The floor of 1 is new and applies only to the active turn, and only in this mode, so the model always sees its last real result.
- **Summarized:** every other non-user item of the active turn before the newest kept pair: older pairs, interim assistant messages and reasoning. Reasoning is not rendered (`session/compaction_summary.go:250`).
- **User and steering messages** of the active turn are **kept verbatim**, as today (`:542-544`), and are **also rendered** into the summarizer input in order. That way the summary can say "after step N the user steered: ...", because their position relative to the summarized steps is lost in the retained list.
- `continued_items` is still computed from the retained items (`:115-130`), so steering stays non-turn-starting. **[inferred]**
- Without `PreserveResponseID` (a final-answer response, or manual compaction), nothing changes. Manual compaction runs only when no run is in flight (`app/maintenance.go:102`).

### 5.2 Option A: extend the dropped set (recommended)

`planCompaction` gets an `activeDropped [][]byte` part, and `dropped()` returns older turns plus `activeDropped`. The existing single summary request, the digest check (`transcript/maintenance.go:101-103`) and on_failure handling then cover the new items unchanged. `CompactHistory` builds the active turn's retained list from the boundary rule instead of `retainCompactedTurn`.

The summary item stays at index 0. Labels are inserted into the rendered input: `--- earlier turns ---` and `--- current request, earlier steps (the request itself is kept) ---`. The prompt gains one sentence asking for a "Progress on the current request" section.

| | |
|---|---|
| Model calls | one per compaction |
| Code | small: plan split, `dropped()`, retained list, one flag |
| Chain | the earlier summary is re-summarized each time (needs the P7 fix) |
| Weakness | the summary sits before the older kept turns, but describes work done after them |

### 5.3 Option B: a separate mid-turn pass

The older-turns summary stays as it is. A second request, with its own prompt focused on working notes, summarizes the active turn's completed pairs into a "progress note". The note is placed in the active turn, right after its user and steering messages.

| | |
|---|---|
| Model calls | up to two per compaction (more latency inside the run, `timeout_ms` each) |
| Code | larger: two items and two digests in `CompactHistory`, a note-recognition rule, a second prompt key |
| Chain | the note must survive when its turn ends (same P7 issue, now for notes inside turns) |
| Strength | correct chronological placement |

### 5.4 Rejected options

- **`KeepLatestTurn = true`:** rejected; see P3.
- **Rely on `compaction.overflow.tool_output_max_bytes`:** it cuts outputs only in one request, and the transcript keeps them whole (`sessionfs/compaction_summary_settings.go:32-36`). Every round stays at the limit.
- **Agent nudge:** excluded by decision 3.

### 5.5 Recommendation

**Option A.** It reuses the single-summary path, its digest and its failure rule. It adds no second request inside the run. Its main risk, placement, can be measured with the quality procedure in §10.2. If that comparison shows the model redoing work because of placement, move the item placement to the active turn in a follow-up; the dropped set and the request stay the same. That follow-up becomes Option B's placement without the second request.

### 5.6 PreserveResponseID and steering details

- `excludePreservedItems` removes the preserved response's items by content count (`transcript/maintenance.go:267-287`). So they are never in `activeDropped`, and the in-flight calls cannot be summarized away. **[verified]**
- The newest kept pair is chosen among pairs whose output exists in the compactable history. The preserved calls have no outputs yet, so they cannot take that slot. **[inferred]**
- Parallel calls in one response are separate pairs, and only the newest one(s) are kept whole. **[inferred]**
- Reasoning before a call is already stripped under `assistant_final` today. Whether some providers then reject a call that has lost its reasoning item is **[unverified]**. It is an existing behavior, not a new one.
- The compaction runs in the run's goroutine before the tools run (`session/runner.go:792` then `:813`). Steering is stored at the start of the next round (`:722`). So nothing appends during the summary in this path, and the digest check still guards other writers. **[inferred]**

## 6. Guard for a small dropped set, and fixing P7

### 6.1 Threshold
- New key `compaction.summary.min_input_bytes`: an integer, default 2 000, 0 turns the guard off, and it must be ≤ `max_input_bytes`.
- It is measured on the rendered text of the dropped items **excluding an earlier summary**: the part `RenderItemsForSummary` puts after `earlier`.
- The check runs before any model call, in the callback returned by `NewCompactionSummarizer`.

### 6.2 Behavior below the threshold

| Choice | Effect | Verdict |
|---|---|---|
| skip summary, compact anyway | drops "proceed" (could be a real instruction) and, if the dropped set holds the earlier summary, drops that too | rejected |
| no-op (return `transcript.ErrCompactionSkipped`) | transcript untouched, no model call; manual returns `compacted:false`; automatic tries again next round (no model call while still tiny) | **recommended** |
| fold the tiny turns into the kept window | keeps everything, still reduces older kept turns | possible later; more plan logic |

The no-op path already exists (`transcript/compaction_summary.go:111-113`). It is logged at info level, not through `OnSummaryError`, because it is not a failure. With Option A, a mid-turn dropped set is normally large, so the guard mostly affects manual compaction and large `keep`.

### 6.3 Keeping an earlier summary (P7)
- Add `CompactionOptions.SummaryHeader` (empty means off).
- In `planCompaction`, a first turn that consists only of a message whose text starts with the header is a **summary turn**. It does not count toward `KeepTurns`.
  - When `Summarize` runs, it is always part of `dropped()`, so it is chained as "Earlier summary" (already handled, `session/compaction_summary.go:207-209`).
  - When there is nothing else to drop, or the summary is off, it is retained verbatim.
- This fixes the second-mid-turn-compaction loss.

### 6.4 Cleaning up an existing bad summary
- **Natural:** after §6.3, the bad item is the summary turn. The next compaction that gets past the guard re-summarizes it together with real content, and a summary with no facts adds nothing. No migration is needed.
- **Immediate (manual, documented, no code):**
  1. Make sure the session is idle.
  2. Back up `staging/compaction-cache.json` and `.backup`.
  3. Delete `items[0]`.
  4. Subtract 1 from every `continued_items` index, dropping any that become negative.
  5. Write the same content to both files. The reader tries the primary first, then the backup (`transcript/maintenance.go:780`).
  - `assistant_final_items` holds item strings, not indexes, and needs no change.
  - State-context entries store `After` as an item count (`transcript/state_context.go:59`), so a block lands one item later. That should be harmless. **[inferred]**
  - The history memo notices the changed file stamps (`transcript/compaction_memo.go:53`). **[inferred]**
- A cleanup endpoint is not proposed (see Q5).

## 7. Optional `compaction.summary.model`

- **Key:** `compaction.summary.model`, a string. In a catalog-layout session it is a model ID `<category>/<name>[@effort]`. A value ending in `.json` is a model file. Absent means the session's model, which is today's behavior.
- **Normalization** (`sessionfs/compaction_summary_settings.go`):
  - Add `model` to `compactionSummaryKeys`.
  - Add `Model`, `ModelPath` and `ReasoningEffort` to `CompactionSummary`.
  - A `.json` value is resolved like an agent's (`resolveFrom(settingDir, expandHome(model))`, `sessionfs/native_subagent_settings.go:508-510`).
  - The rejection of `.json@effort` (`sessionfs/layout_load.go:229-231`) is copied.
- **Resolution:** add `resolveCompactionSummaryModel` next to `resolveFallbackModels` and call it at `sessionfs/layout_load.go:116`. It uses `SplitModelEffort`, `models.Lookup` (an unknown ID is a config error that lists the IDs) and `checkModelEffort`.
- **Validation at run preparation** (next to `app/resolver.go:480-491`): if `max_run_cost_usd` or a cost budget is set and the summary model has no pricing, refuse with the same 422. This is stricter than fallback, which skips the model (`app/model_fallback.go:275-279`): a summary request that cannot be priced would otherwise slip past the cap without any notice.
- **Streamer and ModelSettings:**
  - Build `summaryConfig := config` and set its `ModelPath` and `ReasoningEffort`, then call `WithModel()` (as `childConfig` does, `app/subagent.go:233-247`).
  - Get the key with `providerAPIKey`.
  - The streamer is `withPricing(newProviderStreamer(...), loaded.Model)`, i.e. the primary branch of `buildRunStreamer` (`app/model_fallback.go:291-298`). The session's `model_fallback` is not applied (Q2).
  - The ModelSettings are `runnerModelSettings(summaryConfig)`.
  - **Automatic:** add `Streamer responses.Streamer` and `Model *ModelSettings` to `session.CompactionSummarySettings`. `automaticCompactionOptions` (`session/runner.go:997-999`) uses them when they are non-nil, otherwise `r.streamer`/`r.settings`. `compactionSummarySettings` (`app/compaction_summary.go:15-30`) fills them.
  - **Manual:** `manualCompactionSummarizer` (`app/compaction_summary.go:58-70`) uses the same helper.
  - The client should be cached with the runner (`app/resolver.go:492-497`) rather than built for every run (Q4).
- **Cost:**
  - In automatic compaction, the summary request uses the run context, so the run's meter prices it at the summary model's own rate (`app/provider_cost.go:20-41`). It counts toward `max_run_cost_usd`, checked in `runLimit` (`session/runner.go:1055-1060`).
  - Manual compaction puts no meter in its context: there is no `WithCostMeter` in `app/maintenance.go` or `httpapi/mutation_handlers.go`, so its cost is not counted against any run cap. **[inferred from grep]** Budget accounting was not traced. **[unverified]**
- **Same provider recommended:** one credential, consistent token accounting, and fewer request-shape surprises. The input is plain text, so a different provider works in principle. **[inferred]**
- **Context window:** the summary model must accept roughly `max_input_bytes/4` tokens. There is no context-window field to check this against. **[inferred; grep found none]**
- **Caching:** do not count on cache hits (P6). Optionally send the session's derived `PromptCacheKey` when the provider is the same; expect few hits, because the prefix changes.

## 8. Input size handling

- **Today:** a 250K-token, tool-heavy input (about 1 MB, assuming about 4 bytes per token **[inferred]**) is cut to 4 000 bytes per item. Once it is over 200 000 bytes, the oldest blocks are dropped. Those include the user's original instructions in older dropped turns, and the first steps of the active turn.
- **Proposed rendering priority** (Phase 4, in `RenderItemsForSummary`):
  1. The earlier summary (cap stays `max_input_bytes/2`).
  2. All user and steering messages, and assistant messages, each up to `max_item_bytes`. These are never left out.
  3. Tool calls: name and arguments, cut to `max_item_bytes`.
  4. Tool outputs: cut to a new `max_tool_output_bytes` (default = `max_item_bytes`), keeping **head and tail**, because errors and test results are usually at the end. Under budget pressure, outputs are replaced oldest-first by `tool result (id): [N bytes omitted]`, so the step stays visible.
- **Cost bound:** each compaction reads at most about `max_input_bytes` (≈50K tokens at the default) plus `max_output_tokens` 4 096. That is small next to one 200K main round. Raising `max_input_bytes` toward 1 MB is the user's quality/cost choice, and makes a cheaper `compaction.summary.model` worthwhile.
- **Quality risks:** chained summaries lose a little more each time; facts from early in a long run fade. Mitigations:
  - The prompt already asks to keep the earlier summary's facts (`sessionfs/compaction_summary_settings.go:43`).
  - Option A keeps one cumulative item.
  - `max_output_tokens` can be raised for long runs.
  - The §10.2 procedure measures the loss after 2-3 chained compactions.
- Map-reduce over chunks is left out for now (several requests, more latency inside the run).

## 9. Config, schema, compatibility, migration

New keys under `compaction.summary`. Each needs a normalizer, `sample/schemes/setting.schema.json` (the summary object is `additionalProperties: false`) and `sessionfs/compaction_summary_settings_test.go`.

| Key | Type / default | Phase |
|---|---|---|
| `min_input_bytes` | int, 2000, 0 = off, ≤ `max_input_bytes` | 1 |
| `summarize_active_turn` | bool, `false` | 2 |
| `model` | string, absent = session model | 3 |
| `max_tool_output_bytes` | int, default = `max_item_bytes`, 100..1 000 000 | 4 |

- `summarize_active_turn` defaults to false so existing summary users see no new model calls mid-turn. The user's config sets it to true (Q1).
- Earlier configs stay valid. The compaction-cache format is unchanged: the summary turn is recognized by `header`, which already exists (`:132-134`).
- Changing `header` means an old summary is no longer recognized; that limitation exists today too.
- No migration is needed for existing sessions. A bad summary is cleaned as in §6.4.
- `messages.jsonl`: the active tail written by `ReplaceRecords` (`transcript/maintenance.go:196-200`) keeps its current shape. Run `messages-golden-diff` to confirm.

## 10. Test plan

### 10.1 Unit and integration tests

Every bug-fix test is checked with `mutate-check.sh`.

`transcript` tests go through `CompactHistory` with a recording `Summarize`, using fixtures like `newSteeredFixture` (`transcript/compaction_continued_test.go:20-54`).
1. Mid-turn, `assistant_final`, tools 1, `PreserveResponseID` set:
   - Summarize gets the older turn(s) plus `call_2..3` (calls and outputs) and the active user and steering messages.
   - Retained: those user and steering messages, `call_4` and its output, the active tail `call_5`.
   - `continued_items` is correct.
2. Same with tools 0: `call_4` is still kept (floor of 1). With tools 3: `call_2..4` are kept and only interim messages are summarized.
3. Same in `full` mode: interim assistant messages are summarized, and the newest pair is kept.
4. `summarize_active_turn` off: behavior is identical to today (the regression baseline is the §2 P2 observation).
5. Without `PreserveResponseID`, the active-turn rule does not apply.
6. A steering message between pairs: it stays as a continued (not turn-starting) user item, and is rendered in order.
7. P7: a summary turn inside the kept window is retained, in both modes. When something is dropped, it goes into `dropped()`. It does not use up a `keep` slot.
8. Digest: an item appended during Summarize is refused (as in `session/compaction_summary_test.go:156-186`).

`session` tests:
- Guard: a "proceed"/"ok" dropped set leads to no request, a `compacted:false` no-op and an unchanged history. At exactly the threshold, the summary request is made. With `min_input_bytes` 0, it is made too.
- Automatic mid-turn via `multiRoundStreamer`:
  - A round with calls and `input_tokens >= trigger` triggers the summary.
  - The request contains the outputs of the completed pairs and the labels.
  - It is counted on the run's meter.
- Render priority (Phase 4): a user message is never left out; tail-keeping cut; omitted-output marker.

`sessionfs` tests: each new key's default, bounds and wrong type. An unknown summary model ID gives an error that lists the IDs. A bad effort. `.json@effort` is refused.

`app` tests:
- Manual and automatic both send the summary to the summary model's client (a fake provider per model); without the key, the session model is used.
- A missing price with `max_run_cost_usd` gives a 422.
- Manual compaction still uses no active-turn rule.

Final gate: affected packages while iterating, then `verify-go.sh --race "./internal/session/ ./internal/transcript/ ./internal/sessionfs/"`, `git diff --check`, and `messages-golden-diff`.

### 10.2 Manual quality comparison

1. Copy a real long, tool-heavy session directory to `/tmp` (never the live one).
2. Write 8-10 probe questions with answers taken from the full transcript: files changed, the last failing test and why, user constraints, the next step.
3. Run a test-only build three times on copies, each with the same trigger point: (a) today's behavior, (b) Option A with the session model, (c) Option A with a cheaper model.
4. For each run, record: tokens after compaction, summary input bytes, "[N left out]" counts, summary cost, and the full summary text.
5. Ask the session model each probe from the compacted history, and score blind (correct / partial / wrong / hallucinated).
6. Repeat with 2-3 chained compactions to measure compounding loss.
7. Check whether (b) shows redo or confusion caused by placement, which decides the §5.5 follow-up.

## 11. Risks

- More latency mid-run: one summary request (up to `timeout_ms`, default 120 s) inside the round. On failure, `on_failure` applies (`compact` = today's lossy strip, `skip` = no compaction).
- A summary placed before the older kept turns may confuse the model (§5.5).
- If the summary fails under `on_failure: compact`, the run loses its tool work exactly as today. Consider recommending `skip` with `summarize_active_turn`.
- The guard turns some manual compactions into no-ops. The user may then see "nothing compacted" and needs a clear log line.
- A cheaper model writes weaker summaries, and the chain compounds that loss.
- Treating the first turn as a summary turn relies on a header prefix match. A user message that starts with the header would be taken for a summary. That risk exists today in rendering (`session/compaction_summary.go:257`).

## 12. Open questions

- Q1. Should `summarize_active_turn` default to true? It is opt-in under a feature (the summary) that is already opt-in.
- Q2. Should the summary model use the session's `model_fallback`? Or should it fall back to the session model when the summary model fails?
- Q3. Should the completed pairs of *older kept* turns, which `assistant_final` reduces to the final answer, also be summarized once?
- Q4. Where should the summary-model client be cached (per runner fingerprint, or in `shared`)?
- Q5. Is a cleanup action (an endpoint or CLI) for a bad summary wanted, beyond the §6.4 procedure?
- Q6. Should overflow recovery (`KeepLatestTurn`) use the same boundary rule instead of keeping the whole turn, then fall back to cutting tool outputs?
- Q7. Should a manually compacted, interrupted turn (no final answer) count as active?

## 13. Phased plan (rough line counts; tests are separate)

**Phase 1, smallest viable: guard + P7.** No behavior change unless a summary is too small, or an earlier summary would have been lost.
- `sessionfs/compaction_summary_settings.go` +12 (key, default, bound)
- `sample/schemes/setting.schema.json` +6
- `session/compaction_summary.go` +20 (measure without the earlier summary, return skipped, info log)
- `app/compaction_summary.go` +2
- `transcript/maintenance.go` +4 (`SummaryHeader`)
- `transcript/compaction_summary.go` +25 (summary turn: not counted, always dropped, kept verbatim)
- `session/runner.go` +1 and `app/maintenance.go` +1 (pass the header)
- Tests about +150

**Phase 2: active-turn summary (Option A).**
- `transcript/compaction_summary.go` +40 (`activeDropped`, boundary)
- `transcript/maintenance.go` +25 (retained active turn, floor of 1)
- `session/runner.go` +3 (flag when `PreserveResponseID` is set)
- `session/compaction_summary.go` +15 (labels, prompt sentence)
- settings and schema +15
- Tests about +200

**Phase 3: `compaction.summary.model`.**
- `sessionfs/compaction_summary_settings.go` +20
- `sessionfs/layout_load.go` +30
- `session/compaction_summary.go` and `session/runner.go` +12
- `app/compaction_summary.go` +35
- `app/resolver.go` +10
- schema +4
- Tests about +180

**Phase 4: render priority and `max_tool_output_bytes`.**
- `session/compaction_summary.go` +50
- settings and schema +12
- Tests about +100

**Phase 5 (optional): the boundary rule in overflow recovery (Q6).**
- `session/runner.go` and `transcript` +20
- Tests about +80
