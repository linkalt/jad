import assert from "node:assert/strict";
import {mkdtemp, readdir, rm} from "node:fs/promises";
import {tmpdir} from "node:os";
import {join} from "node:path";
import test from "node:test";

import {RunContentCache} from "../src/run-content-cache.js";
import { SessionRunRegistry } from "../src/run-registry.js";
import { assistantMessage, fieldsFromParts } from "../src/plugins/web-ui/shared-web-ui.js";
import type { WebUiInteraction, WebUiMessage, WebUiPart } from "../src/plugins/web-ui/types.js";

function message(id: string, text: string, status: "streaming" | "complete"): WebUiMessage {
  return {
    id,
    sender: "assistant",
    text,
    timestamp: "12:00",
    parts: [{type: "text", text}],
    status,
  };
}

function summaryRefs(registry: SessionRunRegistry, sessionRef: string): string[] {
  const run = registry.current(sessionRef, undefined);
  return run === undefined ? [] : [run.summary().turn_id];
}

/** The message-level fields the server derives from the parts. */
function withParts(
  id: string,
  parts: WebUiPart[],
  status: "streaming" | "complete" = "streaming",
): WebUiMessage {
  return assistantMessage(id, parts, "12:00", status);
}

/** The client half of the protocol: replays the increments onto the held message. */
function appliedMessage(events: readonly {event: string; data: unknown}[]): WebUiMessage | undefined {
  let current: WebUiMessage | undefined;
  for (const item of events) {
    if (item.event === "ui.message") {
      current = (item.data as {message: WebUiMessage}).message;
      continue;
    }
    if (current === undefined) {
      continue;
    }
    const data = item.data as {index: number; append?: string; part?: WebUiPart};
    const parts = current.parts.slice();
    const part = parts[data.index];
    if (item.event === "ui.delta" && part !== undefined) {
      const append = data.append ?? "";
      parts[data.index] =
        part.type === "tool_call"
          ? {...part, arguments: part.arguments + append}
          : part.type === "tool_result"
            ? {...part, output: part.output + append}
            : {...part, text: part.text + append};
    } else if (item.event === "ui.part" && data.part !== undefined) {
      parts[data.index] = data.part;
    } else {
      continue;
    }
    current = {...current, parts, ...fieldsFromParts(parts)};
  }
  return current;
}

test("publishes and replays a pending interaction without requiring a UI message", () => {
  const registry = new SessionRunRegistry(60_000);
  const run = registry.begin("session1", undefined, "web-turn-1", new AbortController());
  const pending: WebUiInteraction = {
    id: "interaction-1",
    tool: "request_draft",
    tool_call_id: "call-draft",
    draft: {goal: "write a file"},
    created_at: "2026-01-01T00:00:00.000Z",
    expires_at: "2026-01-01T00:01:00.000Z",
    max_expires_at: "2026-01-01T00:02:00.000Z",
    extend_ms: 30_000,
  };
  const live: Array<{event: string; data: unknown}> = [];
  const detach = run.subscribe(event => live.push(event));

  run.setPending(pending);
  assert.deepEqual(live, [{event: "interaction", data: {turn_id: "web-turn-1", pending}}]);

  const reattached: Array<{event: string; data: unknown}> = [];
  run.subscribe(event => reattached.push(event));
  assert.deepEqual(reattached, [{event: "interaction", data: {turn_id: "web-turn-1", pending}}]);

  const proposal = message("web-turn-1", "proposal", "streaming");
  run.publishMessage(proposal);
  assert.equal(run.latest?.pending?.id, pending.id);
  run.setPending(undefined);
  assert.deepEqual(live.slice(-2), [
    {event: "interaction", data: {turn_id: "web-turn-1", pending: null}},
    {event: "ui.message", data: {message: proposal}},
  ]);
  const afterClear: Array<{event: string; data: unknown}> = [];
  run.subscribe(event => afterClear.push(event));
  assert.deepEqual(afterClear, [
    {event: "ui.message", data: {message: proposal}},
    {event: "interaction", data: {turn_id: "web-turn-1", pending: null}},
  ]);

  detach();
  run.finish("aborted", [{event: "done", data: {}}]);
  registry.clear();
});

