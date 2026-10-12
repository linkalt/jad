# Design: statecontext redesign (plan, nothing built yet)

Status: PROPOSAL. Written 2026-10-12, revised the same day after the user's three additions (section 2.0).
Every "Decide" needs an answer before the phase that uses it.

Rules for every phase: no hardcoded limits (config + defaults + `sample/schemes/setting.schema.json` + config tests);
`messages.jsonl` stays byte-identical unless you decide otherwise in 2.8; a session with no `statecontext` setting sends
a byte-identical provider request. NOTE: the old rule "a session WITH statecontext keeps a byte-identical request" no
longer holds by design: placement (2.8) and empty-skip (2.9) change the request. Those two phases need a golden check
that shows exactly the intended difference and nothing else.

## 1. What exists today (read from source)

- Config: `statecontext` in the session setting (`sessionfs/state_context_settings.go`): `enabled`, `trigger` (an array
  that accepts only `compaction`, line 53), `tools` (`task`, `memory`, `process`, `subagent`), `role` (`developer`
  default), `max_bytes`. Per-provider options live under `tools.native.<tool>.statecontext` (memory has selectors `type`,
  `filter`, `exclude_any`, `limit`; task has `statuses`, `limit`; process and subagent have `states`, `limit`).
- Build: `collectStateContextSnapshot` (`app/state_context.go`) runs each provider, renders ONE JSON document, and puts
  `stateContextPreamble` (`app/state_context.go:14`, hardcoded) in front (`:182`). Over `max_bytes` it trims items from the
  largest provider, then queries. It returns "include" whenever statecontext is enabled and the trigger matches; I found no
  check for "every provider returned nothing" (to be confirmed by a test in step 0).
- Triggers today: manual compaction (`app/maintenance.go:89`) and automatic compaction (`RefreshStateContext`,
  `app/resolver.go:803`). So a new session, or one that never compacted, has no block at all, and `memory` records are
  invisible to the model until the first compaction.
- Storage: the rendered text is stored whole in `statecontext.json` (`transcript.StateContextSnapshot.Text`), read per run
  (`app/resolver.go:819-823`). Compaction replaces it; front-compaction keeps it; reset deletes it.
- Delivery: `session/runner.go:317` appends it as ONE extra message AFTER the whole history on every request, never in
  `messages.jsonl`. In a tool loop the history grows by a call and a result each round, so the block moves to the end each
  time: its tokens are re-processed every round and every request. Sub-agent runs get none (`app/subagent.go`).
- Adapter: the Anthropic adapter folds a mid-history system or developer message into the previous user message, or sends it
  as a `system` role if `mid_conversation_system` is set (`anthropicadapter/input.go` `addSystem`). Its cache breakpoint on the
  history skips folded blocks (`anthropicadapter/cache.go` `lastHistoryBlock`).
- History: `messages.jsonl` is the golden contract. Compaction rewrites a cache (`compactionCache`: `Items`, `ActiveTail`,
  `SourceSHA256`) that is replaced only by a compaction or reset (`transcript/maintenance.go:15`, `compaction_memo.go`).
- Prompt today: one leading prompt (`runner.go:301`) built at settings load by `loadPrompt` (`sessionfs/session.go:1011`) from
  the profile `instructions` plus `prompt_documents` files (paths relative to cwd, `~` expanded). Role from `prompt_role`.
- Frontend: the prompt library is browser `localStorage` (`frontend/src/promptStore.ts`); the server library is read-only
  (`httpapi/library_handlers.go:12`). A run request carries only `input`, `model`, `supports_interaction`
  (`httpapi/handler.go:907`).

## 2. The items

### 2.0 Requirements added by the user (verbatim)

- statecontext read at startup, we need to add more triger rather than only compactions. we will have config for every
  trigger, example custom user prompt statecontext/user triggered.
- to maximize cache hit ratio add it to end of messages history request. example: system prompt + compactions +
  messages.jsonl + newer statecontext + new request or tool_call/result. Now we need to decide where older/newer
  statecontext messages saved, messages.jsonl or compaction-cache.
- Empty statecontext (only header content) shouldn't be added to messages request at all in other words autodisabled.

