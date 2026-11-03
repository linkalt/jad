# lazy-tools (load_tool) review

- phase: lazy-tools change, commits c4b92fd, b296be9, ecfedb5, 89ee723 on top of d6ff00e
- reviewer: single autonomous reviewer (Agent 1 assignment); report to Agent 1
- HEAD: 9f84316 (`profile: refuse a sub-agent's remote info when allow_children is off`)
- files read: `internal/tools/catalog.go`, `tools/lazy.go` (whole), `tools/native/{catalog,lazy_load_tool,subagent}.go`, `app/{resolver,profile_tools}.go`, `sessionfs/{profile_tools,native_load_tool_settings,session,tool_shape}.go`, `session/runner.go` (run loop), `sample/schemes/{profile,setting}.schema.json`, `sample/tools/native/load_tool.json`, `DECISIONS.md`, `audit_findings/design_lazy_tools.md`
- commands (from `backend-go/`): `go build ./cmd/... ./internal/...` exit 0; `go vet ./internal/{tools,tools/native,app,sessionfs,session}/` exit 0; `gofmt -l` empty, exit 0; `go test ./internal/tools/ ./internal/tools/native/ -count=1` ok; `go test ./internal/app/ -run 'TestLazy|TestSubagentChildLoadTool|TestSetLazy' -count=1` ok; two `/tmp` probe tests via `go test -overlay /tmp/lzr/overlay.json` (below).

## 1. Findings

### LZ-01  pre_tool/post_tool hooks no longer fire for `subagent` and `profile_message` calls
- severity: high — lens: A correctness (behaviour regression for every profile, lazy or not)
- location: `backend-go/internal/tools/native/catalog.go:213` (subagent), `:216` (profile_message), `:182-183`; the only hook pass is `:539` inside `buildNativeTools`, which runs before these tools are appended. Same class at `:159` (the `profile` tool, f1d91cd).
- evidence: [measured] c4b92fd deleted both `WithToolEventHooks` call sites (`git show c4b92fd -- internal/tools/native/catalog.go`, `-` lines 56/67) and no replacement runs after the append. Probe `/tmp/lzr/zz_lazy_probe_test.go` (overlay, package `app`, hook `match:"subagent"` on the parent session): `--- FAIL: TestProbePreToolHookForSubagentCall` "no pre_tool event for the subagent call"; the same fixture's child-write hook passes (`TestToolEventHooksRunForAChildAndNameTheAgent` ok in the same run). A blocking pre_tool on `subagent` can no longer block a sub-agent run; hooks validate fine (`app/profile_tools.go:126`) so the rule silently does nothing.
- suggested fix: wrap the subagent, profile_message and profile tools with `WithToolEventHooks` where they are appended, or apply `WithToolEventHooksAll` once to the final list before `tools.NewCatalog`.
- effort: S — prior art: new, introduced by c4b92fd.

### LZ-02  stale design claims about children in `design_lazy_tools.md`
- severity: medium — lens: D maintainability
- location: `audit_findings/design_lazy_tools.md:49-50` ("child catalogs are built before that step, from the raw list, so a child never gets the gate"), `:77-78` ("It is not handed to sub-agents"), `:136-137` ("A subagent child cannot call `load_tool` ... refused as `unknown_tool`")
- evidence: [read] `native/catalog.go:196-197,213` passes `childPool = loaded`, i.e. the already gated+hooked list; `subagent.go:127-137,361-378` gives children `load_tool`; `subagent.go:638-641` gates child MCP tools with the owner's set. 89ee723 added the "Not in v1" section but left these three statements contradicting it.
- suggested fix: update the three lines to the b296be9 behaviour (native children inherit the gate; load_tool reaches a child that names it).
- effort: S — prior art: new.

