import assert from "node:assert/strict";
import test from "node:test";

import type {NativeRequestDraftToolSettings} from "../src/config.js";
import {
  changedPaths,
  executeNativeRequestDraft,
  nativeRequestDraftDefinition,
  renderDraftAnswer,
} from "../src/tools/native/request-draft.js";
import type {
  ToolExecutionContext,
  ToolInteractionAnswer,
  ToolInteractionQuestion,
} from "../src/tools/types.js";

const POLICY: NativeRequestDraftToolSettings = {
  timeout_ms: 300_000,
  headless_auto_accept_timeout_ms: 600_000,
  max_timeout_ms: 600_000,
  extend_ms: 120_000,
  max_draft_bytes: 4096,
  max_draft_depth: 4,
  max_title_chars: 40,
};
const CONTEXT: ToolExecutionContext = {callId: "call_1", workspaceRoot: "/tmp", sessionDir: "/tmp"};

function contextWith(
  handler: (question: ToolInteractionQuestion) => Promise<ToolInteractionAnswer>,
): ToolExecutionContext {
  return {...CONTEXT, requestInteraction: handler};
}

function refusingContext(): ToolExecutionContext {
  return contextWith(async () => {
    throw new Error("the tool must not ask with this argument");
  });
}

test("describes itself so the model can call it before starting work", () => {
  assert.equal(nativeRequestDraftDefinition.id, "native.request_draft");
  assert.equal(nativeRequestDraftDefinition.name, "request_draft");
  assert.equal(nativeRequestDraftDefinition.source, "native");
  const schema = nativeRequestDraftDefinition.inputSchema as {
    required: string[];
    properties: Record<string, {type: string}>;
  };
  assert.deepEqual(schema.required, ["draft"]);
  // The wait is the session's, not the model's: the schema has no argument for it.
  assert.equal(schema.properties.timeout_ms, undefined);
  assert.deepEqual(Object.keys(schema.properties).sort(), ["draft", "title"]);
  assert.match(nativeRequestDraftDefinition.description, /accept, edit or decline/);
  assert.match(nativeRequestDraftDefinition.description, /Call it alone before starting work/);
  assert.match(nativeRequestDraftDefinition.description, /defer other calls from the same response/);
  assert.match(nativeRequestDraftDefinition.description, /configured headless timeout/);
  assert.match(nativeRequestDraftDefinition.description, /`changed`/);
});

test("asks the user with the draft, the session's wait and its bounds", async () => {
  const asked: ToolInteractionQuestion[] = [];
  const output = await executeNativeRequestDraft(
    {title: "  How I read your request  ", draft: {goal: "Improve the page", steps: ["a", "b"]}},
    contextWith(async (question) => {
      asked.push(question);
      return {decision: "accept", outcome: "answered"};
    }),
    POLICY,
  );

  assert.deepEqual(asked, [
    {
      title: "How I read your request",
      draft: {goal: "Improve the page", steps: ["a", "b"]},
      timeout_ms: 300_000,
      extend_ms: 120_000,
      max_timeout_ms: 600_000,
      limits: {max_draft_bytes: 4096, max_draft_depth: 4},
    },
  ]);
  assert.deepEqual(JSON.parse(String(output)), {decision: "accept", status: "answered"});
});

test("publishes the session's wait; a timeout the model sends changes nothing", async () => {
  const asked: ToolInteractionQuestion[] = [];
  await executeNativeRequestDraft(
    {draft: {goal: "Improve the page"}, timeout_ms: 1},
    contextWith(async (question) => {
      asked.push(question);
      return {decision: "accept", outcome: "answered"};
    }),
    POLICY,
  );
  assert.equal(asked[0]?.timeout_ms, 300_000, "the configured wait is what the run publishes");
  assert.equal(asked[0]?.extend_ms, 120_000, "the user's extension step travels with the question");
  assert.equal(asked[0]?.max_timeout_ms, 600_000, "and the ceiling for the whole wait");
});

test("hands the user's edited draft back to the model with the changed paths", async () => {
  const output = await executeNativeRequestDraft(
    {draft: {goal: "Improve the page"}},
    contextWith(async () => ({
      decision: "update",
      draft: {goal: "Rewrite the intro"},
      outcome: "answered",
    })),
    POLICY,
  );
  assert.deepEqual(JSON.parse(String(output)), {
    decision: "update",
    status: "answered",
    draft: {goal: "Rewrite the intro"},
    changed: ["goal"],
  });
});

test("names a field the user added and one they replaced", async () => {
  const proposed = {goal: "Improve the page", scope: ["labels", "grouping"]};
  const output = await executeNativeRequestDraft(
    {draft: proposed},
    contextWith(async () => ({
      decision: "update",
      draft: {goal: "Improve the page", scope: {page: "settings", parts: ["labels"]}, deadline: "Friday"},
      outcome: "answered",
    })),
    POLICY,
  );
  assert.deepEqual(JSON.parse(String(output)), {
    decision: "update",
    status: "answered",
    draft: {goal: "Improve the page", scope: {page: "settings", parts: ["labels"]}, deadline: "Friday"},
    changed: ["scope", "deadline"],
  });
});

