# Design: nesting (tool_smith / subagent / profile_message in children) and removal of the approval system

Status: PROPOSAL, uncommitted. Nothing in the code was changed. Line numbers are from the tree at HEAD (4461dee) and were read, not run; items I did not verify are listed in section C.

User decisions taken as given:
(a) A sub-agent may use tool_smith, another subagent and profile_message.
(b) The whole approval system goes, except request_draft. No sandbox or container code in JAD: isolation is the user's job (they run JAD in their own container). Removal is plain deletion.

---

## Part A - lifting the child restrictions

### A1. Where a child is forbidden today

| # | Place | What it does |
|---|---|---|
| 1 | internal/tools/native/subagent.go:197 (`childToolPools`) | skips `subagentToolID` and `toolSmithToolID`; also skips non-native/extension sources and `tools.deny` entries |
| 2 | internal/tools/native/subagent.go:200-205 | only `ReadOnly` tools enter the read pool; changers are only the names in `sessionfs.SubagentChangeTools` (native_subagent_settings.go:142: write, edit, bash, exec, process) and only with `allow_write`. profile_message is not ReadOnly (profile_message.go:109 sets MayChangeFiles true, no ReadOnly) and not in that list, so it can never reach a child even once 1 is lifted |
| 3 | internal/sessionfs/native_subagent_settings.go:627 | agent `tools` validation uses the change-tool list; the agent `tools` list must also be extended to accept the three names |
| 4 | internal/sessionfs/native_tool_smith_settings.go:41 (`nativeToolSmithExcludedTools`) and :220 | scripts can never call bash, exec, process, async, subagent, tool_smith, task. profile_message is NOT excluded today (design_async_manager.md says Q9 wanted it added). This is a separate rule (script -> tool) from the child rule (child -> tool); decision (a) is about children only, so I propose to leave the script list alone |
| 5 | internal/tools/native/tool_smith_bridge.go:230-237 | a script's nested calls inherit ProcessOwner, ProcessOwnerLimit, ProcessAsync from the outer execution (so a tool_smith in a child already keeps the child's owner) |
| 6 | internal/app/profile_message.go:73-76 | "A run started by profile_message may not message further: the chain has depth one", using `RunningProfileRunCaller` (httpapi/profile_run.go:36,118). Also cycle checks at :55-70 (`ErrProfileCycle`, caller == target) and `ErrProfileSameTranscript` |
| 7 | internal/tools/native/tool_smith_async.go:126-143 | a script whose nested tools could ask runs synchronously (no handle). Disappears with Part B |
| 8 | internal/tools/native/subagent.go:95-103, 551, 602 and subagent_call.go:56-71 | writer background rules keyed on `Approval == ask`. Disappears with Part B |
| 9 | DECISIONS.md:298 | "The subagent tool is never in a child's catalog, so the depth is one with no counter. A child has no approval requester..." and :301 (MCP-ask servers refused for children). Both statements must be rewritten |
| 10 | design_async_manager.md:163 and :436 ("Only subagent and tool_smith stay excluded (no nesting)") | superseded by this document; update when implementing |
| 11 | Tests that pin the rule: internal/tools/native/subagent_toolset_test.go:66 and :162 ("A toolset that lists the subagent tool does not give it to a child: the depth stays one"), catalog_tool_smith_test.go, internal/sessionfs/native_subagent_toolset_test.go, internal/app/subagent_integration_test.go, internal/tools/native/async_tool_test.go. These need to flip to "allowed up to depth N and refused beyond" (file list from grep; I did not open each test body) |

Error messages: the child simply does not get the tool, so a model that names it sees the generic unknown/unavailable path of the catalog (plus the "profile's tools.deny is in effect" suffix at subagent.go:248-249). There is no dedicated "nesting not allowed" message today; one is needed for the depth limit (below).

### A2. Recursion safety design

Principle: a run knows its **depth** and its **root run id**; limits are checked at start time inside the manager, not by the model.

