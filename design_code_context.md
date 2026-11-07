# Native `code_context` Tool Design

**Status:** Implemented (Go). Decisions and what was left out: `backend-go/DECISIONS.md`, "Native `code_context` tool".

## Goal

Explore the source for a task once. Save each useful finding as an exact source range with a description, and let every profile working on the task read those ranges back as code instead of searching again. When code only moves, the entry follows the identical bytes. When code changes, the tool says which entries need updating; it never computes a new range from edits.

Every call saves the model a repeated read or search:

| Repeated work | Avoided by |
|---|---|
| reading code, then a second call to save it | `read` prints and saves in one call |
| re-reading code that only moved | relocation by identical bytes |
| a second `get` printing what the caller already has | `get` with `since` |
| `grep` for something already found | `find`, also across tasks |
| describing the same code again in a new task | `read` reuses a matching description; `add` with `from_task` |
| printing every line when a map is enough | `mode: "outline"`, `max_priority` |
| one call per range saved, changed or read | `items` on `read`, `add` and `update`; `ids` on `drop` and `get` |

`code_context` is a plain native tool. It has no `subagent` or `profile_message` hook, and nothing runs between profiles: an entry's staleness is checked whenever `get` or `list` runs.

## Tasks

A task is one file of entries, named by its key (`T-023`). A call without arguments, or `list` without `task`, lists the tasks of the storage, most recently written first (at most `max_scan_tasks`):

```yaml
# code_context: 2 tasks; list one with task
tasks:
  - task: "T-023"
    description: |-
      Make the glob brace cap configurable.
    entries: 12
    generation: 31
    last_updated_by: "executor"
    updated_at: "2026-11-07T10:42:00Z"
```

It reads only the task files, not the code, so it shows no staleness; `list` with `task` does. A task's description is set with `task_description` on `read`, `add` or `update`; `update` with only `task_description` sets it alone and may start a task with no entries. `list` and `get` of the task show it.

## Entries

An entry is one command, `read(path, start, end)`. The path is relative to the cwd, and the range is 1-based and inclusive. Each entry also has:

- a description,
- a priority,
- the SHA-256 of the whole file and of the range's exact bytes,
- `updated_by`, and
- the task generation of its last write.

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
code_context({action: "add", task: "T-023", path: "internal/tools/native/glob_match.go", start: 34, end: 63, priority: 1,
              description: "expandGlobBraces expands a brace pattern ... so this is where the cap becomes configurable."})
code_context({action: "get", task: "T-023"})                 // every entry: descriptions and current code, and a since token
code_context({action: "get", task: "T-023", since: "3fa9c01b2e7d"})  // only what changed since that get
code_context({action: "get", task: "T-023", mode: "outline", max_priority: 2})  // descriptions and ranges, no code
code_context({action: "get", task: "T-023", ids: [4, 7]})
code_context({action: "find", task: "T-023", query: "glob cap", all_tasks: true})
code_context({action: "add", task: "T-023", from_task: "T-010", id: 2})  // copy another task's entry
code_context({action: "list", task: "T-023"})                // YAML, no code, with stale flags and stats
code_context({action: "update", task: "T-023", id: 1, start: 36, end: 70})  // id kept; omitted fields kept
code_context({action: "drop", task: "T-023", ids: [2]})
code_context({action: "add", task: "T-023", path: "internal/tools/native/glob_match.go", priority: 2, items: [
              {start: 10, end: 20, description: "..."}, {start: 80, end: 95, description: "...", priority: 1}]})  // several in one call
