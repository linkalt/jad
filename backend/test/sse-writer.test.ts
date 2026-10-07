import assert from "node:assert/strict";
import test from "node:test";

import {startSseHeartbeat, writeSse, type SseWritableResponse} from "../src/sse-writer.js";

class FakeSseResponse implements SseWritableResponse {
  public writableEnded = false;
  public destroyed = false;
  public writableLength = 0;
  public readonly writes: string[] = [];

  public write(chunk: string): boolean {
    this.writes.push(chunk);
    this.writableLength += Buffer.byteLength(chunk);
    return true;
  }

  public destroy(): void {
    this.destroyed = true;
  }
}

test("bounds queued SSE frames and closes an over-budget subscriber", () => {
  const response = new FakeSseResponse();

  assert.equal(writeSse(response, "run", {turn_id: "turn-1"}, 128), true);
  assert.equal(writeSse(response, "ui.delta", {append: "x".repeat(128)}, 128), false);
  assert.equal(response.destroyed, true);
  assert.equal(response.writes.length, 1);
});

test("permits an oversized snapshot, but not a second one while the first is still queued", () => {
  const response = new FakeSseResponse();
  const message = {id: "turn-1", status: "streaming", parts: [{type: "text", text: "x".repeat(256)}]};

  assert.equal(writeSse(response, "ui.message", {message}, 128), true);
  assert.equal(response.writableLength > 128, true);
  // A stalled client must not pile up one big snapshot after another.
  assert.equal(writeSse(response, "ui.message", {message}, 128), false);
  assert.equal(response.destroyed, true);
  assert.equal(response.writes.length, 1);
});

test("an oversized snapshot is not charged to the frames behind it, which stay bounded", () => {
  const response = new FakeSseResponse();
  const message = {id: "turn-1", status: "streaming", parts: [{type: "text", text: "x".repeat(256)}]};
  const delta = {id: message.id, index: 0, append: "y".repeat(60)};

  assert.equal(writeSse(response, "ui.message", {message}, 128), true);
  // The snapshot is the one frame the protocol cannot make smaller, so a client
  // that reads it keeps its stream.
  assert.equal(writeSse(response, "ui.delta", delta, 128), true);
  assert.equal(response.destroyed, false);
  // What is queued behind it is still held to the budget.
  assert.equal(writeSse(response, "ui.delta", delta, 128), false);
  assert.equal(response.destroyed, true);
  assert.equal(response.writes.length, 2);
});

test("the terminal events of a run are written however much is queued", () => {
  for (const event of ["completed", "error", "done"]) {
    const response = new FakeSseResponse();
    response.writableLength = 10_000;
    assert.equal(writeSse(response, event, {message: "x".repeat(512)}, 128), true, event);
    assert.equal(response.destroyed, false, event);
    assert.equal(response.writes.length, 1, event);
  }
  // Anything else still closes an over-budget subscriber.
  const response = new FakeSseResponse();
  response.writableLength = 10_000;
  assert.equal(writeSse(response, "usage", {}, 128), false);
  assert.equal(response.destroyed, true);
});

test("a run whose final message is larger than the budget still delivers completed and done", () => {
  const response = new FakeSseResponse();
  const message = {id: "turn-1", status: "complete", parts: [{type: "text", text: "x".repeat(256)}]};

  // The closing burst is written in one tick, so nothing drains between frames.
  assert.equal(writeSse(response, "ui.message", {message}, 128), true);
  assert.equal(writeSse(response, "completed", {message, committed: true}, 128), true);
  assert.equal(writeSse(response, "done", {}, 128), true);
  assert.equal(response.destroyed, false);
  assert.deepEqual(
    response.writes.map((frame) => /^event: (.*)$/m.exec(frame)?.[1]),
    ["ui.message", "completed", "done"],
  );
});

test("does not write to an ended or destroyed response", () => {
  const response = new FakeSseResponse();
  response.writableEnded = true;

  assert.equal(writeSse(response, "done", {}, 1024), false);
  assert.equal(response.writes.length, 0);
});

test("a heartbeat writes comment frames while the stream is quiet and stops on request", async () => {
  const response = new FakeSseResponse();
  const stop = startSseHeartbeat(response, 5);
  await new Promise((resolve) => setTimeout(resolve, 40));
  stop();
  const count = response.writes.length;
  assert.ok(count >= 1, "a quiet stream receives keep-alive frames");
  assert.ok(response.writes.every((frame) => frame === ": keep-alive\n\n"));
  // A comment frame has no `event:` line, so a client parser ignores it.
  assert.ok(response.writes.every((frame) => !frame.includes("event:")));
  await new Promise((resolve) => setTimeout(resolve, 20));
  assert.equal(response.writes.length, count, "no frames after stop");
});

test("a heartbeat adds nothing while earlier output is still queued", async () => {
  const response = new FakeSseResponse();
  response.writableLength = 10;
  const stop = startSseHeartbeat(response, 5);
  await new Promise((resolve) => setTimeout(resolve, 30));
  stop();
  assert.deepEqual(response.writes, []);
});

test("a zero interval disables the heartbeat", async () => {
  const response = new FakeSseResponse();
  const stop = startSseHeartbeat(response, 0);
  await new Promise((resolve) => setTimeout(resolve, 20));
  stop();
  assert.deepEqual(response.writes, []);
});