test("an unchanged answer carries no changed list", () => {
  const draft = {goal: "Improve the page", steps: [{name: "a"}]};
  const answer: ToolInteractionAnswer = {decision: "update", draft: structuredClone(draft), outcome: "answered"};
  assert.deepEqual(JSON.parse(renderDraftAnswer(answer, draft)), {
    decision: "update",
    status: "answered",
    draft,
  });
});

test("changedPaths reports leaves, the middle of a replaced container, and a whole path", () => {
  assert.deepEqual(changedPaths({a: 1}, {a: 1}), []);
  assert.deepEqual(changedPaths({a: 1, b: 2}, {a: 1, b: 3}), ["b"]);
  assert.deepEqual(changedPaths({steps: ["a", "b"]}, {steps: ["a", "c"]}), ["steps[1]"]);
  assert.deepEqual(changedPaths({steps: ["a"]}, {steps: ["a", "b"]}), ["steps[1]"]);
  assert.deepEqual(changedPaths({steps: ["a", "b"]}, {steps: ["a"]}), ["steps[1]"]);
  assert.deepEqual(changedPaths({a: {b: {c: 1}}}, {a: {b: {c: 1, d: 2}}}), ["a.b.d"]);
  assert.deepEqual(changedPaths({a: {b: 1}}, {a: "flat"}), ["a"]);
  assert.deepEqual(changedPaths({a: 1}, {a: 1, b: {c: 2}}), ["b"]);
});

test("refuses a draft larger than the session accepts, before parking the turn", async () => {
  const output = await executeNativeRequestDraft(
    {draft: {notes: "x".repeat(POLICY.max_draft_bytes)}},
    refusingContext(),
    POLICY,
  );
  const parsed = JSON.parse(String(output)) as {
    error: string;
    message: string;
    max_draft_bytes: number;
  };
  assert.equal(parsed.error, "draft_too_large");
  assert.equal(parsed.max_draft_bytes, POLICY.max_draft_bytes);
  assert.match(parsed.message, /at most 4096/);
});

test("refuses a draft nested deeper than the session accepts", async () => {
  const output = await executeNativeRequestDraft(
    {draft: {a: {b: {c: {d: {e: 1}}}}}},
    refusingContext(),
    POLICY,
  );
  const parsed = JSON.parse(String(output)) as {
    error: string;
    message: string;
    max_draft_depth: number;
  };
  assert.equal(parsed.error, "draft_too_deep");
  assert.equal(parsed.max_draft_depth, POLICY.max_draft_depth);
  assert.match(parsed.message, /nests 5 levels/);
});

test("a draft exactly at the depth limit is accepted", async () => {
  const asked: ToolInteractionQuestion[] = [];
  const output = await executeNativeRequestDraft(
    {draft: {a: {b: {c: {d: 1}}}}},
    contextWith(async (question) => {
      asked.push(question);
      return {decision: "accept", outcome: "answered"};
    }),
    POLICY,
  );
  assert.equal(asked.length, 1);
  assert.deepEqual(JSON.parse(String(output)), {decision: "accept", status: "answered"});
});

test("refuses a title longer than the session's line budget", async () => {
  const output = await executeNativeRequestDraft(
    {title: "x".repeat(POLICY.max_title_chars + 1), draft: {goal: "Improve the page"}},
    refusingContext(),
    POLICY,
  );
  const parsed = JSON.parse(String(output)) as {error: string; max_title_chars: number};
  assert.equal(parsed.error, "title_too_long");
  assert.equal(parsed.max_title_chars, POLICY.max_title_chars);
});

test("reports a decline with the user's reason", async () => {
  const output = await executeNativeRequestDraft(
    {draft: {goal: "Improve the page"}},
    contextWith(async () => ({decision: "decline", reason: "too broad", outcome: "answered"})),
    POLICY,
  );
  assert.deepEqual(JSON.parse(String(output)), {
    decision: "decline",
    status: "answered",
    reason: "too broad",
  });
});

test("an unanswered draft reads as a decline the model can act on", () => {
  assert.deepEqual(
    JSON.parse(
      renderDraftAnswer({
        decision: "decline",
        outcome: "timed_out",
        reason: "No answer within 5 minutes; the draft was not confirmed.",
      }),
    ),
    {
      decision: "decline",
      status: "timed_out",
      reason: "No answer within 5 minutes; the draft was not confirmed.",
    },
  );
});

test("a headless run accepts after the configured fallback and reports its source", async () => {
  let resolved: ToolInteractionAnswer | undefined;
  const output = await executeNativeRequestDraft(
    {draft: {goal: "Improve the page"}},
    {
      ...CONTEXT,
      onInteractionResolved: async (answer) => { resolved = answer; },
    },
    {...POLICY, headless_auto_accept_timeout_ms: 5},
  );
  assert.deepEqual(JSON.parse(String(output)), {decision: "accept", status: "answered"});
  assert.deepEqual(resolved, {
    decision: "accept",
    outcome: "answered",
    source: "timeout",
  });
});

test("a draft that is not a JSON object is refused", async () => {
  const output = await executeNativeRequestDraft(
    {draft: ["not", "an", "object"]},
    refusingContext(),
    POLICY,
  );
  assert.deepEqual(JSON.parse(String(output)), {
    error: "invalid_draft",
    message: "draft must be a JSON object.",
  });
});
