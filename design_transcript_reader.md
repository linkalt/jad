# Design: transcript reader tool (`transcript`)

Status: design only, nothing implemented. Items marked **[unverified]** were not confirmed in code or by a run.

## 0. Goal
A native tool that lets an agent read `messages.jsonl` of its own session, of another session, or of another profile's transcript, as bounded text. Listed in missing.md ("messages.jsonl transcript reader tool"). No native bridge, flat input, text output, capped.

## 1. How messages.jsonl is stored today (read from code)
- **Location (layout sessions):** `<layout.State>/<session>/<category>/<name>/messages.jsonl` (sessionfs/layout_load.go `storageEntry`, `profileStateDir`). `Location.ProfileMessagesPath("<category>/<name>")` returns it without loading anything. Non-layout sessions: `<dir>/messages.jsonl` or `<dir>/profiles/<profile>/messages.jsonl` (storage_paths.go `selectProfileStorageDir`); `messages` can be overridden by a setting path. Sibling files: `messages.backup.jsonl`, `staging/` (per-turn items, caches, `statecontext.jsonl`), `debug/http` (HTTP debug, can hold raw request data).
- **Format:** one JSON object per LF-terminated line. Two kinds:
  - input items: `{"type":"message","role":"user","content":[...],"__agent_session_input_batch_id":..,"__agent_session_input_batch_index":..}`, also `function_call_output` and other input item types (batch fields are internal and stripped by `expandTranscriptRecord`);
  - `{"type":"response.completed","session_turn":N,"session_step":M,"response":{id,model,usage,output:[...]}}` where `output` holds assistant `message`, `reasoning`, `function_call` (`name`, `call_id`, `arguments` as a JSON string) items. Lines are large (the sample line carried a full tool_smith script in `arguments`).
  - Tool results are separate input lines (`function_call_output`, matched by `call_id`) **[unverified: exact field names of tool-result lines; read a real file when implementing]**.
- **Writers:** one `transcript.Journal` per transcript, mutex `j.mu`, `O_APPEND`, optional fsync (`DurabilityStrict`). Doc on `NewTurnStore`: the journal must not be shared with another process. A turn stages in `staging/<turn>/` and is appended on commit (`response.completed`), so a finished turn appears as one line.
- **Rotation/compaction:** no rotation. `CompactHistory` copies the file to `messages.backup.jsonl` (replace/append mode) and **atomically replaces** the transcript with `os.Rename` (maintenance.go:653); `Reset` removes it. Byte offsets are therefore valid only until the next compaction/reset; line numbers shift too.
- **Hazard for a reader:** `Journal.ReadRecordsRange` calls `withRepairedFile`, which opens `O_RDWR` and **truncates an incomplete final line** (`RepairTail`). A reader of a transcript another run is writing must not use it. The reader needs its own read-only scanner: open `O_RDONLY`, scan complete LF-terminated lines up to the size seen at open, ignore a trailing partial line, never write. Rename-replace is safe for an already open fd (reader sees the old file whole).

## 2. Tool input (flat)
Tool name `transcript`. Unknown or old keys fail with an error naming the key.

