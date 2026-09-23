import assert from "node:assert/strict";
import test from "node:test";

import { SessionRunRegistry } from "../src/run-registry.js";
import type { WebUiMessage } from "../src/plugins/web-ui/types.js";

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
  const run = registry.current(sessionRef);
  return run === undefined ? [] : [run.summary().turn_id];
}

test("describes a run while it is in flight and after it finishes", () => {
  const registry = new SessionRunRegistry(60_000);
  const controller = new AbortController();
  assert.equal(registry.describe("session1"), undefined);

  const run = registry.begin("session1", "web-turn-1", controller);
  assert.deepEqual(registry.describe("session1"), {
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

  assert.equal(registry.describe("session1")?.status, "completed");
  assert.equal(registry.current("session1")?.latest?.text, "done");
  registry.clear();
});

test("a subscriber that attaches mid-run replays the newest snapshot, then streams", () => {
  const registry = new SessionRunRegistry(60_000);
  const run = registry.begin("session1", "web-turn-1", new AbortController());
  run.publishMessage(message("web-turn-1", "first", "streaming"));

  const seen: string[] = [];
  const detach = run.subscribe(event => seen.push(event.event));
  assert.deepEqual(seen, ["ui.message"]);

  const events: unknown[] = [];
  run.subscribe(event => events.push(event.data));
  run.publishMessage(message("web-turn-1", "second", "streaming"));
  // The replay of the current snapshot is part of what a late subscriber sees.
  assert.deepEqual(events, [
    {message: message("web-turn-1", "first", "streaming")},
    {message: message("web-turn-1", "second", "streaming")},
  ]);

  detach();
  run.publishMessage(message("web-turn-1", "third", "streaming"));
  assert.deepEqual(events, [
    {message: message("web-turn-1", "first", "streaming")},
    {message: message("web-turn-1", "second", "streaming")},
    {message: message("web-turn-1", "third", "streaming")},
  ]);
  registry.clear();
});

test("a subscriber that attaches after the run ended replays the terminal events", () => {
  const registry = new SessionRunRegistry(60_000);
  const run = registry.begin("session1", "web-turn-1", new AbortController());
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
  const run = registry.begin("session1", "web-turn-1", controller);

  const result = registry.stop("session1");
  assert.deepEqual(result, {stopped: true, turn_id: "web-turn-1"});
  assert.equal(controller.signal.aborted, true);

  run.finish("aborted", [{event: "error", data: {message: "stopped"}}, {event: "done", data: {}}]);
  assert.deepEqual(registry.stop("session1"), {stopped: false});
  assert.deepEqual(registry.stop("other-session"), {stopped: false});
  registry.clear();
});

test("abortAll aborts every live run", () => {
  const registry = new SessionRunRegistry(60_000);
  const first = new AbortController();
  const second = new AbortController();
  registry.begin("session1", "web-turn-1", first);
  registry.begin("session2", "web-turn-2", second);

  registry.abortAll(new Error("server_close"));
  assert.equal(first.signal.aborted, true);
  assert.equal(second.signal.aborted, true);
  registry.clear();
});

test("a finished run is dropped after the retention window", async () => {
  const registry = new SessionRunRegistry(20);
  const run = registry.begin("session1", "web-turn-1", new AbortController());
  run.finish("completed", [{event: "done", data: {}}]);
  assert.deepEqual(summaryRefs(registry, "session1"), ["web-turn-1"]);

  await new Promise(resolve => setTimeout(resolve, 60));
  assert.equal(registry.current("session1"), undefined);
  assert.equal(registry.describe("session1"), undefined);
  registry.clear();
});

test("retention zero drops a finished run immediately", () => {
  const registry = new SessionRunRegistry(0);
  const run = registry.begin("session1", "web-turn-1", new AbortController());
  run.finish("failed", [{event: "done", data: {}}]);
  assert.equal(registry.current("session1"), undefined);
  registry.clear();
});

test("keeps a queued second run separate from the running one", () => {
  const registry = new SessionRunRegistry(60_000);
  const first = registry.begin("session1", "web-turn-1", new AbortController());
  registry.begin("session1", "web-turn-2", new AbortController());

  // Attaching and stopping follow the run that is actually executing.
  assert.equal(registry.current("session1")?.turnId, "web-turn-1");
  assert.deepEqual(registry.stop("session1"), {stopped: true, turn_id: "web-turn-1"});

  first.finish("aborted", [{event: "done", data: {}}]);
  assert.equal(registry.current("session1")?.turnId, "web-turn-2");
  registry.clear();
});
