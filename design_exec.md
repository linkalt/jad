# Design: native exec with saved helpers per language

Status: IMPLEMENTED (revision 3; replaces the "function mode" design shipped in 962bffa..42a90d2). As built: the default `functions_dir` is `.jad/exec`; the listing is grouped by runner with names as written and a malformed entry is reported as `<runner>/<file>`; helper names are `^[A-Za-z0-9][A-Za-z0-9_.-]*# Design: native exec with saved helpers per language

; the cwd stays the workspace; the unverified claims in section 4 were checked with real interpreters (bash, python3, node, ruby, perl). exec is experimental; a clean break is fine. tool_smith is unchanged.

## 1. What changes and why

The shipped exec had a top-level `function` mode: the model named a saved file and exec ran it. That is not what was wanted. Saved files are HELPERS that running code calls itself and chains (like `fn.read` inside tool_smith). So:

- Removed: modes `function` and `argv`, the `args` parameter, the per-call `timeout_ms` parameter.
- Kept: `code` + `lang`, JSON `input` on stdin, `$JAD_RESULT`, `block_ms` async handle, caps, scratch-directory cleanup and startup sweep.
- New: per-language helper directories exposed to the running code through the interpreter's own search path, and `JAD_FUNCTIONS_DIR`.

No native bridge: a script cannot call JAD tools (read/grep/glob). There is no server callback or socket. A helper is called by the language's own mechanism (import, require, PATH lookup, subprocess) or, in python, node, ts and ruby, through the global `fn` that a built-in template installs before the user's code runs (section 4); the template is a preload, so it injects nothing into the code itself.

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
- Refused with the replacement named (RejectedProperties): `function` -> "exec has no function mode; call a saved helper from code (see the list from a call with no parameters)"; `argv` -> "exec has no argv; use code with lang (a command is lang bash, code \"git status\")"; `args` -> "exec has no args; pass data as input (JSON on stdin)"; `timeout_ms` -> "exec has no timeout_ms; the timeout is configured (tools.native.exec.timeout_ms)". Older near-miss names keep their hints, except `command`, which is folded into `code` as an alias (equal values accepted, different values refused).
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

Two mechanisms, both set at call time and neither changing the user's code or its line numbers.

**`fn` (python, node, ts, ruby).** A built-in template, embedded in the binary, is written into the call's scratch directory and attached before the user's code runs; it installs a global `fn`. `fn.<name>` looks the helper up in `$JAD_RUNNER_DIR` (= `functions_dir/<runner>`) when it is read, so a helper that the same program wrote a moment earlier is found. A missing helper fails with `no helper fn.<name> in <folder>`; a name that is not a plain helper name (path-like, `..`) fails with an explanation.

**Listing and the call rule.** The listing shows what to type, with the file name second: `fn.read(...)   read.ts — reads a file`, `fn["my-helper"](...)` (js), `getattr(fn, "my-helper")(...)` (python), `fn.my_helper(...)` (ruby), a bare file name for bash and sh. A legend at the top states the rule below. `fn.<name>(...)` calls the helper's entry; the value returned by `fn.<name>` is also the module, so `fn.<name>.<member>(...)` keeps working.

| runner | entry of `fn.<name>(...)` (first match) |
|---|---|
| ts, node | the default export (`module.exports` as a function); the export named `<name>` (also `_` or camelCase for `-` and `.`); the only exported function |
| python | the function named `<name>` (a dash written `_`); the only public function defined in the module |
| ruby | `self.call`; `self.<name>`; the only singleton method of the constant. `fn.<name>` with no arguments returns the constant |

No match is an error that lists the exports and names the member call. A `_template` file written before this rule replaces the new built-in and keeps the old behaviour (`fn.<name>` is the module only).

| runner | attachment (runner config `template`) | `fn.<name>` is | notes |
|---|---|---|---|
| ts | `bun --no-install --preload {template} -e {code}`, template `js` | `require()` of the helper | sync: a helper must not use top-level `await` (its exported functions may be async and are awaited by the caller); `fn["my-helper"]` for a dash |
| node | `node -r {template} -e {code}`, template `js` | `require()` of the helper (`name` or `name.js`) | same |
| python | template `python`: a `sitecustomize.py` in a per-call directory put in front of `PYTHONPATH` | the imported module, re-imported when the file changed place | names are identifiers (`getattr(fn, "my-helper")` for a dash); `importlib.invalidate_caches()` runs before each lookup; a `sitecustomize` that ours shadows (venv, user site, `PYTHONPATH`) is run after ours |
| ruby | `ruby -r {template} -e {code}`, template `ruby` | the capitalised constant (`fn.base` -> `Base`) after `require` of `<name>.rb` | `my_helper.rb` -> `MyHelper` |
| bash, sh | no template | bare command name | PATH lookup is already per call |

Config shape of a runner: `argv`, `mode`, `ext`, `aliases`, `lib_env`, and `template` (`js`, `python` or `ruby`). `js` and `ruby` need `{template}` exactly once in argv; `python` must not have it; a `{template}` without a template is refused at load. The text is the built-in unless `functions_dir/<runner>/_template<ext>` exists (a regular file inside the library, else the call fails with `template_failed`); files starting with `_template` are not listed as helpers. The scratch directory is the call's (`exec-*`), removed with the call, also for handles, and swept at startup like the rest.

**`lib_env` stays**, for helpers that import each other and for code that prefers the language's own way: it prepends `functions_dir/<runner>` to a list variable (existing values are kept, after ours): bash and sh `PATH`, python `PYTHONPATH` (the template directory first), node and ts `NODE_PATH`, ruby `RUBYLIB`, perl `PERL5LIB`. `fn` is a global of the user's code; a helper that wants another helper writes `fn.x` too (the global is there) or imports it. Perl has no template in v1.

| runner | the language's own way (with `lib_env`) |
|---|---|
| bash, sh | `script1.sh`; the file must be executable and have a shebang |
| python | `import read` |
| node | `require("read.js")`; CommonJS only |
| ruby | `require "read"` |
| perl | `use Read;` |
| ts | `import { x } from "read"` or `"read.ts"` (ESM works) |

Why `bun` for ts and why `--no-install`: bun honours NODE_PATH for ESM `import` (node does not). Without `--no-install`, an unknown bare specifier makes bun fetch from the npm registry; with it a typo fails with "Cannot find package". `node` stays CommonJS-only by decision (ESM there would need a loader flag or generated file).

**Facts found while validating (bun 1.4.4, node 26, python 3.15, ruby 4.0, bash 5.3):**
- Late helpers already worked without a template for bash (PATH is searched per command) and ts (`await import("late")` through NODE_PATH); python finds a late helper only in a folder that existed at start unless `invalidate_caches()` runs, which the template does.
- bun `--preload` combined with `-e` works and the Proxy global is visible; file mode and an entry file that `import()`s the code also work, but need no wrapper, so none is used.
- `require()` of a bun helper with top-level `await` fails, so a Proxy that hands out promises cannot be called as `fn.x.y()`; hence sync `require` and "no top-level await in helpers".
- Bash `command_not_found_handle` through `BASH_ENV` works but gains nothing over PATH and leaks `BASH_ENV` into helper children, so bash has no template.
- A `runpy` entry point also works but adds frames and `~~^^` noise to tracebacks; `sitecustomize` keeps them the user's. `sitecustomize` is skipped under `python -I/-S`.

Also always set: `JAD_FUNCTIONS_DIR` = absolute real root and `JAD_RUNNER_DIR` = this runner's folder (also when it does not exist yet). Cross-language chaining is a subprocess by path only: `$JAD_FUNCTIONS_DIR/bash/script1.sh`. There is no native bridge: nothing calls back into JAD.

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
- native_exec_settings.go: runner keys lib_env and template, functions_dir default, schema, fixtures.
- exec_template.go and exec_templates/: the embedded templates, the override file, the per-call attachment.
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
