# `bash` Tool Design

**Status: implemented.** Sections 1–12 describe the shipped design; §13 records the confirmed decisions and
where the implementation deviates from the original proposal. Implemented in
`backend/src/tools/native/bash.ts` (tool) and `backend/src/tools/native/child.ts` (process runner and
bounded output collector), with 26 tests in `backend/test/bash-tool.test.ts`.

## 1. Purpose

`bash` runs one shell command in the workspace and returns its bounded output, so the agent can do the
things no dedicated tool covers: installing dependencies, running tests and builds, `git`, `make`,
linters, pipelines, and one-off scripting.

It is the *fallback* tool, not a general one: file work belongs to `read`, `write`, `edit`, and `grep`,
because those normalize arguments, bound output, and enforce path policy. The model-facing description
must say so, otherwise every task degrades into `bash: cat …`, which costs tokens, produces unbounded
output, and skips the workspace policy.

## 2. Tool selection

| Need | Tool |
| --- | --- |
| Read a file or directory | `read` |
| Create or replace a file | `write` |
| Change part of a file | `edit` |
| Search file contents | `grep` |
| Build, test, install, `git`, pipelines | `bash` |

Proposed name: **`bash`** (the guidelines suggest `exec`; `bash` matches the user's request and the
existing concrete names, and it also tells the model that shell syntax is allowed).

## 3. Request input

Smallest useful call: `bash({command: "npm test"})`.

| Field | Type | Required | Notes |
| --- | --- | --- | --- |
| `command` | string (minLength 1) | yes | Passed verbatim to the shell; quoting and pipelines are the caller's business. |
| `cwd` | string (minLength 1) | no | Directory to run in, relative to the workspace root or absolute. Defaults to the workspace root. Must exist, be a directory, and pass the path policy. |
| `timeout_ms` | integer | no | Per-call override, clamped to `[1000, max_timeout_ms]`; defaults to the configured `timeout_ms`. |

Deliberately **not** exposed, because the backend can decide or the shell already expresses it:

* `shell` / `login_shell` — operator policy (§9).
* `env` — the shell already has `FOO=bar cmd`; per-call env adds no capability.
* `stdin` — stdin is closed (§4); heredocs cover real needs.
* stdout/stderr selection — both are always captured as separate sections, attributed per stream (§5).
* output limits — bounded automatically (§6); narrowing is `head`/`tail`/`grep` inside the command.
* `background` — see §12; a job manager is a separate feature.

Normalization before validation: trim the command; treat a whitespace-only command as a hard error
(`invalid_input` / `invalid_command`); pass other fields through unchanged.

## 4. Execution semantics

```text
resolve cwd (path policy, must be an existing directory)
→ build sanitized environment
→ spawn(shell, ["-c", command], {cwd, detached: true, stdio: ["ignore", "pipe", "pipe"]})
→ stream each pipe into its own bounded collector, teeing both to their spill files
→ return on exit, or on timeout after killing the process group
```

* **Fresh shell per call, no state carried over.** `cd`, `export`, aliases, and functions do not survive
  into the next call. Stateless calls stay debuggable, keep the reported `cwd` unambiguous, and avoid a
  session-lifetime shell protocol. Callers pass `cwd` or chain with `cd x && …` in one command. This is
  the single most important constraint to document for the model, since agents tend to `cd` in one call
  and assume it stuck.
* **Non-login shell by default.** `bash -c <command>`. Login shells source profiles that print banners
  and can `cd`, which would silently move the command elsewhere. Operators can enable `login_shell` when
  a toolchain needs profile-provided `PATH` entries.
* **Own process group** (`detached: true`) so the whole tree can be signalled. Timeout ladder: SIGTERM
  to the group, then SIGKILL after `kill_grace_ms`, then resolve with the partial output.
* **The ladder always ends.** SIGKILL cannot be caught, but a process blocked in uninterruptible I/O does
  not act on it either; a kill deadline (2 s after SIGKILL) therefore resolves the call regardless of
  whether the process was reaped, reporting `left_running: true` so the model knows the process is still
  owned by the operating system. A call can wait at most `timeout_ms + kill_grace_ms + 2 s + 0.5 s`.
