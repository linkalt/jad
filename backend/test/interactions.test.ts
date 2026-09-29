import assert from "node:assert/strict";
import test from "node:test";

import {
  InteractionBroker,
  formatWait,
  parseInteractionExtendRequest,
  parseInteractionReplyRequest,
  type InteractionAnswer,
  type PendingInteraction,
} from "../src/interactions.js";
import {SessionRunRegistry} from "../src/run-registry.js";
import type {WebUiMessage} from "../src/plugins/web-ui/types.js";

const QUESTION = {
  draft: {goal: "Ambiguous request", scope: ["one thing"]},
  title: "How I read your request",
  timeout_ms: 60_000,
  extend_ms: 30_000,
  max_timeout_ms: 120_000,
};
const IDENTITY = {tool: "request_draft", tool_call_id: "call_1"};

function message(id: string, text: string): WebUiMessage {
  return {
    id,
    sender: "assistant",
    text,
    timestamp: "12:00",
    parts: [{type: "text", text}],
    status: "streaming",
  };
}

function snapshotOf(event: {event: string; data: unknown} | undefined): WebUiMessage | undefined {
  return event === undefined ? undefined : (event.data as {message: WebUiMessage}).message;
}

test("publishes the question and settles it with the user's decision", async () => {
  const seen: Array<PendingInteraction | undefined> = [];
  const broker = new InteractionBroker((pending) => seen.push(pending));
  const answer = broker.begin(QUESTION, IDENTITY);

  const pending = broker.pending;
  assert.ok(pending, "the question is pending while the tool waits");
  assert.equal(pending.tool, "request_draft");
  assert.equal(pending.tool_call_id, "call_1");
  assert.deepEqual(pending.draft, QUESTION.draft);
  assert.equal(pending.title, "How I read your request");
  assert.equal(
    new Date(pending.expires_at).getTime() - new Date(pending.created_at).getTime(),
    60_000,
    "the published deadline is the tool's timeout",
  );
  assert.equal(
    new Date(pending.max_expires_at).getTime() - new Date(pending.created_at).getTime(),
    120_000,
    "the question carries the ceiling an extension may reach",
  );
  assert.equal(pending.extend_ms, 30_000, "and the step one extension adds");
  assert.deepEqual(seen, [pending], "the question reaches the run once it is pending");

  assert.equal(broker.reply(pending.id, {decision: "accept"}), "answered");
  assert.deepEqual(await answer, {decision: "accept", outcome: "answered"});
  assert.equal(broker.pending, undefined);
  assert.equal(seen.length, 2);
  assert.equal(seen[1], undefined, "clearing the question is published too");
});

test("carries the user's edited draft and their reason back to the tool", async () => {
  const broker = new InteractionBroker(() => {});
  const updated = broker.begin(QUESTION, IDENTITY);
  const edited = {goal: "Rewrite the intro", scope: ["intro"]};
  broker.reply(broker.pending!.id, {decision: "update", draft: edited});
  assert.deepEqual(await updated, {decision: "update", draft: edited, outcome: "answered"});

  const declined = broker.begin(QUESTION, IDENTITY);
  broker.reply(broker.pending!.id, {decision: "decline", reason: "too broad"});
  assert.deepEqual(await declined, {
    decision: "decline",
    reason: "too broad",
    outcome: "answered",
  });
});

test("answers only the question that is waiting", async () => {
  const broker = new InteractionBroker(() => {});
  const answer = broker.begin(QUESTION, IDENTITY);
  assert.equal(broker.reply("some-other-question", {decision: "accept"}), "unknown");
  assert.ok(broker.pending, "a reply for another question leaves this one pending");
  const id = broker.pending.id;
  assert.equal(broker.reply(id, {decision: "accept"}), "answered");
  assert.equal(broker.reply(id, {decision: "accept"}), "unknown", "a second reply changes nothing");
  await answer;
});

