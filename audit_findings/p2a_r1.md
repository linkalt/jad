# p2a (sessionfs native_* tool settings + layouttest) review, reviewer r1

- phase: p2a; reviewer: r1; HEAD at review: fbe49a8 (worktree dirty only EXECUTOR.md, MASTER.md; untracked audit_findings/, nodir/); go1.27 linux/amd64; date 2026-10-28.
- scope: internal/sessionfs/native_*.go (24 non-test files, 4,845 lines) and internal/sessionfs/layouttest/layouttest.go (355). Phase 1 and 4 fixes assumed in tree; not re-reported.
- read in full: native_async_settings, native_profile_message_settings, native_transcript(s)_settings, native_subagent_{settings,dir,paths,deny}, native_tool_smith_settings (first 180 lines), native_bash_settings (lines 241-532), layouttest.go. Read only as evidence: asyncmgr/config.go + prune.go, tools/native/{subagent,subagent_call,tool_smith,shell_async}.go, app/{subagent,resolver}.go, sessionfs/{session,layout_load,tool_settings,tool_shape}.go, sample/schemes/setting.schema.json (async/profile_message/subagent blocks), DECISIONS entries for subagent, agents_dir, tool_smith, nesting.
- commands (exit codes) so far: `go build ./internal/sessionfs/...` 0; `go vet ./internal/sessionfs/...` 0; `gofmt -l internal/sessionfs` empty, 0; `go test ./internal/sessionfs/ -run 'NativeAsync|Transcript|ProfileMessage|SubagentAgentsDir|SubagentSchemas' -count=1` ok (0.03 s); three throw-away probe tests injected with `go test -overlay` from /tmp/r1/ovl (nothing written in the repo; `git status --short` unchanged, checked after each); two tiny Go programs under /tmp/r1 (duration overflow, ReadFile of /dev/zero under ulimit). No -race, no bench.

## 1. Understanding note

Each `normalizeNative<Tool>Settings(setting, settingPath, ...)` takes the composed `tools` object, digs out `tools.native.<tool>`, rejects unknown keys (`unsupportedToolKey` with edit-distance hint), reads bounded integers with `configuredInteger` (null = default, safe-integer check, min/max), and returns a pointer (nil = tool not configured). `loadSessionSettingsFrom` (session.go:~494) calls 18 of them on every `LoadSessionSettings`, i.e. on every message resolve, catalog listing and storage-recovery pass; the layout loader runs the same normalizers again for each distinct agent toolset (`loadAgentToolsets`). Schema (`sample/schemes/setting.schema.json`) is a second hand-kept copy of every key; `native_tool_schema_test.go` and `TestSubagentSchemasAgreeWithTheLoader` compare key lists for a subset of tools. `NativeAsyncSettings` only wraps `asyncmgr.Config`, whose `Validate` holds the cross-field rules. Agents of `tools.native.subagent` come from inline `agents` and from `agents_dir` folders (agent.json + prompt.md) read at load time. `layouttest` writes a catalog-layout session tree for tests; no production package imports it.

## 2. Hypotheses

| id | statement | status |
|---|---|---|
| T01 | day-valued async keys (max 10,000,000) overflow `time.Duration` in the manager | verified (finding 01) |
| T02 | a model-written agents_dir folder can steer a child's model file/key env | verified by read (finding 02) |
| T03 | prompt_documents is read without file-type/size checks | verified (finding 03) |
| T04 | an agent can list a code-running tool that is not in SubagentChangeTools | verified (finding 04) |
| T05 | duplicate inline agent names are rejected | disproved: accepted (finding 05) |
| T06 | every numeric key has a range and cross-field rules | partly: transcript keys lack relations (finding 06) |
| T07 | the storage_dir/section/list validators are shared helpers | disproved (findings 07, 08) |
| T08 | the 21 `Native*Settings` types hide a shared shape | partly: 25 types in sessionfs, 8 near-identical state-context/storage patterns (finding 08) |
| T09 | unknown keys silently ignored somewhere | disproved for all 21 sections read (every section goes through `unsupportedToolKey` or an explicit switch); statecontext and `paths` sub-objects checked too |
| T10 | env_file / bash env parsing (native_bash_env, native_env_file) leaks or loops | unverified, not read in depth (see not covered) |

## 3. Findings