How they changed the plan: 2.7 (triggers), 2.8 (placement and storage) and 2.9 (empty skip) are new. 2.4 shrinks, and the
old 2.6 ("snapshot stored as parts") is dropped: if each block is an entry stored as rendered, a header or prompt edit
applies to the next entry, and nothing is re-joined at request time.

### 2.1 `request_draft` in statecontext: DECIDED, done

A draft is a user message; the snapshot is a developer prompt. The provider and `accepted-request.json` were removed (see
`DECISIONS.md`). What must survive a compaction goes to `memory` (a type such as `request`, selected by
`tools.native.memory.statecontext`).

### 2.2 Header text in settings

Add `statecontext.header` (string). Default = the current preamble text, so the default text is unchanged.
- Decide: empty allowed? Recommend NO (config error). The header carries the "untrusted data" warning; a typo must not
  silently drop it. Someone who wants a different warning writes one.
- The header counts in `max_bytes` (no second limit); a header larger than `max_bytes` is a config error.
- With 2.8 the header should also say that a later block supersedes earlier ones (see 2.8, option "append").
- Tests: default text unchanged; custom header appears; empty and non-string rejected; schema case.

### 2.3 Prompt text and documents

Add `prompt` (inline) and `prompt_documents` (file paths), same path rules and reader as `loadPrompt` (reuse it; no second
file reader). Order in a block: header (trusted config), custom prompt (trusted config), then the JSON data (untrusted).
The custom prompt stays OUTSIDE the data JSON: the header says values are data, so instructions inside values would
contradict it. A missing file is a config error naming the path.
- Per trigger (2.7), so `compaction` can carry a different prompt from `user`.
- Decide: when are files read? Each run (`Resolver.Resolve`, `app/resolver.go:257`, calls `LoadSessionSettings` on every
  HTTP request, so probably yes, to be confirmed in step 0). With 2.8 a file edit applies to the NEXT block written, not to
  blocks already in the history.

### 2.4 User-controlled prompt in the frontend

Reading: the user types or picks a prompt and fires the `user` trigger (2.7); the result is a block in the history.
- Options for the text:
  - A. Typed (or chosen from the existing prompt library, which already lives in the browser) at the moment of firing, sent
    in the request, kept only inside the block. No new storage. Recommend.
  - B. A saved per-session file on the server, edited through `GET`/`PUT`. More surface, and the block would not change
    when the file does anyway (2.8).
- Config: `triggers.user.enabled` (default false) and `triggers.user.max_prompt_bytes`; a deployment that wants a fixed
  prompt keeps it off.
- API: `POST /api/sessions/{ref}/statecontext` with `{prompt?: string}`; refuses while a run is active, as manual
  compaction does (`httpapi/mutation_gate.go`). Returns the block it wrote.
- Trust: the text goes into a developer or system message. The token already allows `bash`, so this adds no new power;
  write that in `DECISIONS.md` and cap the size.
- UI: a button and a small panel next to the prompt library, with a preview of the block before it is sent. Follows
  `frontend/CLAUDE.md`; the sequence in `CLAUDE.md` applies (impact_analyst, `astryx component` output, frontend_coder,
  typecheck, tests, ui_checker, frontend_reviewer).

### 2.5 Efficient snapshot: counts and references instead of full records

Idea (yours): list how many records match a filter and let the model fetch them with the tool when it needs them.
```json
"memory": {"mode": "summary", "groups": [
  {"type": "bug_report", "filter": {"status": "fixed"}, "count": 3},
  {"type": "bug_report", "filter": {"status": "open"},  "count": 10}]}
```
`type` and `filter` are exactly the arguments of a `memory` list call, so each group is also the recipe to fetch it.
- memory: `group_by` a field per selector; counted by scanning, bounded by `max_scan_records` and `max_scan_bytes`; a
  truncated scan reports `at_least: true`. This is the large win.
- task: counts per status; ids only for `in-progress`. process, subagent: counts per state; running and queued ones listed
  with ids (the model must act on them).
- Config: `mode` (`items` default, `summary`, or `hybrid`: items for some states, counts for the rest) on the existing
  per-tool `statecontext` block, plus `group_by` on memory selectors. Default stays `items`.
- Decide: render as JSON counts or as sentences ("You have 3 memory type bug_report with status fixed")? Recommend JSON:
  field values come from records the model wrote, and putting them into a sentence blurs data and instructions.
