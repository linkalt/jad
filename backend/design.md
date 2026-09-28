# Agent Harness Persistence Design

**Status:** Implemented (initial)
**Scope:** OpenAI Responses API response streaming and session persistence
**Implementation target:** TypeScript under `backend/`

## 1. Purpose

The system `setting.json` establishes the global JAD data root as the directory containing that file. The default workspace stores each session below:

```text
<global-jad-root>/.jad/sessions/<session-id>/
├── messages.jsonl
├── model.json
├── setting.json
├── tools/                  # optional tool definition documents
└── staging/
```

The default layout is configurable per session through `setting.json.paths`: `messages`, `model`, `staging`, and `http_debug`. Relative values resolve from the directory containing that session's `setting.json`; absolute values are used as-is. When omitted, the paths above retain the default layout shown here.

This document defines the first version of the response persistence protocol. The primary goals are:

- preserve the OpenAI Responses API response shape;
- commit only complete responses to `messages.jsonl`;
- bound memory by the largest active output item rather than total response size;
- recover safely after process, stream, or filesystem failures;
- keep the persisted response suitable for deterministic local replay and cache-key generation.

## 2. Core decision

`messages.jsonl` stores one compact JSON event for each committed model response:

```json
{"type":"response.completed","session_turn":3,"session_step":2,"response":{"id":"resp_123","status":"completed","model":"gpt-...","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Hello"}]}]}}
```

The exact fields inside `response` are those supplied by the Responses API. The example is illustrative; fields must not be invented or removed except that `response.output` is assembled from staged completed output items.

The following events are **not** written to `messages.jsonl`:

- `response.output_text.delta`;
- `response.content_part.added`;
- `response.output_item.added`;
- `response.output_item.done`;
- other intermediate streaming events.

Intermediate events are transport state. A provider `response.incomplete` event is also committed through the replay-compatible `response.completed` envelope, while preserving its `response.status` and `incomplete_details`; completed output items, including function calls, must not be discarded merely because the provider stopped at an output limit.

### 2.1 Input transcript decision

The same `messages.jsonl` file also stores request-history input items as raw Responses API input items, one object per line and without a harness envelope:

```json
{"type":"message","role":"user","content":[{"type":"input_text","text":"Find the answer"}]}
{"type":"function_call_output","call_id":"call_123","output":"answer"}
```

The transcript therefore has two canonical line categories:

1. raw input items such as `message` and `function_call_output`;
2. completed model events with the exact `response.completed` shape.

The history reader yields raw input items directly and expands each committed `response.completed.response.output` array into input items for the next request. This preserves output order and avoids a custom cache-relevant wrapper. `model.json`, `setting.json`, and tool definition documents remain separate configuration sources.

### 2.2 Conversation compaction

Compaction maintains the active `compaction-cache.json` plus an atomically published `.backup` copy used to recover a missing or invalid active cache; it does not keep an unbounded checkpoint history. If both copies are unavailable while the backup transcript differs from the live transcript, request-history reads fail visibly rather than silently omitting compacted context. `paths.messages_backup` stores the pre-compaction `messages.jsonl` (replaced by default, or appended when `compaction.backup_mode` is `append`). `paths.compaction_cache` relocates the compaction cache. Unless explicitly overridden, compaction, history, usage, cursor, and statecontext files live under the resolved `paths.staging` directory. For named path profiles, the default messages backup lives beside that profile's messages file; flat configurations retain the session-level backup default. Relative paths resolve from `setting.json`.

Named path profiles use `paths.active_profile` and a `paths.profiles` map. The stored-default profile continues to inherit flat `paths` storage entries for compatibility; other profiles do not inherit those flat storage fields. Unless explicitly set, the stored-default profile uses its existing session-root fallbacks, while other profiles resolve messages, staging, caches, and debug paths under `profiles/<profile-name>`. Profile-specific `messages` or `staging` entries determine the directory used for that profile's remaining defaults. Cache files follow the resolved staging directory, and each profile's default transcript backup sits beside its messages file. When the API changes the stored default, it pins both profiles' resolved storage paths into their profile entries so their paths remain stable. Identical resolved storage paths across profiles are rejected, keeping transcripts and caches independent. A profile may set `models` to model-configuration JSON paths (relative to `setting.json`, or absolute) and `model_default` to one of those paths; if only `model_default` is set it is the sole choice, and if only `models` is set its first path is the default. Selecting an entry loads that file's provider, credentials configuration, request defaults, and API model; the per-session path selection is persisted in the browser. `paths.model` and `paths.profiles.<name>.model` are not supported. Profiles without `models` or `model_default` continue using the session-level `model.json`. Example:

```json
{
  "paths": {
    "active_profile": "profile1",
    "profiles": {
      "profile1": {
        "messages": "profiles/profile1/messages.jsonl",
        "messages_backup": "profiles/profile1/messages.backup.jsonl",
        "compaction_cache": "profiles/profile1/staging/compaction-cache.json",
        "history_cache": "profiles/profile1/staging/history-cache.json",
        "usage_cache": "profiles/profile1/staging/usage-cache.json",
        "statecontext": "profiles/profile1/staging/statecontext.json",
        "model_default": "chatgpt/model.json",
        "models": ["chatgpt/model.json", "openai/model.json"],
        "staging": "profiles/profile1/staging",
        "http_debug": "profiles/profile1/debug/http"
      }
    }
  }
}
```