### p2a-r1-01  `retention_days` / `unread_ttl_days` accept up to 10,000,000; the manager multiplies them into a `time.Duration` that overflows past 106,751 days and then prunes everything
- severity: medium
- lens: A correctness
- location: backend-go/internal/sessionfs/native_async_settings.go:10,80-86 (cap `maxNativeAsyncValue` = 10,000,000 for the day keys); consumers asyncmgr/prune.go:84,114, asyncmgr/prune.go:131-137 (`noteExpiryLocked`), tools/native/shell_async.go:114,132
- evidence: [measured arithmetic, read flow] `/tmp/r1/ov` (go run, exit 0): `time.Duration(106751)*24*time.Hour` is fine, 106752 gives -2562047h and `now.Add(-d)` lands in the future (`cutoff.After(now) == true`), same for 200000, 1,000,000 and 10,000,000. `pruneLocked` then treats every unread signal as older than the cutoff (`r.changed.Before(cutoff)`, prune.go:86-87) and drops every finished record beyond the first (`finished[drop].changed.Before(cutoff)`, :115); `sweepShellResults` deletes every result file whose mod time is before a future cutoff (shell_async.go:132-136). A value that the schema documents as valid (`"maximum": 10000000`) therefore turns "keep for a very long time" into "delete everything at the next prune", including unread completion signals and result files the model has not read. `asyncmgr.Config.Validate` has no upper bound either (config.go:97). Reachable by any config author; the unit tests only try `-1` and `99999999999` (native_async_settings_test.go:57-75).
- suggested fix: cap `retention_days` and `unread_ttl_days` (for example 36,500, ten times the tool bash retention `maxNativeBashRetention` = 3650) in native_async_settings.go, mirrored in sample/schemes/setting.schema.json and in `Config.Validate`; add the two values to the bad-input test table.
- effort: S
- prior art: new (86552a4 / 6db2207 introduced the keys; not in earlier audits)

### p2a-r1-02  An agent folder (`agents_dir`) can name any model file and so choose `api_key_env` and `base_url` for its child: a model that may write into the folder can send any server environment variable to a URL of its choice
- severity: medium (read-only chain, cap per section 5.2; promote to high if the chain is confirmed end to end)
- lens: B security
- location: backend-go/internal/sessionfs/native_subagent_settings.go:482-489 (`model` resolved with `resolveFrom(settingDir, expandHome(model))`, no containment), native_subagent_dir.go:106-161 (folders are read on every load); consumers app/subagent.go:177-180 (`config.ModelPath = request.Agent.ModelPath`), app/resolver.go:475-481 (`r.environment[config.Model.APIKeyEnv]`), :492-495 (`BaseURL: config.Model.BaseURL`); session.go:933-1006 (`base_url`, `api_key_env` taken from the model file)
- evidence: [read] + probe. Probe (overlay test, passed): `agents/x/agent.json` with `"model":"/tmp/.../secret.txt"` loads, `ModelPath=/tmp/.../secret.txt.json`; any path, relative paths resolve from the folder, so a model file inside the workspace works too. `loadModel` accepts `base_url` (any http/https host) and `api_key_env` (any identifier). `providerAPIKey` looks the variable up in the server's whole environment copy (only `JAD_API_TOKEN` is removed, app/config.go:83) and `newProviderStreamer` sends it as the bearer key to `BaseURL`. DECISIONS "The subagent tool loads with zero agents" says the model is expected to create agent folders itself, and `agents_dir` is resolved from the session cwd, which the model writes with `write`/`edit`. Chain: model writes `<cwd>/agents/x/agent.json` plus `<cwd>/m.json` (`base_url` attacker, `api_key_env` e.g. another provider's key or a `JAD_REMOTE_*` token) → next message loads the folder → `subagent` call → the child's first provider request carries the secret. Not executed end to end (no network). Needs: subagent configured with an `agents_dir` inside a model-writable tree. Without `bash`/`exec` in the profile this is an escalation; with them the model already reads the environment.
- suggested fix: for folder agents (and inline ones) accept only a catalog model ID, or require a `.json` path to be inside the catalog models dir; or refuse `api_key_env` names that the session's own model did not already use. Files: native_subagent_settings.go (`buildNativeSubagentAgent`), layout_load.go `resolveSubagentModels`.
- effort: M
- prior art: new; DECISIONS 731 ("the person who writes the agent vouches for the toolset") is about toolsets, not about model-written folders

