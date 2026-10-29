> **Superseded in part by design_async_manager.md:** the process tool now has only `command`, `stdin`, `cwd` and `block_ms`; wait, status, list and stop moved to the `async` tool, and `wait_timeout_ms`/`max_wait_timeout_ms` were removed in favour of the async manager's `default_block_ms`/`max_block_ms`.

# Native `process` Tool Design

**Status:** Implemented

**Scope:** Session-owned long-running shell processes under `backend/`

## 1. Goal

`process` starts a command that may outlive an agent turn, then lets the agent inspect, wait for, list, and stop that same process. It is intentionally distinct from `bash`:

- `bash` is foreground, has one bounded call, and cleans up its process group when done.
- `process` is background, has an opaque process id, and retains its process group until it exits or is explicitly stopped.

The tool follows the `agent-tool-design-guidelines` skill: common calls express only intent, backend policy supplies limits and retention, validation normalizes harmless variants, and results stay bounded.

## 2. Scope and safety boundary

A managed process belongs to one **AgentSession** and is visible only to that session. Its id is opaque and unguessable. It survives provider/tool rounds and browser disconnects, but not an orderly session or backend shutdown:

1. `AgentSession.close()` stops all of its running managed process groups.
2. Server shutdown calls session close and waits for the configured TERM-to-KILL ladder.
3. After an unclean backend loss, persisted records that were `running` are changed to `lost` on the next session open. The backend must not signal a recorded PID after restart: PID reuse makes that unsafe.

This is deliberately not a durable operating-system supervisor. A process can escape its group with `setsid`/a new session, as with `bash`; the tool is not a sandbox. A process that must survive backend restart requires a separate, explicitly designed supervisor/daemon with authenticated ownership and PID-start-time identity validation.

The first implementation is POSIX-only. It starts each shell in a separate process group and targets that group with TERM then KILL. Refusing unsupported platforms is safer than falsely claiming to control a process tree.

## 3. Agent interface

One coherent lifecycle tool is preferable to five near-identical tool names. `action` is optional where the operation is safely inferable.

```ts
process({command, cwd?})                         // start
process({id})                                    // status, including bounded logs
process({})                                      // list all session processes
process({action: "list", filter?: ProcessState})
process({action: "wait", id, timeout_ms?})
process({action: "stop", id})
```

### 3.1 Provider schema

```json
{
  "type": "object",
  "properties": {
    "action": {
      "type": "string",
      "enum": ["start", "status", "list", "wait", "stop"],
      "description": "Omit when command, id, or no fields already makes the intent clear."
    },
    "command": {
      "type": "string",
      "minLength": 1,
      "description": "Shell command to start."
    },
    "cwd": {
      "type": "string",
      "minLength": 1,
      "description": "Working directory for start. Defaults to the session workspace."
    },
    "id": {
      "type": "string",
      "minLength": 1,
      "description": "Id returned when the process was started."
    },
    "filter": {
      "type": "string",
      "enum": ["running", "exited", "stopped", "failed", "lost"],
      "description": "Optional state filter for list."
    },
    "timeout_ms": {
      "type": "integer",
      "minimum": 1,
      "description": "How long wait may block. Defaults to the configured short polling interval."
    }
  },
  "additionalProperties": false
}
```

There is no separate logs action. `status`, `wait`, and `stop` return separate bounded head-and-tail excerpts for stdout and stderr; `list` returns metadata only.

### 3.2 Normalization and semantic validation

Normalize before AJV validation:

- Trim `command`, `cwd`, `id`, and `filter`; treat an empty `filter` as absent.
- Normalize action/filter case and harmless aliases: `run`/`launch` -> `start`, `kill`/`terminate` -> `stop`, `state`/`info` -> `status`, `all` -> no filter.
- Infer `start` when only `command` is supplied; infer `status` when only `id` is supplied; infer `list` when neither a command nor an id is supplied, including when only `filter` is supplied.
- Omitted `cwd` uses the session workspace. Omitted wait timeout uses `tools.native.process.wait_timeout_ms`; a larger supplied timeout is clamped to `max_wait_timeout_ms`.

Hard errors are limited to ambiguous or unsafe intent:

- `command` and `id` together without an explicit, compatible action;
- an action missing its required target (`start` needs command; `status`, `wait`, and `stop` need id);
- `filter` outside list;
- process id absent from this session;
- `stop` requested for a `lost` process, because no trusted live handle exists;
- invalid/non-directory/denied `cwd`; unavailable shell; process capacity reached.

`stop` is idempotent for an already terminal process: return its terminal snapshot rather than creating a retry loop. `wait` timing out is also a normal result (`state: "running", waited: false`), not an error.

## 4. States and compact results

States are `running`, `exited`, `stopped`, `failed`, and `lost`.

- `exited` means the shell exited; `exit_code` or `signal` identifies success/failure.
- `stopped` means this tool initiated TERM/KILL.
- `failed` means the shell could not be started or the manager failed before a valid child existed.
- `lost` means a prior backend instance disappeared while the process was recorded as running. Its logs remain inspectable, but it cannot be controlled.

All outputs use the existing compact JSON result renderer. Examples omit normal false/empty fields.

```json
{"status":"ok","action":"start","id":"proc_…","state":"running","pid":1234,"cwd":"."}
```

```json
{
  "status":"ok",
  "action":"status",
  "id":"proc_…",
  "state":"running",
  "pid":1234,
  "stdout":{"text":"…latest output…","shown_bytes":4096,"produced_bytes":12000},
  "stderr":{"text":"…","shown_bytes":120,"produced_bytes":120}
}
```

```json
{"status":"ok","action":"wait","id":"proc_…","state":"running","waited":false}
```

```json
{
  "status":"ok",
  "action":"list",
  "processes":[
    {"id":"proc_…","state":"running","pid":1234,"cwd":".","started_at":"…"}
  ]
}
```

A stream whose disk cap was reached includes `stored_bytes`, `produced_bytes`, and `truncated: true`. Line rendering uses the existing ANSI/control stripping and configured line-length policy. Neither `list` nor routine status echoes the original command, avoiding needless repetition and accidental secret disclosure.

## 5. Configuration

Add `tools.native.process.storage_dir` to session settings. Relative directories resolve beneath the selected profile's `paths.profiles.<name>.tool_storage` root, or from `setting.cwd` when no profile is selected; absolute paths are used as-is. The profile root defaults to `<cwd>/.jad/profiles/<profile-name>/tools`. Same-profile sessions and explicit shared paths still share storage without write coordination. All limits are configuration, not call-site constants. The process policy inherits `bash` values where that produces consistent behavior; explicit process values override them.

```ts
interface NativeProcessToolSettings extends NativePathPolicy {
  shell: string;                    // default: bash shell
  login_shell: boolean;             // default: bash setting
  kill_grace_ms: number;            // default: bash setting
  max_running: number;              // default: 8
  wait_timeout_ms: number;          // default: 10_000
  max_wait_timeout_ms: number;      // default: 120_000
  stdout: NativeBashStreamBudget;   // default: bash stdout head/tail budget
  stderr: NativeBashStreamBudget;   // default: bash stderr head/tail budget
  log_cap_bytes: number;            // default: 8 MiB per stream
  max_line_chars: number;           // default: bash setting
  strip_ansi: boolean;              // default: bash setting
  env_file?: string;                // optional strict dotenv; session-relative or absolute; own setting, not inherited from bash
  env: Record<string, string>;      // safe defaults, then env_file, then inline env (inline wins)
  env_deny: string[];               // removed last; API key always denied
  retention_days: number;           // default: 14; 0 disables pruning
  max_completed: number;            // default: 100 retained terminal records
  storage_dir: string;              // default: .processes under profile tool_storage (or setting.cwd without a profile)
  paths: NativePathRule[];          // default: bash paths, then write paths
}
```

`storage_dir` defaults to `.processes` below the active profile's `tool_storage` root, or below `setting.cwd` when no profile is selected; absolute paths are used as configured. It is harness-configured storage, not a model argument. `retention_days`/`max_completed` prune terminal records and their log directories only; they never prune `running` records.

The configuration validator must reject incompatible bounds (for example `max_wait_timeout_ms < wait_timeout_ms`, zero capacities, oversized log/status limits, invalid paths, or a retention policy that could remove active records). The legacy `status_log_bytes` setting remains tail-only compatibility input and cannot be combined with explicit stream budgets. The shipped setting schema must document every default and the native definition manifest must exactly match the in-code tool schema.