Automatic compaction is evaluated after each completed provider response stream using `response.usage.input_tokens` and `compaction.trigger_tokens`. If the response contains tool calls, compaction removes older history while retaining that just-completed response as an active transcript tail; tool execution and matching outputs can then continue without orphaning calls. The active calls are not part of the compacted cache. Manual compaction is available at `POST /api/sessions/{id}/compact`; the session run lock queues it until active streaming/tool rounds finish. A compaction that would not reduce serialized request history is skipped to avoid repeated no-op compactions.

Compaction first backs up the active transcript, atomically publishes a cache derived from the compactable request history, then atomically rewrites `messages.jsonl` to the preserved active tool-call response (or empties it when there is no in-flight call) and invalidates `history-cache.json`. The cache records the source transcript hash and active tail so startup can recover a crash after publishing the cache but before rewriting the transcript. Future requests are assembled as the configured prompt, compaction-cache items, then history reconstructed from the active transcript.

`compaction.keepbatch.turn.keep` retains the latest user turns. `assistant_final` retains each selected user message and the last assistant message from the latest completed response without function calls (reasoning is omitted); `full` retains user/assistant messages and reasoning while always dropping tool calls/results. `compaction.keepbatch.tools` optionally retains the last N complete tool call/result pairs in `assistant_final` mode; its default is zero. Unmatched calls/results are never retained.

### 2.3 Usage summary cache

`usage-cache.json` is an atomic, incremental cache separate from `history-cache.json`. It stores cumulative token totals, the latest completed response's usage and turn/step, counters, and a transcript prefix checkpoint. Cached input tokens are shown separately but are included in input-token totals. Normal appends validate the checkpoint and process only the newly appended transcript bytes; a compaction advances the checkpoint without discarding accumulated usage. Session listings read the cached summary directly rather than re-aggregating usage from `messages.jsonl`; active snapshots refresh the cache incrementally through the session store. A missing/invalid cache or an unrecognized transcript rewrite triggers a rebuild from the transcript and available backup; ordinary appends process only new bytes. If compaction may have erased responses before a rebuild, `history_partial` signals that totals may be incomplete. `session_turn` counts submitted turns and `session_step` distinguishes model responses within one turn; older records without metadata receive best-effort positions during cache reconstruction. SSE `usage` events are emitted after each committed provider response and cache refresh, before tool execution or the next response round; committed `response.incomplete` events are included, with any absent usage fields left absent. Failed or uncommitted streams do not emit a response-level usage update.

`statecontext.enabled` opts into tool-provided state snapshots. Currently `trigger: ["compaction"]` refreshes the snapshot after successful manual or automatic history compaction. With automatic compaction that preserves tool calls, refresh happens after their results are appended. The latest snapshot replaces the previous file at `paths.statecontext` (default `staging/statecontext.json`) and remains available on subsequent requests until the next successful trigger. It is appended after compacted history and active transcript/tool results, not before the cache-stable prompt; the snapshot is not written into the transcript or compaction cache. Its configured `role` is `system` or `developer`, and `max_bytes` bounds the rendered state data.

Only `task`, `process`, and `memory` currently provide state context. Their options live with their native-tool settings: `tools.native.task.statecontext` chooses task statuses and a limit; `tools.native.process.statecontext` chooses process states and a limit; `tools.native.memory.statecontext` is a list of dynamic `{type, filter, exclude_any, limit}` selectors. Memory `filter` fields use exact-match AND semantics. `exclude_any` is a list of non-empty exact-match clauses: fields within each clause use AND, while clauses use OR. For example, `exclude_any: [{"status": "closed"}, {"status": "fixed"}]` omits either status without enumerating the statuses to keep. Filter and exclusion fields share the configured field/depth limits. Memory snapshots use read-only listing (no cursor creation). Rendered values are explicitly labeled as untrusted data and the model is told not to follow instructions or policy embedded in them. Example:

```json
{
  "statecontext": {"enabled": true, "trigger": ["compaction"], "tools": ["task", "process", "memory"], "role": "developer"},
  "paths": {"statecontext": "staging/statecontext.json"},
  "tools": {"native": {
    "task": {"statecontext": {"statuses": ["in-progress", "done"], "limit": 25}},
    "process": {"statecontext": {"states": ["running"], "limit": 20}},
    "memory": {"statecontext": [{"type": "bug_report", "exclude_any": [{"status": "closed"}, {"status": "fixed"}], "limit": 10}]}
  }}
}
```

### 2.3 Configuration loading

`src/config.ts` loads and validates the session configuration. JSON Schema definitions are kept in `sample/schemes/model.schema.json` and `sample/schemes/setting.schema.json`:

- `model.json` is required and contains `provider`, `model`, optional `base_url`, an `api_key_env` name, and request-generation options;
- `setting.json` is optional and defaults to the session directory name, the global JAD root as an absolute `cwd`, and an empty system prompt;
- API keys are never read from JSON files;
- `setting.json` is re-read from disk, so an edit applies to the next turn without restarting the backend; compiled source changes are not, and need a rebuild plus restart;
- `cwd` must be an absolute existing directory; relative working directories are rejected;
- unknown fields are preserved for forward compatibility, except under `tools.native.<tool>`, where an unsupported key (a singular `path` beside `paths`, or a `tools.native.mcp` block) is refused at load time with a suggestion;
- normalized model and setting objects receive canonical SHA-256 hashes;
- `setting.json.paths` optionally relocates the transcript, transcript backup, compaction cache, model file, staging directory, and HTTP debug directory; relative paths resolve from the session `setting.json` directory and absolute paths are used as-is; a leading `~` expands to the process home directory in `cwd`, these paths, `tools.native.<tool>.storage_dir`, `tools.native.write|edit.backup_dir`, and native path rules;
- `setting.json.http_debug` is disabled by default and controls native request/response diagnostics;
- enabled diagnostics write numbered JSON records to the configured `paths.http_debug` directory, defaulting to `debug/http/request1`, `debug/http/response1`, and subsequent request/response files;
- API-key and authorization headers are redacted, and bodies are opt-in and capped by `max_body_bytes`;
- `tool_definition_paths` selects definition documents relative to the global JAD root; absolute paths are used as-is;
- a tool must also be present in the per-session `tools` policy to be exposed, so omitted tools are disabled;
- native `read` accepts request paths relative to `cwd` or as absolute paths;
- `tools.native.read.paths` accepts either normalized `{action: "allow"|"deny", path}` rules or the `{allow: [...], deny: [...]}` object whose entries are path strings; relative rule paths resolve from the global JAD root. Paths inside `cwd` are always allowed, paths outside `cwd` require an allow rule, deny rules take precedence, and an object form with an empty `allow` list lifts the cwd restriction entirely (the tool may then reach any path the process can);
- native `read` uses `max_bytes` as its output-content cap and defaults its optional `limit` to 250;
- native `read` supports `includeContent: false` for stat-only file metadata without loading content;
- native `read` chooses the representation itself: UTF-8 text when the bytes decode, base64 otherwise (reported as `encoding: base64` with `encoding_reason: invalid_utf8`); an explicit `contentEncoding` is honoured, and asking for `utf8` on binary content returns `unsupported_encoding` with `error_code: binary_file` and a suggestion;
- native `read` takes `offset` as a 1-based line number, or a 0-based byte offset with `rangeType: "bytes"`; values below the start clamp to the start and set `offset_clamped: true` instead of failing validation, and out-of-range reads still report a no-content EOF summary;
- `contentEncoding` and `rangeType` accept near-miss spellings (`utf-8`, `UTF8`, `Lines`) that are normalized before validation, and validation failures include a compact `example_operation`;
- `tools.native.read.max_file_bytes` (64 MiB by default, at most 1 GiB) bounds the file that will be loaded; larger files return `blocked` with `error_code: size_limit` and a suggestion, while `includeContent: false` metadata stays available for them;
- a directory read is an ordinary `status: ok` result with `kind: directory`, `entry_count`, and one `entry` line per item, so successful outcomes share one status vocabulary;
- native `grep` searches contents with ripgrep: `pattern` (required), optional `path`, `include` glob, `literal`, and `context`; results are grouped per file as `path` then `line:text` (matches) and `line-text` (context), inside the shared CONTENT frame, and line numbers match `read`'s;
- native `grep` runs ripgrep without a shell using `--json --no-config --sort path --max-count --max-filesize --` (no symlink following, no user config), parses JSON Lines incrementally, and kills the child on any limit;
- `tools.native.grep` bounds a search: `max_results` (200), `max_matches_per_file` (20), `max_bytes` (64 KiB), `max_line_chars` (240), `max_file_bytes` (8 MiB), `timeout_ms` (10 s), and `binary` (rg); truncation stays a successful result with `truncated`, `truncation_reason` (max_results, max_content_bytes, timeout), and a narrowing `hint`, and there is deliberately no pagination cursor;
- native `grep` reports `per_file_limit_reached: true` when any file hits its per-file cap, treats zero matches as `status: ok` with `matches: 0` and a hint about hidden/ignored files, and filters every matched file through the same path policy as `read` (`tools.native.grep.paths` defaults to the read rules); malformed patterns return `invalid_input` with `invalid_pattern` and a `literal` suggestion, and a missing ripgrep returns `ripgrep_unavailable`;
- native `bash` runs one shell command per call in a resolved working directory: `command` (required), optional `cwd` and `timeout_ms`, with no shell state carried between calls, so the result always reports the directory the command ran in;
- native `bash` spawns the shell in its own process group with stdin closed and `detached: true`, SIGTERMs the group on timeout, escalates to SIGKILL after `kill_grace_ms`, and by default signals leftover descendants when the command finishes, so backgrounded children cannot outlive the call;
- native `bash` always returns: after SIGKILL a fixed kill deadline (2 s) resolves the call even if the process never reacts (uninterruptible I/O), reporting `left_running: true` and leaving that process to the operating system, and a straggler that keeps the pipes open is bounded by a 500 ms drain window after the leader exits;
- native `bash` guarantees apply to the process group: a command that moves itself into a new group (`setsid`) or a new session escapes the group kill, which is the documented limit of the mechanism (a cgroup would be needed to contain it);
- native `bash` child environment = backend environment minus the harness credential (`model.api_key_env`, always removed) and `tools.native.bash.env_deny`, plus `tools.native.bash.env` additions (`NO_COLOR`, `TERM=dumb`, `GIT_TERMINAL_PROMPT=0`, pager settings); this keeps `bash: env` from printing the harness API key while leaving `PATH`, `HOME`, and toolchain tokens intact;
- native `bash` bounds output per stream with four numbers that together fit `max_bytes` (stdout 16 KiB head / 32 KiB tail, stderr 4 KiB head / 12 KiB tail by default), cuts lines past `max_line_chars`, and strips ANSI escapes and control characters; stdout and stderr are kept in separate windows and rendered as labeled sections only when both wrote (`content_stream` reports `stdout`, `stderr`, or `mixed`), so a chatty stdout cannot evict stderr; truncation stays `status: ok` with `truncated`, `truncation_reason` (`max_bytes` or `timeout`), per-stream produced/shown/omitted byte counts, and a marker line, while `binary_output: true` flags output that is not valid text;
- native `bash` tees each stream to a spill file while it is read, because the middle of a truncated stream exists nowhere else: `spill.storage_dir` selects a cwd-relative directory and the tool generates a distinct per-call/per-stream filename; `truncated_only` deletes a file whose output the model saw in full, `cap_bytes` stops the write and reports `stdout_full_capped`/`stderr_full_capped`, a write failure reports `spill_error` without changing the command's result, expired generated files are pruned by `spill.retention_days` without touching unrelated files, and an unreadable target is a configuration error unless `spill.allow_unreadable` is set. Deprecated `spill.stdout_full`/`stderr_full` templates remain temporarily supported for existing settings and preserve their template-based behavior;
- native `bash` replaces `capture_bytes` and `head_ratio` with the per-stream budgets and refuses a session that still sets either, because one shared window let either stream evict the other;
- native `bash` reports `status: error` for a timeout (`timeout`), a non-zero exit (`nonzero_exit`, with the full output still returned and a hint for 126/127), a signal (`signaled`), an unusable shell (`shell_unavailable`), and spawn failures (`spawn_failed`); `cwd` problems reuse the shared path policy errors (`not_found`, `not_a_directory`, `path_outside_workspace`, `path_denied`);
- native `bash` is not a sandbox: `tools.native.bash.paths` (defaulting to the write rules) only selects the starting directory, and the command itself runs with the backend user's rights, which is why the tool stays opt-in and its model-facing description promises no containment;
- native `glob` lists files by name pattern (`pattern` optional, plus optional `path`) and is the recursive counterpart of `read`'s one-level directory listing: patterns without a slash match a file name at any depth, patterns with a slash match the workspace-relative path, `*`/`?`/`**`/`{a,b}` are supported, and malformed patterns stay literal instead of failing validation;
- native `glob` runs ripgrep as `--files --no-config --no-messages --sort path` (plus `--hidden` unless `include_hidden` is false, and a built-in `!.git/**` exclusion), so it never reads file contents, never follows symlinks, and inherits the ignore semantics of the searched tree: `.gitignore` applies inside a git repository, while a plain tree carries a hint that ignore rules do not apply;
- native `glob` matches patterns in process rather than through ripgrep's `--glob`, because an inclusion glob makes matching files explicit and silently overrides ignore rules (the behaviour `tools.native.grep.include` still has); the harness therefore never lists a file that git ignores because of a filter;
- native `glob` bounds a listing with `max_results` (500), `max_bytes` (64 KiB, read from the start of the output), `max_line_chars` (1000), and `timeout_ms` (10 s); truncation and timeouts stay `status: ok` with `truncated`, `truncation_reason` (`max_results`, `max_bytes`, `timeout`) and a narrowing hint, there is no pagination, and every listed path is filtered through the read path policy (`tools.native.glob.paths` defaults to the read rules);
- native `write` accepts `path` and `content`, optionally `contentEncoding` (`utf8` by default), and `backup` (true by default); invalid base64 returns `invalid_base64` with a suggestion;
- native `write` creates missing parent directories, writes atomically, preserves existing file permissions, and creates a versioned backup before replacing an existing regular file;
- identical content is reported as `action: unchanged` without a backup, a write, or a metadata change;
- backups are named `<file>.bak-<ms>-<random hex>` and collect in `tools.native.write.backup_dir` (default `<session>/backups`), mirroring the target's directory relative to the workspace and falling back to `@external/<sha256 prefix>` for targets outside it, so the edited tree itself stays clean; a relative `backup_dir` resolves against the session cwd, an absolute one is used as given, and `null` keeps backups beside their file;
- backups are pruned per file to `backup_max_count` (5) and `backup_max_total_bytes` (10 MiB), never removing the newest backup;
- `fsync` (true by default) syncs the staging file and its directory before success is reported;
- mutations of one canonical path are serialized in-process across tools and sessions; `read` reports `changed_during_read` and `edit` reports `changed_during_edit` instead of returning or writing a mixed snapshot;
- `tools.native.write.max_bytes` bounds writes, and `tools.native.write.paths` applies the same normalized allow/deny path policy as `read`;
- native `edit` replaces exact text in an existing UTF-8 file: `path`, `oldText`, `newText`, optional `replaceAll` and `backup`; `oldText` must match exactly once unless `replaceAll` is set, and an empty `newText` deletes the match;
- native `edit` normalizes line endings to the file's style when that style is unambiguous, refuses non-UTF-8 files, reports `no_match` with a bounded diagnostic (nearest line, matched lines, first divergence) and `ambiguous_match` with occurrence lines, and otherwise reports `action: edited` with `replacements`, `bytes`, `line_ending`, `start_line`/`end_line`, and `backup_path`;
- `tools.native.edit.max_bytes` (1 MiB) bounds both the file and the result, while `tools.native.edit.paths`, `backup_dir`, backup quotas and durability default to the `tools.native.write` settings and can each be overridden per tool, so both mutating tools share one policy, one lock and one backup directory while keeping their own quotas;