### p2a-r1-03  `prompt_documents` of an agent are read with `os.ReadFile` without a file-type or size check on every settings load: `/dev/zero` or a FIFO blocks or exhausts the server
- severity: medium
- lens: A correctness (B for the model-written folder case)
- location: backend-go/internal/sessionfs/native_subagent_settings.go:555-573 (`loadSubagentPrompt`), native_subagent_dir.go:116 and :145 (`agent.json` read twice, `prompt.md` after `regularFileExists`)
- evidence: [measured, FIFO] probe test: `prompt_documents:["<fifo>"]` → `normalizeNativeSubagentSettings` still blocked after 1.5 s (released by opening the FIFO). `regularFileExists` guards `agent.json` and `prompt.md`, but the `prompt_documents` entries have no such check, no `Lstat`, no byte limit; `os.ReadFile("/dev/zero")` never returns (it grows its buffer). `LoadSessionSettings` runs on every message resolve (DECISIONS 582), so a blocked read stalls every request of that session, and the entry stays in the file. Reachable like finding 02 (agent.json written by the model into the cwd) or by a config author with a typo to a device file.
- suggested fix: `Stat` first and require `Mode().IsRegular()`, read through `io.LimitReader` with a documented cap (a new key `prompt_documents_max_bytes`, default and schema entry). File: native_subagent_settings.go.
- effort: S
- prior art: new

### p2a-r1-04  `SubagentChangeTools` omits `tool_smith`, `subagent` and `profile_message`: an agent that lists them is "read-only" for checkpoints, scheduling, background hand-off and the `paths` guard
- severity: medium
- lens: A correctness (B for the `paths` promise)
- location: backend-go/internal/sessionfs/native_subagent_settings.go:159 (list), :649-660 (`ChangeTools`, `ChangesFiles`), native_subagent_paths.go:80-100 (`checkSubagentPaths`); consumers tools/native/subagent.go:371-385 (`callMayChangeFiles`), :458 (`changeMarker`), subagent_call.go:48,71, tool_smith.go:74-77
- evidence: [measured load, read consequences] overlay probe: `tools:["read","tool_smith"]` and `tools:["read","subagent","profile_message"]` load with `ChangesFiles()==false`; `tools:["write","tool_smith"]` with `paths:{allow:["src"]}` loads (the guard only refuses bash, exec and process). DECISIONS 933/945 (2026-10-23) made all three listable for agents, and 893 states a tool_smith run "is as powerful as exec/bash". Consequences: (1) `callMayChangeFiles` is false, so the parent's call to such an agent takes no checkpoint and may run in parallel with other calls; a `tool_smith` script, a nested writer child (child → `subagent` → agent with `write`) or `profile_message` (its own `MayChangeFiles` is always true, profile_message.go:278) can change the workspace outside any restorable checkpoint; (2) `changeMarker` does not tell the model the agent can change files; (3) `paths` gives a false confinement for `tool_smith`, the very case `checkSubagentPaths`' comment calls "an error instead of a false promise"; (4) a read-only-looking agent may be handed off as a background handle even when `background_writers` is off.
- suggested fix: add `tool_smith` to the shell list of `checkSubagentPaths`; count `tool_smith` and `subagent` (when the target agent changes files, or always) in `ChangeTools`; add loader tests for each combination. Files: native_subagent_settings.go, native_subagent_paths.go.
- effort: S-M (the semantic choice for `subagent` and `profile_message` needs a decision)
- prior art: new (introduced by ffe974e and the profile_message-in-child step; not covered by earlier audits)

### p2a-r1-05  A duplicated key in the inline `agents` object yields two agents with the same name
- severity: low
- lens: A correctness
- location: backend-go/internal/sessionfs/native_subagent_settings.go:336-356 (`orderedObjectKeys` returns duplicates, `decodeConfigObject` keeps the last value, `taken` is written but never checked for inline names)
- evidence: [measured] overlay probe: `{"agents":{"a":{...},"a":{...,"description":"second"}}}` loads with `len(Agents)==2`, both named `a`, both built from the second object. DECISIONS 310 says "the same name twice is an error and neither wins silently"; folder duplicates are refused (native_subagent_dir.go:94), inline duplicates are not. `Agent(name)` returns the first; `withoutDeniedSubagents` and the tool's agent list then show the name twice.
- suggested fix: reject a repeated name in the inline loop (the `names` slice is already ordered) with the same "defined twice" message.
- effort: S
- prior art: new

