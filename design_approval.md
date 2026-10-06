# Tool Approval Design

**Status:** Proposed. Nothing is implemented. Facts below are marked **read** (seen in code) or **untested**.

## 1. Goal

Let a user require an explicit decision before selected tools run, and let them turn that off. With approval disabled (the default) behaviour must be exactly what it is today.

## 2. What exists today (read)

* No tool asks before running. `request_draft` is a tool the *model* chooses to call.
* Interaction machinery exists and is reusable:
  * `InteractionBroker.begin(question, identity, signal)` publishes a pending question as run state and resolves on a reply, a deadline or abort (`interactions.ts:130`). It **throws if a second question is pending** (`interactions.ts:135`).
  * Replies come through `POST …/run/interaction` (`server.ts:897-908`, `:1447`) with `decision: accept | update | decline`; a decline requires a reason (`parseInteractionReplyRequest`, `interactions.ts:305`). There is an `extend` call for more time.
  * Runs without a frontend that can answer get no `requestInteraction` (`server.ts:2041`, gated by `supports_interaction`); `request_draft` then auto-accepts after `headless_auto_accept_timeout_ms`.
  * The frontend drawer is `DraftRequestDrawer.tsx` (408 lines), built for editing a draft.
* Scheduler: `catalog.isUserDecisionCall(call)` finds a call that needs a decision; the session then runs **only that call** and gives every other call in the response the output `user_decision_required` (`session.ts:572-583`). That is right for `request_draft` and wrong for approvals (it would skip every co-batched call).
* Go has the same structure: `RequiresUserDecision` (`catalog.go:46`), `runner_tools.go:197`, `httpapi/interaction_*.go`. Its code was **not read** beyond these names.
* `bash` is not a sandbox (`bash.ts:12`); its `paths` rules choose the start directory only (`design_bash.md:302`).

## 3. Design

An **approval gate** runs inside the per-call execution wrapper, before `catalog.executeFunctionCall`:

```text
call arrives -> tool needs approval? -> no  -> execute (parallel, as today)
                                      -> yes -> wait for the approval slot (mutex)
                                             -> begin(question{kind:"approval"})
                                             -> accept  -> execute
                                             -> decline / timeout / cancel / headless-deny -> tool output "blocked"
```

* **Per-call, not per-batch.** Calls that need no approval keep running in parallel. Calls that do are asked one at a time because the broker allows one pending question (the mutex serializes `begin`).
* **Denial is a normal tool result**, status `blocked`, `error_code: approval_denied` (or `approval_timeout`, `approval_unavailable`), carrying the user's reason. The model sees it and re-plans; the run does not fail.
* **No `update` decision** for approvals: the reply parser must reject it for `kind: "approval"`, so a user cannot silently run a different command than the one shown.
* **Not the request_draft path.** Approvals do not use `writeAcceptedRequest` or the "user-decision" scheduler hook, so the "only that call runs" rule is untouched.

### 3.1 What the user sees

The pending question carries `kind: "approval"`, the tool name, the call id and a bounded preview:

| Tool | Preview |
|---|---|
| `bash` | command, resolved `cwd`, timeout |
| `write` | path, create or replace, byte count, first bytes |
| `edit` | path, `oldText` and `newText` (bounded), `replaceAll` |
| `process` | action, command |
| MCP tool | server, tool, arguments (bounded) |

Preview size is a config value, not a constant.

### 3.2 Ordering note

Approved calls start when approved, not in the model's order, when `tool_call_scheduling` is `"concurrent"`. With the default `"ordered"` a response that holds a state-changing call runs one call at a time in the model's order, so approvals are asked in that order too (`DECISIONS.md`, "Tool call scheduling").

## 4. Configuration (all defaults; nothing hardcoded)

Per-tool node, consistent with the existing `tools.native.<tool>` settings, plus a session-level block for the shared wait values:

```jsonc
"approval": {
  "timeout_ms": 120000,          // wait for the user, then deny
  "extend_ms": 60000,
  "max_timeout_ms": 600000,
  "headless": "deny",            // "deny" | "allow": runs with no client that can answer
  "preview_bytes": 4096
},
"tools": { "native": {
  "bash":    { "approval": "ask" },   // "off" (default) | "ask"
  "write":   { "approval": "off" },
  "edit":    { "approval": "off" },
  "process": { "approval": "ask" }
}},
"mcp_servers": [ { "id": "x", "approval": "off", "approval_tools": [] } ]
```

* `approval` per tool defaults to `"off"`. With every tool `off`, the gate is never entered and no run state, event or output changes.
* The numbers above are placeholders; real defaults come from the request_draft values already in `config.ts` so the two stay consistent. They need a schema entry in `sample/schemes/setting.schema.json` and config tests in both backends.
* `headless: "deny"` is the safe default when a tool is set to `ask` and nobody can answer. `"allow"` is the explicit opt-out for unattended runs. (Contrast: `request_draft` auto-accepts when headless.)

## 5. Protocol and UI changes

* `PendingInteraction` gains `kind?: "draft" | "approval"` and, for approvals, the preview. Absent `kind` means `draft`, so old clients keep working.
* Frontend: an approval variant of the drawer (approve / deny with reason, extend time, countdown), a badge on the session while waiting, and the existing notification panel entry. `DraftRequestDrawer.tsx` is draft-editing code; the approval view should be a separate small component, not a mode flag inside it.
* Both backends publish the same event and accept the same reply; add differential scenarios.

## 6. `messages.jsonl`

Approved calls produce the same records as today. Denied calls add one ordinary `function_call_output` with the denial text. With approval off nothing is written differently. Prove with the `messages-golden-diff` skill, with the feature off and on.

## 7. What approval does not do (state it in the docs and UI)

* It is not confinement. After approval a `bash` command still runs with the backend user's rights, and a command can fetch and run more code.
* It cannot show what a script will do, only the command text.
* Only listed tools are gated. Other MCP tools, and files changed by an approved process later, are not.
* Real confinement needs an OS-level sandbox (bubblewrap, a container, a separate user). That is a different project.

## 8. Failure handling

* Reply for an unknown or expired interaction: existing 404/409 behaviour.
* Run aborted while waiting: broker cancels, the call returns `approval_cancelled`, nothing executes.
* Server restart while waiting: the run is gone; the question is not resumed. Same as `request_draft` today (**untested** for approvals).
* Two parallel calls needing approval: asked one after another; the second waits behind the first and its timeout starts only when asked.

## 9. Tests

* Config: defaults, validation, schema, both backends.
* Gate: off = no change; ask + accept; ask + decline with reason; timeout; headless deny and allow; abort while waiting; co-batched non-approval call runs without waiting; two approval calls serialize; `update` rejected.
* HTTP: reply validation for `kind: approval`.
* Frontend: approval view, countdown, deny reason required.
* `messages.jsonl` golden diff, off and on.

## 10. Open decisions

1. Shape of the setting: per-tool `approval` (proposed) or one `approval.tools` list.
2. Headless default: `deny` (recommended) or `allow`.
3. "Allow for this run" / "allow this command pattern" shortcuts: not in v1 (recommended); each adds a policy surface to maintain.
4. Whether MCP tools are gated per server or per tool name.
5. Ordering semantics for approved calls (see 3.2), decided together with the ordering bug.
