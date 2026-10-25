# Design: native exec redesign (any language, saved functions, JSON input)

Status: IMPLEMENTED (phases 1-5) as accepted; this file records the final design. Where it differs from the first proposal, the "As built" section below wins. tool_smith stays unchanged (JS/TS only). exec stays experimental.

## 0. What exists today (so the design builds on it)

- exec.go: input {lang, code, args[], cwd, timeout_ms, block_ms}. Config tools.native.exec.langs: per-language {argv, code: "stdin"|"file", ext, aliases}; default_lang (bash), code_max_bytes (256K), result_max_bytes (16K), line_aware. Code is delivered on STDIN by default (python3 -, node -, bash -s --); "file" delivery uses a temp file and "{file}" in argv.
- Result file: env EXEC_RESULT points at a per-call temp file; read back up to result_max_bytes and shown ahead of stdout.
- Async: block_ms handle via shell_async.go (e5d9366, 62b7769).
- Near-miss normalization (language/interpreter/source/script/program/argv/workdir/dir).

Key conflict: today stdin carries the CODE. The new requirement puts the JSON INPUT on stdin. So code can no longer travel on stdin when an input is given. This is the central change of the design (section 4).

## 1. Input schema (exact, flat)

One tool, three mutually exclusive modes, chosen by which of code / function / argv is present; none of them present = list.

    {
     "type":"object",
     "properties":{
      "lang":      {"type":"string","minLength":1,"description":"Runner name or alias (python, node, bash, go, ...). Required with code unless default_lang is set."},
      "code":      {"type":"string","minLength":1,"description":"Inline program source."},
      "function":  {"type":"string","minLength":1,"description":"Name of a saved function to run."},
      "argv":      {"type":"array","minItems":1,"items":{"type":"string"},"description":"Program and arguments run directly, no shell."},
      "input":     {"description":"Any JSON value. Written to the program's stdin as JSON text."},
      "args":      {"type":"array","items":{"type":"string"},"description":"Extra arguments appended to argv (code and function modes)."},
      "filters":   {"type":"array","items":{"type":"string"},"description":"List mode only: names or wildcard patterns such as read*."},
      "cwd":       {"type":"string","minLength":1},
      "timeout_ms":{"type":"integer"},
      "block_ms":  {"type":"integer","minimum":0}
     },
     "additionalProperties":false
    }

Rules (validated in Go, not only in the schema, so the messages are ours):
- Exactly one of code, function, argv, or none (list). Two or more: error naming them, e.g. "give either code or function, not both".
- lang: only with code. With function the language comes from the file extension; lang plus function is an error ("lang is only for code; a saved function brings its own language").
- input: allowed with code and function (and argv). Absent input = stdin is empty and closed (EOF at once).
- args: allowed with code, function and argv? NO for argv: argv already is the full vector. args with argv is an error. Justification for keeping args at all: it exists today, costs nothing, and lets a script receive flags without JSON; but it is the second way to pass data. See open question Q3 (recommend: keep, since removing breaks current callers, but describe input as the preferred one).
- filters: list mode only; with code/function/argv it is an error (tool_smith silently ignores it with a note; here an error is clearer).
- No env-var input. No second code-passing field (no run/script/source in the schema).

## 2. Runners config (replaces and extends langs)

tools.native.exec.runners (rename of langs; see migration). Map name -> entry:

    "python": {"argv":["python3","-c"],        "mode":"arg",  "ext":".py",   "aliases":["python3","py"]},
    "node":   {"argv":["node","-e"],           "mode":"arg",  "ext":".js",   "aliases":["nodejs","js"]},
    "bash":   {"argv":["bash","-c"],           "mode":"arg",  "ext":".sh",   "aliases":["shell"]},
    "sh":     {"argv":["sh","-c"],             "mode":"arg",  "ext":".sh"},
    "ruby":   {"argv":["ruby","-e"],           "mode":"arg",  "ext":".rb",   "aliases":["rb"]},
    "perl":   {"argv":["perl","-e"],           "mode":"arg",  "ext":".pl"},
    "go":     {"argv":["go","run","{file}"],   "mode":"file", "ext":".go"},
    "rust":   {"argv":["sh","-c","rustc -o \"$1.bin\" \"$1\" && \"$1.bin\"","sh","{file}"], "mode":"file","ext":".rs"},
    "java":   {"argv":["java","{file}"],       "mode":"file", "ext":".java"}

