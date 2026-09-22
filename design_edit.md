# `edit` Tool Design

## 1. Purpose

`edit` is the native surgical text-mutation operation. It changes part of an existing UTF-8 text file without resending the whole file: the agent supplies the exact text it wants to replace and the replacement text.

The intended workflow is:

```text
read the target  →  copy the exact text as `oldText`
edit             →  the exact match is the precondition check
```

The operation never infers intent. It either finds the requested text exactly once (or every time, with `replaceAll`) and replaces it, or it refuses with a bounded diagnostic that is usually enough to fix the call without another read.

## 2. Tool selection

| Tool | Use for |
|---|---|
| `read` | Inspecting files, directories, ranges; the source of `oldText`. |
| `edit` | Changing part of one existing text file. |
| `write` | Creating files, replacing whole files, writing binary content (base64). |

The three tools do not overlap: `edit` cannot create a file (it returns `not_found` plus a "use write" suggestion), `write` never interprets its content as a search pattern, and binary content is out of scope for `edit`.

## 3. Request input

| Field | Type / default | Meaning |
|---|---|---|
| `path` | string, required | Workspace-relative or authorized absolute path. Same resolution, symlink, policy, and canonical-path rules as `write`. |
| `oldText` | string, required, minLength 1 | Exact text to replace, copied from a `read`. Must occur exactly once unless `replaceAll` is set. |
| `newText` | string, required, may be empty | Replacement text. Empty deletes the match. |
| `replaceAll` | boolean, default `false` | Replace every occurrence instead of requiring one. |
| `backup` | boolean, default `true` | Back up the previous content before replacing it. The session decides where backups are kept. |

Deliberately absent:

* line numbers, offsets, and `mode`/`operation` fields — the selected tool and the anchored text already express the intent;
* `contentEncoding` — `edit` is text-only (binary edits stay in `write` with base64);
* any file revision, hash, or checksum argument — a `read` returns `file_revision` as diagnostic metadata, never as an edit input.

The smallest useful call is `edit({path, oldText, newText})`.

Because deletion (`newText: ""`), insertion, prepending, and appending are all expressible with these two strings, there are no separate insert/delete operations.

## 4. Match semantics

1. **Exact substring match** against the file's current bytes, decoded as UTF-8.
2. **Line-ending normalization.** When the file has a single detectable style (LF, CRLF, or CR), `oldText` and `newText` are rewritten to that style first, so an agent that always emits `\n` can edit a CRLF file. A file with mixed or no line endings gets byte-exact matching only, and the style is reported.
3. **Occurrence rules.**
   * `0` occurrences → `not_found` / `no_match` with diagnostics (below).
   * exactly `1` occurrence, or `replaceAll: true` → replace.
   * more than `1` and no `replaceAll` → `error` / `ambiguous_match`, with the occurrence count and up to five occurrence line numbers. `edit` never picks a site.
4. **Literal replacement.** Occurrences are replaced by index arithmetic, never by a regular expression, so `$&`, `$1`, and friends keep their literal meaning.
5. **No-op detection.** If the resulting bytes equal the current bytes (for example `oldText === newText`, or a replacement that normalizes back to the same bytes), the result is `unchanged`: no backup, no write, and the inode and mtime are untouched.

### 4.1 Failure diagnostics

`no_match` is a `not_found` result carrying bounded, deterministic diagnostics instead of a guess:

```text
{"id":"edit-1","status":"not_found","summary":"No exact match in src/app.ts"}

--- INFORMATION ---
error_type: execution
error_code: no_match
path: src/app.ts
line_ending: lf
nearest_line: 1
matched_lines: 1
expected:   return 1;
actual:     return 1;
difference: whitespace
suggestion: re-read the file around nearest_line and use its exact text
```

