import assert from "node:assert/strict";
import test from "node:test";

import {AsyncMutex} from "../src/concurrency.js";
import {InteractionBroker, parseInteractionReplyRequest} from "../src/interactions.js";
import type {ApprovalSettings} from "../src/config.js";
import {
  approvalPreview,
  callNeedsApproval,
  createApprovalRequester,
  cutText,
} from "../src/tools/approval.js";
import type {ToolInteractionAnswer, ToolInteractionQuestion} from "../src/tools/types.js";

const SETTINGS: ApprovalSettings = {
  default: "off",
  timeout_ms: 60_000,
  extend_ms: 30_000,
  max_timeout_ms: 120_000,
  headless: "deny",
  preview_bytes: 64,
};

const CALL = {callId: "call-1", modelToolName: "bash", tool: "bash", args: {command: "ls"}};

function answer(overrides: Partial<ToolInteractionAnswer>): ToolInteractionAnswer {
  return {decision: "accept", outcome: "answered", source: "user", ...overrides};
}

test("cutText keeps whole characters and reports the real size", () => {
  assert.deepEqual(cutText("hello", 10), {text: "hello", truncated: false, bytes: 5});
  // "é" is two bytes; cutting at 3 must not leave half of the second one.
  const cut = cutText("ééé", 3);
  assert.equal(cut.text, "é");
  assert.equal(cut.truncated, true);
  assert.equal(cut.bytes, 6);
});

test("preview shows what a bash command will do and cuts long text", () => {
  const long = "x".repeat(200);
  const preview = approvalPreview("bash", undefined, {command: long, cwd: "sub", timeout_ms: 5000}, 64);
  assert.equal(preview.title, "Run shell command");
  assert.equal((preview.draft.command as string).length, 64);
  assert.equal(preview.draft.command_truncated_bytes, 200);
  assert.equal(preview.draft.cwd, "sub");
  assert.equal(preview.draft.timeout_ms, 5000);
});

test("preview of write omits base64 content but reports its size", () => {
  const preview = approvalPreview("write", undefined, {path: "a.bin", content: "AAAA", contentEncoding: "base64"}, 64);
  assert.equal(preview.draft.content, undefined);
  assert.equal(preview.draft.content_bytes, 4);
  assert.equal(preview.draft.contentEncoding, "base64");
});

test("preview of edit shows both texts and replaceAll", () => {
  const preview = approvalPreview("edit", undefined, {path: "a.ts", oldText: "a", newText: "b", replaceAll: true}, 64);
  assert.deepEqual(preview.draft, {tool: "edit", path: "a.ts", oldText: "a", newText: "b", replaceAll: true});
});

test("preview of an MCP call names the server and shows its arguments", () => {
  const preview = approvalPreview("echo", "fixture", {text: "hi"}, 64);
  assert.equal(preview.title, "Run fixture tool echo");
  assert.equal(preview.draft.server, "fixture");
  assert.equal(preview.draft.arguments, '{"text":"hi"}');
});

test("only a process start needs approval", () => {
  assert.equal(callNeedsApproval("process", {command: "sleep 9"}), true);
  assert.equal(callNeedsApproval("process", {action: "start", command: "sleep 9"}), true);
  assert.equal(callNeedsApproval("process", {action: "stop", id: "p1"}), false);
  assert.equal(callNeedsApproval("process", {id: "p1"}), false);
  assert.equal(callNeedsApproval("process", {}), false);
  assert.equal(callNeedsApproval("bash", {command: "ls"}), true);
});

test("a process that is not started does not ask", async () => {
  let asked = 0;
  const request = createApprovalRequester({
    settings: SETTINGS,
    gate: new AsyncMutex(),
    ask: async () => {
      asked += 1;
      return answer({});
    },
  });
  const verdict = await request({callId: "c", modelToolName: "process", tool: "process", args: {action: "list"}});
  assert.deepEqual(verdict, {allowed: true});
  assert.equal(asked, 0);
});

test("accept lets the call run and the question carries kind approval, the preview and the call identity", async () => {
  const seen: Array<{question: ToolInteractionQuestion; identity: {tool: string; tool_call_id: string}}> = [];
  const request = createApprovalRequester({
    settings: SETTINGS,
    gate: new AsyncMutex(),
    ask: async (question, identity) => {
      seen.push({question, identity});
      return answer({});
    },
  });
  assert.deepEqual(await request(CALL), {allowed: true});
  assert.equal(seen.length, 1);
  assert.equal(seen[0]!.question.kind, "approval");
  assert.equal(seen[0]!.question.limits, undefined);
  assert.equal(seen[0]!.question.timeout_ms, 60_000);
  assert.equal(seen[0]!.question.extend_ms, 30_000);
  assert.equal(seen[0]!.question.max_timeout_ms, 120_000);
  assert.equal(seen[0]!.question.draft.command, "ls");
  assert.deepEqual(seen[0]!.identity, {tool: "bash", tool_call_id: "call-1"});
});

