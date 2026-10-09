# Project rules (myagent3)

Working directory: `/home/www/Downloads/tmp/myagent3`. Priority: system > developer > this file > user request. Runtime tool schemas are the source of truth for tool names and parameters.

## Critical rules (never skip)

1. **Never kill or restart the running backend.** The current conversation depends on it. Tell the user when a restart is needed instead.
2. **Never change the observable output of `messages.jsonl`** (TypeScript or Go backend). It is a golden-output contract; any unrelated difference is a regression. Prove it with the `messages-golden-diff` skill. Change the format only when the task explicitly asks.
3. **Never hardcode configurable values** (limits, timeouts, feature flags, enable/disable options). Put them in config with a default, and update the schema in `sample/schemes/` and the config tests.
4. **Analyze before you propose or edit.** Trace the affected code paths, callers and dependencies first.
5. **Never say something was changed, tested or verified unless you did it in this session.** Report exact counts and measurements, not "should work".
6. **Do not stop after implementing.** Loop: IMPLEMENT -> RUN -> TEST -> MEASURE -> FIX -> REPEAT.
7. **Report separate bugs; do not fix them silently or ignore them.** Procedure: `bug-report-memory` skill.
8. **Never** use `pkill`, `killall` or kill by name or pattern; it can hit the backend or your own shell. Kill only a PID you started.

## Default workflow

1. **Locate:** pick the tool by question type:
   - **Known symbol, callers, call chains:** Codebase Memory (project `home-www-Downloads-tmp-myagent3`: `search_graph`, then `get_code_snippet` or `trace_path`).
   - **Exact text** (identifier, config key, error message): `Grep` with `path`, `include` and a limit.
   - **Concept or "where is X done" with no known name** (it spans both backends, or the wording is unknown): `zvec_grep_search` with `root` set to the repo, a natural-language `query` and `limit` 5. Do not repeat the same text in `fts`. Its source snippets count as read, but `Read` the exact lines before editing.
   - **A lookup that would take more than about three searches or fill the context** (all callers of a symbol, every reader of a config key, how a feature spans both backends): hand it to a subagent (see Subagents) instead of searching yourself.
2. **Read** only the needed range: `Read` with `offset` and `limit`. Read the exact lines before editing them.
3. **Change:** `Edit` for existing code, `Write` for new or whole-file content.
4. **Check:** `npm run typecheck` (TypeScript) or `go vet` (Go, see `backend-go/CLAUDE.md`), then only the affected test file or package.
5. **Finish:** the full suite once (see Verify), plus `git diff --check`.

## Tool use