* **Group cleanup on completion** (`kill_process_group: "always"`, recommended): when the shell exits, its
  remaining descendants are signalled, so `nohup server &` cannot leave stray processes behind.
  Signalling happens on the child's `exit` event, before the child is reaped, and `ESRCH` is ignored, so a
  recycled pid cannot be hit. Descendants that move themselves into another process group are not caught —
  that is the documented limit of this mechanism. With `"timeout_only"` backgrounded children survive, and
  leaks become the operator's problem.
* **No TTY, stdin closed.** Interactive prompts cannot hang the turn; `GIT_TERMINAL_PROMPT=0` keeps git
  from waiting for credentials. A command that genuinely needs input fails fast instead of hanging.
* **Environment** = a small allowlist from the backend environment, plus `env_file` and inline `env`
  additions (§8). `env_deny` and the harness API-key variable are applied last.
* **One `bash` at a time per session.** `AgentSession.run` already executes tool calls of a round
  sequentially under a mutex, so a long command delays the other calls of the same round. That is
  acceptable and needs no extra locking, but the timeout is the only bound — there is no cancellation
  path from the session layer yet (§8, §12).

## 5. Result

```text
{"id":"c1","status":"ok","summary":"Command completed in 412 ms"}

--- INFORMATION ---
cwd: src
content_stream: mixed
exit_code: 1
duration_ms: 412
stdout_bytes: 228906
stdout_shown_bytes: 2048
stdout_omitted_bytes: 226858
stderr_bytes: 24
stderr_shown_bytes: 24
returned_bytes: 2089
truncated: true
truncation_reason: max_bytes
omitted_bytes: 226858
stdout_full: .bash-output/bash-c1.stdout.log
stdout_full_bytes: 228906
hint: the complete output is in .bash-output/bash-c1.stdout.log; read or grep it instead of re-running the command

--- CONTENT token=r_… encoding=utf8 ---
[stdout]
<16 KiB from the start of stdout>
[... 226858 bytes omitted ...]
<32 KiB from the end of stdout>
[stderr]
<all of stderr>
--- END CONTENT token=r_… ---
```

* `cwd` is the canonical display path actually used, so the model never has to guess where a command ran.
* stdout and stderr are captured through separate pipes, **kept in separate windows, and rendered as
  separate sections** — labeled `[stdout]`/`[stderr]` only when both wrote, so single-stream output stays
  a clean block. `content_stream` (`stdout`, `stderr`, or `mixed`) states which streams the content holds,
  and the head and tail of each stream are kept independently: a flood on stdout can no longer evict the
  error on stderr. Cross-stream ordering is not preserved in the content, and the section order is fixed
  (stdout, then stderr); arrival ordering was the price of attribution and the spill files keep the raw
  per-stream record.
* Each stream reports three numbers: `stdout_bytes` (produced), `stdout_shown_bytes` (head + tail), and
  `stdout_omitted_bytes` (what the model does not see) — the same trio for stderr.
* Truncated output names its complete file: `stdout_full`/`stderr_full` plus `*_full_bytes`, and
  `*_full_capped: true` when the file itself hit `spill.cap_bytes` and is therefore incomplete too. A
  `hint` points at the file so the model reads or greps it instead of re-running the command. A spill that
  could not be written is reported as `spill_error` — the command's own result is never changed by it.
* No output → no CONTENT frame (metadata and summary only), matching `read` and `grep`.
* Metadata appears only when useful: `truncation_reason`, `omitted_bytes`, `timed_out`, `signal`,
  `binary_output` are added only in the situations that set them.
* Summary: `Command completed in <ms>` for exit 0, `Command exited with <code> in <ms>` otherwise,
  `Command timed out after <ms>` for a timeout.

## 6. Limits and truncation

