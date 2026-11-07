# Native `code_context` Tool Design

**Status:** Implemented (Go). Decisions and what was left out: `backend-go/DECISIONS.md`, "Native `code_context` tool".

## Goal

Explore the source for a task once. Save each useful finding as an exact source range with a description, and let every profile working on the task read those ranges back as code instead of searching again. When code only moves, the entry follows the identical bytes. When code changes, the tool says which entries need updating; it never computes a new range from edits.

Every call saves the model a repeated read or search:

| Repeated work | Avoided by |
|---|---|
| reading code, then a second call to save it | `read` prints and saves in one call |
| re-reading code that only moved | relocation by identical bytes |
| `grep` for something already found | `find` |
| one call per range saved, changed or read | `items` on `read` and `update`; `ids` on `drop` and `get` |

`code_context` is a plain native tool. It has no `subagent` or `profile_message` hook, and nothing runs between profiles: an entry's staleness is checked whenever `get`, `list` or `find` runs.

## Tasks

A task is one file of entries, named by its key (`T-023`). A call without arguments, or `list` without `task`, lists the tasks of the storage, most recently written first (at most `max_scan_tasks`):

```
code_context: 2 tasks, most recently written first; list one with task
T-023: 12 entries, generation 31, by executor, 2026-11-07T10:42:00Z
  Make the glob brace cap configurable.
T-019: 4 entries, generation 6, by reviewer, 2026-11-06T16:03:11Z
```

It reads only the task files, not the code, so it shows no staleness; `list` with `task` does. A task's description is set with `task_description` on `read` or `update`; `update` with only `task_description` sets it alone and may start a task with no entries. `list` and `get` of the task show it.

## Entries

An entry is one command, `read(path, start, end)`. The path is relative to the cwd, and the range is 1-based and inclusive. Each entry also has:

- a description,
- a priority,
- the SHA-256 of the whole file and of the range's exact bytes, and
- `updated_by` and the task generation of its last write (kept in the file, not printed).

A command returns only code: never a match list, symbol table or summary. A grep or symbol hit is discovery evidence. Turn it into the line range it points at before saving.

The description says what the code does, its inputs, outputs and constraints, and why it matters for this task. It is written for a reader who will not open the file. It describes behavior, not line numbers, so it is still true after the range moves.

Priority is a number from 1 to 6, and `get` leaves out the highest numbers first:

1. target
2. callers and callees
3. interfaces and implementations
4. data and persistence
5. tests
6. secondary

## Calls

```ts
code_context({action: "read", task: "T-023", path: "internal/tools/native/glob_match.go", start: 34, end: 63})  // prints the code and saves it
code_context({action: "read", task: "T-023", path: "internal/tools/native/glob_match.go", start: 34, end: 63, priority: 1,
              description: "expandGlobBraces expands a brace pattern ... so this is where the cap becomes configurable."})  // saves, no code printed
code_context({action: "get", task: "T-023"})                 // every entry: descriptions and current code
code_context({action: "get", task: "T-023", ids: [4, 7]})
code_context({action: "get", task: "T-023", ids: ["1-20"]})       // entries 1 to 20; ids and ranges mix: [3, "10-20"]
code_context({action: "find", task: "T-023", query: "glob cap"})
code_context({action: "list", task: "T-023"})                // one line per entry, no code, with stale and moved notes
code_context({action: "update", task: "T-023", id: 1, start: 36, end: 70})  // id kept; omitted fields kept
code_context({action: "drop", task: "T-023", ids: [2]})
code_context({action: "read", task: "T-023", path: "internal/tools/native/glob_match.go", priority: 2, items: [
              {start: 10, end: 20, description: "..."}, {start: 80, end: 95, description: "...", priority: 1}]})  // several in one call
code_context({action: "update", task: "T-023", items: [{id: 1, description: "..."}, {id: 3, start: 40, end: 52}]})
code_context({action: "read", task: "T-023", items: [{path: "a.go", start: 1, end: 30}, {path: "b.go", start: 5, end: 9}]})
code_context({action: "clear", task: "T-023"})
code_context({})                                             // the tasks, with their descriptions
code_context({action: "update", task: "T-023", task_description: "Make the glob brace cap configurable."})
```