- addendum to p2a-r1-03 [measured]: `/tmp/r1/z` (`os.ReadFile("/dev/zero")` under `ulimit -v 3000000`, exit 2) dies with `fatal error: out of memory`; without a limit it grows until the OOM killer acts. A `prompt_documents` entry pointing at a device file therefore kills the server process, not just the request. Severity of 03 stays medium only because it needs a config author or a model-written agent.

### p2a-r1-06  `env_file` is read with `os.ReadFile` and no file-type or size bound, on every settings load (bash, exec, process, tool_smith child via the shared loader)
- severity: low
- lens: A correctness / C performance
- location: backend-go/internal/sessionfs/native_bash_env.go:12-18 (`loadNativeBashEnvFile`), called from native_env_file.go:15-30 (`loadConfiguredEnvFile`) and native_bash_settings.go:~228
- evidence: [read] same primitive as p2a-r1-03 (`/dev/zero` kills the process, measured there). The path comes from the config author (root `defaults.env_file`, profile or tool settings), not from the model, so this is a hygiene point rather than a reachable escalation; the file is also re-read and re-parsed on each `LoadSessionSettings` (per message, per catalog listing), once for each of bash/exec/process that inherits the default. No benchmark (unmeasured).
- suggested fix: `Stat` + regular-file check + `io.LimitReader` (for example 1 MiB, documented) in `loadNativeBashEnvFile`; share one fix with p2a-r1-03 via a `readConfigTextFile(path, max)` helper.
- effort: S
- prior art: new

### p2a-r1-07  Inconsistent value types and null handling inside the same sections
- severity: low
- lens: D maintainability (A for the null case)
- location: native_bash_settings.go:241-250 (`kill_children_on_run_end` is the string "on"/"off" while every other flag, e.g. `login_shell`, `strip_ansi`, is a JSON boolean; `null` is an error here but "default" for all neighbouring keys); native_profile_message_settings.go:133-138 (`compact: null` is an error, `timeout_ms: null` is the default); native_bash_settings.go:380 (legacy-template guard compares the raw JSON text of `storage_dir` against `"\".bash-output\""`, so `".bash-output"` spelled with a JSON escape, or the exec default under a bash section, is treated as a non-default and rejected)
- evidence: [measured for profile_message: overlay probe `compact:null` → `compact must be "off" or "on"`, `timeout_ms:null` → ok] [read for the others]. Schema says `compact` is an enum string, so null being refused is consistent with the schema but not with the loader's own rule "null means default" (`configuredInteger`, `bashBool`, `optionalSubagentBool`).
- suggested fix: pick one rule (null = absent) and apply it in `normalizeKillChildrenOnRunEnd`, profile_message `compact`; compare the decoded string, not the raw text, in the spill guard. `kill_children_on_run_end` as on/off is documented (DECISIONS) so only note the oddity.
- effort: S
- prior art: new

### p2a-r1-08  Exec runner argv: only the first item and non-emptiness are checked, per-item and total length are not bounded
- severity: low
- lens: B security (hardening, reachable only by the config author)
- location: backend-go/internal/sessionfs/native_exec_settings.go:287-297 (`argv`: 1..32 non-empty strings, no max length per item), 217-253 (`lang` entries: unbounded count)
- evidence: [read] an `argv` item may contain NUL (JSON `\u0000`), which makes `exec.Command` fail at call time (`invalid argument`) instead of at load; no limit on the number of languages or on the item length. Reachable only by a config author who already runs commands; listed so the merge can drop it.
- suggested fix: reject NUL and cap item length (for example 4096) at load; low value.
- effort: S
- prior art: new