* `nearest_line` is the first file line whose trimmed text equals the trimmed first line of `oldText`.
* `matched_lines` counts how many leading `oldText` lines matched the file exactly at that point.
* `expected` / `actual` are the first diverging pair, trimmed to one line of at most 200 characters.
* `difference: whitespace` appears only when the divergence is exclusively leading/trailing whitespace (the most common copy error).
* Without a candidate line the fields are omitted and the suggestion is "re-read the file and use its exact text".

## 5. Result

Success:

```text
{"id":"edit-1","status":"ok","summary":"Edited src/app.ts"}

--- INFORMATION ---
path: src/app.ts
action: edited
replacements: 1
bytes: 412
line_ending: lf
start_line: 42
end_line: 43
backup_path: backups/src/app.ts.bak-1789994906472-00f123bb126c656b
```

* `action` is `edited` or `unchanged`.
* `replacements` is how many occurrences were replaced.
* `bytes` is the new file size.
* `line_ending` is reported whenever the file has a detectable style.
* `start_line` / `end_line` are emitted only for a single replacement and describe the replaced region in the new content.
* `backup_path` is present only when a backup was created: workspace-relative when the backup lands inside the workspace (the default backup directory does), absolute otherwise.
* The summary adds `(N replacements)` when more than one occurrence changed.
* No content is echoed; the agent re-reads when it wants to verify.

## 6. Errors

| Status | `error_code` | When |
|---|---|---|
| `invalid_input` | `validation_failed` | Schema; includes a compact `example_operation`. |
| `not_found` | `not_found` | Target file missing (`suggestion: use write to create the file`). |
| `not_found` | `no_match` | File exists, `oldText` absent; diagnostics as in 4.1. |
| `error` | `ambiguous_match` | Repeated `oldText` without `replaceAll`. |
| `forbidden` | `path_outside_workspace`, `path_denied`, `symlink_forbidden`, `symlink_outside_workspace` | As `write`; the edit policy falls back to the write policy. |
| `error` | `target_is_directory`, `unsupported_file_type`, `invalid_path`, `parent_not_directory` | Path shape and target type, as `write`. |
| `unsupported_encoding` | `binary_file` | Not valid UTF-8 or contains a NUL byte (`suggestion: use write with contentEncoding "base64"`). |
| `blocked` | `size_limit` | File or resulting file exceeds `tools.native.edit.max_bytes`. |
| `error` | `changed_during_edit` | The file changed between the two stats around the read. |
| `error` | `backup_failed`, `write_failed`, `read_failed` | Filesystem failures; the original file is preserved. |

## 7. Atomicity, locking, and durability

`edit` shares the whole mutation path with `write` (`backend/src/tools/native/mutate.ts`), so both offer identical guarantees:

1. Path resolution and target inspection happen first and have no filesystem side effect, so a rejected call never creates, renames, or removes anything.
2. The read-modify-write window runs inside `pathLocks`, one `KeyedMutex` keyed by canonical path shared by every mutating native tool and session.
3. The file is read once, verified by two `stat` calls (`changed_during_edit` otherwise), and the replacement is computed from those bytes.
4. The previous content is copied to `<backup_dir>/<mirrored directory>/<file>.bak-<13-digit ms>-<16 hex>` (mode preserved) unless `backup` is false; the directory is created on demand and a failed backup aborts before the write.
5. New bytes are written to `<file>.tmp-<hex>` in the same directory, fsynced when the policy says so, then renamed over the target; the staging file is removed on failure.
6. Backups are pruned to the tool's own quota, never touching the backup that was just created, and only files matching this module's own name pattern.

## 8. Limits

* `tools.native.edit.max_bytes` (default 1 MiB) bounds both the file that is read and the file that results. The result size is estimated before allocating the new content, so an edit that would explode the file is rejected with `size_limit` before anything is built.
* Files above the limit are rejected before any read-modify-write step.

## 9. Configuration

```json
{
  "tools": {
    "native": {
      "write": {"backup_max_count": 5, "backup_max_total_bytes": 10485760, "backup_dir": "backups", "fsync": true},
      "edit": {"max_bytes": 1048576}
    }
  }
}
```