| Limit | Key | Default | Why |
| --- | --- | --- | --- |
| Wall clock | `timeout_ms` | 120000 | Bounds the only blocking resource a command can hold. |
| Per-call ceiling | `max_timeout_ms` | 600000 | Upper clamp for the `timeout_ms` argument. |
| Kill grace | `kill_grace_ms` | 2000 | Time between SIGTERM and SIGKILL of the process group. |
| Kill deadline | (fixed) | 2000 | Time after SIGKILL before the call returns anyway, reporting `left_running`. |
| Returned bytes | `max_bytes` | 65536 | Ceiling the four stream budgets must fit inside. |
| stdout window | `stdout.head_bytes` / `stdout.tail_bytes` | 16384 / 32768 | What the model sees of stdout: its start and its end. |
| stderr window | `stderr.head_bytes` / `stderr.tail_bytes` | 4096 / 12288 | Same for stderr, which noise from stdout can no longer evict. |
| Spill directory | `spill.storage_dir` | `.bash-output` | Relative paths resolve from the selected profile's `tool_storage` root, or the session cwd without a profile; absolute paths are used as-is. |
| Spill file | `spill.cap_bytes` | 67108864 | Bytes per stream written to disk before the file is left incomplete. |
| Spill retention | `spill.retention_days` | 14 | Expired spill files are pruned; 0 disables age-based pruning. |
| Line length | `max_line_chars` | 2000 | Cuts pathological single lines. |

* Each stream keeps its own *head* window (the start) and *tail* window (the end), so a chatty build
  cannot exhaust memory, its ending is preserved (where failures are reported), and neither stream can
  push the other out of memory. A stream that fits inside its two windows is retained exactly once, with
  no duplication and no gaps, and renders without a marker; longer streams get
  `[... N bytes omitted ...]` between head and tail. Lines longer than `max_line_chars` are cut with
  `[... truncated line]`. Windows are byte budgets, and a chunk larger than the remaining room is split at
  a UTF-8 character boundary rather than allowed to overflow the budget.
* Truncation is never an error: `status: ok`, `truncated: true`, `truncation_reason`
  (`max_bytes` or `timeout`), per-stream `stdout_omitted_bytes`/`stderr_omitted_bytes` plus their total,
  and a `hint` naming the spill file. A timeout *is* reported as an error (§7) because the requested
  action did not complete.
* Retrieval replaces pagination: the complete stream is teed to the file named by `stdout_full` or
  `stderr_full` while it is read, because the middle of a truncated stream no longer exists in memory.
  The model greps or reads that file — re-running a chatty command to see its middle is not required, and
  there is still no cursor-based paging over the *content* block.
* `spill.keep: "truncated_only"` (the default) deletes the file again when the model saw the whole
  stream, so a spill only survives when it is the only copy of something; `"always"` keeps one file per
  call per stream as a durable log. Spill files are pruned by `spill.retention_days`, matching only the
  generated per-call/per-stream filename pattern, so a project directory never loses unrelated files.
* Control characters: ANSI escape sequences and other C0 control bytes (except `\n`/`\t`) are stripped
  when `strip_ansi` is true, so colour codes cannot eat context or smuggle escape sequences into the UI.
  Output that still contains NUL or U+FFFD gets `binary_output: true` and a hint to use `read` with
  `contentEncoding: "base64"` for file bytes.

## 7. Errors

| Condition | `status` | `error_code` | Extra fields |
| --- | --- | --- | --- |
| Shell missing / not executable | `error` | `shell_unavailable` | `shell`, `system_error`, suggestion |
| Command ran, non-zero exit | `error` | `nonzero_exit` | `exit_code`, **full output still returned** |
| Timeout | `error` | `timeout` | `timed_out: true`, partial output, suggestion |
| Killed by a signal | `error` | `signaled` | `signal` |
| Spawn failed (EACCES/ENOMEM) | `error` | `spawn_failed` | `system_error` |
| Whitespace-only command | `invalid_input` | `invalid_command` | — |
| `cwd` missing | `not_found` | `not_found` | `path` |
| `cwd` is a file | `invalid_input` | `not_a_directory` | `path` |
| `cwd` outside the workspace | `forbidden` | `path_outside_workspace` | `path` |
| `cwd` denied by a rule | `forbidden` | `path_denied` | `path` |

* A non-zero exit keeps the output, because build and test failures are exactly what the caller needs to
  read. The summary states the exit code so a "failed" command is never mistaken for a broken tool, and
  the description warns that commands whose logic *is* their exit status (`grep -q`, `test`, `diff`)
  will surface as `nonzero_exit` — read `exit_code` in that case.
* Exit codes 126/127 get a hint (`command not found or not executable`), the most common recoverable
  mistake.
* Suggestions stay operational: for a timeout, raise `timeout_ms`/`max_timeout_ms` or narrow the
  command; for a missing shell, point at `tools.native.bash.shell`.

## 8. Safety and trust