### p2a-r1-09  `fetch.denied_hosts` / `allowed_hosts`: entries are not normalised the way URL hosts arrive, so a trailing-dot host (`example.com.`) skips a deny entry for `example.com`
- severity: medium
- lens: B security
- location: backend-go/internal/sessionfs/native_fetch_settings.go:37 (`nativeFetchHostPattern` forbids a trailing '.'), :188-211 (`nativeFetchHosts`: lower-case + dedupe only); consumer tools/native/fetch.go:402-411 (`strings.ToLower(parsed.Hostname())`, `containsHost` equal-fold), cross-phase (7a)
- evidence: [measured parse + read] `/tmp/r1/ov` (go run, exit 0): `url.Parse("https://Example.COM./x").Hostname()` lower-cased is `"example.com."` and the loader pattern rejects `example.com.` as an entry, so the deny entry `example.com` never equals the URL host; DNS resolves the rooted name to the same addresses, so the request goes to the denied host. The same gap makes `allowed_hosts: ["example.com"]` refuse the rooted spelling (fail-closed, harmless). Public-address checks (`isPublicFetchAddress`) still block loopback/private targets, so this only defeats a deny/allow policy on public hosts. Actor: the model chooses the URL; the policy is the operator's control for a model that may be prompt-injected (a denied exfiltration host, for example). Not run against a live fetch (no network).
- suggested fix: strip one trailing '.' from the URL host before the comparison in fetch.go and from entries in `nativeFetchHosts`; add a test with the rooted spelling. Files: native_fetch_settings.go, tools/native/fetch.go.
- effort: S
- prior art: new (audit batch 9 covered fetch SSRF only, per plan row 7a)

### p2a-r1-10  Transcript and transcripts settings have no cross-field rules; async has them, profile_message has them
- severity: low
- lens: A correctness
- location: backend-go/internal/sessionfs/native_transcript_settings.go:90-109 (nine integers, no relation checked); compare native_transcripts_settings.go:111 (`max_entries <= scan_max_entries`), asyncmgr/config.go:100-126 (`Validate`)
- evidence: [read; overlay probe for the first] accepted today: `line_max_bytes: 268435456` with `scan_max_bytes: 1` (probe, err nil), `value_bytes` far above `preview_bytes` (both can be 65,536 independently), `response_max_bytes: 1` with the default `max_detail_bytes` (the tool silently uses `min(max_detail_bytes, response_max_bytes)`, tools/native/transcript_detail.go:96), `suggest_max` and `max_list_entries` vs `response_max_bytes` unrelated. A scan limit smaller than one allowed line makes every non-empty transcript return `scan_max_bytes` exceeded (transcript.go:296-299) with no hint that the two keys contradict. `RemoteAPITranscriptSettings` takes the defaults, so the remote API is unaffected.
- suggested fix: after the integer loop add `line_max_bytes <= scan_max_bytes` and `value_bytes <= preview_bytes` (the async `latest_value_bytes <= latest_bytes` rule is the precedent); test rows in `TestTranscriptSettingsCapTheListAndDetailLimits`.
- effort: S
- prior art: new (a57403b / 2828a68..010e616)