test("describes a run while it is in flight and after it finishes", () => {
  const registry = new SessionRunRegistry(60_000);
  const controller = new AbortController();
  assert.equal(registry.describe("session1", undefined), undefined);

  const run = registry.begin("session1", undefined, "web-turn-1", controller);
  assert.deepEqual(registry.describe("session1", undefined), {
    turn_id: "web-turn-1",
    started_at: run.summary().started_at,
    status: "running",
  });

  run.publishMessage(message("web-turn-1", "partial", "streaming"));
  run.finish("completed", [
    {event: "ui.message", data: {message: message("web-turn-1", "done", "complete")}},
    {event: "completed", data: {message: message("web-turn-1", "done", "complete"), committed: true}},
    {event: "done", data: {}},
  ], message("web-turn-1", "done", "complete"));

  assert.equal(registry.describe("session1", undefined)?.status, "completed");
  assert.equal(registry.current("session1", undefined)?.latest?.text, "done");
  registry.clear();
});

test("a subscriber that attaches mid-run replays the newest message, then receives what changed", () => {
  const registry = new SessionRunRegistry(60_000);
  const run = registry.begin("session1", undefined, "web-turn-1", new AbortController());
  run.publishMessage(message("web-turn-1", "first", "streaming"));

  const seen: string[] = [];
  const detach = run.subscribe(event => seen.push(event.event));
  assert.deepEqual(seen, ["ui.message"]);

  const events: unknown[] = [];
  run.subscribe(event => events.push(event.data));
  run.publishMessage(message("web-turn-1", "first and second", "streaming"));
  // The replayed message is what a late subscriber starts from; the next state is
  // the appended suffix only, not another whole message.
  assert.deepEqual(events, [
    {message: message("web-turn-1", "first", "streaming")},
    {id: "web-turn-1", index: 0, append: " and second"},
  ]);

  detach();
  run.publishMessage(message("web-turn-1", "first and second and third", "streaming"));
  assert.deepEqual(events[events.length - 1], {id: "web-turn-1", index: 0, append: " and third"});
  registry.clear();
});

test("replays the latest usage summary when a subscriber reconnects", () => {
  const registry = new SessionRunRegistry(60_000);
  const run = registry.begin("session1", undefined, "web-turn-1", new AbortController());
  const firstUsage = {completed_responses: 1, responses_with_usage: 0, turn_count: 1, history_partial: false};
  const latestUsage = {completed_responses: 2, responses_with_usage: 1, turn_count: 1, history_partial: false};
  run.publish("usage", {usage: firstUsage});
  run.publish("usage", {usage: latestUsage});
  run.publishMessage(message("web-turn-1", "partial", "streaming"));

  const liveEvents: {event: string; data: unknown}[] = [];
  run.subscribe(event => liveEvents.push(event));
  assert.deepEqual(liveEvents.map(event => event.event), ["ui.message", "usage"]);
  assert.deepEqual(liveEvents[1]?.data, {usage: latestUsage});

  const final = message("web-turn-1", "answer", "complete");
  run.finish("completed", [
    {event: "ui.message", data: {message: final}},
    {event: "completed", data: {message: final, committed: true}},
    {event: "done", data: {}},
  ], final);
  const replayedEvents: {event: string; data: unknown}[] = [];
  run.subscribe(event => replayedEvents.push(event));
  assert.deepEqual(replayedEvents.map(event => event.event), ["usage", "ui.message", "completed", "done"]);
  assert.deepEqual(replayedEvents[0]?.data, {usage: latestUsage});
  registry.clear();
});

test("a subscriber that attaches after the run ended replays the terminal events", () => {
  const registry = new SessionRunRegistry(60_000);
  const run = registry.begin("session1", undefined, "web-turn-1", new AbortController());
  const final = message("web-turn-1", "answer", "complete");
  run.finish("completed", [
    {event: "ui.message", data: {message: final}},
    {event: "completed", data: {message: final, committed: true}},
    {event: "done", data: {}},
  ], final);

  const seen: string[] = [];
  run.subscribe(event => seen.push(event.event));
  assert.deepEqual(seen, ["ui.message", "completed", "done"]);
  registry.clear();
});

test("stop aborts the run in flight and ignores finished runs", () => {
  const registry = new SessionRunRegistry(60_000);
  const controller = new AbortController();
  const run = registry.begin("session1", undefined, "web-turn-1", controller);

  const result = registry.stop("session1", undefined);
  assert.deepEqual(result, {stopped: true, turn_id: "web-turn-1"});
  assert.equal(controller.signal.aborted, true);

  run.finish("aborted", [{event: "error", data: {message: "stopped"}}, {event: "done", data: {}}]);
  assert.deepEqual(registry.stop("session1", undefined), {stopped: false});
  assert.deepEqual(registry.stop("other-session", undefined), {stopped: false});
  registry.clear();
});

