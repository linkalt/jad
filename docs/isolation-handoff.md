# Isolation and sandbox: handoff (2026-11-09)

This note hands the T-120 isolation work over to local development. The design is
`docs/isolation-sandbox-design.md`, the decisions are in `docs/isolation-decisions.md` (one section
for each phase that has been built), and the backend log is `backend-go/DECISIONS.md` (T-120
entries).

## State

- **Branch:** `claude/tender-brown-w3dzeg`, pushed, with a clean working tree. No PR has been
  opened.
- **Done, in commit order:**

  | Commit | Phase |
  |---|---|
  | `2c23c7e` | P0 + P1: S1–S5 and U1 closed, lost worktrees recovered |
  | `ce950a3` | P2: `read`/`write` keys, templates, agent and `profile_message` narrowing |
  | `0c8c602` | P2 follow-up: an agent's own sandbox shown in its tool descriptions, MCP servers of toolsets |
  | `5abe8af` | P3: isolated sub-agent runs with private git (checkout materialiser) |
  | `7bf04fd` | P3: delegated `profile_message` runs in a worktree |
  | `43aa33f` | P2b: per-call sandbox choice in `subagent` and `profile_message` |
  | `16fe917` | P3 completion: W′ check, recovery of delegated worktrees, remote runs in a worktree |
  | `a308767` | P5: exec `read_only` and plan mode enforced by a sandbox; `read_only_calls` removed |

- **Last full run (`a308767`):** `go test ./internal/... ./cmd/...` passed 5,487 tests and failed 0,
  with 29 skipped, across 45 packages. `go vet`, `gofmt -l` and `git diff --check` were clean.
  The suite also passes with bwrap hidden from `PATH`.
- **Not done:** P6 (optional), D7 (still waits on your answer; see "Needs the user" in the
  decisions file). P4 is implemented since (next section), with the gaps its decisions list.

## Using it with the sandbox off

- **The sandbox is off by default.** `sandbox.enabled` defaults to `false`
  (`sessionfs/sandbox.go`, `parseSandbox`), and with no `sandbox` key at all there is no sandbox.
  When it is off, normal tool calls start as they did before T-120.
- **What still changes with the sandbox off (P5, D2/D3):**
  - `exec` calls with `read_only: true`, and every `exec` call in plan mode, run in a write-only
    fallback sandbox. It uses bwrap on Linux and seatbelt on macOS, and only stops writes:
    everything stays readable, nothing is hidden and the network stays open. Only
    `write.plan_paths` can be written, by default `{tmp}`.
  - The fallback is probed once per process. If bwrap or seatbelt doesn't work on the host, plan
    mode refuses `exec` code, with an error that says so, and `read_only` calls are scheduled as
    normal calls, in order.
  - A setting that still contains `tools.native.exec.read_only_calls` fails to load; delete the
    key.
- **Build:** `cd backend-go && go build ./cmd/jad` succeeds at `a308767`. The running backend has
  to be restarted to pick up the new binary.
- **On Ubuntu 23.10 or later:** AppArmor may block unprivileged user namespaces for a custom bwrap.
  The distribution's bwrap has a profile. If the fallback cannot start, the backend logs it once.

## P4: overlay materialiser

**Status (2026-11-10):** implemented on a host with bubblewrap 0.13.1 and util-linux 2.42.4; see
"P4 implementation" in `docs/isolation-decisions.md` (what was built, where it departs from the
design, what is not done) and the T-120 batch 15 entries in `backend-go/DECISIONS.md`. The
section below is the original brief.

**Why it was not done here:** it needs bwrap 0.10 or later (`--overlay-src`/`--overlay`), and the
cloud container had 0.9.0 (P0 table in the decisions file). Check on your host with
`bwrap --version` and `bwrap --help | grep overlay`.

**Scope (design §12):** overlay materialiser, per-run bwrap holder joined with `nsenter` (D14), base
cache, an in-process view through `/proc/<holder>/root` (with `openat2(RESOLVE_IN_ROOT)`), and
`isolation.share_paths` (D6, default empty). It touches `worktree/{overlay,base}.go`,
`tools/native/sandbox_holder.go`, `path.go` and `shell_changes.go`. The relevant decisions are
D1, D6 and D14, plus design §13 (risks).

**Where to start:**

- **Settings:** `sessionfs/sandbox.go`.
  - `sandboxIsolationKeys` (line 75) accepts only `mode` and `base` today. `materialize` and
    `share_paths` are refused as unknown keys, and `SandboxIsolation` (around line 160) has no
    fields for them yet.
  - Add both keys, then update `sample/schemes/sandbox.schema.json`, `setting.schema.json`,
    `agent.schema.json` and the config tests (CLAUDE.md rule 3).
  - `sandbox_template_test.go` checks the current refusals; update it.
- **Materialiser:** `internal/worktree/`.
  - `Create` and `add` (`worktree.go:188`, `:256`) build today's checkout.
  - `workingSnapshot` (`snapshot.go`) handles `base: working`.
  - `Finish`, `collect` and `importObjects` (`finish.go`) collect and import the run's commits;
    `Recover` handles lost runs.
  - The overlay path needs to produce the same `Tree`/`State`, so that `Finish` and recovery stay
    shared. `auto` must fall back to checkout when the probe fails (D1).
- **Probe:** `tools/native/sandbox_probe.go` (`ProbeSandbox`), called by `jad check`
  (`app/check.go:24`).
  - Add overlay and `nsenter` probes there, so `jad check` reports them (D14).
  - At the same time, report whether the P5 fallback works: `jad check` doesn't show it yet.
- **In-process readers:** `tools/native/path.go` and `path_scope.go` (used by `code_context`,
  `read_image`, `lsp` and `transcript`). They must read through the holder's root during an
  overlay run. Missing an `os.Open` reads the empty host folder (§13).
- **`hooks.shell_changes`:** it stats the host workspace and must go through the holder's root
  (design table at line 1311, "fixed in P4").

**Tests:**

- Real bwrap tests skip when bwrap is missing; follow `exec_read_only_test.go` and the P3 worktree
  tests.
- The overlay tests should skip when `--overlay` is missing, so they still pass on 0.9.0.
- Also run the suite with bwrap off `PATH`.

## Open items carried over

- **`jad check`** now reports (as `note:` lines) whether the P5 fallback starts and whether
  overlay runs work (P4).
- **`tool_smith`** has no P5 fallback, so its code calls are still refused in plan mode.
- **Hooks in plan mode** are not confined by the fallback, only by an enabled sandbox that lists
  `hooks` (U1).
- **`read_only` scheduling** outside plan mode is judged by the session's settings, not by a
  sub-agent's own `command` sandbox. This was accepted and documented in the P5 section.
- **D10 / P6:** MCP servers of an isolated run still act on the session folder; per-run stdio MCP
  is P6.
- **Remote callers:** delegated runs are not narrowed by a remote caller's sandbox (P3 completion,
  item list).
- **macOS:** seatbelt read-deny and `{system}` are untested; there is no macOS runner.
- **Unrelated doc bug (not fixed):** in `sample/schemes/setting.schema.json`, the
  `tools.native.subagent` description (around line 1982) still mentions an agent `read_only` key,
  which T-119 removed.

## Checks before each commit

From `backend-go/`:

```bash
go test ./internal/... ./cmd/...
go vet ./internal/... ./cmd/...
gofmt -l internal cmd
git diff --check
```

Never run `go test ./...`. End each phase with a section in `docs/isolation-decisions.md` and a
T-120 entry in `backend-go/DECISIONS.md`, as the earlier phases did.