Example write-tool enablement:

```json
{
  "tool_definition_paths": ["sample/tools/native/write.json"],
  "tools": {
    "native": {
      "write": {}
    }
  }
}
```

Example storage and HTTP debug paths:

```json
{
  "max_tool_rounds": 8,
  "persistence": {
    "durability": "turn",
    "max_item_bytes": 10485760,
    "response_metadata": "minimal",
    "cursor_cache_checkpoint_bytes": 1048576
  },
  "compaction": {
    "auto_enabled": true,
    "trigger_tokens": 200000,
    "backup_mode": "replace",
    "keepbatch": {"tools": 0, "turn": {"type": "assistant_final", "keep": 1}}
  },
  "paths": {
    "messages": "data/messages.jsonl",
    "messages_backup": "data/messages.backup.jsonl",
    "compaction_cache": "data/compaction-cache.json",
    "model": "config/model.json",
    "staging": "runtime/staging",
    "http_debug": "/var/tmp/agent-harness/http"
  },
  "http_debug": {
    "enabled": false,
    "include_request_headers": false,
    "include_request_body": false,
    "include_response_headers": true,
    "include_response_body": false,
    "max_body_bytes": 65536
  }
}
```

Example model configuration:

```json
{
  "schema_version": 1,
  "provider": "openrouter",
  "base_url": "https://openrouter.ai/api/v1",
  "api_key_env": "OPENROUTER_API_KEY",
  "model": "inclusionai/ling-3.0-flash-fin:free",
  "request": { "max_output_tokens": 4096 }
}
```