- `Bash` is for build, test, git and long-running work (`Process`). Do not use `sed`, `cat`, `grep` or `python` heredocs for what `Read`, `Grep` or `Edit` does. A script is allowed only to apply one change in many places, and it must assert that every replacement applied.
- **`Edit` fails (not found or not unique):** `Read` the range again and retry with more surrounding context. Do not fall back to a script.
- **Codebase Memory looks stale or wrong:** use `Grep`. Re-index only if the user asks. For `zvec_grep_search`, check `freshness` in the result; if it is not `fresh`, confirm hits with `Grep` before relying on them.
- **Truncated output:** the result names a full-output file; `Read` or `Grep` it and do not re-run the command. For a command you expect to be large, redirect it to a file in `/tmp` and `Grep` that.
- **Parallel calls:** the backend runs a batch concurrently. Batch independent reads, greps and graph queries freely. Never batch calls that write the same path, or calls that depend on each other (edit then build, build then test).
- Keep queries bounded: narrow paths, globs, limits, line ranges. Keep paths, line numbers and truncation details as evidence.
- **Skills:** load by name from the table below, once per task; do not call `skill({})` to browse. Do not reload a skill you already loaded.
- Prefer modular code. Avoid monolithic files and functions.
- **Two backends:** `backend-go/` is the product and gets the new features. `backend/` (TypeScript) is a **frozen reference**: do not add features to it. Keep it building and passing its tests, because the differential harness and the `messages.jsonl` golden diff compare Go against it. Touch it only for a bug the harness or the golden contract exposes, or when a task names it. When a change affects behavior both already share (HTTP routes, SSE, tools, config parsing, transcripts), check the other side and report any gap. See `backend-go/DECISIONS.md`, "TypeScript backend: frozen reference".
  - **What frozen means (decided 2026-10-08):** it is not deleted and not scheduled for removal. A Go-only feature is **not** a gap and owes no TypeScript port; do not report it as one. Go-only today: `auth_mode: "openai_oauth"`, `cmd/openai-auth`, `cmd/claude-auth`, `cmd/anthropic-adapter`, sub-agent persistence, owner records and orphan stopping, the startup recovery scans (`server.recover_*_at_start`), and the log keys.
  - **Its job:** it is the oracle of the differential harness (`backend-go/tools/differential` starts `backend/dist/src/server.js` next to the Go binary) and of the `messages.jsonl` golden contract. A change under `backend/` therefore needs `cd backend && npm run build` before the harness, and `npm test` must stay green.
  - **Which side moves:** when the two differ on shared behavior, Go is aligned to TypeScript unless `DECISIONS.md` says the difference is intended. Do not "fix" TypeScript to match Go; touch it only for a bug the harness or the golden diff exposes, or when a task names it.
  - **Known intended difference:** the ChatGPT OAuth token is refreshed inside the TypeScript backend and not in Go (`openai-auth refresh` is manual). Do not port it.
  - **Reporting a difference:** use `parity_checker`, check its `path:line` claims yourself, then record a real one with `bug-report-memory` (low severity for message wording).

## Subagents (the `subagent` tool)

A child has a fresh context and returns only its final answer (cut at 32 KB). Use it to keep your own context small and to get an independent look. Agent folders live in `.jad/subagents/<name>/` (`agent.json`, `prompt.md`, a model file); `.jad/` is git-ignored, so they are machine-local.

| Agent | Model | Access | Use it for |
| --- | --- | --- | --- |
| `code_discovery` | Haiku | read-only, code graph, zvec | find files and symbols; report paths and line numbers |
| `impact_analyst` | Sonnet | read-only, code graph, zvec | **before an edit** (critical rule 4): callers, readers of the same field or config key, covering tests, whether it is shared behavior |
| `config_auditor` | Haiku | read-only | **after a config change** (critical rule 3): loader, default, schema, tests, use and `DECISIONS.md` must agree |
| `parity_checker` | Sonnet | read-only | one behavior in `backend-go/` against the frozen `backend/`: every difference with `path:line` |
| `code_reviewer` | Sonnet | read-only, code graph, zvec | **after a meaningful change**: independent review of a diff file; cost is real, so not after every edit |
| `verifier` | Haiku | `bash` only | run the exact commands you list and return exit codes and pass/fail counts |

