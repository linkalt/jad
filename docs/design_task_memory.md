# Shared Task Memory: Design

## 1. Goal
Give five agents (master, decider, explorer, executor, reviewer) a shared per-task memory so each skips work a previous agent already did. Each agent loads `codemem.txt` in full into context. There is no search or filtering.

## 2. Problems codemem solves

1. **Duplicate exploration across subagents.** With many specialized subagents, each one explores the same code on its own. Subagent 1 explores a package, then subagent 2 gets another task that touches it and explores it again. Every repeat costs tokens and time, and the two may reach different conclusions. With codemem, the first explorer records what it covered (`explored`) and what it found (`files`, risks, unknowns). Later agents read that and explore only the gaps.
2. **Exploration can move to a cheap model.** Exploration is high-volume and low-judgment: find symbols, locations and relationships. If its output is a shared, structured record, a cheap model can do it, and the expensive executor and reviewer models read the result instead of exploring. This works only if the format is strict and facts are tool-verified where possible (section 9), so the executor can trust a cheap model's output.

Success measures: fewer exploration tool calls per task after the first, and a lower share of tokens spent on the executor and reviewer models.

## 3. What is stored

| Stored | Not stored |
|---|---|
| Symbol definitions (kind, signature) | Grep results |
| Relationships (calls, implements, ...) | Source snippets |
| Source locations (line ranges) | Intermediate reasoning |
| Risks (typed, one line) | Free-form prose (goes in report.md) |
| Unknowns (open questions) | |
| Explored scope, changed symbols, decisions | |

## 4. Files per task

```
.task/<id>/
  issue.md       # problem document: written once by the decider, read by all
  codemem.txt    # loaded by every agent: explored, files, changed, unknowns, decisions
  impact.txt     # loaded by reviewer only: callers and tests of changed symbols
  report.md      # free-form output of each phase
  snapshots/     # copy of codemem.txt at each phase boundary
```

With `codemem` (section 10), `codemem.txt` and `impact.txt` become `codemem` records; `issue.md` and `report.md` stay as files because codemem holds code facts only.

`impact.txt` is separate so explorer and executor do not pay for it.

## 5. `codemem.txt` format

Line-oriented text. A path appears once per file; symbols nest under it.

```
# T-123 rev:abc123
## explored
pkg/a edges | pkg/b sig
## files
pkg/a/foo.go #9f2c
  40-72 method Foo.Bar(ctx,id string) error
    -> b.Store.Get:51, a.Foo.validate:48
    ! concurrency: holds f.mu while calling Store
pkg/b/store.go #1d07
  12-30 method Store.Get(id string) (Item, error)
    ? retries on error?
## changed
mod a.Foo.Bar (executor)
## unknowns
[open] b.Store.Get retries on error? (executor)
## decisions
D1 Store.Get retry is out of scope (decider)
```

Conventions:
- `#hash` is per file. A mismatch means the file entry is stale: re-read the file and rewrite the entry.
- Line numbers are hints. The hash decides staleness.
- `->` outgoing edge, `!` typed risk, `?` unknown, `~` prefix on LLM-inferred facts (tool-derived facts have no prefix).
- `explored` depth levels: `name`, `sig`, `body`, `edges`.

## 6. `impact.txt` format

```
## changed: a.Foo.Bar
callers: a.Handler.Serve:88, c.Job.Run:41
tests: a/foo_test.go TestBar
contracts: messages.jsonl golden
verdict: ok | issue <one line>
```

## 7. Roles and sections

| Role | Reads | Owns and writes |
|---|---|---|
| Master | memory (header, explored, changed, unknowns) | task phase and dispatch only |
| Explorer | memory | explored, files, risks, unknowns |
| Executor | memory | changed, updates to files it touched, unknowns |
| Reviewer | memory + impact | impact.txt, risks, closes unknowns |
| Decider | memory | decisions, answers to unknowns |

Each agent rewrites only its own sections. Others add an `unknown` instead of editing.

## 8. Workflow
1. Master creates the task dir and dispatches the explorer.
2. Explorer writes explored, files, risks, unknowns. Master snapshots.
3. Questions from any agent go to the decider, not the user. The decider writes `decisions` and closes the unknown.
4. Executor reads memory. If `explored` has a gap it requests targeted exploration instead of grepping. It implements, then updates file hashes and `changed`.
5. Reviewer uses `changed` as its work list, traces callers and tests, writes `impact.txt`, and re-checks files with stale hashes.
6. Master checks open unknowns and verdicts, then finishes or loops back to the executor.

