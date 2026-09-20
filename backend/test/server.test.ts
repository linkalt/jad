import assert from "node:assert/strict";
import { mkdir, mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";

import type OpenAI from "openai";

import { createAgentServer } from "../src/server.js";

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

  const reasoning = {
    type: "reasoning",
    content: [{ type: "reasoning_text", text: "Plan the bridge response." }],
  };
  const output = {
    type: "message",
    role: "assistant",
    content: [{ type: "output_text", text: "BRIDGE_OK" }],
  };
  const fakeClient = {
    responses: {
      create: async () =>
        (async function* () {
          yield {
            type: "response.reasoning_summary_text.delta",
            delta: "Plan the bridge response.",
          };
          yield {
            type: "response.output_text.delta",
            delta: "BRIDGE_OK",
          };
          yield {
            type: "response.output_item.done",
            output_index: 0,
            item: reasoning,
          };
          yield {
            type: "response.output_item.done",
            output_index: 1,
            item: output,
          };
          yield completedEvent([reasoning, output], "resp_bridge");
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
    assert.match(stream, /Plan the bridge response\./);
    assert.match(stream, /"type":"text"/);
    assert.match(stream, /BRIDGE_OK/);
    assert.ok(stream.indexOf('"type":"reasoning"') < stream.indexOf('"type":"text"'));
    assert.match(stream, /"status":"complete"/);
    assert.match(stream, /event: completed/);
    assert.match(stream, /event: done/);

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
    assert.deepEqual(final.messages[1]?.parts, [
      { type: "reasoning", text: "Plan the bridge response." },
      { type: "text", text: "BRIDGE_OK" },
    ]);
    assert.equal(final.messages[1]?.status, "complete");
  } finally {
    await running.close();
    await rm(root, { recursive: true, force: true });
  }
});