The loader rejects runner-owned request fields such as `input`, `instructions`, `stream`, and `tools` inside `model.json`.

### 2.3 Session runner and native tools

`src/session.ts` provides `AgentSession` for the streaming turn path:

1. load and normalize model/settings files;
2. load the selected tool definition documents and compile their input schemas with AJV;
3. normalize text input into a Responses `message` input item;
4. append the input item to the configured transcript path, defaulting to `messages.jsonl`;
5. reconstruct all prior input items and completed response output items from that same path;
6. build a streaming request with `model`, `input`, `instructions`, model request options, and selected tools;
7. persist the streamed response through the response adapter, including provider `response.incomplete` terminal responses;
8. execute returned function calls, append `function_call_output` items, and continue until a response has no tool calls;
9. if execution is interrupted, the round limit is reached, or an older transcript contains an unanswered `function_call`, append an explicit error `function_call_output` before the next provider request. This prevents a committed function call from becoming an invalid provider history; missing results are not rerun automatically because a previous tool may already have had side effects.

`setting.json` controls the hard limit with `max_tool_rounds` (default `8`, range `0..50000`). The initial provider request is not counted; each tool-execution continuation consumes one round. A final tool result receives its follow-up provider request before the limit is enforced. `AgentRunOptions.maxToolRounds` can override the session setting for one run. The same file controls persistence `durability` (`process`, `turn`, or `strict`), `max_item_bytes`, and `cursor_cache_checkpoint_bytes` (default `1048576`, range `1..67108864`). When a cursor index is read, it is atomically checkpointed after at least this much transcript growth; after a restart, any uncheckpointed tail is rebuilt from authoritative `messages.jsonl`.

Runs are serialized per `AgentSession` instance. Tool-call schema failures and execution failures are returned as compact tool output text; they do not cause an unnecessary duplicate provider request.

### 2.4 Local HTTP/SSE bridge

`src/server.ts` exposes the runner to the local frontend without exposing provider credentials to the browser:

- `GET /api/health` checks that the server is available;
- `GET /api/sessions/:id` returns the persisted transcript snapshot;
- `GET /api/sessions/:id/tools` returns the effective provider-neutral tool definitions;
- `POST /api/sessions/:id/run` accepts `{ "input": "..." }` and emits SSE `ui.message`, `completed`, `done`, or `error` events;
- `GET /api/sessions/:id/run/stream` attaches to the run a session is executing, replaying its latest `ui.message` snapshot and then live events; it answers `event: idle` when nothing is running;
- `POST /api/sessions/:id/run/stop` aborts the run in flight and returns `{ "stop": { "stopped": true, "turn_id": "..." } }`.

### 2.4.1 Runs outlive their request

A run stays registered in `src/run-registry.ts` after the request that started it disappears, because closing a browser tab must not discard an accepted turn. The registry keeps one entry per session with the newest normalized `WebUiMessage` snapshot plus a subscriber set, so:

- the transcript snapshot and the session list report `run: { turn_id, started_at, status }` while a turn is in flight, which is how a reloaded page discovers a run it did not start;
- attaching means replaying that snapshot (every `ui.message` payload is already a complete message, so no delta history has to be buffered) and then streaming the remaining events to any number of clients, including the request that started the run;
- stopping is an explicit request, not a side effect of disconnecting: the abort signal cancels the provider request, so the stopped round commits no response while already-committed rounds stay in the transcript;
- a finished run is retained for `server.run_state_retention_ms` (default 30000, `setting.json`) so a reload that lands just after completion still receives its `completed`/`done` pair.

### 2.4.2 Streaming tool arguments

A function call is announced by `response.output_item.added`, then its arguments arrive as `response.function_call_arguments.delta` events and are confirmed by `response.function_call_arguments.done`; the finished item arrives with `response.output_item.done`. The normalizer therefore appends the `tool_call` part as soon as the item is added (marking it `partial: true`), grows its `arguments` with each delta, and lets `done`/`output_item.done` replace the text and clear the marker — so a long `write`/`edit` body is visible while it streams, not only once the call is complete. `complete()` never leaves the marker set.

Snapshots are cumulative, so forwarding one per delta would grow quadratically (a 5.9 KB `write` body produced 1244 snapshots and 4.8 MB). `src/stream-emitter.ts` coalesces the in-progress snapshots to at most one per `server.tool_stream_interval_ms` (default 120), emitting immediately when the window is open and flushing the newest state before a tool result or a terminal event.

