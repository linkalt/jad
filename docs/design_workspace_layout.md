# Workspace layout (T-028): global shared catalogs, workspace-private sessions, profiles and state

Status: **phase 0 (design).** Nothing here is built yet. Mode v1 (today's single root
setting) stays the default and is not changed by any phase. Section 9 lists the questions
that are still open; phase 1 must not start before Q1, Q2, Q5, Q6 and Q7 are settled.

Sources: the user's request (T-028), the two designer reports (kept unchanged by the user in
`design-layout-catalog.md`), the jad/decider decisions for T-028 (layout, and the prompt
document follow-up D1 to D3), and a review of the current code (section 2 and 3).

## 1. Goal

The user wants:

- **Tools and models shared globally**, with the option to keep some private.
- **Sessions, profiles and their state private to one workspace** (a project directory), so a
  session of project X keeps its transcripts in X and is never listed by project Y.
- **Profiles that can read and improve themselves** (self-optimization), without loosening
  security unless the user opts in.
- **Prompts shared within a workspace**, and prompt documents private to one profile.
- **No data loss and no forced migration**: today's setups keep working as they are.

Out of scope: global profiles, workspaces found automatically, adding a workspace without a
restart, confidentiality against other processes of the same OS user, and hardening the local
admin HTTP API (section 6).

## 2. What the code does today (v1)

Facts checked in the code for this design (T-028 code_context entries and the review in
section 3):

- **One root setting per server.** `sessionfs.Load` finds exactly one `setting.json`
  (explicit path, `JAD_ROOT_SETTING_PATH`, or `rootsetting.Find`: `<cwd>/.jad/setting.json`,
  `$HOME/.jad/setting.json`, `<cwd>/setting.json`, `<cwd>/../setting.json`; first regular file
  wins) and reads it once at start (DECISIONS "The root setting is read once at start").
  `schema_version` must be 1.
- **One `Layout` for every root.** `catalogs` gives seven folders (profiles, models, tools,
  mcp, remotes, state, sandbox), resolved against the setting folder; every root of the setting
  shares them. Session IDs must be unique across roots because the roots share one state folder
  (`checkUniqueSessionIDs`).
- **State** (transcripts, staging, HTTP debug) is `<Layout.State>/<sid>/<cat>/<name>/`.
  **Tool storage** is `<cwd>/.jad/state/<sid>/<cat>/<name>/tools` (`tool_storage_dir`), so it is
  already per project.
- **Settings layering** is `mergeSettings`: root `defaults` < session `settings` < profile
  `settings`; the session cwd is the nearest of profile, session and defaults, and is required.
- **The only per-session catalog** is `catalogs.sandbox: "{cwd}/..."` (D28).
- **Catalog HTTP routes** use the first root with a Layout (`Resolver.catalogLayout`).
- **The content library** finds its setting with the same `rootsetting.Find`
  (`contentlibrary.findLibrarySettingPath`); its roots (`libraries`) are relative to that
  setting. This repository keeps its prompt templates and skills there
  (`.jad/prompts/{prompts,templates,skills}`).
- **Prompt documents** are file paths in `prompt_documents` (session, profile, sub-agent,
  statecontext), relative to the session cwd, `~` expanded, read when settings load.
  `defaults.prompt_documents` is refused. `project_instructions` (off by default) finds
  `AGENTS.md`/`CLAUDE.md`.
- **Sandbox.** `sandbox.enabled` defaults to **false**. When enabled (bwrap or seatbelt), the
  session's config paths (`sessionConfigPaths`: the root setting, the session file, the catalog
  folders, the state folder, the sandbox template) are read-only, and `hide_paths` (default:
  credential stores including `{home}/.jad/auth`) are hidden. A profile may *replace*
  `hide_paths`. Read paths default to the whole file system (`WholeFileSystemReadable`).
- **There are no native `write`/`edit` tools.** The native `read`, `write`,
  `edit`, `apply_patch`, `grep`, `glob`, `bash` tools were removed (`removedNativeTools` in
  `sessionfs/tool_shape.go`); files are changed through `exec`, which runs in the sandbox when
  one is enabled. `environmentPolicy` mirrors hidden paths and read reach to the in-process
  readers, but it does **not** add `ReadOnlyPaths` as deny rules. Whether a remaining in-process tool
  (`records`, `task`, `code_context`, `tool_smith`, ...) can write to a path a model chooses was
  not checked; phase 1 audits it.