- **`read`**: without a description, prints lines start..end and saves them; the entry shows `(no description)` until a `read` with a description or an `update` adds one. Lines an entry already holds unchanged are printed but not saved again. Reading the exact range of a stale entry refreshes that entry. Beyond `max_entries` the code is printed and not saved. With a description the caller has seen the code, so the range is saved, or the entry of that exact range is described, without printing it; beyond `max_entries` that is a `too_many_entries` error.
- **`update`**: re-reads the range, even when only the id is given, and stores the new hashes.
- **`find`**: matches the query's words, case-insensitive, against each entry's path and description. Entries matching every word are shown; if none does, those matching most. Each is checked against its file.
- **Ids**: an id is never reused after `drop`.
- **`items`**: `read` and `update` take up to `max_batch_items` operations in one call. Each item takes the fields of a single call, and a field given outside `items` (a shared `path` or `priority`) applies to every item that leaves it out. The items are applied in order in one write of the task file; one that fails is reported on its line and does not stop the others. When every item fails nothing is written and the answer is a tool error (`all_items_failed`, one line per item). `read` prints each item's block, and once the call's code reaches `max_output_bytes` the remaining items are saved without printing their code. `drop` and `get` already take `ids`; items that only hold an `id` are taken as those ids.
- **Requirements**: the schema holds only types; what an action needs (`drop` needs `ids`, `find` a `query`, `read` a `path`, `start` and `end`, `update` an `id`) is checked by the tool and refused with `missing_fields` or `invalid_field`.

## Output

Every answer is plain text that starts with one header line. An entry is one line, with its description after two spaces and its later lines indented:

```
code_context task T-023, generation 31: 12 entries, 1 relocated, 1 stale
task description: Make the glob brace cap configurable.
#3 p1 internal/tools/native/glob_match.go:34-63  expandGlobBraces caps brace expansion at 64 patterns;
  this is where the cap becomes configurable.
#5 p2 internal/tools/native/glob_match.go:80-95 [moved from 76-91]  matchGlob calls it once per pattern.
#7 p5 internal/tools/native/glob_match_test.go:10-40 [stale: the file changed and these lines differ from what was saved]  brace cases
```

`get` answers:

1. **Header:** the generation, counts of entries shown, relocated, stale and left out, and the task description.
2. **Code blocks:** one block per file region, merging current entries of one file whose ranges overlap or touch. A block shows every entry's line, then the exact code between `--- code <token> ---` and `--- end code <token> ---`. The token is random per call.
3. **Stale entries:** each entry's line with the reason, but no code. The caller re-reads the code and then updates or drops the entry.
4. **Left out:** entries over `max_output_bytes`, as `#id p<priority> path:start-end`.

`ids` of `get` and `drop` takes entry ids and ranges of ids, `"a-b"` with both included, so a large task is read in parts (`["1-20"]`, then `["21-40"]`). A range skips the ids no entry has (dropped ones); only a range that names no entry at all, or a single id with no entry, is reported as `no entry`.

## Staleness

Each distinct file is read once per `get`, `list` or `find`. An entry is current while its lines still hold the bytes it saved; another part of the file may have changed.

When they do not, the tool looks for the saved bytes elsewhere in the file: as many lines, the same length, the same first line and the same SHA-256. If they occur exactly once, the code only moved. The entry is relocated there and `get`, `list` and `find` save the move; the output says where it was. If they occur several times, or the new range is one another entry already reads, the entry is stale.

Otherwise it is stale:

- the file is gone, cannot be read, or is now over `max_file_bytes`,
- it has fewer lines than the range, or
- the range holds other bytes.

A reverted file makes its entries current again. Only the profile that changed the code fixes a range, with `update`. An entry nobody updates stays in the stale list on every later `get`.

## Storage and config

`tools.native.code_context`, opt-in through `sample/tools/native/code_context.json`. The file must match the in-code schema.

| Key | Default | Meaning |
|---|---|---|
| `storage_dir` | `.code_context` | one `<task>.json` per task. A relative path is under the profile's `tool_storage` (else the cwd). An absolute path shares one task between profiles. |
| `paths` | workspace only | read rules (`read_style`) |
| `max_entries` | 500 | entries per task |
| `max_description_chars` | 4000 | an entry's or a task's description |
| `max_range_lines` | 2000 | lines per entry |
| `max_file_bytes` | 4 MiB | largest file a range is read from |
| `max_output_bytes` | 400000 | code blocks of one `get` (about 100K tokens; a ceiling, not a target) |
| `max_storage_bytes` | 1 MiB | largest task file |
| `relocate` | true | move a range to the single place its saved bytes are now |
| `default_priority` | 6 | priority of a range `read` saves without one |
| `max_find_results` | 20 | entries one `find` shows |
| `max_scan_tasks` | 200 | tasks a call without `task` lists, most recent first |
| `max_batch_items` | 50 | items one `read` or `update` may hold |

Writes are atomic, and writes to the same task file are serialized within the process. Separate processes are not coordinated.

## Workflow it supports

1. **Discovery:** a discovery profile (Code Context) runs `find`, searches the code, `read`s the ranges it needs (saving them as it goes) and describes them in one `update` with `items`.
2. **Executor:** runs `list`, then `get` (all, or `ids`), implements, then `update`s the entries for the code it changed and `read`s new dependencies.
3. **Reviewer:** `get`s the task. Stale entries show which code changed; it fixes code directly and updates the entries the same way.

How much code the tool printed, and how much reading it saved, is measured from the transcripts: every call and its result are in `messages.jsonl`, and so are the reads and greps made with `exec`, which the tool cannot see.