* `tools.native.edit.max_bytes` is edit-specific.
* `tools.native.edit.paths` falls back to `tools.native.write.paths` when omitted (one path policy for file mutation), and can be overridden per session.
* `tools.native.write.backup_dir` is absolute or relative to the session cwd and defaults to the session's own `backups` directory, so backups never litter the edited tree; `null` keeps them beside each file. The `tools.native.edit` values default to the write settings and can be overridden per tool.
* As with `read` and `write`, the tool is only exposed when `tools.native.edit` is configured **and** `native.edit` is present in `tool_definition_paths` (`sample/tools/native/edit.json`).

## 10. Implementation map

| File | Role |
|---|---|
| `backend/src/tools/native/edit.ts` | Definition, request normalization, match handling, result rendering. |
| `backend/src/tools/native/text.ts` | Pure helpers: UTF-8 decode, line-ending detection/normalization, occurrence counting, literal replacement, diagnostics. |
| `backend/src/tools/native/mutate.ts` | Shared mutation path: path locks, backups, quota pruning, atomic write (`commitFile`). |
| `backend/src/tools/native/path.ts` | Shared resolution/inspection (`resolveMutatePath`, `inspectMutateTarget`, `prepareMutateParent`). |
| `backend/src/tools/native/registry.ts` | Declares which native tools exist and how each loads. |
| `backend/src/tools/catalog.ts` | Spec-driven loading, validation, dispatch, schema errors. |
| `backend/src/config.ts` | `NativeEditToolSettings` with write inheritance; `NativeMutationSettings`. |
| `sample/tools/native/edit.json` | Definition document for sessions. |
| `backend/test/edit-tool.test.ts` | 17 tests covering the contract below. |

## 11. Tests

`backend/test/edit-tool.test.ts` covers: the smallest call and its line reporting; deletion, insertion, prepending, appending, multi-line replacement; CRLF/LF normalization and mixed-ending exactness; ambiguity and `replaceAll` (single- and multi-line occurrences); all `no_match` diagnostics; no-op inode/mtime preservation; missing file, directory, symlink, and escape rejection; policy inheritance and override; edit defaults inherited from write and edit's own backup overrides (`backup_max_count`, `backup_dir: null`); external targets mirrored under `@external`; binary refusal; pre-flight size limits; backup failure; shared lock and backup history with `write` (including a concurrent write+edit pair); a 500-line file; argument-schema and settings validation; and the required definition document.

## 12. Deliberate non-goals

* **No multi-hunk `edits: [...]` array.** The harness executes every function call of one assistant turn in a single provider round trip, so several `edit` calls in one turn already give multi-hunk editing without nesting, ordering rules, or overlap validation. Partial application is visible per call and recoverable with a read.
* **No line-number targeting.** Line numbers shift after every edit and a stale hint can silently address the wrong, identical-looking site. Exact text (with context when needed) is self-verifying.
* **No fuzzy matching.** Whitespace-insensitive matching risks writing to a site the agent did not intend; the diagnostics in 4.1 make the strict path cheap instead. An opt-in fuzzy mode remains possible if real usage shows repeated whitespace-only failures.
* **No patch/unified-diff input.** Diff headers and context markers are a common source of malformed tool calls and would need their own parser and diagnostics.
* **No binary editing, no create-on-missing, no revision argument.**

## 13. Open items

* `changed_during_edit` is implemented but not covered by a deterministic test: the window between the two `stat` calls is sub-millisecond, so triggering it reliably from a test would need an injected filesystem seam.
* CR-only files are normalized, but this is only exercised indirectly (the mixed/LF/CRLF paths are tested).
* `start_line`/`end_line` are omitted for multi-occurrence replacements; an agent that wants them can read the reported backup or the file.
* The definition exposes `oldText` with `minLength: 1`; an empty `oldText` is therefore a schema error rather than a whole-file replacement (that is `write`).