**`bash` is not a sandbox, and the design must not pretend otherwise.** A command can read or write
outside the workspace, ignore `tools.native.bash.paths`, and start processes; the policy only governs
which `cwd` the tool itself will start from. Containment is operator-level:

* the tool is **opt-in** — it exists only when `tools.native.bash` is configured *and* its definition is
  listed in `tool_definition_paths`, like every other native tool;
* it runs as the backend user with that user's filesystem rights;
* the honest framing lives in the design and in `backend/design.md`; the model-facing description states
  the workspace directory is the default `cwd` and says nothing that implies containment.

Concrete hygiene the tool does own:

* **Credential hygiene in the environment:** the child inherits only `PATH`, `HOME`, locale/temp, and
  platform shell variables from the backend. It then merges default values (`NO_COLOR=1`, `TERM=dumb`,
  `GIT_TERMINAL_PROMPT=0`, `GIT_PAGER=cat`, `PAGER=cat`), optional `env_file` values, and inline `env`
  values in that order. `env_deny` and the harness credential (`model.api_key_env`, e.g.
  `OPENAI_API_KEY`) are removed last. Put command credentials in a protected dotenv file rather than
  `setting.json`; this is still not a sandbox, because a command can read any file the backend user can read.
* **No leaked processes:** the process group is signalled on timeout and, by default, on completion.
* **No unbounded calls:** the timeout ladder ends in a hard deadline, so a command cannot hold a call open
  even if it never reacts to SIGKILL.
* **No hangs on prompts:** stdin closed, no TTY, `GIT_TERMINAL_PROMPT=0`.
* **Context hygiene:** control characters stripped; output is untrusted data that the model must treat
  as information, not as instructions.
* **Spill scope:** a spill file holds exactly what the command already printed, so it grants the model no
  capability the call did not already have. It is written only to a location the configuration proved
  readable through the session cwd or a `read`/`grep` allow rule (and not covered by a deny rule), unless
  `spill.allow_unreadable` says otherwise; pruning matches only generated spill filenames. Legacy templates
  remain accepted temporarily and retain their own matching/pruning rules.
* **Bounded blast radius in rounds:** calls are sequential under the session mutex, and the timeout caps
  how long one command can hold the turn.

Known limitation: the session layer has no cancellation path (`AbortSignal`), so a cancelled or stalled
turn waits for `timeout_ms`. Wiring cancellation into `AgentSession.run` is a separate change.

## 9. Configuration

```jsonc
"tools": {
  "native": {
    "bash": {
      "shell": "/bin/bash",        // PATH name or absolute path
      "login_shell": false,        // add -l (profile PATH, but banners and cd risk)
      "timeout_ms": 120000,
      "max_timeout_ms": 600000,
      "kill_grace_ms": 2000,
      "kill_process_group": "always",  // "always" | "timeout_only"
      "max_bytes": 65536,          // ceiling the four budgets below must fit inside
      "stdout": {"head_bytes": 16384, "tail_bytes": 32768},
      "stderr": {"head_bytes": 4096, "tail_bytes": 12288},
      "spill": {
        "enabled": true,
        "keep": "truncated_only",  // or "always": a file per call
        "cap_bytes": 67108864,
        "retention_days": 14,
        "storage_dir": ".bash-output",
        "allow_unreadable": false   // false refuses a location read/grep could not open
      },
      "max_line_chars": 2000,
      "strip_ansi": true,
      "env_file": "command.env", // dotenv; relative to the session directory, or absolute
      "env": {"NO_COLOR": "1", "TERM": "dumb", "GIT_TERMINAL_PROMPT": "0", "GIT_PAGER": "cat", "PAGER": "cat"}, // overrides env_file
      "env_deny": [],              // always applied after env_file/env; harness API key is always denied
      "paths": []                  // defaults to the tools.native.write rules
    }
  }
}
```

* `spill.storage_dir` is a plain directory path. Relative values resolve beneath the selected profile's
  `tool_storage` root, or relative to the session cwd without a profile; absolute paths are accepted as-is.
  The tool generates `bash-{call_id}.{stream}.log` names internally, encoding call IDs so they cannot
  introduce path separators and bounding very long names. `stdout_full`/`stderr_full` remain result
  metadata identifying retained files; they are no longer the normal configuration keys.