- **The `profile` native tool** has `list`, `info`, `compact`, `reset`.

## 3. Review of the decisions against the code

The decider's layout (section 4) is feasible on the current architecture. The review changed
or added these points:

1. **The in-process write guard is a smaller job than feared.** The decider's stop condition
   ("in-process `write`/`edit` can't be guarded") does not apply: those tools no longer exist.
   Phase 1 must still list every in-process writer (candidates: `records`, `task`,
   `code_context`, `tool_smith`, `profile` reset, sub-agent stores) and check that none can be
   pointed at a config path by a model argument, and add a test that pins this.
2. **Nothing is enforced without a sandbox.** With `sandbox.enabled` false (the default) or on
   a platform without bwrap/seatbelt, `exec` runs as the user and can read every workspace and
   write every config file. "Private" then means separate storage and namespaces only. The UI
   and docs must say so; nothing in this design may be described as isolation unless a
   sandbox enforces it (section 6).
3. **Reading other workspaces is not blocked today even under a sandbox,** because read paths
   default to the whole file system. Q4 proposes hiding other workspaces' `.jad/`.
4. **The global file name collides with v1 discovery.** `$HOME/.jad/setting.json` is v1
   discovery candidate 2. A v2 global file at the same place would be read by v1 starts in
   other directories and fail on `schema_version`. See Q1.
5. **Workspace equal to the global folder.** Starting in `$HOME` makes `W/.jad` equal to `G`.
   This must be refused (Q2 default).
6. **Libraries.** The decider keeps libraries global, but this repository's prompt library is
   workspace content (`.jad/prompts`) and the user asked for workspace-shared prompts. See Q3.
7. **Which relative paths follow the declaring file.** The decider's rule "relative paths
   resolve against the folder of the declaring file" was narrowed for `prompt_documents` (D2).
   The same reasoning applies to every key that is cwd-relative today (`agents_dir`, MCP
   config paths, `project_instructions`, `tool_storage_dir`). See Q5.
8. **Unselected-profile check.** The private-document check (D1) can run in
   `checkUnselectedProfile` with `filepath.EvalSymlinks` on the path only, without reading the
   document, so the decider's stop condition for it should not be hit (confirmed in phase 2).
9. **Hidden paths can be replaced by a profile.** "`G/auth` hidden in every sandbox" needs a
   hidden list that a profile's `hide_paths` cannot remove (a new fixed part), not a default.
10. **"Loosen" is undefined for keys without an order** (hooks, `env_file`, `cwd`, endpoints).
    See Q7.

## 4. Design

### 4.1 Who owns what

| Scope | Location | Holds | Who writes |
|---|---|---|---|
| Global `G` | `$HOME/.jad`, or `--global-dir` / `JAD_GLOBAL_DIR` | the v2 global setting (Q1), `models/`, `tools/`, `mcp/`, `remotes/`, `sandbox/`, libraries, `auth` | the user |
| Workspace `W` (one exact directory) | `W/.jad/` | `workspace.json`, `sessions/`, `profiles/<cat>/<name>.json`, private `models/` and `tools/`, `state/<sid>/<cat>/<name>/` (same shape as today), `documents/` (suggested, not created) | the user; the engine writes sessions and state |
| Profile | `W/.jad/profile-own/<cat>/<name>/{models,tools,documents}` | the profile's private models, toolsets and prompt documents | that profile, only through the `profile` tool (4.7) |
| Profile working data | `W/.jad/data/profiles/<cat>/<name>/` | notes and memory the profile keeps across sessions | that profile, with ordinary tools (`{profile_data}`) |
| Tool storage | unchanged (`tool_storage_dir`, under the session cwd) | | |

There is no global profile catalog. A profile is reused elsewhere by copying it (an import
command can come later).

### 4.2 Which mode runs

- **v2 is on when `<start dir>/.jad/workspace.json` exists.** Otherwise discovery is exactly
  today's, and every v1 test and golden stays as it is.
- **The start directory** (or `--cwd`) is a workspace. **Other workspaces** must be listed in
  the global setting, `workspaces: [{id, path, label}]`, read at start. A cloned repo's `.jad`
  is never trusted just because it exists. `jad workspace add|list|remove` edits the list and
  says a restart is needed.
