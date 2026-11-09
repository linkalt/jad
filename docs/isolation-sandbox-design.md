# Isolation and sandbox design: per-run workspaces, a real sandbox, and profiles

Design answer to `docs/isolations.md` (task T-120). Design only: no code, config, schema or test
changes are part of it. Every statement about current behaviour carries a `file:line` under
`backend-go/internal/` unless another root is named; every proposal names the file it changes.
Facts measured on this machine are marked **[measured]** (commands in Appendix A); facts about
git, bubblewrap or the kernel that come from their documentation and were not measured are marked
**[external]**; numbers that are reasoned rather than measured are marked **[estimate]**.

## 0. Recommendation in one page

**Rule that shapes everything:** do not limit tools, limit the environment. Every path rule below is
a mount (bwrap), a profile rule (seatbelt) or the absence of a path in a namespace; no tool is hidden
and no call is refused for its path, except where §2.4 explains why an in-process reader mirrors the
environment.

**Recommended architecture (Linux, bwrap ≥ 0.10, unprivileged user namespaces):**

1. **One frozen, shared, read-only base per base commit** (a detached linked worktree of that commit,
   checked out once and reused by every run that starts from it) as the overlay *lowerdir*.
2. **One overlay upper/work pair per run**, mounted by bwrap's `--overlay` *inside one long-lived
   per-run sandbox* (the "holder"); every process of the run (exec, tool_smith, hooks, LSP, stdio
   MCP of the run, and jad's own git for collection) joins the holder's namespaces with `nsenter`.
   One mount per upper is a hard requirement (§2.3): the kernel silently allowed a second concurrent
   mount of the same upper in the spike **[measured]**, so jad must guarantee it.
3. **Git metadata per run is a linked worktree created with `--no-checkout`** (admin directory
   `<common>/worktrees/<run>`), with **a detached HEAD, a private object directory and the shared
   object store as a read-only alternate**. The run never writes the shared `.git`; its new objects
   reach the repository as a pack that jad imports (`git index-pack` + `git update-ref`), which is
   data only **[measured]**.
4. **Change collection is git, run inside the run's environment** (`git status/add/commit/pack-objects`
   with the base's index copied in, so nothing is re-hashed **[measured]**). Upper-dir whiteouts are
   never parsed.
5. **Integration never overwrites:** the run's branch is validated with `git merge-tree --write-tree`
   against the current target and handed back as a branch, exactly like today's
   `isolation: "worktree"` result; merging stays an explicit act of the parent or the user.

**Configuration is template-based (§14):** an environment — `read`, `write`, `hide_paths`, network,
covered tools and `isolation` — is one file `.jad/sandbox/<name>.json`, named by any number of
profiles (`"settings": {"sandbox": "<name>"}`) and sub-agents (`"sandbox": "<name>"` in `agent.json`),
optionally with per-use overrides; an agent's template is narrowed by its parent's and refused if it
would widen it. With `sandbox_choice` set, the `subagent` and `profile_message` tools gain a per-call
`sandbox` argument (§15) that picks one of the listed templates for that run — narrowing only, like
exec's `read_only` for one call.

**Fallback everywhere else (macOS seatbelt, Linux without overlay-capable bwrap):** today's full
`git worktree add` checkout (`worktree/worktree.go:89-150`), sandboxed per call through the existing
chokepoint `sandboxed()` (`tools/native/sandbox.go:45-88`), with the same private-git and
protected-path rules. The fallback changes cost, not the confinement rules.

**Why this and not the alternatives** (detail in §8): overlay makes run creation O(1) once the shared base exists, and disk O(changed
files) on every Linux file system, including ext4 where reflinks do not exist; a frozen lower avoids
the kernel's "lower must not change" rule; a holder avoids the "one mount per upper" rule and gives
jad a view of the run (`/proc/<holder>/root`) for in-process readers; protected git paths, hardened
host git and private git metadata close two sandbox escapes that exist today (§7.2, both reproduced **[measured]**); and the fallback is the
code path that already ships and is tested.

**Verdict on the question behind the task (§R1):** exec's per-call `read_only` **stays** (it selects a
narrower environment for one call, which is the rule, and it is what lets read calls of one response
run together); `tools.native.exec.read_only_calls` **can be removed only after** phase P1 closes the shared-`{tmp}`
and plan-mode-hook gaps and the user accepts that, on a host without a sandbox, `read_only` then no
longer counts (§R1, decisions D2/D3).

---

## 1. Existing architecture findings (deliverable 1)

### 1.1 The single process-spawn chokepoint

| Fact | Where |
|---|---|
| `callSandbox` picks the plan-mode form when `ExecutionContext.PlanMode` is set | `tools/native/sandbox.go:34-39` |
| `sandboxed()` is the only place a child is wrapped (bwrap, seatbelt, command) | `tools/native/sandbox.go:45-88` |
| Callers: exec/tool_smith/hooks spawn `runBashCommand` | `tools/native/bash_child.go:115-129` |
| standby interpreters | `tools/native/exec_standby.go:447` |
| `jad check` trial start | `tools/native/sandbox_probe.go:19-56`, called from `app/check.go:24` |
| stdio MCP servers via `SandboxCommand` | `tools/native/sandbox.go:265-275`, `app/mcp_sampling.go:43-56`, `tools/mcp/manager.go:476-498`, wired at `app/resolver.go:655` |
| bwrap argv: `--die-with-parent --unshare-all`, `--ro-bind / /`, `--dev`, `--proc`, `--bind-try` per writable path, `--tmpfs`/`/dev/null` per hidden path, `--chdir` | `tools/native/sandbox.go:100-136` |
| seatbelt profile: `(allow default)`, `(deny file-write*)`, allow writable subpaths, deny read+write of hidden paths, `(deny network*)` unless network | `tools/native/sandbox_seatbelt.go:17-46` |
| **No read restriction exists in either backend**; `hide_paths` is the only read control | same two places |
| Placeholders `{workspace} {cwd} {git_common_dir} {tmp} {home}`; empty value drops the path | `tools/native/sandbox.go:141-160`, `sessionfs/sandbox.go:53-61` |
| `{git_common_dir}` found by walking to `.git` / a worktree's `commondir` | `tools/native/sandbox.go:163-179` |
| Default writable `{workspace} {git_common_dir} {tmp}`; plan-mode default `{tmp}` | `sessionfs/sandbox.go:45`, `:49` |
| An enabled sandbox whose program is missing is refused at load (no silent fallback) | `sessionfs/sandbox.go:218-227` |
| Sandbox covers `exec`, `tool_smith` by default; `hooks`, `mcp` optional; **LSP is not a choice** | `sessionfs/sandbox.go:38-40` |
| Sandbox is handed to tools by pointer at load (`applySandbox`), once per session config | `sessionfs/sandbox.go:278-291`, `sessionfs/session.go:533-535` |
| Plan mode swaps the writable set (`ForPlanMode`), bwrap/seatbelt only | `sessionfs/sandbox.go:267-274` |
| Hooks take `hooks.Sandbox` directly, **not** `callSandbox`: no plan-mode swap for hooks | `tools/native/hooks.go:170` |
| tool_smith standby uses `callSandbox` | `tools/native/tool_smith.go:319` |
| Standby identity includes `sandboxKey` (sandbox JSON + workspace) | `tools/native/exec_standby.go:108`, `tools/native/sandbox.go:207-213` |
| Child environment filtered by an allowlist, API key and `JAD_API_TOKEN` removed | `tools/native/shell_run.go:266-306` |
| exec scratch dir is `os.MkdirTemp("", "exec-")`, i.e. under the process TMPDIR = `{tmp}` | `tools/native/exec.go:473-475` |

### 1.2 The in-process path gate

| Fact | Where |
|---|---|
| `resolveExistingTarget`: absolute from `WorkspaceRoot`, deny rules, `Within`, allow rules (workspace always allowed), symlink re-check | `tools/native/path.go:23-102` |
| Its 8 callers: code_context, exec standby cwd, read_image, lsp (×2), exec/tool_smith/hooks cwd, tool_smith standby cwd, transcript | `code_context.go:484`, `exec.go:489`, `image_file.go:35`, `lsp.go:92`, `lsp.go:322`, `shell_run.go:114`, `tool_smith.go:309`, `transcript_path.go:98` **[gonav refs]** |
| exec's `paths` checks only the cwd; what the program opens is not checked | `tools/native/shell_run.go:114-124` |
| `scopedPolicy` turns `PathScope.Allow` into `Within`; `workspacePolicy` moves rules from the origin into a worktree | `tools/native/path_scope.go:14-100` |
| `narrowScope`: intersection of allows, union of denies; empty intersection denies `/` | `tools/native/path_scope.go:115-156` |
| A child inherits the parent's scope (`ForNested` + `narrowScope`) | `tools/catalog.go:108-117`, `tools/native/subagent_call.go:58-62` |
| `NativePathRule`/`NativePathPolicy`; array form is restricted, object form with no allow is unrestricted | `sessionfs/file_settings.go:30-43`, `:125-170` (`:169`) |
| `hooks.shell_changes` scan stats the whole `WorkspaceRoot` on the host before/after each exec call | `tools/native/shell_changes.go:27-46` |

### 1.3 Worktree isolation today

| Fact | Where |
|---|---|
| Only `isolation: "worktree"` agents get a worktree | `tools/native/subagent_call.go:224-232`, `sessionfs/native_subagent_worktree.go:59-63` |
| `Create`: `rev-parse --show-toplevel`, `--git-common-dir`, `--verify <rev>^{commit}`, then `git worktree add --quiet -b <prefix><run> <dir>/<run> <base>` under a per-repository lock; default dir `<common>/jad-worktrees` | `worktree/worktree.go:89-150`, `:26` |
| Base is HEAD or a given commit: uncommitted and ignored files are absent | `worktree/worktree.go:113-122` |
| `Finish`: `status --porcelain --untracked-files=all`, `add --all`, commit (`--no-verify` unless `commit_hooks`), `rev-parse HEAD`, `diff --shortstat`, `worktree remove --force`, `branch -D` when unchanged; a failed step keeps the folder | `worktree/worktree.go:156-223` |
| Every git command runs on the host with `os.Environ()` + `GIT_TERMINAL_PROMPT=0 LC_ALL=C` — **unsandboxed** | `worktree/worktree.go:233-256` |
| `openWorktree` sets `WorkspaceOrigin`, `WorkspaceRoot`, moves `PathScope`; `closeWorktree` runs after the child and its handles stopped | `tools/native/subagent_worktree.go:69-90`, `:97-115`, `tools/native/subagent_manager.go:848-886` |
| On restart, interrupted runs are marked lost; **their worktree is neither committed nor removed** | `tools/native/subagent_manager.go:147-165` |
| An isolated run is scheduled like a reader (`ChangesWorkspace`) | `sessionfs/native_subagent_worktree.go:63`, DECISIONS 2026-11-04 |
| Non-isolated writer runs as background handles are limited to `max_background_writers` (default 1); isolated runs are not writers of the session, so they run side by side | `sessionfs/native_subagent_settings.go:31-33`, `:102-105` |

### 1.4 Other processes that touch the workspace

| Process | Sandboxed today? | Where |
|---|---|---|
| LSP servers | never (`exec.Command`, cwd = root) | `tools/native/lsp_client.go:136-148`; pool keyed by server + root `lsp_client.go:58-85` |
| Checkpoint shadow git (snapshot, restore) | never; own `GIT_DIR`/`GIT_WORK_TREE`, fixed config | `checkpoint/git.go:85-99`, `:159-170`; restore `checkpoint/service.go:782` |
| Worktree git (`Create`, `Finish`) | never | `worktree/worktree.go:233-256` |
| stdio MCP | only when `sandbox.tools` lists `mcp`; cwd is the session folder, not a worktree | `app/mcp_sampling.go:43-56`, `tools/mcp/manager.go:494` |
| HTTP MCP | not a local process | `tools/mcp/manager.go:479` (only stdio branches into a child) |
| tool_smith bridge calls | run in jad's process as ordinary tools | `tools/native/tool_smith_bridge.go:46-80` |

### 1.5 Scheduling, plan mode, checkpoints and `read_only`

| Fact | Where |
|---|---|
| exec is `ReadOnly: true` with per-call `MayChangeFiles` = code given and not (honored and `read_only`) | `tools/native/exec.go:93-96`, `:126-128` |
| A `read_only` call runs with `ExecutionContext.PlanMode = true` → plan-mode sandbox | `tools/native/exec.go:116-121` |
| `ChangesState` = not ReadOnly or `CallMayChangeFiles` | `tools/catalog.go:463-479` |
| Ordered scheduling (the default) runs a whole response serially if one call changes state | `session/tool_batch.go:30-40`, `session/runner_tools.go:285`, default `sessionfs/tool_settings.go:97-99` |
| Plan mode keeps ReadOnly tools, guards `MayChangeFiles` tools (refusal `plan_mode`), marks every call PlanMode | `tools/plan_mode.go:18-33`, `:54-76`; built once per run `app/bound_runner.go:134-139` |
| Checkpoint: plan-guarded calls skipped; otherwise **exec counts by name**, so `read_only` never avoids a checkpoint outside plan mode | `session/runner_tools.go:214`, `:217`, `checkpoint/run.go:13-24` |
| `read_only_calls`: `trusted` (default) / `sandboxed`; `ReadOnlyEnforced`, `ReadOnlyHonored` | `sessionfs/native_exec_settings.go:37-44`, `:173-186`, `:347-358` |
| Standby warms the plan-mode form too when enforced | `tools/native/exec.go:521-528` |
| Older exec definition files are accepted (`requireExecDefinition`) | `tools/native/catalog.go:537-551`, `tools/native/exec.go:52-64` |

### 1.6 Settings plumbing for profiles

