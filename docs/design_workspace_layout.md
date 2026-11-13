# Workspace layout (T-028): global shared catalogs, workspace-private sessions, profiles and state

Status: **phase 1a built** (v2 loading, scoped names, fixed hidden and read-only sandbox paths,
cross-workspace tool limits, ordered ceilings; section 10). Phase 1b (prompt libraries,
`/api/workspaces`, `scope` on catalog routes) and phases 2 to 5 are not built. Mode v1 (today's
single root setting) stays the default and is not changed by any phase. jad/decider settled
Q1 to Q7 (section 9).

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
  readers, but it does **not** add `ReadOnlyPaths` as deny rules.
- **In-process writer audit (phase 1, grep of every `os.WriteFile`, `Create`, `OpenFile`,
  `Rename`, `MkdirAll`, `Remove*` outside tests in `internal/tools` and the other packages a
  tool reaches).** No in-process writer takes a path from a model argument. They write to
  engine-chosen folders, with model-given names checked against fixed patterns: `records`
  (tool storage, type `^[a-z][a-z0-9_-]{0,63}$`, refs of a fixed file pattern, symlinked
  folders refused), `task` and `code_context` (tool storage, task key
  `^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`), `tool_smith` (a temporary work folder; function names
  come from files of a configured library folder), exec spill and async results (tool storage
  or temporary folders), sub-agent and worktree records (state and worktree folders), the
  `profile` tool's `compact` and `reset` (the named profile's own transcript, through the
  maintenance path). The catalog file writers (`sessionfs/catalog_files.go`, plugins) serve
  the HTTP API and `jad plugin`, not a tool. So the stop condition is not hit and no deny-write
  rule was added; the audit is by search, not a proof, and a new writer must keep to this.
- **The `profile` native tool** has `list`, `info`, `compact`, `reset`.

## 3. Review of the decisions against the code

The decider's layout (section 4) is feasible on the current architecture. The review changed
or added these points:

1. **The in-process write guard is a smaller job than feared.** The decider's stop condition
   ("in-process `write`/`edit` can't be guarded") does not apply: those tools no longer exist,
   and the phase 1 audit (section 2) found no in-process writer that takes a model-chosen path.
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
- Checks at start (Q2): refused, naming the file: a workspace whose tree holds `G` (`W/.jad`
  equal to `G` after symlinks, or W is `$HOME`, `/home`, `/`), two workspaces with the same id
  or directory, a relative `workspaces[].path`, a broken start `workspace.json` or one with an
  unknown key, an unlisted start whose folder-name id is taken by a listed one. Skipped with a
  warning (and kept as unavailable in `Config.Workspaces`): a listed workspace that is missing
  or whose `workspace.json` is broken. Nested workspaces are allowed.
- A named root setting (`LoadOptions.RootSettingPath` or `JAD_ROOT_SETTING_PATH`) keeps v1 even where
  `workspace.json` exists.
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
- The catalog HTTP routes take a `scope` parameter (built in phase 1b step 3: `global` for
  G's models, toolsets, MCP servers and sandbox templates, `workspace:<id>` for that
  workspace's profiles, models and toolsets; a profile's own models are not served by the
  catalog routes, decider 3c C2). Without it the v1
  target is kept: in v2 the start workspace's profiles and the global rest. A kind the scope
  has no folder for, an unknown workspace, and any scope in v1 are 404. A global delete is
  refused while a profile or session of any served workspace still needs the file; a
  workspace delete looks at that workspace only. A profile fork writes into the session's
  workspace. `/api/workspaces` lists the workspaces (served and skipped, the start first).
  `jad plugin` in v2 installs into the global scope (records and files in `G/state/.plugins`)
  and refuses a plugin that ships profiles, since there are no global profiles.

### 4.4 Names and values

- IDs stay `<category>/<name>`. An unscoped name works when it exists in one visible scope
  (profile, workspace, global). In more than one, it is an error listing the candidates;
  `global:`, `workspace:`, `profile:` prefixes pick one. **No silent override.** Built in phase
  1a for models and toolsets (`catalog.Merge`): a name in both is left out of the offered list
  with a warning and naming it is an error listing both files. The prefixes `global:` and
  `workspace:` (phase 1b step 3) pick one scope in a profile's `models`, `model_default` and
  `toolset` (tested); `catalog.Selector`, `Catalog.Lookup` and `Narrow` take them, so a
  sub-agent's model ID, resolved with `Lookup`, should too (not tested); a prefix in v1 matches
  nothing. After narrowing, a `model_default` is stored by its bare
  ID. A toolset reference keeps its prefix; the in-use guard counts a prefixed reference too.
- The existing in-use guard refuses deleting an entry a reference still uses.
- Settings layers, lowest first: built-in < global `defaults` < workspace `defaults` < session <
  profile, with `mergeSettings`.
- **Ordered ceiling keys** (Q7, built in phase 1a, `sessionfs/workspace_ceiling.go`), checked in
  every v2 profile file against the global and workspace defaults and the session: a set
  `max_run_tokens`, `max_run_cost_usd`, `max_run_duration_ms` and `budget` cap can only go
  down; an enabled sandbox cannot be turned off or removed, cannot change backend, executable
  or command, cover fewer tools, hide fewer paths, make more paths writable (also in plan mode)
  or readable, turn the network on or allow more hosts. Paths are compared as written. An
  unselected profile that loosens one is left out with a warning; a selected one fails.
  Depth limits are toolset settings (`tools.native.subagent`), not profile settings, and are
  not checked.
- **Keys without an order** (hooks, `env_file`, `cwd`, `tool_storage_dir`, MCP and exec command
  definitions) stay settable in profile files the user writes, as in v1. Under self-edit
  (phase 3) they are refused under `on` and allowed under `full`; a hook or command a profile
  adds under `full` is the same as running anything unsandboxed. Self-edit can never set or
  change model endpoints or credential references (also not under `full`); a profile's own
  model is **based on** a global or workspace model and changes only allow-listed tuning keys,
  so its endpoint and credential always come from the base (decider 3c C1, section 17).
- Relative paths (Q5): `catalogs`, `libraries` and `defaults.env_file` of the global setting
  resolve from `G`, of `workspace.json` from `W/.jad`. `defaults.cwd` of `workspace.json`
  resolves from `W` (a cwd inside `.jad` would run tools there), of the global setting from
  `G`; without any cwd a session works in `W`. Everything cwd-relative today stays so
  (`prompt_documents`, `agents_dir`, MCP config paths, `project_instructions`,
  `tool_storage_dir`). Other relative paths resolve from the folder their file came from
  (decider ruling on phase 1a, built in 1b): `W/.jad` for the files of a workspace (sessions,
  profiles, workspace toolsets), `G` for global ones (global toolsets, `global.json`). The
  composed settings are read with `W/.jad` as the base (`Root.pathBase`); a global toolset's
  `env_file` (exec, tool_smith, hooks) and path rules (exec, read_image, lsp, code_context) and
  `global.json`'s `libraries.tool_definition_paths`, `defaults.hooks.env_file` and
  `defaults.session_files.paths` are made absolute from `G` before they are merged. Sandbox
  paths must already be absolute or start with a placeholder. `{workspace}` (phase 1b step 3)
  starts a session or profile `prompt_documents` path and always means W, also when the cwd is
  elsewhere; it must be alone or followed by `/`; in v1 the text stays a path as written.
- Plugins stay global: `jad plugin` installs skills and commands into the first global
  folders of `global.json`.
- **Content libraries (Q3, built in phase 1b step 2):** `global.json` `libraries`
  (`prompt_dirs`, `template_dirs`, `skill_dirs`, `command_dirs`, `max_asset_bytes`,
  `tool_definition_paths`) from G, each kind not set using the built-in `.agent/<kind>` under
  G; `workspace.json` `libraries` (the four folder keys and, since 4c, `tool_definition_paths`; none by default) from `W/.jad`,
  seen only in that workspace. Both are read once at start (`sessionfs.Libraries`); no
  `setting.json` is read by the HTTP library, the `skill` tool or `jad plugin` in v2. A skill
  (by name) or command (by word) found in both scopes is left out of every list, named in the
  catalog's `ambiguous`, and reading it is an error naming both (`skill_ambiguous` for the
  tool, 422 for the HTTP routes); inside one scope the first folder still wins, as in v1.
  Prompts and templates are picked by a path-derived ID, so both scopes are listed side by
  side. The HTTP library takes `?workspace=<id>` (default the start workspace; unknown is 404).

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

- **Sealed (built in phase 1b, decider fix A):** `G` and the own `W/.jad` are read-only as a
  whole in every bwrap or seatbelt sandbox of a v2 session (`SandboxSettings.SealedPaths`, set
  with the fixed hidden list so no profile or template removes them). Unlike the v1 read-only
  configuration paths, no writable path is bound again inside a sealed folder: a
  `writable_paths` entry such as `{workspace}/.jad/profiles` or `{home}` does not open it, and
  no new file (a profile, a toolset, `G/global.json`, `W/.jad/setting.json`) can be made. A
  missing sealed folder whose parent is writable becomes an empty read-only folder (bwrap makes
  the mount point, so an empty `G` can appear on the host); one whose parent is missing too is
  not covered. The v1 read-only list (global setting, session file, profiles, catalogs, state,
  sandbox templates, `workspace.json`, `W/.jad/sessions`, `models`, `tools`) still applies.
- **Writable inside the sealed `W/.jad`:** only the engine's helper folders `W/.jad/exec` (when
  the sandbox covers `exec`) and `W/.jad/functions` (when it covers `tool_smith`), the default
  `functions_dir` of each tool for a session working in `W`; made when missing. A
  `functions_dir` configured elsewhere in `W/.jad` is read-only. Tool storage under `W/.jad/state`
  was already read-only to sandboxed calls (the session state folder is a configuration path),
  so nothing that worked before needs `W/.jad` writable. Phase 2b adds the running profile's
  data folder (section 12).
- **Consequences:** a sandboxed call can no longer edit files a project keeps in `W/.jad`, such
  as this repository's `.jad/prompts` library; a worktree run's copy of a tracked `.jad` is
  sealed too.
- **Hidden, not removable by a profile (Q4, built in phase 1a):** `G/auth` and the `.jad/` of
  every other listed workspace (skipped ones and nested ones included), as written and with
  links resolved. `Layout.FixedHidePaths` is added after `hide_paths` is read, to the session
  sandbox, the plan-mode sandbox, agent toolset sandboxes, agent sandboxes and sandbox
  choices, so neither a profile nor a template can remove it. Only `.jad/` is hidden, not the
  other workspaces' project files. A profile can still enable a sandbox of its own where the
  defaults enable none; the fixed part applies to it too.
- Own state and transcript readable; other profiles' state readable as today (the
  `transcript` tool), never writable.
- In-process readers follow the same hidden set (`environmentPolicy`) when a sandbox is on. In
  v2 they also answer the fixed hidden paths as not found without a sandbox (phase 1b, decider
  fix C): the loader puts them in `NativePathPolicy.Hidden` of code_context, lsp, read_image,
  transcript (also `path:`), transcripts, profile, the UI file list and the cwd checks of exec
  and tool_smith; `resolveExistingTarget` and `transcriptDenied` check the requested path and
  the path after links, and give the answer of a missing file. No deny-write rules were added:
  the audit (section 2) found no in-process writer that takes a model-chosen path.
- Built in phase 1a: the read-only list adds `workspace.json`, `W/.jad/sessions`, `W/.jad/models`
  and `W/.jad/tools` to the v1 list (global setting, session file, profiles, global models,
  tools, mcp, remotes, state, sandbox templates). `profile-own/` and `{profile_data}` come with
  phase 2.
- seatbelt (macOS) is unverified: nothing here is claimed for it until it is tested on a Mac.
- With no sandbox in the global or workspace defaults, a v2 start logs one warning.

### 4.7 Self-access and self-edit

The native `profile` tool gets actions on its own profile only:

- `config`: the profile file, where each effective value comes from, its revision, and the
  editable keys.
- `edit`: requires `if_match` (the revision). A trial load runs first and a failing change is
  refused. The old version goes into history, the write is atomic, an audit record goes into
  the transcript, the change applies from the next run.
- `history`, `revert`.
- From phase 3b, `edit` also creates, replaces and deletes the profile's own documents
  (`{profile_own}/documents` only) in the same revision as the profile file; `config` lists
  them with their revisions and `revert` restores them (section 14).

May change: instructions and documents (D3), offered models and default model and tools
from visible scopes, tuning settings (compaction, parallel calls), and its own models and
tools as files based on visible ones that change only allow-listed tuning keys (section 17;
endpoints and credentials always come from the base). Ceiling keys are
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
  files, and only prints `.gitignore` advice. The planner (dry run, phase 4a) is section 23;
  `--apply`, the journal and `--rollback` (phase 4b) are section 24.