test("abortAll aborts every live run", () => {
  const registry = new SessionRunRegistry(60_000);
  const first = new AbortController();
  const second = new AbortController();
  registry.begin("session1", undefined, "web-turn-1", first);
  registry.begin("session2", undefined, "web-turn-2", second);

  registry.abortAll(new Error("server_close"));
  assert.equal(first.signal.aborted, true);
  assert.equal(second.signal.aborted, true);
  registry.clear();
});

test("a finished run is dropped after the retention window", async () => {
  const registry = new SessionRunRegistry(20);
  const run = registry.begin("session1", undefined, "web-turn-1", new AbortController());
  run.finish("completed", [{event: "done", data: {}}]);
  assert.deepEqual(summaryRefs(registry, "session1"), ["web-turn-1"]);

  await new Promise(resolve => setTimeout(resolve, 60));
  assert.equal(registry.current("session1", undefined), undefined);
  assert.equal(registry.describe("session1", undefined), undefined);
  registry.clear();
});

test("retention zero drops a finished run immediately", () => {
  const registry = new SessionRunRegistry(0);
  const run = registry.begin("session1", undefined, "web-turn-1", new AbortController());
  run.finish("failed", [{event: "done", data: {}}]);
  assert.equal(registry.current("session1", undefined), undefined);
  registry.clear();
});

test("keeps a queued second run separate from the running one", () => {
  const registry = new SessionRunRegistry(60_000);
  const first = registry.begin("session1", undefined, "web-turn-1", new AbortController());
  registry.begin("session1", undefined, "web-turn-2", new AbortController());

  // Attaching and stopping follow the run that is actually executing.
  assert.equal(registry.current("session1", undefined)?.turnId, "web-turn-1");
  assert.deepEqual(registry.stop("session1", undefined), {stopped: true, turn_id: "web-turn-1"});

  first.finish("aborted", [{event: "done", data: {}}]);
  assert.equal(registry.current("session1", undefined)?.turnId, "web-turn-2");
  registry.clear();
});

test("publishes growth as a suffix, a new or replaced part once, and the rest as a snapshot", () => {
  const registry = new SessionRunRegistry(60_000);
  const run = registry.begin("session1", undefined, "web-turn-1", new AbortController());
  const events: Array<{event: string; data: unknown}> = [];
  run.subscribe((event) => events.push({event: event.event, data: event.data}));

  const partial: WebUiPart = {
    type: "tool_call",
    callId: "call_read",
    name: "read",
    arguments: '{"pa',
    partial: true,
  };
  run.publishMessage(withParts("web-turn-1", [partial]));
  events.length = 0;

  // Growth — streamed text, reasoning and tool arguments — is the appended suffix.
  const grown: WebUiPart = {...partial, arguments: '{"path":"sample.txt"}'};
  run.publishMessage(withParts("web-turn-1", [grown]), {index: 0, append: 'th":"sample.txt"}'});
  assert.deepEqual(events, [
    {event: "ui.delta", data: {id: "web-turn-1", index: 0, append: 'th":"sample.txt"}'}},
  ]);
  events.length = 0;

  // The finished call is authoritative: it replaces the streamed part whole.
  const finished: WebUiPart = {
    type: "tool_call",
    callId: "call_read",
    name: "read",
    arguments: '{"path":"sample.txt"}',
  };
  run.publishMessage(withParts("web-turn-1", [finished]));
  assert.deepEqual(events, [{event: "ui.part", data: {id: "web-turn-1", index: 0, part: finished}}]);
  events.length = 0;

  // A new part — a tool result — is sent once.
  const result: WebUiPart = {
    type: "tool_result",
    callId: "call_read",
    output: "sample content\n",
    status: "success",
  };
  run.publishMessage(withParts("web-turn-1", [finished, result]));
  assert.deepEqual(events, [{event: "ui.part", data: {id: "web-turn-1", index: 1, part: result}}]);
  events.length = 0;

  // A status change has no incremental form.
  run.publishMessage(withParts("web-turn-1", [finished, result], "complete"));
  assert.deepEqual(events.map((item) => item.event), ["ui.message"]);
  registry.clear();
});

