# Decisions on `docs/isolation-sandbox-design.md` §R6

Records the decisions on every item of the design's "Decisions requiring approval" table. The design
document remains the normative source for the mechanisms; this file records what was chosen, why, in
which phase, and what still needs the user.

## D1–D14

| # | Verdict | Decision | Reason | Phase |
|---|---|---|---|---|
| D1 | accept | Overlay + per-run bwrap holder on Linux where probed; `git worktree add` checkout elsewhere. fuse-overlayfs rejected. | Both paths enforce the same rules and differ only in cost; fuse-overlayfs would be host-visible and needs a daemon. P0 must confirm `--overlay` and overlay sources on the host with destinations on tmpfs, else `auto` falls back to checkout. | checkout P3, overlay P4 |
| D2 | accept with correction | Normal calls stay `enabled: false`; a `read_only` or plan-mode exec call runs in the plan-mode environment wherever a backend probes OK, even when the sandbox is off. | Turning the sandbox on for writer calls would change every install's environment; narrowing only `read_only`/plan-mode calls narrows only. | P5 |
| D3 | defer with a condition **[USER]** | Remove `read_only_calls` together with the D2 correction; `read_only` then counts only where a sandbox enforces it. | Removal alone would make every `read_only` call a writer on a default install and plan mode would refuse all exec. Lands after P1 closes S3. | P5 |
| D4 | accept | `sandbox.read.paths` is deny-by-default as a key with default `["/"]`; a write path is always readable. `hide_paths` and the fixed `.git/hooks`/`.git/config` protections still beat `write`. | This is the user's `read → paths`; the `["/"]` default keeps today's behaviour and write-wins is applied where the environment is built. | P2 |
| D5 | accept with correction | The credential list is the **default value** of `hide_paths` (so it is overridable) and is longer: `{home}/.ssh`, `.gnupg`, `.aws`, `.kube`, `.config/gcloud`, `.azure`, `.config/gh`, `.git-credentials`, `.netrc`, `.npmrc`, `.pypirc`, `.cargo/credentials`, `.docker/config.json`, `.jad/auth` (jad's own credential store, `{home}/.jad/auth`: the `openaiauth`/`claudeauth` default files `openai-codex.json` and `claude-oauth.json`). | Hiding narrows the environment and changes no tool; as a default a profile that needs ssh/gh can replace it. | P2 |
| D6 | accept | `isolation.share_paths` defaults to empty; guessing dependency folders is speculative. Document that an `npm install`-style write fails in a run. | Prefer backend defaults over speculative abstraction. | P4 |
| D7 | accept as first scope **[USER]** | Sandboxing confines every profile run including the interactive session; only the private worktree is delegated-only (subagents, `profile_message` targets). | The confinement half of the request is met for every profile run; a session-lifetime worktree needs an integrate step and is additive later. | P2 sandbox, P3 delegated isolation |
| D8 | accept | No new tool argument for a run's base branch; dependencies via resume or merge. | Resume and merge already cover dependent runs. | P3 |
| D9 | accept | exec/tool_smith `paths` stays the working-directory gate. | An existing key unsandboxed sessions still need; refusing it under a sandbox would add a tool-level limit. | — |
| D10 | accept with correction | An isolated run uses the session's stdio MCP servers (documented), and such a run **counts as changing the workspace** so it is never scheduled or checkpointed as a reader. Per-run servers in P6. | Without the scheduling rule an isolated run can write the session folder through an MCP server. | doc P3, fix P6 |
| D11 | accept with sunset | Old spellings `writable_paths`/`plan_writable_paths` accepted now; both forms set is refused; refused by name in P5. | Existing configs keep loading and the old names disappear on a schedule. | P2, refusal P5 |
| D12 | accept | `isolation.base` defaults to `head`; `working` is opt-in. | Keeps current behaviour for existing isolated agents. | P3 |
| D13 | accept | Agents' git works on a detached HEAD; branches and tags fail by environment, and the tool description says so. | Refused by a read-only directory, not by a tool. | P3 |
| D14 | accept | Depend on `nsenter` to join the holder; `jad check` reports it missing and `auto` falls back to checkout. | Go cannot join a user namespace itself; a C helper is more to maintain. | P0 check, P4 |

## D15–D28

| # | Verdict | Decision | Reason | Phase | [USER] |
|---|---|---|---|---|---|
| D15 | accept | Templates are flat files `.jad/sandbox/<name>.json` referenced as `<name>`; a sub-folder is refused. | Follows the user's example and agent-folder naming; cheap to reverse before release. | P2 | |
| D16 | accept with correction | A reference may override a template per key with `{"template": n, …}`, merged like settings layers (objects per key, arrays replace) before normalising; "write implies read" is applied once, after the merge. | A profile can adjust one environment without copying it; applying the rule after the merge means an override can never write a path it cannot read. | P2 | |
| D17 | accept | No template chaining: `template` inside a template is refused. | `extends` costs little later; chains hide what a run can touch. | P2 | |
| D18 | accept with correction | An agent template that widens its parent is refused at load, naming agent, template and path; read containment is checked against the parent's read ∪ write, write against the parent's write. | Silently cutting it would run the agent in an environment nobody wrote. | P2 | |
| D19 | accept | A template file must state `enabled` or is refused. | A file named to confine runs must say whether it confines them. | P2 | |
| D20 | **reverse the design** (user: yes, 2026-11-09) | A `profile_message` target is narrowed by the caller's resolved sandbox exactly like a sub-agent: a target with no sandbox inherits the caller's; a target whose own template is wider is refused (`widens:`); a per-call `sandbox` narrows further. | Otherwise a confined profile can delegate to an unconfined one — the same kind of escape as the writable `.git/hooks`, against "limit the environment". | P2 | [USER] |
| D21 | accept | `sandbox_choice` is an explicit list of template names. | The config owner decides which environments are offered; dropping a file in `.jad/sandbox/` never offers it. | P2b | |
| D22 | accept with correction | A resume without `sandbox` keeps the source run's recorded template, re-resolved by name and narrowed again by the current agent and session; a missing or now-widening template refuses the resume, as does a choice that changes `isolation.mode`. | Falling back to the agent's default could silently widen a run started narrower. | P2b | |
| D23 | accept with correction | Child tools and MCP servers for a chosen template are built lazily and cached per (agent, template); `NarrowSandbox` for every (agent, choice) pair is computed at load. | Choices nobody makes start no MCP servers, while a broken environment is still caught at load. | P2b | |
| D24 | accept with correction | `Resolve`/`LoadSessionSettings` take `sessionfs.RunChoice{ModelPath, Sandbox, Ceiling}` where `Ceiling` is the caller's resolved sandbox; `lockedConfigKey` hashes both. | The sandbox travels as a resolved object, not a string, and two adjacent strings could be swapped silently. | P2 struct + Ceiling, P2b Sandbox | |
| D25 | accept (user) | `sandbox.tools` is independent of the run's tool set: a listed tool the profile (`tools.allow`/`deny`), toolset or agent catalog does not have is accepted and has no effect; any tool name is accepted (one that starts no child does nothing), a duplicate counts once; only a non-array, empty array or blank name is refused. | One template must serve profiles and sub-agents with different tool sets; the user ruled the list is a filter, not a check. | implemented 2026-11-09 (loader + schema) | user |
| D26 | accept (user) | Additive `read.add_paths`, `write.add_paths`, `write.add_plan_paths` and `add_hide_paths`: concatenated across template and layers, folded into their keys by `normalizeSandbox` after the merge, before "write implies read". `paths` and `hide_paths` still replace (D16, D5). | A profile or session adds one path without copying the template's list; `add_hide_paths` adds a secret without dropping the D5 defaults. | P2 | user |
| D27 | accept (user) | `{session_dir}` placeholder = the session's state folder (`ExecutionContext.SessionDir`); the session's records and settings stay read-only in a sandbox even under a writable path. | Per-session paths without per-session lists; a run must not rewrite its own transcript or settings. | P2 | user |
| D28 | accept (user) | `catalogs.sandbox` is an absolute path, a path relative to the root setting's directory, or `{cwd}/…` resolved per session against the session's `cwd` (never a sub-agent's worktree); the resolved folder is bound read-only in every sandbox. | Project-local templates; a sandboxed run must not widen its own next environment by editing a template. | P2 | user |

## Corrections to the design

1. **D2/D3 coupling.** §12 P5 as written either keeps the promise or breaks plan mode on a default
   install. Enforce `read_only`/plan-mode calls wherever a backend probes OK, then remove
   `read_only_calls`.
2. **D5** is the default value of `hide_paths`, not a fixed rule, and the list is longer (see D5).
3. **D10** needs the scheduling rule: an isolated run using stdio MCP counts as changing the
   workspace.
4. **D20** is reversed: targets are narrowed by the caller (see D20).
5. **§14.4 step 3:** apply "write implies read" in `normalizeSandbox` after the layers are merged,
   never inside a single layer's `{template, …}` expansion.
6. **§14.5 table:** split the combined `read.paths`, `write.paths`, `write.plan_paths` row into a
   read row and a write row: every child `read.paths` entry must lie inside the parent's
   read ∪ write; child `write.paths`/`write.plan_paths` entries inside the parent's write.
7. **§14.4 step 6:** replace "they are separate sessions and are not narrowed by the caller (D20)"
   with the D20 rule: the target's resolved sandbox is narrowed by the caller's (§14.5, compared
   as in correction 8); a target with no sandbox inherits the caller's, and a wider target template
   is refused with `widens:`.
8. **§14.5 (new note):** for a target in another session root compare expanded absolute paths, not
   placeholders; `{tmp}` stays per-run scratch and is compared by placeholder.
9. **§14.3, last paragraph:** keep "a catalog with `profile_message` is never a reader" even after
   D20, but replace its reason "(which reaches another session the template does not confine, D20)":
   after D20 the target is narrowed by the caller, and the rule holds because the target still runs
   its own session's stdio MCP servers (D10).
10. **§15.4 profile_message step 5:** replace the whole sentence "It is not narrowed by the
    caller's (D20 stands); a caller that wants its delegate confined names a template." with: the
    target's resolved sandbox is first narrowed by the caller's (`RunChoice.Ceiling`, D20/D24) — a
    target with no sandbox inherits the caller's, a wider target template is refused with
    `widens:` — and a per-call `sandbox` then narrows it further.
11. **§15.4 subagent step 5:** a resume re-resolves the recorded template by name and narrows it
    again by the current agent and session; no fallback to the default; a missing or now-widening
    template refuses the resume, as does a choice that changes `isolation.mode` (D22).
12. **§15.4 subagent step 4:** narrowing for every (agent, choice) pair is computed at load; only
    tools and MCP are lazy; the cache is dropped with the settings stamp (D23).
13. **§15.4 profile_message step 4 and §12 (P2/P2b):** add `Ceiling` to `RunChoice` and to the
    `lockedConfigKey` hash in P2; P2b adds `Sandbox` (D24, D20).

## Needs the user

1. **D3:** "On a host with no working sandbox (no bwrap/seatbelt), is it acceptable that plan mode
   cannot run exec at all once `read_only_calls` is removed?" Everywhere else plan mode stays usable
   through the D2 correction.
2. **D7:** "Profiles' sandboxes will confine the interactive session, but it keeps editing your
   checkout in place. Do you also want interactive sessions to work in a private worktree with an
   explicit integrate step?"
3. **D20 — answered 2026-11-09: yes, narrow by the caller.** A profile reached through
   `profile_message` from a sandboxed caller is confined by the caller's sandbox; a target with no
   sandbox inherits the caller's, and a target whose own template is wider is refused with `widens:`.
   Corrections 7, 8, 10 and 13 and D24's `Ceiling` are therefore part of P2, no longer waiting.

## Implementable now without user input

D1, D2 (normal calls stay off), D4, D5, D6, D7 (delegated scope), D8, D9, D10 (with the scheduling
rule), D11 (with the sunset), D12, D13, D14, D15, D16, D17, D18, D19, D21, D22, D23, D26, D27, D28, D24 (`RunChoice`
and `Sandbox`; `Ceiling` waits on D20).

P0, P1, P3 and P4 can start now in the order of §12, and so can all of P2 (D20 is answered, so the
narrowing of `profile_message` targets by the caller and D24's `Ceiling` are in it). D3's removal of
`read_only_calls` (P5) waits for the D3 answer.

## Phase risk

The S5 fix relies on bwrap `--bind-fd`, which is unverified. This record places the S5 fix in P1,
following §12 (P1 "Depends on: P0 for S5 only"), because S5 already exists with today's
`writable_paths`. If P0 finds the installed bwrap lacks `--bind-fd`, P1 must refuse write paths that
lie under a writable parent instead of binding them, and P2 keeps that refusal for `write.paths`.

## P0 results (2026-11-09, the cloud container this phase was built in)

Measured with `bwrap` and the commands of P1's tests; nothing here was measured on the design's own
machine (§1.8), on macOS or on Ubuntu with AppArmor.

| Check | Result |
|---|---|
| bwrap | 0.9.0 (Ubuntu package `bubblewrap 0.9.0-1ubuntu0.3`), unprivileged user namespaces work, kernel 6.18 |
| `--overlay-src`/`--overlay` | **absent** (needs bwrap ≥ 0.10): the overlay materialiser (P4) cannot be tested here; `auto` must fall back to checkout on this host |
| `--bind-fd` | present, and the descriptor is **not** inherited by the child (`/proc/self/fd` of the child has no fd 7) |
| `nsenter` | present (`/usr/bin/nsenter`) |
| git | 2.43.0, so `merge-tree --write-tree` (≥ 2.38) is available for P3 |
| A read-only bind of `.git/hooks` alone | does **not** stop the run renaming `.git` itself away (rename of a folder that holds a mount point succeeds) — so S1 needs `.git` pinned by its own bind; pinned, rename, unlink and writes fail with `EBUSY`/`EROFS` |
| `--tmpfs X`, then a bind inside X, then `--remount-ro X` | works: X is empty and read-only, the bind inside keeps its own mode |
| A named pipe on a read-only bind | **is still writable** (`echo > fifo` succeeds): S3 cannot be closed by read-only binds, the other sessions' temp folders must be hidden |
| AppArmor user-namespace restriction, seatbelt `(deny file-read*)`, `{system}` on macOS | not testable here (no AppArmor profile in the container, no macOS) |

## P1 implementation (2026-11-09)

What P1 changed, and where it departs from the design text:

1. **S1/S2 host git** (`worktree/worktree.go`): every git command of `worktree` runs with
   `-c core.hooksPath=/dev/null -c core.fsmonitor=false`. With `commit_hooks` on, the commit Finish
   makes keeps the repository's hooks (it is what the setting asks for) and only drops the fsmonitor;
   the design moves those hooks into the run's environment in P3. Checkpoint git is unchanged
   (accepted in R2: own `GIT_DIR`, fixed config).
2. **S1/S2 sandbox** (`tools/native/sandbox_rules.go`): when a writable path covers the git common
   directory it is pinned (`--bind` onto itself; seatbelt: write of the folder's own name denied),
   `hooks` is read-only (a missing one becomes an empty read-only folder, which bwrap creates as an
   empty folder on the host), and `config`, `config.worktree` and the linked worktree's own
   `config.worktree` are read-only when they exist. Not covered: a `core.hooksPath` that the
   repository's config already points into the workspace (for example husky's `.githooks`) stays
   writable under `{workspace}`; jad's own host git ignores it (item 1), the user's git does not.
3. **S3** — `{tmp}` is the call's own folder `<temp>/jad-sandbox-<uid>/<16 hex>` (0700, owner
   checked), keyed by session key, workspace root **and plan-mode form**: the design keys it per
   session and run only; the form is added because a `read_only`/plan-mode call sharing a folder with
   writer calls could feed a waiting writer standby's code pipe (P0: pipes stay writable on read-only
   binds), which R1 requires closed before `read_only_calls` goes. The whole `jad-sandbox-<uid>`
   folder is hidden in every bwrap/seatbelt sandbox except the call's own folder; the child's
   `TMPDIR` is set to it (bwrap `--setenv`, seatbelt through `/usr/bin/env`); exec and tool_smith
   scratch dirs (and standbys) are made in it. The uid is in the name so users of one machine never
   share the root. A process outside a call (stdio MCP server, `jad check`) keeps the system temp
   folder as `{tmp}`. Folders untouched for `staleScratchAge` are swept at startup with the other
   scratch.
4. **S4** — `<common>/worktrees` and `<common>/jad-worktrees` are hidden (empty, read-only) in every
   sandbox; the call's own admin folder and own run folder are shown again, writable when they were.
   A configured `tools.native.subagent.worktree.dir` outside the common directory is not hidden.
5. **S5** — **not** `--bind-fd`: a writable path is left out of the binds when resolving it, name by
   name, looks a name up in a folder another writable path covers (so a sandboxed call could have
   renamed or replaced it). The covering bind already makes whatever is really there writable, so
   nothing legitimate is lost, and a symlink planted in its place no longer redirects a bind. Kept
   paths are bound by their resolved name. The rule closes the swap from inside one session's
   sandboxes without passing descriptors through every launch path (MCP's `SandboxCommand` has no way
   to pass files). Left open: a swap by a *different* session whose writable set covers this
   session's writable path's parent, during the microseconds between resolving and bwrap's own open;
   `--bind-fd` (present here, P0) would close it and stays the follow-up. Changed behaviour: a
   writable path that is a symlink out of another writable path (e.g. `{workspace}/cache` →
   `~/.cache`) is no longer made writable; list its target instead.
6. **U1** — hooks fired for a tool call (`pre_tool`/`post_tool`, `post_edit`) start in
   `callSandbox` of that call, so plan mode and `read_only` reach them, with the call's `{tmp}`. Run
   events (`run_start`, `stop`, …) carry no call and still use the configured sandbox.
7. **Recover** — a run that was queued or running when the server stopped, and has a worktree
   record without an outcome, is finished at the next start (`worktree.Open` + `Finish`: committed to
   its branch, folder removed); a folder that is gone is recorded as a worktree error. It uses the
   `tools.native.subagent.worktree` settings of the session that builds the manager, the only ones
   known at startup (as `keep_finished`).
8. **Not covered in P1 (P2):** the in-process `write`/`edit` tools are not sandboxed and no default
   path rule refuses `.git` (the only `.git` default is the file listing's ignore list,
   `sessionfs/file_settings.go:53`), so a model can still write `.git/hooks/*` through them; P2's
   in-process mirror of the environment (§2.4, §9.3) closes it. **Corrected in P2:** this was
   wrong. `write` and `edit` were removed before P1 (`sessionfs/tool_shape.go`, `removedNativeTools`:
   every write goes through `exec`), so no in-process tool writes the project. The in-process
   tools left (code_context, read_image, the lsp tool's file view, transcript) only read; P2
   mirrors the environment for them (item 9 of the P2 record).

## P2 implementation (2026-11-09)

What P2 changed, and where it departs from the design text. Verified on this container's
bwrap 0.9.0; nothing was run on macOS.

1. **Settings** (`sessionfs/sandbox.go`): `read.paths` / `read.add_paths` (default `["/"]`),
   `write.paths` / `write.add_paths` / `write.plan_paths` / `write.add_plan_paths` (the older
   `writable_paths` / `plan_writable_paths` still load; one object with both spellings of one
   list is refused, D11), `add_hide_paths`, `isolation.mode`, `template`; `lsp` in `SandboxTools`;
   placeholders `{session_dir}` and `{system}` (a read path of its own only). Each `add_` list is
   folded into its list, then every write path (both forms) is added to the read paths unless
   `"/"` is readable (D16 correction). `hide_paths` defaults to the D5 credential list.
   `isolation` accepts only `mode` (`base`, `materialize`, `share_paths` are P3/P4 and refused);
   it is read even when `enabled` is false. A sandbox built in code with nil `ReadPaths` reads the
   whole file system.
2. **Templates** (`sessionfs/sandbox_template.go`): `catalogs.sandbox` (default `.jad/sandbox`;
   absolute, relative to the root setting, or `{cwd}/…` per session, D28); a name is read with
   `readRegularFile` after `noteFile`, so an edited template changes the settings stamp; flat
   names only (D15), `enabled` required (D19), no `template` inside (D17), `schema_version` 1
   when given. Each layer (root defaults, session, profile) is expanded before the merge; the
   merge is per key with the `add_` lists concatenated, a layer that sets a list clears the
   `add_` lists below it, and `null` drops the layers below (§14.9, D26). As D16 says, a template
   named on an upper layer merges *over* the lower layers, so a key the template does not set
   (for example `network` from the defaults) still comes from below.
3. **Backends** (`tools/native/sandbox.go`, `sandbox_rules.go`, `sandbox_seatbelt.go`): with read
   paths, bwrap starts from an empty root and ro-binds each existing read path by its resolved
   name, recreating a top-level symlink (`/lib` → `/usr/lib`) with `--symlink`; a read path under
   a writable path is left to that bind (its name could have been swapped, as S5). jad's own
   network forwarder is bound read-only whatever the read paths. Hidden paths outside the
   environment are skipped. seatbelt gets `(deny file-read*)`, `(allow file-read-metadata)` and an
   allow per read root and writable root (unverified on macOS).
4. **Configuration read-only (D27, D28, broader than the design):** under a writable path, the root
   setting, the session file, the catalogs (profiles, models, toolsets, MCP servers, remotes,
   sandbox templates) and the whole state folder are read-only in every bwrap/seatbelt sandbox;
   the session's own state folder comes last, so its transcripts stay read-only even when
   `{session_dir}` is listed. A writable path inside one of them is writable again when no
   symlink lies on the way and no other such path holds it (in a read-only folder its name cannot
   be swapped). The design named only the template folder and the session's records; a writable
   toolset (hooks run outside the sandbox unless `tools` lists `hooks`), MCP server file (its
   command) or model file (its endpoint) is the same kind of escape. Agent folders are **not**
   protected: an agent's sandbox is narrowed by the session's, so an edited `agent.json` cannot
   widen the environment, and agents may keep files there. A protected path that does not exist
   is not created.
5. **LSP** (`lsp.go`, `lsp_client.go`): with `lsp` in `tools`, a language server starts through
   `sandboxed()` in the call's sandbox (plan-mode form in plan mode) with the call's `{tmp}`; the
   pool key includes `sandboxKey`, so a server never serves another sandbox.
6. **Agents** (`layout_load.go` `resolveAgentSandboxes`, `sandbox_narrow.go`): agent key
   `sandbox` (name, `{template, …}` or object), expanded like a layer and narrowed by the
   session's at load (D18, correction 6: read ⊆ parent read ∪ write, write ⊆ parent write; and,
   stricter than the design, plan-mode write ⊆ parent plan-mode write). `sandbox.isolation.mode`
   sets the agent's isolation; with the agent key `isolation` too it is refused. At run time
   (`subagent_call.go` `childEnvironment`) the agent's sandbox is narrowed again by the calling
   run's environment, so a narrow agent cannot start a wider one (`sandbox_widens`). An agent
   whose sandbox is bwrap/seatbelt, writes only under `{tmp}` and has no `profile_message` is a
   reader (`SandboxReader`, §14.3).
   **Departure (§14.4 step 5):** no derived tool set per (toolset, template). The child's calls
   follow `ExecutionContext.Environment` (item 7) for every process they start, so confinement
   is the agent's; but the exec/tool_smith *descriptions* a sandbox-only agent sees are the
   session's, and a toolset agent's stdio MCP servers start in the session's sandbox (U4 is fixed:
   `loadAgentToolsetMCP` now sets the wrapper), not the agent's own.