test("a question the run cannot answer settles as declined", async () => {
  const broker = new InteractionBroker(() => {});
  const answer = broker.begin({draft: {goal: "x"}, timeout_ms: 10_000}, IDENTITY);
  broker.cancel("The run ended before the draft was answered.");
  assert.deepEqual(await answer, {
    decision: "decline",
    reason: "The run ended before the draft was answered.",
    outcome: "cancelled",
  });
  assert.equal(broker.pending, undefined);
});

test("a stopped run settles the question instead of leaving the tool waiting", async () => {
  const controller = new AbortController();
  const broker = new InteractionBroker(() => {});
  const answer = broker.begin(QUESTION, IDENTITY, controller.signal);
  controller.abort(new Error("run stopped by the user"));
  const settled = await answer;
  assert.equal(settled.outcome, "cancelled");
  assert.equal(settled.decision, "decline");
  assert.match(settled.reason ?? "", /stopped/);
});

test("a question begun on an already stopped run settles without publishing", async () => {
  const controller = new AbortController();
  controller.abort(new Error("already stopped"));
  const seen: Array<PendingInteraction | undefined> = [];
  const broker = new InteractionBroker((pending) => seen.push(pending));
  const answer = broker.begin(QUESTION, IDENTITY, controller.signal);
  assert.equal((await answer).outcome, "cancelled");
  assert.equal(broker.pending, undefined);
  assert.deepEqual(seen, [], "a question nobody can answer is never published");
});

test("nobody answering ends the wait as a decline that names the deadline", async () => {
  const broker = new InteractionBroker(() => {});
  const answer = broker.begin({draft: {}, timeout_ms: 5}, IDENTITY);
  const settled = await answer;
  assert.equal(settled.outcome, "timed_out");
  assert.equal(settled.decision, "decline");
  assert.match(settled.reason ?? "", /No answer within/);
  assert.equal(broker.pending, undefined);
});

test("an answer that arrives after the deadline is reported as expired", async (context) => {
  context.mock.timers.enable({apis: ["setTimeout", "Date"]});
  const broker = new InteractionBroker(() => {});
  const answer = broker.begin({draft: {}, timeout_ms: 60_000}, IDENTITY);
  const id = broker.pending!.id;
  // The deadline passes without the timer callback running: this is the race the
  // endpoint has to report instead of applying a late answer.
  context.mock.timers.setTime(Date.now() + 60_001);
  assert.equal(broker.reply(id, {decision: "accept"}), "expired");
  const settled = await answer;
  assert.equal(settled.outcome, "timed_out");
  assert.equal(broker.pending, undefined);
  context.mock.timers.reset();
});

test("a run publishes the question as message state and replays the cleared state", async () => {
  const registry = new SessionRunRegistry(0);
  const controller = new AbortController();
  const run = registry.begin("session1", undefined, "turn1", controller);
  run.publishMessage(message("m1", "Working on it"));

  const events: Array<{event: string; data: unknown}> = [];
  run.subscribe((event) => events.push(event));
  const answer = run.interactions.begin(QUESTION, IDENTITY, controller.signal);

  const published = snapshotOf(events.filter((event) => event.event === "ui.message").at(-1));
  assert.ok(published?.pending, "the question rides in the message snapshot");
  assert.equal(published.pending.tool_call_id, "call_1");

  run.interactions.reply(published.pending.id, {decision: "accept"});
  assert.deepEqual(await answer, {decision: "accept", outcome: "answered"});
  const cleared = snapshotOf(events.filter((event) => event.event === "ui.message").at(-1));
  assert.equal(cleared?.pending, undefined);

  run.finish("completed", [
    {event: "ui.message", data: {message: message("m2", "Done")}},
    {event: "done", data: {}},
  ]);
  const late: Array<{event: string; data: unknown}> = [];
  run.subscribe((event) => late.push(event));
  assert.deepEqual(
    late.map((event) => event.event),
    ["ui.message", "done"],
    "a late subscriber still receives the terminal state",
  );
  const replayed = late.map(snapshotOf).filter((candidate) => candidate !== undefined);
  assert.equal(
    replayed.some((candidate) => candidate.pending !== undefined),
    false,
    "a question the finished run cannot answer is never replayed",
  );
});