### p2a-r1-11  The same validation shapes are hand-copied in many files; messages and null rules have already drifted (F-22 type count, duplicated patterns)
- severity: medium (largest maintainability cost of the phase; no behaviour bug)
- lens: D maintainability
- location (all `backend-go/internal/sessionfs/`):
  1. "tools → native → <tool> + unknown-key loop" preamble, 18 copies (`toolFields, err := decodeConfigObject(...)` in native_{async,bash,exec,fetch,glob,grep,memory,mutation,process,profile_message,read_image,request_draft,skill,subagent,task,tool_smith,transcript,transcripts}_settings.go), each 15-30 lines. They disagree on `native: null` (`strings.TrimSpace(...) == "null"` in 9 files, a plain `!exists` in 9 others, so `"native": null` is "not configured" for async but a `must be a JSON object` error for task/memory/fetch/process/glob/grep/bash/request_draft), and on unknown-key reporting (`unsupportedToolKey` with a did-you-mean in async, profile_message, subagent, transcript(s), exec, tool_smith; a bare `"%s.%s is not supported"` in fetch, memory, process, bash-spill, task).
  2. `normalizeNative{Subagent,Task,Process,Memory,Fetch}StorageDir`: five 20-line functions identical except the default and the error text (native_subagent_settings.go:416-437, native_task_settings.go:149-170, native_process_settings.go:249-270, native_memory_settings.go:200-221, native_fetch_settings.go:213-234); the error wording already differs (`configError` vs `fmt.Errorf("resolve ...")`), and native_bash_settings.go:414-426 holds a sixth copy for the spill dir.
  3. Duplicate-free string-list readers: `normalizeProfileMessageDenyProfiles`, `normalizeSessionGlobs`, `normalizeRemoteNames` (native_profile_message_settings.go:171-238), `subagentDenyAgents`, `subagentPathList`, `optionalSubagentTools`, `loadSubagentPrompt` paths, `normalizeSubagentAgentDirs`, `nativeFetchHosts`, `normalizeNativeToolSmithTools`: ten near-identical decode/trim/pattern/duplicate loops with three duplicate wordings ("lists %q more than once", "lists %s twice", "is listed twice") and different null/blank behaviour.
  4. Boolean readers: `bashBool`, `nativeFetchBoolean`, `optionalSubagentBool`, and two inline `[]struct{target *bool}` loops in transcript(s) (9 "must be a boolean" sites); integer readers all go through `configuredInteger`, which is the good precedent.
  5. State-context blocks: `normalizeNative{Subagent,Task,Process,Memory}StateContext` repeat the states/limit/mode/detail parse (the mode/detail helpers were already extracted to native_statecontext_mode.go, the rest was not); the "states must contain ..." wording is copied three times (native_task_settings.go:185-195, native_process_settings.go:285-295, native_subagent_settings.go:388-394) and the subagent message omits `lost` from its list although the map allows it (native_subagent_settings.go:58-60 vs :394).
  6. Name patterns: `remoteNamePattern` (native_profile_message_settings.go:216), `subagentNamePattern` (native_subagent_settings.go:62) and `remote.NamePattern` (remote/entry.go:44) are three copies of `^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`; `internal/remote` imports nothing from sessionfs, so sessionfs could import it, but today a change to one silently splits the rules.
  7. `maxNative*`/`defaultNative*` constants: roughly 230 package constants across the 24 files; the same default appears under per-tool names (`maxNativeSubagentTimeoutMS`, `maxNativeProfileMessageTimeoutMS` both = `maxToolTimeoutMs`).
- evidence: [read; counts by grep] one `rg 'toolFields, err := decodeConfigObject'` gives 18 hits in native_*; per-file `null` behaviour from the first lines after `nativeRaw, exists`. The cost is visible in this phase's own bugs: p2a-r1-05, 07 and the `native: null` split are all drift between copies.
- suggested fix (smallest useful, no abstraction for fewer than three uses): (a) one `nativeToolSection(setting, settingPath, tool, keys) (fields, section, ok, err)` that does the preamble, null rule and `unsupportedToolKey`; (b) one `normalizeStorageDir(fields, def, cwd, toolStorageDir, settingPath, section)`; (c) one `stringList(raw, opts)` for the ten list readers. Do (a) and (b) only; both have five or more uses. Files: new native_section.go; the 18 normalizers shrink by about 350 lines. Goes with a test that runs one table of `{"native":null}`/`{"<tool>":null}`/unknown-key rows over all tools.
- effort: M
- prior art: F-22 (audit_backend.md:212, "intended design, none proposed") is about the package's type count and is not re-opened; this finding is about duplicated validation code, which F-22 did not cover

### p2a-r1-12  `NativeSubagentSettings` carries four "hand-built struct" zero-value contracts, so the loader and the tool disagree on defaults in tests and in `AgentToolsets`
- severity: low
- lens: D maintainability
- location: backend-go/internal/sessionfs/native_subagent_settings.go:89-110, :125-129 (comments: "Zero, only possible in a hand-built struct, means no limit / 1 / ask"), consumers tools/native/subagent_call.go:49 (`max(int(s.settings.MaxDepth), 1)`), :94-95 (`maxPerRoot`, `maxPerSession` zero = unlimited)
- evidence: [read] `ProcessMaxRunning`, `MaxBackgroundWriters`, `MaxDepth`, `MaxRunsPerRoot/Session`, `ListMaxBytes` and `PersistRuns`/`DeleteTranscripts` each have a different meaning for the zero value than for the loaded default, so every consumer must know both. The settings struct is 80 lines with 28 fields; `NativeSubagentAgent` has 22 (native_subagent_settings.go:162-208). The file is 670 lines, close to the 700 guideline, and mixes the section, state-context, storage dir, per-agent build, prompt reading and four optional-field readers.
- suggested fix: move the agent build (`subagentSource` … `optionalSubagentTools`, lines 459-645) into native_subagent_agent.go and make zero mean the default in the consumers' constructor (one `withDefaults` in the tool). Do not rewrite.
- effort: M
- prior art: new