test("publishes a full snapshot when step ranges change, then keeps text growth incremental", () => {
  const registry = new SessionRunRegistry(60_000);
  const run = registry.begin("session1", undefined, "web-step-window", new AbortController());
  const events: Array<{event: string; data: unknown}> = [];
  run.subscribe((event) => events.push({event: event.event, data: event.data}));

  const earlierParts: WebUiPart[] = [
    {type: "reasoning", text: "Thinking"},
    {type: "tool_call", callId: "call-1", name: "read", arguments: "{}"},
    {type: "tool_result", callId: "call-1", output: "result", status: "success"},
  ];
  run.publishMessage(withParts("web-step-window", earlierParts));
  events.length = 0;

  const withSummary = {
    ...withParts("web-step-window", [...earlierParts, {type: "text", text: "Summary"}]),
    steps: [
      {id: "response-tool", startPart: 0, endPart: 3, hasTools: true},
      {id: "response-final", startPart: 3, endPart: 4, hasTools: false},
    ],
  };
  run.publishMessage(withSummary);
  assert.deepEqual(events.map((event) => event.event), ["ui.message"]);
  assert.deepEqual(appliedMessage(events), withSummary);
  events.length = 0;

  const summaryGrowth = {
    ...withParts("web-step-window", [...earlierParts, {type: "text", text: "Summary is ready"}]),
    steps: withSummary.steps,
  };
  run.publishMessage(summaryGrowth, {index: 3, append: " is ready"});
  assert.deepEqual(events.map((event) => event.event), ["ui.delta"]);
  assert.deepEqual(events[0]?.data, {id: "web-step-window", index: 3, append: " is ready"});
  registry.clear();
});

test("rejects append metadata when the message step shape changes", () => {
  const registry = new SessionRunRegistry(60_000);
  const run = registry.begin("session1", undefined, "web-step-shape", new AbortController());
  const events: Array<{event: string; data: unknown}> = [];
  run.subscribe((event) => events.push({event: event.event, data: event.data}));

  run.publishMessage(withParts("web-step-shape", [{type: "text", text: "answer"}]));
  events.length = 0;
  const next = {
    ...withParts("web-step-shape", [{type: "text", text: "answer ready"}]),
    steps: [{id: "step-1", startPart: 0, endPart: 1, hasTools: false}],
  };
  run.publishMessage(next, {index: 0, append: " ready"});

  assert.deepEqual(events.map((event) => event.event), ["ui.message"]);
  assert.deepEqual(appliedMessage(events), next);
  registry.clear();
});

test("applying every event lands on the message the run holds", () => {
  const registry = new SessionRunRegistry(60_000);
  const run = registry.begin("session1", undefined, "web-turn-1", new AbortController());
  const events: Array<{event: string; data: unknown}> = [];
  run.subscribe((event) => events.push({event: event.event, data: event.data}));

  run.publishMessage(withParts("web-turn-1", [{type: "reasoning", text: "think"}]));
  run.publishMessage(withParts("web-turn-1", [{type: "reasoning", text: "think twice"}]));
  const partial: WebUiPart = {
    type: "tool_call",
    callId: "call_read",
    name: "read",
    arguments: '{"pa',
    partial: true,
  };
  run.publishMessage(withParts("web-turn-1", [{type: "reasoning", text: "think twice"}, partial]));
  const grown: WebUiPart = {...partial, arguments: '{"path":"a.txt"}'};
  run.publishMessage(withParts("web-turn-1", [{type: "reasoning", text: "think twice"}, grown]));
  const result: WebUiPart = {
    type: "tool_result",
    callId: "call_read",
    output: "ok",
    status: "success",
  };
  run.publishMessage(
    withParts("web-turn-1", [{type: "reasoning", text: "think twice"}, grown, result]),
  );
  const complete = withParts(
    "web-turn-1",
    [
      {type: "reasoning", text: "think twice"},
      {type: "tool_call", callId: "call_read", name: "read", arguments: '{"path":"a.txt"}'},
      result,
      {type: "text", text: "done"},
    ],
    "complete",
  );
  run.publishMessage(complete);

  assert.deepEqual(run.latest, complete);
  assert.deepEqual(appliedMessage(events), run.latest);
  registry.clear();
});

