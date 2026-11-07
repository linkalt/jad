# Native `code_context` Tool Design

**Status:** Implemented (Go). Decisions and what was left out: `backend-go/DECISIONS.md`, "Native `code_context` tool".

## Goal

Explore the source for a task once. Save each useful finding as an exact source range with a description, and let every profile working on the task read those ranges back as code instead of searching again. When code changes, the tool says which entries need updating; it never guesses a new range.

`code_context` is a plain native tool. It has no `subagent` or `profile_message` hook, and nothing runs between profiles: an entry's staleness is checked whenever `get` or `list` runs.

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
code_context({action: "add", task: "T-023", path: "internal/tools/native/glob_match.go", start: 34, end: 63, priority: 1,
              description: "expandGlobBraces expands a brace pattern ... so this is where the cap becomes configurable."})
code_context({action: "get", task: "T-023"})                 // every entry: descriptions and current code
code_context({action: "get", task: "T-023", ids: [4, 7]})
code_context({action: "list", task: "T-023"})                // YAML, no code, with stale flags
code_context({action: "update", task: "T-023", id: 1, start: 36, end: 70})  // id kept; omitted fields kept
code_context({action: "drop", task: "T-023", ids: [2]})
code_context({action: "clear", task: "T-023"})
```

- **`add`**: adding an identical path and range returns the existing entry.
- **`update`**: re-reads the range, even when only the id is given, and stores the new hashes.
- **Ids**: an id is never reused after `drop`.

## Output

`list`, `add` and `update` answer YAML. Each entry shows `id`, `priority`, `stale` (and `stale_reason`), `command`, `description`, `updated_by`, `generation` and the two hashes.

`get` answers plain text in four parts:

1. **Header:** the generation, and counts of entries shown, stale and left out.
2. **Code blocks:** one block per file region, merging current entries of one file whose ranges overlap or touch. A block shows every entry's description, then the exact code between `--- code <token> ---` and `--- end code <token> ---`. The token is random per call.
3. **Stale entries:** each with its id, path, old range, reason and description, but no code. The caller re-reads the code and then updates or drops the entry.
4. **Left out:** entries over `max_output_bytes`, by id.

## Staleness

Each distinct file is read once per `get` or `list`. An entry is current while its lines still hold the bytes it saved; another part of the file may have changed. Otherwise it is stale:

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
| `max_description_chars` | 4000 | |
| `max_range_lines` | 2000 | lines per entry |
| `max_file_bytes` | 4 MiB | largest file a range is read from |
| `max_output_bytes` | 400000 | code blocks of one `get` (about 100K tokens; a ceiling, not a target) |
| `max_storage_bytes` | 1 MiB | largest task file |

Writes are atomic, and writes to the same task file are serialized within the process. Separate processes are not coordinated.

## Workflow it supports

1. **Discovery:** a discovery profile (Code Context) searches the code and `add`s ranges.
2. **Executor:** `get`s the task, implements, then `update`s the entries for the code it changed and `add`s new dependencies.
3. **Reviewer:** `get`s the task. Stale entries show which code moved; it fixes code directly and updates the entries the same way.