### p2a-r1-13  layouttest: dead `Spec` knobs, fixtures that mirror loader rules, and thrice-copied file writers
- severity: low
- lens: D maintainability
- location: backend-go/internal/sessionfs/layouttest/layouttest.go:33-34 (`Defaults`), :101-113, :269-282, :299-312, :316-337 (four copies of mkdir + marshal + write), :339-355 (`withAsync`)
- evidence: [grep] `Defaults` is set nowhere in any test (`rg 'Defaults:|spec\.Defaults'` over internal/ finds only layouttest itself; the tests that set root defaults assign `layout.Defaults` on the loader's own struct, e.g. provider_stream_settings_test.go:65). `withAsync` re-implements the loader rule "process, subagent or tool_smith need async" (session.go:501) in the fixture, so a test of that very rule has to pass `Native` through the fixture helper that hides it (the only test, native_async_settings_test.go:107, uses `ToolsetRaw` to avoid it). `Write`, `AddSession`, `AddSessionOn`, `AddSessionInRoot` each carry their own `put` closure; `AddSession`'s doc says the fixture must have the default profile, enforced nowhere (a missing `dev/coder` fails later with a loader error naming the profile). 108 test sites use the package, so changes are cheap to check.
- suggested fix: delete `Defaults`; extract one `writeJSON(t, path, value)`; keep `withAsync` but document that tests of the async requirement must use `ToolsetRaw`.
- effort: S
- prior art: new

### p2a-r1-14  Test gaps for the new keys
- severity: low
- lens: D maintainability
- location: backend-go/internal/sessionfs/native_async_settings_test.go:57-88; native_profile_message_settings_test.go:47-98; native_subagent_settings_test.go
- evidence: [read] (a) async: the bad-input table has no day-key upper bound (p2a-r1-01), no `stop_wait_ms` above 6 h, no `latest_scan_max_bytes` boundary; the schema/loader agreement test (native_tool_schema_test.go:179) lists exec, read_image, bash, profile_message, tool_smith, transcript, transcripts but not `async`, `subagent` (separate test), `task`, `memory`, `fetch`, `process`, so a key added to `nativeAsyncKeys` without the schema passes CI (I checked the two sets by script today: identical, exit 0, so no current drift). (b) No test where an agent lists `tool_smith`/`subagent` with `paths` (p2a-r1-04). (c) No test for a duplicated inline agent name (p2a-r1-05). (d) No test that `prompt_documents` refuses a non-regular file (p2a-r1-03). (e) The profile_message `sessions` globs are validated but there is no test that a 129-character glob is refused (pattern allows 1..128).
- suggested fix: add `async`, `task`, `memory`, `fetch`, `process` to the key-agreement test; one row per item above.
- effort: S
- prior art: new

## 4. Performance hypotheses for measurement

- H01: every `LoadSessionSettings` re-runs 18 normalizers over the same `tools` JSON (each does `decodeConfigObject` of `tools` and `tools.native`, plus `orderedObjectKeys` re-tokenising the section), reads `prompt_documents` (every agent), `agent.json`/`prompt.md` of every `agents_dir` folder and the `env_file` of bash/exec/process, native_*_settings.go (preamble), native_subagent_dir.go:68-161, native_subagent_settings.go:540-576.
  workload: a toolset with all tools enabled, an `agents_dir` of 30 folders (agent.json + prompt.md + 2 prompt documents), 20 profiles, `LoadSessionSettings` called per message and per profile listing (app/transcript_profiles.go:35, app/catalog.go:322, app/storage_recovery.go:122).
  benchmark: existing `BenchmarkLoadSessionConfig` (sessionfs/settings_load_benchmark_test.go:10) extended with an `agents_dir` fixture, or new `BenchmarkLoadSessionSettingsAgentsDir`; compare 0 / 30 / 100 folders.
  expected: linear in the number of agent folders (about 4 file reads and 4 JSON parses each); the fixed 18-normalizer part is likely microseconds.
- H02: `orderedObjectKeys` decodes each value of an object just to list keys (decoder.Decode into RawMessage per member), run for every section and every inline agent, native_*.go + session.go:1224.
  workload: inline `agents` with 200 agents of 2 KiB each.
  benchmark: new `BenchmarkNormalizeSubagentInlineAgents` (200 agents), compare with a prototype that skips values via `decoder.Token` depth counting.
  expected: second full pass over the section (about 2x the JSON cost of the parse).
- H03: `unsupportedToolKey` is only on the error path, but `containsString` over key lists is O(keys x section keys); negligible. (listed so the merge can drop it)
  workload: n/a
  benchmark: none; expected: no effect.

## 5. Cross-phase leads

- L1: tools/native/fetch.go:402-411 host comparison (p2a-r1-09), phase 7a.
- L2: app/resolver.go:475-481 and :492 and app/subagent.go:177-180 take `api_key_env`/`base_url` from the agent's model file (p2a-r1-02); phase 6c/5 should check whether a child is allowed to pick any environment variable.
- L3: tools/native/subagent.go:371-385 `callMayChangeFiles` ignores `tool_smith`/`subagent` agents (p2a-r1-04), phase 6c.
- L4: asyncmgr/prune.go:84,114,131 and tools/native/shell_async.go:114,132 compute `time.Duration(days)*24h` without a bound (p2a-r1-01); phase 4 / 6b should clamp too, in case the loader cap is raised.
- L5: sessionfs/session.go:1224 `orderedObjectKeys` and 494-611 (18 normalizer calls) belong to phase 2b1.
- L6: sessionfs reads `agent.json`, `prompt.md`, `prompt_documents` and `env_file` with `os.ReadFile` and no regular-file check (p2a-r1-03, 06); 2b1/2b2 loaders (`readSessionObject`, session.go:1168) share the pattern for catalog files; worth one helper for all.

## 6. Checked and sound

- Every one of the 21 normalizer sections reads (async, profile_message, transcript, transcripts, subagent, tool_smith, bash/exec shared fields, fetch, memory, process, task, glob, grep, mutation, read_image, request_draft, skill) rejects unknown keys by name; sub-objects (`statecontext`, `paths`, `stdout`/`stderr`, `spill`, exec `langs`) also reject unknown keys.
- `configuredInteger` (tool_settings.go:178-215): rejects strings, fractions, exponents beyond 2^53, negative; null = default; error text names key and range. All async keys have a range; `config.Validate()` catches the cross-field rules, and the schema lists the same 24 async keys (script comparison, exit 0).
- Profile_message: `timeout_ms`, `busy_wait_ms <= timeout_ms`, `max_chain` 2..8, `max_queued_per_profile` 0..256, profile id/session glob/remote name patterns with duplicate refusal; session globs cannot contain '/', ':' or '[' (no root or segment crossing).
- Transcript(s) remote switches: `allow_remote` needs `remotes` and vice versa; `RemoteAPITranscriptSettings` forces `AllowPath=false`, `HidePaths=true` after normalising defaults (native_transcript_settings.go:141-168).
- `checkSubagentPaths` does refuse `bash`, `exec`, `process` with `paths`, and refuses `paths` without write/edit.
- Agent folder names are checked with `subagentNamePattern` before use as a path component (native_subagent_dir.go:87); `agents_dir` entries are deduped after resolution; symlinked folders deliberately allowed.
- Storage dir resolution cleans and absolutises; a relative `storage_dir` cannot leave `toolStorageDir`/cwd by `..` check is not applied, but that is by design (DECISIONS: absolute paths accepted).
- `go build`, `go vet`, `gofmt -l` clean for sessionfs.

## 7. Not covered

- native_glob_settings, native_grep_settings, native_mutation_settings, native_read_image_settings, native_request_draft_settings, native_skill_settings, native_task_settings (beyond state-context wording), native_memory_settings state-context/filter parser (`normalizeNativeMemoryStateContext`, `decodeMemoryFilter`): read only for the preamble and key list, not for bounds logic.
- native_tool_smith_settings.go lines 178-233 (interpreter, tools list) and native_exec_settings.go defaults/`NativeExecSettings` build (lines 124-216): not read line by line.
- native_process_settings.go bounds (read only by grep of `configuredInteger` calls, lines 69-248); native_bash_settings.go lines 1-240 apart from the shared fields.
- Test files read only for the async, profile_message, transcript and subagent dir tests; test quality of the other sets (sleep-based sync grep: none in native_*_test.go).
- No live run of `subagent`, `fetch` or the remote API; p2a-r1-02 and 09 are read/parse-level evidence only. No -race, no benchmarks, per the rules.