The session roots are configured by the system `setting.json` `session_roots` array. Each root path is resolved relative to the directory containing that file and contains session subdirectories; that same directory is the global JAD root used for other relative paths. The same file's `server` object controls host, port, and request-body size defaults; explicit server options and environment variables override those values. A future `jad --config <path-to-setting.json>` entry point will select the system setting explicitly. `JAD_ROOT_SETTING_PATH` selects another system setting file, while `JAD_SESSIONS_DIR` and the `sessionsDir` server option remain single-root compatibility overrides. The default fallback is `.jad/sessions` (or `../.jad/sessions`). When multiple roots are configured, API session references are namespaced as `root-id:session-id` to avoid collisions. The frontend Vite server proxy target is configured in `frontend/src/config.json`. Start the local integration with `npm run server --prefix backend` and `npm run dev --prefix frontend` from the project root. The backend owns its TypeScript compiler and Node types; the frontend is only an HTTP/SSE client and is not a backend build dependency.

The global `setting.json` `libraries` object configures shared Markdown sources with `prompt_dirs`, `template_dirs`, and `skill_dirs` arrays. Relative directory paths resolve from the directory containing the global setting file; absolute paths are accepted. Defaults are `.agent/prompts`, `.agent/templates`, and `.agent/skills`. Prompt and template directories are searched recursively for `.md` files. Each skill directory is a collection root containing folders with a standard `SKILL.md` file and YAML frontmatter `name` and `description`; the skill name must match the folder name. Library routes re-read `setting.json` and asset files on each request, so edits and directory-array changes take effect on the next refresh or selection without a backend restart. Selecting a template inserts its full Markdown body in the composer; prompt bodies use the configured Astryx paste-as-token character threshold; skills are attached as badges. In every case the serialized user message contains the selected Markdown body, not a file path or harness-specific token.

The browser may abort its fetch, but the first bridge version does not cancel an already-started provider request; the server continues persistence so the transcript remains consistent. Raw user input is appended before model/tool/provider setup, so a configuration failure still leaves the user message in the configured transcript path. HTTP debug files are created only when the provider fetch starts; setup failures therefore have no HTTP request artifact.

### 2.5 `web-ui-normalized` plugin

`src/plugins/web-ui/` is a transport/view plugin, separate from `messages.jsonl` persistence. The legacy `src/plugins/web-ui-normalized.ts` module remains a compatibility barrel:

- `reasoning-web-ui.ts` normalizes reasoning output and deltas;
- `assistant-output-web-ui.ts` normalizes assistant text output and deltas;
- `tool-read-web-ui.ts` normalizes native read calls, tool-call fallbacks, and tool results;
- `setting-web-ui.ts` maps session configuration into UI metadata;
- `normalize-web-ui.ts` owns ordering, merging, tool-result pairing, and message assembly.

The plugin:

- converts raw session records into UI messages for `GET /api/sessions` and `GET /api/sessions/:id`;
- the session list requires `setting.json`, validates its session settings, and uses `setting.json.session_name` for the display name; model validation remains part of full session operations;
- it converts provider stream events into ordered `ui.message` SSE updates;
- it keeps adjacent reasoning/text chunks merged in the backend and emits tool call/result blocks as structured parts;
- it does not change the raw `messages.jsonl` format or write a UI projection there;
- the frontend receives ready-to-render messages and does not normalize provider events.

Each normalized assistant message contains ordered `parts` with `reasoning`, `text`, `tool_call`, or `tool_result` types, plus aggregate `text` and `reasoningContent` fields for rendering. Tool results retain their bounded output and a normalized success/error status.

## 3. Terminology

- **Turn:** One Responses API request and its resulting stream.
- **Output item:** One item in `response.output`, such as a message, function call, or reasoning item.
- **Staging:** Durable temporary state for a turn that has not yet been committed.
- **Commit:** The operation that appends one complete `response.completed` line to `messages.jsonl`.
- **Canonical:** Compact, deterministic JSON with the API-compatible top-level shape. Canonical does not automatically mean byte-identical to the provider's original SSE serialization.

## 4. Staging layout

Each active turn gets a unique directory:

```text
staging/<turn-id>/
├── manifest.json
├── items.jsonl
├── items.index.jsonl
└── final.jsonl                 # exists only while committing
```

`items.jsonl` contains internal records, one per completed output item:

```json
{"output_index":0,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Hello"}]}}
{"output_index":1,"item":{"type":"function_call","call_id":"call_123","name":"search","arguments":"{}"}}
```

The staging envelope is internal and must never appear in the final `messages.jsonl` record.

`items.index.jsonl` stores the byte offset and length of each item record. It allows the commit phase to emit output items in `output_index` order without loading the whole response into memory. `final.jsonl` is a fully rendered canonical line prepared before it is appended to `messages.jsonl`; it makes commit retry and hash-based idempotency possible.

A manifest contains transaction state, for example:

```json
{
  "schema_version": 1,
  "turn_id": "turn-001",
  "response_id": "resp_123",
  "status": "streaming",
  "item_count": 1,
  "next_sequence": 1
}
```

Recommended manifest states:

```text
streaming -> ready -> committing -> committed
                         \-> failed
```

The manifest may also contain a canonical-line hash after the final record has been rendered. The response ID and hash are used for recovery and duplicate-commit detection, not added to `messages.jsonl`.

## 5. Streaming workflow

### 5.1 Start a turn

1. Allocate a unique `turnId`.
2. Create the staging directory.
3. Create the manifest with `status: "streaming"`.
4. Open the Responses API stream.

Only one writer should commit a session at a time. The default harness should also process turns sequentially because response order affects conversation history and provider prompt caching.

### 5.2 Process deltas

While an output item is incomplete, the stream adapter keeps the current item state in memory. Deltas update that state; they are not appended to `messages.jsonl`.

The normal memory bound is:

```text
size of the largest active output item + stream/parser buffers
```

It is not the size of the whole response.