- A workspace is one exact directory: no search upward, no git-root detection.
- Refused at start, naming the file: `W/.jad` equal to `G` (after symlinks), two workspaces with
  the same id or the same directory, a listed path that is missing, an unknown key in
  `workspace.json`.
- The old `W/.jad/setting.json` is ignored in v2 with a warning. Rolling back is deleting
  `workspace.json`.

### 4.3 Roots, sessions and references

- **One workspace is one `Root`** with its own `Layout` (no shared pointer). Root IDs are the
  workspace ids, so `<root>:<sid>` references and the ambiguity errors keep working.
- Session IDs need only be unique inside a workspace; the same id in two workspaces is
  ambiguous without a qualifier, and an ambiguous reference is an error.
- A session belongs to one workspace. Its cwd defaults to `W`. A cwd set on a profile or a
  worktree never moves the session's catalogs or state (consistent with D28).
- Caches, locks, settings fingerprints and run registries are keyed by workspace id and
  qualified id, never by session or profile name alone.
- The catalog HTTP routes take a `scope` parameter (`global`, `workspace:<id>`,
  `profile:<id>/<cat>/<name>`); without it the v1 target is kept. `/api/workspaces` lists the
  workspaces.

### 4.4 Names and values

- IDs stay `<category>/<name>`. An unscoped name works when it exists in one visible scope
  (profile, workspace, global). In more than one, it is an error listing the candidates;
  `global:`, `workspace:`, `profile:` prefixes pick one. **No silent override.**
- The existing in-use guard refuses deleting an entry a reference still uses.
- Settings layers, lowest first: built-in < global `defaults` < workspace `defaults` < session <
  profile, with `mergeSettings`.
- **Ceiling keys** are not merged upward by a profile: sandbox, hooks, `env_file`, `cwd`,
  `tool_storage_dir`, model endpoints and credential references, MCP and exec command
  definitions, budgets, `max_run_*`. A profile value may not loosen them. What "loosen" means
  per key is open (Q7).
- Relative paths: see Q5. `env_file` in `workspace.json` resolves against `W/.jad`, in the
  global setting against `G` (this replaces the v1 rule "relative to the root folder" for v2).
- Plugins stay global; libraries see Q3.

### 4.5 Prompt documents (decider D1 to D3)

**D1. Three places, no global prompt-document folder.**

| Scope | Where | Named as | Usable by |
|---|---|---|---|
| Workspace-shared | anywhere in the project tree; suggested `W/.jad/documents/` (not created) | a path relative to the session cwd (today), or `{workspace}/...`, which always means `W` | profiles of that workspace |
| Profile-private | `W/.jad/profile-own/<cat>/<name>/documents/` | `{profile_own}/documents/...` | that profile only: another profile's path that resolves into it (after symlinks) is a load error |
| Global | none | | |

- `defaults.prompt_documents` stays refused in both setting files: nothing is added to a prompt
  automatically from outside the workspace.
- The global content library is not a source of prompt documents.
- Paths the user writes may still be absolute or start with `~`, as today.

**D2. Loading does not change.** Order stays `instructions`, `prompt_documents` in order, then
project instructions. Paths are not resolved through scopes (4.4 does not apply). Relative
paths stay relative to the session cwd, so a worktree session reads them from its worktree,
while `{workspace}` still means `W`. Agent folders, statecontext prompts and
`project_instructions` are unchanged. The placeholders exist only in v2, so no v1 golden file
changes. A missing file keeps today's error; an unselected profile with a missing document is
hidden with a warning (T-018).

**D3. Self-edit limits for `prompt_documents`.**

- Under `on`: only paths under the profile's own `{profile_own}/documents/`, or inside `W` but
  not `W/.jad/` except `W/.jad/documents/`. Refused, after following symlinks: absolute paths
  outside `W`, `~`, `G/`, other workspaces, other profiles' private folders.
- Under `full`: any path the user could write, except `G/auth` and other profiles' private
  documents.
- A profile writes its own documents only through the `profile` tool; raw writes into
  `profile-own/` stay blocked (by the sandbox; see section 6 without one).

**The private-document check is a prompt-assembly rule, not a read barrier.** Without a
sandbox that hides other profiles' `profile-own/`, a profile can still read them with `exec`.

### 4.6 Sandbox rules (when a sandbox is enabled)

