# Run Checkpoint (Undo) Design

**Status:** Proposed. Nothing is implemented. Facts below are marked **read** (seen in code), **measured** (run in a scratch directory on 2026-10-06) or **untested**.

## 1. Goal

Let a user return the workspace to the state it had before an agent run changed it, including changes made by `bash` and `process`, which the existing per-file backups do not cover.

## 2. What exists today (read)

* `write` and `edit` always copy the previous content to `<file>.bak-<ms>-<hex>` before replacing it (`write.ts:162`, `edit.ts:312`; Go: `edit.go:213`). Defaults: 5 backups and 10 MiB per file (`config.ts:710-711`); `backup_dir` can collect them in one directory.
* Nothing restores a backup. No tool, endpoint or UI action does it.
* Backups are per file. Many edits to one file evict the oldest backup, and there is no "everything this run touched".
* `bash` and `process` changes (`sed -i`, `rm`, formatters, `git checkout`) have no backup.

Checkpoints do not replace these backups. They cover what the backups cannot.

## 3. Approach: shadow git directory

A bare git directory outside the workspace is used with the workspace as work tree and a **temporary index file**. The user's own repository state (index, stash, refs, HEAD) is never touched.

Measured in a scratch repo:

* Snapshot = `GIT_INDEX_FILE=<tmp> git add -A` + `write-tree` + `commit-tree` + `update-ref`. After it, `git status --short`, `git stash list` and `git write-tree` of the user's repo were identical to before.
* The snapshot contains tracked files and untracked files that are not ignored (including a dirty tracked file and an untracked file), file modes (755 kept) and symlinks.
* Restore = `read-tree` into a temporary index + `checkout-index -a -f` + removing files that are untracked, not ignored and absent from the snapshot. It restored edited files, deleted files, a deleted symlink and a mode, and removed a file created afterwards.
* **Ignored files are not restored.** A changed `build/out.js` (ignored) stayed changed. This is a limit, not a bug: ignored paths are outside the snapshot.
* On this repository (679 files after ignore rules; `node_modules` and `.jad/` excluded because `.gitignore` covers them): one full snapshot took 0.51 s and the shadow directory was 45 MB. Incremental snapshots were **not measured**.

Why not `git stash create` or a commit in the user's repo: it needs a git repo, pollutes the user's refs, and uses the user's index.

## 4. Behaviour

1. **When:** lazily, immediately before the first call of a run that can change files: any `exec` call, a call whose tool says it may change files (`MayChangeFiles`: a sub-agent that may write, `profile_message`, an `async` stop), and any MCP tool unless `include_mcp_tools` is off. A call plan mode refuses takes none. A read-only run costs nothing.
2. **One checkpoint per run** (per `turn`). Subsequent mutating calls in the same run do not snapshot again.
3. **At run end** a second snapshot records the end state (see safe restore below).
4. **Restore** (user-initiated only, never by the model):
   * refused while the session has a live run (409);
   * refused at once (409, "session profile is already changing") while another restore, compaction or reset of the same profile is running; it is never queued behind it;
   * `dry_run` returns the path lists (restore, delete, conflicts) without changing anything;
   * takes a **pre-restore checkpoint** first, so a restore is itself undoable;
   * **safe restore (recommended default):** only paths whose current content equals the run-end snapshot are reverted. Paths the user changed after the run are reported as conflicts and left alone. This needs the run-end snapshot; it is **untested**.
   * **A run cut short.** If the server is killed between the first snapshot and the run's end, the record stays `open` with no `after_tree`. The list reports such a record as `interrupted` (with a warning) whenever no run of the session is live; while a run is live it stays `open`. This is derived at read time from the run state and is never written: the checkpoint service is built per request and cannot know which runs are live, and a write from the list route could race with a run that has just begun. A `pre_restore` checkpoint left open (a restore killed halfway) is reported the same way. Only a full restore is offered for an interrupted record, because a safe restore needs the run-end snapshot. The stored `checkpoints.json` keeps `open`, so a script that reads the file directly still sees it.