- mode "arg": the code is appended as the LAST argv element (so -c <code>). Args from the call follow it (bash -c code name arg1 ... : $0 is the first). For bash/sh the entry is ["bash","-c"] and the Go side inserts nothing else.
- mode "file": a temp file with ext is written; every "{file}" in argv is replaced by its path; call args are appended.
- mode "stdin" (legacy, today's default): only allowed when the call has no input; see section 4.
- Only entries present in config run; defaults ship for python, node, bash, sh, ruby, perl (the six existing). go/rust/java are examples in sample/schemes, not defaults, because a toolchain in the container cannot be assumed. Defaults only matter if the binary exists; a missing binary gives today's spawn_failed with the setting path.
- Validation at load: argv non-empty, argv[0] has no "{file}", mode file requires at least one "{file}", mode arg forbids "{file}", ext starts with ".", names match ^[a-z][a-z0-9_]*$, aliases unique across entries, max argv elements as today (32).
- default_lang stays (bash). Old key langs is rejected with a message naming runners (section 7).

## 3. Saved functions

Location: tools.native.exec.functions_dir, relative to the workspace, same containment rules as tool_smith (reuse resolveToolSmithFunctionsDir, toolSmithInside, openToolSmithFile). Default: its own folder, NOT tool_smith's, so the .ts library and this one never mix (proposal: ".jad/exec-functions"; confirm the tool_smith default and Q1). Not set = function mode and listing are off and the description says so.

File = one function: <name><ext>, e.g. summarize.py, build.sh. name matches ^[a-z][a-z0-9_]*$. The runner is the one whose ext matches; the ext must be unique across runners for this to be unambiguous (.sh belongs to bash AND sh: first in runners order wins, deterministic, documented; or give sh no ext for listing, Q4).
Header (this is the manifest; no separate file, so code and description cannot drift): the leading comment block, first non-empty line = description, using the language's line comment: "#" or "//" or "--" (any of the three is accepted at listing; the first line may follow a shebang line, which is skipped). Missing description = malformed, listed under "malformed files" like tool_smith.
Optional second header line "# input: {path: string, limit?: number}" is free text shown in the list; not validated (keeps it language-neutral; tool_smith derives a signature from TS, which we cannot do for every language).

Listing: exec with no parameters, or only filters, returns: "name \u2014 description [input hint]" per line, a "malformed files (n):" section, and truncation info, exactly the tool_smith shape and the same limits (list_max_results, list_max_files, list_header_bytes, function_max_bytes keys under tools.native.exec, same defaults). filters: lower-cased; a filter with * or ? is a whole-string wildcard (reuse newToolSmithFilter / wildcardMatch); otherwise substring; all filters must match name or description. Plain stdlib reuse: these helpers take no TS-specific state; the only new code is the header parser and extension scan.

Calling: {"function":"summarize","input":{...}}. The Go side resolves and re-validates the file at call time (symlink containment, regular file, size <= function_max_bytes, TOCTOU: open once, read from that handle, write into the per-call temp dir, run THAT copy). Running the copy, not the original, means an edit during the run does not change it, and relative cwd stays the workspace (not the library dir). Unknown name: error with the closest matching names (up to 5) and "call exec with no parameters to list".

## 4. stdin / args semantics

- With input: stdin = compact JSON text of input + "\n", then EOF. Strings are JSON strings (the script must parse). Size limit: input_max_bytes (new, default 1 MiB) -> error input_too_large.
- Without input: stdin empty, closed.
- Because stdin is taken, code is delivered by arg or file (section 2). The legacy "stdin" delivery stays valid only for a runner that declares it AND a call without input; with input it is an error naming the runner and the fix ("runner X reads its code from stdin; configure mode arg or file to pass input"). This keeps existing user configs working for plain code.
- Large code exceeds ARG_MAX (about 128 KiB per single arg on Linux, MAX_ARG_STRLEN): for mode arg, code over arg_max_bytes (default 100000) is automatically written as a file only if the runner has a file form; otherwise error code_too_large with advice. Simpler alternative: mode arg runners must ALSO give a file fallback; rejected as more config. Chosen: error with advice (smallest). code_max_bytes remains.
- args: appended after the code (arg mode) or after the file (file mode). For function: appended after the file.
- argv mode: stdin as above; no code, no temp file, no shell, cwd as given.

## 5. Result mechanism

Stdout is primary and is the result (capped as today by the output budgets, spill files on overflow, line-aware cut). The optional structured result:

- Keep a file, but rename and clarify: env JAD_RESULT (the user suggested it) pointing at <workdir>/result.json. Keep EXEC_RESULT as read alias? NO: two names = confusion. Replace EXEC_RESULT by JAD_RESULT; document it (breaking, experimental tool, Q5).
- Content is free text or JSON; Go does not parse it: it is shown in a [result] block, as tool_smith shows a returned value, capped at result_max_bytes (16K) with a "truncated" note. Not parsed because the contract is "the model reads it", and parsing would add a failure mode.
- Why a file and not a marker on stdout or fd 3: a file works in every language (open/write), survives noisy stdout, needs no sentinel escaping, and can be written early. fd 3 is cleaner but not portable to every runtime (Java, some Windows). A stdout sentinel can collide with data. Recommendation: file.
- Exit code: non-zero -> status error with exit code, signal, stderr tail (as today).

## 6. Temp-file lifecycle and resources

- Per call: os.MkdirTemp("", "exec-") mode 0700 (exists today). Contents: code<ext> (0600), function copy (0600), result file. Removed by defer in the run closure, on every path, including timeout/stop/restart-lost (handle's run closure ends when the process group is killed; the defer then runs). After a hard backend crash the directory leaks in the OS temp dir: add a startup sweep of stale exec-* dirs older than 24h? (Q6; tool_smith has the same gap, do not diverge silently.)
- File mode compile languages (go, rust, java) build artifacts inside workDir: they are removed with it; go run needs a writable GOCACHE: set GOCACHE/GOFLAGS only via runner "env" (new optional key, map) so nothing is hardcoded. Default none.
- Output caps: unchanged (stdout/stderr budgets, spill files). New caps: input_max_bytes, function_max_bytes, result_max_bytes. All config keys with defaults; sample/schemes and the config tests get updated (house rule).
- Goroutines: no new long-lived goroutine; stdin is written by one goroutine bounded by the process lifetime (write end closed on exit/kill; a script that never reads cannot block: the write is done in a goroutine and abandoned when the process dies; leak test required). Listing is synchronous and bounded by list_max_files.
- Security: the script is unsandboxed (container is the boundary). Specific concerns: (1) function library symlink escape -> same containment as tool_smith; (2) function name never joined to a path unvalidated (regex first); (3) argv mode runs any program with the backend user's rights, same as bash; (4) JSON on stdin avoids shell-quote injection of data; (5) code in argv is visible in ps to other processes of the container user (secrets must not be put in code; document); (6) 0700 workdir on a shared /tmp; (7) async handles keep the existing 6h timeout cap and owner scoping.

## 7. Errors for old and ambiguous forms

- code + function: "give either code or function, not both".
- code + argv, function + argv: same pattern.
- lang + function; lang + argv: "lang is only for code".
- args + argv: "argv already is the full command; use args only with code or function".
- filters with an action: "filters is only for listing; call exec with no parameters".
- Unknown lang: today's unknown_lang with available list (names + aliases).
- Old input: command (the bash tool's name) -> "exec has no command; use code with lang, or argv". run / script: today normalized to code; DROP the aliases source/script/program, keep none, so there is one way (lesson from tool_smith). Hint in the error: "use code".
- Old config key langs -> error "tools.native.exec.langs was renamed runners; entries now use mode (arg|file|stdin) instead of code".
- Old EXEC_RESULT users: no error possible; it simply is not set (documented).
- Tool description ends with the async sentence (block_ms) as today.

## 8. Migration

Existing input {lang, code, args, cwd, timeout_ms, block_ms} remains valid and works unchanged (code + lang). Removed: near-miss aliases (language, interpreter, source, script, program, workdir, dir) and argv-as-alias-of-args: argv now means "run a program directly" so the old alias would silently change meaning; the old alias case argv(with code) errors via the code+argv rule. langs -> runners (hard rename with a clear error; config is experimental). EXEC_RESULT -> JAD_RESULT. Existing default stdin delivery becomes arg delivery in the shipped defaults (behaviour difference visible only for scripts reading stdin themselves, which previously got their code there; now they get input).

## 9. Docs and tests

Docs: DECISIONS.md (why flat modes, why file results, why separate library, why no bridge), design_exec.md folded into a short note, exec.json sample, setting.schema.json (runners, functions_dir, limits), tool description.
Tests (each must fail when its fix is reverted): mutual-exclusion errors (one per pair); input reaches stdin as JSON, empty stdin without input; arg-mode and file-mode delivery incl. {file} replacement and args order; code over arg_max_bytes; legacy stdin mode with and without input; function list (filters substring and wildcard, malformed, truncation); function call (copy is run, symlink escape refused, unknown name suggestion, size cap); JAD_RESULT shown and capped; temp dir removed on success, failure, timeout, stop (goroutine and dir leak check); runners config validation (each rule); config migration errors; async paths already covered by exec_async_test.go keep passing.

## 10. Phased plan

1. Config: runners, modes, limits, validation, schema, sample, config tests (no behaviour change yet beyond rename).
2. Input: validation of the modes, stdin JSON, arg and file delivery, JAD_RESULT, error messages, migration of aliases; tests.
3. argv mode.
4. Saved functions: list, filters, call, copy-and-run; tests.
5. Docs/DECISIONS/exec.json; full suite and -race.
Each phase one commit.

## 11. Open questions

- Q1 Library folder: separate from tool_smith's (proposed) or shared one folder with extensions telling them apart?
- Q2 Should a function be allowed to declare its own timeout or runner override in its header? Proposed: no.
- Q3 Keep args beside input? Proposed yes (compat), input preferred.
- Q4 .sh ambiguity between bash and sh: first runner wins, or sh has no ext?
- Q5 Breaking rename EXEC_RESULT -> JAD_RESULT acceptable, or keep both for one release?
- Q6 Startup sweep of stale exec-* temp dirs (also for tool_smith)?
- Q7 Default for input_max_bytes (1 MiB) and arg_max_bytes (100000).
- Q8 Should functions be writable by the model (write tool into functions_dir) with no extra tool? Proposed yes, as for tool_smith.

## As built (differences from the proposal above)

- Open questions resolved as recommended: separate `functions_dir` (`.jad/exec-functions`); `JAD_RESULT` clean break; `args` kept beside `input`; `.sh` owned by the first runner listed; stale scratch sweep for `exec` and `tool_smith`; `input_max_bytes` 1 MiB and `arg_max_bytes` 100000; no header overrides.
- Runner argv uses a `{code}` placeholder (mode `arg`) instead of "code is appended last": `["python3","-c","{code}"]`, `["bash","-c","{code}","exec"]`. Config validation requires `{code}` for `arg`, `{file}` for `file` and neither for `stdin`. No per-runner `env` key (not built).
- The schema requires nothing; the executor reports `conflicting_input` when two of code/function/argv are given. `filters` with a run is `conflicting_input`; `lang`, `args` or `input` with a listing is `invalid_input`.
- Old aliases are refused by the schema with the right key (`RejectedProperties`) instead of being listed in a custom error; `argv` is now a mode, so it is not an alias of `args`.
- Function header: first comment line is the description; a later `input:` line is shown. The listing line is `name [runner] — description (input: ...)`.
- The sweep age is 7 hours (above the 6-hour ceiling), not 24, and has no config key.
- Not built: runner `env`, a Windows run, a benchmark of listing a large library.