test("a long stream costs the appended text, not the whole message", () => {
  const registry = new SessionRunRegistry(60_000);
  const run = registry.begin("session1", undefined, "web-turn-1", new AbortController());
  const heavy: WebUiPart = {
    type: "tool_result",
    callId: "call_read",
    output: "x".repeat(120_000),
    status: "success",
  };

  let bytes = 0;
  run.subscribe((event) => {
    bytes += Buffer.byteLength(JSON.stringify({event: event.event, data: event.data}));
  });
  run.publishMessage(withParts("web-turn-1", [heavy]));
  // The state a client already holds; only what the stream adds from here counts.
  bytes = 0;

  const deltas = 2_000;
  let text = "";
  for (let index = 0; index < deltas; index += 1) {
    text += "0123456789";
    run.publishMessage(withParts("web-turn-1", [heavy, {type: "text", text}]));
  }

  assert.equal(run.latest?.text.length, deltas * 10);
  // A snapshot per delta re-sent the 120 KB part every time — ~240 MB here.
  assert.ok(bytes < 256 * 1024, `streamed ${bytes} bytes for ${deltas} deltas`);
  registry.clear();
});

test("spills content after its UTF-8 budget and restores split surrogate appends", async () => {
  const root = await mkdtemp(join(tmpdir(), "web-ui-run-content-cache-"));
  const cacheDirectory = join(root, "profile-a", "staging", "web-ui-runs");
  const cache = new RunContentCache({directory: cacheDirectory, memoryBytes: 4});
  try {
    const partial = message("turn-cache", "prefix \\ud83d", "streaming");
    cache.update(partial);
    assert.equal(cache.isSpilled, true);
    assert.equal(cache.metadata()?.text, "");
    const cachedPart = cache.metadata()?.parts[0];
    assert.equal(cachedPart?.type === "text" ? cachedPart.text : undefined, "");

    const complete = message("turn-cache", "prefix 😀", "streaming");
    cache.update(complete, {index: 0, append: "\\ude00"});
    assert.deepEqual(await cache.read(), complete);
    assert.equal((await readdir(cacheDirectory)).length, 1);

    cache.dispose();
    assert.deepEqual(await readdir(cacheDirectory), []);
  } finally {
    cache.dispose();
    await rm(root, {recursive: true, force: true});
  }
});

test("spilled profile run snapshots replay independently and survive terminal retention", async () => {
  const root = await mkdtemp(join(tmpdir(), "web-ui-run-profile-cache-"));
  const masterDirectory = join(root, "master", "staging", "web-ui-runs");
  const alternateDirectory = join(root, "alternate", "staging", "web-ui-runs");
  const registry = new SessionRunRegistry(60_000, 8);
  try {
    const master = registry.begin(
      "session1", "master", "turn-master", new AbortController(), masterDirectory,
    );
    const alternate = registry.begin(
      "session1", "alternate", "turn-alternate", new AbortController(), alternateDirectory,
    );
    const masterText = "master stream with enough content to spill";
    const alternateText = "alternate stream remains private";
    master.publishMessage(message("turn-master", masterText, "streaming"));
    alternate.publishMessage(message("turn-alternate", alternateText, "streaming"));
    assert.equal(master.latest?.text, "");
    assert.equal(alternate.latest?.text, "");

    const masterEvents: Array<{event: string; data: unknown}> = [];
    const masterSubscription = master.subscribe((event) => masterEvents.push(event));
    const masterNext = `${masterText} plus more`;
    master.publishMessage(
      message("turn-master", masterNext, "streaming"),
      {index: 0, append: " plus more"},
    );

    const alternateEvents: Array<{event: string; data: unknown}> = [];
    const alternateSubscription = alternate.subscribe((event) => alternateEvents.push(event));
    await Promise.all([masterSubscription.ready, alternateSubscription.ready]);
    assert.deepEqual(appliedMessage(masterEvents), message("turn-master", masterNext, "streaming"));
    assert.deepEqual(
      (alternateEvents[0]?.data as {message: WebUiMessage}).message,
      message("turn-alternate", alternateText, "streaming"),
    );

    const final = message("turn-master", masterNext, "complete");
    master.finish("completed", [
      {event: "ui.message", data: {message: final}},
      {event: "completed", data: {message: final, committed: true}},
      {event: "done", data: {}},
    ], final);
    const terminalEvents: Array<{event: string; data: unknown}> = [];
    const terminalSubscription = master.subscribe((event) => terminalEvents.push(event));
    await terminalSubscription.ready;
    assert.deepEqual(terminalEvents.map((event) => event.event), ["ui.message", "completed", "done"]);
    assert.deepEqual(
      (terminalEvents[0]?.data as {message: WebUiMessage}).message,
      final,
    );
    assert.deepEqual(
      (terminalEvents[1]?.data as {message: WebUiMessage; committed: boolean}).message,
      final,
    );

    registry.clear();
    assert.deepEqual(await readdir(masterDirectory), []);
    assert.deepEqual(await readdir(alternateDirectory), []);
  } finally {
    registry.clear();
    await rm(root, {recursive: true, force: true});
  }
});

