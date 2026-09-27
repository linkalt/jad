import assert from "node:assert/strict";
import test from "node:test";

import {writeSse, type SseWritableResponse} from "../src/sse-writer.js";

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
