# Impact trace: aa2c904 (owner hooks on child nested subagent and profile_message)

- base ba62b87, commit aa2c904; repo `backend-go/`, `sample/`, `audit_findings/design_lazy_tools.md`
- read: `tools/native/subagent.go` (SubagentMCP, newSubagentToolRemotes, addSelfToNestingAgents, bindMCP, subagentChildTools), `tools/native/catalog.go` (BuildCatalogWithAsync), `tools/native/hooks_events.go`, `app/resolver.go` (buildRegisteredCatalog, loadAgentToolsetMCP), `tools/catalog.go` (Listed/Refused), `sessionfs/profile_tools.go`, `catalog_tool_hooks_test.go`, `app/{hooks,mcp,profile_message,lazy_tools,subagent}_*_test.go`, `sample/schemes/profile.schema.json`, `setting.schema.json`
- commands (from `backend-go/`): `go build ./cmd/... ./internal/...` 0; `go vet ./internal/tools/native/ ./internal/app/` 0; `gofmt -l` empty; `go test ./internal/tools/native/ -run 'TestPreToolHookFires|TestNestedCopyIsHooked|TestRefusedLazyCall|TestNotLazyBindable|TestChildOfAParentWithANativeLazyTool|TestBlockingPreTool|TestToolEventHooks' -count=1` PASS (18); `go test ./internal/app/ -run 'TestHooks|TestToolEventHooks|TestHTTPRunExecutesMCPToolAndPassesMultimodalOutputToNextRound|TestSubagentCallsAnMCPToolItsListNamesThroughTheSharedConnection' -count=1` PASS; `go test ./internal/sessionfs/ ./internal/tools/ -count=1` ok.

## Findings

### AT-01  a child's MCP tool copies still carry no hooks (regression, missed by this commit)
- severity: high — lens: A correctness (a configured audit/blocking guard silently does not run)
- location: `backend-go/internal/app/resolver.go:603-607` (hooks go into the copy `parentMCPTools` only) and `:629` (`BindToolsets(mcpTools, toolsetMCP)` passes the raw list); `backend-go/internal/tools/native/subagent.go:629,650` (`bindMCP` applies only `GateFor`, never `WithToolEventHooks`)
- evidence: [measured] overlay probe `/tmp/lzr2/zz_child_mcp_hook_probe_test.go` (child agent tools `read, mcp:fixture:lookup`, `pre_tool` `match: mcp:fixture:*`): the MCP server ran once and no marker was written — `--- FAIL ... no pre_tool event for the child's MCP call`. The parent's own MCP call does fire, per the passing `internal/app/mcp_integration_test.go:206`. Regression: before c4b92fd the resolver hooked `mcpTools` in place and only then bound it to children (`git show d6ff00e:backend-go/internal/app/resolver.go:591,601`). Toolset MCP tools *are* hooked for children (`resolver.go:901`), so the gap is the session's own MCP servers only. aa2c904 hooked the native `profile_message` and nested `subagent` copies and documented only those (`audit_findings/design_lazy_tools.md:140-144`); no test covers a child's MCP hook, which is how the gap survived.
- suggested fix: pass the hooked list to `BindToolsets`, or wrap each child copy in `bindMCP` (`subagent.go:650`); extend the design note bullet.
- effort: S — prior art: new for this commit (same class as the ba62b87 fix).

### AT-02  the child-pool comment no longer says the list is unhooked
- severity: low — lens: D maintainability
- location: `backend-go/internal/app/resolver.go:603-605`
- evidence: [read] "the raw list is kept for the subagent's toolsets below" — "raw" now means ungated *and* unhooked; AT-01 is exactly what a reader would miss here.
- suggested fix: word it as "unhooked and ungated; a child gates and hooks its own copy" after AT-01.
- effort: S — prior art: new.

## Checked and sound
- `NewSubagentMCP` callers: `app/resolver.go:584` (owner hooks + key env, built before `BuildCatalogWithAsync`, so nested copies are hooked before any `Bind`/`SetLazy`) and `native/catalog.go:206-208` (nil handle fallback). Tests still use `&SubagentMCP{}`; zero hooks make `WithToolEventHooks` return the tool unchanged, so those expectations are not stale.
- `SetHooks` is gone: `grep -rn SetHooks backend-go sample audit_findings *.md` = no hits; no later call can run in the wrong order, so ordering cannot regress.
- Exactly once, no double wrap: the parent's copy is the raw `self` wrapped once at `catalog.go:217`; children get `runtime.self`, wrapped once at `subagent.go:338-341` and shared by every depth; the child pool's `profile_message` is wrapped once (`catalog.go:182-183`) and appended raw (`:219`). Tests assert exact counts (2 fires after parent + child `profile_message`, 3 after the child's nested `subagent`; 2 in `TestNestedCopyIsHookedBeforeAnyBindOrLazySet`).
- Nil-handle path: correct for hooks (the handle exists, `mcp.tool = runtime` is set, nested copies are hooked). The handle is local, so the caller's own nil handle stays nil and a later `Bind`/`SetLazy` on it still no-ops — unchanged from before, and every production caller passes a real handle.
- Remote and `profile_message` target runs are unchanged: the remote child runs under the remote profile's own hooks (nothing crosses the wire), and a target resolves its own catalog, so its hooks are its own; the calling side's copy is hooked once.
- `NotLazyBindable` vs the schema: the lazy pattern and Go refuse the same six names (`subagent`, `profile_message`, `profile`, `transcript`, `transcripts`, `load_tool`); `decide_systemone` stays accepted on both sides because it is built inside `buildNativeTools` (`native/catalog.go:537`) and so is bindable. `sample/schemes/profile.schema.json:80-86` and `setting.schema.json:1556` match the code; no stale text found there.

## Not covered
- No run of the whole `internal/app` or `native` suites (targeted tests only), and no measurement.