If a single output item can be very large, the adapter should support a spill threshold. Once the threshold is exceeded, text or argument fragments are written to a turn-local temporary file instead of remaining entirely in memory. This is an optional second phase; the initial implementation may enforce a maximum item size and fail safely when it is exceeded.

### 5.3 Process `response.output_item.done`

When an output item is complete:

1. Validate the item shape.
2. Determine its `output_index` from the API event. If unavailable, assign the next monotonic index.
3. Serialize an internal staging record.
4. Append exactly one complete line to `items.jsonl`.
5. Flush according to the configured durability policy.
6. Update the manifest.
7. Release the in-memory item and its buffers.

The item must be staged before its memory is released. A failed staging write must not advance the manifest or discard the item.

The final output order is determined by `output_index`, not merely by the order in which asynchronous callbacks happen to finish.

### 5.4 Process `response.completed`

When the final event arrives:

1. Verify that its response ID matches the manifest.
2. Verify that all expected output indexes are present and unique.
3. Preserve the final response metadata, including unknown future API fields.
4. Replace or ignore the final event's in-memory `response.output` and rebuild it from staged items in index order.
5. Render one canonical `response.completed` line.
6. Append the line to `messages.jsonl`.
7. Flush the messages file.
8. Mark the manifest `committed`.
9. Remove the staging directory after successful commit confirmation.

If the SDK has already materialized a complete `response.output` array internally, the harness cannot eliminate that SDK-level allocation. The persistence layer must nevertheless avoid retaining or serializing a second full copy.

### 5.5 OpenAI SDK adapter

`src/responses.ts` provides two boundaries:

- `persistResponseStream()` accepts any `AsyncIterable<unknown>` containing Responses-compatible events;
- `persistOpenAIResponse()` accepts the official `openai` Node SDK client and its typed streaming request.

The official SDK owns authentication, HTTP, and SSE decoding. The adapter forwards only `response.output_item.done` and `response.completed` events to the persistence layer. This keeps storage tests offline and keeps persistence independent of SDK-specific object types.

## 6. Memory-efficient final serialization

The implementation must not reconstruct a large response with one unbounded call such as:

```ts
JSON.stringify(fullResponse)
```

Instead, the final line should be rendered incrementally from metadata and staged items:

```text
write {"type":"response.completed","response":<metadata-without-output>,"output":[
write staged item 0
write ,
write staged item 1
write ]}}
write newline
```

The exact serializer must produce valid JSON and must preserve the required property order if byte-level cache keys are used.

Because an append-only file cannot atomically replace an existing file, a crash may leave a partial final line. On startup, the writer must retain complete newline-terminated records and truncate only the incomplete trailing record before resuming.

## 7. Canonical `messages.jsonl` rules

Every committed response record must follow these rules:

1. UTF-8 encoding.
2. One JSON object per physical line.
3. No pretty-print whitespace.
4. A terminating newline after every committed record.
5. Top-level shape exactly:

   ```json
   {"type":"response.completed","response":{}}
   ```

6. No harness-specific `turn_id`, sequence, checksum, or staging fields in the final record.
7. Preserve the API response fields, including fields unknown to the current TypeScript version.
8. Preserve `response.output` array order.
9. Omit no meaningful API field and do not serialize JavaScript `undefined` values as custom placeholders.
10. Use a deterministic serializer. Fixed object construction order is sufficient for one runtime; a canonical JSON algorithm is preferable if multiple runtimes will produce cache keys.

This provides structural API compatibility. If byte-for-byte equality with the original provider payload is required, the raw provider event must also be captured. Re-serializing an SDK object cannot guarantee the provider's original whitespace, property order, or number representation.

## 8. Failure and recovery rules

### Process failure during an item

Previously staged items remain available. The current incomplete item is discarded and must be rebuilt when the stream/request is restarted.

### Failure during an item staging write

The last incomplete `items.jsonl` line is truncated to its previous newline. The item is retried; the manifest is not advanced until the complete line is durable.

### Failure after staging but before `response.completed`

The response is not committed. The staging directory remains for recovery or diagnostics.

### Failure while appending the final message

On startup:

1. truncate any incomplete trailing `messages.jsonl` line;
2. inspect the unfinished staging manifest;
3. compare the response ID and canonical-line hash with the last complete message record;
4. mark the transaction committed if the final line already exists;
5. otherwise retry the append.

This prevents duplicate commits after a crash between the messages flush and staging cleanup.

### Missing `response.completed`

Do not write a synthetic `response.completed` record. Retain the staged items and mark the turn incomplete. A later recovery operation may retrieve the response by ID if supported or restart the request.

### Remote stream failure

The local staging protocol can preserve completed items, but the Responses API stream should not be assumed to support arbitrary item-level resume. If the provider connection is lost, the harness may need to restart the entire request. It must not splice items from a new response into an old response unless response identity and output equivalence are explicitly proven.

### Missing transcript file

A session that has never committed a response has no `messages.jsonl` yet. Reading the usage checkpoint stats the transcript, so a missing file is treated as an empty transcript (and created lazily) instead of failing the request; an empty session must be able to answer `GET /api/sessions/:id` and accept a run.

## 9. Durability and performance

The writer should support a configurable durability mode:

- **process:** await writes, optimized for process failures but not sudden power loss;
- **turn:** flush staged data and `messages.jsonl` at turn boundaries;
- **strict:** flush each completed staged item and the final committed line.

Recommended initial behavior:

- one sequential writer per session;
- await backpressure from file writes;
- flush each completed item in strict/recovery-sensitive mode;
- always flush the final `messages.jsonl` line before deleting staging;
- never use synchronous filesystem calls on the stream event loop.