1. **Max depth** - new key `tools.native.subagent.max_depth` (default 2: session -> child -> grandchild; range 1..8; 1 = today's behaviour). Config only, never in a tool schema or description. Depth is carried in `ExecutionContext` (new field `AgentDepth int`, next to `AgentName` in tools/catalog.go) and set by `SubagentManager` when it builds the child's execution (near subagent_manager.go:517, where ProcessOwner is set). A start at depth >= max_depth returns a normal tool error (`subagent_depth_exceeded`, names the limit); the nested tool stays in the catalog so the model gets a clear reason instead of a vanished tool. Same counter gates tool_smith-in-child and profile_message-in-child where they start runs.
2. **Per-root and per-session caps** - `max_concurrent`/`max_queued` are per manager (shared per storage dir), so grandchildren compete with the session's own children and can starve it. Add:
   - `max_runs_per_root` (default 16): total live+queued descendants of one root run (counter on the root record, incremented in `Register`-time of each descendant, decremented at end).
   - `max_runs_per_session` (default 32): live descendants of one session key, any depth.
   Both refuse with `subagent_limit_reached`. They sit in front of the existing queue.
3. **Queue interaction / deadlock** - the real hazard: a parent holds a slot (max_concurrent=3), waits for a grandchild, and the grandchild is queued behind it. With all 3 slots held by parents waiting on children, nothing can ever run. Options: (i) a child that nests does not hold its slot while it blocks on a descendant - complicated; (ii) descendants bypass the queue and use a separate reserved pool (`max_nested_concurrent`, default 3); (iii) refuse a nested start when it would queue (fail fast, no queueing for depth > 1). **Recommend (iii)**: simplest, cannot deadlock, and the model gets an immediate "no free slot" it can retry later. This is the one real design choice here; see D1.
4. **Owner scoping** - children already use owner = run id for their processes and async handles (async_tool.go:155-156 uses `ProcessOwner`; subagent_manager.go:558 stops `DeleteOwnersWithPrefix(run.id, ...)` at run end). A grandchild gets owner = its own run id, so each level sees only its own handles (wait/list/stop). Result flows upward only through the child's final answer text (and `ref` file), never through shared handle lists. **Prefix hazard**: `DeleteOwnersWithPrefix(run.id)` is a string prefix; run ids must not be prefixes of one another. Ids are random with a fixed length today (I did not verify the length is constant across all id sources); if grandchild ids are built as `<parent>/<child>` this is safe by construction, so use that form and delete by `id+"/"` prefix plus the exact id.
5. **Cancel propagation** - stop/reset/delete reach top-level runs via `CancelOwner`, `CancelOwnersWithPrefix`, `DeleteOwnersWithPrefix`, `CancelUnmanagedOwner` (asyncmgr/manager.go:380,398; app/resolver.go:202; app/session_catalog_files.go:177-181). With hierarchical owner keys (`<session>/<run>/<run>`) the existing prefix fan-out already reaches descendants. Additionally, when a run ends or is cancelled its own context is cancelled, which cancels its descendants' contexts if the descendant's ctx is derived from the parent run ctx (a detached handle uses context.Background today in tool_smith_async.go; for descendants started by a child, derive from the child's run ctx so an ended child never leaves orphans; `subagent_run_ended` already stops handles). Tests required: stop parent -> grandchild gone; reset; session delete; goroutine count back to baseline.
6. **Non-blocking Stop** - keep the guarantee: cancel is signal-only (cancel func + state), never waits on a descendant. Stopping a root walks the prefix set once under the manager lock and cancels; it must not wait for grandchildren to exit.
7. **Token/context cost** - each level pays: its own system prompt (instructions + prompt_documents), tool definitions (the three added tools plus `async` add roughly the same description sizes the parent already pays; tool_smith's description is ~1 KB and subagent's grows with the agent list, up to `list_max_bytes` 4096 B), and its answer re-enters the parent's context up to `result_max_bytes` (32768). Worst case at depth 2 with fan-out 3: 1 + 3 + 9 model runs. `max_run_tokens` is per run, not per tree, so add (optional) `max_tree_tokens`? Not proposed now; the per-root run cap bounds it indirectly. I did not measure token numbers.
8. **Checkpoint/approval (after Part B)** - the old reason for "read-only children" (checkpoint is per parent run and a child's writes escape it; DECISIONS.md:298) is not touched by approval removal. Children that write still escape the parent's checkpoint (subagent_write tests exist). Nesting does not worsen this but multiplies the places writes can come from. Mention in docs; no code change in this task.

### A3. profile_message from a child

- Identity: the caller key is the *session profile* (the child has no profile). Use the **root session's profile** as the caller identity, taken from `SessionKey` (child executions carry it) - not the run id. Reason: targets are profiles; the existing cycle check (`caller`, `canonical`) compares profile names.
- Owner/handles: profile_message is synchronous today (not async; later task). Nothing to own. When it becomes async the owner must be the *calling run id* (child), consistent with A2.4, not the session key, so the child's end cancels it.
- Loop risk: A messages B, B's run (a normal profile run) messages A (or a child of B does). The current depth-one rule (app/profile_message.go:73-76) forbids any chain, which blocks the new feature if a child of A may message B and B's run then spawns a child that messages C.
- Guard: replace depth-one by a **chain**: each profile run started by profile_message carries a `chain` (ordered list of profile names + chain length) in the run record. Refuse when the target is already in the chain (generalised cycle check) or chain length >= `tools.native.profile_message.max_chain` (new, default 2, config only). `RunningProfileRunCaller` currently returns one caller; it must return the chain (httpapi/profile_run.go:36,118 interface change). Child runs started inside a profile run inherit its chain.
- Same-transcript and busy rules stay (ErrProfileSameTranscript, ProfileBusyError, `busy_wait_ms`).
- Child pool: profile_message is not ReadOnly and not in `SubagentChangeTools`; give it its own allow path (agent `tools` list may name it; it is not governed by `allow_write` unless the user wants that - D2).
- Caller-run compaction map (`callerRuns`, resolver.go:50-52, bound_runner.go:126) is keyed by the *caller run id* from ctx (`callerRunKey`); a child run needs that key set too, otherwise `compact` double-compacts or is skipped. Needs a check in implementation.

---

## Part B - removing the approval system (keep request_draft)

### B1. Inventory (counts from rg -i approval: 719 matching lines in Go, 80 in frontend_new/src; 35 Go test files mention it)

**Config / settings (internal/sessionfs)**
- approval_settings.go (112 lines): `approval` top-level section (`default`, `timeout_ms`, `max_timeout_ms`, `extend_ms`, `headless`, `preview_bytes`), `normalizeApprovalMode`, `ApprovalOff/Ask`, `ApprovalHeadlessDeny/Allow`.
- approval_default.go (91): applies `approval.default` to write, edit, bash, exec, process, profile_message, tool_smith and MCP servers.
- per-tool `approval` key: native_bash_settings.go:106,162 (style keys, shared by bash/exec/tool_smith/process), native_process_settings.go:101, native_mutation_settings.go:187 (write/edit), native_profile_message_settings.go:25, native_tool_smith_settings.go:133, native_exec_settings.go:131, mcp_settings.go (server `approval`), layout_files.go:31,165 and layout_load.go:597-598 (toolset `use.approval`, top-level key allow-list), session.go:335.
- tests: approval_settings_test.go, approval_default_test.go, plus mentions in ~15 other sessionfs tests and testdata/layout-settings.cases.json.

**Tools package**
- tools/approval.go (293): `ApprovalCall`, `ApprovalVerdict`, `ApprovalRequester`, `NewApprovalRequester`, `CallNeedsApproval`, preview building (`ApprovalPreview`, `CutText`, `encodeOrdered`, `writeJSONValue`). **Check before deleting**: `CutText`/helpers may be used elsewhere (I did not check callers).
- tools/catalog.go:37-40 (`ExecutionContext.RequestApproval`), :104 (`ApprovalAsk`), :378-386 (the gate returning `approval_unavailable`), `Tool.Approval` field, `approvalCall`.
- tools/interaction.go: **shared with request_draft** - only `InteractionKindApproval` (line 49) and approval-only fields go; the rest stays.
- tools/mcp/manager.go:251 (MCP server with approval ask), extension decide tests (tools/extension/decide_test.go) - check what they actually pin.

**Runtime wiring**
- session/runner.go:111-113 (`Options.Approval`), :599-603 (one requester per round with a mutex); runner_approval_test.go, runner_tool_smith_policy_test.go.
- app/bound_runner.go:74-79 (ApprovalOptions from config), app/subagent.go, app/checkpoint_integration_test.go.
- checkpoint/run.go:35 uses `tools.CallNeedsApproval("process", args)` to decide whether a process call counts as a change. **This is a hidden dependency**: it is used as "does this process call start something", not for approval. Must be moved to a small local helper (command present => mutating) in the same commit that deletes CallNeedsApproval.
- native/catalog.go:238,252,278,291,328,413 (`tool.Approval = approvalMode(...)`), native/exec.go, hooks_events.go, profile_message.go:107.

**Sub-agent / tool_smith rules**
- subagent_call.go: `childApproval` (142-), `detachGate`, `backgroundApproval` use (67), `allow_detached_writers` leftovers; subagent.go:95-103, 551, 602 (writer is background only if no tool asks); background writers flag `background_writers` / `max_background_writers` (native_subagent_settings.go:35-37,48,81-88,260-263); subagent_manager.go:429 `BackgroundWriters`, `reserveWriter`; app/resolver.go:185-191 `BackgroundWriters`; httpapi/handler_ports.go:76 and checkpoint_handlers.go:108 (checkpoint restore refused while background writers run - **this guard is about checkpoint safety, not approval; keep the concept**, see B4).
- tool_smith: tool_smith_async.go:126-143 (`toolSmithNeedsApproval`), :166 `backgroundApproval`; tool_smith_bridge.go:230,259-270 (`nestedApproval`, `toolSmithNestedAgent` label).
- Errors: `approval_unavailable` (tools/approval.go:17, catalog.go:379).

**HTTP / SSE**
- httpapi/interaction_handlers.go (191): `handleRunInteraction`, `handleRunInteractionExtend`, :142-144 approval-only decision validation. **Shared with request_draft** (same endpoint, kind distinguishes). Keep the endpoint; delete the approval branches. interaction_state.go, registry.go, executor.go:62 (broker wiring), approval_interaction_test.go (delete), interaction_handlers_test.go (trim).
- internal/interactions/broker.go (382): single-slot broker used by both. Keep; remove approval-specific fields (extend semantics may be draft-only - check).
- webui/types.go:67 (`Kind` "approval").

**Frontend (frontend_new/src)**
- components/ApprovalDrawer.tsx (177), approval-answer.ts (110; `interactionFailureTitle` is imported by App.tsx:12 and is shared - keep that function), approval-answer.test.mjs, App.tsx:12,16,2707,2731-2732,3288,3531, ViewPane.tsx:167,255,624 (`pendingApproval` also covers drafts: rename, do not delete), api.ts:102,114,1163 (`'approval'` kind), api-stream.test.mjs, interaction-end.ts/.test.mjs (check), pane-interaction-drawer.test.mjs, config.json:94 (`approval_preview_max_height_px`).
- Frontend CLAUDE.md rules apply: `npm test`, `npm run typecheck`, no `npm run build`.

**Docs / schema / samples**
- sample/schemes/setting.schema.json (approval section + per-tool keys), sample/tools/native/*.json (approval in samples: I did not list which), sample/jad (not inspected), DECISIONS.md (approval chapters, :298, :301, request_draft chapter at :70 which mentions the approval flow), design_approval.md (retire or mark historical), design_tool_smith.md, design_async_manager.md (sections 21 and the approval chapter at :490-519), CLAUDE.md files, audit_*.md (history; leave).

### B2. request_draft independence

request_draft.go uses only `tools.InteractionQuestion/Answer/Identity/Limits`, `execution.RequestInteraction` and `OnInteractionResolved` (request_draft.go:86-110) and its own headless auto-accept (`headless_auto_accept_timeout_ms`, native_request_draft_settings.go:72), not `approval.headless`. The only coupling is *package-level*: shared types in tools/interaction.go, the broker and the HTTP endpoint. The gate in catalog.go is not involved. So removal is feasible without touching request_draft logic, provided the shared interaction types stay. One test should be added first: request_draft end-to-end with approval config absent.

### B3. Old config keys: reject or ignore

Recommend **reject with a clear error naming the key and saying it was removed** for a deprecation window of one release (a one-line list in sessionfs: `removedKeys`), and not silent ignore. Reasons: a setting like `approval: "ask"` that is silently ignored removes a protection the user thinks they have; a loud failure at session load costs a one-line edit. Keys: top-level `approval` (all subkeys), per-tool `approval` on write, edit, bash, exec, process, profile_message, tool_smith, MCP server and toolset `use.approval`, subagent `background_writers`/`max_background_writers`/`allow_detached_writers`(if still accepted), frontend `approval_preview_max_height_px`. Open decision D4: once rejection is released, delete the list or keep as warnings.

### B4. What gets simpler

- `background_writers` flag and `max_background_writers`: a writer child is a handle exactly like a read-only one; no static "does any tool ask" check, no `reserveWriter`. **But** the checkpoint restore guard (checkpoint_handlers.go:108 -> `BackgroundWriters`) protects against restoring files while a writer still changes them. That safety is unrelated to approval; keep a (renamed) count of running writer children. So the flag goes, the counter stays.
- Block-on-ask: tool_smith no longer needs the synchronous fallback (tool_smith_async.go:141-143), so scripts with nested write tools become handles; subagent writers never block the call; `block_ms` works uniformly.
- Runner: no per-round requester/mutex; catalog gate loses ~10 lines; ExecutionContext loses `RequestApproval`; process `CallNeedsApproval` special case disappears.
- Frontend: one drawer (draft) and no approval preview height.
- Roughly 1.3-1.8 kLoC of non-test Go plus about 300 lines of TSX/TS and ~25 test files touched or deleted (estimate from the file sizes above, not a diff).

### B5. The safety property that disappears

Today, with `approval: ask`, no write, edit, bash, exec, process, tool_smith, profile_message or MCP call runs until a human has seen it and said yes. After removal **every enabled tool runs immediately with the permissions of the JAD process**: a prompt-injected page, a bad model turn, or a script from tool_smith can delete files, run any command, and reach the network and any credential visible to the process. JAD will have no second line of defence for this: the `paths` allow/deny lists (hard deny, not a question) and the `tools.deny` list remain and are the only remaining in-process limits, and they are not a sandbox (bash/exec/tool_smith can bypass path lists, as tool_smith's own description admits). The model of protection becomes: **the user's container is the security boundary.** README/DECISIONS must say this on the first page, and the default sample should say that JAD is meant to run inside a container with only the project mounted and no secrets in its environment. JAD cannot verify that it is in one; no detection code is proposed.

### B6. Removal order (small commits, each green: gofmt, vet, full tests, -race on touched packages; frontend: npm test + typecheck)

| Step | Content | Effort |
|---|---|---|
| B-0 | Tests first: request_draft e2e and checkpoint "process with command counts as change" without approval config | S (0.5 d) |
| B-1 | checkpoint/run.go: replace `tools.CallNeedsApproval` by local helper | S |
| B-2 | Sub-agent/tool_smith rules: delete `childApproval`, `detachGate`, `backgroundApproval`, `toolSmithNeedsApproval`, `nestedApproval`, `background_writers` flag (keep writer counter for checkpoint guard); writers and scripts become handles unconditionally | M (1-1.5 d; tests in subagent_background_writer_test, subagent_write_test, tool_smith_async_test migrate) |
| B-3 | tools package: remove catalog gate, `RequestApproval`, `ApprovalAsk`, `Tool.Approval`, approval.go, `InteractionKindApproval`; runner and bound_runner wiring; native/catalog.go assignments | M (1 d) |
| B-4 | sessionfs: delete approval_settings/approval_default, per-tool key parsing; add removed-key errors; layout files; MCP server `approval`; update schema, samples, testdata | M-L (1.5 d; many tests) |
| B-5 | httpapi/broker/webui: drop approval branches of the interaction handlers, kind, tests | S-M (0.5-1 d) |
| B-6 | Frontend: delete ApprovalDrawer, trim approval-answer.ts, api.ts kind, App/ViewPane rename `pendingApproval`, config.json key | S-M (0.5-1 d) |
| B-7 | Docs: DECISIONS.md, design docs, CLAUDE.md, container-responsibility statement | S (0.5 d) |

Total about 5-7 working days, two of which are test migration. Part A depends on B-2 (no approval means a child needs no requester), so do Part B through B-3 first.

### B7. Part A implementation order and effort

| Step | Content | Effort |
|---|---|---|
| A-1 | `AgentDepth` in ExecutionContext, hierarchical run/owner ids, `max_depth`, per-root/per-session caps, fail-fast when queued at depth>1, error codes | M (1.5 d) |
| A-2 | Allow subagent and tool_smith in child pools (`childToolPools`, agent `tools` validation, script bridge keeps its list); flip pinning tests | S-M (1 d) |
| A-3 | Cancel/ctx derivation for descendants; tests for stop/reset/delete/leaks | M (1 d) |
| A-4 | profile_message in child: chain instead of depth-one, interface change in profile_run.go, `callerRunKey` for children | M (1.5 d) |
| A-5 | Docs and schema keys | S |

---

## C. Open decisions for the user

- **D1** Queue behaviour for nested starts: fail fast when no slot (recommended) vs. separate reserved pool vs. releasing the parent's slot while it waits.
- **D2** Is profile_message in a child governed by `allow_write` (it can cause writes in another profile) or by the agent's `tools` list only?
- **D3** Defaults: `max_depth` 2, `max_runs_per_root` 16, `max_runs_per_session` 32, profile_message `max_chain` 2. Fine?
- **D4** Removed approval keys: reject for one release (recommended) then drop the list, or reject permanently?
- **D5** Should scripts (tool_smith bridge) also be allowed to call subagent/tool_smith/profile_message? Proposed: no; this document changes children only.
- **D6** Keep the checkpoint restore guard for running writer children (recommended) - confirms the flag goes but the counter stays.
- **D7** Should the MCP rule "children may not use MCP servers with approval: ask" (DECISIONS.md:301) simply vanish? Yes under B.

## D. Risks

- Removing approval is irreversible in practice and makes JAD unsafe outside a container; the docs must say so (B5).
- Deadlock/starvation from nested runs holding slots (A2.3) - mitigated by fail-fast.
- Cost explosion (1 + 3 + 9 runs, each with its own prompt and tools) - mitigated only by caps, not by token budgeting per tree.
- Prefix-based owner fan-out (A2.4) can hit unrelated owners if ids are not delimiter-safe.
- Large mechanical deletion touching ~35 Go test files and several frontend tests: keep commits small; a green full suite per step.
- `CallNeedsApproval` in checkpoint/run.go and `BackgroundWriters` in checkpoint_handlers.go are non-approval uses of approval-named code; deleting them blindly would break checkpoint behaviour.
- Existing flaky test (TestSlowSubscriberIsDroppedAndDoesNotHoldBackThePublisher in httpapi) may confuse step-wise verification.
- profile_message chains: loops across profiles that share a transcript or run busy; guard relies on chain data being propagated to every nested run.

## E. Not verified

- No code was built or run; everything is from reading and grep. Counts of lines are grep counts, not diffs.
- Which sample files under sample/ mention approval (grep path was wrong at first; the sample tree is at the repo root, not backend-go/sample) and the bodies of the test files listed in A1.11.
- Whether `CutText` and preview helpers in tools/approval.go have users outside approval, and whether the broker's extend feature is draft-only.
- Whether MCP-related tests (tools/extension/decide_test.go) depend on approval semantics.
- Run id length/shape for the prefix-safety claim in A2.4.
- Token cost numbers (none measured).
- How `SessionKey` looks for a child execution in practice (I assumed it equals the session key as in tool_smith_async.go:99).
