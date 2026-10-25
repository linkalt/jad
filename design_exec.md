# Design: native exec with saved helpers per language

Status: IMPLEMENTED (revision 3; replaces the "function mode" design shipped in 962bffa..42a90d2). As built: the default `functions_dir` is `.jad/exec`; the listing is grouped by runner with names as written and a malformed entry is reported as `<runner>/<file>`; helper names are `^[A-Za-z0-9][A-Za-z0-9_.-]*# Design: native exec with saved helpers per language

; the cwd stays the workspace; the unverified claims in section 4 were checked with real interpreters (bash, python3, node, ruby, perl). exec is experimental; a clean break is fine. tool_smith is unchanged.

## 1. What changes and why

The shipped exec had a top-level `function` mode: the model named a saved file and exec ran it. That is not what was wanted. Saved files are HELPERS that running code calls itself and chains (like `fn.read` inside tool_smith). So:

- Removed: modes `function` and `argv`, the `args` parameter, the per-call `timeout_ms` parameter.
- Kept: `code` + `lang`, JSON `input` on stdin, `$JAD_RESULT`, `block_ms` async handle, caps, scratch-directory cleanup and startup sweep.
- New: per-language helper directories exposed to the running code through the interpreter's own search path, and `JAD_FUNCTIONS_DIR`.

No native bridge: a script cannot call JAD tools (read/grep/glob). There is no template/prelude injection and no server callback or socket: a helper is called by the language's own mechanism (import, require, PATH lookup, subprocess).

## 2. Input schema (flat)

    {"type":"object","properties":{
      "lang":    {"type":"string","minLength":1,"description":"Interpreter name or alias; defaults to bash."},
      "code":    {"type":"string","minLength":1,"description":"Program source."},
      "input":   {"description":"Any JSON value, written to stdin as one line of JSON."},
      "filters": {"type":"array","items":{"type":"string"},"description":"Listing only: ..."},
      "cwd":     {"type":"string","minLength":1},
      "block_ms":{"type":"integer","minimum":0}
    },"additionalProperties":false}

- `code` present: run. `code` absent: list the helpers (`filters` optional). `lang`, `input` with a listing is invalid_input.
- A plain command is `{"lang":"bash","code":"git status"}`. There is no argv form.
- Refused with the replacement named (RejectedProperties): `function` -> "exec has no function mode; call a saved helper from code (see the list from a call with no parameters)"; `argv` -> "exec has no argv; use code with lang (a command is lang bash, code \"git status\")"; `args` -> "exec has no args; pass data as input (JSON on stdin)"; `timeout_ms` -> "exec has no timeout_ms; the timeout is configured (tools.native.exec.timeout_ms)". Older near-miss names keep their hints.
- `filters` together with `code` stays conflicting_input.

### timeout_ms interpretation

Only the per-call parameter is removed. The configured `timeout_ms` (default) and the 6-hour ceiling stay, so a run cannot hang forever. A run that must outlive the default uses block_ms/handle: the handle is subject to the same configured timeout. `max_timeout_ms` stays an accepted key (shared bash_style block) but no longer has an effect on exec because nothing can request more than the default; it is documented as such.

## 3. Layout and naming

    <functions_dir>/<runner>/<file>

- `functions_dir` (config, relative to the workspace, default `.jad/exec`) is the root; the sub-directory name is a runner name exactly (not an alias): `bash/`, `python/`, `node/`, ...; any configured runner (e.g. `ts`) has its own directory.
- The directory selects the runner, so there is no extension ownership rule any more (the shipped ".sh belongs to the first runner" rule is superseded and its code removed).
- A helper is a regular file in that directory. Its name is the file name as written (`script1.sh`, `script1`, `read.ts`), matching ^[A-Za-z0-9][A-Za-z0-9_.-]*$; dotfiles and sub-directories are not listed (sub-directories stay importable, e.g. a Python package). Unique per runner by construction (a directory). The same name may exist in several runners.
- Description: first leading comment line (#, // or --), after a shebang; a later `input:` line documents the input. No description = listed as malformed.

## 4. How running code reaches the helpers

Per runner, config key `lib_env` (default below) names an environment variable that receives the runner's directory, PREPENDED to the existing value with the OS list separator. Exposed only when the directory exists and resolves inside the workspace.

| runner | lib_env | how code uses a helper (verified with real interpreters) |
|---|---|---|
| bash, sh | PATH | `script1.sh` or `script1 '{"a":1}'`; the file must be executable (chmod +x) and have a shebang |
| python | PYTHONPATH | `import read` (file read.py) |
| node | NODE_PATH | `require("read.js")`; CommonJS only (ESM import ignores NODE_PATH); a configured ts runner on a Node that strips types can `require("read.ts")` |
| ruby | RUBYLIB | `require "read"` |
| perl | PERL5LIB | `use Read;` (file Read.pm) |
| ts (bun --no-install -e) | NODE_PATH | `import { x } from "read"` or `"read.ts"`; ESM, types and top-level await work; helpers import helpers the same way (verified with bun 1.4.4) |

Why `bun` for ts and why `--no-install`: bun honours NODE_PATH for ESM `import` (node does not), with or without extension, in `-e` and file mode. A tsconfig `baseUrl` does not work for `-e`. Without `--no-install`, an unknown bare specifier makes bun fetch from the npm registry (auto-install); `--no-install` turns that off, so a typo fails with "Cannot find package" instead of a network call. `node` stays CommonJS-only by decision: its `require("name.js")` works, and moving ESM helpers there would need a loader flag or a generated file, which the design refuses; use ts for ESM helpers.

Also always set: `JAD_FUNCTIONS_DIR` = absolute real root. Cross-language chaining is a subprocess by path: `$JAD_FUNCTIONS_DIR/bash/script1.sh`, `subprocess.run(["python3", os.environ["JAD_FUNCTIONS_DIR"]+"/python/x.py"], input=...)`. Helpers call other helpers the same way, because the child inherits the environment. Input to a helper is its stdin/args as the caller chooses; the contract between helpers is theirs.

Why no server: nothing needs to call back into JAD. Why no template: the language's own import/PATH is the call helper; injecting text around the user code would change line numbers and error text.

## 5. Calling limits

- No recursion counter in exec: helpers are ordinary processes. A cycle A->B->A is bounded by the one wall-clock timeout (configured, <= 6 h) and by the process group kill, not by a depth limit. Stated honestly as a limit of the no-bridge design.
- Stop/reset/delete/restart: unchanged. Everything runs in the exec child's process group; kill_process_group and kill_children_on_run_end apply, so stopping the handle kills helpers that stayed in the group. A helper that daemonizes or calls setsid escapes, as for bash.
- Output: only the top-level process's stdout/stderr are captured under the existing budgets and spill; a nested process writing to the inherited stdout is part of it. One `$JAD_RESULT` file per call (nested processes inherit the variable and share it).
- Env for nested calls: the child's environment as built (allowlist, env, env_deny, harness credential removed) plus the variables above.

## 6. Containment and resources

- functions_dir is resolved with the tool_smith rules (relative, no .., real path inside the real workspace); a missing dir is not an error for running (no variables set) and an empty listing for listing.
- Listing rejects symlinks that leave the runner directory or the workspace and files over function_max_bytes (malformed). Running cannot enforce this: the interpreter follows symlinks itself. The container is the boundary.
- Python writes __pycache__ into the helper directory; harmless, ignored by the listing.
- Listing is synchronous, bounded by list_max_files (total) and list_max_results. No new goroutines.

## 7. Listing

A call without code lists, grouped by runner in table order:

    bash:
      script1.sh — Print a greeting (input: ...)
    python:
      read.py — ...

filters (same semantics as tool_smith: lower-case, * and ? whole-string wildcards, else substring, all must match name or description). A directory with no runner of that name is reported under "malformed". Truncation reporting as shipped.

## 8. What changes versus the shipped code

- exec.go: planExec reduced to code mode; planExecFunction/planExecArgv/args handling removed; execGiven kept; RejectedProperties extended; `timeout_ms` dropped from schema and from the call (parseBashArguments stays for bash).
- exec_functions.go: scan per runner directory; loadExecFunction, ambiguity and extension ownership removed; header parser and entry reader kept.
- bash.go: shellChildRun gets prependEnv (prepend to the built value).
- native_exec_settings.go: runner key lib_env, functions_dir default, schema, fixtures.
- Tests: function-by-name tests replaced by helper-call tests.

## 9. Test plan

Real interpreters, skip when absent: bash->bash chain, bash->python (cross-language), python->python import chain, node require chain; JAD_FUNCTIONS_DIR set; lib var prepended and original kept; missing dir -> no variable; listing per language, filters, malformed, symlink escape, truncation; old keys function/argv/args/timeout_ms refused with the replacement named; handle works; stop of a handle kills a helper child; scratch cleanup and stdin-writer leak tests stay.

## 10. Phases / commits

1. This design.
2. Remove function/argv/args/timeout_ms modes and their dead code.
3. Per-runner layout, lib_env exposure, JAD_FUNCTIONS_DIR, listing; helper tests.
4. Docs: exec.json, setting.schema.json, DECISIONS.md, fixture.

## 11. Open questions

- Should a depth/cycle guard be added (an env counter helpers may check)? Not without a wrapper; skipped.
- Bash helpers need chmod +x: acceptable, or should exec run non-executable files via `bash file`? Skipped.