test("an extension moves the deadline by its step, up to the session's ceiling", async () => {
  const seen: Array<PendingInteraction | undefined> = [];
  const broker = new InteractionBroker((pending) => seen.push(pending));
  const answer = broker.begin(QUESTION, IDENTITY);
  const id = broker.pending!.id;
  const created = Date.parse(broker.pending!.created_at);

  assert.equal(broker.extend("some-other-question"), "unknown");
  assert.equal(broker.extend(id), "extended");
  assert.equal(
    Date.parse(broker.pending!.expires_at) - created,
    90_000,
    "the step is added to the deadline",
  );
  assert.equal(
    seen.at(-1)?.expires_at,
    broker.pending!.expires_at,
    "the run republishes the question with its new deadline",
  );

  assert.equal(broker.extend(id), "extended");
  assert.equal(
    Date.parse(broker.pending!.expires_at) - created,
    120_000,
    "the ceiling caps the whole wait",
  );
  assert.equal(broker.extend(id), "at_max", "a question at its longest wait cannot be extended again");
  assert.equal(Date.parse(broker.pending!.expires_at) - created, 120_000);

  assert.equal(broker.reply(id, {decision: "accept"}), "answered");
  assert.deepEqual(await answer, {decision: "accept", outcome: "answered"});
});

test("the timer follows the extended deadline instead of the first one", async (context) => {
  context.mock.timers.enable({apis: ["setTimeout", "Date"]});
  const broker = new InteractionBroker(() => {});
  const answer = broker.begin(
    {draft: {}, timeout_ms: 60_000, extend_ms: 60_000, max_timeout_ms: 180_000},
    IDENTITY,
  );
  const id = broker.pending!.id;
  context.mock.timers.setTime(Date.now() + 50_000);
  assert.equal(broker.extend(id), "extended");
  // The original deadline passes with the extension in place: the wait goes on.
  context.mock.timers.tick(20_000);
  assert.equal(broker.pending?.id, id, "the wait outlives the original deadline");
  context.mock.timers.tick(50_000);
  const settled = await answer;
  assert.equal(settled.outcome, "timed_out");
  assert.match(settled.reason ?? "", /2 minutes/, "the reason names the whole wait");
  context.mock.timers.reset();
});

test("a deadline already reached cannot be extended", async (context) => {
  context.mock.timers.enable({apis: ["setTimeout", "Date"]});
  const broker = new InteractionBroker(() => {});
  const answer = broker.begin({draft: {}, timeout_ms: 60_000, extend_ms: 60_000}, IDENTITY);
  const id = broker.pending!.id;
  context.mock.timers.setTime(Date.now() + 60_001);
  assert.equal(broker.extend(id), "expired");
  const settled = await answer;
  assert.equal(settled.outcome, "timed_out");
  assert.equal(broker.pending, undefined);
  context.mock.timers.reset();
});

test("only a well-formed extension names the question it moves", () => {
  assert.deepEqual(parseInteractionExtendRequest({interaction_id: "i1"}), {interaction_id: "i1"});
  const rejected = [undefined, "i1", {}, [], {interaction_id: ""}, {interaction_id: 7}];
  for (const value of rejected) {
    const parsed = parseInteractionExtendRequest(value);
    assert.ok("error" in parsed, `expected a refusal for ${JSON.stringify(value)}`);
  }
});

test("a record of the wait reads as minutes or seconds", () => {
  assert.equal(formatWait(300_000), "5 minutes");
  assert.equal(formatWait(60_000), "a minute");
  assert.equal(formatWait(90_000), "90 seconds");
  assert.equal(formatWait(1_000), "a second");
  assert.equal(formatWait(1), "a second");
});

