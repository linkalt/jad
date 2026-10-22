> **Note (design_async_manager.md WP7, 2026-10-22):** `tool_smith` is now an async tool and the `list` object is gone: a call with no `run` lists the saved functions, `filters` is top-level and accepts `*`/`?` wildcards, and `run` takes `block_ms` (the script becomes an `async` handle when it outlasts it). Sections 4, 5.1 and 15 below still describe the earlier `list:{filters}` shape; `backend-go/DECISIONS.md` ("`tool_smith` is an async tool") is authoritative.

# Tool Smith — Design and Workflow

Target: jad (Go backend in `backend-go/`). Companion: `plan_tool_smith.md`. Phase-1 spike answers (`.jad/subagents/.shared/docs/T-002/code-explorer_spike.md`) and spawn measurements (`tester_spawn.md`) are folded in below. Items marked *(unverified)* were not run or read.

## 1. Overview

Tool Smith is a dynamic tool for the agent harness. An agent can discover reusable functions, execute TypeScript that composes them (and jad's native tools) in one call, and keep useful workflows as editable files.

Reusable functions are plain TypeScript files that agents create and change with their existing `write`/`edit` tools. One-off code goes straight into `run`.

The core principle is a small, stable model-facing API while the function library evolves.

**The native-tool bridge is a core requirement (user decision).** `tool_smith` is a dynamic, optimized codemode-style tool: scripts call jad's own tools as `tools.<name>(args)` with parsed results, under Go-enforced policy, approvals and limits. A skill plus `exec` cannot provide that, so the bridge is not optional and not gated on a prototype.

### Goals

- Minimize model-facing tool definition size (it is sent on every request).
- Reduce model-to-tool round trips by composing calls inside one script, and return only the final, shaped result.
- Let agents create, modify and reuse workflows.
- Discover existing capabilities before writing new ones.
- Give scripts policy-enforced access to native tools (the bridge, 5.4).
- Keep every limit in config (`tools.native.tool_smith.*`), enforced by Go, not by the script.

## 2. Architecture

### 2.1 Agent harness

The agent reasons, discovers functions, composes scripts and decides what becomes reusable. It manages function files with `write`/`edit` and reads them with `read`/`grep`; Tool Smith has no create, edit, delete or get operation.

### 2.2 Tool Smith API

One native tool, `tool_smith`, with two top-level parameters: `list` and `run` (section 4).

### 2.3 Function library

Editable `.ts` files in a workspace directory (default `.jad/functions`, key `functions_dir`). Native tools (`read`, `grep`, `glob`, ...) are not library files: scripts reach them through the bridge as `tools.<name>(args)` (section 5.3). Library functions are called as `fn.<name>(input)`.

### 2.4 Execution runtime

An external interpreter (default bun, run through the `interpreter` argv with a `{file}` placeholder; node 22+ with type stripping also works). Go owns everything that must be enforced: process start, timeout, output limits, the nested-call bridge, approvals, cancellation and cleanup. There is no embedded JS engine and no persistent runtime process: each `run` starts a fresh child, so there is no module cache to invalidate. Measured warm spawn of a trivial `.ts` file: bun 1.4.3 about 4 ms, node v26.12.0 about 54 ms; deno is not installed. There is no built-in `ts` lang today (defaults are python, node, bash, sh, ruby, perl: `backend-go/internal/sessionfs/native_exec_settings.go:75-83`); `lang` entries are argv lists, so `bun --preload <prelude> {file}` is accepted by the parser (`native_exec_settings.go:~225-245`; that bun accepts the flag was not run *(unverified)*).

## 3. Layout and conventions

    <workspace>/.jad/functions/
    ├── inspect_project.ts
    ├── trace_callers.ts
    └── audit_changes.ts

The Go side owns the runner and a small embedded prelude (written into a private per-run directory, like `exec`'s `file` code delivery). There is no runtime TypeScript tree, registry, loader or docs directory.

### Function conventions

- One primary function per file; the file name (`^[a-z][a-z0-9_]*$`) is the function name.
- The file starts with a header comment: first line is a one-line description. The documentation lives only there (no separate `docs/*.md`).
- The file exports `async function <name>(input)`; it may use the global `tools` and `fn`.

Example:

    // Search project code for a pattern and return matching paths with lines.
    export async function inspect_project(input: { query: string; path?: string }) {
      const r = await tools.grep({ pattern: input.query, path: input.path ?? "." });
      return r.content;
    }

Types are not checked at run time (bun and node only strip them); validate JSON input explicitly when it matters.

## 4. Public API

### 4.1 Schema

    {
      "type": "object",
      "properties": {
        "filters": { "type": "array", "items": { "type": "string" } },
        "run": {
          "type": "object",
          "properties": {
            "function": { "type": "string", "description": "TypeScript async function body." },
            "input": { "type": "object", "additionalProperties": true },
            "block_ms": { "type": "integer", "minimum": 0 }
          },
          "required": ["function"],
          "additionalProperties": false
        }
      },
      "additionalProperties": false
    }

No `required` at the top level and no `action` enum. Validation is forgiving, because a refused call costs a model round (the `exec` trial: 3 of 25 calls refused over a missing `lang`, `backend-go/DECISIONS.md:265`):

- `NormalizeArguments` accepts `run` as a plain string (the body), and the aliases `script`/`code`/`body` for `function`.
- If both `filters` and `run` are given, `run` wins and the result notes `ignored: filters`. A call with neither lists the library.
- Provider schema dialects: adapt only if a provider rejects the schema.

### 4.2 Parameter summary

| Parameter | Purpose |
|---|---|
| `filters[]` | Case-insensitive; `*`/`?` make a wildcard pattern, else a substring; matched on function name and description. |
| `run.block_ms` | Most time to block for the result; a longer script continues as an `async` handle. |
| `run.function` | Async TypeScript function body. |
| `run.input{}` | JSON data bound to the `input` variable. |

## 5. Operation behavior

### 5.1 `list`

Discover library functions without loading them into context. Output is plain text, one line per function, no JSON:

    inspect_project(input: {query, path?}) — Search project code for a pattern
    trace_callers(input: {symbol}) — Find callers of a symbol

Rules:

- Go scans `functions_dir`, reads only the first `list_header_bytes` of each file and derives the description from the header and the signature from the exported function line.
- At most `list_max_files` files are scanned and `list_max_results` lines returned; a cut result says `truncated` and suggests a filter. No pagination.
- Empty library and scan failure are different results (`ok` with `0 functions` versus `error`).
- No cache: reading a small header of a few dozen files per call is cheap. Add an (path, mtime, size) cache only if measured.

`list` is part of the tool. Plan phase 8 measures its token and round cost against `glob`/`grep` discovery.

### 5.2 Function source and docs

There is no `get`. The agent reads `functions_dir/<name>.ts` with `read` (the workspace is inside the default path policy). One source of truth, no extra operation or schema text.

### 5.3 `run`

Execute a TypeScript function body with optional `input`.

    { "run": { "function": "const r = await tools.grep({pattern: input.query}); return r.content.slice(0, 2000);",
               "input": { "query": "TargetFunction" } } }

Go writes a wrapper into a private per-run directory (mode 0700): it loads the prelude, binds `input`, runs the body as an `async (input) => { ... }` and delivers the return value through a result channel in the style of `$EXEC_RESULT` (`backend-go/DECISIONS.md:261`). `input` is delivered as a JSON file, so data never needs escaping inside code. Body plus input are bounded by `code_max_bytes`.

Bindings in the script:

- `input`: the JSON object (empty object when omitted).
- `tools.<name>(args)`: allowlisted native tools via the bridge (5.4). Returns `{ok, status, summary, content, info}` parsed from the native result envelope (`backend-go/internal/tools/output.go:20-60`); it does not throw for tool-level failures, so one failed call in a `Promise.all` does not abort the others.
- `fn.<name>(input)`: library functions. Before the child starts, Go validates the library like `list` and copies the valid files into the run's private temp dir; the prelude imports a copy on the first call. A name that is not in the manifest never reaches a path. Saved functions may use the globals `tools` and `fn`. A malformed file (including one over `function_max_bytes`) throws `function X is malformed: <problem>`; an unknown name throws `no saved function X; available: ...`.

The body can use normal control flow, `await`, `Promise.all` (limits in 5.4), filtering and shaping, and returns one value. Bodies are not saved automatically.

### 5.4 Bridge to native tools (required)

The child talks to Go over one multiplexed per-run channel (a Unix socket in the private per-run temp directory made by `os.MkdirTemp`, mode 0700; newline-delimited JSON frames with ids). The socket path and a per-run random token reach the child as environment variables through `shellChildRun.extraEnv` (`backend-go/internal/tools/native/bash.go:158-159`, as exec does for `EXEC_RESULT`), so there is no extra fd and `runShellChild` needs no change (`bashChildRequest` has no ExtraFiles, `bash_child.go:31-48`). Loopback HTTP is not used. Go dispatches each frame through `Catalog.ExecuteFunctionCall` on a filtered nested catalog (`backend-go/internal/tools/catalog.go:318-404`), so schema validation, normalization, approval and panic recovery apply. The nested catalog is built from the hook-wrapped native tools in the style of `childToolPools` (`backend-go/internal/tools/native/subagent.go:150-166`).

- Allowlist: `tools.native.tool_smith.tools`, default the read-only tools (`read`, `grep`, `glob`). Writers only if listed. `bash`, `exec`, `process`, `subagent`, `tool_smith` and `task` are always excluded, so recursion is impossible by construction.
- Sub-agents never get `tool_smith`: it is `ReadOnly: true` (section 15), and `childToolPools` puts every `ReadOnly` native tool except the subagent into a sub-agent's read-only pool, so `childToolPools` (`backend-go/internal/tools/native/subagent.go:162-170`) gets an explicit exclusion of the `tool_smith` tool ID next to the `subagentToolID` skip. The nested catalog uses the same exclusion.
- Concurrency: non-read-only nested calls run one at a time (mutex); read-only ones run in parallel up to `max_parallel_calls`. `max_nested_calls` caps the total.
- Approval: a nested call with `approval: ask` prompts through a wrapped `RequestApproval`, as the subagent child does (`backend-go/internal/tools/native/subagent_call.go:58-73`). Approval of the outer `run` never approves nested writes.
- Each nested result is cut at `nested_result_max_bytes` (UTF-8 safe, `truncated` in `info`).
- Nested calls are not written to `messages.jsonl` (section 10).

## 6. Function composition

Composition is the main value. Rules for scripts:

1. Reuse library functions and native tools instead of re-implementing them.
2. `Promise.all` only for independent operations; await results later steps depend on.
3. Keep intermediate results in script variables; do not read the same input twice.
4. Filter, count and summarize inside the script; return only what the task needs, as compact data (not pretty-printed, no logs).
5. When several outputs derive from one expensive input, compute them in one pass.

## 7. Persistent workflow evolution

- Ad hoc: `run.function` for one-offs and experiments.
- Persistent: when a workflow proves reusable, the agent writes `functions_dir/<name>.ts` with `write`, tests it with `run`, and finds it later with `list`.

Cycle: discover, identify a gap, choose (reuse / compose / persist), implement, test, reuse, improve on evidence. Do not persist every temporary script.

## 8. Agent instructions

Guidance lives in a skill, `.jad/prompts/skills/tool-smith/SKILL.md` (loaded on demand by the `skill` tool), not in the tool description. The description stays about five static lines.

Skill content:

- Use `list` first; read a function file before changing it.
- Prefer one `run` over several tool calls when steps are mechanical; do not use it for a single call.
- Return shaped, small values; never print what you can return.
- Persist only reusable workflows; test them before relying on them; keep changes focused.
- Report unrelated bugs without expanding the task.

## 9. Freshness (replaces dynamic reload)

Every `run` starts a new process, and the library is copied at the start, so the file content at that moment is what runs. No watching, hashing, cache invalidation or registry versions. A syntax or import error is the child's own error on stderr, returned to the agent. A script that edits a function file during the same run does not change what `fn` calls in that run; the next run sees the edit.

## 10. Results, errors and audit

`run` returns the standard native envelope (`RenderToolResult`), not a second `{ok, error}` shape:

- Success: content is the return value (strings raw, other values as compact JSON), cut at `result_max_bytes` with `truncated` and a visible marker that states how many bytes were cut. v1 does not spill: exec's spill covers the stdout/stderr streams only (`planBashSpill`, `backend-go/internal/tools/native/bash.go:212`), and the return channel is cut by `readShellResultFile` (`bash.go:~237-262`). Script stdout/stderr follow `exec`'s rule (`[result]` then `[output]`; `backend-go/DECISIONS.md:261`).
- Failures map onto existing statuses and fields: `invalid_input` (bad parameters, oversized code), `blocked` (approval denied), `error` (script exception, bridge failure), `exit_code`, `timed_out`, `truncated`, and `interpreter_unavailable` when bun is missing (`DECISIONS.md:263`).
- Audit: counters only, as `Information` lines: nested `calls`, per-tool counts, nested write count, bytes returned, duration. A nested write count makes partial completion visible. No nested-call log.
- Nested calls and the bridge produce no transcript items. Only the outer `function_call`/`function_call_output` reach `messages.jsonl`, as for any tool. Sessions without `tool_smith` are byte-identical.

## 11. Security and reliability

The interpreter is a child process, **not a sandbox**. A script can use raw `fs`, `fetch` or `child_process` and bypass tool policy, hooks, path policy and checkpoints of the nested calls. `run` therefore has the trust level of `exec`/`bash`:

- `tool_smith` has its own bash-style `Child` settings block, as `exec` does (`backend-go/internal/sessionfs/native_exec_settings.go:~62-65,~125`, parsed with `normalizeBashStyleFields`): environment allowlist, kill grace, stdout/stderr budgets and spill keys are its own, with the same defaults. It does not depend on `tools.native.bash` being configured.
- `approval` (`off`/`ask`) gates the whole `run`. Approving it does not approve nested writes.
- The bridge is a policy-enforced path, not an exclusive one. A hard boundary needs a different interpreter (for example deno with `--allow-*` flags through the `interpreter` key; not installed here) and is out of scope.
- Enforced by Go: timeouts (`timeout_ms` clamped by `max_timeout_ms`), output and nested-result limits, call counts, process kill and temp/socket cleanup on success, timeout and cancel.
- Idempotence: scripts may have side effects before failing; the counters show how many nested writes happened.

## 12. Efficiency

In order of expected value:

1. Fewer model rounds: one `run` replaces several mechanical tool calls.
2. Small returns: shaped values, compact JSON, plain-text `list`, no logs.
3. Small fixed cost: static short tool description, guidance in a skill, no dynamic list in the definition (it would break the prompt prefix and cost tokens every turn).
4. Cheap spawn: one process per run; the prelude is small; library functions load lazily.
5. Parallel reads inside a run, serialized writes.

Measured, not assumed: the plan's phase-2 baseline (rounds and tokens of plain tool calls for reference tasks; the spawn part is done, bun about 4 ms versus node about 54 ms warm, so bun is the default) is compared with the same tasks in phase 8. A `run` does not reduce model API requests if the agent calls it many times for single steps.

## 13. Implementation plan

See `plan_tool_smith.md` (8 phases): the evidence spike (phase 1) is done and the spawn measurement is done (token baseline pending); then config/registration (phase 3, three steps 3a-3c), `run`, the bridge, `list`, tests with golden checks, and finally skill, review and live trial. There is no go/no-go gate; the bridge is built.

## 14. Non-goals

- Create, edit, delete or get operations; a model-facing tool per function; a `name` parameter for `run`.
- Dynamic reload, module caches, registry versions, file watching.
- A persistent runtime process or an embedded JS engine.
- Metrics beyond result counters.
- A nested-call transcript or log.
- An OS sandbox, or TypeScript as a security boundary.
- Cross-call result caches.

## 15. Final design decisions

| Area | Decision |
|---|---|
| Public tool | `tool_smith` |
| Operations | no parameters/`filters` = list, `run` (no `get`, no `action` enum) |
| Discovery | `filters[]` (wildcards), plain-text compact output |
| Source inspection | existing `read` on `functions_dir/<name>.ts` |
| Execution code | `run.function` (+ aliases, string form) |
| Dynamic data | `run.input{}`, delivered as a JSON file |
| Runtime | external bun (or node) child process per run, via the `interpreter` argv |
| Native tools | `tools.<name>` over a Go-owned bridge; **required**; read-only allowlist by default |
| Library | `.jad/functions` in the workspace, edited with existing tools |
| Freshness | new process per run; no reload machinery |
| Audit | counters in result `Information` |
| Trust | exec/bash level; no OS sandbox |
| Config | every limit is a `tools.native.tool_smith.*` key (default, schema, test, `DECISIONS.md`); own bash-style `Child` block, no dependency on `tools.native.bash` |
| Scheduling | `ReadOnly: true`; `MayChangeFiles` true only for calls containing `run` (serial, checkpointed); `list` parallel |
| Sub-agents | never get `tool_smith` (explicit exclusion in `childToolPools`) |
| Bridge channel | Unix socket in a private temp dir; path and token via env vars; no extra fd |
| Oversized return value | cut at `result_max_bytes` with a visible truncated marker; no spill in v1 |
| Frontend | no change (tool cards are generic, `frontend_new/src/components/TranscriptParts.tsx:386-440`) |

Keys: `functions_dir`, `interpreter`, `code_max_bytes`, `result_max_bytes`, `list_max_results`, `list_max_files`, `list_header_bytes`, `function_max_bytes`, `tools`, `max_nested_calls`, `max_parallel_calls`, `nested_result_max_bytes`, `approval`, plus the bash-style `Child` keys (`timeout_ms`, `max_timeout_ms`, kill grace, stdout/stderr budgets, spill) shared with `exec` through `nativeBashStyleKeys` (`native_exec_settings.go:~40,~125`); the schema must list exactly the keys the loader accepts (`native_tool_schema_test.go:122`).

Proposed defaults (from the spike; byte keys have minimum 1 and maximum `maxNativeToolBytes` = 10485760, `backend-go/internal/sessionfs/tool_settings.go:20`):

| key | default | min | max |
|---|---|---|---|
| `functions_dir` | `.jad/functions` | non-empty | read path rules |
| `code_max_bytes` | 262144 (= exec) | 1 | 10485760 |
| `result_max_bytes` | 16384 (= exec) | 1 | 10485760 |
| `nested_result_max_bytes` | 16384 | 1 | 10485760 |
| `timeout_ms` / `max_timeout_ms` | 120000 / 600000 (bash-style Child) | 1 | Child max |
| `max_nested_calls` | 100 | 1 | 10000 |
| `max_parallel_calls` | 4 | 1 | 64 |
| `list_header_bytes` | 2048 | 1 | 65536 |
| `function_max_bytes` | 65536 | 1 | 10485760 |
| `list_max_files` | 200 | 1 | 10000 |
| `list_max_results` | 50 | 1 | 1000 |
| `tools` | `["read","grep","glob"]` | unique native names | not in {bash, exec, process, subagent, tool_smith, task} |
| `approval` | `off` | `off`/`ask` | |

## 16. Lessons taken from harness2

harness2 (google-research) is not a codemode tool; only these ideas about editable file libraries are adopted:

- **Validate and report everything at once.** There is no save hook (agents use `write`), so validation happens in `list` and `fn.<name>`: a malformed file (bad name, missing header, no exported function) is reported in one message listing all problems, never dropped silently.
- **Short index, bodies on demand.** `list` shows name, signature and description only; bodies are read with `read`. Always-on text stays small: static tool description of about five lines, guidance in a skill, list output capped by `list_*` keys.
- **Visible truncation with a floor.** A cut result carries a marker with counts (for example `+N bytes`, `M of K functions shown`). `result_max_bytes`, `nested_result_max_bytes` and the `list_*` caps have a schema minimum (set in the config phase, recorded in `DECISIONS.md`) so a cap cannot be so small that the model is misled by a fragment.
- **Whitelist and escape check.** Function names match `^[a-z][a-z0-9_]*$`; paths are built only from validated names, and `..`, absolute paths and symlinks leaving `functions_dir` are rejected in both `list` and `fn` imports.
- **Avoid:** silent drops and hard-coded limits (every limit stays a `tools.native.tool_smith.*` key).

## Revision notes

Changes from the rewritten (efficiency) draft:

- Removed: `get`, `docs/*.md`, the runtime TypeScript tree (registry/loader/executor/permissions), dynamic reload (old section 9, now section 9 "Freshness"), persistent runtime, phase-4 metrics, "runtime reuse" and "registry version" language, the `{ok, error:{code}}` result shape, and filesystem/network "restrictions" a child process cannot enforce.
- Kept: no create/edit/delete operations, top-level operation names instead of an `action` enum, `run.function` plus dynamic `run.input`, composition rules, workflow-evolution cycle, non-goals, "TypeScript is not a sandbox".
- Changed (this revision): the bridge is a core requirement by user decision; the plan's phase 1 is no longer a go/no-go gate but a baseline measurement; harness2 lessons added (section 16).
- Changed: native tools are bridge calls (`tools.<name>`), not library files; allowlist, always-excluded set, approval, serialization and audit counters are specified; errors reuse the native envelope; config keys listed.

jad-specific optimizations proposed (to be confirmed by measurement):

- **List/run token cost:** `list` is plain text, one line per function, capped and `truncated`; `run` returns a shaped value as compact JSON, with stdout only per `exec`'s rule; nested results arrive pre-parsed (`{ok, status, summary, content, info}`) so scripts do not parse envelope text. The tool description stays static (prompt-cache friendly), guidance is in a skill.
- **Spawn cost:** one child per run, a tiny prelude, lazy `fn` imports, no `npm install`/heavy imports. Measured warm spawn: bun about 4 ms, node about 54 ms, both small against a model round, so a persistent runtime is not reconsidered unless phase 8 shows otherwise.
- **Bridge batching:** one multiplexed connection per run; `Promise.all` gives parallel reads without a batch API. No `tools.batch` sugar.
- **Within-run memo:** not built. The audit counters may add a `duplicate_calls` count (identical read-only name and arguments); build a memo only if phase 8 data shows duplicates matter.
- **Big results (decided):** a returned value above `result_max_bytes` is cut with a visible truncated marker and a hint to return less. No spill in v1: exec's spill covers streams only and a spilled return value would need new code (`bash.go:212`, `bash.go:~237-262`).
- **Forgiving schema:** no top-level `required`, string form and aliases for `run`, `run` wins when both keys are given.
- **Checkpoint and scheduling (decided, spike-confirmed):** `ReadOnly: true` with `MayChangeFiles` true only for calls that contain `run`. `roundChangesWorkspace` takes the checkpoint when `CallMayChangeFiles` is true (`backend-go/internal/session/runner_tools.go:152-170`); the checkpoint is a whole-workspace snapshot taken before the round, so nested and raw `fs` writes are covered (`checkpoint/service.go:417`, `checkpoint/run.go:63-79`). `ChangesState = !ReadOnly || CallMayChangeFiles` (`backend-go/internal/tools/catalog.go:247-261`) makes `run` serial and `list` parallel; `subagent` is the precedent (`native/subagent.go:245-246`). The only other `ReadOnly` reader is `childToolPools` (`subagent.go:162-170`), hence the explicit exclusion. `MayChangeFiles` sees normalized arguments, so the string form of `run` works (`catalog.go:267-293`).
- **Persistence (confirmed):** `function_call_output` is appended through `appendToolOutputs` (`runner_tools.go:~100-135`, `transcript/input_history.go:22,39`); no byte cap found, so the only cap is per tool.
- **Native bridge and settings (decided):** own `Child` block, not a bash dependency; Unix socket plus env var.
- **Dropped from the earlier plan:** the mtime cache for `list` (replaced by a bounded header read); a `get` of any kind.
- **Bridge (decided):** required. At exec trust a script can already use raw `fs`, but the bridge adds policy (path rules, approvals, hooks, native truncation behavior), parsed results and counters, which skill plus `exec` cannot give. Its cost is measured in phase 8, not used to decide whether to build it.
