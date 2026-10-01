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

test("coalesces a burst of growing tool arguments into one emit", () => {
  const sent: WebUiMessage[] = [];
  const emitter = new MessageStreamEmitter(60_000, (value) => { sent.push(value); });

  let argumentsText = "{";
  emitter.push(toolCall(argumentsText, true));
  for (const chunk of ['"path"', ':"notes.md"', "}"]) {
    argumentsText += chunk;
    emitter.push(toolCall(argumentsText, true), {index: 0, append: chunk});
  }

  // The first snapshot goes out immediately; the growth waits for the interval.
  assert.equal(sent.length, 1);
  assert.equal(sent[0]?.parts[0]?.type, "tool_call");

  emitter.flush();
  assert.equal(sent.length, 2);
  assert.equal(
    (sent[1]?.parts[0] as {arguments?: string}).arguments,
    '{"path":"notes.md"}',
  );
  emitter.discard();
});

test("keeps response output-item deltas separate across the coalescing window", () => {
  const sent: Array<{outputIndex?: number; append?: {index: number; append: string}}> = [];
  const emitter = new MessageStreamEmitter(60_000, (_message, append, outputIndex) => {
    sent.push({
      ...(outputIndex === undefined ? {} : {outputIndex}),
      ...(append === undefined ? {} : {append}),
    });
  });

  emitter.push(message([{type: "text", text: ""}]));
  emitter.push(
    message([{type: "text", text: "first"}]),
    {index: 0, append: "first", outputIndex: 0},
  );
  emitter.push(
    message([{type: "text", text: "first second"}]),
    {index: 0, append: " second", outputIndex: 1},
  );

  assert.deepEqual(sent.map((event) => event.outputIndex), [undefined, 0, 1]);
  assert.deepEqual(sent[1]?.append, {index: 0, append: "first", outputIndex: 0});
  assert.deepEqual(sent[2]?.append, {index: 0, append: " second", outputIndex: 1});
  emitter.discard();
});

test("coalesces streamed text and publishes a new part at once", () => {
  const sent: WebUiMessage[] = [];
  const emitter = new MessageStreamEmitter(60_000, (value) => { sent.push(value); });

  emitter.push(message([{type: "text", text: "hello"}]));
  emitter.push(message([{type: "text", text: "hello world"}]), {index: 0, append: " world"});
  assert.equal(sent.length, 1, "growth waits for the interval");

  emitter.push(
    message([
      {type: "text", text: "hello world"},
      {type: "tool_result", callId: "call_read", output: "ok", status: "success"},
    ]),
  );
  // The new part flushes the held state first, then goes out immediately: a tool
  // result is not growth and must never wait behind one.
  assert.equal(sent.length, 3);
  assert.equal((sent[1]?.parts[0] as {text?: string}).text, "hello world");
  assert.equal(sent[2]?.parts[1]?.type, "tool_result");
  emitter.discard();
});

test("flushes a pending append before another part grows", () => {
  const sent: WebUiMessage[] = [];
  const emitter = new MessageStreamEmitter(60_000, (value) => { sent.push(value); });

  emitter.push(message([
    {type: "text", text: "first"},
    {type: "text", text: "second"},
  ]));
  emitter.push(message([
    {type: "text", text: "first!"},
    {type: "text", text: "second"},
  ]), {index: 0, append: "!"});
  emitter.push(message([
    {type: "text", text: "first!"},
    {type: "text", text: "second?"},
  ]), {index: 1, append: "?"});

  assert.equal(sent.length, 3);
  assert.equal((sent[1]?.parts[0] as {text?: string}).text, "first!");
  assert.equal((sent[2]?.parts[1] as {text?: string}).text, "second?");
  emitter.discard();
});

test("a completed snapshot is not delayed by a pending one, and supersedes it", () => {
  const sent: WebUiMessage[] = [];
  const emitter = new MessageStreamEmitter(60_000, (value) => { sent.push(value); });

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
  const emitter = new MessageStreamEmitter(60_000, (value) => { sent.push(value); });

  emitter.push(toolCall("{", true));
  emitter.push(toolCall('{"path":', true), {index: 0, append: '"path":'});
  emitter.discard();
  emitter.flush();

  assert.equal(sent.length, 1, "only the immediate snapshot was sent");
  emitter.discard();
});

test("an interval of zero streams every snapshot", () => {
  const sent: WebUiMessage[] = [];
  const emitter = new MessageStreamEmitter(0, (value) => { sent.push(value); });

  emitter.push(toolCall("{", true));
  emitter.push(toolCall('{"path"', true), {index: 0, append: '"path"'});

  assert.equal(sent.length, 2);
  emitter.discard();
});

test("a scheduled flush delivers the newest snapshot without an explicit flush", async () => {
  const sent: WebUiMessage[] = [];
  const emitter = new MessageStreamEmitter(20, (value) => { sent.push(value); });

  emitter.push(toolCall("{", true));
  emitter.push(toolCall('{"path":"notes.md"', true), {index: 0, append: '"path":"notes.md"'});
  await new Promise((resolve) => setTimeout(resolve, 80));

  assert.equal(sent.length, 2);
  assert.equal(
    (sent[1]?.parts[0] as {arguments?: string}).arguments,
    '{"path":"notes.md"',
  );
  emitter.discard();
});