test("only a well-formed reply reaches the pending question", () => {
  assert.deepEqual(parseInteractionReplyRequest({interaction_id: "i1", decision: "accept"}), {
    decision: "accept",
    interaction_id: "i1",
  });
  assert.deepEqual(
    parseInteractionReplyRequest({interaction_id: "i1", decision: "update", draft: {goal: "edited"}}),
    {decision: "update", draft: {goal: "edited"}, interaction_id: "i1"},
  );
  assert.deepEqual(
    parseInteractionReplyRequest({interaction_id: "i1", decision: "decline", reason: "  too vague  "}),
    {decision: "decline", reason: "too vague", interaction_id: "i1"},
  );
  // An accept keeps the tool's own draft: an edited one sent alongside is ignored.
  assert.deepEqual(
    parseInteractionReplyRequest({interaction_id: "i1", decision: "accept", draft: {goal: "edited"}}),
    {decision: "accept", interaction_id: "i1"},
  );

  const rejected = [
    undefined,
    "accept",
    {},
    {interaction_id: "", decision: "accept"},
    {interaction_id: "i1", decision: "maybe"},
    {interaction_id: "i1", decision: "update"},
    {interaction_id: "i1", decision: "update", draft: "not an object"},
    {interaction_id: "i1", decision: "decline"},
    {interaction_id: "i1", decision: "decline", reason: "   "},
    {interaction_id: "i1", decision: "accept", reason: 7},
    {interaction_id: "i1", decision: "decline", reason: "x".repeat(2_001)},
  ];
  for (const value of rejected) {
    const parsed = parseInteractionReplyRequest(value);
    assert.ok("error" in parsed, `expected a refusal for ${JSON.stringify(value)?.slice(0, 60)}`);
  }
});

test("the question carries the bounds the user's edit is measured against", async () => {
  const broker = new InteractionBroker(() => {});
  const limits = {max_draft_bytes: 2048, max_draft_depth: 4};
  const answer = broker.begin({...QUESTION, limits}, IDENTITY);
  assert.deepEqual(broker.limits, limits, "the endpoint reads the limits of the waiting question");
  broker.cancel("done");
  await answer;
  assert.equal(broker.limits, undefined, "nothing is pending, so nothing is bounded");
});

test("an edited draft beyond the question's bounds is refused before it is applied", async () => {
  const limits = {max_draft_bytes: 512, max_draft_depth: 3};
  const deep = {a: {b: {c: {d: 1}}}};
  const tooDeep = parseInteractionReplyRequest(
    {interaction_id: "i1", decision: "update", draft: deep},
    limits,
  );
  assert.ok("error" in tooDeep);
  assert.match(tooDeep.error, /nests 4 levels; the limit is 3/);

  const tooLarge = parseInteractionReplyRequest(
    {interaction_id: "i1", decision: "update", draft: {notes: "x".repeat(600)}},
    limits,
  );
  assert.ok("error" in tooLarge);
  assert.match(tooLarge.error, /limit is 512/);

  assert.deepEqual(
    parseInteractionReplyRequest(
      {interaction_id: "i1", decision: "update", draft: {goal: "shorter"}},
      limits,
    ),
    {decision: "update", draft: {goal: "shorter"}, interaction_id: "i1"},
    "an edit inside the bounds still passes",
  );
  // Without limits the parser validates shape only; the tool's own policy is what
  // a live session always sends.
  assert.ok(
    !("error" in parseInteractionReplyRequest({interaction_id: "i1", decision: "update", draft: deep})),
  );
});

test("one question at a time, because the runner executes calls in order", async () => {
  const broker = new InteractionBroker(() => {});
  const answer: Promise<InteractionAnswer> = broker.begin(QUESTION, IDENTITY);
  assert.throws(() => broker.begin(QUESTION, IDENTITY), /already pending/);
  broker.cancel("done");
  await answer;
});