* Legacy `stdout_full`/`stderr_full` path templates are accepted temporarily for existing settings and
  marked deprecated in the schema. If either is configured, legacy mode takes precedence and an explicit
  non-default `storage_dir` cannot be combined with them; remove both templates to migrate. Legacy
  templates still support `{cwd}`, `{session_dir}`, `{session_name}`, `{call_id}`, and `{stream}`. With a
  named profile, relative template targets are resolved under its `tool_storage` root; targets resolving
  to absolute paths remain direct. Without a profile, existing template behavior is preserved.
* Spill locations are validated so the model can retrieve them: generated files (or legacy template
  targets) must be readable through the session cwd or through a `tools.native.read`/`grep` allow rule,
  and not covered by a deny rule, or the session refuses to load unless `spill.allow_unreadable` is set.
  A spill that the model cannot read would be worse than no spill at all.
* `capture_bytes` and `head_ratio` are gone: they described one shared window for both streams, which
  let a chatty stdout evict stderr. Sessions that still set them fail to load with the replacement named.

* `paths` inherits the **write** rules (bash mutates; the stricter rule set is the right default), then
  overrides per session. It constrains `cwd` selection only, per §8.
* Validation mirrors the other tools: type/range checks with `ConfigError` messages naming
  `tools.native.bash.<field>`, defaults materialized at load, and the shipped
  `sample/schemes/setting.schema.json` gaining a `bash` entry.
* `sample/tools/native/bash.json` ships the definition document; the catalog validates the required
  field order (`["command"]`) exactly as it does for the other native tools.

## 10. Implementation map

| Path | Role |
| --- | --- |
| `backend/src/tools/native/bash.ts` | Definition, normalization, `cwd` resolution against the policy, result rendering, metadata, error mapping. |
| `backend/src/tools/native/child.ts` | Reusable child-process runner: spawn options, timeout timer, SIGTERM→SIGKILL ladder with the kill deadline, process-group kill, spawn-error classification, plus the per-stream `OutputCollector` (own head/tail windows per stream, streaming UTF-8 decode, line trimming, control-character stripping) and the `SpillWriter` tee. |
| `backend/src/tools/native/spill.ts` | Spill filename safety, legacy template resolution, generated/legacy prune matchers, directory preparation, and retention pruning. |
| `backend/src/tools/native/registry.ts` | `bashSpec`: `required: ["command"]`, `exampleOperation: {command: "npm test"}`, `configured`, `build`. |
| `backend/src/config.ts` | `NativeBashToolSettings` + validation and defaults; `paths` inheritance from write. |
| `sample/tools/native/bash.json` | Shipped definition document. |
| `sample/schemes/setting.schema.json` | `tools.native.bash` schema entry with defaults. |
| `backend/test/bash-tool.test.ts`, `backend/test/tool-session.ts` | Tests plus `makeBashToolSession`/`refreshBashSession` helpers. |
| `backend/test/definitions.test.ts`, `backend/package.json` | Drift test entry and the test file list. |
| `backend/design.md` | Design-doc bullets, including the "not a sandbox" statement. |

`child.ts` is kept independent of bash so `ripgrep.ts` can migrate onto it later; that refactor is not
part of this change (it would touch a tool that currently passes its tests).

## 11. Tests

Smallest call first, then one case per behaviour:

* `echo` succeeds, reports `cwd`, `exit_code`, and no truncation; empty output produces no CONTENT frame.
* Exit 1 and exit 127: `nonzero_exit` with output intact and the 127 hint.
* stdout/stderr attribution (`stderr_bytes`) and a command that writes both.
* `strip_ansi: true` removes colour codes; `false` keeps them.
* Line truncation, byte truncation with head+tail and `[... N bytes omitted ...]`, capture ceiling with a
  `seq`-driven flood, and a check that the tail of a large log survives.
* Timeout: `sleep 30` with a 200 ms budget returns `timeout` with `timed_out: true`; a marker file proves
  the process group was killed (`sleep 30; touch marker` never touches it).
* `kill_process_group: "always"` kills a backgrounded child on normal exit.
* `cwd`: default workspace root, explicit relative and absolute paths, missing, file-not-directory,
  outside the workspace, and denied by a rule (all four error codes).
* Environment: the harness credential variable is absent from `env`, `NO_COLOR` is present, `PATH`
  survives; `env_deny`/`env` overrides apply.