| key | type | meaning |
|---|---|---|
| `profile` | string, optional | `<category>/<name>`; with no selector at all, the caller's own profile |
| `session` | string, optional | session reference; only with `profile` (default: the caller's session) |
| `subagent` | string, optional | sub-agent run id `sub_xxxxxxxx` (see 2a) |
| `path` | string, optional | a transcript file by path; only when `allow_path` is true (see 2a) |
| `source` | `"messages"` (default) \| `"backup"` | live transcript or `messages.backup.jsonl` |
| `turns` | string, optional | user-turn range, `"3"`, `"3-7"`, `"-5"` (last 5), `"12-"`. Turn = a user input line plus the assistant lines up to the next user line (same notion as `countUserTurns`) |
| `tail` | int, optional | last N turns; shorthand for `turns:"-N"`; mutually exclusive with `turns` and `cursor` |
| `role` | string, optional | `user`, `assistant`, `tool` (call + result), `reasoning` (default excluded) |
| `tool` | string/glob, optional | only calls/results of tools with this name (`read`, `exec*`) |
| `search` | string, optional | case-insensitive substring in the rendered text of an entry; matching entries only, with `context` neighbours |
| `context` | int, optional | entries around each search hit (default 0) |
| `full` | bool, optional | do not truncate payloads per entry (still bound by `response_max_bytes`) |
| `cursor` | string, optional | opaque cursor from the previous page |
| `list` | bool, optional | list readable sessions/profiles with turn counts and size instead of reading |

### 2a. Selectors (option C, decided by the user)
Flat and mutually exclusive: exactly one of `profile` (with optional `session`), `subagent` or `path`; none means the caller's own profile. Two given is an error that names both (`give only one of profile, subagent, path: got profile and path`). `session` without `profile` is an error naming `profile`.

- **`path`:** off unless `allow_path` is true (error names `tools.native.transcript.allow_path`). The path must pass the same path policy as a `read` of that file (the tool reuses the read tool's `paths`, as `read_image` does, plus the call's `PathScope`), symlinks are resolved before the check, the file must be a regular file whose first record parses as a transcript record (a JSON object with `type`), and it is read by the same read-only scanner with the same caps.
- **`subagent` (verified in code):** a sub-agent's transcript is `<subagent.storage_dir>/<call-id>-<hex>/messages.jsonl` (app/subagent.go:196-209). The subagent tool result does **not** always carry that path: only a stopped, timed-out or lost run returns `transcript` (subagent_call.go:198, `partialInformation`), and only runs that are still going or ended abnormally show their `run` id; a plain finished synchronous result carries neither. A handle (`block_ms` expired) ends with `subagentRunRef` = that same path (subagent_async.go:85). The run id is always resolvable in the manager (`SubagentManager.byID`, manifests persist ids and `Dir` across restarts). So `subagent` resolves the id through the manager, not through a path the model must copy: the run must be owned by the caller's session key, or any run when the caller is the session itself and `allow_children` permits. The file is then read by the same scanner. An id without a transcript yet (queued) fails with `no_transcript_yet`.

Selecting by entry offset is not offered: offsets die at compaction. Turn numbers use `session_turn` where present, else the counted user turns **[unverified: whether `session_turn` stays stable across compaction; if not, number turns from the file start and say so in the output header]**.

**Cursor:** encodes `{file identity (size at start + sha256 of first 4 KiB), next turn, next entry}`. If the file identity no longer matches (compaction/reset/other file) the call fails with `cursor_stale: transcript changed, repeat without cursor`. Appends do not invalidate it (the prefix hash covers the head only).

## 3. Output
Plain text, one header line then entries, nothing in JSON:

    transcript researcher/web @ session 20260101-ab12  turns 41-44 of 120  (page 1, next: cursor=...)
    #41 user: <text>
    #41 assistant: <text>
    #41 call read {"path":"a.go"}  [call_00_x]
    #41 result [call_00_x] <first N bytes> ... [truncated 18432 bytes]

- Entries are rendered from the expanded records (reuse `expandTranscriptRecord` for `response.completed`; skip batch fields, `usage`, response metadata, encrypted reasoning). Reasoning is hidden by default; `role:reasoning` shows the summary text only.
- **Truncation:** per entry text/arguments/result capped at `entry_max_bytes` (head 70 % + tail 30 %, with `[truncated N bytes]`); `full:true` lifts the per-entry cap only. The whole response stops at `response_max_bytes` (same pattern as other native tools); on stop the header carries the `cursor` to continue. Image/base64 payloads (`data:` URLs, `input_image`) are replaced by `[image <mime> N bytes]`.
- A result whose `call_id` is matched to a call is shown next to it; an orphan is shown on its own.
- Large `function_call.arguments` (e.g. whole scripts) are cut like any other payload and flagged.
- `list`: one line per readable `<session>  <category>/<name>  turns  bytes  mtime  running?`, capped at `list_max_entries` with a note when cut.

## 4. Access rules
- **Who may read what.** Container is the boundary; approvals do not exist. Proposal: *default allow every session and profile the server's session roots resolve* (`sessionfs.Resolve(roots, reference)` + `ProfileMessagesPath`), since profile_message already reaches other profiles. Restrict with config (`allow_other_sessions`, below) rather than prompts.
- **Path scope.** The tool takes no filesystem path, so `paths allow/deny` does not apply to its input. But the transcript files live under `layout.State`, which a scoped read could be denied: a profile whose `paths` deny the state dir should not read transcripts through a side door. Rule: the resolved transcript path must pass the same `scopedPolicy(policy, execution)` check a `read` of that file would (`path_scope.go`, `withinAny`). A profile that cannot `read` the state dir cannot use `transcript` on others; its own transcript stays readable only if it passes the same check. **[unverified: whether state dir is inside the default allow set of profiles; check before choosing "deny by default for foreign sessions if not in scope"]**.
- **Per-owner async scoping.** Calls use the tool's async handle (`block_ms`) like other native tools; the handle is owned by the caller's `SessionKey`, unchanged. The reader holds no state besides the cursor, which carries nothing secret.
- **Sub-agent children.** A child (`AgentDepth>0`) reads with the child's `PathScope` (`narrowScope`). Default for `session`/`profile` in a child = the *session's* profile transcript (the parent's), which the root run is writing; that is the intended use ("what did the parent decide?"). Config can restrict children to own-session only. A child's own transcript is not a messages.jsonl (child runs have their own `messages.jsonl` under the sub-agent store, **[unverified: app/subagent.go:141 mentions it]**); the `subagent` selector (2a) now covers it.
- **Reading while the target runs.** Allowed; reader sees the committed prefix only (staged turns are in `staging/`, never read). The tool states `target running` in the header if the run registry knows it **[unverified: needs a port to `RunningProfileRunChain`-like registry; can be dropped]**.
- **Leaks.** The transcript is the provider request history, so it holds everything the model saw: tool outputs (file contents, env dumps, `fetch` bodies, tokens a command printed) and user input. It does *not* hold API keys of the model credentials or the auth file: those are loaded by the run after input is stored and sent as headers (`resolver.go:384` comment) **[unverified: grep a real transcript and the HTTP debug path for `Authorization`]**. Never read `debug/http` and `staging/` (the tool only opens the one `messages.jsonl`/`messages.backup.jsonl` file, resolved by code, no user path). Secrets a tool printed earlier can reach another profile; this is inherent to the feature and is the reason for the `allow_other_sessions` switch. No redaction is attempted (would give false assurance).
- **Symlinks:** the resolved path is `EvalSymlinks`-checked to stay under the session state root; a symlinked messages.jsonl pointing outside is refused.

## 5. Configuration: `tools.native.transcript` (new `transcript.json` sample)
Capped, unknown keys rejected, in sample/schemes + setting.schema.json + config tests (as every native tool).

| key | default | cap | note |
|---|---|---|---|
| `enabled` | false | | opt-in, like other tools |
| `response_max_bytes` | 65536 | 4 MiB | whole response |
| `entry_max_bytes` | 4000 | response_max_bytes | per payload |
| `max_turns_per_call` | 50 | 1000 | upper bound for one page |
| `scan_max_bytes` | 256 MiB | 4 GiB | most bytes scanned per call; fail `too_large: use turns/tail/cursor` |
| `list_max_entries` | 200 | 5000 | |
| `allow_other_sessions` | true | | false: own session (any of its profiles) only |
| `allow_other_profiles` | true | | false: own profile only |
| `allow_children` | true | | children may read at all |
| `allow_path` | false | | the `path` selector; off unless true |
| `line_max_bytes` | 16 MiB | 256 MiB | longest line the scanner accepts; a longer line is skipped and counted |
| `default_timeout_ms` / `max_timeout_ms` | 30000 / 21600000 | 6 h | same as the other async-capable tools |

`tail` and the last page need to find the last N user turns without scanning from the start: read backwards in 64 KiB blocks, counting user-input lines, bounded by `scan_max_bytes`. Forward ranges scan from the start (a per-file line index is not cached in v1).

## 6. Where it lives
- `tools/native/transcript.go` (+ `transcript_render.go`, `transcript_scan.go`): schema, arg parsing, scan, render, caps. No import of `app`.
- A small port in native (like `ProfileMessenger`): `TranscriptSource.Resolve(ctx, ownerKey, session, profile, source) (path string, running bool, err)`, implemented in `app/transcript_source.go` using `Resolver.roots`, `Location.ProfileMessagesPath`, `LoadSessionSettings` for non-layout sessions (`config.MessagesPath`). Native side opens and scans the file read-only.
- `native_transcript_settings.go` in sessionfs for config (pattern of `native_exec_settings.go`); registered in the catalog; async via existing `async_tool.go` pattern.

## 7. Tests
1. Scanner on fixtures: user/assistant/call/result pairing, orphan result, reasoning hidden, images replaced, `turns` forms (`3`, `3-7`, `-5`, `12-`), `tail`, `role`, `tool` glob, `search`+`context`.
2. Truncation: per-entry head/tail, response cap, cursor continues exactly where it stopped (concatenated pages == one unbounded read); stale cursor after compaction/reset fails.
3. Concurrency: reader never writes (assert file bytes + mtime unchanged after reading a file with a partial final line; **fails if the Journal repair path is used**); append during pagination; compaction (rename) during a read; run with `-race`.
4. Access: other session/profile allowed/denied by config; path scope denial (policy denying state dir); child scope; symlinked messages.jsonl outside root refused; no path input accepted (unknown key `path` rejected naming it); only the two fixed files can be opened.
5. Settings: defaults, caps (timeouts ≤ 6 h), unknown key errors, schema + frozen fixture case count.
6. Async: `block_ms` handle returns the same text; handle owner scoping (another session cannot collect it); goroutine-leak test (scan cancelled by ctx stops; no goroutine left).
7. Real-file smoke: scan a copy of a real multi-MB transcript, check page sizes and timing (benchmark in the style of `jsonl_benchmark_test.go`).
Tests that must fail on revert: read-only (no truncate), response cap, stale cursor, scope denial, `allow_other_*`, symlink refusal.

## 8. Phases (one commit each)
1. Settings + schema + sample + fixture; tool skeleton with `list` only for the caller's own session (this design file is committed here).
2. Read-only scanner, turn/tail ranges, rendering, truncation, caps, cursor.
3. Filters (`role`, `tool`, `search`, `context`), `source:backup`.
4. Selectors: `profile`/`session` through the port, `subagent` through the sub-agent manager, `path`; mutual exclusion errors; access config, path-scope check, children.
5. Async/handle integration, leak tests, DECISIONS.md, docs, missing.md entry removed.

## 8a. As built (phases 1-4)
- Selectors, access switches and the read path rules are as in 2a and 4. `subagent` is resolved by run id through `SubagentManager.RunTranscript`; the subagent tool result does not carry the path for a finished run, so the id is the key. A run whose child has no transcript folder yet answers `no_transcript_yet`.
- Not built: `list` of other sessions and profiles (it lists the caller's own files only), the `running` flag in the header, a line index cache. Hand check on six real transcripts (18 KB to 1.5 MB): 0.6 to 16 ms per call.
- The header names turn numbers: `turns A-B; the transcript holds turns C-D`, plus a compaction note when C is above 1.
- `scan_max_bytes` applies to the whole file because turn numbers need one pass over it; the pass reads each line once and keeps only offsets.

## 9. Open questions (with recommendation)
1. **Default for foreign sessions: allow or deny?** Recommend allow (container is the boundary, profile_message already crosses profiles) with the three `allow_*` switches and the path-scope check.
2. **Redaction of secrets in output?** Recommend no; document that transcripts contain whatever tools printed. A regex redactor gives false safety.
3. **Reasoning entries?** Recommend hidden by default; summaries only via `role:reasoning`. Encrypted content never shown.
4. **Turn numbering across compaction.** Recommend report `session_turn` when present and a header note when the first visible turn is not 1 (compacted); need to verify stability.
5. **Sub-agent run transcripts** are in v1 through the `subagent` selector (user decision, option C). Staged partial turns stay out of scope.
6. **Line index cache for fast forward ranges.** Recommend no in v1 (scan cap protects); revisit if benchmarks on multi-hundred-MB files are slow.
7. **Include `messages.backup.jsonl`?** Recommend yes via `source:"backup"` (it holds pre-compaction history, the main reason to look into a compacted session).
8. **One tool or two (`transcript` + `transcript_list`)?** Recommend one tool with `list:true` (same flat-input style as exec listing).
9. **Name.** `transcript` vs `history`; recommend `transcript` (matches the package and missing.md).