- Link to 2.8: if blocks are appended and never edited, small blocks matter. Summary mode is what makes frequent triggers
  affordable. Do not start before the measurement in step 0.

### 2.7 Triggers (new)

Today `statecontext.trigger` is a list that accepts only `compaction`. Proposed events, each with its own config:

| Trigger | When | Why |
|---|---|---|
| `compaction` | after manual or automatic compaction | exists |
| `session_start` | the first run of a session that has no block yet (and after a front-compact that removed it, see 2.8) | tasks and memory are visible from the start instead of after the first compaction |
| `user` | the user fires it from the UI (2.4) | on-demand block with a custom prompt |
| `run_start` | the start of each user turn; written only when the data differs from the latest block | keeps the model current without compaction; needs 2.5 and 2.9 to be cheap |

Not planned: a trigger the model fires through a tool. Say if you want it.

Config shape (replaces the flat `trigger`, `tools`, `max_bytes`; `enabled`, `header`, `role` stay at the top):
```json
"statecontext": {
  "enabled": true, "header": "...", "role": "developer",
  "triggers": {
    "compaction":    {"tools": ["task","memory"], "max_bytes": 65536, "prompt": "...", "prompt_documents": []},
    "session_start": {"tools": ["memory"]},
    "user":          {"tools": ["task"], "max_prompt_bytes": 4096}
  }
}
```
A trigger fires only if it appears in `triggers`; there is no per-trigger `enabled` flag to forget.
- Decide: the old flat `trigger`/`tools` keys. Recommend a config error that names the new shape (the project does this for
  removed keys, for example `instructions` in a model file), not a compatibility reader. The root `.jad/setting.json` has no
  `statecontext` now, so nothing of yours breaks.
- Decide: "read at startup" means (a) first run of a session, as in the table, or (b) when the server starts. Recommend (a);
  (b) has no session to attach a block to.

### 2.8 Placement and storage (new): the cache question

Goal: the request prefix must stay byte-identical from one request to the next. A block stored where it was written, and
never edited, satisfies that: `system prompt + compaction + messages + block + newer input`. The block must be stored
RENDERED, never re-rendered per request (data can change between requests and would break the prefix).

Behavior to decide:
- Append: every trigger appends a new full block; older blocks stay untouched, and the header says a later block supersedes
  earlier ones. Cache-friendly, but the history grows by one block per trigger until the next compaction removes all of them.
  Cheap only if blocks are small (2.5, 2.9).
- Replace: the new block replaces the older one. One block in the history, but everything after the old block's position loses
  its cache on every trigger.
Recommend Append, with compaction clearing every older block (compaction rewrites the prefix anyway).

Where the blocks are saved:

| | `messages.jsonl` | compaction cache | new sidecar file |
|---|---|---|---|
| Contract | changes the golden format; needs your explicit OK and `messages-golden-diff` | none | none |
| Lifecycle | in the transcript | rewritten only by a compaction or reset, so `session_start`, `user` and `run_start` blocks cannot be added without changing that | append-only, written at any trigger |
| Side effects | shows in the UI as a message; touches turn and usage counting, front-compact (anchored by message id), checkpoints, cursor caches | cache is validated against the transcript hash (`SourceSHA256`) | needs its own anchor handling |
| Fit | all triggers | `compaction` only | all triggers |

Recommend the sidecar (`statecontext.jsonl` in staging). Each entry: rendered text, role, trigger, time, and an anchor (the
history position it follows). The request builder inserts each entry after its anchor. This is a third option beyond your
two; if you prefer one of them, the compaction cache is the cheaper choice but then only the `compaction` trigger places a
block in the history, and the other triggers would need the old end-of-request placement.

Open checks (step 0, in `transcript/input_history.go`): what is a stable anchor for an input item (stored message id, batch
id, or position plus a hash of the item). Edge cases to settle once the anchor is known:
- front-compact removes the oldest messages: entries anchored there should move to the start of what remains (the latest
  entry still describes the current state), not be dropped;
- compaction and reset: clear all entries; reset deletes the file;
- an entry whose anchor cannot be found is ignored and logged, never an error that stops the run;
- the Anthropic adapter: a block just written, with nothing after it, sits past the last cache breakpoint (breakpoints skip
  folded blocks), and is covered from the next request on; test both the `mid_conversation_system` on and off cases.

### 2.9 Empty block is not sent (new)