A single `items.jsonl` per turn is preferred over one file per item because it reduces inode operations. The trade-off is that recovery must truncate an incomplete trailing line.

## 10. TypeScript boundaries

The implementation should keep transport, staging, and canonical persistence types separate:

```ts
interface StagedOutputItemRecord {
  output_index: number;
  item: Record<string, unknown>;
}

type ResponseInputItem = Record<string, unknown>;

interface TurnManifest {
  schema_version: 1;
  turn_id: string;
  response_id?: string;
  status: "streaming" | "ready" | "committing" | "committed" | "failed";
  item_count: number;
  next_sequence: number;
  canonical_line_sha256?: string;
}

interface CompletedResponseEvent {
  type: "response.completed";
  response: Record<string, unknown> & {
    output: Array<Record<string, unknown>>;
  };
}
```

The public stream adapter also exposes:

```ts
persistResponseStream(store, stream, { turnId });
persistOpenAIResponse(client, store, streamingRequest, { turnId });
```

The official SDK dependency is limited to the adapter; the file format and staging implementation do not import SDK response classes.

The final event type should remain extensible. `usage` is normalized in both metadata modes to the
counters used by the usage cache (`input_tokens`, `output_tokens`, `total_tokens`, and
`input_tokens_details.cached_tokens`); provider-specific details such as per-item attribution maps are
not stored in the transcript. This keeps the usage cache rebuildable without duplicating growing
provider metadata on every response. Under `persistence.response_metadata: "full"`, other unknown API
fields survive persistence verbatim, so adding a newer Responses API field requires no migration. The
default `"minimal"` stores only the fields the store and its readers use (`id`, `object`, `status`,
`model`, `created_at`, `completed_at`, `incomplete_details`, compact `usage`, `output`) because
providers echo the whole request — tool definitions included — on every completed response. A reader
never needs that echo: the request context is rebuilt from configuration, and `http_debug` records the
wire traffic when the echo is wanted for auditing. Set `"full"` to restore other response metadata.

## 11. Validation and tests

The first implementation should test at least:

- a response with one message output item;
- multiple output items with nontrivial indexes;
- message, function-call, and reasoning output item shapes;
- interleaved stream deltas;
- a large response whose total size exceeds the intended memory budget;
- process failure before and after each staging write;
- a partial trailing staging line;
- a partial trailing `messages.jsonl` line;
- crash after final message append but before staging deletion;
- duplicate response ID recovery;
- missing or duplicate output indexes;
- `response.completed` without staged items;
- unknown fields in the response object;
- validation that `messages.jsonl` contains only complete canonical records.

## 12. Deliberate scope boundaries

This design standardizes assistant response completion records, raw user/tool input items, and native tool execution. MCP client support is described below; destructive native tools and automatic retry strategy remain outside this design.

The design also assumes that cache compatibility means stable structural/serialized response data. Provider prompt-cache hit rate additionally depends on stable input history, model settings, tools, and other request fields; storing the response event alone cannot guarantee a provider cache hit.

## 13. Acceptance criteria

The design is complete when:

- every successful Responses API turn produces exactly one canonical `response.completed` line;
- no intermediate streaming event is committed to `messages.jsonl`;
- completed output items are staged before their memory is released;
- recovery never exposes a partial JSON record as a valid message;
- final response serialization does not require retaining the full response in application memory;
- staging can be safely replayed or discarded without corrupting session history;
- the final record remains compatible with the Responses API event shape.

## 14. MCP client tools

`setting.json.mcp_servers` is an opt-in list of server references. Each entry has a stable `id`, an absolute `config_path` or a path relative to the normalized session `cwd`, an `enabled` flag (default false), and exact remote tool names in `deny_tools`. Disabled entries are not read, connected, or launched. A server config file declares either an MCP stdio command or a Streamable HTTP URL. Stdio `transport.env` supplies literal process settings; use `env_from` for secrets referenced by environment-variable name. HTTP credentials use `headers_from`.

The session-owned `McpServerManager` keeps enabled clients alive across provider tool rounds, refreshes tool discovery at the start of each run, and closes clients with the session. Provider-visible names are namespaced and mapped back to the original `(server id, tool name)` for dispatch. Deny filtering happens before tool definitions are advertised. MCP input schemas are compiled for runtime argument validation; server/config errors identify the server and config path.

MCP text results become `input_text`; images become Responses `input_image` data URLs; embedded image resources become images and other binary resources become `input_file`; resource links are represented as text references. `structuredContent` is serialized as text. Audio is not enabled: audio blocks are represented by an explicit omission note. Per-server `timeout_ms` and `max_result_bytes` bound calls and model-bound output. The UI renders textual summaries/placeholders for image/file outputs and does not forward base64 media to the browser.

Example session reference:

```json
{
  "mcp_servers": [
    {"id": "docs", "config_path": "mcp/docs.json", "enabled": true, "deny_tools": ["delete_document"]}
  ]
}
```

Example `mcp/docs.json` for a local server:

```json
{
  "schema_version": 1,
  "transport": {
    "type": "stdio",
    "command": "node",
    "args": ["server.js"],
    "cwd": "tools/docs-server",
    "env": {"LOG_LEVEL": "warn"},
    "env_from": {"DOCS_TOKEN": "DOCS_TOKEN"}
  },
  "timeout_ms": 30000,
  "max_result_bytes": 1048576
}
```

The server config's `cwd` is optional and relative to the session `cwd`. The equivalent remote transport uses `{"type":"streamable_http","url":"https://…","headers_from":{"Authorization":"MCP_AUTH_HEADER"}}`.