- **Read-only:** the global setting and global catalog folders; `workspace.json`, `sessions/`,
  `profiles/`, `models/`, `tools/`, `mcp/`, `remotes/`, `sandbox/`, `profile-own/`, `state/` of
  the workspace. Listed exactly, not the whole `W/.jad`, because exec helpers and tool storage
  live there too.
- **Writable inside them** (existing inner-writable rule): tool storage, and the running
  profile's `{profile_data}` folder. Other profiles' data stays read-only.
- **Hidden, not removable by a profile:** `G/auth` (a fixed hidden part beside `hide_paths`).
- Own state and transcript readable; other profiles' state readable as today (the
  `transcript` tool), never writable.
- In-process readers follow the same hidden set (`environmentPolicy`); phase 1 adds the
  read-only list as deny-write rules for any in-process writer that takes a path.

### 4.7 Self-access and self-edit

The native `profile` tool gets actions on its own profile only:

- `config`: the profile file, where each effective value comes from, its revision, and the
  editable keys.
- `edit`: requires `if_match` (the revision). A trial load runs first and a failing change is
  refused. The old version goes into history, the write is atomic, an audit record goes into
  the transcript, the change applies from the next run.
- `history`, `revert`.

May change: instructions and documents (D3), offered models and default model and tools
from visible scopes, tuning settings (compaction, parallel calls), and its own models and
tools as copies of visible ones with only non-endpoint fields changed. Ceiling keys are
refused unless `profile_self_edit: "full"`. The setting is `on` by default, may be `off`, is set
at global or workspace level, and a profile cannot change it. Refused for sub-agents, for
other profiles and in plan mode.

Risk accepted by the decider: self-edit makes a prompt injection persistent. History, revert,
the audit record and `off` are the mitigations.

## 5. Compatibility and migration

- v1 runs unchanged when there is no `workspace.json`. `TestLoadFindsTheRootSettingInTheJadFolderThenHome`,
  `TestLoadDefaultRootsAndQualifiedSingleRootReference`, `TestRootSettingCatalogsAreReadAndChecked`
  and all v1 goldens
  stay green in every phase; a changed v1 golden stops the phase.
- `jad migrate workspace` prints a plan and changes nothing without `--apply`. It **copies**,
  never moves: models and tools to `G`, the workspace keeps profiles, sessions and `state`
  (paths unchanged). It rewrites relative `env_file` paths, refuses when an id already exists
  in `G`, writes a manifest, leaves the old `setting.json`, never deletes or edits ignored
  files, and only prints `.gitignore` advice.

## 6. Security boundaries: what is and is not enforced

| Claim | Enforced by | Holds without a sandbox |
|---|---|---|
| Workspace Y does not list or load X's sessions | the loader (separate roots and state) | yes |
| Same ids in two workspaces share no cache, lock or state | keys (4.3) | yes |
| An ambiguous name never resolves silently | the loader | yes |
| A profile cannot raise a ceiling key through the `profile` tool | the `profile` tool | yes, but without a sandbox `exec` can still rewrite the profile file |
| A profile cannot put another profile's private document in its prompt | the loader (symlink-resolved check) | yes |
| A tool call cannot write config, sessions or state | bwrap/seatbelt read-only paths | **no** |
| A tool call cannot read another workspace or another profile's private files | hidden paths (Q4) | **no** |
| `G/auth` unreadable to tool calls | fixed hidden path | **no** |

Not covered at all: other processes of the same OS user, MCP stdio servers and hooks that
are not started in a sandbox (unverified per server), and the local admin HTTP API, which an
agent with network access can call to change config (true today; a separate hardening task).

## 7. Phases

Each phase is one commit or a few coherent ones, never pushed. Each runs the affected package
tests, `verify-go.sh` (with targeted `--race`), frontend tests and typecheck when touched, and
`git diff --check`. Open gaps go into `backend-go/DECISIONS.md`.

0. **Docs** (this file and the DECISIONS entry).
1. **v2 loading:** mode selection, global and workspace scopes, workspace list, per-workspace
   Layout and state, scoped names, ceiling keys, `{workspace}`, sandbox read-only and fixed
   hidden paths, in-process writer audit, `/api/workspaces`, `scope` on catalog routes,
   content library following the mode.
