import assert from "node:assert/strict";
import test from "node:test";

import { MessageStreamEmitter, isStreamingToolCallMessage } from "../src/stream-emitter.js";
import type { WebUiMessage, WebUiPart } from "../src/plugins/web-ui/types.js";

function message(parts: WebUiPart[]): WebUiMessage {
  return {
    id: "turn-1",
    sender: "assistant",
    text: "",
    timestamp: "12:00",
    parts,
    status: "streaming",
  };
}

function toolCall(argumentsText: string, partial: boolean): WebUiMessage {
  return message([
    {
      type: "tool_call",
      callId: "call_write",
      name: "write",
      arguments: argumentsText,
      ...(partial ? {partial: true} : {}),
    },
  ]);
}

test("recognizes an in-progress tool call", () => {
  assert.equal(isStreamingToolCallMessage(toolCall("{", true)), true);
  assert.equal(isStreamingToolCallMessage(toolCall("{}", false)), false);
  assert.equal(
    isStreamingToolCallMessage(message([{type: "text", text: "hello"}])),
    false,
  );
});

test("coalesces a burst of in-progress snapshots into one emit", () => {
  const sent: WebUiMessage[] = [];
  const emitter = new MessageStreamEmitter(60_000, (value) => sent.push(value));

  emitter.push(toolCall("{", true));
  for (const chunk of ['"path"', ':"notes.md"', "}"]) {
    emitter.push(toolCall(chunk, true));
  }

  // The first snapshot goes out immediately; the rest wait for the interval.
  assert.equal(sent.length, 1);
  assert.equal(sent[0]?.parts[0]?.type, "tool_call");

  emitter.flush();
  assert.equal(sent.length, 2);
  assert.equal(
    (sent[1]?.parts[0] as {arguments?: string}).arguments,
    "}",
  );
  emitter.discard();
});

test("a completed snapshot is not delayed by a pending one, and supersedes it", () => {
  const sent: WebUiMessage[] = [];
  const emitter = new MessageStreamEmitter(60_000, (value) => sent.push(value));

  emitter.push(toolCall('{"path":', true));
  emitter.push(toolCall('{"path":"notes.md"}', false));

  // The finished call flushes first: the coalesced snapshot must never arrive
  // after the state that replaced it.
  assert.equal(sent.length, 2);
  assert.equal((sent[0]?.parts[0] as {arguments?: string}).arguments, '{"path":');
  assert.equal((sent[1]?.parts[0] as {arguments?: string}).arguments, '{"path":"notes.md"}');
  emitter.discard();
});

test("discard drops a coalesced snapshot that a newer state replaced", () => {
  const sent: WebUiMessage[] = [];
  const emitter = new MessageStreamEmitter(60_000, (value) => sent.push(value));

  emitter.push(toolCall("{", true));
  emitter.push(toolCall('{"path":', true));
  emitter.discard();
  emitter.flush();

  assert.equal(sent.length, 1, "only the immediate snapshot was sent");
  emitter.discard();
});

test("an interval of zero streams every snapshot", () => {
  const sent: WebUiMessage[] = [];
  const emitter = new MessageStreamEmitter(0, (value) => sent.push(value));

  emitter.push(toolCall("{", true));
  emitter.push(toolCall('{"path"', true));

  assert.equal(sent.length, 2);
  emitter.discard();
});

test("a scheduled flush delivers the newest snapshot without an explicit flush", async () => {
  const sent: WebUiMessage[] = [];
  const emitter = new MessageStreamEmitter(20, (value) => sent.push(value));

  emitter.push(toolCall("{", true));
  emitter.push(toolCall('{"path":"notes.md"', true));
  await new Promise((resolve) => setTimeout(resolve, 80));

  assert.equal(sent.length, 2);
  assert.equal(
    (sent[1]?.parts[0] as {arguments?: string}).arguments,
    '{"path":"notes.md"',
  );
  emitter.discard();
});