- **Write the task as if to a stranger.** A child knows nothing of this conversation. Give the repo-relative paths, the symbol or key, what the change is meant to do, and the exact output you want (a list of `path:line`, a table). For `code_reviewer`, write the diff to `/tmp/<name>.diff` with `git diff` first, name it in the task, and delete it afterwards.
- **Measured cost of one run each** (this repo, 2026-10-09): `code_discovery` 8k tokens, 4 s; `parity_checker` 28k, 27 s; `impact_analyst` 36k, 23 s; `config_auditor` 63k, 29 s; `code_reviewer` 169k, 76 s. Three read-only agents ran side by side in 29 s. Use `code_reviewer` once per meaningful change, not per edit.
- **A child's answer is a lead, not proof.** In the first live runs every finding had a real basis, but two statements were wrong when checked: `impact_analyst` called code covered by ten tests "not covered" without searching the test files, and `code_reviewer` gave a wrong mechanism (map order) for a real concern. Check "not covered", "none found" and any mechanism with one `Grep` before you rely on it. `Read` the exact lines before you edit or cite them. Report a child's numbers as "the verifier reported", never as your own measurement (critical rule 5). If a child contradicts what you read, trust what you read and say so.
- **Parallelism.** The read-only agents may run side by side in one response (`max_concurrent` is 3). `verifier` has `bash`, so its calls run **in order, one at a time**: put every command of one check round into a single verifier task. Never start two harness runs at once.
- **`verifier` is the only agent that can hurt the tree.** Its prompt forbids git commands that change anything, `rm`, `kill`, writes into the repo and `./...`. Give it exact commands, never "fix it". It checks; you fix. Run the full suite yourself before you commit, because that number goes in your report.
- **Do not delegate:** edits, anything that needs the design context you hold, the final full suite before a commit, anything that touches the backend process, and questions you can answer with one `Grep`.
- **A slow child comes back as a run id.** Do not start the same task again; call `subagent` with that `id`.
- **Adding or changing an agent:** a bad `agent.json` is a configuration error that stops the session loading. Build the folder in `/tmp`, load-test it against a copy of the session, then install it. New agents appear in the tool list on the next run, not the current one.

## Skills (load when the trigger fires)

Skills live in `.jad/prompts/skills/`. A rule that must hold if you forget to load a skill stays in this file; only the "how" lives in a skill.

| Trigger | Skill |
| --- | --- |
| Working in `frontend/` | read `frontend/CLAUDE.md` |
| Working in `backend-go/` | read `backend-go/CLAUDE.md` (cgo quirks, commands, parity harness) |
| Before adding or changing any agent-facing tool or its schema | `agent-tool-design-guidelines` |
| A change could affect `messages.jsonl` (persistence, serialization, tool results, streaming, session code) | `messages-golden-diff` |
| You find a bug unrelated to the current task | `bug-report-memory` |
| Starting a long multi-step task, resuming after compaction, or finishing a meaningful batch of it | `resume-state-memory` (keep at most one record; if it contradicts the repo, trust the repo) |
| Writing or reviewing Go code, or Go performance work | `go-high-performance-guidelines` |
| Locating code, finding callers, or checking what a change affects (choosing between Grep, Codebase Memory and `zvec_grep_search`) | `code-navigation-workflow` |
| The user asks for OpenSpec (`openspec`, `opsx`) work | the matching `openspec-*` skill |

## Verify

- **Backend:** `cd backend && npm test` (cleans `dist/test`, builds, then runs every `test/*.test.ts` automatically; no registration needed). Quick check: `npm run typecheck`.
- **Frontend:** `cd frontend && npm test`.
- **Go backend:** `cd backend-go && go test ./internal/... ./cmd/...` (never `./...`). For HTTP, SSE, tool, config or transcript changes also run the differential harness in `backend-go/CLAUDE.md`.
- **Always:** `git diff --check`, and report the pass/fail counts you saw.
- **Parallel checks:** for a check round with several commands (vet, gofmt, tests of named packages, the harness), hand the whole list to `verifier` in one task and keep working. The full suite before a commit you run yourself.
- **Bug fix:** add a regression test and confirm it fails without the fix.
- **Performance work:** measure before and after; state the numbers and what they do not prove.

## Git

- Commit only when asked. Stage explicit paths, never `git add -A` or `git commit -a`.
- Leave unrelated pre-existing changes and untracked files alone. Put scratch files in `/tmp` and delete them.
- Report the commit hash, the test result, what you left uncommitted, and whether the backend was restarted.

## Asking the user

Use `RequestDraft` for every question or confirmation; do not ask in plain chat. Use it before starting when the request is ambiguous, and before risky, destructive or out-of-scope actions (deleting files, changing the `messages.jsonl` format, touching the backend process). Put your recommended answer in the draft. The `decision` field controls approval; `status: "answered"` alone is not approval. Skip it for questions the code or a command can answer.