test("decline blocks with the user's reason", async () => {
  const request = createApprovalRequester({
    settings: SETTINGS,
    gate: new AsyncMutex(),
    ask: async () => answer({decision: "decline", reason: "too risky"}),
  });
  assert.deepEqual(await request(CALL), {allowed: false, code: "approval_denied", reason: "too risky"});
});

test("timeout and cancellation block with their own codes", async () => {
  const timeout = createApprovalRequester({
    settings: SETTINGS,
    gate: new AsyncMutex(),
    ask: async () => answer({decision: "decline", outcome: "timed_out", source: "timeout", reason: "late"}),
  });
  const cancelled = createApprovalRequester({
    settings: SETTINGS,
    gate: new AsyncMutex(),
    ask: async () => answer({decision: "decline", outcome: "cancelled", reason: "stopped"}),
  });
  const timedOut = await timeout(CALL);
  const stopped = await cancelled(CALL);
  assert.equal(timedOut.allowed, false);
  assert.equal(timedOut.allowed === false && timedOut.code, "approval_timeout");
  assert.equal(stopped.allowed === false && stopped.code, "approval_cancelled");
});

test("with nobody to ask, headless deny blocks and headless allow runs", async () => {
  const deny = createApprovalRequester({settings: SETTINGS, gate: new AsyncMutex(), ask: undefined});
  const denied = await deny(CALL);
  assert.equal(denied.allowed, false);
  assert.equal(denied.allowed === false && denied.code, "approval_unavailable");

  const allow = createApprovalRequester({
    settings: {...SETTINGS, headless: "allow"},
    gate: new AsyncMutex(),
    ask: undefined,
  });
  assert.deepEqual(await allow(CALL), {allowed: true});
});

test("two calls that need approval are asked one after the other", async () => {
  let open = 0;
  let overlap = 0;
  const order: string[] = [];
  const request = createApprovalRequester({
    settings: SETTINGS,
    gate: new AsyncMutex(),
    ask: async (_question, identity) => {
      open += 1;
      overlap = Math.max(overlap, open);
      order.push(`start ${identity.tool_call_id}`);
      await new Promise((resolve) => setTimeout(resolve, 20));
      order.push(`end ${identity.tool_call_id}`);
      open -= 1;
      return answer({});
    },
  });
  await Promise.all([request({...CALL, callId: "a"}), request({...CALL, callId: "b"})]);
  assert.equal(overlap, 1, "the broker holds one question, so two must never be open at once");
  assert.deepEqual(order, ["start a", "end a", "start b", "end b"]);
});

test("a broker accepts two approvals in turn through the requester without throwing", async () => {
  const broker = new InteractionBroker(() => undefined);
  const request = createApprovalRequester({
    settings: SETTINGS,
    gate: new AsyncMutex(),
    ask: (question, identity) => broker.begin(question, identity),
  });
  const first = request({...CALL, callId: "a"});
  const second = request({...CALL, callId: "b"});
  const firstId = await waitForPending(broker);
  assert.equal(broker.pending?.kind, "approval");
  assert.equal(broker.reply(firstId, {decision: "accept"}), "answered");
  assert.deepEqual(await first, {allowed: true});
  const secondId = await waitForPending(broker, firstId);
  assert.equal(broker.reply(secondId, {decision: "decline", reason: "no"}), "answered");
  const denied = await second;
  assert.equal(denied.allowed, false);
});

test("a stopped run turns a waiting approval into approval_cancelled", async () => {
  const broker = new InteractionBroker(() => undefined);
  const controller = new AbortController();
  const request = createApprovalRequester({
    settings: SETTINGS,
    gate: new AsyncMutex(),
    ask: (question, identity) => broker.begin(question, identity, controller.signal),
  });
  const waiting = request(CALL);
  await waitForPending(broker);
  controller.abort();
  const verdict = await waiting;
  assert.equal(verdict.allowed, false);
  assert.equal(verdict.allowed === false && verdict.code, "approval_cancelled");
});

test("the reply parser refuses update for an approval but still accepts it for a draft", () => {
  const body = {interaction_id: "i1", decision: "update", draft: {a: 1}};
  const refused = parseInteractionReplyRequest(body, undefined, "approval");
  assert.ok("error" in refused);
  assert.match(refused.error, /accept.*decline/);
  assert.ok(!("error" in parseInteractionReplyRequest(body)));
  assert.ok(!("error" in parseInteractionReplyRequest({interaction_id: "i1", decision: "accept"}, undefined, "approval")));
});

test("declining an approval still requires a reason", () => {
  const parsed = parseInteractionReplyRequest({interaction_id: "i1", decision: "decline"}, undefined, "approval");
  assert.ok("error" in parsed);
  assert.match(parsed.error, /reason/);
});

async function waitForPending(broker: InteractionBroker, previousId?: string): Promise<string> {
  for (let attempt = 0; attempt < 200; attempt += 1) {
    const pending = broker.pending;
    if (pending !== undefined && pending.id !== previousId) {
      return pending.id;
    }
    await new Promise((resolve) => setTimeout(resolve, 5));
  }
  throw new Error("no approval became pending");
}