| Fact | Where |
|---|---|
| Run settings = defaults ⊕ session ⊕ profile (`mergeSettings`, objects merge per key, arrays replace) | `sessionfs/layout_load.go:318-324`, `sessionfs/layout_merge.go:7-31` |
| A profile may carry `sandbox` but not `tools`, `isolation`, `read`, `write` | `sessionfs/layout_files.go:30-36`, refusal `:199-222` |
| Sub-agent definitions (and so `isolation`) reach a profile only through toolsets | `sessionfs/layout_load.go:481-500`, `layout_files.go:149-160` |
| An agent's toolsets are composed with the selected profile's settings, so its exec gets the profile's sandbox | `sessionfs/layout_load.go:156-201` |
| A child's `SessionConfig` is the parent's (same sandbox) | `app/subagent.go:235-249` |
| An agent's toolsets get a derived config and tool set of their own, keyed by `ToolsetKey` | `layout_load.go:156-201`, `tools/native/catalog.go:268-293`, `app/resolver.go:909-925` |
| The stdio MCP servers of those toolsets start **without** the sandbox wrapper (only the session manager gets one) | `app/resolver.go:655` is the only `SetCommandWrapper` call; `loadAgentToolsetMCP` `:909-925` sets none |
| Catalog files are `<root>/<category>/<name>.json`; a flat file is refused; agent folders are flat by name | `catalog/scan.go:40-75`, `:65`; `native_subagent_dir.go:14-15` |
| Catalog kinds the file API serves: profile, model, toolset, mcp | `sessionfs/catalog_files.go:21-26` |
| Sandbox parsed from the merged settings; a refusal stops the session | `sessionfs/session.go:410-412` |
| Per-call context has no sandbox field: a per-run environment needs one | `tools/catalog.go:28-68` |
| Base execution context per run (`WorkspaceRoot`, `SessionDir`) | `app/bound_runner.go:160-161` |

### 1.7 Config patterns to reuse

Named refusals (`sessionfs/native_subagent_settings.go:473-478`, hints `sessionfs/removed_keys.go:18-31`),
small enum normaliser (`sessionfs/native_exec_settings.go:347-358`), unknown-key refusal with
suggestion (`sessionfs/layout_files.go:199-222`), worktree settings normaliser
(`sessionfs/native_subagent_worktree.go:85-134`), hand-written schemas checked by loader-agreement
tests (`sample/schemes/setting.schema.json:627-678`, `sessionfs/sandbox_test.go:140-173`,
`sample/schemes/agent.schema.json:92-97`).

### 1.8 What this repository measures **[measured]**

1,771 tracked files / 16.0 MB tracked; 34,835 files / 863 MB including ignored files (mostly
`node_modules`); `.git` 37 MB; index 203 KB; git 2.56.0, bubblewrap 0.13.1, kernel 7.4, source on
btrfs, scratch on tmpfs. These are the sizes behind §8.

---

## 2. Proposed architecture and component relationships (deliverable 2)

### 2.1 Components, and which existing abstraction each one is

| Responsibility | Brief's name | Owner (existing package, extended) | New code |
|---|---|---|---|
| Per-run workspace and branch | WorkspaceManager / Workspace / WorktreeManager | `internal/worktree` (`Tree`, `Create`, `Finish`) | materialisers, base cache, collect/import split, recovery |
| Overlay mount | OverlayManager | folded into `internal/worktree` (materialiser `overlay`) + the holder in `internal/tools/native` | none separate |
| Sandbox (mounts, namespaces) | SandboxManager | `internal/tools/native/sandbox.go` (`sandboxed()`), `internal/sessionfs/sandbox.go` (settings) | holder start/join, read/write sets |
| Change collection | ChangeCollector | `worktree.Tree.Finish` (git status/add/commit), now run inside the environment | pack export |
| Integration | WorkspaceIntegrator | `worktree` (validation with `merge-tree`) + the parent's own `git merge` (today's result text) | `Validate` |
| Path gate | — | `tools/native/path.go` + `path_scope.go` | environment view for in-process readers |
| Checkpoint | — | `internal/checkpoint` unchanged; isolated runs need none | none |

No new interface type is proposed. The materialiser is a `switch` on a string in `worktree.Create`,
like the backend `switch` in `sandboxed()`.

### 2.2 Diagram

```
                    session run (profile P)          isolated run R (sub-agent or delegated profile run)
                    ───────────────────────          ───────────────────────────────────────────────────
ExecutionContext    WorkspaceRoot = session dir      WorkspaceRoot = <runs>/<R>/ws   WorkspaceOrigin = session dir
                    Sandbox = P's sandbox            Sandbox = P's ∩ R's, Environment{Holder pid, Tmp, GitWrite}
                          │                                     │
 tools ───────────────────┼──── sandboxed() ────────────────────┼───────────────── one chokepoint
                          │  per call: bwrap/seatbelt argv      │  overlay: nsenter -t <holder> … [-- bwrap ro for plan/read_only]
                          ▼                                     │  checkout: per-call bwrap/seatbelt as today
               host folder (session dir)                        ▼
                                                  ┌──────── holder (bwrap, one per run) ─────────┐
                                                  │ --overlay-src <base>[--overlay-src <delta>]   │
                                                  │ --overlay <R>/upper <R>/work <R>/ws           │
                                                  │ read/write/hide binds, --unshare-all          │
                                                  │ <R>/ws/.git → <common>/worktrees/<R> (rw)     │
                                                  │ GIT_OBJECT_DIRECTORY=<R>/objects (rw)         │
                                                  │ <common>/objects as alternate (ro)            │
                                                  └───────────────────────────────────────────────┘
 in-process readers (read_image, code_context, lsp view, cwd checks) ── /proc/<holder>/root/… (overlay)
                                                                        host path (checkout)
 Finish:  inside holder: git add/commit/pack-objects  ──pack──▶  host: index-pack, merge-tree, update-ref jad/<R>
```

### 2.3 Combining the three technologies: compatibility issues and their resolution

| # | Issue | Evidence | Resolution |
|---|---|---|---|
| C1 | Lower must not change while mounted: "the behavior of the overlay is undefined" | kernel `Documentation/filesystems/overlayfs.rst`, "Changes to underlying filesystems" **[external]** | The lower is never the live session folder: it is a frozen base checkout per commit (+ an optional delta layer for `base: "working"`, §5.2). Neither is in any write set. |
| C2 | An upper/work pair may be used by one mount only; two mounts sharing an upper are undefined | same document, "Sharing and copying layers" **[external]**; the kernel did **not** refuse a second concurrent mount here **[measured]** | One holder per run owns the only mount; every process of the run joins it (`nsenter`, 1 ms **[measured]**); a `flock` on `<R>/lock` held by jad makes a second holder impossible, also after a crash. |
| C3 | The merged view exists only inside the holder's mount namespace; on the host `<R>/ws` holds only `.git` | host listing showed 0 entries while the holder saw 29 **[measured]** | In-process readers resolve through `/proc/<holder>/root/<path>` (readable and writable by the same uid **[measured]**); host-side git for collection runs inside the holder. |
| C4 | Worktree creation inside each overlay would put `.git/worktrees/<R>` and new objects in the shared common dir, which every run must then write | spike: git in the overlay failed `index.lock: Read-only file system` with the common dir read-only **[measured]** | `git worktree add --no-checkout --detach` is made by jad on the host; only `<common>/worktrees/<R>` is bound rw; objects go to `<R>/objects` via `GIT_OBJECT_DIRECTORY`, shared objects are an alternate **[measured]**. |
| C5 | A `--no-checkout` worktree has an empty index: `git status` would list every file | 1,771 entries **[measured]** | Copy the base worktree's index (203 KB) into `<common>/worktrees/<R>/index`; status then reports 0 changes with no re-hash **[measured]**. |
| C6 | Directory renames without `redirect_dir` copy the whole subtree up | renaming a 3.0 MB folder put 3.0 MB in upper **[measured]** | Accepted; counted in §8; change tracking is git's (rename detection), not the upper's. |
| C7 | Deletes are whiteouts (`c 0,0`), not absent files | **[measured]** | Never parsed by jad: git in the merged view sees a deleted file. Recovery after a crash re-mounts the upper (holder is dead, so no C2 conflict) and runs the same git. |
| C8 | A bundle of the run's commits was refused by `git fetch` ("prerequisite commits … not connected") on git 2.56 | **[measured]** | Transfer is `git pack-objects --revs` (inside the holder) → `git index-pack --stdin --fix-thin` + `git update-ref` (host), which worked **[measured]**. |
| C9 | `git commit` in the run tried `packed-refs.lock` in the shared dir (auto maintenance) | **[measured]**, commit still succeeded | The run's git environment sets `GIT_CONFIG_COUNT` entries `gc.auto=0`, `maintenance.auto=false`. |
| C10 | Unprivileged overlay needs kernel ≥ 5.11 and bwrap ≥ 0.10 (`--overlay-src/--overlay`) | **[external]**; works here with 0.13.1/7.4 **[measured]** | Probed once per host (§R3); `auto` falls back to checkout. |
| C11 | macOS has no overlayfs and no `/proc/<pid>/root` | **[external]** | macOS uses the checkout materialiser and per-call seatbelt (§R3). |

### 2.4 Why in-process readers mirror the environment instead of being refused

read_image, code_context, the lsp tool's file view and the cwd checks run inside jad's own process
(`tools/native/path.go` callers, §1.2): there is no child to put into a namespace. Under an overlay
run they read through `/proc/<holder>/root`, i.e. *through the environment itself* — what is hidden
or not mounted for the run is absent for them too, with no separate policy. Under per-call
sandboxes (checkout, seatbelt) there is no live namespace to read through, so the gate in
`resolveExistingTarget` is given the environment's own sets (§9.4) and answers `not_found` for a
path the environment does not contain. That is a mirror of the environment, not an extra tool limit:
the tool sees exactly what a sandboxed child sees.

---

## 3. Workspace lifecycle (deliverable 3)

Owner of every step: the run's owner — `SubagentManager` for a sub-agent run
(`tools/native/subagent_manager.go:848-886`), the bound runner for a delegated profile run (§9.6).
Steps 1–4 replace `openWorktree` (`tools/native/subagent_worktree.go:69-90`); steps 8–12 replace
`closeWorktree` (`:97-115`). The order "collect after every process of the run stopped" is kept
(`subagent_manager.go:865-884`).

| # | Step | Overlay materialiser | Checkout materialiser (today + fixes) |
|---|---|---|---|
| 1 | Create run workspace | resolve top/common/start commit (3 `rev-parse`, as `worktree.go:94-121`); `mkdir <R>/{upper,work,objects,tmp}`; write `<R>/jad-run.json` (§10.2); take `flock <R>/lock` | as today, plus `<R>/tmp`, `<R>/objects`, `jad-run.json`, lock |
| 2 | Initialise isolated file system | acquire base `<common>/jad-worktrees/.base/<sha>` from the cache (miss: `git -c core.hooksPath=/dev/null worktree add --detach … <sha>` once, then `git worktree lock`); for `base: "working"` build the delta layer (§5.2) | `git -c core.hooksPath=/dev/null worktree add --detach <R>/ws <start>` (full checkout) |
| 3 | Attach git worktree/branch | `git worktree add --no-checkout --detach <R>/ws <start>`; copy the base index into `<common>/worktrees/<R>/index`; move `<R>/ws/.git` into `<R>/upper/.git` | already attached by step 2; HEAD detached |
| 4 | Start bwrap | holder: `bwrap --info-fd 3 --die-with-parent --unshare-all <read/write/hide binds> --tmpfs <common>/jad-worktrees --overlay-src <base> [--overlay-src <delta>] --overlay <R>/upper <R>/work <R>/ws --bind <R>/tmp <R>/tmp --bind <R>/objects <R>/objects … -- jad sandbox-hold`; record `child-pid`. Upper, work and other runs are not mounted into the child. That bwrap takes overlay and bind *sources* from the host root while the destinations sit on the tmpfs is to be checked in P0 | nothing long-lived; each call starts its own bwrap/seatbelt with the run's sets |
| 5 | Execute tools | `sandboxed()` emits `nsenter -t <pid> -U -m -p -n -i -u --preserve-credentials -- <prog>`; plan/`read_only` calls add a nested `bwrap --ro-bind / / --bind <R>/tmp …` (6 ms **[measured]**) | per-call bwrap/seatbelt as today |
| 6 | Track changes | git in the merged view; `hooks.shell_changes` scan stats through `/proc/<pid>/root` | git; scan on the host path |
| 7 | Preserve changes after execution | upper and private objects persist on disk; holder exit drops only the mount | worktree folder persists |
| 8 | Collect | one join running `git add --all && git commit … && git pack-objects --revs` (env: `GIT_OBJECT_DIRECTORY`, alternates, `gc.auto=0`, `--no-verify` unless `commit_hooks`); then stop holder | same commands, run through `sandboxed()` with the run's sets |
| 9 | Validate against target | host: `git index-pack`, then `git merge-tree --write-tree <target> <tip>` (§5.3) | same |
| 10 | Merge accepted changes | not automatic: branch `jad/<R>` is created (`update-ref`) and named in the result with its conflict list; the parent/user merges (§5.4) | same |
| 11 | Handle conflicts | reported, never resolved by jad (§6) | same |
| 12 | Cleanup | remove `<R>` (upper, work, objects, tmp), `git worktree prune`; release base ref; GC unused base; keep `<R>` and report its path when any step failed (as `Outcome.Kept`, `worktree.go:156-190`) | `git worktree remove --force` / prune as today |

Failed, timed-out and stopped runs follow the same path from step 8 (as today:
`subagent_manager.go:880-884`). A run whose collection fails keeps everything and reports it.

---

## 4. File-system layout (deliverable 4)

```
<common> = git rev-parse --git-common-dir (e.g. <repo>/.git)
<common>/objects/                         shared object store            read-only to every run
<common>/hooks/, <common>/config          repository hooks and config    read-only to every run (S2)
<common>/worktrees/<R>/                   run's worktree admin: HEAD (detached), index, logs   rw for run R only
<common>/jad-worktrees/                   DefaultDirName (worktree.go:26); hidden from every run except its own <R>
  .base/<sha>/                            frozen base checkout per start commit (linked, detached, locked)  ro lower
  .base/<sha>.refs                        one line per run holding it (refcount; removed under the cache lock)
  <R>/
    ws/                                   merged view (overlay) or full checkout; WorkspaceRoot of the run
    upper/  work/                         overlay layers (overlay only); never mounted elsewhere (C2)
    delta/                                working-state delta layer (base "working", overlay only)  ro lower
    objects/                              GIT_OBJECT_DIRECTORY of the run                  rw for run R
    tmp/                                  TMPDIR and {tmp} of the run; exec/tool_smith scratch    rw for run R
    changes.pack                          collected objects (step 8), removed after import
    jad-run.json                          persistent state (§10.2)
    lock                                  flock held while a holder may run
```