code_context({action: "update", task: "T-023", items: [{id: 1, description: "..."}, {id: 3, start: 40, end: 52}]})
code_context({action: "read", task: "T-023", items: [{path: "a.go", start: 1, end: 30}, {path: "b.go", start: 5, end: 9}]})
code_context({action: "clear", task: "T-023"})
code_context({})                                             // the tasks, with their descriptions
code_context({action: "update", task: "T-023", task_description: "Make the glob brace cap configurable."})
```

- **`read`**: prints lines start..end and saves them. Description and priority are optional; a range saved without a description shows as `undescribed`, and `update` adds one later. Without a description, `read` takes the description of another task's entry of the same path whose saved bytes are the bytes just read. Lines an entry already holds unchanged are printed but not saved again. Reading the exact range of a stale entry refreshes that entry. Beyond `max_entries` the code is printed and not saved.
- **`add`**: adding an identical path and range returns the existing entry. With `from_task` and `id`, it copies that task's entry (range, description and priority, unless given) once its code is checked unchanged or found moved; a stale source is refused.
- **`update`**: re-reads the range, even when only the id is given, and stores the new hashes.
- **`find`**: matches the query's words, case-insensitive, against each entry's path and description. Entries matching every word are shown; if none does, those matching most. With `all_tasks`, the other tasks of the storage are searched too, and their entries are shown only while their code is unchanged.
- **Ids**: an id is never reused after `drop`.
- **`items`**: `read`, `add` and `update` take up to `max_batch_items` operations in one call. Each item takes the fields of a single call, and a field given outside `items` (a shared `path` or `priority`) applies to every item that leaves it out. Items run in order and each is saved on its own, so one that fails is reported on its line and does not stop or undo the others; only when every item fails is the answer a tool error (`all_items_failed`, one line per item). `add` and `update` answer one YAML document of the entries saved, headed by a comment line per item; `read` prints each item's block, and once the call's code reaches `max_output_bytes` the remaining items are saved without printing their code. `drop` and `get` already take `ids`; items that only hold an `id` are taken as those ids.

## Output

`list`, `add` and `update` answer YAML. Each entry shows `id`, `priority`, `stale` (and `stale_reason`), `command`, `description`, `updated_by`, `generation` and the two hashes.

`list` also shows the task's stats.

`get` answers plain text in five parts:

1. **Header:** the generation; counts of entries shown, unchanged since `since`, relocated, stale and left out; the code bytes printed and not printed again; and a new `since` token.
2. **Code blocks:** one block per file region, merging current entries of one file whose ranges overlap or touch. A block shows every entry's description, then the exact code between `--- code <token> ---` and `--- end code <token> ---`. The token is random per call.
3. **Unchanged entries:** with `since`, the entries whose path, code and description are as that get printed them, by id and current range. Code that only moved counts as unchanged.
4. **Stale entries:** each with its id, path, old range, reason and description, but no code. The caller re-reads the code and then updates or drops the entry.
5. **Left out:** entries over `max_output_bytes`, by id.

With `mode: "outline"`, `get` prints the header and every entry's range, status and description, and no code.

## Staleness

Each distinct file is read once per `get`, `list` or `find`. An entry is current while its lines still hold the bytes it saved; another part of the file may have changed.

When they do not, the tool looks for the saved bytes elsewhere in the file: as many lines, the same length, the same first line and the same SHA-256. If they occur exactly once, the code only moved. The entry is relocated there and `get` and `list` save the move; the output says where it was. If they occur several times, or the new range is one another entry already reads, the entry is stale.

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
| `max_storage_bytes` | 1 MiB | largest task file; also bounds its state file |
| `relocate` | true | move a range to the single place its saved bytes are now |
| `default_priority` | 6 | priority of a range `read` saves without one |
| `max_deliveries` | 20 | `since` tokens kept per task, oldest forgotten first |
| `max_find_results` | 20 | entries one `find` shows |
| `max_scan_tasks` | 200 | other task files `find` (`all_tasks`) and `read` go through, and tasks a call without `task` lists; most recent first |
| `max_batch_items` | 50 | items one `read`, `add` or `update` may hold |

The `since` deliveries and the stats are in `<storage_dir>/.state/<task>.json`, apart from the entries. `clear` removes both.

Writes are atomic, and writes to the same task file are serialized within the process. Separate processes are not coordinated.

## Workflow it supports

1. **Discovery:** a discovery profile (Code Context) first runs `find` with `all_tasks` and copies entries that still hold. Then it searches the code, `read`s the ranges it needs (saving them as it goes) and describes them in one `update` per entry.
2. **Executor:** runs `get` (or `mode: "outline"`, then `get` with `ids`), implements, then `update`s the entries for the code it changed and `read`s new dependencies. A second `get` passes `since`.
3. **Reviewer:** `get`s the task. Stale entries show which code changed; it fixes code directly and updates the entries the same way.

The stats in `list` show how much code was printed, how much was not repeated, and how often a `read` hit lines already saved. The tool cannot see reads and greps made with `exec`; measuring those still needs the transcripts.