## 6. Runtime architecture

### 6.1 `ProcessManager`

Create one `ProcessManager` per `AgentSession`, injected into the process native tool when the catalog is built. It owns:

- a map of trusted live child handles by opaque id;
- per-process lifecycle promises used by `wait`;
- bounded stdout/stderr head-and-tail collectors;
- atomic manifests and capped append-only logs in the process storage path;
- pruning and `close()`.

`ToolCatalog` needs an optional async close hook, or `AgentSession` must own the manager independently and close it before closing the response store. A manager cannot be instantiated per tool call because status/wait/stop need the original child handle.

### 6.2 Start

1. Resolve and authorize `cwd` with the same policy and symlink protections as `bash`.
2. Reject if the session has reached `max_running`.
3. Create `<processes>/<id>/` and atomically persist a `starting` manifest.
4. Spawn the configured shell with `detached: true`, closed stdin, safe environment, and piped stdout/stderr.
5. Attach stream readers immediately. They append only up to `log_cap_bytes` per stream while independently maintaining each stream's configured status head and tail; they never buffer unbounded output in memory.
6. Persist `running` with the PID only after spawn succeeds. On `error` or `close`, persist a terminal manifest, resolve waiters, and retain the logs according to policy.

The `start` call does **not** use the foreground `ToolExecutionContext.signal`: cancelling an agent run must not accidentally kill a process just created for later observation. Session/backend close is the owner cancellation path.

### 6.3 Status, wait, list, and stop

- `status` reads the live record or persisted terminal manifest and includes each stream’s bounded head-and-tail excerpt. It does not block.
- `wait` subscribes to the live process completion promise until the configured/clamped deadline. On timeout it returns the same bounded status with `waited: false`; on exit it returns `waited: true` and the final logs.
- `list` reads live and terminal manifests, optionally filters one state, sorts newest first, and limits records using backend `max_completed` policy. It never includes logs.
- `stop` signals the trusted process group with SIGTERM, waits `kill_grace_ms`, then SIGKILLs the group if necessary. It returns the final status/log snapshot. A process that moves itself to another session can escape this guarantee and must be reported `left_running: true`.

## 7. Persistence and recovery

Each process directory contains:

```text
<storage_dir>/<id>/
  manifest.json
  stdout.log
  stderr.log
```

The manifest records only operational metadata: schema version, id, state, cwd display path, timestamps, PID while live, exit code/signal, stop reason, per-stream produced/stored counts, and truncation flags. It does not persist the command or environment.

At manager open, scan manifests before exposing the tool. Any nonterminal manifest is atomically converted to `lost` with `ended_at` and `stop_reason: "backend_restarted"`; keep its logs for inspection. Malformed records are isolated and reported as `failed` diagnostics rather than preventing the entire session from loading.

## 8. Files and tests for implementation

Expected implementation areas:

- `backend/src/config.ts` and `sample/schemes/setting.schema.json`: process storage path, policy type, defaults, normalization, and validation.
- `backend/src/tools/native/process.ts`: tool schema, normalization, compact rendering, and execution dispatch.
- `backend/src/tools/native/process-manager.ts`: child ownership, manifests, per-stream head/tail/cap handling, waiters, cleanup, and recovery.
- `backend/src/tools/native/registry.ts`, `backend/src/tools/types.ts`, `backend/src/tools/catalog.ts`, and `backend/src/session.ts`: registration, manager injection, and deterministic session shutdown.
- `sample/tools/native/process.json`: opt-in manifest entry.
- `backend/test/process-tool.test.ts`, config tests, catalog definition parity test, and server/session-close tests.

Required regression coverage:

1. minimal start/status/list/wait/stop calls and alias normalization;
2. command/cwd/path-policy errors, secret removal, and tool-definition parity;
3. timeout returns running rather than an error; exit and nonzero exit state are correct;
4. status reports bounded stdout/stderr and cap truncation without memory growth;
5. list filter excludes other states and never includes log bodies;
6. stop kills descendants in the managed POSIX process group and is idempotent after exit;
7. session/server close terminates all live processes and leaves no child behind;
8. abnormal-restart recovery marks live manifests `lost` without signaling recorded PIDs;
9. retention removes only terminal, expired/excess records and logs.