The session's own (non-isolated) run gets a per-session `{tmp}`: `<os.TempDir()>/jad-<hash(session key)>/`
created 0700 (§7.3, S3). Worktrees stay inside the common git directory for the reason recorded in
DECISIONS 2026-11-04 (search tools never look there, `/tmp` may be cleaned).

---

## 5. Git integration strategy (deliverable 5)

### 5.1 Branch and commit model

- Each run has a **detached HEAD** in its own admin dir; its own commits are on that HEAD, objects in
  `<R>/objects`. It cannot create branches or tags or update shared refs: those writes hit the
  read-only common dir and fail with `Read-only file system` — the environment says no, not a tool.
- At collection jad commits what is left (today's `git add --all` + commit, `worktree.go:156-170`),
  exports a pack of `tip ^start`, imports it on the host and creates `refs/heads/<prefix><R>` with
  `git update-ref` (7 KB pack for a one-line change **[measured]**). Branch naming, author fallback
  and `commit_hooks` keep today's settings (`sessionfs/native_subagent_worktree.go:16-46`).
- Unchanged runs create no branch (today: `branch -D`, `worktree.go:210-223`).

### 5.2 Base: `head` (default) or `working`

- `head` — the start commit is HEAD (today, `worktree.go:113-122`).
- `working` — closes T-119 open item (2) (DECISIONS:2547-2556). jad builds a snapshot commit `W` of the
  session's tracked and untracked, non-ignored files with a temporary index: copy `<git-dir>/index` to
  a temp file, `GIT_INDEX_FILE=<temp> git add -A`, `git write-tree`, `git commit-tree -p HEAD`. Copying
  the index keeps the stat cache, so only changed files are hashed **[estimate: cost ∝ size of the
  changed files]**. `W` is unreachable from refs and is collected by git's normal prune unless a run
  branch keeps it.
  - Overlay: lower = `.base/<HEAD>` (cached) under a **delta layer**: built once per `W` by mounting
    an overlay of `.base/<HEAD>` with `delta/` as upper in a one-shot bwrap and applying
    `git diff --name-status HEAD W` (copy changed files, delete removed ones); the result is a valid
    read-only lower (`--overlay-src base --overlay-src delta`), shared by every run of the same `W`.
  - Checkout: `git worktree add --detach <R>/ws W`.
  - Collection rebases the run's changes onto HEAD: `git merge-tree --write-tree --merge-base W HEAD tip`
    → `git commit-tree -p HEAD` → that commit is the branch, so the branch carries only the run's
    changes, never the user's uncommitted ones. When that three-way merge conflicts (the run edited
    lines the user had changed), the branch is `tip` on `W` and the result says so with the command
    to apply it (`git diff W <branch> | git apply --3way`).
