import assert from "node:assert/strict";
import { mkdir, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";

import type OpenAI from "openai";

import { createAgentServer } from "../src/server.js";
import {
  normalizeWebUiSession,
  WebUiNormalizedRun,
} from "../src/plugins/web-ui-normalized.js";
import type { LoadedSessionConfig } from "../src/config.js";

function completedEvent(output: Record<string, unknown>[], id: string): Record<string, unknown> {
  return {
    type: "response.completed",
    response: {
      id,
      object: "response",
      status: "completed",
      model: "fake-model",
      output,
    },
  };
}

function ssePayloads(stream: string, eventName: string): Record<string, unknown>[] {
  return stream.split("\n\n").flatMap((block) => {
    const event = block
      .split("\n")
      .find((line) => line.startsWith("event:"))
      ?.slice("event:".length)
      .trim();
    if (event !== eventName) {
      return [];
    }
    const data = block
      .split("\n")
      .find((line) => line.startsWith("data:"))
      ?.slice("data:".length)
      .trimStart();
    return data ? [JSON.parse(data) as Record<string, unknown>] : [];
  });
}

test("preserves interleaved reasoning and assistant parts", () => {
  const normalizedRun = new WebUiNormalizedRun("interleaved-turn");
  const events = [
    { type: "response.reasoning_summary_text.delta", delta: "r1" },
    { type: "response.output_text.delta", delta: "a1" },
    { type: "response.reasoning_summary_text.delta", delta: "r2" },
    { type: "response.output_text.delta", delta: "a2" },
    { type: "response.reasoning_summary_text.delta", delta: "r3" },
    { type: "response.reasoning_summary_text.delta", delta: "r4" },
  ];
  for (const event of events) {
    normalizedRun.update(event);
  }

  const expectedParts = [
    { type: "reasoning", text: "r1" },
    { type: "text", text: "a1" },
    { type: "reasoning", text: "r2" },
    { type: "text", text: "a2" },
    { type: "reasoning", text: "r3r4" },
  ];
  assert.deepEqual(normalizedRun.complete({
    id: "response-interleaved",
    output: [],
  }).parts, expectedParts);

  const snapshot = normalizeWebUiSession(
    "session1",
    {
      model: { provider: "fake", model: "fake-model" },
      settings: { session_name: "interleaved" },
    } as unknown as LoadedSessionConfig,
    [
      completedEvent(
        [
          { type: "reasoning", content: [{ type: "reasoning_text", text: "r1" }] },
          { type: "message", role: "assistant", content: [{ type: "output_text", text: "a1" }] },
          { type: "reasoning", content: [{ type: "reasoning_text", text: "r2" }] },
          { type: "message", role: "assistant", content: [{ type: "output_text", text: "a2" }] },
          { type: "reasoning", content: [{ type: "reasoning_text", text: "r3" }] },
          { type: "reasoning", content: [{ type: "reasoning_text", text: "r4" }] },
        ],
        "response-interleaved",
      ),
    ],
  );
  assert.deepEqual(snapshot.messages[0]?.parts, expectedParts);
});

test("serves a session snapshot and streams a committed run over SSE", async () => {
  const root = await mkdtemp(join(tmpdir(), "agent-harness-server-"));
  const sessionDir = join(root, "session1");
  await mkdir(sessionDir, { recursive: true });
  await writeFile(
    join(sessionDir, "model.json"),
    JSON.stringify({
      schema_version: 1,
      provider: "fake",
      api_key_env: "FAKE_API_KEY",
      model: "fake-model",
    }),
  );
  await writeFile(
    join(sessionDir, "setting.json"),
    JSON.stringify({
      schema_version: 1,
      session_name: "server-test",
      cwd: sessionDir,
      system_prompt: "Be concise.",
    }),
  );

  const reasoningOne = {
    type: "reasoning",
    content: [{ type: "reasoning_text", text: "Plan the bridge " }],
  };
  const outputOne = {
    type: "message",
    role: "assistant",
    content: [{ type: "output_text", text: "BRIDGE_" }],
  };
  const reasoningTwo = {
    type: "reasoning",
    content: [{ type: "reasoning_text", text: "response." }],
  };
  const outputTwo = {
    type: "message",
    role: "assistant",
    content: [{ type: "output_text", text: "OK" }],
  };
  const fakeClient = {
    responses: {
      create: async () =>
        (async function* () {
          yield {
            type: "response.reasoning_summary_text.delta",
            delta: "Plan the bridge ",
          };
          yield {
            type: "response.output_text.delta",
            delta: "BRIDGE_",
          };
          yield {
            type: "response.reasoning_summary_text.delta",
            delta: "response.",
          };
          yield {
            type: "response.output_text.delta",
            delta: "OK",
          };
          yield {
            type: "response.output_item.done",
            output_index: 0,
            item: reasoningOne,
          };
          yield {
            type: "response.output_item.done",
            output_index: 1,
            item: outputOne,
          };
          yield {
            type: "response.output_item.done",
            output_index: 2,
            item: reasoningTwo,
          };
          yield {
            type: "response.output_item.done",
            output_index: 3,
            item: outputTwo,
          };
          yield completedEvent(
            [reasoningOne, outputOne, reasoningTwo, outputTwo],
            "resp_bridge",
          );
        })(),
    },
  } as unknown as OpenAI;

  const running = createAgentServer({
    sessionsDir: root,
    client: fakeClient,
  });
  await new Promise<void>((resolve, reject) => {
    running.server.once("error", reject);
    running.server.listen(0, "127.0.0.1", () => resolve());
  });

  try {
    const address = running.server.address();
    assert.ok(address && typeof address !== "string");
    const baseUrl = `http://127.0.0.1:${address.port}`;

    const health = await fetch(`${baseUrl}/api/health`);
    assert.equal(health.status, 200);
    assert.deepEqual(await health.json(), { ok: true });

    const snapshot = await fetch(`${baseUrl}/api/sessions/session1`);
    assert.equal(snapshot.status, 200);
    const initial = (await snapshot.json()) as { turns: number; messages: unknown[] };
    assert.equal(initial.turns, 0);
    assert.equal(initial.messages.length, 0);

    const run = await fetch(`${baseUrl}/api/sessions/session1/run`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ input: "say bridge ok" }),
    });
    assert.equal(run.status, 200);
    const stream = await run.text();
    assert.match(stream, /event: ui\.message/);
    assert.match(stream, /"status":"streaming"/);
    assert.match(stream, /"type":"reasoning"/);
    assert.match(stream, /Plan the bridge /);
    assert.match(stream, /"type":"text"/);
    assert.match(stream, /BRIDGE_/);
    assert.match(stream, /response\./);
    assert.match(stream, /OK/);
    assert.ok(stream.indexOf('"type":"reasoning"') < stream.indexOf('"type":"text"'));
    assert.match(stream, /"status":"complete"/);
    assert.match(stream, /event: completed/);
    assert.match(stream, /event: done/);

    const uiMessages = ssePayloads(stream, "ui.message").map((payload) =>
      payload.message as {
        status?: string;
        parts: Array<{type: string; text: string}>;
      },
    );
    assert.deepEqual(uiMessages.map((message) => message.status), [
      "streaming",
      "streaming",
      "streaming",
      "streaming",
      "complete",
    ]);
    assert.deepEqual(uiMessages.map((message) => message.parts), [
      [{type: "reasoning", text: "Plan the bridge "}],
      [
        {type: "reasoning", text: "Plan the bridge "},
        {type: "text", text: "BRIDGE_"},
      ],
      [
        {type: "reasoning", text: "Plan the bridge "},
        {type: "text", text: "BRIDGE_"},
        {type: "reasoning", text: "response."},
      ],
      [
        {type: "reasoning", text: "Plan the bridge "},
        {type: "text", text: "BRIDGE_"},
        {type: "reasoning", text: "response."},
        {type: "text", text: "OK"},
      ],
      [
        {type: "reasoning", text: "Plan the bridge "},
        {type: "text", text: "BRIDGE_"},
        {type: "reasoning", text: "response."},
        {type: "text", text: "OK"},
      ],
    ]);

    const finalSnapshot = await fetch(`${baseUrl}/api/sessions/session1`);
    const final = (await finalSnapshot.json()) as {
      turns: number;
      messages: Array<{
        sender: string;
        text: string;
        parts: Array<{ type: string; text: string }>;
        status?: string;
      }>;
    };
    assert.equal(final.turns, 1);
    assert.equal(final.messages.length, 2);
    assert.deepEqual(final.messages.map(message => message.sender), ["user", "assistant"]);
    const expectedParts = [
      {type: "reasoning", text: "Plan the bridge "},
      {type: "text", text: "BRIDGE_"},
      {type: "reasoning", text: "response."},
      {type: "text", text: "OK"},
    ];
    assert.deepEqual(final.messages[1]?.parts, expectedParts);
    assert.equal(final.messages[1]?.status, "complete");

    const persisted = (await readFile(join(sessionDir, "messages.jsonl"), "utf8"))
      .trimEnd()
      .split("\n")
      .map((line) => JSON.parse(line) as Record<string, unknown>);
    const persistedResponse = persisted[1]?.response as Record<string, unknown>;
    assert.deepEqual(persistedResponse.output, [
      reasoningOne,
      outputOne,
      reasoningTwo,
      outputTwo,
    ]);
    assert.equal(JSON.stringify(persisted).includes('"parts"'), false);
  } finally {
    await running.close();
    await rm(root, { recursive: true, force: true });
  }
});
