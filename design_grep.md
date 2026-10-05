# `grep` Tool Design

## 1. Purpose

`grep` is the native content-search operation. It finds lines matching a pattern across the workspace and returns them grouped by file, with line numbers that `read` and `edit` can consume directly.

The intended workflow is:

```text
grep a symbol or string  →  read the file around a match  →  edit it
```

It is a read-only tool: it never writes, and its result is bounded by policy so an exploratory search cannot flood the model context.

## 2. Tool selection

| Tool | Use for |
|---|---|
| `read` | Reading known paths, ranges, and directory listings. |
| `grep` | Finding which files and lines contain something, when the path is unknown. |
| `write` / `edit` | Changing content. |

`grep` overlaps neither `read` (which requires knowing the path) nor the mutating tools. A future filename-oriented tool (`glob`/`find`) would remain distinct: `grep` searches content, that tool lists paths.

## 3. Request input

| Field | Type / default | Meaning |
|---|---|---|
| `pattern` | string, required, minLength 1 | ripgrep regular expression (Rust syntax: no lookaround or backreferences). |
| `path` | string, default `.` | File or directory to search, workspace-relative or authorized absolute. Same resolution, symlink, and policy rules as `read`. |
| `include` | string | Only search files matching one glob (`*.ts`, `src/**/*.{ts,tsx}`). |
| `exclude` | string | Skip files matching one glob (`*.test.ts`, `{dist,vendor}/**`). Passed to ripgrep as `--glob '!<exclude>'` after `include`, so it wins when both match. A leading `!` and surrounding whitespace are stripped; a blank value is treated as absent. |
| `literal` | boolean, default `false` | Treat `pattern` as exact text instead of a regular expression. |
| `context` | integer 0-20, default 0 | Lines shown before and after each match. |

The smallest useful call is `grep({pattern})`.

Deliberately absent: pagination cursors, `caseSensitive` (ripgrep smart-case is the default: a pattern containing uppercase is case-sensitive, otherwise case-insensitive), `maxResults` (backend-bounded from config), `hidden`/`ignored` toggles, `filesOnly`, `json`, `multiline`, and any replacement mode.

## 4. Search semantics

* **Regex flavor**: ripgrep's Rust regex engine, so `\b`, `(?i)`, alternation, and character classes work while lookaround and backreferences are rejected. `literal: true` (`--fixed-strings`) covers text that contains regex characters; the description says so, because the model must know when to use it.
* **Case**: smart-case by default, no argument.
* **Ignore rules and hidden files**: traversal respects `.gitignore`, `.ignore`, and `.rgignore` when the searched tree is inside a git repository, and always skips hidden entries. Passing an explicit *file* searches it even when it is ignored or hidden; passing an explicit *directory* still applies the ignore rules to its contents. This is ripgrep's own behavior, verified by tests rather than re-implemented.
* **Binary files**: skipped during traversal. An explicitly requested binary file that matches is reported by ripgrep and yields a group header with no match lines; the file is not decoded.
* **Symlinks**: never followed (`--follow` is never passed), so a search cannot leave the allowed tree through a link.
* **Ordering**: `--sort path`, so result order is deterministic and a narrowed re-query is reproducible.
* **Determinism**: `--no-config` ignores any user `RIPGREP_CONFIG_PATH`, and `--color never` keeps ANSI escapes out of results.

## 5. Result

```text
{"id":"call_7","status":"ok","summary":"3 matches in 2 files for TODO"}

--- INFORMATION ---
path: .
pattern: TODO
include: *.ts
context: 1
files: 2
matches: 3
truncated: false
max_results: 200
max_content_bytes: 65536
max_matches_per_file: 20

--- CONTENT token=r_9f3c... encoding=utf8 ---
src/a.ts
1-alpha
2:beta TODO

src/b.ts
4:TODO now
--- END CONTENT token=r_9f3c... ---
```

* The payload is verbatim ripgrep-style output: a path heading followed by `line:text` matches and `line-text` context lines, blank line between files. Line numbers are 1-based and match `read`'s, and the text after `:` is the exact file content, so it can be pasted into `edit`'s `oldText`.
* The payload uses the shared CONTENT frame (randomized boundary token), so file content cannot forge the tool's metadata.
* `pattern` is echoed because it is small and identifies the call. `include`, `exclude`, `literal`, and `context` are echoed only when they were provided, because they change how the payload must be read (context lines use `-`).
* `files`/`matches` count what was returned. Zero matches is a success: `status: ok`, `matches: 0`, summary `No matches for <pattern>`, plus a `hint` that hidden files and files ignored by git are not searched.
* `per_file_limit_reached: true` appears when a file returned as many matches as `max_matches_per_file` allows, so a capped file is never mistaken for a complete one.

## 6. Truncation, limits, and no pagination

`grep` has no cursor and no page parameters. Content search is narrowed by re-querying (`pattern`, `path`, `include`), not by paging: a page of extra matches is rarely the agent's next decision, a sound cursor would require re-traversing or re-enumerating the tree with changed results between calls, and partial results that look complete are worse than an explicit cap.

Instead, limits are enforced by the backend and reported loudly:

| Limit | Config | Default | Effect |
|---|---|---|---|
| Matches returned | `max_results` | 200 | Stop reading output, kill ripgrep, `truncated: true`, `truncation_reason: max_results`, summary says `Showing the first N matches ...`, `hint` names the narrowing options. |
| Matches per file | `max_matches_per_file` | 20 | ripgrep `--max-count`, so one busy file cannot consume the whole budget; when any file reaches it, the result carries `per_file_limit_reached: true` and a hint to search that path directly. |
| Payload bytes | `max_bytes` | 64 KiB | Stop, `truncation_reason: max_content_bytes`. |
| Characters per line | `max_line_chars` | 240 | Long lines are cut when rendering, marked `[... truncated line]`; minified or single-line files cannot dominate the result. |
| File size searched | `max_file_bytes` | 8 MiB | ripgrep `--max-filesize`; larger files are not read at all. |
| Wall clock | `timeout_ms` | 10 s | The backend kills ripgrep; the partial result is returned as `ok` with `truncation_reason: timeout`. |

A timeout or byte cap is a successful partial answer, not an error, because the agent can act on what came back and narrow the next call.

## 7. Errors

| Status | `error_code` | When |
|---|---|---|
| `invalid_input` | `validation_failed` | Schema; includes `example_operation`. |
| `invalid_input` | `invalid_pattern` | ripgrep rejects the pattern (exit 2, `regex parse error`); `suggestion: use literal: true to search the exact text`. |
| `not_found` | `not_found` | Search path missing. |
| `forbidden` | `path_outside_workspace`, `path_denied` | Search root outside the workspace or denied by policy; matched files are also filtered so denied content never appears. |
| `error` | `ripgrep_unavailable` | The configured binary cannot be spawned; `suggestion` names `tools.native.grep.binary`. |
| `error` | `grep_failed` | Any other non-zero exit, with a bounded `system_error` line from stderr. |

## 8. Execution model and safety

1. The search root is resolved with `resolveExistingTarget`, i.e. the same canonicalization, symlink, and allow/deny policy as `read`; `tools.native.grep.paths` falls back to `tools.native.read.paths`.
2. ripgrep runs as a child process spawned with an argument array (never a shell), `cwd` set to the workspace root, `--` before the pattern and paths.
3. Output is consumed as JSON Lines (`--json`) and parsed incrementally, so limits apply while reading; non-UTF-8 match lines (`bytes`) are dropped rather than decoded.
4. Each file that matches is resolved through the same policy check before it is reported; denied files are dropped entirely and their paths never appear.
5. The child is killed on any limit (results, bytes, timeout); the timer is cleared on exit, and partial output already parsed is kept.
6. Nothing is written: `grep` has no side effect beyond running the search.

## 9. Configuration

```json
{
  "tools": {
    "native": {
      "grep": {
        "binary": "rg",
        "max_results": 200,
        "max_matches_per_file": 20,
        "max_bytes": 65536,
        "max_line_chars": 240,
        "max_file_bytes": 8388608,
        "timeout_ms": 10000
      }
    }
  }
}
```

* `binary` may be an absolute path or a `PATH` name; a missing binary is reported as `ripgrep_unavailable`, so the tool fails loudly instead of silently.
* `tools.native.grep.paths` defaults to `tools.native.read.paths`; overrides are normalized per session.
* As with the other native tools, `grep` is exposed only when `tools.native.grep` is configured **and** `native.grep` is present in `tool_definition_paths` (`sample/tools/native/grep.json`).

## 10. Implementation map

| File | Role |
|---|---|
| `backend/src/tools/native/ripgrep.ts` | Process and output layer: argument assembly helpers, JSON Lines parsing, grouped rendering, line/byte/result budgets, timeout and kill, error classification. |
| `backend/src/tools/native/grep.ts` | Definition, normalization, policy application, result metadata and summary. |
| `backend/src/tools/native/registry.ts` | `grepSpec` (id, enabling setting, required fields, example call). |
| `backend/src/config.ts` | `NativeGrepToolSettings` with read-path inheritance and validation. |
| `sample/tools/native/grep.json` | Definition document. |
| `sample/schemes/setting.schema.json` | Settings schema entry. |
| `backend/test/grep-tool.test.ts` | Tool tests (skipped when ripgrep is unavailable). |
| `backend/test/definitions.test.ts` | Asserts the shipped document matches the code definition. |

## 11. Tests

Minimal call across a fixture tree; ignore-file and hidden behavior including the explicit-path bypass; `include` filtering; `literal` search for regex metacharacters; context lines and the `-` separator; result truncation with reason and hint; per-file cap; line truncation; byte truncation; zero matches with hint; invalid pattern with suggestion; missing path; denied path (and that denied files never appear); path outside the workspace; unusable `binary`; timeout as a partial success; settings validation; definition drift.

## 12. Deliberate non-goals

* **No pagination or cursor** (see 6).
* **No `caseSensitive` argument**: smart-case covers the realistic cases; a wrong-case search is cheap to re-run.
* **No `hidden`/`ignored` toggles**: explicit paths already bypass ignore rules, which keeps the schema minimal.
* **No `filesOnly` mode**: grouped headings already reveal which files match; a filename tool is a separate intent.
* **No replacement, multiline, or `--follow`**: mutation belongs to `write`/`edit`, multiline regex costs a flag plus line-number ambiguity, and following symlinks breaks the policy guarantee.
* **No second pass for totals**: when the result is truncated, we do not re-run ripgrep with `-l` to count all matching files; the hint tells the agent how to narrow instead.

## 13. Open items

* `max_matches_per_file: 20` and `max_line_chars: 240` are first estimates; they should be revisited once real searches show whether files are being cut too aggressively.
* Tests skip when `rg` is not installed; CI without ripgrep therefore does not cover this tool.
* `timeout_ms` has no cancellation path from the session layer yet: a cancelled turn waits for the timeout before its child is killed.
* Ignore rules come from the searched tree's git repository: a workspace that is not a repository searches everything, so `.gitignore` files alone do not filter results.