5. **Not covered** (state this in the UI): files outside `cwd` (`write` may allow `/tmp`, `bash` may allow `/`), ignored files, git history changes made by `bash` (commits, branch moves; `.git` is not snapshotted), network and database side effects, and anything a background `process` changes after the run ends.
6. **Interaction with `.bak` files:** backups created in the workspace are untracked, non-ignored files. A restore would delete the ones created after the checkpoint, and the next snapshot would include older ones. Whether that is acceptable, or `backup_dir` should be required, is open. **Untested.**

## 5. Storage

* Shadow git directory: `<session_dir>/staging/checkpoints.git`, plus metadata in a new cache file `checkpoints.json` in the same directory (id, turn id, created_at, file count, byte size, state). Both paths go through the existing `paths` profile mechanism like the other staging files.
* `messages.jsonl` is **not** written. Checkpoints are listed by a new endpoint, not by transcript records. This keeps the golden contract; prove it with the `messages-golden-diff` skill.
* Pruning: keep at most `max_checkpoints` per session; `git gc` is not run automatically.

## 6. Configuration (all defaults; nothing hardcoded)

A new top-level `checkpoint` node in the session setting, with schema entry in `sample/schemes/setting.schema.json` and config tests in both backends:

| Key | Default | Meaning |
|---|---|---|
| `enabled` | `false` | Master switch. Off changes no behaviour. |
| `git_binary` | `"git"` | PATH name or absolute path, like `grep.binary`. |
| `max_checkpoints` | 20 | Per session; oldest pruned first. |
| `max_files` | 20000 | Snapshot is skipped, with a warning result, above this. |
| `max_total_bytes` | 512 MiB | Same, for the shadow directory. |
| `include_mcp_tools` | `true` | Treat MCP calls as mutating. |
| `snapshot_on` | `"first_mutation"` | or `"run_start"`. |
| `safe_restore` | `true` | Requires the run-end snapshot. |

Exact defaults need measurement on a larger workspace before they are fixed. `max_files` and `max_total_bytes` above are placeholders.

## 7. API (new)

* `GET /api/sessions/{id}/checkpoints` : list, `{enabled, restoring, checkpoints[]}`. A record's `state` is `open` (a run is live), `complete`, `skipped`, `failed`, or `interrupted` (it was left open and no run is live; see section 4). `restoring` is true while a real restore is rewriting the workspace and false otherwise; a dry run never sets it. It exists because a restore runs on the server and goes on after the client disconnects, so a page that reloads mid-restore needs a way to see that; the frontend then disables every Restore control and re-reads the list until it is false.
* `POST /api/sessions/{id}/checkpoints/{checkpoint_id}/restore` with `{dry_run?: boolean}` : returns `{restored[], deleted[], conflicts[]}`.
* Both require the API token like every other `/api` route. Errors: 404 unknown id, 409 live run, 422 when the workspace has no readable git binary.

## 8. Failure policy

* A failed or skipped snapshot **does not block the tool call** (a checkpoint is a safety net, not a gate). The run records a `checkpoint_failed` warning that the UI shows, and the tool result is unchanged.
* Workspace not a git repo: works (the shadow directory does not need one), but ignore rules then come only from `.gitignore` files that exist in the tree. Without any, `node_modules`-sized trees would be snapshotted; the `max_files` guard is the only protection. **Untested.**

## 9. Both backends

TypeScript is the reference, Go the port. Go needs the same config node (`internal/sessionfs`), the same cache file, endpoints in `internal/httpapi`, and a hook in `runner_tools.go` before the batch. Add differential-harness scenarios for list, restore dry run and 409. Go code was **not read** for this design beyond the places named above.

## 10. Tests

* Unit: snapshot/restore round trip, deletion of new files, symlink and mode, ignored files untouched, conflict detection, pre-restore checkpoint, pruning, limit guards, missing git binary.
* Both backends: config validation, schema, HTTP status codes.
* `messages.jsonl` unchanged with the feature off and on (golden diff).
* Frontend: checkpoint list and restore dialog.

## 11. Open decisions

1. `snapshot_on` default: `first_mutation` (recommended) or `run_start`.
2. Safe restore as the default (recommended) or full restore.
3. Require `backup_dir` when checkpoints are on, to keep `.bak` files out of the snapshot.
4. Whether MCP tools count as mutating by default (recommended: yes).
5. Whether the UI offers restore for old checkpoints or only the latest.
