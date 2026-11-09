# Design: one run model for sub-agents and profiles

**Status:** plan, not started. Begin after the isolation work (T-120, including P4) is done; see
`docs/isolation-handoff.md`. The user agreed to the direction on 2026-11-09. File and line
references are from `d72ef85` and were read, not run.

## 1. Goal

There should be one way to start an agent run: **an agent definition shaped like a profile, run as
one of two kinds of instance, with a context handed down by the caller.**

| Kind | Transcript | Concurrency | Started by |
|---|---|---|---|
| **Persistent** (today's profile) | one per session and profile, kept | one run at a time; others queue | the user, `profile_message` |
| **Ephemeral** (today's sub-agent) | a fresh one per run, under the agent storage folder | many at once from the same definition, no queue per definition | `subagent` |

The caller hands down only what describes *where and under what limits* the run happens:

- the working directory and worktree;
- the sandbox ceiling and isolation;
- plan mode;
- depth, the root run and the chain;
- the cancel parent and the owner of the run's handles;
- the change and cost reporting;
- the remote hops.

Everything else, such as the prompt, model, tools, toolset and settings, comes from the definition
itself, resolved the way a profile is.

With this model, nesting works the same way in every direction: sub-agent to sub-agent, sub-agent
to profile, profile to sub-agent, and a remote server in between. Nesting is limited only by
configured caps, not by special wiring at each level.

## 2. How it works today

### 2.1 The engine is already shared

Both kinds end in the same `session.Runner`:

- **Profile run:**
  - `app.boundRunner.Run` (`app/bound_runner.go:53`) takes the transcript's run lock (`runMu`),
    applies `max_run_duration_ms`, and then runs `runLocked`.
  - `runLocked` checks the budget, runs the `session_start` and `prompt_submit` hooks, persists the
    input, and calls `prepareRun` (`app/resolver.go:487`), which builds the catalog, MCP servers
    and state context from the profile's `SessionConfig`.
  - It is started by the HTTP run API (`httpapi` `activeRun`, which the UI streams over SSE), or by
    `profile_message` through `ProfileRunStarter` (`httpapi/profile_run.go:47`).
- **Sub-agent run:**
  - `app.subagentLauncher.Run` (`app/subagent.go:50`) calls `session.NewRunner` directly, with a
    store in a fresh folder (`openStore`, `:253`).
  - The child's config is **the caller's `SessionConfig`** with only the model and prompt replaced
    (`childConfig`, `:235`).
  - The child's catalog is not built by the resolver. The `subagent` tool builds it inside
    `tools/native`.
  - The child gets no `session_start` or `prompt_submit` hooks and no run lock. Its spending goes
    to the caller's cost meter, and its end fires `subagent_stop`.

So the difference lies in **two wrappers and two catalog builders**, not in two engines.

### 2.2 The sub-agent wrapper (about 4,000 lines without tests)

Sizes of the main files:

| File | Lines |
|---|---|
| `tools/native/subagent.go` | 896 |
| `subagent_manager.go` | 1214 |
| `subagent_call.go` | 520 |
| `subagent_worktree.go` | 369 |
| `subagent_remote.go` | 397 |
| `subagent_store.go` | 258 |

There are also smaller files for resume, async, answer, changes, sandbox and fingerprint.

What these files do:

- **Building the catalog:**
  - `childNativePool` (`:291`) and `newSubagentAgent` (`:628`) choose the tools.
  - `buildCatalogs` (`:424`) makes a `catalog` and a `leaf` catalog for each agent, the leaf one
    without `subagent`, for the deepest level.
  - `nestedSelf` (`:414`) hands the child the owner's own `subagent` tool.
  - `SubagentMCP.Bind`, `BindToolsets`, `hookTool` and `hookWith` (`:170`–`:218`) attach the MCP
    tools and wrap tools with the owning profile's hooks.
  - `describeAgentSandbox` (`subagent_sandbox.go:35`) and `choiceAgent` (P2b) build per-sandbox
    copies of the tools whose descriptions mention the sandbox.
- **Lifecycle:**
  - `SubagentManager` (`subagent_manager.go:58`) handles queueing, the caps (`admitNestedLocked`,
    `:670`) and the background writer slots (`reserveWriter`, `:602`).
  - It also covers async handles, the run store and manifest, recovery after a restart, and the
    stop and delete fan-out.
- **Context:**
  - `SubagentRequest` and `subagentStart` (`subagent_manager.go:540`) carry about 30 fields.
  - The execution context comes from `ExecutionContext.ForNested()` (`tools/catalog.go`), and a
    test that walks every field makes sure nothing is dropped.

### 2.3 The profile_message wrapper

- `profileMessenger.Prepare` (`app/profile_message.go:50`) checks for a cycle, the chain length
  (`max_chain`, default 2, `sessionfs/native_profile_message_settings.go:28`) and a shared
  transcript.
- `app/profile_message_queue.go` (580 lines) keeps a first-in-first-out queue per target and makes
  the async handles.
- `profile_message_sandbox.go` narrows the target by the caller's sandbox (D20).
- `profile_delegation.go` runs the target in a worktree when the sandbox's isolation asks for one.

### 2.4 The definitions already almost match

| | agent (`agent.json`, `nativeSubagentAgentKeys`) | profile (`layoutProfileFile`, `sessionfs/layout_files.go:90`) |
|---|---|---|
| Text | `description`, `instructions`, `prompt_documents` | `description`, `instructions`, `prompt_documents`, `prompt_role`, `name` |
| Model | `model` | `models`, `model_default` |
| Tools | `tools`, `toolset` | `tools`, `toolset` (one or several) |
| Limits | `max_tool_rounds`, `max_run_tokens`, `timeout_ms`, `auto_compaction`, `compaction_trigger_tokens` | `settings` (the full session settings, including compaction and run limits) |
| Context | `state_context` | `settings` |
| Environment | `sandbox`, `isolation` | `settings.sandbox` (isolation inside it) |

### 2.5 Why nesting is hard today

Every capability a child should have needs its own wiring into the native catalog builder. Each of
these was a separate change:

- `async` (2026-10-22);
- `subagent` and `tool_smith` (2026-10-23);
- `profile_message` (2026-10-23);
- `profile_message` for agents that use a toolset (2026-10-25);
- `ForNested` dropping 6 of 17 fields (2026-11-01);
- each agent's own sandbox in its tool descriptions (P2);
- per-choice copies of the catalog (P2b).

The leaf catalog, `nestedSelf`, hook wrapping and MCP binding exist only because the child's catalog
is not built the way a profile's is. A profile reached through `profile_message` from a child uses
a third path again, the chain check.

## 3. Target design

### 3.1 Definitions

- An **agent definition** is a profile-shaped file: the `layoutProfileFile` keys plus the
  agent-only ones, `max_tool_rounds`, `max_run_tokens` and `timeout_ms`, which become run limits
  under `settings`.
- `agents_dir`, inline `agents` and `.jad/profiles` are all resolved by **one loader** into one
  `RunTemplate`.
- Whether a profile can also be started as an ephemeral run is decision U1. Old `agent.json` keys
  keep loading, mapped to the new shape. None are refused unless U2 says so.

### 3.2 Configuration of a run

The run's `SessionConfig` is resolved like a profile's:

1. root defaults;
2. the session;
3. the template, with its own `toolset`, `tools` and `settings`;
4. then narrowed by the caller's context (§3.3).

This replaces `childConfig`, which copies the caller's config and swaps two fields. The visible
changes:

- An agent that sets no model still gets the caller's model, as today, because the session's model
  is the inherited default.
- An agent with no `tools` list gets what the session gives a profile, narrowed by the caller, as
  T-118 already does.

### 3.3 CallerContext

`CallerContext` is a new type, the single thing a caller hands to the run it starts. It is built
from `ExecutionContext.ForNested()` plus what is now in `SubagentRequest` and
`ProfileMessageRequest`.

| Field | Today | Notes |
|---|---|---|
| `Workspace`, `WorkspaceOrigin`, `PathScope` | `ExecutionContext` | A worktree run sets its own |
| `Environment` (sandbox ceiling), `Isolation` | `ExecutionContext.Environment`, sandbox choice | Narrowed, never widened (D20, P2b) |
| `PlanMode` | `ExecutionContext.PlanMode` | |
| `Depth`, `RootRun`, `Chain` | `AgentDepth`, `RootRun`; profile chain | **One** chain for both kinds (§3.5) |
| `Parent` (cancel), `HandleOwner` | the manager's context tree, `ProcessOwner` | Stopping the parent stops every descendant |
| `OnFileChange`, `CostMeter`, `HookReporter` | `ExecutionContext`, `CallerValues` | Ephemeral runs report changes to the caller |
| `RemoteHops`, `RemoteCaller` | `ExecutionContext` | |
| `ModelChoice`, `SandboxChoice` | per-call arguments | |
| `CallerTranscript` | `callerMessagesPathKey` | Used by the shared-transcript check |

A test that walks every field, like `TestForNestedKeepsEveryRunScopedField`, guards this type.

### 3.4 One starter

```go
// app package
type RunSpec struct {
    Template RunTemplateRef // a profile or agent, local or remote
    Kind     RunKind        // Persistent | Ephemeral
    Input    json.RawMessage
    Caller   CallerContext  // zero value for a user-started run
    Seed     string         // resume: transcript to start from (ephemeral)
}
func (r *Resolver) StartRun(ctx context.Context, spec RunSpec) (*RunHandle, error)
```

The starter runs these steps in order:

1. Resolve the template's config, then narrow it by `Caller` (sandbox, isolation, plan mode).
2. Run admission (§3.5).
3. Open the transcript:
   - persistent: the profile's own, behind `runMu`, through the profile queue;
   - ephemeral: a fresh folder, in today's store format.
4. Optionally open a worktree, through the shared materialiser from the isolation work.
5. Call `prepareRun`, so **the catalog comes from the resolver for both kinds**.
6. Run `session.Runner`.
7. Finish: collect from the worktree, fire `subagent_stop` or `run_end`, and report the result.

The `subagent` and `profile_message` tools become thin adapters. Each turns its arguments into a
`RunSpec` and its `RunHandle` into today's result text and async handle.

### 3.5 Admission and limits (one place)

- **Depth:** `max_depth` covers both kinds, counted on `CallerContext.Depth`. Today
  `tools.native.subagent.max_depth` defaults to 2 with a maximum of 8. The maximum can rise, but it
  stays finite. "Unlimited nesting" means a configurable cap, not none, because every level is a
  paid model loop.
- **Run counts:** `max_runs_per_root` and `max_runs_per_session` cover every run in the tree, of
  either kind.
- **Chain and cycle:** a single `Chain` of nodes. A persistent node is (session, profile); an
  ephemeral node is (template, run id). A start whose persistent target is already in the chain is
  refused as a cycle. Ephemeral nodes never form a cycle, because each one is new. `max_chain`
  becomes a cap on persistent hops only, or is folded into `max_depth` (decision U4).
- **Fail fast:** a start from inside a run never waits in a queue for a slot; it fails with
  `busy`. That rule exists today because a parent waiting for its own queued child can deadlock,
  and it stays. A persistent target that is busy still queues its message: the target's queue is
  not the caller's slot, and the cycle check stops A→B→A.

### 3.6 Catalog and tools

- `prepareRun` builds the catalog for (template, effective environment, plan mode), and the result
  is cached on that key. This is where `choiceAgent`'s cache moves to.
- `subagent`, `profile_message`, `async` and `tool_smith` are ordinary tools of the catalog. They
  check depth and the other caps **when called**, not by being left out of the catalog, so the
  `leaf` catalog and `nestedSelf` go away.
- Hooks wrap tools once, in the resolver, for the template's profile, so the `hookTool` and
  `hookWith` wrapping goes away.
- MCP servers stay per session or toolset (D10); this design doesn't change that.

### 3.7 Lifecycle and records

- **One run tree registry** (from `SubagentManager`) holds every run started by a caller, of either
  kind. It handles:
  - the parent-to-child cancel tree;
  - handle ownership;
  - stop, reset and delete fan-out;
  - background writer slots;
  - recovery after a restart.
- User-started persistent runs stay in `httpapi` `activeRun`. Persistent runs started by a caller
  are registered in both places, as `profile_message` handles are today.
- **Checkpoints:**
  - An ephemeral run takes none; the caller's checkpoint covers it, as today (`app/subagent.go:43`).
  - A persistent run takes its own checkpoint, as today.
  - The background-writer gap documented on 2026-10-22 stays.
- **Hooks:**
  - An ephemeral run fires `subagent_stop`, not `session_start` or `prompt_submit`, as today.
  - A persistent run fires its own hooks.
  - Whether an ephemeral run should fire `run_start` and `run_end` is decision U5.

### 3.8 What stays byte-for-byte

- The `messages.jsonl` of both kinds (CLAUDE.md rule 2). The new path writes through the same
  `TurnStore`, and a golden diff runs before and after each phase.
- The sub-agent run manifest and store layout (`subagent_store.go`), so runs from before the change
  can still be resumed.
- The model-facing schemas and result texts of `subagent` and `profile_message` (INFORMATION keys,
  `subagent_depth_exceeded`, `subagent_limit_reached`, `profile_busy`, and the others).
- The remote API `/api/remote/v1`.
- The definition fingerprint (`subagent_fingerprint.go`). If it has to change, an old run gets the
  existing "definition changed" note on resume, and that is documented.

## 4. Decisions for the user

1. **U1:** Can `subagent` start **any profile** as an ephemeral run (`agent: "profiles/coder"`),
   or only definitions in `agents`/`agents_dir`? *Recommendation:* yes, behind an allow-list
   setting like `remotes`, so one definition serves both.
2. **U2:** Keep `agent.json` as a separate file format mapped to the profile shape, or move agents
   into the profile catalog? *Recommendation:* keep it loading, mapped, with no refusals; decide on
   moving later.
3. **U3:** Does a caller's `tools.deny` also narrow the runs it starts? Today a child is narrowed by
   the *session profile's* `tools.deny`. *Recommendation:* yes, it is inherited like the sandbox
   ceiling.
4. **U4:** Keep `max_chain` separately, or count persistent hops in `max_depth`?
   *Recommendation:* use one `max_depth` for both kinds, and keep `max_chain` as an alias for one
   release.
5. **U5:** Hooks of ephemeral runs: is `subagent_stop` enough, or should they also fire
   `run_start` and `run_end`, with a field telling which kind of run it is?
6. **U6:** UI: should ephemeral runs show in the session's run list and SSE stream like persistent
   runs, or stay inside the caller's tool call as today? *Recommendation:* as today in this
   project; a separate frontend task can change it.
7. **U7:** Defaults and maximums: `max_depth` default 2 with maximum 8 today. Raise the maximum
   (for example to 32), and keep the default?

## 5. Phases

Every phase:

- ends with `go test ./internal/... ./cmd/...`, `go vet`, `gofmt -l` and `git diff --check`;
- runs the `messages-golden-diff` check against the build before the phase;
- adds an entry in `backend-go/DECISIONS.md`;
- changes **no behaviour** unless the phase says so.

| Phase | Content | Main files | Risk |
|---|---|---|---|
| R0 | Baseline: golden transcripts for a nested sub-agent tree, a profile_message chain, a resume and a remote call; an inventory of the fields in `SubagentRequest`, `subagentStart` and `ProfileMessageRequest` | tests only | low |
| R1 | `CallerContext` type, built from `ExecutionContext` plus requests, with a field-walk test. `SubagentRequest` and `ProfileMessageRequest` carry it; nothing else changes | `tools/catalog.go`, `native/subagent_call.go`, `native/profile_message.go`, `app/subagent.go`, `app/profile_message.go` | low |
| R2 | `Resolver.StartRun` for **ephemeral** runs: `subagentLauncher.Run` becomes a call to it. The config is still `childConfig` and the catalog is still native. Moves the code without changing behaviour | `app/subagent.go`, new `app/run_start.go` | medium |
| R3 | Template loader: agents resolved like profiles (§3.2), replacing `childConfig`. Old `agent.json` maps on | `sessionfs/native_subagent_*.go`, `layout_load.go`, `layout_files.go`, schemas | **high** (config) |
| R4 | Catalog from `prepareRun` for ephemeral runs, cached per (template, environment, plan mode). Removes `buildCatalogs`, `leaf`, `nestedSelf`, `hookTool`, and the toolset and choice copies. Depth is checked at call time | `native/subagent.go`, `subagent_sandbox.go`, `sandbox_choice.go`, `app/resolver.go` | **high** (tool sets, descriptions in definitions) |
| R5 | Admission and run-tree registry shared by both kinds; one chain; `profile_message` goes through `StartRun` (persistent). The queue stays per target | `native/subagent_manager.go`, `app/profile_message*.go`, `httpapi/profile_run.go` | **high** (concurrency) |
| R6 | New nesting allowed by U1–U4 (for example, `subagent` starting a profile), and the U7 limits | settings, schemas, tests | medium |
| R7 | Remove the dead paths; update the schema text, `sample/` and docs | | low |

R2 to R5 must leave the model-facing definitions, result texts and transcripts unchanged; tests
compare them byte for byte.

## 6. Tests that must exist

- **Nesting matrix**, using a scripted model and real runs, at depths 1, 2 and the cap:
  - ephemeral→ephemeral;
  - ephemeral→persistent;
  - persistent (through `profile_message`)→ephemeral;
  - persistent→persistent;
  - each through a remote hop.
- **Cascades:** stop, reset and delete of the root reach every descendant, and leave no goroutines
  behind (as in `subagent_goroutines_test.go`).
- **Cycles:** A→B→A refused at any depth, with ephemeral levels in between.
- **Fail fast:** a nested start with no free slot answers `busy` and never waits.
- **Concurrency:** two ephemeral runs of the same template at once; two `profile_message` calls to
  one target queue in order.
- **Narrowing:** the sandbox, plan mode and `tools.deny` (U3) of the caller hold at every level,
  with real bwrap where it is present.
- **Compatibility:** an old `agent.json`, a resume of a run recorded before R3 and R4, and a remote
  caller on the old version.
- **Output:** golden `messages.jsonl` and identical tool definitions before and after.

## 7. Risks

- **`messages.jsonl` and tool definition drift:** a catalog built a different way can reorder tools
  or change descriptions. R4 needs a byte-for-byte comparison of definitions for every agent in
  `sample/`.
- **Concurrency:** merging two registries with different lock orders (`SubagentManager.mu`, the
  profile queue, `runMu`). Write the lock order down in R5 before coding.
- **Cost of building catalogs:** `prepareRun` per ephemeral run is heavier than reusing a prebuilt
  catalog, so the cache in §3.6 is required. Measure the time to start a run before and after
  (CLAUDE.md: performance changes need measurements).
- **Isolation:** `Environment` narrowing, the worktree materialiser and `SandboxReader` scheduling
  (P2b item 8) must keep their tests green throughout. That is why this work starts after P4.
- **Size:** R3 to R5 each touch config, concurrency and transcripts. Review each with two
  reviewers.

## 8. Not in scope

- Per-run MCP servers (D10, P6).
- Changes to the frontend (U6).
- Changing the approval model (it was removed; the container is the boundary).
- Merging the two transcript layouts.