7. **`ExecutionContext.Environment`** (`tools/catalog.go`): `{Sandbox}` only (holder, tmp and the
   rest come with P3/P4). The bound runner and the remote API set it from the session's sandbox;
   `ForNested` copies it; `callSandbox(tool, configured, execution)` uses it when set (if it
   covers the tool), else the tool's configured sandbox.
8. **D20 / D24 departure:** no `RunChoice{…, Ceiling}` through `Resolve`/`LoadSessionSettings` and
   no `lockedConfigKey` change. `Prepare` refuses a target whose sandbox would widen the caller's
   (`sandbox_widens`, before any handle or queue place), and the run request carries a `Confine`
   hook that httpapi applies to the target's runner after it is resolved: its config's sandbox is
   replaced by the narrowed one (paths compared expanded, `{tmp}`/`{system}` by name, correction 8;
   a target without a sandbox inherits the caller's with the caller's paths and keeps its own
   session file read-only). A cached config is therefore never a narrowed one, and the cache key
   needs no ceiling. A plan-mode caller narrows by its plan-mode form. Not covered: a remote
   target (another server), and the target session's stdio MCP servers when `tools` does not list
   `mcp` (D10).
9. **In-process mirror** (`path_scope.go` `environmentPolicy`): with an environment, hidden paths,
   the `{tmp}` root and other runs' worktrees are denied, and with read paths only what they and
   the writable paths show is within reach (intersected with any existing scope). This replaces
   P1 item 8, which was wrong (see there). tool_smith bridge calls carry the environment through
   `ForNested`; no test runs a bridge call under it.
10. **Catalog API**: kind `sandbox` (`/api/catalog/sandbox/<name>`), flat IDs; a save is
    checked by the template loader (an enabled template needs its program on this machine); a
    delete is refused while the root defaults, a session, a profile or an agent inline in a toolset
    names it (agent folders are not searched; such a session then fails to load naming the
    template). With `{cwd}` the folder is per session and the API refuses to list it.
11. **Schemas**: `setting.schema.json` (`$defs.sandbox_fields`, `sandbox` as name, null or object;
    agent key `sandbox` in both agent schemas), `root-setting.schema.json` (`catalogs.sandbox`),
    new `sandbox.schema.json` (`schema_version` optional, as in the loader). Sample templates
    `sample/jad/sandbox/{workspace-writer,reader}.json`.
12. **`jad check`** starts sandboxes that differ only in their per-session read-only paths once.

Not done in P2 (later phases or follow-ups): `isolation.base/materialize/share_paths` and delegated
profile-run isolation (P3/P4); per-call `sandbox` / `sandbox_choice` (P2b); `--bind-fd` for S5; an
agent-specific tool description and MCP servers; seatbelt read rules on a Mac.