- Ignored files (`node_modules`: 847 MB of this repository's 863 MB **[measured]**) are in neither
  base. They are offered by `isolation.share_paths` (§9.1): session-relative folders bound
  **read-only** from the live session folder into the run (not as overlay layers, so C1 does not
  apply). Writes to them fail by environment. Default empty (decision D6).

### 5.3 Validation

`worktree.Validate` (new, `internal/worktree/validate.go`) runs on the host after import:
`git merge-tree --write-tree --name-only <target> <branch>` where `<target>` is the session's HEAD,
and — when the session folder is dirty — the snapshot `W'` taken at that moment (§5.2). It returns the
conflicting paths. Result lines `worktree_conflicts:` and `worktree_target:` are added next to today's
`worktree_*` lines (DECISIONS 2026-11-04 "messages.jsonl": only isolated runs carry them).
`merge-tree --write-tree` needs git ≥ 2.38 **[external]**; with an older git the line is
`worktree_conflicts: not checked (git < 2.38)`.

### 5.4 Merging

Unchanged in principle: the parent (model through exec, or the user) merges the branch; the result
already names the commands (DECISIONS 2026-11-04). `git merge` refuses to overwrite uncommitted local
changes of the files it touches **[external]**, so a merge never silently overwrites the user's or
another run's work. jad adds no merge tool (rule: do not add or limit tools). For a parent in a
sandbox the merge needs write access to the common dir minus the protected paths (§7.2), which the
non-isolated session has by default.

---

## 6. Concurrency and conflict handling (deliverable 6)

| Situation | Behaviour |
|---|---|
| Runs change different files | independent uppers/checkouts; branches merge cleanly; `merge-tree` reports none |
| Runs change the same file | each branch holds its version; the second merge conflicts; `Validate` already reports it at the second run's end when the first was merged; jad never picks one |
| Runs create files in the same directory | per-run uppers; a same-name file is a conflict like any other |
| Dependencies (B needs A's result) | B starts from A's branch: today a resume does this (`from`, `subagent_worktree.go:73-79`); for a new run the parent merges A first or B is resumed from A — no new tool argument (decision D8) |
| Runs based on different project versions | each branch records its start; `merge-tree` validates against the current target |
| One run finishes while another runs | collection touches only `<R>`, the shared object store (append-only `index-pack`) and one new ref; the repository lock (`worktree.go:139-141`) serialises `worktree add/prune` and base-cache changes |
| Cancellation during file writes | the holder is stopped after the child's processes (`subagent_manager.go:864-868`); overlay copy-up is atomic (done in `work/` then renamed **[external]**); a half-written file is committed as the run left it, as today |
| Run crash | holder dies with jad (`--die-with-parent`) or alone; `jad-run.json` says `running`; recovery (§10) collects it |
| Main workspace changes during runs | no effect on runs (frozen lower or own checkout); integration reconciles |
| Two runs of one base | share `.base/<sha>` and its `delta/` read-only (allowed **[external]**); nothing shared is writable |
| Parallel calls inside one run | same as today in the session: they share the run's workspace, so ordered scheduling and `read_only` keep their meaning (§R1) |
| Base cache concurrency | `acquire`/`release` under the per-repository lock; the base is removed only at refcount 0 and when it is not the current HEAD |

---

## 7. Security model (deliverable 7)

### 7.1 Isolation kinds, kept apart

| Kind | Mechanism | Covers |
|---|---|---|
| File system | bwrap mounts / seatbelt rules from read, write, hide (§9.3); overlay for run state | what a process can read and write |
| Process | `--unshare-all` (pid, ipc, uts, cgroup, user); one pid namespace per holder (calls of one run can signal each other, runs cannot); no capabilities in a joined process (`CapEff 0`, remount refused **[measured]**) | signals, ptrace, `/proc` |
| Network | today's `network`, `network_allow` proxy (`tools/native/sandbox.go:100-136`) | unchanged |
| Privilege | unprivileged user namespace; bwrap sets no-new-privs **[external]**; setuid binaries cannot gain host root inside a user namespace **[external]** | escalation |
| Environment variables | allowlist + secret removal (`shell_run.go:266-306`) | leaked tokens |

### 7.2 Findings: escapes that exist today (and their fix)

- **S1 — repository hook escape.** With the default writable `{git_common_dir}` (`sessionfs/sandbox.go:45`)
  a sandboxed exec can write `.git/hooks/post-checkout`; the next `git worktree add` that jad runs on
  the host (`worktree.go:142`, unsandboxed `:233-256`) executes it outside the sandbox. Reproduced
  **[measured]**. *Fix:* `<common>/hooks` is always bound read-only after the writable binds (bwrap) /
  denied for write (seatbelt); every host git command jad runs gets `-c core.hooksPath=/dev/null`
  (commit hooks, when `commit_hooks` is on, run inside the run's environment instead).
- **S2 — config escape.** The same sandbox can set `core.fsmonitor` in `.git/config`; the next host
  `git status` (`Finish`, `worktree.go:158`) runs it. Reproduced **[measured]**. *Fix:* `<common>/config`
  read-only to every run; host git gets `-c core.fsmonitor=false`; collection runs in the run's
  environment anyway.
- **S3 — shared `{tmp}`.** Every sandbox shares the process TMPDIR (`exec.go:473-475`): every call's
  scratch dir (0700, but owned by the same uid as every sandboxed child) holds its code, template and
  result files, and a `pipe` standby's code FIFO (`exec_standby.go:425`). A run with `{tmp}` writable
  can therefore rewrite another run's or the session's result file, or feed code to a waiting pipe
  standby, which then executes in that other, possibly wider, environment **[estimate: by reading the
  code; not reproduced]**. *Fix:* `{tmp}` expands per run (`<R>/tmp`) and per session
  (`jad-<hash>`), jad's scratch dirs are made there (`makeExecWorkDir` gets a dir argument), the
  child's `TMPDIR` is set to it, and other runs' tmp dirs are not mounted.
- **S4 — sibling runs.** Worktrees live under `<common>/jad-worktrees` (`worktree.go:26`), inside the
  writable `{git_common_dir}`: run A can rewrite run B's files and B's branch ref. *Fix:*
  `<common>/jad-worktrees` and `<common>/worktrees` are hidden (tmpfs) except the run's own `<R>`
  and admin dir.
- **S5 — bind-source swap (TOCTOU).** A writable path listed under a folder the run can write (e.g.
  `{workspace}/build`) can be replaced by a symlink before bwrap binds it, binding an arbitrary target
  read-write **[estimate: from bwrap's source-follows-symlinks behaviour, not reproduced]**. *Fix:* jad
  resolves each write path, refuses one whose resolved form lies outside the run's roots, and passes
  it to bwrap by file descriptor (`--bind-fd`, opened `O_PATH|O_NOFOLLOW`) — to verify in Phase 0
  that the installed bwrap has `--bind-fd`.

### 7.3 The remaining questions of the brief

- *Read-only base:* the base and delta layers are in no write set and are lowers only.
- *Other agents' workspaces:* S4.
- *Symlink and mount escapes:* S5; overlay follows the kernel's rules; seatbelt already resolves
  symlinks first (`sandbox_seatbelt.go:11-16`); joined processes have no capabilities, so they cannot
  unmount the overlay to reveal what is under it **[measured]**.
- *Git metadata and credentials:* runs read the whole history (same project); they write only their
  admin dir and objects (§5.1). Credentials in `{home}` are readable under the default `read: ["/"]`;
  `hide_paths` is the control, and decision D5 proposes default hidden credential paths.
- *Network:* unchanged; with network off a run cannot exfiltrate except through what it writes.
- *Temporary files and cleanup:* S3; `<R>/tmp` is removed with `<R>` (§10).
- *Not covered by design:* jad's own process (it is the trusted party), HTTP MCP (remote), checkpoint
  restore (user action) — see §R2.

---

## 8. Storage and performance (deliverable 8 and required section R5)

### 8.1 What each option costs here

Measured on this repository (§1.8), warm page cache, run folders on tmpfs **[measured]** unless marked.

| Option | Creation | Disk per run | Copy-up on first write | Change collection | Integration | Ignored files present |
|---|---|---|---|---|---|---|
| Full copy (`cp -a`, incl. ignored) | 2.18 s | 900 MB | none (already copied) | needs a reference copy or git index refresh | diff/merge by hand or git | yes |
| Git worktree alone (today) | 0.07 s (`worktree add`, 1,771 files) | 19 MB | none | `git status` 0.02 s | branch, `git merge` | no |
| Overlay alone (no git) | 0.01 s (`bwrap --overlay` start) | changed files only | whole file (13,960 B for one appended line); a renamed 3.0 MB folder copied whole | parse upper incl. whiteouts (`c 0,0`) and opaque dirs | hand-made patch | only if in lower |
| Overlay + worktree metadata (recommended) | `worktree add --no-checkout` 0.00 s + index copy 203 KB + holder 0.01 s; base miss adds one checkout (0.07 s) per start commit | changed files + private objects (20 KB for one commit) | as overlay | `git status` with copied index: 0 changes, no re-hash; commit + pack (7,352 B) | `index-pack` + `merge-tree` + `update-ref` | via `share_paths` (ro) |
| Btrfs snapshot / reflink copy | **[estimate]** subvolume snapshot O(1) but only if the session folder is a subvolume (jad cannot make it one); `cp --reflink` O(files): metadata of 34,835 files, of the order of the `cp -a` time minus the data copy | metadata only, CoW per extent | CoW per extent, not per file | needs git (index copy) like a checkout | branch | yes (if copied) |

Not measured: reflink on this btrfs source (scratch was limited to tmpfs), anything on macOS, cold
caches, and repositories larger than this one.

### 8.2 Extrapolation **[estimate]**

- Checkout cost scales with bytes written (git decompresses and writes every tracked file): 16 MB →
  0.07 s here suggests ~4–5 s per GB on a warm cache; a 1 GB / 80k-file checkout per run is seconds and
  1 GB of disk per run, paid N times for N runs. Overlay pays it once per start commit.
- Copy-up scales with the size of files first written; a run that rewrites a 500 MB artefact copies
  500 MB. New build outputs are plain writes to upper.
- Index copy is proportional to tracked files (≈115 bytes per entry here: 203 KB / 1,771).
- Process cost per call: nsenter join 1 ms, nested read-only bwrap 6 ms, a fresh bwrap with overlay
  10 ms **[measured]**; today's per-call bwrap is the same order as the last.
- Git invocations per run: today up to 4 at creation and 9 at finish (`worktree.go:89-223`). Proposed
  overlay: 4–5 at creation (+1 on a base-cache miss), 1 joined script at collection, 3–4 on the host
  (`index-pack`, `merge-tree`, `update-ref`, `worktree prune`).

### 8.3 Choice

Overlay + worktree metadata where available; checkout elsewhere. Btrfs snapshots are rejected as the
mechanism (subvolume requirement, Linux btrfs only, no change tracking of their own); reflink/APFS
clone is a later, optional materialiser for the base→run step on macOS (Phase 6) because it keeps
the host-visible checkout path.

---
## 9. Go interfaces, data structures and configuration (deliverable 9)

### 9.1 Settings (`internal/sessionfs`)

`sessionfs/sandbox.go` — `SandboxSettings` gains one field and two keys change spelling:

```go
type SandboxSettings struct {
    // … existing fields (sandbox.go:67-87) …
    // ReadPaths are the only paths a child can read (read.paths); default ["/"], which is
    // today's --ro-bind / /. WritablePaths and PlanWritablePaths keep their meaning and are
    // read from write.paths and write.plan_paths (writable_paths and plan_writable_paths stay
    // accepted spellings, D11).
    ReadPaths []string
}
```

`sessionfs/sandbox_isolation.go` (new, ~120 lines, modelled on `native_subagent_worktree.go:65-134`)
parses the `isolation` sub-object of a sandbox object:

```go
// IsolationSettings is sandbox.isolation: how a delegated run that uses this sandbox works.
type IsolationSettings struct {
    Mode        string   // "none" (default) | "worktree"
    Base        string   // "head" (default) | "working"
    Materialize string   // "auto" (default) | "checkout" | "overlay"
    SharePaths  []string // session-relative folders bound read-only into the run (default none)
}
// SandboxSettings gains: Isolation IsolationSettings; Template string (the name it came from, §14).
```

Isolation lives **inside** the sandbox object, so one template (§14) carries the whole environment of a
run — confinement and workspace — and `layoutSettingKeys` (`layout_files.go:30-36`) needs no new key:
`sandbox` is already allowed on root defaults, sessions and profiles. `sandboxKeys`
(`sessionfs/sandbox.go:51`) gains `read`, `write`, `isolation`, `template`. Unlike the rest of the
sandbox, `isolation` is read even when `enabled` is false (a worktree without confinement is today's
`isolation: "worktree"`). The agent key `isolation` (`none|worktree`,
`native_subagent_worktree.go:67-83`) stays as a shorthand for `sandbox.isolation.mode` of that agent;
setting both on one agent is refused. `tools.native.subagent.worktree` (git, prefix, author, hooks,
timeout) stays where it is and also serves delegated profile runs (defaults from
`DefaultNativeSubagentWorktreeSettings`, `:48-56`, when the section is absent).

### 9.2 The proposed configuration

Normally a profile or an agent only names a template (§14):

```json
"settings": {"sandbox": "workspace-writer"}
```

The same object can still be written inline, or a template can be named with overrides:

```json
"sandbox": {
  "template": "workspace-writer",
  "network": true
}
```

```json
"sandbox": {
  "enabled": true,
  "tools": ["exec", "tool_smith", "hooks", "mcp", "lsp"],
  "read":  {"paths": ["{system}", "{workspace}", "{home}/.cache/go-build"]},
  "write": {"paths": ["{workspace}", "{git_common_dir}", "{tmp}"], "plan_paths": ["{tmp}"]},
  "hide_paths": ["{workspace}/.env"],
  "isolation": {"mode": "worktree", "base": "working", "materialize": "auto", "share_paths": ["node_modules"]}
}
```

New placeholder `{system}`: the platform's runtime set (Linux: `/usr /bin /sbin /lib /lib32 /lib64
/libx32 /etc`; macOS: `/System /usr /bin /sbin /Library /private/etc /private/var/db /dev`), each
dropped when absent like other paths (`tools/native/sandbox.go:141-160`). The macOS list is
unverified (Phase 0). New `SandboxTools` value `lsp` (`sessionfs/sandbox.go:38`).

### 9.3 Resolved precedence of one call's environment

Applied in this order by `bwrapArguments` (`tools/native/sandbox.go:100-136`) and `seatbeltProfile`
(`sandbox_seatbelt.go:17-46`); a later step wins over an earlier one for the same path:

1. **Nothing.** bwrap starts from an empty root; seatbelt gets `(deny file-read*)` unless `read.paths`
   is `["/"]` (then `--ro-bind / /` / `(allow default)` as today).
2. **`read.paths`** — bound read-only (`--ro-bind-try`), seatbelt `(allow file-read* (subpath …))`.
3. **`write.paths`** — or `write.plan_paths` for a plan-mode run and for an exec call with `read_only`
   (today's `ForPlanMode`, `sessionfs/sandbox.go:267-274`) — bound read-write. **Write implies read**:
   a write path is readable even when no read path covers it (the user's rule), because a bind is
   both.
4. **`hide_paths`** — empty tmpfs / `/dev/null`; wins over read and write (a secret inside a writable
   workspace stays hidden), which is today's order (`sandbox.go:110-121`: hides follow writes).
5. **Fixed environment rules, not configurable** (§7.2): `<common>/hooks`, `<common>/config` read-only;
   `<common>/jad-worktrees` and `<common>/worktrees` hidden except the run's own `<R>` and admin dir.
6. **Isolation narrowing** (an isolated run never reaches more than its own state, in the spirit of
   `narrowScope`, `path_scope.go:115-156`): `{workspace}` = `<R>/ws`; `{tmp}` = `<R>/tmp` (a
   non-isolated session: `jad-<hash>`); `{git_common_dir}` in `write` = the run's admin dir and
   `<R>/objects`, in `read` = the common dir; `share_paths` bound read-only.
7. **jad's plumbing:** `/dev`, `/proc`, the call's scratch dir (inside `{tmp}`), the network proxy
   socket and the jad binary that forwards to it (`sandbox.go:125-132`), and the jad binary that runs
   `sandbox-hold` in a holder — always mounted read-only, whatever `read` lists.

**`read` is deny-by-default as a key, with `["/"]` as its default value.** What is not listed is not in
the environment — that *is* limiting the environment, and it is the only form an allowlist can take
in a mount namespace. The default value keeps today's behaviour (`--ro-bind / /`), so no existing
sandbox changes until a profile lists paths. An allow-by-default key with a deny list would be
`hide_paths` again and cannot express "only these". The two shapes of `NativePathPolicy`
(`file_settings.go:125-170`, `:169`) are not copied: `read` and `write` are plain arrays with one
meaning.

**Interaction with the in-process gate (`NativePathRule`/`NativePathPolicy`).** When a sandbox is
enabled, `scopedPolicy` (`path_scope.go:14-38`) additionally intersects each in-process tool's policy
with the environment: `Within` += the resolved roots of read ∪ write, deny += `hide_paths` and the
fixed hidden paths. With `read: ["/"]` this changes nothing. Under an overlay holder the readers go
through `/proc/<holder>/root` instead (§2.4) and the intersection is only a second line. exec's and
tool_smith's own `paths` keep gating the cwd (`shell_run.go:114`) — redundant under a sandbox (bwrap
`--chdir` fails outside the environment), kept for unsandboxed sessions (D9).

### 9.4 Per-call context (`internal/tools/catalog.go`)

```go
type ExecutionContext struct {
    // … existing (catalog.go:28-68) …
    // Environment, when set, is the environment of an isolated run: every child of the call
    // starts in it, and in-process readers see through it. Nil: the session's sandbox as
    // configured. ForNested copies it, so a nested run stays inside its parent's.
    Environment *Environment
}

type Environment struct {
    Sandbox *sessionfs.SandboxSettings // the run's resolved sandbox; nil = none
    Holder  int                        // pid of the run's holder; 0 = a sandbox per call
    Tmp     string                     // the run's {tmp}
    Workspace string                   // <R>/ws (also WorkspaceRoot)
    GitWrite []string                  // what {git_common_dir} means in write
    Hidden  []string                   // fixed hidden paths (other runs, §9.3 step 5)
    Share   []string                   // resolved share_paths (read-only)
}
```

The `tools` package already imports `sessionfs`, so no new dependency.
`ForNested` (`catalog.go:108-117`) copies the struct field unchanged.

### 9.5 Spawn and view (`internal/tools/native`)

- `sandbox.go` `callSandbox` (`:34-39`): use `execution.Environment.Sandbox` when an environment is
  set, then the plan-mode form. `sandboxPlace` (`:17-22`) gains `tmp`, `gitWrite`, `hidden`, `share`,
  `holder`; every caller already builds a place, so their signatures do not change.
- `sandboxed()` (`:45-88`): new first branch for `place.holder != 0` returning
  `nsenter -t <pid> -U -m -p -n -i -u --preserve-credentials -- [bwrap <plan-mode binds> --] <program>`;
  `--wd` is not used (it resolved outside the namespace in the spike **[measured]**), the child's
  `Dir` stays the logical cwd, which exists inside.
- `sandbox_holder.go` (new): `startHolder(sandbox, place, tree) (*sandboxHolder, error)` (bwrap with
  `--info-fd`, the run's sets and the overlay; parses `child-pid`), `(*sandboxHolder).stop()`, and the
  hidden subcommand `jad sandbox-hold`, which waits for stdin EOF (precedent: `SandboxNetForwardCommand`,
  `sandbox_network.go:32`). Started under the run's `flock`.
- `sandboxKey` (`:207-213`) includes the holder pid and `tmp`, so standbys never cross runs.
- `exec.go:473-475` `makeExecWorkDir(dir string)`; callers pass `{tmp}` of the call; the child's
  `TMPDIR` is set to it (`shell_run.go:266-306`).
- `hooks.go:170`: `callSandbox(hooks.Sandbox, execution)` (also fixes U1).
- `lsp_client.go:136-148`: start through `SandboxCommand` when the sandbox covers `lsp`; `lspKey`
  (`lsp_client.go:58`) gains the sandbox key so a run's server is its own.
- `path.go:23-102`: `resolvedTarget` gains `openPath`. With `Environment.Holder != 0` the candidate's
  symlinks are resolved inside `/proc/<pid>/root` (`openat2` with `RESOLVE_IN_ROOT`), `realPath`
  stays the logical path for display and policy, and the 8 callers (§1.2) open `openPath`.
  `shell_changes.go:27-46` scans through the same root.

### 9.6 Worktree (`internal/worktree`)

```go
type Options struct {
    // … existing (worktree.go:32-48) …
    Base        string        // "head" | "working"
    Materialize string        // "auto" | "checkout" | "overlay"; auto = overlay when Probe says so
    Wrap        CommandWrapper // runs a git command inside the run's environment (set by the owner)
}

// CommandWrapper has the shape of the MCP manager's wrapper (app/mcp_sampling.go:53-55).
type CommandWrapper func(program string, args []string, cwd string) (file, argv0 string, arguments []string, err error)

type Tree struct {
    // … existing Path, Workspace, Branch, Base (worktree.go:51-62) …
    Start   string   // commit the files start from: Base, or W for base "working"
    Mode    string   // "checkout" | "overlay"
    Lower   []string // overlay lowers, top first (delta, base)
    Upper, Work, Objects, Tmp, Admin string
    state   string   // jad-run.json
    lock    *os.File // flock held while a holder may run
}

func Create(ctx context.Context, options Options, workspace, name, from string) (*Tree, error) // signature kept
func (t *Tree) Collect(ctx context.Context, message string) (Outcome, error) // status/add/commit/pack via Wrap
func (t *Tree) Import(ctx context.Context) (Outcome, error)                  // index-pack, rebase W→HEAD, update-ref
func (t *Tree) Remove(ctx context.Context, dropBranch bool) error            // exported remove (worktree.go:210-223)
func (t *Tree) Finish(ctx context.Context, message string) (Outcome, error)  // Collect+Import+Validate+Remove
func Validate(ctx context.Context, options Options, workspace, branch string) ([]string, error)
func Recover(ctx context.Context, options Options, common string, alive func(Server) bool) ([]Outcome, error)
func Probe(ctx context.Context, options Options) (overlay bool, reason string) // once per host, cached
```

`Outcome` (`worktree.go:65-76`) gains `Conflicts []string`, `Target string`, `StartedFromWorking bool`.
Every git command jad runs on the host gets `-c core.hooksPath=/dev/null -c core.fsmonitor=false`
in `run` (`worktree.go:233-256`). The base cache is unexported (`base.go`): `acquire(commit)`,
`release`, `gc`, all under `lockFor(common)` (`worktree.go:139`).

### 9.7 Resource ownership and integration with the pipeline

| Resource | Owner | Released by |
|---|---|---|
| `<R>` folder, admin dir, branch | the run owner via `Tree` | `Finish` / `Recover`; kept and reported on failure |
| holder process + mount | the run owner (`sandboxHolder`) | `stop()` after the run's handles stopped, before `Collect` ends; `--die-with-parent` on crash |
| `flock <R>/lock` | the run owner | `Remove`, or the kernel when jad dies |
| base checkout | base cache (refcount) | last `release` + `gc` |
| per-session `{tmp}` | the session (resolver) | session close; stale ones swept like exec scratch (`sweepStaleScratchOnce`, `exec.go:76`) |

- **Sub-agent runs:** `worktreeStart` (`subagent_call.go:224-232`) adds `Base/Materialize/Share` from
  the agent's resolved `SandboxSettings.Isolation` (§14.4); `openWorktree` (`subagent_worktree.go:69-90`) calls `Create`, starts the
  holder (overlay), and sets `Environment` next to `WorkspaceRoot/Origin/PathScope`; `closeWorktree`
  (`:97-115`) calls `Finish` with `Wrap` joining the holder, then stops it.
- **Delegated profile runs** (the run a `profile_message` starts in the target profile's session,
  `app/profile_message.go:50`): when the target's resolved `SessionConfig.Sandbox.Isolation.Mode` is `worktree`, the
  bound runner opens a `Tree` where it sets the run's base context (`app/bound_runner.go:160-161`),
  sets no checkpoint for that run (it writes no session file; `:158`), and finishes it at run end,
  reporting the same `worktree_*` lines. The interactive session of a profile is not isolated (D7).
- **Sandbox per profile and per agent:** a profile's `settings.sandbox` (`layout_load.go:318-324`)
  names a template or holds an object (§14); an agent may name its own, which is narrowed by its
  parent's (§14.5); an agent without one gets its parent's (`app/subagent.go:235-249`) as today.

---

## 10. Failure recovery and cleanup (deliverable 10)

### 10.1 Failure modes

| Failure | Result |
|---|---|
| `Create` fails part-way | removes what it made (as `worktree.go:142-148`); the run fails before the child starts (`subagent_manager.go:851-856`) |
| holder fails to start | same as above; overlay probe result cached as "no" for the host with the reason; `auto` falls back to checkout for later runs; an explicit `overlay` fails the run |
| a call cannot join (holder gone) | the call fails with `sandbox_unavailable`; the run is collected as stopped |
| `Collect` fails | everything kept, `worktree_error` + `worktree_path` as today (`subagent_worktree.go:104-112`) |
| `Import`/`Validate` fails | pack and `<R>` kept; branch not created; reported |
| jad crashes | holder dies (`--die-with-parent`), mount disappears with its namespace (no stale host mount), upper/objects stay; `Recover` at the next start |
| disk full during copy-up | the run's write fails with ENOSPC inside the run; collection still runs |

### 10.2 Persistent state

`<R>/jad-run.json`, written atomically (temp + rename) at each phase change:
`{"version":1,"run":"sub_…","session":"<key>","server":{"pid":…,"start":…},"mode":"overlay",
"start":"<sha>","head":"<sha>","lower":["…"],"branch":"jad/sub_…","phase":"running|collecting|imported|failed"}`.
The server identity reuses `serverIdentity`/`foreignServerAlive` (`tools/native/server_identity.go:21-24`).
The sub-agent run store keeps its own record (`subagentWorktree`) as today.

### 10.3 Recovery (closes the gap at `subagent_manager.go:147-165`)

At startup `SubagentManager.recover` (and the resolver, for delegated runs) calls `worktree.Recover`:
for every `jad-run.json` whose server is not alive and whose lock can be taken — phase `running` or
`collecting`: re-collect with a one-shot bwrap over the same lowers and upper (no holder is alive, so
C2 holds), import, create the branch, record the outcome on the lost run; phase `imported`: `Remove`.
A run of a live foreign server is left alone (as `recover` already does, `:150-154`). Then `gc` the base
cache.

### 10.4 Cleanup guarantees

After a successful `Finish` nothing of `<R>` remains except the branch (or nothing, when unchanged).
After any failure everything remains and its path is reported. Nothing holding uncommitted work is
deleted. Base checkouts are deleted only at refcount 0 and when not the current HEAD.

---

## 11. Testing strategy (deliverable 11)

| Layer | Tests (beside the code, as `backend-go/CLAUDE.md` requires) |
|---|---|
| `sessionfs` | `read`/`write` parsing, the old spellings, both-spellings refusal, `{system}`, `lsp` tool value, `sandbox.isolation`, refusals by name; schema agreement in the style of `sandbox_test.go:140-173` for `sandbox` and for template files; template tests of §14.8 |
| `tools/native` unit | `bwrapArguments` argv for every precedence step (§9.3); `seatbeltProfile` text; expander with per-run `{tmp}`/`{git_common_dir}`; `sandboxed()` holder branch argv; `callSandbox` with `Environment`; `sandboxKey` differs per run |
| real bwrap (`requireBwrap`, as `sandbox_test.go:46-66`) | a file outside `read` is absent; write implies read; hide wins over write; regression tests for S1–S4 (each fails without its fix, checked with `mutate-check.sh`); S5 symlink swap |
| overlay (skip unless `worktree.Probe`) | holder start/join; two joins see one another's writes; a second holder for the same run is refused by the lock; nested read-only call cannot write; read_image of a file created only in the overlay; copied index ⇒ `git status` empty before a change; pack import; kill the holder, `Recover` ⇒ branch |
| `worktree` | base-cache refcount and GC with 8 concurrent runs under `-race` (as the existing 8-run test, DECISIONS 2026-11-04); `base: "working"` snapshot, rebase onto HEAD, conflict fallback; `Validate` conflicts; git < 2.38 path; host git ignores repository hooks and fsmonitor |
| `app` integration | headless isolated sub-agent with real tools (pattern of `app/subagent_worktree_integration_test.go`); a delegated profile run whose template sets `isolation`; a profile and two agents sharing one template |
| contract | `messages-golden-diff`: tool descriptions change through `sandboxDescription` (`sandbox.go:217-246`) only when a sandbox is on; exec's schema is unchanged while `read_only` stays (§R1) |
| performance | `worktree` `*_benchmark_test.go`: `Create`+`Finish` per materialiser on a generated 10k-file repository |
| per-call choice | §15.6 |
| macOS | seatbelt read-deny and `{system}` need a macOS runner; none exists in this setup (gap) |

---

## 12. Implementation phases with dependencies (deliverable 12)

| Phase | Content | Depends on | Changes |
|---|---|---|---|
| P0 | Spikes on target hosts: bwrap `--overlay` and `--bind-fd`, Ubuntu's AppArmor user-namespace restriction, seatbelt `(deny file-read*)` with `{system}`, `nsenter` presence | — | none (report) |
| P1 | Security fixes in today's model: S1, S2 (host git flags; protected git paths), S3 (per-session/run `{tmp}`), S4 (hide sibling runs), U1 (hooks plan mode), `Recover` for lost worktrees (checkout mode) | P0 for S5 only | `sessionfs/sandbox.go`, `tools/native/sandbox.go`, `sandbox_seatbelt.go`, `exec.go`, `hooks.go`, `worktree/worktree.go`, `subagent_manager.go`, schema text |
| P2 | `sandbox.read`/`write`, `{system}`, `lsp` in the sandbox, `sandbox.isolation` (mode only), sandbox templates `.jad/sandbox/<name>.json` with references from defaults, sessions, profiles and agents, agent narrowing (§14), `ExecutionContext.Environment`, in-process mirror | P1 | `sessionfs/sandbox.go`, `sessionfs/sandbox_isolation.go`, `sessionfs/sandbox_template.go`, `sessionfs/sandbox_narrow.go`, `layout_root.go`, `layout_files.go`, `layout_load.go`, `catalog_files.go`, `native_subagent_settings.go`, `native_subagent_dir.go`, `session.go`, `tools/catalog.go`, `tools/native/{sandbox,path,path_scope,lsp_client,catalog}.go`, `app/resolver.go`, `setting.schema.json`, new `sandbox.schema.json`, `agent.schema.json` |
| P2b | Per-call `sandbox` in `subagent` and `profile_message` (`sandbox_choice`, §15) | P2 | `sessionfs/native_subagent_settings.go`, `native_profile_message_settings.go`, `layout_load.go`, `tools/native/{subagent,subagent_call,subagent_model,profile_message,catalog}.go`, `app/{profile_message,profile_message_queue,resolver,locked_config}.go`, `httpapi/{profile_run,handler_ports,handler}.go`, `sessionfs/{session,settings_stamp}.go`, `sample/tools/native/{subagent,profile_message}.json`, `setting.schema.json` |
| P3 | Isolated runs sandboxed with private git (checkout materialiser): detached HEAD, private objects, `Collect` in the environment, pack `Import`, `Validate`, `base: "working"`, delegated profile runs | P2 | `worktree/*`, `subagent_worktree.go`, `subagent_call.go`, `app/bound_runner.go`, `agent.schema.json` text |
| P4 | Overlay materialiser, holder, base cache, `/proc/<holder>/root` view, `share_paths` | P3, P0 | `worktree/{overlay,base}.go`, `tools/native/sandbox_holder.go`, `path.go`, `shell_changes.go` |
| P5 | Remove `read_only_calls`; decide the sandbox default (implemented 2026-11-09, docs/isolation-decisions.md "P5 implementation") | P1, decisions D2/D3 | `sessionfs/native_exec_settings.go`, `removed_keys.go`, `tools/native/exec.go` text, schema |
| P6 | Optional: APFS/reflink clone materialiser; per-run stdio MCP (D10) | P3 | `worktree`, `app/mcp_sampling.go` |

Each phase ends with its tests, `verify-go.sh` and a DECISIONS entry.

---

## 13. Risks, limitations and unresolved decisions (deliverable 13)

- **Kernel/bwrap dependency of the overlay path** (C10): older distributions run the checkout path.
- **Ubuntu ≥ 23.10 restricts unprivileged user namespaces through AppArmor** **[external]**; the
  distribution's bwrap has a profile, a custom one may not — `jad check` already catches this
  (`sandbox_probe.go:19-56`).
- **`nsenter` is a new runtime dependency** (util-linux) for the overlay path (D14).
- **In-process readers through `/proc/<pid>/root`** need `openat2(RESOLVE_IN_ROOT)` (Linux ≥ 5.6
  **[external]**) and care with every `os.Open` of a resolved path; missing one reads the empty host
  folder, not a wrong file.
- **Copy-up of large files and directory renames** (C6) can cost as much as a copy for runs that touch
  big artefacts.
- **Seatbelt read restriction** is unverified; `sandbox-exec` is deprecated by Apple **[external]**
  though still shipped.
- **Ignored files** are absent from runs unless shared read-only (D6); tools that write into them
  (`npm install`) fail in the run.
- **MCP servers** of an isolated run still act on the session folder (D10).
- **A shared template is shared on purpose:** editing `.jad/sandbox/<name>.json` changes the
  environment of every profile and agent that names it at their next load (settings stamp, §14.1), and a
  narrowing that becomes a widening is refused at that load, which can stop a session from loading until
  the template or the agent is fixed.
- **A holder's mounts are fixed when it starts:** a `read`/`write` path that uses `{cwd}` (per call) can
  only be narrowed by the nested per-call bwrap, never widened; in holder mode `{cwd}` paths are
  resolved against the run's workspace root. To be documented in the schema text.
- **A deny-by-default `read` list breaks interpreters outside `{system}`** (for example toolchains under
  `{home}/.mise` or `{home}/.cargo`): the profile must list them; the tool descriptions already tell the
  model what the sandbox allows (`sandboxDescription`, `tools/native/sandbox.go:217-246`) and gain the
  read list.
- **Agents' git** cannot make branches or tags (D13).
- **Unverified here:** reflink costs, macOS, cold caches, large repositories, submodules and Git LFS
  (still unproven since DECISIONS 2026-11-04), `--bind-fd`, the S3 and S5 attacks (reasoned, not
  reproduced).
- Unresolved decisions: §R6.

---

## 14. Sandbox templates: `.jad/sandbox/<name>.json`

One environment definition — confinement (`read`, `write`, `hide_paths`, network, covered tools) and
workspace (`isolation`) — is written once as a named file and used by any number of profiles and
sub-agents by name, the way an agent folder under `.jad/subagents/<name>/` is written once and a
toolset `.jad/tools/<category>/<name>.json` is named by many profiles.

### 14.1 Where templates live

- Folder: root setting `catalogs.sandbox`, default `.jad/sandbox` — one more entry in
  `layoutCatalogDefaults` (`sessionfs/layout_root.go:13-20`), one more `Layout` field
  (`sessionfs/layout_files.go:17-26`), and `sandbox` added to the list in the `catalogs.<key>` error
  (`layout_root.go:36`). The value takes one of three forms (D28):

  | Value | Resolved against | When |
  |---|---|---|
  | absolute path (or `~/…`, through `expandHome`) | itself | at layout load |
  | relative path (the default `.jad/sandbox`) | the root setting file's directory, like every other catalog (`resolveFrom`, `sessionfs/roots.go:928-933`) | at layout load |
  | `{cwd}/…` | the session's `cwd` (`sessionCWD`, `sessionfs/session.go:1249-1266`), so each project can keep its own templates | per session, in `LoadSessionSettings`, since `cwd` is a session setting |

  `{cwd}` is the only placeholder accepted here and only as the first element; any other `{…}` is
  refused with the list of the three forms. A `{cwd}` folder always uses the **session's** `cwd`,
  never a sub-agent's worktree: an isolated child's copy of the folder is the child's own writable
  files and must not decide the child's environment. The catalog API (§14.6) lists the folder that
  the session resolved; with `{cwd}` the editor needs a session to list it.
- **The template folder is never writable from a sandbox.** When the resolved folder lies inside a
  sandbox's `write.paths` (always the case for `{cwd}/.jad/sandbox` under a `{workspace}` writer), it
  is bound read-only in that sandbox, like the protected git paths of P1 (§7.2 S1/S2). Otherwise a
  run could edit `workspace-writer.json` and, because an edited template reloads the sessions that use
  it (`noteFile`), widen its own next environment. Test: a sandboxed exec under `{workspace}` write
  cannot change a `{cwd}` template (fails without the read-only bind).
- **Flat, one file per template:** `.jad/sandbox/<name>.json`, referenced as `<name>` (the user's
  example; the same flat naming as agent folders, `sessionfs/native_subagent_dir.go:14-15`). The name
  is checked with `catalog.ValidateIdentifier` (`catalog/scan.go:17`); entries starting with `.` and
  files not ending in `.json` are ignored; a sub-folder is refused (the inverse of `catalog.Scan`'s
  category rule, `catalog/scan.go:63-67`, message at `:65`). Decision D15 records the category alternative.
- **Read by name, not scanned:** resolving `"workspace-writer"` reads `<dir>/workspace-writer.json`
  with `readLayoutFile` (`layout_files.go:173`), stamped first with `CatalogScans.noteFile`
  (`catalog_scans.go:74-83`), so editing a template reloads every session that uses it, exactly as an
  edited toolset does (`layout_load.go:493`). The folder is listed only for the catalog API and for the
  did-you-mean of a missing name.
- A session whose root has no catalog layout (`Location.UsesLayout`, `layout_root.go:121-130`) cannot
  name a template: refused with "sandbox templates need the catalog layout"; inline objects work as today.

### 14.2 File shape

The file is a sandbox object (every key `normalizeSandbox` accepts, `sessionfs/sandbox.go:97-230`,
plus `read`, `write`, `isolation` of §9.1) with the catalog file header:

```json
{
  "schema_version": 1,
  "description": "Writer in its own worktree: workspace only, no network, credentials hidden",
  "enabled": true,
  "backend": "bwrap",
  "tools": ["exec", "tool_smith", "hooks", "lsp"],
  "read":  {"paths": ["{system}", "{workspace}", "{home}/.mise", "{home}/.cache/go-build"]},
  "write": {"paths": ["{workspace}", "{git_common_dir}", "{tmp}"], "plan_paths": ["{tmp}"]},
  "hide_paths": ["{home}/.ssh", "{workspace}/.env"],
  "network": false,
  "isolation": {"mode": "worktree", "base": "working", "materialize": "auto", "share_paths": ["node_modules"]}
}
```

```json
{ "schema_version": 1, "description": "Reader: sees the workspace, writes nothing but scratch",
  "enabled": true, "read": {"paths": ["{system}", "{workspace}"]}, "write": {"paths": ["{tmp}"], "plan_paths": ["{tmp}"]} }
```

- **One template serves every user** because every path is a placeholder filled in per call
  (`tools/native/sandbox.go:141-160`): `{workspace}` is the session's folder for a profile and the
  run's own worktree for an isolated agent; `{tmp}` is per session or per run (§7.2 S3).
- `enabled` is **required** in a template file (refused when absent): a file someone names to confine
  a run must say whether it confines it (D19). Everything else keeps the loader's defaults.
- A template cannot name another template (`template` is refused inside a file): no chains (D17).
- **`tools` is not tied to the tool set of the run that uses it** (D25). It names the tools the
  environment wraps *when the run has them*; it is never checked against a profile's `tools.allow` /
  `tools.deny`, its toolset, or an agent's `tools` list. A listed tool the run does not have (not
  registered, hidden by the profile, absent from the agent's catalog) is not an error and has no
  effect, so one template serves a profile that shows only `read` and `search` and another that runs
  `exec`, `hooks` and `lsp`. This is today's behaviour and stays so: `applySandbox` hands the sandbox
  only to tool settings that exist (`sessionfs/sandbox.go:276-291`), the profile filter
  (`applyProfileTools`) runs after and never looks at the sandbox, and `jad check` reports nothing for
  such an entry. `tools` is a filter, not a check: **any tool name is accepted** (implemented
  2026-11-09), a name outside the wrappable set (`SandboxTools`, `sessionfs/sandbox.go:38`, which
  gains `lsp`) or not in the run's tool set has no effect, and a name listed twice counts once. Only
  the shape is refused: not an array, an empty array, or a blank name.
- `schema_version` and `description` are the only keys outside the sandbox object.
- Schema: new hand-written `sample/schemes/sandbox.schema.json`, whose properties are the
  `sandbox` definition of `setting.schema.json:627-678` (shared through `$ref`), plus the header and
  `"required": ["schema_version", "enabled"]`.

### 14.3 Where a template can be named

| Place | Accepted values of `sandbox` |
|---|---|
| root setting `defaults.sandbox` | `"<name>"`, `{"template": "<name>", …overrides}`, or an inline object (today) |
| session settings `sandbox` | same |
| profile file `settings.sandbox` (`layoutProfileFile.Settings`, `layout_files.go:83-98`) | same |
| sub-agent, inline `tools.native.subagent.agents.<n>` or `agent.json` (new agent key `sandbox`) | same; narrowed by the parent (§14.5) |
| toolset file | **not allowed** (unchanged: a toolset carries no sandbox; DECISIONS 2026-11-06) |

Examples:

```json
// .jad/profiles/coding/main.json
{ "schema_version": 1, "toolset": ["jad/default"], "settings": {"sandbox": "workspace-writer"} }

// .jad/subagents/fixer/agent.json
{ "schema_version": 1, "description": "Fix one failing test", "toolset": ["jad/default"], "sandbox": "workspace-writer" }

// .jad/subagents/explore/agent.json
{ "schema_version": 1, "description": "Investigate, never edit", "sandbox": "reader" }

// a profile that needs the network once
{ "settings": {"sandbox": {"template": "workspace-writer", "network_allow": ["proxy.golang.org"]}} }
```

The `explore` example closes the T-119 note that `explore` could not be protected without a stale
HEAD-only worktree: `reader` confines it to read-only access of the live workspace, with no worktree.
Scheduling can then follow the environment instead of a promise: `ChangesWorkspace`
(`native_subagent_worktree.go:63`) also becomes false for an agent whose resolved sandbox is enforcing
(bwrap or seatbelt, as `ReadOnlyEnforced`, `native_exec_settings.go:176-179`), whose `write.paths`
cover neither `{workspace}` nor `{git_common_dir}`, and whose catalog has no `profile_message` (which
reaches another session the template does not confine, D20). Such a child is then scheduled, handed
off and checkpointed like a reader — the environment-enforced replacement of the removed agent
`read_only`.

### 14.4 Resolution order

1. **Expand each layer before merging.** In `layoutCompose.compose` (`layout_load.go:318-324`), each
   of defaults, session and profile has its `sandbox` value expanded: a string becomes the template's
   object; `{"template": n, …}` becomes `mergeSettingValue(template, rest)` (objects per key, arrays
   replace, `layout_merge.go:19-31`); an inline object is unchanged. Expansion first is required:
   `mergeSettingValue` replaces a string with an object instead of merging it, so expanding after the
   merge would lose a template named on one layer and overridden on another. `add_*` keys are
   concatenated across the layers, not replaced (§14.9, D26).
2. **Merge the layers** as today (defaults → session → profile).
3. **Normalise** the result with `normalizeSandbox` (`session.go:410-412`): every refusal, the program
   check and `jad check`'s probe (`app/check.go:24`) work unchanged, per distinct resolved sandbox.
   `SandboxSettings.Template` keeps the name for messages and the tool description.
4. **Agents.** The agent loader (`native_subagent_settings.go` agent parsing, and agent folders
   `native_subagent_dir.go:106`) expands the agent's `sandbox` the same way, normalises it, then
   narrows it by the session's resolved sandbox (§14.5). The session's sandbox is known by then:
   `normalizeSandbox` runs at `session.go:410`, before the sub-agent settings. An agent without
   `sandbox` keeps its parent's (`app/subagent.go:235-249`). The agent's legacy `isolation` key sets
   `isolation.mode`; an agent with both is refused.
5. **The child's tools are built with the child's sandbox** by the machinery that already builds a
   separate tool set per agent toolset: `config.AgentToolsets` keyed by `ToolsetKey`
   (`layout_load.go:156-201`), built by `buildAgentToolsets` (`tools/native/catalog.go:268-293`) and
   `loadAgentToolsetMCP` (`app/resolver.go:909-925`, which today starts those MCP servers unwrapped,
   U4, and gets `SetCommandWrapper(mcpSandboxWrapper(derived))`). The key becomes `ToolsetKey` + the
   template's resolved identity, the derived config receives the narrowed sandbox (`applySandbox`), and an agent
   with a sandbox but no toolset gets a derived config of the session's own tools. So exec's
   description (`sandboxDescription`, `sandbox.go:217-246`), standbys (`sandboxKey`) and MCP of the
   child all follow the child's environment, not the parent's. At run time the same settings reach
   `ExecutionContext.Environment.Sandbox` (§9.4).
6. **Delegated profile runs** (`profile_message` targets) use the target profile's own resolved
   template; they are separate sessions and are not narrowed by the caller (D20).

### 14.5 Narrowing an agent's template by its parent

`sessionfs/sandbox_narrow.go` (new): `NarrowSandbox(parent, child *SandboxSettings) (*SandboxSettings, error)`,
a pure function in the spirit of `narrowScope` (`tools/native/path_scope.go:115-156`). A child is
never wider than the run that starts it; a template that would widen is **refused at load** with the
agent, the template and the path, rather than silently cut (D18).

| Field | Rule (parent `P`, child `C`) |
|---|---|
| no sandbox | `P` nil → `C` (a child may be more confined than its parent); `C` nil → `P` |
| `enabled` | `P.enabled ∨ C.enabled` |
| `backend` | must be equal when both are enabled; `command` only when both commands are identical |
| `tools` | union (a child covers at least what its parent covers); never compared with either run's tool set (D25) |
| `read.paths`, `write.paths`, `write.plan_paths` | every `C` entry must lie inside some `P` entry; entries compare by placeholder and remainder (`{workspace}/src` ⊂ `{workspace}`; `{system}` and absolute paths compare after expanding `{system}`); `["/"]` contains everything |
| `hide_paths` | union |
| `network` | `C.network` only if `P.network` |
| `network_allow` | `C` ⊆ `P` (each host or a subdomain of a `P` host); `C` none with `P` none → none |
| `isolation` | `C`'s; a child can always be more isolated. `C` `none` under an isolated `P` works inside `P`'s worktree (today's nesting via `WorkspaceRoot`, `subagent_worktree.go:69-90`) |

Placeholders compare symbolically because each run expands its own: `{workspace}` of a worktree child
is that worktree, which is the intent of "the child writes its own workspace".

### 14.6 Catalog API and editor

A fifth `CatalogKind`, `CatalogSandboxes = "sandbox"` (`sessionfs/catalog_files.go:21-26`), with flat
ids: the id-to-path rule (`catalog_files.go:123`, `strings.Cut(id, "/")`) gets a flat branch for this
kind. A save is validated by the template loader (§14.2); a delete is refused while a root default,
session, profile or agent names the template (`sandboxUsers`, in the style of `toolsetUsers`,
`catalog_toolsets.go:47`, and the `CatalogInUse` refusals, `catalog_files.go:279-313`). The editor tab
in `frontend_new/` is a separate, later frontend task.

### 14.7 Compatibility

Nothing existing changes meaning: an inline `sandbox` object stays valid everywhere it is today, and a
setting with no template behaves exactly as before. New: a string value, `template`, the agent key
`sandbox`, `catalogs.sandbox`. Fingerprints: `NativeSubagentAgent` gains `Sandbox *SandboxSettings`
with `json:",omitempty"` (as `Isolation`, `native_subagent_settings.go:232-240`), so an agent without a
sandbox keeps its fingerprint; an edited template changes the fingerprint of agents that use it, which
is the resume note's purpose.

### 14.8 Tests

Template found, missing (did-you-mean), malformed, without `enabled`, naming another template, in a
sub-folder; string and `{template, overrides}` forms on each layer, including a template on the
defaults overridden by a profile's object; edit of a template invalidates the settings stamp;
`NarrowSandbox` table (every row of §14.5, accept and refuse); an agent with a template builds its own
exec description and standby key; a profile and two agents sharing one template each get their own
`{workspace}` and `{tmp}`; catalog API save/delete/in-use for `sandbox`; schema agreement between
`sandbox.schema.json` and the template loader; a template whose `tools` lists `exec`, `tool_smith`,
`hooks`, `mcp` and `lsp` loads for a profile whose `tools.allow` is `["read", "search"]` and for an
agent whose catalog has none of them (D25); a name that wraps nothing (`read`, `search`) loads too.

### 14.9 Per-session and per-profile paths

A template is shared, but some paths belong to one session or one profile (a profile that also needs
`{home}/.m2`, a session that keeps data in its own folder). Two mechanisms cover them without copying
the template's lists (D26, D27).

**Placeholders (D27).** Every path is filled in per call (`sandboxExpander`,
`tools/native/sandbox.go:138-160`). Today: `{workspace}`, `{cwd}`, `{git_common_dir}`, `{tmp}`,
`{home}`; proposed: `{system}` (§9.1) and **`{session_dir}`**, the session's state folder
(`ExecutionContext.SessionDir`, set from `config.StateDir` at `app/bound_runner.go:161`). A sub-agent
uses its session's `{session_dir}`; per-run scratch is `{tmp}` (P1). When `{session_dir}` lies under a
writable path, the session's records (`messages*.jsonl`) and settings files stay read-only in the
sandbox, for the same reason as the template folder (§14.1). Narrowing (§14.5) compares
`{session_dir}` symbolically like `{workspace}`.

**Additive keys (D26).** `paths` keeps replacing (D16), and four keys add instead:

| Key | Adds to |
|---|---|
| `read.add_paths` | `read.paths` |
| `write.add_paths` | `write.paths` |
| `write.add_plan_paths` | `write.plan_paths` |
| `add_hide_paths` | `hide_paths` (whose default is the D5 credential list) |

```json
// .jad/profiles/java/main.json: the shared template plus one cache
{ "settings": { "sandbox": { "template": "workspace-writer",
    "read":  { "add_paths": ["{home}/.m2"] },
    "write": { "add_paths": ["{home}/.m2/repository"] } } } }

// a session that hides one more secret, keeping the template's hidden paths
{ "sandbox": { "template": "workspace-writer", "add_hide_paths": ["{workspace}/secrets"] } }
```

Rules:

1. **Merging.** Across the template and every layer (template → defaults → session → profile, and a
   `{template, …}` reference's own overrides), `add_*` lists are **concatenated**, not replaced, except
   that a layer setting a key (`read.paths`, `write.paths`, `write.plan_paths`, `hide_paths`) first
   clears the `add_*` list accumulated for that key below it (rule 3). This
   needs a sandbox-aware merge in `layoutCompose.compose` (`layout_load.go:318-324`) instead of the
   generic `mergeSettingValue`, which replaces arrays (`layout_merge.go:19-31`).
2. **Folding.** After the merge, `normalizeSandbox` folds each `add_*` list into its key
   (`paths ∪ add_paths`, a repeated entry counts once) and then applies "write implies read"
   (D16 correction). The resolved `SandboxSettings` has no `add_*` fields, so narrowing (§14.5), the
   exec description and the standby key see only the final lists.
3. **Replace still works.** A layer that sets `paths` replaces everything below it, including the
   lower layers' `add_paths`; its own `add_paths` and those of higher layers are added afterwards.
   `hide_paths` keeps replacing too, so a profile that needs `~/.ssh` can drop it (D5); `add_hide_paths`
   is how a layer adds a secret without removing the defaults.
4. **Agents.** An agent's `add_*` paths are checked against the parent like any other path (§14.5): an
   `add_paths` entry outside the parent's environment is refused with `widens:`.
5. **Where accepted.** Everywhere a sandbox object is (inline, template file, `{template, …}`); in a
   template file they just add to the file's own lists. Each is an array of non-blank strings; the
   same placeholders as `paths`.
6. **Schema.** `setting.schema.json`'s `sandbox` definition gains the four keys; the
   loader-agreement test (`sessionfs/sandbox_test.go`) covers them.

Tests: an `add_paths` on the profile adds to a template's `read.paths`; `add_*` from defaults, session
and profile all appear; a session `paths` drops the defaults' `add_paths` but keeps the profile's;
`add_hide_paths` keeps the D5 list; a write `add_paths` is readable; an agent `add_paths` outside the
parent is refused; `{session_dir}` expands per session and the session's records stay read-only;
`catalogs.sandbox` in each of its three forms, and `{cwd}` of an isolated child resolving to the
session's `cwd`.

---

## 15. Per-call sandbox choice in `subagent` and `profile_message`

The model can pick, per call, a sandbox template (§14) for the run it starts — the agent-level
counterpart of exec's per-call `read_only` (§R1): the model asks for a **narrower** environment, the
environment enforces it, and a choice can never widen what the run would otherwise get. This follows
the existing per-call `model` argument of both tools (`model_choice`) step for step.

### 15.1 The precedent this copies: `model_choice`

| Piece | `model` today | Where |
|---|---|---|
| switch (off by default) | `tools.native.subagent.model_choice`, `tools.native.profile_message.model_choice` | `sessionfs/native_subagent_settings.go:63-71`, `:319`; `sessionfs/native_profile_message_settings.go:81-84`, `:169` |
| schema property only when on | `subagentModelProperty`, `profileMessageModelProperty`, cut out by `modelChoiceSchema` | `tools/native/subagent.go:20-23`, `:288`; `profile_message.go:21-24`, `:280`; `subagent_model.go:33-45` |
| definition files of either form accepted | `requireModelChoiceDefinition` | `tools/native/catalog.go:570-583`, used at `:159`, `:181` |
| choices listed in the description | `subagentModelText` | `subagent_model.go:23-31`, `subagent.go:350` |
| per-call check, named error with the list | `withCallModel`, `modelUnavailable` (`model_unavailable`) | `subagent_model.go:45-74`, `subagent_call.go:79` |
| remote targets refuse it | — | `subagent_call.go:40-42`; `profile_message.go:351-355` |
| profile_message carries it to the target | `ProfileMessageRequest.Model` → plan → `httpapi.ProfileRunRequest.Model` → `Resolve(…, modelPath)` → `LoadSessionSettings(…, requestedModel)` | `tools/native/profile_message.go:146-148`, `app/profile_message.go:89-97`, `app/profile_message_queue.go:174-175`, `:207`, `httpapi/profile_run.go:15-25`, `httpapi/handler_ports.go:30`, `sessionfs/session.go:285` |
| config cache keyed by it | `lockedConfigKey{file, profile, modelPath}` | `app/locked_config.go:17-19` |

### 15.2 Settings

`tools.native.subagent.sandbox_choice` and `tools.native.profile_message.sandbox_choice`: an array of
template names the calls may pick, for example `["reader", "workspace-writer"]`. Absent or null: off,
and the tool is exactly what it is today. An empty array, a duplicate, or a name without a template file
is refused at load (did-you-mean as for missing templates, §14.1); a session without the catalog layout
refuses the key. A list rather than a switch (unlike `model_choice`, whose list is the profile's
models): the config owner decides which environments are offered, and adding a file to
`.jad/sandbox/` never offers it by itself (D21). Both keys are tool settings, so a toolset may carry
them; a toolset saved alone cannot resolve template names (its check session has no layout,
`catalog_toolsets.go:40`), so the names are checked when a session using the toolset loads, as for
other session-dependent checks (DECISIONS 2026-11-06). New fields: `NativeSubagentSettings.SandboxChoices
map[string]*SandboxSettings` and the same on `NativeProfileMessageSettings`, filled with the resolved
templates by the layout loader (as `ProfileModels`, `layout_load.go:104`). Keys added to
`nativeSubagentKeys` (`native_subagent_settings.go:51`) and `nativeProfileMessageKeys`
(`native_profile_message_settings.go:38`).

### 15.3 Tool definitions

Two schema lines, placed after `model` and present only when the list is set:

```go
// subagent.go, after subagentModelProperty
const subagentSandboxProperty = `
    "sandbox":{"type":"string","description":"Sandbox template for this run: one of the sandboxes listed below. Omit to use the agent's own sandbox, or the session's when the agent names none. A template can only narrow what the run may read, write and reach; one that would widen it is refused."},`

// profile_message.go, after profileMessageModelProperty
const profileMessageSandboxProperty = `
    "sandbox":{"type":"string","description":"Sandbox template the profile answers this message in: one of the sandboxes listed below. Omit to use the profile's own sandbox. It can only narrow that sandbox; one that would widen it is refused."},`
```

- No `enum`: the definition file must match the binary's schema exactly (`requireDefinition`,
  `tools/native/catalog.go:586-605`), so the names are listed in the description, like the models.
- `modelChoiceSchema` (`subagent_model.go:33-45`) becomes `optionalPropertiesSchema(schema, on
  map[string]bool)`, cutting every optional property that is off; `requireModelChoiceDefinition`
  (`catalog.go:570-583`) becomes `requireOptionalPropertiesDefinition`, accepting a file that holds the
  full schema, the schema without `sandbox`, or the schema without `sandbox` and `model` — so every
  definition file in use today still loads. `sample/tools/native/subagent.json` and
  `profile_message.json` get the full schema.
- With `sandbox_choice` off the schema the model sees is byte-identical to today, so `messages.jsonl`
  does not change (check with `messages-golden-diff`).
- Description: `subagentSandboxText` / `profileMessageSandboxText`, appended where `subagentModelText`
  is (`subagent.go:350`) and after `profileMessageListText` (`profile_message.go:294`):
  `" Sandboxes for sandbox: reader (Reader: sees the workspace, writes nothing but scratch); workspace-writer (…)."`
  — each template's `description` (§14.2), cut to one line.
- `normalizeSubagentArguments` (`subagent.go:784`) and `normalizeProfileMessageArguments`
  (`profile_message.go:332`) trim `sandbox` like `model`.

### 15.4 What a call with `sandbox` does

**subagent** (`tools/native/subagent_call.go`)

1. A remote agent with `sandbox` is refused (`sandbox_unavailable`: "A remote agent runs in its
   remote's own environment; leave sandbox out"), next to the model check at `:40-42`.
2. After `withCallModel` (`:79`), `withCallSandbox(agent, arguments, choices)`: an unknown name →
   `sandbox_unavailable` with `available:` (like `modelUnavailable`, `subagent_model.go:71-74`);
   otherwise `NarrowSandbox(agent's resolved sandbox, chosen)` (§14.5) — the agent's sandbox is already
   narrowed by the session's, so the result is never wider than the session, the agent or the choice.
   A widening choice → `sandbox_unavailable` naming the path or setting that would widen.
3. **Scheduling follows the choice:** `callMayChangeFiles` (`subagent.go:438-450`) applies the same
   choice before `ChangesWorkspace`, so `sandbox: "reader"` makes a writer agent's call a reader call
   (parallel, no checkpoint, may be handed off) by the rule of §14.3 — enforced, not promised.
   `isolation` from the chosen template applies too (a call may ask for a worktree).
4. **Child tools for the chosen environment** come from the per-agent machinery of §14.4 step 5, keyed
   by (agent toolset key, template identity) and built **lazily on the first call that picks it**, then
   cached for the life of the session config, so a choice nobody makes costs nothing (D23). The cache
   lives where `buildAgentToolsets` (`tools/native/catalog.go:268-293`) and `loadAgentToolsetMCP`
   (`app/resolver.go:909-925`) are driven; the MCP manager key gains the template identity, so a choice
   never reconnects the default servers (a changed wrapper key reconnects, `tools/mcp/manager.go:219-221`).
5. **Resume:** a resume without `sandbox` runs in the environment the source run recorded (its template
   name is stored in the run record next to the worktree record); with `sandbox`, in the new choice,
   still narrowed by the agent and session (D22).
6. The run record, the result and the async summary gain `sandbox: <name>`.

**profile_message** (`tools/native/profile_message.go`, `app/profile_message*.go`)

1. A remote profile with `sandbox` is refused, next to the model check (`profile_message.go:351-355`).
2. An unknown name → `sandbox_unavailable` with the list, in `execute` before `Prepare`.
3. `ProfileMessageRequest` (`profile_message.go:146-148`) gains `Sandbox`, carried through
   `profileMessagePlan` (`app/profile_message_queue.go:174-175`) and `httpapi.ProfileRunRequest`
   (`httpapi/profile_run.go:15-25`) to the target's resolution, exactly like `Model`.
4. The target's config is loaded with the choice: `Resolve`'s trailing `modelPath string`
   (`app/resolver.go:417`, interface `httpapi/handler_ports.go:30`, caller `httpapi/handler.go:795`)
   becomes `sessionfs.RunChoice{ModelPath string; Sandbox *SandboxSettings}`, passed down to `LoadSessionSettings*`
   (`sessionfs/session.go:285-291`, `settings_stamp.go:83-95`) and into `lockedConfigKey`
   (`app/locked_config.go:17-19`, keyed by the template's name and a hash of its resolved JSON) (D24).
   The template is resolved in the **caller's** layout — `sandbox_choice` is the caller's list — and
   travels as the resolved object, because a target in another session root may have a different
   `.jad/sandbox/` with the same name, unlike a model ID, which the target resolves among its own
   models. After `normalizeSandbox` (`session.go:410-412`) the target's
   sandbox is narrowed by the chosen template; a widening choice is a `ProfileSandboxError`, mapped to
   `sandbox_unavailable` like `ProfileModelError` (`app/profile_message.go:89-97`).
5. The chosen template narrows **the target profile's own** sandbox, so `sandbox` never widens the
   target. It is not narrowed by the caller's (D20 stands); a caller that wants its delegate confined
   names a template. The target's other runs (interactive or other messages) are unaffected: the choice
   is part of that run's config only.

### 15.5 Errors and texts

`sandbox_unavailable` (status invalid_input), with `available: <names>` for an unknown name and
`widens: <path or setting>` for a refused narrowing. The tool descriptions keep saying what each listed
template allows, and the child's or target's own exec/tool_smith descriptions describe the chosen
environment (`sandboxDescription`, `tools/native/sandbox.go:217-246`), so the model is never told one
environment and run in another.

### 15.6 Tests

Settings: off by default, list parsing, empty/duplicate/unknown refused, layout required, toolset
checked at session load; schema agreement for both keys (`setting.schema.json`). Definitions: each of
the three accepted definition-file forms loads; with the switch off the shown schema equals today's
byte for byte (golden). subagent: unknown name, remote agent, widening refused with `widens:`,
narrowing accepted and enforced (a write under `reader` fails with `Read-only file system` under bwrap),
`callMayChangeFiles` false for a writer called with `reader`, lazy child tools built once per
(agent, template) and reused, MCP key per template, resume keeps the recorded template. profile_message:
unknown name, remote, widening refused, the target run confined and the target's next plain run not,
`lockedConfigKey` separates choices. `-race` for the lazy cache.

---

## R1. The three `read_only` items

**(i) Sub-agent agent key `read_only`** — removed in T-119 and refused by name
(`sessionfs/native_subagent_settings.go:473-478`, `removed_keys.go:28-31`). Not re-proposed. Under this
design an agent that must not write the session's files either gets `isolation: "worktree"` with a real
environment around it, or names a template whose `write` set excludes the workspace (`reader`, §14.3),
which also lets it be scheduled as a reader by environment rather than by promise. Per call, the
parent can ask for that with `subagent`'s `sandbox` argument (§15).

**(ii) exec's per-call `read_only` argument** (`tools/native/exec.go:46`, `:93-128`)

| Behaviour that depends on it today | Where | What replaces it in this design |
|---|---|---|
| Read calls of one response run together under ordered scheduling | `exec.go:126-128` → `CallMayChangeFiles` → `ChangesState` (`tools/catalog.go:463-479`) → `toolBatchConcurrency` (`session/tool_batch.go:30-40`, `runner_tools.go:285`) | nothing cheaper: calls of one run share its workspace. Per-call overlays would need a merge per call. **Kept.** |
| Plan mode runs exec at all | `plan_mode.go:18-33`, guard `:72-76`, checkpoint skip `runner_tools.go:214` | kept; under a sandbox the call is confined by `write.plan_paths` |
| The call starts with the plan-mode writable set | `exec.go:116-121` → `callSandbox` → `ForPlanMode` (`sessionfs/sandbox.go:267-274`) | kept; in a holder it is the nested read-only bwrap (6 ms **[measured]**). This *is* an environment limit chosen per call — the rule, not an exception to it |
| Checkpoint decision | `runner_tools.go:214-217`, `checkpoint/run.go:13-24` | does **not** depend on `read_only` outside plan mode (exec counts by name); inside plan mode the guard skip decides. Nothing to replace. (An enforced `read_only` call could skip the checkpoint — a possible later optimisation, not proposed.) |
| Standby warm-up of the plan-mode form | `exec.go:521-528` | kept; under a holder the key includes the holder (§9.5) |
| Definition compatibility | `tools/native/catalog.go:537-551`, `exec.go:52-64` | kept unchanged; removing the argument would need a third older-schema entry and change the golden tool definition |

**Verdict: keep, with a reason** — it is the model asking for a narrower environment for one call,
which is exactly "limit the environment", and it is the only input that lets read calls of a
response run together. What must go is only its *unenforced* form, which is item (iii).

**(iii) `tools.native.exec.read_only_calls`** (`sessionfs/native_exec_settings.go:37-44`, `:173-186`,
`:347-358`; schema `setting.schema.json:1284`)

| Behaviour that depends on it | Where | Replacement |
|---|---|---|
| `trusted` (default): `read_only` counts without a sandbox — a promise | `ReadOnlyHonored`, `native_exec_settings.go:183-186` | none possible by environment: without a sandbox there is nothing to enforce |
| `sandboxed`: `read_only` counts only under bwrap/seatbelt | `ReadOnlyEnforced`, `:176-179` | becomes the only behaviour |
| exec's description text says a session may ignore `read_only` | `exec.go:46` | the text stays true under the only behaviour |

**Verdict: remove only after X**, where X = (a) P1 has landed — enforcement already exists per call
under bwrap and seatbelt (`ReadOnlyEnforced`), but until S3 is closed an enforced `read_only` call can
still write the shared `{tmp}` that other calls use, and until U1 is closed a plan-mode hook is not
confined — **and** (b) the user decides (D2/D3) what an unsandboxed host
does: with `sandboxed` as the only behaviour a `read_only` call there counts as a write — it runs in
order and plan mode refuses it, so plan mode cannot use exec without a sandbox. Then the key is refused
by name with a hint ("read_only counts only where a sandbox enforces it; enable sandbox"), following
`removed_keys.go`. Removing it earlier would either make the promise permanent (keep `trusted`) or
break plan mode on every unsandboxed host (keep `sandboxed`).

---

## R2. Where enforcement lives

### Chokepoints the design relies on

1. `sandboxed()` — `tools/native/sandbox.go:45-88`: every child process (bwrap, seatbelt, command,
   holder join).
2. `callSandbox()` — `tools/native/sandbox.go:34-39`: which environment one call gets (run
   environment, plan mode, `read_only`).
3. `startHolder()` — new `tools/native/sandbox_holder.go`: the mount set of an overlay run.
4. `resolveExistingTarget()` — `tools/native/path.go:23-102`: every in-process reader.
5. `worktree.run` — `worktree/worktree.go:233-256`: every host git command (hardened; content work
   moves into the environment via `Options.Wrap`).
6. `applySandbox` / settings — `sessionfs/sandbox.go:278-291`: which tools are covered.
7. `SandboxCommand` — `tools/native/sandbox.go:265-275`: MCP stdio and, newly, LSP.

### Per surface

| Surface | Today | Design | Gap and its fix or acceptance |
|---|---|---|---|
| exec | environment, when `sandbox.tools` has exec; cwd gated in-process | environment (per call or holder) | none while a sandbox is enabled; unsandboxed sessions are the user's choice (D2) |
| tool_smith gate (its child) | environment (`tool_smith.go:319`) | environment | bridge calls run in-process with their own gates (`tool_smith_bridge.go:46-80`); that they carry `Environment` and `PlanMode` is to be tested in P2 (unverified) |
| MCP stdio | environment only with `tools: ["mcp"]`, and only for the session's manager: an agent toolset's servers are never wrapped (U4); cwd = session folder (`manager.go:494`) | same, plus the agent toolset managers wrapped with the derived config's sandbox (§14.4 step 5) | an isolated run's MCP still acts on the session folder — accepted and documented (D10), or per-run servers in P6 |
| MCP HTTP | not a local process | uncovered | accepted: no local file access; remote side effects are outside a file environment |
| LSP | **uncovered** (`lsp_client.go:136-148`) | environment (`sandbox.tools: ["lsp"]`, holder join) | fixed in P2 |
| hooks | environment with `tools: ["hooks"]`, no plan-mode swap (`hooks.go:170`) | environment via `callSandbox` | fixed in P1 (U1) |
| checkpoint restore | uncovered host git (`checkpoint/service.go:782`) | uncovered | accepted: a user action, shadow git with its own `GIT_DIR` and fixed config (`checkpoint/git.go:85-99`), not influenced by repository hooks |
| checkpoint snapshot | uncovered host git, reads the workspace | uncovered | accepted, same reason; isolated runs take none |
| worktree git | **uncovered**, exploitable (S1, S2) | host part hardened and data-only; content part in the environment | fixed in P1/P3 |
| sub-agent worktree | in-process path rules only; "not a sandbox" (`agent.schema.json:96`) | environment of the run (§9.3), private git | without an enabled sandbox it stays "not a sandbox" — documented, D2 |
| read_image, code_context, lsp view, transcript | in-process gate | through the holder's root, or the gate mirroring the environment (§2.4) | none |
| `hooks.shell_changes` scan | host stat of the workspace | through the holder's root | fixed in P4 |

---

## R3. Portability

| Host | Sandbox | Isolation materialiser | Overlay |
|---|---|---|---|
| Linux, bwrap ≥ 0.10, kernel ≥ 5.11, user namespaces allowed | bwrap | overlay (auto) | yes, inside the holder |
| Linux, older bwrap or kernel, or overlay probe fails | bwrap per call | checkout | no |
| Linux, no bwrap or user namespaces forbidden | none can be enabled (refused at load if enabled, `sessionfs/sandbox.go:218-227`; `jad check` fails, `sandbox_probe.go:19-56`) | checkout, not a sandbox | no |
| macOS | seatbelt per call (`sandbox-exec`) | checkout (APFS clone later, P6) | no — OverlayFS is Linux-only; the macOS equivalent for cheap copies is APFS `clonefile` **[external]**, not a union mount |
| Other (Windows; whether the Go backend runs there was not checked) | `command` backend only | checkout | no |

**Fail-closed where asked, documented fallback where not:** an enabled sandbox whose program is missing
stays a load refusal (today's rule, kept). `materialize: "overlay"` set explicitly and not available
fails the run with the probe's reason. `materialize: "auto"` falls back to checkout — this changes
cost, not confinement, so it is a documented fallback, not a security fallback. An unsandboxed run is
allowed only when no sandbox is enabled, which is the user's explicit configuration.

---

## R4. Migration and compatibility

| Existing key | New design | Status |
|---|---|---|
| `sandbox.enabled`, `tools`, `backend`, `executable`, `network`, `network_allow`, `proxy_port`, `command`, `check_timeout_ms` | unchanged; `tools` gains `lsp`; `tools` stays independent of the run's tool set (D25) | kept |
| `sandbox.writable_paths` | `sandbox.write.paths` | old spelling accepted; both set → refused (D11) |
| `sandbox.plan_writable_paths` | `sandbox.write.plan_paths` | same |
| `sandbox.hide_paths` | unchanged, highest precedence | kept |
| — | `sandbox.read.paths` (default `["/"]`) | new |
| `{workspace}`, `{cwd}`, `{tmp}`, `{home}`, `{git_common_dir}` | same names; `{tmp}` becomes per session/run; `{git_common_dir}` narrows in an isolated run's write set; protected git paths always read-only | kept, narrowed (behaviour change only under an enabled sandbox) |
| exec/tool_smith `paths` | cwd gate, unchanged | kept (D9) |
| agent `isolation` | unchanged values; now also confined when a sandbox is on | kept |
| `tools.native.subagent.worktree` | unchanged; also used by profile runs | kept |
| — | `sandbox.isolation` (inline or in a template) | new |
| `sandbox` object at defaults/session/profile | may also be a template name, or an object with `template` and overrides (§14) | kept, extended |
| — | agent key `sandbox` (template name or object), narrowed by the parent's | new |
| — | root `catalogs.sandbox`, default `.jad/sandbox` | new |
| — | `tools.native.subagent.sandbox_choice`, `tools.native.profile_message.sandbox_choice` (template lists, off by default) and the `sandbox` call argument of both tools | new; tool schemas unchanged while off (§15.3) |
| `tools.native.exec.read_only_calls` | removed in P5, refused by name | deprecated now, refused after D3 |
| exec `read_only` argument | unchanged | kept |
| agent `read_only`, agent `paths` | already refused by name (`native_subagent_settings.go:473-478`) | unchanged |

**Without overlayfs or user namespaces:** `auto` gives the checkout path; nothing to migrate.
**Default when nothing is configured:** no sandbox and no isolation — today's behaviour, kept until
D2. With a sandbox enabled and no `read`/`write`: `read ["/"]`, `write` = today's default, plus the fixed
rules of §9.3 steps 5–6, which close S1–S4. That is the design's safe default: it is today's
environment minus the four escapes, with nothing new to configure.

---

## R5. Cost model

See §8 (numbers measured on this repository, estimates labelled).

---

## R6. Decisions requiring approval

Decided: see `docs/isolation-decisions.md` (D1–D24, with corrections).

| # | Chosen | Alternative | Cost of reversing |
|---|---|---|---|
| D1 | Overlay + holder on Linux where probed; checkout elsewhere | checkout only (simplest), or fuse-overlayfs mounted on the host (host-visible, extra daemon, FUSE overhead) | low: the `Tree` API is the same; drop one materialiser |
| D2 | Sandbox default stays `enabled: false` | on by default where a backend probes OK | medium: flipping later changes every user's environment |
| D3 | Remove `read_only_calls` in P5; `read_only` counts only where enforced; plan mode without a sandbox cannot use exec | keep the key (`trusted` default), or keep only `trusted` | low: re-adding an enum key |
| D4 | `read.paths` deny-by-default key with default `["/"]` | allow-by-default with a deny list (`hide_paths` only) | medium: a default change is a behaviour change |
| D5 | When a sandbox is enabled, hide by default `{home}/.ssh`, `.gnupg`, `.aws`, `.config/gh`, `.git-credentials`, `.netrc`, `.docker/config.json` | no default hidden paths (today) | low |
| D6 | `isolation.share_paths` default empty | share common ignored dependency folders automatically | low |
| D7 | Profile isolation applies to delegated runs (sub-agents, `profile_message` targets); the interactive session is not isolated | a session-lifetime worktree for the interactive session, with an explicit integrate action | low: additive later |
| D8 | No new tool argument for a run's base branch; dependencies via resume or merge | a `from` argument on the sub-agent call | low: additive |
| D9 | exec/tool_smith `paths` kept as the cwd gate | refuse `paths` where a sandbox covers the tool | low |
| D10 | stdio MCP of an isolated run = the session's servers (documented) | per-run servers inside the run's environment (reconnect per run) | medium |
| D11 | Old spellings `writable_paths`/`plan_writable_paths` accepted; both forms set is refused | rename with a removed-key refusal | low |
| D12 | `isolation.base` default `head` (today's behaviour) | default `working` (sees uncommitted files) | medium: behaviour change for existing isolated agents |
| D13 | Agents' git works on a detached HEAD; branches and tags fail by environment | writable per-run ref directory (not expressible for a ref file that does not exist yet without exposing siblings) | medium |
| D14 | `nsenter` (util-linux) to join the holder | a jad join helper with a C setns preamble (Go cannot `setns` a user namespace from a multi-threaded process **[external]**) | medium |
| D15 | Templates are flat files `.jad/sandbox/<name>.json`, named `<name>` (your example, like agent folders) | `.jad/sandbox/<category>/<name>.json` named `<category>/<name>`, reusing `catalog.Scan` and the file API unchanged | low before release: rename files and references |
| D16 | A reference may override a template per key (`{"template": n, …}`), merged like settings layers | name only, no overrides | low |
| D17 | No template chaining (`template` refused inside a template) | `extends` | low: additive |
| D18 | An agent template that widens its parent is refused at load | cut silently to the intersection | low |
| D19 | `enabled` required in a template file | default `false` as inline | low |
| D20 | `profile_message` targets use their own template, not narrowed by the caller | narrow them like sub-agents (a sandboxed profile could otherwise delegate to an unsandboxed one) | medium |
| D21 | `sandbox_choice` is an explicit list of template names | a switch offering every file in `.jad/sandbox/` (like `model_choice` offers every profile model) | low |
| D22 | A resume without `sandbox` keeps the source run's recorded template | resume in the agent's default | low |
| D23 | Child tools for a chosen template are built lazily on first use and cached per (agent, template) | build all choices at catalog time (MCP servers started for choices never made) | low |
| D24 | `Resolve`/`LoadSessionSettings` take `sessionfs.RunChoice{ModelPath, Sandbox}` instead of a second trailing string | add a positional `sandbox string` (smaller diff, easy to swap with `modelPath`) | low |
| D26 | Additive `read.add_paths`, `write.add_paths`, `write.add_plan_paths`, `add_hide_paths`, concatenated across layers and folded after the merge (§14.9) | arrays only replace: a layer repeats the template's whole list | low: additive |
| D27 | `{session_dir}` placeholder (session state folder), its records read-only in a sandbox (§14.9) | none; per-run scratch through `{tmp}` only | low: additive |
| D28 | `catalogs.sandbox` is absolute, relative to the root setting's directory, or `{cwd}/…` resolved per session; the folder is read-only in every sandbox (§14.1) | root-relative only, like the other catalogs | low |
| D25 | `sandbox.tools` is a filter independent of the run's tool set: any tool name is accepted, a listed tool the profile or agent does not have (or that starts no child) has no effect (decided by the user; implemented) | refuse or warn when `tools` names a tool the run cannot use | low |

---

## Unrelated existing issues (kept out of the design)

- **U1** Hooks take `hooks.Sandbox` and not `callSandbox` (`tools/native/hooks.go:170`), so in a plan-mode
  run a sandboxed hook keeps the normal writable paths. (Fixed in P1 because the design touches the
  line; listed here because it is a bug today.)
- **U2** `normalizeNativePathPolicy` gives the array and object forms different defaults
  (`sessionfs/file_settings.go:169`).
- **U3** `jad check` is the only sandbox probe; nothing re-probes during a run
  (`tools/native/sandbox_probe.go:15-18`), so a host change after start shows only as failing calls.
- **U4** The stdio MCP servers of an agent's toolsets are started without the sandbox wrapper even when
  `sandbox.tools` lists `mcp`: `SetCommandWrapper` is called only for the session's manager
  (`app/resolver.go:655`), not in `loadAgentToolsetMCP` (`app/resolver.go:909-925`). Fixed by §14.4
  step 5 (P2).

---

## Appendix A. Spike log (this machine, `/tmp`, removed afterwards)

Repository: a `git clone` of this repository in `/tmp/t120/repo`; base = `git worktree add --detach`.

| Check | Command (abridged) | Result |
|---|---|---|
| checkout cost | `git worktree add -q --detach wt1 HEAD` | 0.07 s, 19 MB |
| no-checkout | `git worktree add --no-checkout --detach wt2 HEAD`; `git status` | 0.00 s; 1,771 entries (empty index) |
| full copy | `cp -a <repo> full` | 2.18 s, 900 MB |
| overlay in bwrap | `bwrap … --overlay-src wt1 --overlay upper work merged …` | starts in 0.01 s; edits land in `upper` |
| copy-up | append one line to `CLAUDE.md` | upper holds the whole 13,960-byte file |
| delete | `rm CLAUDE.md` | `c--------- 0, 0` whiteout in upper |
| dir rename | `mv audit_findings audit_moved` | whiteout + 3.0 MB copied |
| shared upper | second bwrap on the same upper while the first runs | mounted without error (undefined per kernel docs) |
| git in overlay, shared dir ro | `git mv` | `index.lock: Read-only file system` |
| index copy | copy base index into the run's admin dir; `git status` | 0 entries, ~0.00 s |
| private objects | `GIT_OBJECT_DIRECTORY=<run>/objects GIT_ALTERNATE_OBJECT_DIRECTORIES=<common>/objects git commit` | commit made; objects 20 KB; `packed-refs.lock` warning (C9) |
| bundle transfer | `git bundle create … HEAD ^base`; `git fetch <bundle>` | refused: "prerequisite commits … not connected" |
| pack transfer | `git pack-objects --revs` → `git index-pack --stdin --fix-thin`; `git update-ref` | 7,352-byte pack; branch shows the change |
| holder join | `bwrap --info-fd 3 … -- sleep`; `nsenter -t <pid> -U -m -p -n -i -u --preserve-credentials` | sees the overlay, writes land in upper, `CapEff 0`, `mount` refused; 1 ms |
| `--wd` | `nsenter … --wd=<merged>` | did not see the overlay (resolved outside) |
| nested read-only | `nsenter … -- bwrap --ro-bind / / …` | `Read-only file system`; 6 ms |
| host view | `cat /proc/<pid>/root/<merged>/file`; write via the same path | readable and writable; `readlink /proc/<pid>/root` = `/` |
| S1 | `.git/hooks/post-checkout`; `git worktree add` | hook ran |
| S2 | `core.fsmonitor` in `.git/config`; `git status` in a worktree | fsmonitor command ran |
| hooksPath | `git -c core.hooksPath=/dev/null worktree add` | no hook |