## 9. Rules
1. Read the whole file first. Anything absent counts as unexplored.
2. Only the owner role edits its sections.
3. Hashes are computed by the tool, never by hand.
4. **Budget: `codemem.txt` must be at most 25,000 bytes**, the same as the `exec` output limit, so one `exec` call returns it untruncated. The tool rejects a write that exceeds it. `impact.txt` has its own 25,000-byte cap.
5. Prune order when over budget: `name`/`sig`-only entries, then entries not tied to a changed symbol or risk. Never drop risks, unknowns, `changed` or decisions.
6. Prune on every rewrite so the file shrinks as well as grows.
7. Snapshot at each phase boundary.
8. Prefer tool-derived facts (gonav). Mark LLM-inferred facts with `~`.

## 10. Tool decision

**Use the existing native memory tool (`native.memory`, to be called `codemem`). Do not build a new tool.**

Note: no tool or setting named `codemem` exists in the repo today. `native.memory` is defined in `sample/tools/native/memory.json` and `backend-go/internal/sessionfs/native_memory_settings.go`. `codemem` would be a new profile or toolset alias for it, or a rename. Confirm which.

How the design maps onto it:
- It stores bounded JSON-object records with `create`, `get`, `list`, `search`, `update` and `delete`, and returns an ETag. `if_match` gives safe concurrent updates.
- Record type `codemem`. One record per `(task, section)`, with data `{task, section, owner, rev, text}`. `text` uses the line format from section 5. Agents load a task by `list` with `filter {task: "<id>"}`. This replaces the `.task/<id>/` files.
- `max_record_bytes` is configurable per setting, so set it to 25,000 to enforce the per-record cap. The 25,000-byte total across sections is not enforced by the tool.
- It keeps source snippets out only by convention. Nothing in the tool stops an agent storing them.

What `native.memory` does not give us, and what to do about it:

| Gap | Handling |
|---|---|
| Section ownership | Prompt rule plus the `owner` field. `mem validate` can flag violations. |
| Total size cap across a task's records | Validate script, or a record limit per task. |
| File hash computation and stale detection | Small `exec` helper `mem rehash`. Agents never hash by hand. |
| Format validation | `mem validate` |
| Phase snapshots | Copy records with a `phase` field, or snapshot via the helper. |

So the helper shrinks to `rehash`, `validate` and `snapshot`. Storage and access stay in the native tool. Open point to verify before building: whether the existing state-context feature (`tools.native.memory.statecontext` selectors) can inject a task's records into context automatically. That would give the "load the full file" behavior without each agent calling `list`.

## 11. Problem / issue document

`codemem` stores code facts only, so the problem being solved needs its own document: `.task/<id>/issue.md`. Every agent reads it first. It is short, and the decider owns it.

```
# T-123 <title>
## problem
What is wrong or missing, in 2-3 sentences. Observed vs expected.
## scope
In: <packages, behaviors>. Out: <explicitly excluded>.
## acceptance
- <testable criterion>
## constraints
- <compat, golden outputs, config rules, performance>
## references
- <bug link, log path, failing test>
```

Rules:
- Only the decider edits it. Scope changes go through a `decisions` entry in memory plus an edit to `issue.md`.
- `acceptance` is what the reviewer and master check at the end.
- `codemem.txt` `unknowns` and `decisions` refer back to it by section name.
- Keep it under about 2 KB. Longer analysis goes in `report.md`.

## 12. Open decisions
- Where `decisions` live long-term (per task, or promoted to a project file).
- Whether `codemem` is an alias for `native.memory` or a rename.
- Whether state-context selectors can auto-load a task's records.
- Whether symbol extraction is wired into the helper (calls gonav and writes `files` records) or agents write records themselves. Recommend the former in v2.
- Whether "25K" means 25,000 or 25,600 bytes. This doc uses 25,000 as the safe value.

## 13. Build order
1. Fix the format. Configure `codemem` (record type, `max_record_bytes` 25,000). Write the `mem` helper with `rehash`, `validate`, `snapshot` only.
2. Pilot with explorer and executor on one real task. Measure tokens saved.
3. Add the reviewer and `impact.txt`.
4. Add the decider inbox and master dispatch logic.