A block is empty when no provider returned an item or group and no custom prompt applies; the header alone is never sent.
- Skip it at write time (nothing is written, nothing is added to a request) and treat `has_state_context` as false.
- Decide: a non-empty block exists, then the data becomes empty (all tasks deleted). The old block would now be wrong. With
  Append (2.8), recommend writing ONE minimal "state is now empty" block in that case only, since silence would leave a
  false statement in the history. This is the one exception to "never send an empty block"; say if you want none.
- A custom prompt with no data is not empty: the user asked for those words to be sent.

### 2.10 Dropped

Old 2.6 (snapshot stored as parts) is replaced by 2.8.

## 3. Phases (each one builds, passes tests and can be committed alone)

0. Measure and read. No code. Real sessions: bytes of the block per provider and in total; the number of requests that carry
   it; cached-token counts in provider usage if reported (to see what the current placement costs); whether `Resolve` reloads
   settings and prompt files per request; the anchor choice (2.8); and a test that confirms the empty case of 1.
   Output: numbers pasted here.
1. Empty block is not sent (2.9). Small, independent, works on today's single snapshot file.
2. `header` key (2.2).
   Phases 1 and 2: DONE.
3. `triggers` config shape and the `session_start` trigger (2.7), still one snapshot file and the old end-of-request
   placement. Settles the config surface before the storage change.
   DONE (see `DECISIONS.md`, "Statecontext triggers"). `run_start` and `user` are added to the same `triggers` map by
   phases 6 and 7.
4. `prompt` and `prompt_documents` per trigger (2.3).
   DONE (see `DECISIONS.md`, "Statecontext prompt"). The frontend-typed prompt (2.4) stays with phase 7.
5. Placement and sidecar store (2.8). The largest risk: golden check of the provider request for both providers
   (Responses and the Anthropic adapter), `messages.jsonl` unchanged, front-compact, reset and compaction tests.
   DONE for the store and the placement (see `DECISIONS.md`, "Statecontext placement and sidecar store"). Anchor: the
   count of history items. Not in this phase: the "state is now empty" block (it needs a trigger that runs on later
   requests, so it comes with phase 7) and a live check after a rebuild.
6. `user` trigger: endpoint, config, UI (2.4).
   Backend DONE (see `DECISIONS.md`, "Statecontext user trigger"): config, `POST .../statecontext` with preview, tests. UI
   still to do, following the frontend sequence.
7. `run_start` with change detection.
   DONE (see `DECISIONS.md`, "Statecontext run_start trigger"). Change detection compares `data_sha256` of the latest
   block; the "state is now empty" block is the first block whose providers are all empty. Live check pending.
8. Summary mode (2.5): memory first, then task, process, subagent. Gated by step 0.
   Memory part DONE (see `DECISIONS.md`, "Statecontext memory summary mode"): per-selector `group_by`. Task, process and
   subagent summaries are not done. Step-0 live byte measurements are still not taken.

Order reason: 1 to 4 are small and keep today's storage, so they can ship before the risky part; 5 must precede 6 and 7;
8 is independent and can move earlier if step 0 shows large blocks.

## 4. Verification for every phase

- Go: `go test ./internal/... ./cmd/...`, `go vet`, `gofmt -l`, `git diff --check`.
- A config test per new key (default, valid, invalid), the schema updated, and a mutation check for each rule.
- Phases 1 and 5: golden check of the provider request showing only the intended difference; `messages-golden-diff` if any
  transcript path changes (phase 5 only if you pick `messages.jsonl`).
- Frontend phases: `npm test`, `npm run typecheck`, then `ui_checker` and `frontend_reviewer`.
- A live check after a Go rebuild and restart (the server is never restarted by the agent).

## 5. Decisions (answered by the user)

1. Header may not be empty (2.2): YES, an empty header is a config error.
2. Config shape (2.7): YES, `triggers` object; the old flat keys become a config error naming the new shape.
3. "Startup" means the first run of a session (2.7): YES.
4. Newer blocks: APPEND, cleared at compaction.
5. Storage: SIDECAR file (`statecontext.jsonl` in staging).
6. Empty data after a non-empty block: ONE "now empty" block.
7. User prompt: typed at firing time (2.4 option A).
8. Summary entries: JSON counts.
9. Start with step 0 and phases 1 and 2: YES.
