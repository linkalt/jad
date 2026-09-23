import assert from "node:assert/strict";
import test from "node:test";

import { capWebUiMessages } from "../src/plugins/web-ui/part-cap-web-ui.js";
import type {
  WebUiMessage,
  WebUiPart,
  WebUiTextPart,
} from "../src/plugins/web-ui/types.js";

function message(parts: WebUiPart[], id = "o100"): WebUiMessage {
  const text = parts
    .filter((part): part is WebUiTextPart => part.type === "text")
    .map((part) => part.text)
    .join("");
  const reasoningContent = parts
    .filter((part): part is WebUiTextPart => part.type === "reasoning")
    .map((part) => part.text)
    .join("\n\n")
    .trim();
  return {
    id,
    sender: "assistant",
    text,
    timestamp: "12:00",
    parts,
    status: "complete",
    ...(reasoningContent.length === 0 ? {} : {reasoningContent}),
  };
}

function cursors(...ids: string[]): Map<string, number> {
  return new Map(ids.map((id, index) => [id, 4_000 + index]));
}

function bytesOf(value: string): number {
  return Buffer.byteLength(value, "utf8");
}

test("a compacted message is capped without a turn cursor", () => {
  const parts: WebUiPart[] = [
    {type: "tool_result", callId: "call-1", output: "x".repeat(5_000), status: "success"},
  ];
  const original = message(parts, "h3");
  const capped = capWebUiMessages([original], {
    maxInlineBytes: 64,
    // The compacted head has no turn to re-read; it is served from the cache.
    cursors: cursors("o100"),
  });
  const [result] = capped;
  assert.notEqual(result, original);
  assert.equal(result?.cursor, undefined);
  assert.deepEqual(result?.parts[0]?.truncated, {bytes: 5_000});
});

test("a message that fits is returned untouched", () => {
  const parts: WebUiPart[] = [
    {type: "reasoning", text: "short"},
    {type: "tool_call", callId: "call-1", name: "read", arguments: "{}"},
    {type: "tool_result", callId: "call-1", output: "{\"status\":\"ok\"}", status: "success"},
  ];
  const original = message(parts);
  const capped = capWebUiMessages([original], {maxInlineBytes: 64, cursors: cursors("o100")});
  assert.equal(capped[0], original);
});

test("caps each part kind and reports its full size", () => {
  const output = `{"status":"ok"}\n${"y".repeat(1_000)}`;
  const args = JSON.stringify({path: "big.txt", padding: "z".repeat(500)});
  const reasoning = "r".repeat(400);
  const capped = capWebUiMessages(
    [message([
      {type: "reasoning", text: reasoning},
      {type: "tool_call", callId: "call-1", name: "write", arguments: args},
      {type: "tool_result", callId: "call-1", output, status: "success"},
    ])],
    {maxInlineBytes: 64, cursors: cursors("o100")},
  );

  const [result] = capped;
  assert.ok(result);
  assert.equal(result.cursor, 4_000);
  const [reasoningPart, callPart, resultPart] = result.parts;
  assert.ok(reasoningPart && reasoningPart.type === "reasoning");
  assert.equal(bytesOf(reasoningPart.text), 64);
  assert.deepEqual(reasoningPart.truncated, {bytes: bytesOf(reasoning)});
  assert.ok(callPart && callPart.type === "tool_call");
  assert.equal(bytesOf(callPart.arguments), 64);
  assert.deepEqual(callPart.truncated, {bytes: bytesOf(args)});
  assert.equal(callPart.name, "write");
  assert.ok(resultPart && resultPart.type === "tool_result");
  assert.equal(bytesOf(resultPart.output), 64);
  assert.equal(resultPart.status, "success");
  assert.deepEqual(resultPart.truncated, {bytes: bytesOf(output)});
  // The derived fields are recomputed from the kept text: shipping the whole
  // value beside a cut part would defeat the cap.
  assert.equal(result.reasoningContent, reasoningPart.text);
  assert.equal(result.text, "");
});

test("a text part is capped with the message text", () => {
  const text = "a".repeat(300);
  const capped = capWebUiMessages(
    [message([{type: "text", text}])],
    {maxInlineBytes: 100, cursors: cursors("o100")},
  );
  const [result] = capped;
  assert.ok(result);
  assert.equal(result.text, "a".repeat(100));
  assert.equal(result.parts[0]?.type === "text" ? result.parts[0].text : "", result.text);
  assert.deepEqual(result.parts[0]?.truncated, {bytes: 300});
});

test("cuts on a code point boundary", () => {
  const text = "あ".repeat(50);
  const capped = capWebUiMessages(
    [message([{type: "text", text}])],
    {maxInlineBytes: 4, cursors: cursors("o100")},
  );
  const part = capped[0]?.parts[0];
  assert.ok(part && part.type === "text");
  // 4 bytes would land inside the second character, so one character is kept.
  assert.equal(part.text, "あ");
  assert.equal(bytesOf(part.text), 3);
  assert.deepEqual(part.truncated, {bytes: 150});
});

test("a limit of zero sends parts uncut", () => {
  const parts: WebUiPart[] = [
    {type: "tool_result", callId: "call-1", output: "x".repeat(5_000), status: "success"},
  ];
  const original = message(parts);
  const capped = capWebUiMessages([original], {maxInlineBytes: 0, cursors: cursors("o100")});
  assert.deepEqual(capped, [original]);
  assert.deepEqual(capped[0], original);
});

test("caps only the parts that are too large", () => {
  const big = (id: string): WebUiMessage =>
    message([{type: "text", text: "q".repeat(500)}], id);
  const small = message([{type: "text", text: "tiny"}], "h0");
  const capped = capWebUiMessages([big("o10"), small, big("o20")], {
    maxInlineBytes: 32,
    cursors: cursors("o10", "o20"),
  });
  assert.equal(capped[0]?.cursor, 4_000);
  assert.equal(capped[1], small);
  assert.equal(capped[2]?.cursor, 4_001);
});