2. **Self-access:** `{profile_own}`, `{profile_data}`, the private-document check, reading own
   config and state, `profile config`.
3. **Self-edit:** `edit`, `history`, `revert`, profile-scope models and tools,
   `profile_self_edit`, the D3 path limits.
4. **Migration command.**
5. **UI:** workspace picker, scope badges, "set by" display, a visible "not sandboxed" notice.

**Acceptance** (the designers' test plans, designer1 §7 and designer2 §8, plus):

- unrelated directories never see each other's sessions;
- the same session or profile id in two workspaces shares no cache, lock or state;
- an ambiguous name is an error, never a silent pick;
- each ceiling key is refused under `on` and allowed under `full`;
- a raw write to config from a sandboxed process fails (bwrap; seatbelt where available);
- v1 behaves exactly as before;
- prompt documents: a workspace document is used by profiles of `W` and not reachable through
  `{workspace}` from another workspace; profile B naming A's `{profile_own}` document, directly
  or through a symlink, fails to load; under `on`, self-edit to `~/x.md`, `G/...`, another
  workspace or `W/.jad/sessions/...` is refused, under `full` `~/x.md` is allowed and
  `G/auth/...` still refused; a worktree cwd resolves relative paths from the cwd while
  `{workspace}` means `W`; `defaults.prompt_documents` still refused.

**Stop and report if:** a sandbox backend cannot make own data writable while config stays
read-only; serving several workspaces would need re-reading a setting while running; any v1
golden changes; an in-process writer is found that takes a model-chosen path.

## 8. Not built

Global profiles; automatic workspace discovery; adding workspaces without restart;
confidentiality against same-user processes; admin API hardening; profile import command.

## 9. Open questions (for jad/decider)

Each has the proposed default the executor would use.

- **Q1. Name of the v2 global setting** (blocks phase 1). `$HOME/.jad/setting.json` is a v1
  discovery candidate. Proposed: the v2 global setting is `G/global.json`, which v1 never
  reads, so v1 starts in any directory are untouched; a v1 `G/setting.json` keeps serving v1
  starts. Alternative: `G/setting.json` with `schema_version: 2`, which v1 discovery would then
  have to skip with a warning.
- **Q2. Refusals at start** (blocks phase 1). Proposed: refuse `W/.jad == G`, duplicate
  workspace ids or directories, and a missing listed workspace (instead of serving the others
  with a warning).
- **Q3. Workspace-shared prompt library** (blocks the content-library part of phase 1).
  Proposed: `workspace.json` may list `libraries` roots that only that workspace sees, added
  after the global ones; the same entry name in both is an error, as for catalog names. This
  keeps this repository's `.jad/prompts` usable without making it global.
- **Q4. Hide other workspaces under a sandbox** (phase 1 or 2). Proposed: every sandbox of a
  v2 session hides the `.jad/` of every other listed workspace (in-process readers too). Hiding
  other profiles' `profile-own/` inside the same workspace needs hide-then-re-expose of the own
  folder; its feasibility on bwrap and seatbelt is unverified and is checked in phase 2 before
  it is promised.
- **Q5. Relative paths** (blocks phase 1). Proposed: the declaring-file rule applies to paths
  in the global setting and `workspace.json` (`catalogs`, `libraries`, `workspaces`,
  `env_file`); keys that are cwd-relative today (`prompt_documents`, `agents_dir`, MCP config
  paths, `project_instructions`, `tool_storage_dir`) stay cwd-relative, like D2.
- **Q6. Cross-workspace transcript reads** (blocks phase 1). Proposed: in v2 the `transcript`
  and `transcripts` tools and `profile_message` reach only the own workspace; another
  workspace's sessions are not found.
- **Q7. What a ceiling key "loosening" means** (blocks phase 1). The decider says a profile
  value cannot loosen a ceiling key, but only some keys have an order. Proposed: keys with an
  order are checked against the lower layers (sandbox `enabled` cannot go from true to false,
  `hide_paths` and read-only paths can only grow, `network` cannot widen, budgets and
  `max_run_*` can only go down); keys without one (hooks, `env_file`, `cwd`,
  `tool_storage_dir`, endpoints, credential references, MCP and exec command definitions) may
  still be set by the user in a profile file, as in v1, and are refused only for self-edit
  under `on`. A stricter reading (refuse them in every profile file) would break profiles
  that set them today when they move to v2.