* Binary output: `cat` of a file with NUL bytes yields `binary_output: true` and no NUL in the content.
* Settings validation and defaults, `paths` inheritance from write, and the "configured but no
  definition" gate; definition drift for `bash.json`.
* Bash absent → the suite skips, like the ripgrep tests.

Verification plan when implemented: `npm test` (all suites), `tsc` build, plus a live smoke against a
scratch session running `env`, `git status`, and a timeout case.

## 12. Deliberate non-goals

* No persistent shell, no cwd/env carry-over between calls.
* No background job manager (`bash_output`, `bash_kill`, job ids) and no PTY/interactive TTY.
* No per-call `env` or `stdin` parameters.
* No streaming of output to the UI while a command runs (the harness has no tool-output stream path yet).
* No sandboxing (bubblewrap/container/`sudo` handling); operators can point `shell` at a wrapper only
  once a `shell_args` seam exists — deferred by design.
* No Windows support: the tool requires `bash`.
* No `shell_args`, aliases, or multiple shells per session.

## 13. Confirmed decisions and implementation notes

All ten open items were confirmed as recommended. Where the implementation differs from the proposal:

1. **Name:** `bash` (not `exec`). The schema also accepts the near-miss spellings `cmd`, `workdir`, and `dir`,
   normalized before validation, and trims the command.
2. **Non-zero exit:** `error` + `nonzero_exit`, with the full output returned and a hint for 126/127.
3. **Timeouts:** `timeout_ms` defaults to 120 s, `max_timeout_ms` to 600 s, and the per-call `timeout_ms` is
   clamped to `[MIN_NATIVE_BASH_CALL_TIMEOUT_MS, max_timeout_ms]`. The floor is 1000 ms for *per-call*
   overrides only; the *configured* `timeout_ms` may be smaller (tests use 300 ms), which keeps a typo in a
   tool call from killing a command instantly.
4. **`kill_process_group: "always"`** is the default; the group is signalled on the child's `exit`, before it
   is reaped, so a recycled pid cannot be hit.
5. **Environment:** `tools.native.bash.env_file` is a strict dotenv file; paths are session-relative or
   absolute. Its values merge over `DEFAULT_NATIVE_BASH_ENV`, then inline `tools.native.bash.env` wins.
   `buildCommandEnvironment()` removes `model.api_key_env` (resolved when the catalog is built) plus every
   name in `env_deny` last, so neither source can re-enable the harness credential.
6. **Budgets:** per stream — stdout 16 KiB head / 32 KiB tail, stderr 4 KiB head / 12 KiB tail, together no
   more than `max_bytes` (64 KiB) — plus `max_line_chars` 2000; the middle is dropped and the complete
   stream lives in its spill file while it is read.
7. **`paths`** defaults to the write rules (bash mutates).
8. **`child.ts`** is new and used by bash only; `ripgrep.ts` keeps its own runner.
9. **Description** mentions the workspace directory but promises no containment; the limits live in this
   document and in `backend/design.md`.
10. **Non-goals** unchanged: no persistent shell, background job manager, PTY, per-call env/stdin, streaming,
    sandbox seam, Windows, or `shell_args`.

Extra behaviour worth recording, all covered by tests:

* A straggler that inherits stdout and keeps the pipe open no longer hangs a call: after the leader exits, the
  runner drains for at most 500 ms before returning (see `DRAIN_AFTER_EXIT_MS`).
* `strip_ansi` also removes NUL, other C0 controls, DEL, and C1 controls, maps lone carriage returns to
  newlines, and preserves surrogate pairs; output that contained NUL or invalid UTF-8 reports
  `binary_output: true`.
* Verification: `npm test` passes with 146 tests. The bash suite covers the credential-stripping check, the
  process-group kill and SIGKILL escalation (including the bounded ladder for a command that ignores SIGTERM
  and the guarantee that a timeout leaves no process behind), stdout/stderr attribution with labeled sections,
  stderr surviving a stdout flood, head/tail windows per stream with exact byte accounting, the spill round
  trip (complete file the model can read, deleted when nothing was omitted, kept per `keep: "always"`, capped
  with `stdout_full_capped`, failure reported as `spill_error` without failing the command), spill pruning
  that leaves unrelated files untouched, the straggler drain, and the settings validator including the
  removed `capture_bytes`/`head_ratio` keys and a spill location the model could not read.