## 6. Security boundaries: what is and is not enforced

| Claim | Enforced by | Holds without a sandbox |
|---|---|---|
| A session of Y keeps its state and catalogs apart from X's | the loader (separate roots and state) | yes |
| A tool of Y (`transcript`, `transcripts`, `profile`, `profile_message`) does not reach X's sessions; X's session reads as not found | `sessionfs.ResolveFrom` (Q6) | yes, but `exec` and the in-process readers can still read X's files by path |
| Same ids in two workspaces share no cache, lock or state | keys (4.3) | yes |
| An ambiguous name never resolves silently | the loader | yes |
| A profile cannot raise a ceiling key through the `profile` tool | the `profile` tool | yes, but without a sandbox `exec` can still rewrite the profile file |
| A profile edits only its own file, only from its own top-level run, not in plan mode, and every edit passes the level and a trial load | the `profile` tool and `sessionfs.EditProfile` (phase 3a) | yes, but without a sandbox `exec` can still rewrite the file |
| The history of self-edits cannot be changed by the profile's own calls and is not seen by other profiles | sealed `W/.jad` and the hidden `profile-own` (phase 3a), bwrap tested | **no** |
| A profile cannot put another profile's private document in its prompt | the loader (symlink-resolved check) | yes |
| A tool call cannot write config, sessions or state | bwrap/seatbelt read-only paths | **no** |
| A tool call cannot read another workspace's `.jad` or another profile's private and data folders | hidden paths (Q4, phase 2b `ProfileHidden`), bwrap tested | **no** |
| A tool call writes only its own profile's data folder, and only in that profile's own top-level run (not plan mode, a sub-agent, a sandboxed caller's `profile_message`, a sandbox choice) | `ProfileData` and the hide rules (phase 2b), bwrap tested | **no**: without a sandbox `exec` writes every profile's folders |
| The in-process readers answer another profile's private and data folders as not found | `NativePathPolicy.Hidden` with `HiddenExcept` (phase 2b) | yes; not `exec` |
| `G/auth` unreadable to tool calls | fixed hidden path | **no** |
| A sandboxed call cannot change `G` or `W/.jad` (but the helper folders) | sealed paths (fix A), bwrap tested | **no** |
| The in-process readers (code_context, lsp, read_image, transcript `path:`, transcripts, profile) answer `G/auth` and other workspaces' `.jad` as not found | `NativePathPolicy.Hidden` (fix C) | yes; not other workspaces' project files, and not `exec` |

Not covered at all: other processes of the same OS user; children that the sandbox does not
cover (by default it covers only `exec` and `tool_smith`: hooks, MCP stdio servers and the
`lsp` servers run unsandboxed unless `sandbox.tools` lists them; HTTP MCP servers, plugins and
extension tools are outside any sandbox); the transcript tool's `path:` form when
`allow_path` is on and no sandbox hides the path; and the local admin HTTP API and UI, which
see every workspace by design and which an agent with network access can call to change
config (a separate hardening task; the phase 5 UI and the docs must say so).

## 7. Phases

Each phase is one commit or a few coherent ones, never pushed. Each runs the affected package
tests, `verify-go.sh` (with targeted `--race`), frontend tests and typecheck when touched, and
`git diff --check`. Open gaps go into `backend-go/DECISIONS.md`.

0. **Docs** (this file and the DECISIONS entry).
1. **v2 loading.** Split in two commits:
   - **1a (built):** writer audit, mode selection, `G/global.json` and `workspace.json`, workspace
     list and Q2 checks, per-workspace Layout and state, scoped model and toolset names,
     ordered ceilings (Q7), sandbox read-only and fixed hidden paths (Q4), cross-workspace
     tool limits (Q6), schemas `sample/schemes/global.schema.json` and `workspace.schema.json`.
   - **1b (built, section 11):** prompt libraries following the mode (global `libraries` from
     `global.json`, workspace `libraries` in `workspace.json` from `W/.jad`, visible only in that
     workspace, a name in both an error; the HTTP library, the `skill` tool and commands),
     `/api/workspaces` (from `Config.Workspaces`), `scope` on catalog routes, the `global:` and
     `workspace:` prefixes, `{workspace}`. Until 1b, a v2 start refuses any `libraries` key of
     `global.json` but `tool_definition_paths`, and the prompt library, the `skill` tool and
     commands still find their setting as in v1 (they may read `W/.jad/setting.json` or
     `G/setting.json`); the catalog routes use the start workspace's layout (global models and
     tools, the start workspace's profiles).
2. **Self-access (section 12):** `{profile_own}`, `{profile_data}`, the private-document check,
   reading own config and state, `profile config`. Built in two commits (2a backend, 2b
   sandbox), section 12.
3. **Self-edit:** `edit`, `history`, `revert`, profile-scope models and tools,
   `profile_self_edit`, the D3 path limits. 3a (the profile file, history, revert, the level) is
   built (section 13); 3b (own documents) is built (section 14); 3c.1 (hand-written own models)
   is built (section 17); 3c.2 (self-edit of own models) is built (sections 18 to 20); 3c.3
   (own toolsets, written by hand and through `edit`) is built (section 21); 3c.3b (a load reads
   one revision of each profile) is built (section 22).
4. **Migration command.** 4a (the planner and the dry run, `jad migrate workspace`) is built
   (section 23); 4b (`--apply`, the journal, the v1/v2 check and the cleanup, `--rollback`) is built
   (section 24), tested only in temporary folders.
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

## 9. Questions settled by jad/decider

Answers (T-028, after phase 0): Q1 accepted (`G/global.json`; `G/setting.json` not read in v2,
an info line when it exists; v1 and v2 may read the same `G/models` and `G/tools`, so v2 writes
nothing v1 cannot read). Q2 accepted with one change: a missing or broken listed workspace is
skipped with a warning, not refused (section 4.2). Q3 accepted: `workspace.json` `libraries`,
resolved from `W/.jad`, after the global ones, only in that workspace, a name in both an error,
no profile libraries (phase 1b). Q4 accepted with a fixed hidden list (section 4.6); hiding
other profiles' `profile-own/` is a phase 2 feasibility check, else the private-document rule
is a prompt-building rule only. Q5 accepted (section 4.4). Q6 accepted: not found, never
forbidden; remotes still work; the HTTP API and UI see every workspace; v1 multi-root is
unchanged. Q7 accepted with tighter self-edit limits (section 4.4). The proposals as they were
put to the decider follow.

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
- **Q6. Cross-workspace transcript reads** (blocks phase 1; for `profile_message` superseded by
  section 29, `transcript`, `transcripts` and the profile actions keep it). Proposed: in v2 the `transcript`
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

## 10. Phase 1a as built

- `sessionfs.Load` runs `loadWorkspaces` (`sessionfs/workspace_load.go`) when
  `<start>/.jad/workspace.json` is a file and no root setting is named. G is
  `LoadOptions.GlobalDir`, `JAD_GLOBAL_DIR`, or `$HOME/.jad`. `G/global.json` is optional
  (keys: `schema_version` 2, `server`, `cli`, `workspaces`, `catalogs` of models, tools, mcp,
  remotes, sandbox, `defaults`, `libraries.tool_definition_paths`); `workspace.json` takes
  `schema_version` 2, `id`, `label`, `defaults`, `libraries` (folders and, since 4c, `tool_definition_paths`). Unknown keys are refused. Both are read once.
- One `Root` per served workspace, start first: ID the workspace id, `Directory`
  `W/.jad/sessions`, `GlobalJadRoot` G, its own `Layout` (profiles and state in `W/.jad`, global
  catalogs, `WorkspaceModels` and `WorkspaceTools` in `W/.jad`, defaults: cwd W < global <
  workspace). Same session ids in two workspaces are allowed (`sharesState`); unqualified they
  are ambiguous for the HTTP API.
- Tests: `sessionfs/workspace_load_test.go` (one per Q2 row, v1 ignores `global.json`, v2
  ignores `setting.json`, separate state, scoped names, fixed hidden and read-only paths,
  ceilings), `sessionfs/workspace_schema_test.go`, `tools/native/sandbox_workspace_test.go`
  (a real bwrap run: `G/auth` and the other workspace's `.jad` unreadable with
  `hide_paths: []`, `workspace.json` and session files read-only), `app/workspace_tools_test.go`
  (Q6 for transcript, transcripts listing, profile list and actions, profile_message).
- Interface change: `native.ProfileActions` and `native.TranscriptSessions` take the caller's
  session key, so the app can look sessions up as the caller sees them.

## 11. Phase 1b as built

**Step 1, safety (decider fixes and ruling, first 1b commit):**

- Paths resolve from the folder of their file (section 4.4): `Root.pathBase`,
  `absoluteGlobalToolsetPaths` and the `global.json` rewrites in `workspaceDefaults` and
  `globalLayout` (`sessionfs/workspace_paths.go`, `workspace_load.go`). The toolset check of the
  catalog routes reads a workspace toolset from `W/.jad` too. v1 keeps the setting folder.
- Fix A, sealed `G` and `W/.jad` (section 4.6): `Layout.SealedPaths` and `SealedHelpers`,
  `SandboxSettings.SealedPaths` and `SealedInner` (kept by `NarrowSandbox`, which keeps a helper
  folder only where the parent lets it, by the worktree move and cleared for sandbox probes),
  `sandboxSealedRules` after the configuration rules and before the hidden paths.
- Fix C, in-process readers (section 4.6): `NativePathPolicy.Hidden` set by
  `SessionConfig.hideFixedPaths`, kept by every policy copy of `scopedPolicy`.
- Tests: `sessionfs/workspace_paths_test.go` (two workspaces with `env_file: ".env"` read their
  own file, global toolset and `global.json` paths from `G`, workspace toolset from `W/.jad`;
  narrowed sealed lists; reader policies get the hidden list),
  `tools/native/sandbox_workspace_test.go` (real bwrap: with `$HOME` and
  `{workspace}/.jad/profiles` writable, editing a profile, a new profile, a new toolset,
  `W/.jad/setting.json`, a `G/models` file, a new `G/tools` toolset and a new `G/global.json` all
  fail, while `$HOME`, `W/.jad/exec` and the project stay writable; a missing sealed folder),
  `tools/native/path_hidden_test.go` (a hidden file by name and through a link gets exactly the
  missing-file answer; the policy copies keep the list).

**Step 2, libraries (fix B with Q3):** `sessionfs/workspace_libraries.go` (parsing,
`Layout.Libraries`, `SessionConfig.Libraries`), `contentlibrary/scopes.go` (scoped listing and
reads, `ListWorkspaceSkills`, `ReadWorkspaceSkill`, `GlobalInstallFolders`), the `workspace`
parameter of `httpapi.RootLibrary`, `native.NewSkillTool(…, libraries)`, `app.LoadConfig`
(`Options.Workspaces`), `cmd/jad/plugin.go`; schemas `global.schema.json` and
`workspace.schema.json`. Tests: `sessionfs/workspace_libraries_test.go`,
`contentlibrary/scopes_test.go`, `app/workspace_library_test.go` (HTTP: own and global
library, another workspace does not see it, no `setting.json` read, ambiguous names left out,
unknown workspace 404), `tools/native/skill_test.go`
(`TestWorkspaceSkillToolRefusesANameInBothLibraries`).

**Step 3, scope, workspaces, prefixes, `{workspace}`, Q7 additions (fix D):**
`sessionfs/workspace_scope.go` (`ScopedCatalogLayout`, scoped `catalogDir`, in-use checks over
every workspace for a global delete), the `scope` argument of `httpapi.CatalogFiles`,
`app/profile_fork.go`, `GET /api/workspaces` (`app/workspaces.go`, `httpapi/workspaces.go`),
`cmd/jad/plugin.go` and `plugins.Target.NoProfiles`; `catalog.MergeScopes`, `Entry.Scope`,
selector and lookup prefixes; `{workspace}` in `loadPrompt`; `sandboxPathKey` for the Q7 path
comparison. Depth limits stay unchecked: they live in toolsets, which no lower layer
(defaults, session) can set, so there is nothing to compare against until a profile can write
its own toolsets (phase 3). Tests: `sessionfs/workspace_scope_test.go`, `catalog/scope_test.go`,
`app/workspace_routes_test.go` (`/api/workspaces`, a scoped PUT lands in b, 404s, a fork in b
writes into b), `plugins_test.go` (`TestInstallRefusesProfilesWhereThereAreNoGlobalOnes`),
`sessionfs/workspace_paths_test.go` (`TestCeilingComparesSandboxPathsCleaned`).

## 12. Phase 2 as built

**Commit 2a, backend only (no sandbox involved):**

- `{profile_own}` (D1): a leading `{profile_own}` in a profile's `prompt_documents` is
  `W/.jad/profile-own/<cat>/<name>` (`Layout.ProfileOwnDir`, expanded in
  `layoutCompose.addProfileFields`, which knows the profile ID). It must be alone or followed by
  `/`. v1 keeps the text as a path relative to the cwd, as with `{workspace}`.
  `SessionConfig.ProfileOwnDir` and `ProfileDataDir` (`W/.jad/data/profiles/<cat>/<name>`) are set
  in v2 only.
- The private-document check (D1, P2): a prompt document that resolves after links into the
  `.jad/profile-own/` of any known workspace (`Layout.ProfileOwnRoots`, served or skipped), and
  not under `<real profile-own>/<cat>/<name>` of the profile itself, is a load error. The own
  folder is taken by its name under the real `profile-own`, so an own folder linked to another
  profile's does not make that one "own"; the same ID in another workspace is another profile.
  `judgeScope` checks every profile's documents (a relative one from the cwd its layers give), so
  an unselected profile is left out with a warning; the selected profile's load checks the
  documents it actually read (its own, statecontext triggers', agents' and agent toolsets') and
  fails naming the profile file. A missing file keeps today's error.
- `profile config` (P3): new action of the native `profile` tool. The session and profile come
  from the execution context's owner key only (`ProfileActions.ProfileConfig(ctx, ownerKey)`);
  `path` may be omitted, `self` or the caller's own name; another profile is refused
  (`profile_config_self_only`) whatever `allow_other_profiles` says, a sub-agent too
  (`profile_children_denied`) whatever `allow_children` says. Plan mode does not refuse the
  action; the `profile` tool is not `ReadOnly`, so a plan-mode run sees it only through a
  `plan_mode.allow` entry, as before. It returns the profile file (contents, path, revision =
  `CatalogETag` of the bytes shown), the effective settings (the `layoutSettingKeys` of the
  composed setting, taken from the compose that runs), `set_by` per dotted leaf (an array is one
  value) and the layer list (`built-in`, `global`, `workspace`, `session`, `profile` in v2;
  `defaults`, `session`, `profile` in v1), and the own and data folders. `set_by` is computed by
  mirroring `mergeSettings` (an object over an object merges, anything else replaces and drops
  the earlier layers); a key whose composed value is not that merge (`sandbox`, composed with
  templates and added lists; `hooks` a toolset adds) is one entry naming every layer that has
  it, or `toolset`. A setting in neither list takes its built-in default. Not shown: `server`,
  triggers of `global.json`, remotes and their tokens, `G/auth`, the contents of `env_file` (none
  of them is a layer setting key; toolsets are not shown either).
- Schema: `sample/tools/native/profile.json` changed with the Go schema. The definition file the
  deployment's `tool_definition_paths` points at (in this checkout `~/.jad/tools_definition/native/profile.json`)
  is not changed by the commit and must match the sample again, or the running jad refuses the
  `profile` tool. The path is stored in the workspace or global setting, so it is not repeated here.

**Commit 2b, the sandbox side:**

- `SandboxSettings.ProfileHidden` (`W/.jad/profile-own`, `W/.jad/data/profiles`, as written and
  below the real `W/.jad`), `ProfileShown` (the running profile's own folder and data folder) and
  `ProfileData` (its data folder). `layoutCompose.addFixedHidePaths` sets the first two on every
  sandbox of a v2 session (session, agents, agent toolsets, sandbox choices, the plan-mode
  fallback); `finishSandbox` sets `ProfileData` on the session's own sandbox only.
- S3, hiding: `sandboxProfileRules` (bwrap and seatbelt, after the sealed rules) cover each
  hidden folder with an empty read-only one and show the own folders again by name below the
  hidden folder's target; a shown folder that is missing, a link or behind a link is not shown.
  The parent folders are hidden and the own subfolders bound back, so folders made later are
  hidden too (no listing at run start). The in-process readers get the same folders as
  `NativePathPolicy.Hidden` with the own ones in `HiddenExcept`, with or without a sandbox; an
  except folder never shows a hidden path inside it.
- S1, own data writable (2c, decider F2): the shown data folder is bound read-write whenever it
  is `ProfileData`, with or without a writable path that covers it, and never counts against the
  writable-paths ceiling; `ProfileData` is set only for the profile's own top-level runs (S2).
  The hide rule is what decides, so neither a writable path inside `W/.jad` nor a
  `{profile_data}` entry can open another profile's folder. `ensureProfileData` makes it (0700,
  with its parents) before a bwrap or seatbelt start and refuses the call when any part below
  the real `W/.jad` is a link or not a folder.
- Missing roots (2c, decider F1): `ensureProfileRoots` makes `W/.jad/profile-own` and
  `W/.jad/data/profiles` (0700, same link check) before every bwrap or seatbelt start of a v2
  sandbox, whatever the run, so their hide rule always applies and no call can make another
  profile's folder in a missing root. Failing to make them refuses the call. A root whose
  `W/.jad` does not exist (a worktree's moved copy of the paths) is left out.
- S2, only the profile's own top-level runs: `ForPlanMode`, `callSandbox` for `AgentDepth > 0`,
  `NarrowSandbox` and `InheritSandbox` clear `ProfileData`. `NarrowSandbox` keeps the hidden
  folders of both and shows only folders both show (a `profile_message` target under a
  sandboxed caller sees neither's); `InheritSandbox` shows none and drops the caller's
  `{profile_data}` paths. A target's own runs (by the user, a trigger, an unsandboxed caller)
  write normally. A sandbox choice (never given `ProfileData`) and a worktree run
  (`DelegatedWorktree.Sandbox` clears it) do not write the data folder.
- `{profile_data}` is a sandbox path placeholder filled from `SandboxSettings.ProfileData` at
  each start (`sandboxedLaunch`, the reader mirror `environmentPolicy`) and from
  `SessionConfig.ProfileDataDir` in the narrowing places (`SandboxPlace.ProfileData`), so it
  names nothing wherever the data folder was cleared and the path is left out. In v1 it is
  refused at load (`refuseV1ProfileData`), as an unknown placeholder was.
- `profile config` reports `data_writable` and `data_access` (`ProfileDataAccess`): from the
  settings of this run (plan mode, the run's own environment, the sandbox), not from the host.
- S4, concurrency: the engine takes no lock on data files. The path holds the workspace and the
  profile ID, so nothing is shared across workspaces; sessions of one profile share the folder
  by design.
- Not changed: without a sandbox, `exec` reads and writes every profile's folders; seatbelt is
  not run on a Mac (its profile text is tested only).

## 13. Phase 3a as built

Decider phase 3 decisions S1 to S7; 3b (own documents) and 3c (own models and tools) follow.

- **S1, `profile_self_edit`:** a top-level key (`off`, `on`, `full`) of `G/global.json` and
  `workspace.json` (schemas updated). The effective level is the lower of the global one (`on`
  when unset) and the workspace's, so a workspace can lower it and never raise it; it is read
  once at start (`Layout.ProfileSelfEdit`, `SessionConfig.ProfileSelfEdit`). A profile or
  toolset file cannot carry it. `off` refuses `edit` and `revert`; `config` and `history` still
  work. `profile config` shows the level and `editable`.
- **S2, who may edit:** `edit` and `revert` are refused to sub-agents and toolset agents
  (`AgentDepth > 0`), to another profile or session, in plan mode (`PlanMode` or `PlanGuarded`),
  and in runs another run or server started (`ChainDepth > 0`, a `RunNode`, `RemoteHops > 0`);
  no `allow_*` flag and no `plan_mode.allow` entry lifts these. `history` is refused only to
  sub-agents and other profiles, like `config`. The owner is always the execution context's
  owner key. A remote path keeps its `profile_remote_denied`.
- **S3, revisions and writers:** `if_match` is required and compared with `CatalogETag`, a
  SHA-256 of the bytes (with or without its quotes; `*` is no revision). One in-process lock
  per profile file is shared by `edit`, `revert`, the model-default route and the catalog write
  and delete of profiles; a held lock answers a conflict at once, as a file changed meanwhile already did (409 on the
  model-default route, 412 on the catalog write and delete routes, `profile_edit_conflict` in
  the tool). The edit takes no session gate or run lock: it is
  called from inside the run that holds them. The file is written through a temporary file,
  fsync, a rename that refuses a file whose size or mtime changed, and an fsync of the folder.
- **S4, trial load:** the candidate is never written before it passes. The whole load of the
  caller's session with this profile selected runs with the candidate in an in-memory overlay
  of the catalog scans (`judgeScope` is the one place a load reads a profile file), so the
  profile checks, the ceilings, the private-document check, the toolsets, composition and the
  prompt documents all judge it; a failure is the selected profile's error.
- **S5, policy (old file against candidate, key by key):** at every level but `off`:
  `description`, `name`, `prompt_role`, `instructions`, `prompt_documents`, `models`,
  `model_default`, `toolset`, `tools`, `schema_version` (the load still checks them). Under
  `on` the settings a profile may change are the tuning ones: `compaction`,
  `front_compaction`, `max_parallel_tool_calls`, `tool_call_scheduling`, `max_tool_rounds`,
  `provider_retry`, `provider_stream`, `provider_network`, `image_resize`, `model_fallback`;
  every other setting (ceilings, `sandbox`, `budget`, `hooks`, `env_file`, `cwd`,
  `tool_storage_dir`, `plan_mode`, `session_files`, `statecontext`, `checkpoint`,
  `persistence`, `image_store`, `http_debug`, `project_instructions`) needs `full`, and under
  `full` the trial load still refuses passing a ceiling. A new prompt document passes D3 after
  links (documents the file already names are left as the user wrote them); an `env_file` in
  `G/auth` (settings or hooks) and a document there are refused at every level. The size limit
  is that of every catalog file. `revert` passes the same check against the current file.
- **S6, history:** `W/.jad/profile-own/<cat>/<name>/history/<seq>-<hash12>.json` (folders made
  0700 by the engine, any link on the way refused), holding the file as it was and the
  metadata (time, session, profile, call id, origin, action, old and new revision, changed
  keys, level, the reverted entry). It is written and fsynced before the change is published.
  Only self-edits and reverts add entries; admin writes and hand edits do not, which is why
  `revert` needs `if_match`. No pruning. The tool result (old and new revision, changed keys,
  history id) is the audit record in the transcript; compaction can remove it, the history file
  stays. With a sandbox the history is read-only to the profile's own calls (sealed `W/.jad`)
  and hidden from other profiles (hidden `profile-own`), tested in a real bwrap run; without
  one `exec` can change it.
- **S7, running runs:** a run keeps the settings its `Resolve` loaded (`boundRunner.config` is
  used by `prepareRun`, compaction, sub-agents and model fallback; no code path reloads the
  profile file during a run), so an edit applies from the next run.
- Workspace layout only: in v1 the actions answer that self-edit needs `workspace.json`; no v1
  path or golden changed.

## 14. Phase 3b as built

Decider phase 3b D1 to D7: a profile writes its own documents through the `profile` tool.

- **D1, shape:** `edit` takes `contents` (now optional), `documents`, or both; each item is
  `{path, content, if_match}` to create (no `if_match`) or replace, or `{path, delete: true,
  if_match}`. `content` is taken as the model wrote it, never trimmed. One call is one revision
  and one history entry, so a new document and the profile reference to it arrive together.
  `config` lists `documents_dir` and each own document (`path`, `size`, `revision`); `history`
  lists the documents each entry touched (path, change, old and new revision; no bytes).
- **D2, where:** only `W/.jad/profile-own/<cat>/<name>/documents`, at `on` and `full`; `off`
  refuses. A path is relative with `/` between folders; no empty, `.`, `..` or dot-leading
  segment, no backslash or NUL. Every folder from the real `W/.jad` down is walked one part at
  a time (`ownFolder`, the walk the history uses), refusing a link or a file, and made 0700 when
  missing; an existing target must be a regular file, not a link. Each document and all of
  one edit's new contents are capped at `maxPromptDocumentBytes` (8 MiB; there is no tool
  argument cap). Sandboxed `exec` cannot write the folder (`sandboxProfileRules` keeps the own
  folder read-only); no admin route writes it.
- **D3, revisions and lock:** the profile file's `if_match` is required for every edit, also a
  document-only one; each document has its own (its `CatalogETag`; none means "must not
  exist"). The profile file's lock (`lockProfileFile`) covers the documents, so edit, revert
  and the 3a writers stay serialized with no second lock. All preconditions are checked under
  the lock before anything is written; a mismatch is a precondition failure listing every
  document. **Writers outside the tool:** the phase 1 audit found none that takes a model path,
  but a self-edit could point a setting (`tool_storage_dir`, `session_files`, a sandbox
  `writable_paths`, ...) into `profile-own`, after which an in-process writer or a sandbox bind
  would reach the documents and the history around the lock. A self-edit is therefore refused,
  at every level, when any string of a changed setting resolves into a `profile-own` folder of
  any known workspace (`checkSelfEditSettingPaths`; `{profile_own}`, `{workspace}` and `~`
  expanded, a relative string judged from the session cwd and from `W/.jad`).
- **D4, order and trial:** preconditions, paths and sizes, the S5 policy for the profile file
  (only when it changes; document contents are allowed like instructions), then the trial load
  with an overlay: the candidate profile file (3a) and every changed document, deleted ones
  missing (`Location.documents`, read by `readPromptDocument` for the profile's and session's
  prompt documents, agent toolsets included). Deleting a document still named fails the trial.
  The other document readers (statecontext triggers, sub-agent prompt files, project
  instructions) are not routed through the overlay; if the trial configuration shows one of
  them read a document of the change, the edit is refused (fail closed). A failure writes
  nothing and keeps no history.
- **D5, history:** entries get `version: 2`; `previous` (the profile file) only when it
  changed; `documents` with path, change, old and new revision and the old bytes (base64).
  3a entries (no version) still decode, list and revert. An entry above the read limit
  (8 × the JSON file cap) is refused before anything is written, so every entry can be read
  back.
- **D6, publish:** new and changed documents (each through a temporary file, fsync, and an
  exclusive link or a rename that refuses a changed file), then the profile file, then deletes
  (only of the file the check saw), then an fsync of every touched folder. A failure after
  publishing began undoes what was published newest first and returns an error naming the
  history entry and whether the undo finished. Not crash-atomic across files: a crash or a
  failed undo between the steps can leave new documents with the old profile file; a running
  run is never affected (S7: prompt documents are read when the run's settings load). Since
  3c.3b a session load that runs between the steps of this process's publish reads again
  instead of seeing the mix (section 22).
- **D7, revert:** restores exactly what the entry touched: the profile file when it changed
  and each document to its old bytes or to absent. Each touched document must now be at the
  entry's new revision or already at its old one (left alone, which covers a partly published
  edit); anything else is a conflict listing the documents, and nothing changed later is
  overwritten. Revert passes the same policy, trial, history and publish order.
- **Fixed on the way:** an edit or revert that removes the `prompt_documents` key was refused
  in 3a ("must be an array of strings").
- With a sandbox on, the own `exec` cannot create, change, delete or move documents and other
  profiles cannot see them (real bwrap test); without one nothing here is private or
  tamper-proof, and the link checks race with `exec`.

## 15. Phase 3b.1 as built: where configured writers may write

Decider 3b.1: the rule that no configured in-process writer reaches a `profile-own` folder is
checked where the loader resolves the write targets, not on the setting's text.

- **One point:** after the selected profile's configuration and its agent toolsets are loaded
  (`loadLayoutSessionSettings`), `Layout.checkWriteTargets` takes every in-process write target
  of the resolved `SessionConfig` (`SessionConfig.writeTargets`): transcript, backup, caches,
  staging, HTTP debug, tool storage (`tool_storage_dir`), the `storage_dir` of task, records,
  code_context and subagent, the subagent worktree folder, the spill folder and the folders of
  the `stdout_full`/`stderr_full` files of exec and tool_smith, `image_store.dir`, and the
  `functions_dir` of exec and tool_smith (relative from the cwd, as a top-level run resolves
  it). Each is taken as the loader resolved it (placeholders, relative base, `Clean`), then the
  links of its existing part are followed; one inside the `profile-own` folder of any known
  workspace (as written or after links) fails the load with the setting's name.
- **Who meets it:** a self-edit through its trial load (so a toolset switch whose storage
  points there is refused too; the text check of 3b stays as the earlier, clearer refusal) and
  every load of a hand-written profile, session, workspace default or toolset. v1 has no
  `profile-own`, so nothing changes there.
- **Not a write target:** `session_files` is the read and listing policy of the UI file list
  and the transcript tools; it writes nothing and is still covered by the 3b text check for
  self-edits. `env_file`, MCP `config_path` and agent folders are read, not written.
- **Sandbox writable paths:** a listed writable path into the own documents, another profile's
  documents, the `profile-own` root, through `..` or a link, or the whole `W/.jad`, opens
  nothing: `sandboxProfileRules` are bound after the writable paths and hide or keep read-only
  as before (real bwrap test). No bind order change was needed.
- **Limits:** a link made after the load (by `exec` without a sandbox) is not seen; hooks and
  MCP servers run as their own processes and are not in-process writers. Worktree runs and
  checkpoint restore were open here and are closed by 3b.2 (section 16).

## 16. Phase 3b.2 as built: checkpoints and worktree runs

Decider 3b.2 closes the two 3b.1 gaps that could change what self-edit owns without its lock and
history.

- **What self-edit owns** (`SessionConfig.SelfEditPaths`, v2 only, from what the writer
  writes): the `profile-own` folder of every known workspace (own documents and the edit
  history) and the workspace's profile catalog `W/.jad/profiles` (every profile file there is
  edited by its own profile's self-edit). `workspace.json`, toolsets, models and sessions are
  not in it, so the decider's stop condition did not apply.
- **Checkpoints leave it out:** the paths are checkpoint harness paths (excluded from new
  snapshots, also under the name the workspace gives them when it is reached through a link),
  each snapshot first drops them from the snapshot index (an exclusion alone keeps only
  untracked files out), and a restore drops every planned path at, below or above them (a
  file or link that would replace the folder) in both modes, also from a checkpoint taken
  before they were left out. The result's warning says how many paths were left. A self-edit is
  undone with the profile tool's `revert`; this is a deliberate change of what v2 checkpoints
  keep. v1 has no `SelfEditPaths` and is unchanged.
- **Worktree runs:** they do not resolve their configuration again; they move the run's
  workspace root to the worktree and each tool resolves its relative targets per call. The two
  per-call resolvers are the guarded points: `planBashSpill` (the spill folder and the
  `stdout_full`/`stderr_full` files of exec and tool_smith, one call site) and
  `resolveToolSmithFunctionsDir` (every reader and writer of the function library). The load
  gives exec and tool_smith (and each agent toolset's) `NativeBashSettings.ProfileOwnRoots`;
  a target there after links (through `..` or a link committed in the repository) fails that
  call, naming the setting, before the command runs or a spill folder is pruned or written
  (`write_target_refused`); a refused library is treated like a missing one. The session is
  not failed. A normal worktree run is unchanged.
- **Limits:** an overlay run's paths are judged on the host path, not through its holder; the
  links race after the check stays (not inside bwrap).

## 17. Phase 3c.1 as built: a profile's own models

Decider 3c C1, C2 and C4, stage 1: own models written by hand. Self-edit of them (3c.2) and own
toolsets (3c.3) followed (sections 19 and 21).

- **File and format:** `W/.jad/profile-own/<cat>/<name>/models/<model cat>/<model name>.json`
  (schema `sample/schemes/profile-model.schema.json`) holds `schema_version`, `based_on` (a
  shared model with its scope, `global:x/y` or `workspace:x/y`; never `profile:`) and `set`,
  the keys it changes (3c.1b: one key table, `ownModelSet`, shared with 3c.2). `name`,
  `efforts` and `prompt_cache_key` replace the base's value; `request`, `anthropic` and
  `chat_completions` are merged into the base's object key by key, objects recursively, so a
  key the own model does not name keeps the base's value (a base's `store: false` stays) and
  none can be removed (null is refused). Nested allow-lists: `request` takes `reasoning`
  (`effort`, `summary`), `max_output_tokens`, `temperature`, `top_p`, `parallel_tool_calls`,
  `truncation` and `text` (`verbosity` only); `anthropic` takes `default_max_tokens`, `cache`
  (`automatic`, `tools`, `system`, `history`, `ttl`) and `default_profile.effort_budget`;
  `chat_completions` takes `request_timeout_ms` and `system_role`. Refused by name with the
  reason, also under `full`: `base_url`, `api_key_env`, `provider`, `api`, `model`,
  `auth_mode`, `oauth_auth_file`; in `request` the runner's keys, `service_tier` (price the
  budget does not count), `store` and `background` (kept at the provider),
  `previous_response_id`, `conversation`, `prompt` (other account content), `metadata`, `user`,
  `safety_identifier` (attribution) and `prompt_cache_key`; in the adapter blocks `upstream`
  (endpoint, auth header, beta, extra headers), `log` (could write the key into a log),
  `models`, `extra_headers` and `extra_body` (also in `default_profile`). Every other key at
  every level (`pricing`, `hosted_tools`, keys added later) is refused by the allow-lists, and
  each value's JSON kind is checked; the composed model then passes the whole model rules
  (the adapter block through `ParseEmbeddedConfig` or `ParseConfig`), and a base that fails
  them is not repaired by a set. Shared model files keep their full freedom. Nothing of the endpoint is stored in `profile-own`: a
  run reads the base each time, so an edit of the base reaches the own model, and a
  `based_on` changed after the load fails the run until the session is loaded again.
- **Loading:** the loader reads the own folder walked from the real `W/.jad` with every
  folder checked (a link is refused, as for history and documents); a linked category folder
  or file is not used. An own file that fails (its rules, a refused key, a missing or
  failing base, a link) stays in the profile's catalog and fails like a model file that
  fails the model rules: it is left out of what the profile offers, with a warning in that
  profile's load only. In the workspace layout a model file that sets `based_on` and is
  read as a whole model (a shared model, a sub-agent's model path, an own file reached any
  other way) is a load error, so an own file never runs without its base; a sub-agent that
  names an own file by its path runs it whole (`SessionConfig.WithAgentModel` drops it from
  the bases), so it fails. v1 ignores a stray `based_on`, as before phase 3c
  (`SessionConfig.WorkspaceLayout`; the sub-agent call's effort check reads under the v1 rule
  and the run's model load applies the layout's).
- **Scopes:** `profile` is a third scope prefix (`catalog.ScopePrefixes`). The shared catalogs
  take only the global and workspace folders; a third folder is an error (the old clamp
  named it `workspace`). Each profile is judged with its own catalog, the shared models with
  its own as the `profile` scope (`Layout.profileModels`), for its `models`, `model_default`,
  `model_fallback`, the model-default route and the catalog route's profile check. An own ID
  that is also a shared ID is ambiguous in that profile only and needs a prefix; other
  profiles still see the shared one. The offered list keeps bare IDs, as for a
  prefix-picked shared model; the listing marks an own model with `scope: "profile"`, and the
  model drawer shows a "profile" label beside its ID. A profile that offers `profile:x` and
  `global:x` together is refused (`Narrow`), so within a profile a bare ID names one model.
- **Visibility:** sub-agents (a `profile:` model or toolset is refused by name, the model
  choice offers shared models only), another profile, a `profile_message` caller naming a
  model (refused when the target would run its own model) and the catalog routes do not see
  own models. A sub-agent without a model of its own inherits the run's model, own or not,
  and the session's `model_fallback` list, as before.
- **Deletion guard:** deleting a model is refused while an own model of a profile of a served
  workspace is based on that file (`dev/coder (its own model profile:x/y is based on it)`),
  and each profile's use is judged with its own catalog. A workspace the server does not
  serve is not checked; its own model is then left out with a warning at its next load.
- **Other:** the session version and checkpoint harness paths include the base of an own
  model; the run reads own models through `SessionConfig.ModelBases`.
- **Tested:** composition and following a base edit, every refused and unknown key (same
  `base_url` with another `api_key_env` included), missing and broken bases, links,
  ID clashes, fallback and sub-agent refusals, deletion guard, schema agreement, listing,
  and a bwrap run whose own exec cannot write `profile-own/.../models` while another profile
  cannot read it.
- **Limits:** load warnings name own model files and reasons; they go to the server log only.
  An error about an own model file is kept from other profiles since 3c.2a (section 18).
  `jad check` checks own model files since 3c.2 (section 19).

## 18. Phase 3c.2a as built: own model errors and other profiles

An error about one of a profile's own model files names a file under `profile-own` and why
it fails. The sandbox hides `profile-own` from other profiles, so another profile may not read
that text either (decider 3c.2a).

- **Marked errors:** `sessionfs.OwnModelError` wraps such an error with its text whole, so a
  `ConfigError` or `ClientError` is still one. It marks:
  - an own file whose base cannot be resolved (`ownModels.faults`);
  - a composition or model-rule failure of an own file (`loadModelOf`);
  - an effort the own model does not offer (`markOwnEffort`, at load, in `WithModel`, for
    `model_default` and `model_fallback`);
  - "no model it lists can be used" when an own model is among them.

  `mapSessionError` keeps the mark through `httpapi.StatusError.Cause`, which is never sent.
- **Where other profiles meet them:**
  - `profile_message`: a model the call names that is one of the target's own is refused
    before any run with the existing own-model refusal, also when that model cannot be
    loaded (it was started and failed) or does not offer the effort (its text, with the own
    efforts and the base's model name, was passed on). A target whose own default cannot be
    loaded fails its run; the caller reads the run's generic failure line
    (`clientRunErrorMessage`), which never held the detail.
  - `profile` tool `compact`, `reset`, `info` of another profile: an own model error becomes
    "profile X: its selected model could not be loaded" (`otherProfileError`). Before,
    `compact` passed the model fault's text, path and reason, to the caller.
  - Remote callers: the remote API sends a generic line for every backend error, and a remote
    run's failure is the generic run line. Nothing changed.
- **Kept whole:** the profile's own calls (same profile ID, any session of the workspace),
  its session listing (`model_fault`), and the server log.
- **Not redacted:** what the target's model writes in its answer. A target that reads its own
  files can repeat them; that is the target's choice, not an error path.

## 19. Phase 3c.2 as built: self-edit of a profile's own models

Decider 3c C3 with the 3c.1b key table: `edit` writes the profile's own model files through
the same pipeline as the profile file and the documents (sections 13 and 14).

- **Argument:** `models: [{id, content | delete, if_match}]`. `id` is `<category>/<name>`
  (catalog identifiers, so no `/`, `..` or leading dot); `content` is the own model file as a
  JSON object, written with two spaces of indent and a final newline. `if_match` follows the
  document rule: required to replace or delete, left out to create. The profile file's
  `if_match` is required for every edit. `config` lists `models_dir` and `models` (ID, size,
  revision).
- **Where:** only `{profile_own}/models/<category>/<name>.json`, walked from the real `W/.jad`
  with links refused (the document walk, now one `ownFileKind` table for documents and models),
  folders made 0700, an existing target a regular file, each file and all of one edit at most
  4 MiB.
- **Rules, the same at `on` and `full`:**
  - the own model file rules of section 17 (`ownModelKeys`, `schema_version`, `based_on` with
    `global:` or `workspace:`, `set` through `ownModelSet`), checked before the trial;
  - an ID no global or workspace model has (decider C2: self-edit never makes a clash; a
    hand-written clash keeps the prefix rules);
  - `off` refuses every edit.
- **Trial:** the trial memo also overlays own model files by profile and ID: a created file is
  added to the profile's own catalog, a deleted one removed, and a written one is read from
  memory (`ModelBase.trial`), so the whole selected-profile load judges the candidate. After
  the load:
  - each own model the change writes must be usable: its base found, the composed model passing
    the model rules (`ownModels.problem`);
  - the model a run of the profile would use must load with its effort (`ModelFault`, else
    `WithModel`). A fault that is the same before the change does not block it, so a profile
    whose base an admin broke can still change other things or repair it.
- **History and revert:** history entries are version 3 and hold the touched own models
  (`models`, with old bytes), beside `documents` and `previous`. Version 2 (3b) and 3a entries
  are still read and reverted. Revert restores each own model under the "old or new revision"
  rule and passes the same rules and trial. An older build does not read version 3 entries
  that hold only own models (T-016 rule: no downgrade).
- **Publish and lock:** the profile file's lock, the history entry first, writes, profile file,
  deletes, folder sync, and undo on failure, as in 3b. A deleted own model is removed after the
  profile file, so the old profile never loses a model it offers.
- **Not new write targets:** own model files are under `profile-own`, which is already in
  `SelfEditPaths` (checkpoints leave it alone, section 16) and closed to configured writers
  (section 15) and to sandboxed exec (read-only for the own run, hidden from others).
- **`jad check`:** reports, for each profile of the workspace, each own model file its load
  leaves out (skipped name, rule, base, composition), with the loader's reason.

## 20. Phase 3c.2b as built: a run keeps the own models it started with

Decider 3c.2b: a run never uses an own model edited after it started.

- **What re-read before:** the trace of every `WithModel` and model-read call reachable while a
  run goes found these readers of the run's own model files: the prompt_submit and
  session_start hooks' key variable (before the run loads its model), the run's model and its
  fallback plan (`prepareRun`, `planRunStreamer`), the key variables hidden from tools and
  hooks (`runModelKeys`, again at every hook event), a sub-agent that keeps the run's model
  (`childConfig`) and a remote sub-agent start. A fallback switch itself reads nothing: its
  client is built when the run prepares. So an edit could reach a run between those reads,
  give a sub-agent the new `set`, hide another key variable than the one a fallback client
  uses, or fail a later read with "load the session again".
- **The copy:** `SessionConfig.PinOwnModels` runs when a run starts (`runLocked`, and
  `remoteSubagentBase` for a remote sub-agent). It composes the run's model and each
  `model_fallback` model that is an own model with its base, while it holds the profile
  file's lock, so it reads one revision: an edit publishes under that lock, and one that fails
  halfway and is undone is never seen. The result is kept on the run's `ModelBases`
  (`ModelBase.pin`); `ownModelFields` returns it and reads neither the own file nor its base
  again. Effort is applied per load from the copy, as before.
- **Fail closed:** an own model of the profile that the run did not reach when it started has a
  pin that is an error ("this run did not read this own model when it started"), so no path
  reads it from the disk during the run. A copy that failed (a base gone, `based_on` changed
  since the load) fails each load that needs it, as the read did before.
- **What follows its files as before:** shared models and v1 are read from their files on each
  load, as today; a config with no own model is returned as it is and takes no lock. The next
  run loads the session again and takes a new copy.
- **Lock order:** a starting run holds its session's run lock and waits for the profile file's
  lock; no writer of the profile file waits for a run, so the wait ends when the writer does.
- **Limits:** the copy is of the own files at run start, while the profile file was read when
  the session was resolved, before the run waited for its session's run lock; an edit between the two is judged by the existing
  `based_on` check and the fail-closed missing file. Background work a run left going keeps
  that run's copy. One process only, as every lock here.
- **Shared ID added later:** when a shared model is added with an own model's ID, the bare ID is
  ambiguous for that profile and its load leaves the profile out (T-018); deleting the own model
  through `profile edit` (or naming it with `profile:`) repairs it.

## 21. Phase 3c.3 as built: a profile's own toolsets

Decider 3c C1 to C4 for toolsets, with the 3c.2b run rule. An own toolset is a file based on a
shared one, written by hand or through `edit`; it never holds anything the base's owner did not
already allow.

- **File and format:** `W/.jad/profile-own/<cat>/<name>/tools/<toolset cat>/<toolset name>.json`
  (`profile-toolset.schema.json`) with `schema_version`, `based_on` (`global:<id>` or
  `workspace:<id>`, never `profile:`), `omit` and `set`. Every other key is refused.
- **What it may change:**
  - `omit.native`, `omit.extension`, `omit.mcp`: names of the base to leave out (narrowing).
  - `set.native.<tool>`, `set.extension.<tool>`: the tuning keys of a tool the base has, merged
    into the base's settings of that tool key by key; the tool's own rules judge the result, its
    ranges included. A value is a string, number or boolean, never null.
- **What it never changes, at every `profile_self_edit` level, also under `full`, and in files
  written by hand:** the hooks, the MCP connections (only `omit`), and every tool key that names a
  path, a command, an environment or secret, an endpoint, credential, provider or model, what a
  tool can reach (other sessions, profiles, remotes, agents, tools, domains), a limit on what a
  tool may start, keep or store, or how the engine runs it. `ownToolsetTools` classifies every key
  of every native and extension tool (tuning, or refused with its reason);
  `TestOwnToolsetTableClassifiesEveryToolKey` fails when a tool or key is added and not
  classified. This is stricter than decider C1, which allowed hooks, `env_file`, `cwd`, command
  definitions and ceiling keys under `full`: those stay in the profile file's settings, where
  `full` already allows hooks, `env_file` and `cwd`; tool limits and paths are the user's. An own
  toolset adds no tool, server or hook.
- **Base paths:** a global base has its relative paths made absolute from G first
  (`absoluteGlobalToolsetPaths`), a workspace base keeps reading them from `W/.jad`, so the own
  file's place changes nothing; it cannot set a path of its own.
- **Scope and visibility:** the profile's toolset list takes `profile:<id>` (schema and loader);
  `Layout.profileToolsets` is the shared toolset catalog plus the profile's own as the `profile`
  scope, used for that profile's own toolset list only (load, `judgeScope`, the composition
  check, the default's tool-setting check, the catalog route's profile validation). Agent toolset
  lists read the shared catalog only and refuse `profile:` by name; another profile's
  `profile:` names its own (empty) scope; the catalog routes never serve own toolsets. A bare
  ID that a hand-written own toolset shares with a shared one is ambiguous for that profile only
  (it is left out); self-edit refuses such an ID.
- **Bad files:** a file that breaks the rules, whose base is missing or that does not compose
  leaves the profile out of the session with a warning, and selecting it fails with an error
  marked `OwnToolsetError` (so is a composition error of a toolset list holding an own toolset).
  A load passes over a name the own toolset omits or sets that its base no longer has (an edit of
  the base dropped it); nothing is added. An edit's trial and `jad check` refuse such a name.
- **Self-edit:** `edit` takes `toolsets: [{id, content | delete, if_match}]` beside `contents`,
  `documents` and `models`; one call is one revision under the profile file's lock, with the
  same checks, publish order, rollback and revert as own models (`ownToolsetKind`). Before the
  trial each written toolset must pass the file rules and take an ID no shared toolset has; the
  trial overlays the written own toolsets, the profile's load applies those it names, and each
  written one must compose strictly with its base and pass the toolset rules
  (`validateLayoutToolset`) whether the profile names it yet or not. History entries are version
  4 with `toolsets`; version 3, 2 and 3a entries still read and revert. `config` lists the own
  toolsets with their revisions (`toolsets_dir`, `toolsets`).
- **Other profiles:** an own toolset error (load, compact, reset, info) reaches a caller of
  another profile only as "profile X: one of its own toolsets could not be loaded"
  (`otherProfileError`; `mapSessionError` keeps the mark). A `profile_message` to a profile whose
  own toolset fails is refused before any run with the tool's generic "Profile X failed".
- **Runs:** a run's tools, servers and hooks come from the settings its session load composed; no
  path reads a toolset file again while the run goes (traced: the run keeps its loaded config,
  sub-agents use the toolsets prepared at load, the MCP servers start from it). An edit applies
  from the next run (`TestARunKeepsTheOwnToolsetItStartedWith`).
- **Guards:** own toolset files are under `profile-own`, so they are in the checkpoint exclusion
  (`SelfEditPaths`), read-only to the profile's own sandboxed exec and hidden from other profiles
  under bwrap (`TestProfileSandboxKeepsTheOwnToolsetsReadOnlyAndPrivate`). An own toolset sets no
  write target; the write targets of the composed config are judged at load as before
  (`checkWriteTargets`). The toolset deletion guard counts a profile one of whose own toolsets is
  based on the file (a global delete checks every served workspace, as for models); MCP and
  sandbox guards need nothing, since an own toolset adds no server or sandbox.
- **Limits:**
  - **One load, several files:** closed by 3c.3b (section 22): a load that runs while an edit
    publishes reads again once the publish ended, without taking the profile file's lock.
  - **Stricter than C1:** no own toolset key opens anything under `full`; a need for one is a
    decider question.
  - **Deletion guard over served workspaces only:** a base used by an own toolset of a workspace
    this server does not serve is not counted; that own toolset fails closed at its next load.
  - **A new own tools folder is not stamped when it is missing:** like own models, a list cached
    before the first own toolset exists notices it through the profile file's change.

## 22. Phase 3c.3b as built: a session load reads one revision of each profile

Decider 3c.3b. A self-edit publishes a profile's own files (documents, models, toolsets), then
its profile file, then its deletes, one rename at a time. A session load that ran in between
could pair the old profile file with new own files, a combination no trial judged. A lock in the
load could deadlock (the trial and the catalog checks load under a writer's lock), so the load
checks instead:

- **Publish counter:** `publishTracker` (one per process) raises a generation when a publish of a
  profile file begins and again when it ends, after the last rename or after the undo
  (`publishProfileEdit`, under the profile file's lock). Each profile file keeps the generation
  of its last change and, while a publish runs, a channel closed when it ends.
- **One wrapper:** every own file a load reads (each judged profile's file, own models and own
  toolsets, and the selected profile's own documents) is read inside one
  `loadLayoutSessionOnce`. `loadLayoutSessionSettings` notes the generation when its memo is
  made, before anything is read (a memo shared across loads of a request is made earlier still),
  and after the load asks whether any profile it judged has a publish running or one that began
  or ended since. If so it waits for the running publishes, then loads again from a new memo
  (the old one may hold files of the old revision); after three loads
  (`profileLoadAttempts`) it fails with a `ConflictError` "the profile file ... is being edited;
  try again" and never returns a config of two revisions. A load's error is retried the same
  way, since a mix can be its cause. A recording view gets the stamps of the load it returns.
- **Exempt:** v1 (no self-edit, nothing publishes) and a trial load: it reads its own profile
  from the candidate under the writer's lock before the writer publishes, so its own counter is
  never raised, and a retry from a new memo would lose its overlay. The catalog checks run under
  a writer's lock do not go through the session load. A load waits only for a publish that is
  running, and a publish waits for no load, so the wait cannot deadlock.
- **Cost:** `BenchmarkLoadWorkspaceSessionConfig` (one own model and one own toolset, each
  based on a global one) takes about 2.2 ms and 7150 allocations per settings load, with or
  without the check (the check adds about 4 allocations); the v1
  `BenchmarkLoadSessionConfig` is unchanged at about 224 µs.
- **Limits:**
  - **One process:** like the profile file's lock, the counter sees the publishes of this
    process only; a hand edit or another process is not seen.
  - **After the load:** `WithModel` (in `LoadSessionConfig`) and the run-start copy
    (`PinOwnModels`, section 20) read own model files again after the checked load; an edit in
    between pairs the loaded profile file with a newer own model, which the `based_on` check or a
    missing file catches only in part (the 3c.2b gap, unchanged).
  - **Trial loads:** a trial is not checked, so a publish of another profile in the session's
    scope during the trial can make that profile's verdict in the trial one of two revisions;
    the edited profile is read from the candidate.

## 23. Phase 4a as built: the migration planner (dry run)

`jad migrate workspace [--root NAME] [--json]` (`cmd/jad/migrate.go`) plans the migration of the
one v1 setting the exact current folder W loads today and **writes nothing**: no file, folder,
lock or temporary (pinned by a before-and-after snapshot of the whole temporary tree, including G
and `$HOME`). `--apply` (phase 4b, section 24) writes. A blocked plan exits 1. The planner is
`sessionfs.PlanWorkspaceMigration` (`migrate_plan.go`); it reads v1 through the v1 loader
(`Load`, the root's `Layout`, `LoadLayoutSessionSettings`), never from assumed defaults.

**Blockers** (the plan stops instead of guessing):

- `workspace_exists`: `W/.jad/workspace.json` exists; `no_v1_setting`; `v1_setting_invalid`.
- `global_folder_place`: G is `W/.jad` or inside W.
- `path_base_changes`: the v1 setting is not in `W/.jad`. Relative paths of sessions, profiles,
  toolsets and defaults resolve from the v1 setting's folder in v1 and from `W/.jad` in v2, and
  the loader resolves them in each tool's normalizer with no one list of path keys (the decider's
  M3 stop condition). The planner therefore plans only a setting at `W/.jad/setting.json`, where
  both bases are the same folder and no workspace-side path moves.
- `unknown_key`: a top-level or `libraries` key the migration does not map.
- `root_required` / `unknown_root`: several session roots and no (or an unknown) `--root`; every
  root is listed with its sessions, and the sessions of other roots are listed as not migrated.
- `name_clash`: a destination in W or G holds other content; the same content is `same` (skipped).
- `ambiguous_id`: a model or toolset ID would exist in G and in `W/.jad/models|tools`, so every
  bare reference would be ambiguous in v2.
- `global_json_conflict`: `G/global.json` exists and its `server`, `cli` or global library keys
  differ from v1's (compared as JSON after the path rewrites; a relative path that means the same
  file is still a conflict, which fails safe); the migration never edits `global.json`.
- `symlink`: a link at or below a source folder; links are never followed or copied.
- `forbidden_place` (4c, replaces `storage_outside`): a write target of a migrated session's profile
  (`SessionConfig.writeTargets`, also of agent toolsets, from the v1 load) outside `<v1 state>/<session>`
  lies in G, in the `.jad` of another workspace the existing `global.json` lists, or in this
  workspace's `profile-own` folder, where v2 keeps no workspace storage (the write-target guard
  refuses the last). A target outside the copied state anywhere else is *shared storage*, not a
  blocker (section 25). The same kind stops a v1 tool definition path in `W/.jad/profile-own`,
  `W/.jad/data`, `W/.jad/profiles` or `G/auth`.
- `sandbox_catalog_cwd`: `catalogs.sandbox` is `{cwd}/...`; `unreadable`.
- `tied_to_workspace` (4a.1, decider phase 4): a path in `G/global.json` (`server.log_file`,
  `server.remote_api.tokens_file`, from the rewritten or the absolute v1 value) or in a remote's
  file that G receives (`remote.token_file`) names a place inside W. G is read by every workspace,
  so it would be tied to this one. None of these keys exists in `workspace.json`, and the migration
  never adds a key on its own, so the plan blocks, naming the key (one blocker per key, with the
  first value and the count); the person changes the v1 setting so the path names a place
  outside the workspace. A path outside W stays absolute in G. The test is the whole workspace W,
  one step stricter than W/.jad. A shared toolset, MCP server or sandbox template that names a
  place in W is only warned (a single shared file, decider M3). **`libraries.tool_definition_paths`
  is no longer in this list since 4c: a path in W goes to `workspace.json` (section 25).**

**Where things go** (each file is `copy` with size and sha256, `in_place`, or `same`):

- **Sessions** of the root into `W/.jad/sessions`, **profiles** into `W/.jad/profiles`, **state**
  `<v1 state>/<session>/...` into `W/.jad/state/<session>/...`. v1 (catalog layout) and v2 key a
  profile's state the same way, `<state>/<session>/<category>/<name>` (`profileStateDir`; the
  legacy `profiles/<key>` form of `selectProfileStorageDir` is not reachable for a layout
  session), so the mapping is one to one and the "state key cannot be mapped" stop condition
  cannot occur. Lock files and `<state>/.mcp-oauth` (MCP sign-ins, credentials) are not copied.
  A folder already where v2 reads it is `in_place` (this repository: everything in `W/.jad`).
- **mcp, remotes, sandbox** into G (`global.json` catalogs or `G/<kind>`), listed as additions
  every workspace sees.
- **models and tools** into G, except when the v1 folder already is `W/.jad/models|tools`: v2
  reads that folder as the workspace's own catalog, so the files stay `in_place`,
  workspace-private, with a warning (a G copy would make every bare ID ambiguous; the decider's
  same-content rule). A model or toolset that `W/.jad` already holds with the same content is
  not copied to G either.
- **workspace.json**: `schema_version` 2, `id` and `label` of the v1 root (so `root:session`
  references keep their meaning), the v1 `defaults` unchanged (plus the pin of section 25), the
  four library folder keys (each v1 fallback folder written out, since v2 has no workspace
  fallback) and, since 4c, the v1 `tool_definition_paths` that name a place in W.
- **global.json** (only when missing): v1 `server`, `cli`, the other `libraries.tool_definition_paths` and
  `libraries.max_asset_bytes`.

**Path rewrites** (each names the place it named in v1, recorded with key, old and new value):
a moved toolset through `absoluteGlobalToolsetPaths` with the v1 base (the loader's own list for
a global toolset: `env_file` of exec and tool_smith, the path rules of exec, read_image, lsp and
code_context, `hooks.env_file`); a remote's `token_file` from its v1 folder; `server.log_file`,
`server.remote_api.tokens_file` and `libraries.tool_definition_paths` from the v1 setting's
folder. Model, MCP and sandbox files have no path the move changes. `defaults.cwd` is copied as is: v1 requires an
absolute (or `~`) cwd, so the decider's `"."` example cannot occur.

**Secrets**: keys that look like secrets (key, token, secret, password, authorization, credential,
cookie; every value inside `env` or `headers`) are named by file and key, never by value. `_env`,
`_file`, `*tokens*` and `*cache_key` keys are not named (variable names, paths, counts). State
files (transcripts) are not scanned.

On this repository (read-only run, 2026-11-12, after 4a.1) the plan was blocked by one
`tied_to_workspace` blocker for the 16 tool definition paths; 4c removed that blocker (section 25).

## 24. Phase 4b as built: `--apply`, the journal and `--rollback`

`jad migrate workspace --apply` plans again (section 23), refuses a plan with blockers (nothing
is written, exit 1) and otherwise applies it. `ApplyWorkspaceMigration` is in
`sessionfs/migrate_apply.go`, the v1 and v2 comparison in `migrate_compare.go`. It was run only in
temporary workspaces, G and `$HOME`; it was never applied to this repository's data.

**What it writes.** Only what the plan lists as `copy` or `create`: files of `G` (mcp, remotes,
sandbox templates, models and tools that are not already where v2 reads them, `global.json` when it
is missing), sessions, profiles and state copied into `W/.jad`, and `workspace.json`. Folders it makes
are mode 0700, settings files 0600, copies keep the source's permission bits and modification time.
Rewritten toolsets and remote files are rebuilt from the source bytes (`globalCandidate`); every other
copy is byte for byte. **No source file is changed, replaced or deleted.** The only things written
next to a source are the lock files jad makes itself (`.messages.jsonl.jad-lock`, below).

**Publish without replacing.** A file is written to `.migrate-<run>-<n>.tmp` in its destination
folder (hashed as it is written, synced), then hard-linked to its name: a destination that exists, or
that appears between the check and the link, is an error and keeps its content (the same primitive as
`createFileExclusive`, but streamed). A folder on the way is made one part at a time and a link or a
file in the way is refused. A filesystem without hard links fails closed.

**Order.** (1) the session and transcript locks (`sessionlock.TryAcquire`, no waiting) for every
copied session file and `messages.jsonl`: a running jad that holds one stops the migration before
anything is written; (2) the v1 loads of every migrated session and profile, taken while `W` is
still v1; (3) the journal; (4) additions to G; (5) workspace files; (6) `global.json`; (7) **verify**: every
created file is hashed again against the hash written, every copied source is hashed again against the
hash the plan read (a source that changed while it was copied stops the run); (8) **`workspace.json`,
the switch**; (9) the comparison; (10) `complete`.

**The journal** `W/.jad/migrations/<run>.jsonl` is append-only and records, with a timestamp: `start`
(with W and G), each folder made (`mkdir`), each `temp` file, each `create` (path, temporary file, sha256,
size; synced before the link), `done` after the link, `switch`, `complete` or `failed`, and what a
cleanup `removed` or `kept`. It holds paths and hashes, never contents. It is the manifest of the run
and is never removed.

**Cleanup** (on any failure after the first write, and `--rollback <journal>`) removes only what the
journal shows this run created, and only if it still has the recorded hash: a file counts as created
by this run when it has a `done` entry, or when its temporary file still is the same file (a crash
between the link and `done`); a file with neither is kept ("this run did not create it"), however
its content looks. A file whose content changed since is kept, a path outside the run's W and G is kept,
and a folder is removed only if this run made it and it is empty. `workspace.json` is removed first
(reverse order), which is the rollback to v1. Running it twice finds nothing more.

**The comparison** loads W as v2 (with `JAD_ROOT_SETTING_PATH` left out, since it would keep v1) and
compares, per migrated session and per profile of its scope, with the v1 loads: identity, profile
list, model offer (each model file by content hash), working folder, where the transcript, backup,
usage cache and staging are (relative to the session's state folder), every in-process write target
(`writeTargets`), tool definition paths, MCP servers (by content), which tools are on, and the server and
cli settings as JSON. Any difference rolls the run back, `workspace.json` first. It guards against a path or a
name that moved; it is not a proof that the two layouts behave alike (hooks, sandbox rules, prompts
and the model's behaviour are not compared). A session that does not load in v1 is compared as an
error on both sides. One consequence: an absolute `tool_storage_dir` (or any write target) into the
v1 state keeps its place in v2 while the copied transcripts move: the copy is then abandoned and
v1 and v2 write the same files; the comparison reports it and rolls back (a target outside the v1
state is shared storage and passes, section 25).

**Limits that are not hidden.**
- `workspace.json` exists, and the folder is v2 for any process that starts there, from the switch
  until the comparison ends (milliseconds on a small workspace). The decision was to write it last and
  compare after it; the only way to load v2 is through that file.
- The v1 state copy and the write targets under the cwd: since 4c these are shared storage, not
  a blocker (section 25). This repository's state is already in `W/.jad` (in place).
- A copy is not a live replica: a v1 run after the migration changes the v1 files only.
- Locks are advisory (flock): a process that does not lock is not stopped. Sources are re-hashed at the
  end, so a change is detected, not prevented.
- The journal is local; a different machine's run is not seen. The cleanup trusts hard links and
  inode identity of a local filesystem.
- Creating the lock files touches the v1 state folders (as any jad run does).
- Not done: moving models or tools into G (they stay workspace-private when already in `W/.jad`;
  a `promote` step is the person's decision), settings outside `W/.jad`, the `.jad/.jad` bug (T-029; fixed for v1 by T-035, see DECISIONS.md).

## 25. Phase 4c as built: workspace tool definitions and shared storage

Decider (phase 4 ruling, after 4b), built in 4c.

**`libraries.tool_definition_paths` in `workspace.json`.** Tool definitions in `W/.jad` are tools
private to the workspace, so v2 gets a place for them. The key sits in `workspace.json`'s
`libraries` next to the four folder keys (`sample/schemes/workspace.schema.json`). A relative
path is relative to `W/.jad` (the file's own folder), an absolute path is kept; an empty string,
a non-array or `null` fails the start. The loader reads the global list first (G/global.json,
from G), then the workspace's own; the same file in both is read once. `Layout.WorkspaceToolDefinitions`
holds the workspace's own, `Layout.ToolDefinitionPaths` both. Another workspace sees only the
global list.
- **Duplicates:** the same tool ID in two files is the existing error, now naming both files
  (`<second>: duplicate tool definition <id> (also defined in <first>)`, `loadDefinitions`; v1's
  message gains the second file). There is no override.
- **Refused places** (`Layout.checkToolDefinitionPaths`, run for every served workspace at start,
  for both lists, as written and after links): the fixed hidden list (`G/auth` and the other
  workspaces' `.jad`), and the folders a profile writes (the `profile-own` folders, the `data`
  folder of every known workspace and this workspace's `profiles`). The decider named the
  hidden paths; the writable folders are added because a profile could otherwise change what its
  tools are. The check is made once at start: a link swapped later is not seen (the known
  links-race limit), and a workspace listed but not started has its list judged the same way (a bad
  path there fails the start, as a bad library folder does).
- **Sandbox:** every file of the workspace's own list is on the read-only list of every sandbox of
  the workspace (`sessionConfigPaths`), inside or outside `W/.jad`, so a writable path that covers
  it does not open it (real bwrap: change, delete and replace refused, the sibling file stays
  writable). They are not self-edit targets. The global list is not added: its files are in G
  (sealed) or outside, as in v1.
- **Process reads:** `loadDefinitions` reads the files with the process's permissions; the check
  above is what keeps that read away from the hidden folders. Sandboxed tool calls are the
  sandbox's business.

**Migration.** `splitToolDefinitions` sorts the v1 list: a path that names a place in W (lexically
or after links) goes to `workspace.json`: as written when it was relative (v1 and v2 both resolve it
from `W/.jad`, since the v1 setting must be in `W/.jad`), relative to `W/.jad` when it was an absolute
path there (reported as a rewrite), absolute for a place in W outside `.jad`; the others stay in G's list,
absolute from the v1 setting's folder. `server.log_file`, `server.remote_api.tokens_file` and a remote's
`token_file` still block when they name a place in W (`tied_to_workspace`): they are settings of the
shared server, not of a workspace. A `libraries` object in `global.json` is written only when
something is left for it. The comparison reads both layouts' tool definition lists sorted (v2 puts the
global ones first).

**Shared storage.** A write target outside `<v1 state>/<session>` (the default tool storage under
the working folder, the exec and tool_smith function libraries, an absolute `tool_storage_dir`) is
shared by v1 and v2, never copied: it holds work, not configuration, and v1 and v2 do not run in one
workspace at once by rule. The plan gives one note per setting (first path and count) and does not
stop, unless the target is in a place v2 refuses (`forbidden_place`). The comparison accepts a
target when both layouts follow the state at the same place, or when v1's lies outside its state folder
and both resolve the same absolute path (v2 may keep it inside its own state folder:
`sameWriteTargets`, with the absolute path printed beside the state-relative one). A v1 target inside
the v1 state with v2 pointing at the same absolute path is still a difference and rolls back: the
copy would be abandoned. After a rollback the shared storage keeps whatever v2 wrote; nothing is
deleted (the journal only removes files it shows this run created).

**Different defaults.** v2 layers an existing `G/global.json`'s `defaults` under the workspace's. If
those set `tool_storage_dir` and the v1 defaults do not, a session with no value of its own would write
elsewhere in v2; the migration then writes v1's default, `.jad/state` (under the session's working
folder), into the workspace defaults (a rewrite and a note). `tool_storage_dir` is the only default a
write target comes from. Any other default a global.json adds shows as a difference of the comparison
and rolls back. No key other than `libraries.tool_definition_paths` was added.

**v1 and v2 processes in one workspace (verified, not enforced).**
- The mode is chosen once at start by `.jad/workspace.json` in the start folder; a v1 process
  started before the switch stays v1 (the root setting is read once), a new one is v2, and
  `JAD_ROOT_SETTING_PATH` (or an explicit root setting path) keeps v1 after the switch.
- With the folders in place both resolve the same transcript, state folder and lock file, so the
  per-transcript flock excludes the other mode (`TestV1AndV2ProcessesInOneWorkspace...`). Copied
  state is different folders: a v1 process after the switch changes only the v1 copy.
- The profile files are shared when in place. A v2 self-edit that gives a profile a `profile:` model or
  toolset makes that profile fail to load in v1 (tested): rolling back to v1 after such an edit needs the
  profile file reverted by hand (the history is in `profile-own`).
- Process-local only: the profile-file lock, the publish counter and the pin of the own models are in
  one process. A v1 admin write of a profile file and a v2 self-edit in two processes are ordered only
  by the size-and-time check of `writeFileIfUnchanged`.
- A v1 process does not see `profile-own`, the checkpoint exclusions or the sealed sandbox.

**Not built:** the UI (phase 5), `promote`, seatbelt runs.

## 26. Phase 5 as built: the session rail groups by workspace

Decider (phase 5 ruling). "Session category" is gone from the UI; a session's **workspace** takes
its place. Nothing in the backend, the API, a session file or an id changed.

**What a workspace is in the UI.** The session root a session is stored in: `root_id` and
`root_label` of every list record. In the workspace layout each served workspace is one root, in
the old layout the roots of the setting are the workspaces, so one rule serves both. The UI shows
labels (a workspace's label defaults to its id, the old default root keeps "Local sessions") and
never a directory: `GET /api/workspaces` still sends `directory`, `parseWorkspaces` drops it.
`sessionRef` is unchanged (`root:name` with more than one root, the bare name with one), so stored
views and pinned panes keep working. A session's `category` is still accepted, listed and part of
the list ETag; the UI neither shows nor checks it (`session.schema.json` calls it legacy). The
profile `<category>/<name>` of a profile id is another thing and is untouched (fork and
duplicate of a profile still take it from the id).

**Rail.** With more than one workspace: a "Workspace" switcher on top and one heading per
workspace, the start workspace first, then the others by label, sessions in list order under their
heading, a heading with a running run shows the spinner. With one workspace: a flat list, no
heading and no switcher. "Others" and its config key are gone. A session whose root is not among
the known workspaces gets a heading of its own (an older server that names no root: "Unknown
workspace") instead of vanishing. A listed workspace the server does not serve has no section (it
can hold no session); the switcher shows it greyed out (section 27). Collapse state of a heading is per workspace and, as before,
not stored.

**The chosen workspace** is the switcher's pick and follows the session the user opens. It decides
where a new session is created and which workspace the catalog dialog and the library act on. It
is not stored: after a reload it follows the restored session, else the start workspace.

**Catalog dialog (`catalogScope`).** In the workspace layout every request carries an explicit
scope, so no file goes to the server's start workspace without the screen saying so:
profiles `workspace:<chosen>`; models and toolsets `global` or, with the "Files of" choice,
`workspace:<chosen>`; MCP servers `global`. Sessions carry no scope (their reference names the
workspace). Outside the layout no scope is sent (the server refuses one). The dialog does not list
or write anything until `GET /api/workspaces` answered; a failed read shows the error and a Retry,
and the lists are dropped whenever the workspace or the scope changes, so a row of one scope is
never opened or written under another. The workspace choice is locked while a file is open (a new
session may still change it: only its name depends on it).

**New session.** The name is created as `<chosen>:<name>` when more than one workspace exists
(`newSessionRef`), a name typed with its own root (`other:name`) is used as typed, and the line
under the name says where it will be created. The server enforces the rest: a bare name with
several workspaces is refused (`newLayoutSession`) and nothing is written, a qualified name lands in
that workspace only (`TestWorkspaceSessionsAreCreatedInTheNamedWorkspaceAndABareNameIsRefused`).
Fork and duplicate of a session stay in the source's root (`sessionRefInRootOf`), as before.

**Session catalog and profile filter.** The category column and filter of the session list became a
workspace column and filter (shown only when sessions span several workspaces); the profile list's
session filter is sectioned by workspace (a plain list with one). A session's category text is not
read there either.

**Library.** The prompt library panel and the composer's slash menu ask for the workspace of the
pane's session (`?workspace=`), in the workspace layout only; before this a pane of any workspace
read the start workspace's library. If the workspace list could not be read the UI cannot tell the
layouts apart and names the workspace anyway, so an old layout refuses the request loudly instead of
the start workspace's library being shown silently.

**Limits.**
- In the old layout the workspaces are read from the session list, so a root with no session is
  unknown to the UI: with one known root a new bare name is sent bare and the server refuses it when
  the setting has more roots (an error naming the fix, nothing written). Writing `<root>:<name>`
  works.
- Not stored: the chosen workspace and the collapse of a heading.
- Verified in a real browser against a temporary two-workspace server (rail, switcher, catalog
  scopes, session create, model scope choice, library panel); the layout was checked by eye in one
  small viewport only, and there is no automated rendering test: the frontend tests cover the pure
  rules, the request URLs and source-text wiring.
- A profile row whose profile the session does not offer is refused by the server (422) if a stale
  stored view names it; the UI does not clean such a view up.

## 27. Phase 5b as built: cleanup after phase 5

Decider phase 5b ruling (step 37.10 of the decider transcript).

**Bare session name with several workspaces.** The refusal now says how to fix it:
`name the workspace: write the session as <root>:<name>; served: a, b`. It lists root ids only,
never a directory (the ids are on every list record). The old layout with several roots and a root
that has no session is still unknown to the UI (workspaces come from the session list there);
making that layout list its roots would be new behaviour in the old layout, so it stays a limit and
this message is how the user gets out of it. Pinned by `TestNewLayoutSessionNeedsAnUnambiguousRoot`
and `TestWorkspaceSessionsAreCreatedInTheNamedWorkspaceAndABareNameIsRefused`.

**Unavailable workspaces.** `GET /api/workspaces` already marks a listed workspace the server does
not serve (`unavailable`). The rail switcher (`workspaceOptions`) now lists it after the served ones,
greyed out and not selectable, with "Not available (the server log says why)". It still has no
section and cannot be a target. The server's reason is **not** shown: it is an error text that names
files and folders (for example the path of its `workspace.json`), and the screen shows no path. The
catalog dialog's workspace choice lists served workspaces only. No backend change.

**Stored view and the 422.** A request for a profile the session's scope does not list fails in
`loadLayoutSessionSettings` (`judged.all.Lookup`) as a config error, which the HTTP layer answers with
422. That is old behaviour of the loader. The UI removes such a pinned row once the session list
names the profiles (`prunePinnedViews`, since 2026-10-18). Phase 5 did not change either, and the
workspace pick drives no session request (it feeds only the switcher, the new-session name and the
catalog dialog), so the pick cannot cause it. Left as it was.

**Tests that skip.** Three frontend tests (`every model/toolset/MCP server ... in the local layout`)
read `.jad/models`, `.jad/tools` and `.jad/mcp` of the checkout. In the workspace layout those kinds
are global (`G`, `~/.jad` by default), so the folders are absent and the tests skip; the skip reason
now says so. A unit test must not read `$HOME`; `jad check` judges the global files.

## 28. profile_message and the workspace layout (T-036)

The user said `profile_message` still addressed only a session. Decider ruling for T-036 (step 41.6
of the decider transcript), checked against the code before it was built.

**Local calls did not change in T-036; section 29 changes them for `profile_message` only.**
`<session>/<category>/<profile>` already goes through
`sessionfs.ResolveFrom`: in the workspace layout only the caller's own workspace is searched, and a
session of another workspace, written bare, as `b:s1` or as a name only that workspace holds, is
`ErrProfileUnknown`, the same answer as a session that does not exist (Q6). That stays: a local
`<workspace>:<session>` form that crossed workspaces would undo Q6. The `sessions` globs match
session ids of the caller's workspace only. The tool's description now says "(in the workspace
layout, only those of your own workspace)". The profile and its private folder belong to the
workspace, not the session, so a message to the same profile in another session of the workspace
reaches the same `profile-own` and data folder, by design. v1 is unchanged, several session roots
included.

**Remote calls name the workspace.** The way across workspaces is a remote entry, which is
configured, holds a token and has an allow-list. Its target is now
`remote:<name>/[<workspace>:]<session>/<category>/<profile>`; `<workspace>` is the root id the
remote server lists (`/api/workspaces`, `makeLocation`): `^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`, one
colon, no more. One grammar serves every remote use: `profile_message`, `subagent`, `profile`
info, `transcript` and `transcripts` (`remote.SessionRefPattern`), and the entry's
`default_session` may be written the same way.

- The server's routes take the same form (`sessionPattern` in `remoteapi`) and resolve it with
  `sessionfs.Resolve`, which already read `root:session`. A bare id that more than one workspace
  holds was "no session"; it is now 409 `ambiguous_session` ("write `<workspace>:<session>`", no
  workspace named). A bare id held by one workspace, and every v1 request, work as before.
- The ambiguity is told only where the token's globs have already admitted the session id (start
  of a message or sub-agent, profile info). The profile list reads the names before the token's
  globs are applied, so there it still answers `no_session`; a token that does not reach a session
  id learns nothing about it.
- A remote reader of transcripts was not a session: in the workspace layout its caller key
  (`remote-api\x00reader`) went through `ResolveFrom`, which resolves the caller first, so with
  several workspaces every remote transcript read failed as "session not found". The reader now
  resolves with `sessionfs.Resolve` (any workspace, token globs decide), recognised by its whole
  owner key, which no session's key equals (a profile id has a slash).
- Token globs (`profiles`, `agents`) are matched over `<session>/<category>/<name>` with the
  session as written. A qualified session contains a colon, which `*` would match: a glob written
  for bare ids (`s*/dev/*`, `*1/dev/*`) would have started to reach `staging:x`. So a qualified
  session is matched only by a glob whose session part is `*` or has a colon of its own
  (`a:*/dev/*`, `*:s1/dev/*`, `a:s1/dev/coder`). `*` still means every session of every workspace.
- Client side, `remote_ambiguous_session` tells the form to write.

**Security statement.** A remote token reaches every workspace of the server it points to, as far
as its globs allow; that was already so (Q6: remotes still work, the HTTP API and UI see every
workspace) and is now usable on purpose. An entry that points at the server it runs on is an
explicit way across workspaces. Not built: a token limited to workspaces, or a session/workspace
allow-list in the client entry (`Entry.Allows` covers category/name only). The target still runs
with its own sandbox, cannot be given a model or sandbox, a `reset` fails closed, and the hop and
depth limits apply; chain nodes are keyed by the full reference (`a:s1`, not `s1`), so the same
profile name in two workspaces is never taken for a cycle.

**Not changed.** The manifest `sample/tools/native/profile_message.json` and its schema: the
catalog refuses a definition whose schema differs from the code, so a schema edit would make every
copied definition fail. The workspace wording is in the description the code appends. The manifest
is now pinned by `TestProfileTranscriptsAndExecManifestsMatchTheGoSchemas`.

**Limits.**
- Transcript reads and token globs written for bare ids (T-036b). The transcript tools re-check a
  token against the session's canonical reference (`a:s1` with several workspaces). A token whose
  globs are written for bare ids could start a run there but not read its transcript, so the check
  now tries two names: the canonical reference under the rule above, and the bare id, but only
  while the bare id resolves (`sessionfs.Resolve`, the lookup a start request makes) to exactly
  that session. A bare id that two workspaces hold, a session of another workspace and an error
  all give no second name, so access fails closed; a second workspace that later holds the id
  takes the access away again. The same two names apply to the agent globs of sub-agent runs
  (`tokenProfiles.sessionNames`) and to the profile names offered when a read fails. A read can
  therefore reach only a session the token could start a run on, never more: the one difference
  from a start is that the session may be written as `a:s1` (as a cursor does) when the bare id
  resolves to `a:s1`. In v1 with several session roots the same rule applies with the root id (a
  bare token could not read there before either). Race: the lookup of the bare id and the read are
  two steps; a workspace that gains the id between them is seen by the next read, not this one.
- A workspace that is not listed in `global.json` (the start directory) gets an id derived from
  its folder name; list it with an `id` for a remote address that does not depend on the folder.
- v1 with several session roots now also takes `<root>:<session>` on the remote routes and answers
  `ambiguous_session` for a bare id held by two roots (before: `no_session`).
- Tests: `remote_workspace_test.go` (two workspaces that both hold `s1`, real server, real client
  tool, remote transcripts, tokens, hop limit, local refusal), `remote_bare_token_test.go` (bare
  globs: unique, ambiguous, other workspace, cursor, agents, v1 with two roots),
  `remote_loop_test.go`, `remote/target_workspace_test.go`, `remoteapi/hardening_test.go`.
- The loop through a remote entry that points at the server itself is run end to end
  (`remote_loop_test.go`): four profiles of two workspaces message one another round through
  `remote:self/...` (the entry's address is the server's own listener, taken before it starts);
  the call that would come back to the first profile is refused with `remote_hop_limit` (max_hops
  3) before it reaches that busy profile, every run ends, and no profile gets a second run. The
  chain nodes of local messages are not involved: remote hops count hops only, so a cycle through
  the server's own entry is stopped by max_hops, not by cycle detection.

## 29. profile_message across workspaces, locally (T-037)

The user asked to allow workspace to workspace for the local `profile_message`. This reverses Q6
for `profile_message` only; it was checked against the code before it was built (decider T-037,
step 44.9, with the changes below).

**Addressing.** `<workspace>:<session>/<category>/<profile>`, the grammar the remote path already
uses (`remote.SessionRefPattern`); `<workspace>` is the root id of the workspace (the id
global.json lists it under). A bare `<session>/<category>/<profile>` still means the caller's own
workspace, so a session id held by two workspaces is never ambiguous locally. The caller's own
workspace written qualified is the same as bare. `remote:` always means a remote server, so a
workspace called `remote` is written `local:remote:<session>/<category>/<profile>` (T-037b): the
`local:` prefix is read only in front of a `<workspace>:<session>` (two colons, never valid before),
so `local:<session>` is still the session of a workspace called `local` and every existing form
means what it meant. The glob stays `remote:*` (globs match the form without `local:`). The tool
description tells the model about the prefix. Nothing else reads a bare `remote:` as a workspace:
`transcript` and the other tools reach only the caller's own workspace and take no workspace.

**Both sides must say yes, nothing by default.**

1. *The caller's side.* `tools.native.profile_message.sessions` may hold
   `<workspace glob>:<session glob>` (exactly one colon, `*` and `?` only). A qualified reference
   is matched only by globs with a colon and a bare one only by globs without: a bare `*` has
   always meant "my workspace" and still does (`localTarget`). `*:*` is allowed when written.
2. *The target's side.* `W/.jad/workspace.json` gets `profile_message.accept_from`: globs of workspace
   ids whose profiles may message this workspace's profiles. Unset or empty: nothing comes in. Why
   both: the caller's globs can come from a cloned repository (`workspace.json` `defaults` may carry
   tool settings), and a repository must not be able to drive the agents of every other workspace.
   `workspace.json` is read-only in every sandbox, a profile cannot edit it, and the
   `profile_self_edit` ceiling already keeps a repository from raising itself.
3. *Both workspaces must be listed in global.json `workspaces`* (`Layout.Listed`). An unlisted start
   directory gets its id from its folder name or from its own `workspace.json`, which whoever wrote
   the repository chooses, so an `accept_from` naming that id could be satisfied by another
   directory. An `accept_from` in an unlisted workspace is ignored with a start warning.

The rule is `Layout.AcceptsProfileMessagesFrom`, called from `sessionfs.ResolveMessageTarget`, the
one resolver that crosses workspaces; `ResolveFrom` still gates every other tool. A workspace that
does not exist, one that does not accept the caller, one not listed, a session that does not exist
and a profile that does not exist all answer `unknown_profile` with the caller's own list of
profiles. It is not symmetric: b accepting a does not let b message a.

**What did not change.** `transcript`, `transcripts`, the `profile` tool (info, compact, reset,
config, edit, history) and `subagent` still reach only the caller's workspace. The caller reads the
target's answer (that is the feature) but not its transcript. The remote path and its tokens are a
separate permission and are unchanged. v1 is unchanged: outside the workspace layout a reference
that names a root is refused (`ResolveMessageTarget`), and a colon glob is inert there.

**Where the target runs.** In its own workspace: its profile file, `profile-own`, data folder,
transcript and state folder, queue and chain identity (nodes are `<workspace>:<session>`
references, so the same name in two workspaces is two nodes, and a message that returns to a profile
the chain already holds is `profile_cycle`). The caller's `max_chain` and `max_depth` apply.

**Sandbox.** A target that has no sandbox of its own used to take the caller's, filled in with the
caller's paths. Across workspaces that carries the caller workspace's folders into the target's run
(and the target's own folders are not among them), so it is refused with `sandbox_widens`: the
target needs a sandbox of its own, and the usual narrowing applies with each side's own
placeholders (`{workspace}` of the caller is the caller's folder, of the target the target's). The
target's run never reaches more than the caller's, so a sandboxed caller can message another
workspace only when its sandbox covers what the target's sandbox writes (the caller lists the
target's folder). The fixed hidden and sealed paths are the target run's: it hides every other
workspace's `.jad` and `G/auth`. A `sandbox` argument that names a template still works for a target
without a sandbox: the template's placeholders stand for the target's places and nothing of the
caller's is copied (decided against the code; the decider's blanket refusal would have made a
harmless call unusable). The check runs again when the run starts, with the settings it starts with.

**Limits (honest).**

- Without an enabled sandbox nothing keeps the caller's tools from reading the other workspace's
  files: `accept_from` is about who may *start a message run* in the workspace, not a file
  isolation. It is stated by the existing "private means separate storage and names only" rule.
- A profile that may switch toolsets (`on` and `full`) can switch to a visible toolset that carries
  a colon glob; toolsets are user-managed files, and the target's `accept_from` still decides. A
  profile file cannot carry tool settings in the workspace layout, and an own toolset can only tune
  `timeout_ms`, `busy_wait_ms`, `result_max_bytes` and `default_block_ms`, so a self-edit cannot
  write a colon glob (there is no self-edit key table change for it). This is tested
  (`TestSelfEditCannotGrantAWorkspaceSessionGlob`): an own toolset's `sessions`, `context_trigger`
  and `deny_profiles`, a profile file's `tools`, `settings.tools` and `profile_message`, at `on` and
  `full`, are refused and nothing is written; `accept_from` is in `workspace.json`, which no
  self-edit can write.
- The target's worktree, when its message ran in one, is reported to the caller on purpose (the
  changes are the answer); its path is a path in the target workspace.
- A failure of the target run that carries a client-safe provider message shows that message as it
  does for any message; only the transcript is covered by the rule below.
- A target workspace's own `accept_from` can be written by whoever wrote that repository; a
  workspace that lists a repository it does not trust and also accepts `*` from the others lets that
  repository's profiles be driven by the others' callers' prompts. List workspaces you trust.
- `compact` of the caller's `context_trigger` applies to a target in another workspace as it does
  to another session: it deletes nothing, keeps its backup and runs with the *target's* own
  compaction and model settings (the hook is asked about the target's session, and the target's
  tokens pay for it), so accepting a caller accepts that.

**Follow-up (T-037b, decider step 45.10).**

- *A reset `context_trigger` is refused across workspaces.* The trigger comes from the caller's
  settings (a cloned repository can carry them), and it clears the target's whole history before
  every qualifying message. `Prepare` refuses it (`native.ErrContextTriggerForeignReset`, code
  `context_trigger_refused`, "Nothing was sent or changed") right after the target's workspace has
  accepted the caller, so the refusal tells nothing a refusal at that point does not: before any
  queue place, run or transcript lock, with or without `after`. The refusal is for any reset
  trigger, even one that would not have fired yet: the setting is the caller's request to clear
  the target. Compact and a reset inside one workspace (own session, another session, the own
  workspace written qualified) are unchanged. Later, if wanted, the target could allow resets
  from chosen callers with its own key; not built.
- *The target's transcript is shown by name.* For a target in another workspace the tool's
  `transcript` information, the `ref_transcript` of a handle and the "transcript:" line of its polls
  show `<workspace>:<session>/<category>/<profile>` instead of the file in the target's `.jad`
  (`native.ProfileTranscriptLabels`, one place: the plan). The host keeps the file for its own
  reads, so the activity block of a handle still follows the target. The caller's own transcript
  path and a same-workspace target are shown as before. The path held no secret and a sandbox hides
  the target's `.jad`; this is the rule already applied to own-model errors. Not covered: the text
  of a failed run the provider reported, and the worktree line above.
- *Workspace named `remote`:* the explicit `local:` form above. No warning at start or in
  `jad check` is added: the form works, and the tool description names it.
