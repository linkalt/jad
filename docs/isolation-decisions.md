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

## D15–D24

| # | Verdict | Decision | Reason | Phase | [USER] |
|---|---|---|---|---|---|
| D15 | accept | Templates are flat files `.jad/sandbox/<name>.json` referenced as `<name>`; a sub-folder is refused. | Follows the user's example and agent-folder naming; cheap to reverse before release. | P2 | |
| D16 | accept with correction | A reference may override a template per key with `{"template": n, …}`, merged like settings layers (objects per key, arrays replace) before normalising; "write implies read" is applied once, after the merge. | A profile can adjust one environment without copying it; applying the rule after the merge means an override can never write a path it cannot read. | P2 | |
| D17 | accept | No template chaining: `template` inside a template is refused. | `extends` costs little later; chains hide what a run can touch. | P2 | |
| D18 | accept with correction | An agent template that widens its parent is refused at load, naming agent, template and path; read containment is checked against the parent's read ∪ write, write against the parent's write. | Silently cutting it would run the agent in an environment nobody wrote. | P2 | |
| D19 | accept | A template file must state `enabled` or is refused. | A file named to confine runs must say whether it confines them. | P2 | |
| D20 | **reverse the design** **[USER]** | A `profile_message` target is narrowed by the caller's resolved sandbox exactly like a sub-agent: a target with no sandbox inherits the caller's; a target whose own template is wider is refused (`widens:`); a per-call `sandbox` narrows further. | Otherwise a confined profile can delegate to an unconfined one — the same kind of escape as the writable `.git/hooks`, against "limit the environment". | P2 | [USER] |
| D21 | accept | `sandbox_choice` is an explicit list of template names. | The config owner decides which environments are offered; dropping a file in `.jad/sandbox/` never offers it. | P2b | |
| D22 | accept with correction | A resume without `sandbox` keeps the source run's recorded template, re-resolved by name and narrowed again by the current agent and session; a missing or now-widening template refuses the resume, as does a choice that changes `isolation.mode`. | Falling back to the agent's default could silently widen a run started narrower. | P2b | |
| D23 | accept with correction | Child tools and MCP servers for a chosen template are built lazily and cached per (agent, template); `NarrowSandbox` for every (agent, choice) pair is computed at load. | Choices nobody makes start no MCP servers, while a broken environment is still caught at load. | P2b | |
| D24 | accept with correction | `Resolve`/`LoadSessionSettings` take `sessionfs.RunChoice{ModelPath, Sandbox, Ceiling}` where `Ceiling` is the caller's resolved sandbox; `lockedConfigKey` hashes both. | The sandbox travels as a resolved object, not a string, and two adjacent strings could be swapped silently. | P2 struct + Ceiling, P2b Sandbox | |

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
3. **D20:** "Should a profile reached through `profile_message` from a sandboxed caller be confined
   by the caller's sandbox? A target with no sandbox would inherit the caller's, and a target whose
   own template is wider would be refused with `widens:`. This may refuse delegations that work today
   from a sandboxed profile to a wider one."

## Implementable now without user input

D1, D2 (normal calls stay off), D4, D5, D6, D7 (delegated scope), D8, D9, D10 (with the scheduling
rule), D11 (with the sunset), D12, D13, D14, D15, D16, D17, D18, D19, D21, D22, D23, D24 (`RunChoice`
and `Sandbox`; `Ceiling` waits on D20).

P0, P1, P3 and P4 can start now in the order of §12, and so can P2 except two parts that wait for the
D20 answer: the narrowing of `profile_message` targets by the caller (§14.4 step 6, corrections 7, 8
and 10) and D24's `Ceiling` in `RunChoice` and the `lockedConfigKey` hash (correction 13). D3's
removal of `read_only_calls` (P5) waits for the D3 answer.

## Phase risk

The S5 fix relies on bwrap `--bind-fd`, which is unverified. This record places the S5 fix in P1,
following §12 (P1 "Depends on: P0 for S5 only"), because S5 already exists with today's
`writable_paths`. If P0 finds the installed bwrap lacks `--bind-fd`, P1 must refuse write paths that
lie under a writable parent instead of binding them, and P2 keeps that refusal for `write.paths`.
