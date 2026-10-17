# `glob` Tool Design

**Status: implemented.** Sections 1–12 describe the shipped design; §13 records the confirmed decisions and
where the implementation deviates from the original proposal. Implemented in
`backend/src/tools/native/glob.ts`, reusing the runner in `backend/src/tools/native/child.ts`, with 17 tests
in `backend/test/glob-tool.test.ts`.

## 1. Purpose

`glob` answers the one question no current tool can answer cheaply: **which files exist?** Given a name
pattern it returns the matching files, sorted and bounded, under the same path policy and ignore rules the
other tools use.

It replaces the two commands an agent most often fires into `bash` (`ls -R`, `find`) — which are unbounded,
unfiltered, and unavailable when `bash` is not enabled, as is the case in the live sessions today.

## 2. Tool selection

| Question | Tool |
| --- | --- |
| What is in this one directory? | `read` (listing) |
| Which files exist / match a name? | `glob` |
| Which files contain this text? | `grep` |
| What does this file say? | `read` |
| Build, test, git, pipelines | `bash` |

`read` on a directory stays as it is: one level, byte-bounded, `directory_entries_truncated`. `glob` is the
recursive, pattern-based counterpart — the `ls` versus `find` split. Neither tool grows a mode field for the
other's job.

## 3. Request input

Smallest useful call: `glob({pattern: "*.test.ts"})`, and `glob({})` for "every file I can see".

| Field | Type | Required | Notes |
| --- | --- | --- | --- |
| `pattern` | string | no | Glob matched against file names (or paths for patterns with `/`). Defaults to all files. |
| `path` | string | no | Directory to search, relative to the workspace root or absolute. Defaults to the workspace root. |

Normalization before validation: accept `glob` and `include` as aliases for `pattern` and `dir` for `path`
(near-miss spellings cost a retry otherwise); trim both fields; treat an empty `pattern` as "all files".

Deliberately **not** exposed: result limits, hidden-file handling, ignore rules, sorting, and the ripgrep
binary are backend policy (§6, §9). Exclusions are omitted in v1: a single pattern cannot express "all `.ts`
files outside `src/`", but the model can filter a bounded list, and adding a second glob parameter is cheap to
do later if it turns out to be needed.

## 4. Listing semantics

```text
resolve path (shared path policy, must be an existing directory)
→ run `rg --files --hidden --glob '!.git/**' --sort path -- <path>` inside the workspace root
→ match file names in process against the pattern
→ return the matches, bounded
```

Verified ripgrep behaviour this relies on:

* `--files` lists files without reading their contents, so `glob` never opens a file.
* Ignore rules apply: `.gitignore`, `.ignore`, and traversal rules hold **inside a git repository**, exactly
  like `grep` without an `include` glob. A tree that is not a repository has no ignore rules — the result then
  carries a hint, because `node_modules` and friends will show up.
* `--hidden` plus the built-in `!.git/**` exclusion makes `.env` and `.github/workflows/*` reachable by
  pattern, while `.git/**` stays out. An *exclusion* glob does not disable ignore rules (verified), which is
  why the exclusion is safe to always pass.
* Hidden files are therefore **included** by default, unlike `grep`. That difference is deliberate — content
  search over hidden files is rarely wanted, file discovery of hidden config files often is — and it is
  configurable (`include_hidden`, §9) for operators who prefer parity with `grep`.
* Output paths are relative to the workspace root, and a leading `./` is stripped, matching `grep` and `read`.

Pattern matching happens **in process**, not through ripgrep's `--glob`:

* A pattern without `/` matches the file name (`*.ts` finds files at any depth, like `grep`'s `include`).
* A pattern with `/` matches the workspace-relative path (`src/**/*.ts`).
* Supported syntax: `*` (within a path segment), `?`, `**` (across segments), and `{a,b}` alternatives; every
  other character is literal. Matching is case-sensitive and deterministic.
* A malformed pattern is treated literally rather than rejected, so a typo costs one imperfect result instead
  of a validation round trip.

Why not pass the pattern to ripgrep as `--glob`: an inclusion glob makes matching files **explicit**, which
overrides ignore rules and silently surfaces files git ignores. The shipped `grep` tool has exactly this
behaviour for `include` (verified), which `design_grep.md` does not state; §13 keeps that as a separate,
explicitly decided follow-up rather than something this tool copies.

## 5. Result

```text
{"id":"c1","status":"ok","summary":"12 files matching *.test.ts"}

--- INFORMATION ---
path: .
pattern: *.test.ts
files: 12
truncated: false
max_results: 500

--- CONTENT token=r_… encoding=utf8 ---
src/a.test.ts
src/sub/b.test.ts
--- END CONTENT token=r_… ---
```

* One workspace-relative path per line, sorted by path, so the model can copy a path straight into `read`,
  `edit`, or `grep`.
* `path: .` means the workspace root; the field is always present, so the scope of a listing is never guessed.
* No files → `status: ok`, `files: 0`, summary `No files matching <pattern>`, and the hint that ignore rules
  and a non-repository tree change what is visible.
* Cap reached → still `status: ok` with `truncated: true`, `truncation_reason` (`max_results`, `max_bytes`, or
  `timeout`), and a hint to narrow the pattern or path. No pagination cursor: a file list has no stable
  offset, and narrowing is one cheap call.
* Empty result sets produce no CONTENT frame, matching `read` and `grep`.

## 6. Limits

| Limit | Key | Default | Why |
| --- | --- | --- | --- |
| Listed files | `max_results` | 500 | Bounds the context cost of a broad pattern. |
| Payload bytes | `max_bytes` | 65536 | Bounds the frame even with very long paths. |
| Wall clock | `timeout_ms` | 10000 | A huge tree must not hold the turn open. |
| SIGTERM to SIGKILL | `kill_grace_ms` | 2000 | Wait before the process group is killed; 0 kills at once. |
| Wait after SIGKILL | `kill_deadline_ms` | 2000 | How long the call waits for ripgrep before it returns anyway. |
| Pipe drain after exit | `drain_after_exit_ms` | 500 | How long the pipes may stay open after ripgrep exited before the output is cut. |

Truncation and timeouts stay successful results: the listing is a snapshot, and a partial snapshot plus a
narrowing hint is more useful than an error. Only `timeout` also stops the child, which reuses the process
group handling in `child.ts` (SIGTERM, then SIGKILL after the configured grace).

## 7. Errors

| Condition | `status` | `error_code` |
| --- | --- | --- |
| ripgrep missing or not executable | `error` | `ripgrep_unavailable` (suggestion points at `tools.native.glob.binary`) |
| `path` missing | `not_found` | `not_found` |
| `path` is not a directory | `invalid_input` | `not_a_directory` |
| `path` outside the workspace | `forbidden` | `path_outside_workspace` |
| `path` denied by a rule | `forbidden` | `path_denied` |
| ripgrep exits with an unexpected status | `error` | `glob_failed` |

No `invalid_pattern` error exists by design (§4): patterns are matched leniently in process.

## 8. Safety

`glob` is a read-only operation:

* it reuses `resolveExistingTarget`, so the searched directory obeys the same allow/deny rules as `read` and
  `grep`, and its default `paths` inheritance is the **read** rules (unlike `bash`, which inherits writes);
* ripgrep runs without a shell, with `--no-config`, without following symlinks (ripgrep's default), and with
  `--files`, so no file content is read;
* output is bounded in files, bytes, and time, and the process group is killed on timeout;
* nothing outside the workspace becomes reachable, because the pattern filters names rather than widening
  traversal.

## 9. Configuration

```jsonc
"tools": {
  "native": {
    "glob": {
      "binary": "rg",              // PATH name or absolute path
      "include_hidden": true,      // false restores grep parity (hidden files skipped)
      "max_results": 500,
      "max_bytes": 65536,
      "max_line_chars": 1000,      // guards pathological path lengths
      "timeout_ms": 10000,
      "paths": []                  // defaults to the tools.native.read rules
    }
  }
}
```

Validation mirrors the other tools: `ConfigError` messages naming `tools.native.glob.<field>`, defaults
materialized at load, and a `glob` entry in `sample/schemes/setting.schema.json`. Unlike `grep`, there is no
`max_file_bytes` and no `max_matches_per_file`: no file is opened and no per-file count applies.

## 10. Implementation map

| Path | Role |
| --- | --- |
| `backend/src/tools/native/glob.ts` | Definition, argument normalization, `path` resolution, the in-process pattern matcher, ripgrep invocation, rendering, metadata, hints, error mapping. |
| `backend/src/tools/native/child.ts` | Reused unchanged for spawning ripgrep with a timeout and a process-group kill. |
| `backend/src/tools/native/registry.ts` | `globSpec` with `required: []`, `exampleOperation: {pattern: "*.test.ts"}`. |
| `backend/src/config.ts` | `NativeGlobToolSettings` + validation; `paths` inherited from `read`. |
| `sample/tools/native/glob.json`, `sample/schemes/setting.schema.json` | Definition document and settings schema entry. |
| `backend/test/glob-tool.test.ts`, `backend/test/tool-session.ts` | Tests plus `makeGlobToolSession`/`refreshGlobSession`. |
| `backend/test/definitions.test.ts`, `backend/package.json`, `backend/design.md` | Drift entry, test list, design bullets. |

The launcher is `rg --files --hidden --glob '!.git/**' --sort path -- <path>`; results are read line by line,
filtered by the pattern, and stopped once the caps are reached.

## 11. Tests

* `glob({pattern})` finds files at any depth, sorted, with `path: .`.
* `glob({})` lists every visible file.
* Path patterns (`src/**/*.ts`), `?`, and `{ts,tsx}` alternatives behave as documented; unknown syntax stays
  literal.
* **Ignore semantics:** in a git repository, `*.ts` does **not** return a gitignored `*.ts` file, while a
  non-repository tree with the same files does (plus the hint). This is the behaviour that separates `glob`
  from `grep`'s `include`.
* `--hidden` default: `.env` and `.github/workflows/ci.yml` are found, `.git/**` never appears;
  `include_hidden: false` hides them again.
* `path` scoping, plus `not_found`, `not_a_directory`, `path_outside_workspace`, and `path_denied`.
* No matches → `ok`, `files: 0`, hint, no CONTENT frame.
* `max_results` and `max_bytes` truncation with the narrowing hint; a tiny `timeout_ms` yields
  `truncation_reason: timeout` and still returns `ok`.
* Aliases (`glob`, `include`, `dir`) and empty-pattern handling.
* Settings validation and defaults, `paths` inherited from `read`, the configured-without-definition gate, and
  definition drift for `glob.json`.
* ripgrep absent → the suite skips, like the grep tests.

## 12. Deliberate non-goals

* No content search (that is `grep`), no file sizes, types, or modification times in the listing.
* No exclusion parameter, no pagination cursor, no `sort` control (always by path).
* No symlink following, no hidden/exclusion presets beyond the built-in `.git/**`.
* No directory-only or file-only filtering, no `path` globs spanning outside the workspace.
* No directory-tree rendering: `read` lists one level, `glob` lists files.

## 13. Confirmed decisions and implementation notes

All six open items were confirmed as recommended:

1. **Name:** `glob`; the schema also accepts the near-miss spellings `glob` and `include` for `pattern` and
   `dir` for `path`, normalized before validation.
2. **`pattern` is optional**, defaulting to every visible file.
3. **Hidden files are listed by default**, with `include_hidden: false` restoring grep parity.
4. **Patterns are matched in process**, never through ripgrep's `--glob`, so ignore rules stay intact.
5. **`grep`'s `include` override** is out of scope here: `design_grep.md` still describes the traversal
   behaviour without noting the override, and that documentation fix (or a shared-matcher fix) is a separate
   decision.
6. **No `exclude` parameter** in v1.

Where the implementation differs from the proposal, with the reason:

* **Output is parsed line by line, not with `--null`.** The shared runner strips control characters from child
  output — NUL included — so NUL-separated output collapses into a single line. Newline separation works and
  stderr text is dropped by its `rg: ` prefix instead; `--no-messages` keeps file-open errors quiet as well.
* **The zero-result hint points at `read`, not at an explicit path.** Passing a file as `path` is rejected
  (the tool takes a directory), and passing a directory does not bypass ignore rules, so the actionable advice
  is to read a known path directly.
* **`captureBytes` equals `max_bytes` with `headRatio: 1`.** A listing is meaningful from its start, so the
  tail window is unused, and a truncated listing never ends with a half path: the final fragment is dropped
  and the capture marker is filtered out.
* **The non-repository hint is emitted only when a listing is empty or truncated**, so a small successful
  listing is not cluttered with an explanation that only matters for large or surprising results.
* **Every listed path is re-checked against the path policy** before it is returned, exactly like `grep`'s
  matched files, so a denied subtree cannot leak through a broader listing.
* Verification: `npm test` passes with 130 tests, including ignore semantics inside a repository versus a plain
  tree, hidden-file defaults, symlink skipping, the result/byte caps, the timeout path, the `rg: ` stderr
  filter, and the settings validator.
