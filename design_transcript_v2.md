# Design: token-efficient `transcript` (v2)

Builds on design_transcript_reader.md. Selector grammar (`path`), access switches, the read-only scanner, `source: backup`, symlink/path checks and cursor identity do not change. The output, the numbering unit and the size settings do.

## 1. Cost today (measured on `.jad/state`)
One entry per record, each cut at `entry_max_bytes` (4000): a turn of 20 tool calls costs 20 calls + 20 results of up to 4 KB. Reading "what happened" costs far more than finding the one step that matters. v2 splits the read in two: a cheap index, then the full text of one step.

## 2. Items and steps
messages.jsonl holds four kinds of items: user, assistant, tool (a `function_call` item in a `response.completed` line and its `function_call_output` line, joined by `call_id`) and reasoning.

- A **turn** starts at a user message. Turn numbers are unchanged: `session_turn` when the file carries it (so compaction does not renumber turns), else the count. The header says so.
- A **step** is `turn.step`, counted in file order inside the turn as the file is now: the user message that opens a turn is `N.0`; every later assistant message, tool call and reasoning item takes the next number (`N.1`, `N.2`, ...). A call and its result are one step (the result belongs to the call's number). Numbers do not change with paging or filtering. A user message that does not open a turn (batch index > 0), an unknown item type, an unreadable record and a skipped over-long line are steps too (shown as a marker), so nothing hides. A result with no call in the file (call compacted away) is a step of its own, `result orphan`.
- Reasoning takes a number but is never listed, counted or marked: numbers have gaps where it was. The header says gaps are reasoning. `step` + `part: reasoning` reads it on request (summary/content text; encrypted content is never shown).

## 3. Two modes, by `step`
**List (no `step`)** – one line per step, newest turn selection as before: `turns`, `tail`, `cursor`, default the last turn; filters `role` (user|assistant|tool; tool = call steps and orphan results), `tool` (glob on the call's tool), `search` (+`context` steps each side). Search tests the FULL text (message text; for a call its name, arguments and result) but prints the normal preview line.

    <label>turns 3-3 of 1-3 (turn numbers survive compaction; steps are turn.step in file order, N.0 is the user message, gaps are reasoning)
    3.0 user: Create ONE new file: frontend/src/…+2210 bytes…rc/App.tsx
    3.1 call grep ok 1423B {"pattern":"^import|rowAct…+31 bytes…Key","path":"/home/…+12 bytes…/App.tsx"}
    3.3 assistant: Done. I created…+310 bytes…tests pass.

- user/assistant: the text, whitespace runs collapsed to one space (one line per step), cut to `preview_bytes`.
- call: tool name, `ok`/`error` (the envelope `status` of the result: `ok` is ok, anything else error; `unknown` when it cannot be read; `no-result` when no result is in the file), result size in bytes, then the arguments as compact JSON: keys in full, each top-level value cut to `value_bytes` (a string is cut inside its quotes; an object/array value is cut as its compact text). Arguments that are not a JSON object are one value.
- result: not listed (only the marker on the call line). Reasoning: nowhere.
- Paging: at most `max_list_entries` steps and `response_max_bytes` per page; the cursor names the next step (turn index + step) and the end of the range, plus the file identity. A page always holds at least one step.

**Detail (`step` given)** – `step: "3.2"` or a range `"3.1-3.4"` (file order, may cross turns), `part: call|result|both|reasoning` (default both; reasoning is only shown when asked). `step` excludes `turns`, `tail`, `role`, `tool`, `search`, `context`.

    step 3.2 call grep ok, part both; bytes 0-1832 of 1832
    == 3.2 call grep [call_01Q] ok 1423B
    arguments: {"pattern":"^import|rowActionHidden","path":"/home/…/App.tsx"}
    result:
    …full output…

The text of the selected steps is one stream; a page is `max_detail_bytes` of it from a byte offset, cut on a rune boundary. The cursor carries the step, part, offset, identity and range end, so a big result is paged and never silently cut. A step with no such part says so. Base64 `data:` URLs stay replaced by `[data URL N bytes]`, as today.

## 4. Middle truncation
`midCut(text, budget)`: if `len(text) <= budget`, unchanged. Otherwise `head + "…+N bytes…" + tail`, N = omitted bytes, total length (marker included) `<= budget`; head 70 % / tail 30 % of what is left after the marker; both cut on rune boundaries (head backwards, tail forwards), so N counts the whole omitted bytes including the bytes moved off a split rune. If the budget cannot hold the marker plus a byte, or the result would not be shorter than the text, the text or the bare marker is returned, whichever is shorter (never longer than the input).

## 5. Settings (`tools.native.transcript`)
| key | default | cap | why |
|---|---|---|---|
| `preview_bytes` | 200 | 64 KiB | about 50 tokens: enough to recognise a message |
| `value_bytes` | 80 | 64 KiB | about 20 tokens per argument value |
| `max_list_entries` | 100 | 5000 | at most about 6k tokens of index per page |
| `max_detail_bytes` | 16384 | 4 MiB | about 4k tokens per detail page |
Kept: `response_max_bytes` (list page cap), `line_max_bytes`, `scan_max_bytes`, `suggest_max`, `timeout_ms`, `allow_*`. Removed (rejected naming the key): `entry_max_bytes` (replaced by the two above) and `max_turns_per_call` (replaced by `max_list_entries`). Removed input: `full` (use `step`), rejected with a hint.

## 6. Phases (one commit each, tests with each)
1. `midCut`, step-spec parser, new settings keys (old ones still accepted), schema/sample.
2. Step walker and list mode; old keys and `full` removed; tests updated.
3. Detail mode, `part`, paging.
4. DECISIONS, descriptions, `transcripts` consistency, byte measurement on real transcripts, leak/race tests.

## 7. Open points
- "Not reset by compaction" is read as: turn numbers keep `session_turn`, as today. Steps count from the start of the turn as the file holds it.
- A detail cursor is a byte offset into text that is rebuilt per page; if a pending call gets its result between pages the stream can shift (file identity does not change on append). Acceptable; noted in DECISIONS.