test("spilled non-append updates publish only the changed full part", async () => {
  const root = await mkdtemp(join(tmpdir(), "web-ui-run-part-update-"));
  const cacheDirectory = join(root, "profile", "staging", "web-ui-runs");
  const registry = new SessionRunRegistry(60_000, 4);
  try {
    const run = registry.begin(
      "session1", "profile", "turn-parts", new AbortController(), cacheDirectory,
    );
    const existing: WebUiPart = {type: "text", text: "a long enough streamed answer"};
    run.publishMessage(withParts("turn-parts", [existing]));

    const events: Array<{event: string; data: unknown}> = [];
    const subscription = run.subscribe((event) => events.push(event));
    await subscription.ready;
    events.length = 0;

    const result: WebUiPart = {
      type: "tool_result",
      callId: "call-1",
      output: "tool output",
      status: "success",
    };
    run.publishMessage(withParts("turn-parts", [existing, result]));
    assert.deepEqual(events, [
      {event: "ui.part", data: {id: "turn-parts", index: 1, part: result}},
    ]);
  } finally {
    registry.clear();
    await rm(root, {recursive: true, force: true});
  }
});

test("keeps one run slot per session profile so two profiles run at once", () => {
  const registry = new SessionRunRegistry(60_000);
  const primary = registry.begin("session1", "primary", "web-primary-1", new AbortController());
  const alternate = registry.begin("session1", "alternate", "web-alternate-1", new AbortController());

  assert.equal(registry.current("session1", "primary")?.turnId, "web-primary-1");
  assert.equal(registry.current("session1", "alternate")?.turnId, "web-alternate-1");
  // A session that configures no profiles keeps its own single slot.
  assert.equal(registry.current("session1", undefined), undefined);
  assert.equal(registry.current("session1", "missing"), undefined);

  assert.deepEqual(registry.describeAll("session1"), {
    primary: primary.summary(),
    alternate: alternate.summary(),
  });
  assert.equal(registry.describeAll("session2"), undefined);
  assert.deepEqual(
    registry.runsOf("session1").map((run) => run.turn_id).sort(),
    ["web-alternate-1", "web-primary-1"],
  );

  // Stopping one profile's run leaves the other streaming.
  assert.deepEqual(registry.stop("session1", "primary"), {
    stopped: true,
    turn_id: "web-primary-1",
  });
  assert.equal(primary.controller.signal.aborted, true);
  assert.equal(alternate.controller.signal.aborted, false);

  alternate.finish("completed", [{event: "done", data: {}}]);
  assert.equal(registry.describe("session1", "alternate")?.status, "completed");
  assert.deepEqual(Object.keys(registry.describeAll("session1") ?? {}), ["primary", "alternate"]);
  registry.clear();
});

test("a profile-less session and a profiled one never share a slot", () => {
  const registry = new SessionRunRegistry(60_000);
  const plain = registry.begin("session1", undefined, "web-plain", new AbortController());
  const profiled = registry.begin("session1", "primary", "web-primary", new AbortController());

  assert.equal(registry.current("session1", undefined)?.turnId, "web-plain");
  assert.equal(registry.current("session1", "primary")?.turnId, "web-primary");
  assert.deepEqual(registry.stop("session1", undefined), {stopped: true, turn_id: "web-plain"});
  assert.equal(profiled.controller.signal.aborted, false);
  assert.equal(plain.controller.signal.aborted, true);
  // `describeAll` answers for profiles only: the plain slot is not a profile.
  assert.deepEqual(registry.describeAll("session1"), {primary: profiled.summary()});
  registry.clear();
});