### LZ-03  JSON schema descriptions say a sub-agent never gets load_tool / lazy tools
- severity: medium — lens: D maintainability (Go vs schema drift)
- location: `sample/schemes/setting.schema.json:1556` ("A sub-agent never gets this tool."), `sample/schemes/profile.schema.json:82` ("Lazy tools are not handed to sub-agents or profile_message children.")
- evidence: [read] `app/resolver.go:617-624` builds `ChildLoadTool` and `subagent.go:361-378` hands it to an agent whose tools list names `load_tool`; `subagent.go:638-641` gates a child's MCP tools with the owner's set. Tests: `internal/app/child_lazy_load_test.go`, `subagent_child_load_tool_test.go`.
- suggested fix: reword both descriptions (child gets `load_tool` only when the agent lists it and the owner sets `tools.lazy`; loads go to the owner's `lazy_tools.json`).
- effort: S — prior art: new.

### LZ-04  a `tools.lazy` entry naming a tool the lazy pass can never bind is accepted silently
- severity: medium — lens: A correctness (silent config no-op)
- location: `app/profile_tools.go:52-72` (`checkLazyEntriesVisible` only checks visibility); `native/catalog.go:533-538` binds only `buildNativeTools` output, which excludes `subagent`, `profile_message`, `profile`, `load_tool`, and extension tools are added outside it.
- evidence: [measured] probe `/tmp/lzr/zz_lazy_probe_test.go::TestProbeLazyEntryNamingSubagent` (`allow: [subagent, read, load_tool, mcp:fixture:lookup]`, `lazy: [subagent]`): the session loads with no error, `parent tools = [read subagent load_tool mcp_fixture_lookup_…]` (subagent still eager), and `load_tool` answers "subagent: not a lazy tool of this profile" (`native/lazy_load_tool.go:165`).
- suggested fix: refuse any `tools.lazy` entry that matches no tool the lazy pass can bind (a native tool outside `buildNativeTools`, or an extension tool) in `checkLazyEntriesVisible`.
- effort: S — prior art: new.

### LZ-05  an agent naming `load_tool` is silently ignored when the owner has no lazy set
- severity: low — lens: A correctness / D consistency
- location: `app/resolver.go:618` (`lazy != nil && config.NativeLoadTool != nil`), `native/subagent.go:572-574` (`continue` before the byName check)
- evidence: [read] every other opt-in native tool on this path (`tool_smith`, `profile_message`, `async`) returns an explicit error when unavailable (`subagent.go:576-587`); `load_tool` is skipped before that check, so `tools.native.load_tool` with no `tools.lazy` (an allowed config, design Config bullet 4) gives the child nothing and says nothing.
- suggested fix: after `addLoadToolToNestingAgents`, either refuse an agent that names `load_tool` when no child load_tool was built, or document it.
- effort: S — prior art: new.

### LZ-06  the tools array is now marshalled on every round
- severity: low — lens: C performance
- location: `internal/session/runner.go:459-465`
- evidence: [read] c4b92fd moved `ToolCatalog.ResponseTools()` from once per run (before the loop) into the round loop, for every run including profiles without `tools.lazy`; each call re-marshals every listed definition. Unmeasured; workload: 40 tools x 50 rounds.
- suggested fix: keep a per-run cache keyed on a "listed set" revision from the catalog, or accept and record it as a decision.
- effort: S — prior art: new.

### LZ-07  `lazy_tools.json` is written without fsync
- severity: low — lens: A correctness (durability, divergence from neighbouring state writes)
- location: `internal/tools/lazy.go:324-350`
- evidence: [read] `CreateTemp` -> `Write` -> `Close` -> `Rename` with no `Sync()`; `sessionfs/layout_settings.go:190`, `sessionfs/catalog_files.go:492`, `transcript/state_context.go:78`, `transcript/cursor_cache.go:259-275` (file and directory) all sync. A lost write only costs a reload, so this is hygiene.
- suggested fix: add `file.Sync()` before `Close` in `save`.
- effort: S — prior art: new.

### LZ-08  no `DECISIONS.md` record for the new config surface
- severity: low — lens: D maintainability
- location: `backend-go/DECISIONS.md` (no `tools.lazy`, `load_tool` or `lazy_tools.json` entry; `grep -in "lazy\|load_tool"` finds only unrelated text)
- evidence: [read] `CLAUDE.md:56` lists `DECISIONS.md` among what a config change must update; the design note lives only in the uncommitted `audit_findings/` tree.
- suggested fix: add one decision record: the lazy set, its fingerprint and file, `lazy_load_max`, and the accepted prompt-cache rewrite trade-off.
- effort: S — prior art: new.

## 2. Checked and sound
- `Listed` is honoured on the only tools-array path: `Catalog.ResponseTools` (`tools/catalog.go:224-252`) is called only from `session/runner.go:460`, and `buildRoundRequest` carries the result to every provider.
- `Refused` is honoured by the single dispatch path (`tools/catalog.go:345-425`); the tool_smith bridge and `remote_api.go:230` both go through it, so no path runs an unloaded lazy tool's schema.
- MCP gating is nil-safe (`LazySet.Bind`/`Gate`/`GateFor`/`Loaded` all return early on nil): a profile without `tools.lazy` gets the previous catalogs unchanged.
- `checkLazyNeedsLoadTool` placement: runs after `normalizeProfileTools` and `normalizeNativeLoadToolSettings`, so both keys are known; `load_tool` itself is refused as a `tools.lazy` entry.
- `load_tool` reads the bound set at call time; the batch limit counts distinct tools; a failed save rolls back the in-memory marks.
- Go bounds (`1..4096`, `1..100`, defaults 256/16) match `sample/schemes/profile.schema.json:89-104`; the sample definition `sample/tools/native/load_tool.json` matches `loadToolInputSchema` byte for byte.
- `tools.lazy` without `allow`/`deny` is refused by both Go (`sessionfs/profile_tools.go:77`) and the schema `anyOf`.
- Child catalogs exclude `load_tool` from the default pool (`subagentChildTools`), so a child never gets it by default; `addLoadToolToNestingAgents` runs before `BindToolsets`, so the appended tool survives `bindMCP`'s `agent.nativeTools` copy.

## 3. Not covered
- The accepted resolver `ConfigError` mapping gap is not re-reported (design note, Verification limits).
- Commits after c4b92fd that touch the same files (f1d91cd profile tool, 397e633/14c38c5/9f84316) were read only where they share this change's code paths (LZ-01 `:159`); they are not reviewed here.
- frontend/ frontend_new/ have no `tools.lazy` or `load_tool` surface that I checked.
- No measurement was run: LZ-06 is unmeasured by rule.
