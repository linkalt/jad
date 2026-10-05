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

test("permits one oversized frame, then requires snapshot resynchronization", () => {
  const response = new FakeSseResponse();
  const message = {id: "turn-1", status: "streaming", parts: [{type: "text", text: "x".repeat(256)}]};

  assert.equal(writeSse(response, "ui.message", {message}, 128), true);
  assert.equal(response.writableLength > 128, true);
  assert.equal(writeSse(response, "ui.delta", {id: message.id, index: 0, append: "more"}, 128), false);
  assert.equal(response.destroyed, true);
  assert.equal(response.writes.length, 1);
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
