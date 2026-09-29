import assert from "node:assert/strict";
import {access, mkdir, mkdtemp, readFile, rm, writeFile} from "node:fs/promises";
import {tmpdir} from "node:os";
import {join} from "node:path";
import test from "node:test";

import type OpenAI from "openai";

import {createAgentServer, type AgentServerOptions} from "../src/server.js";
import {nativeRequestDraftDefinition} from "../src/tools/native/request-draft.js";
import {nativeWriteDefinition} from "../src/tools/native/write.js";

const TEST_API_TOKEN = "test-api-token-0123456789abcdef0123456789abcdef";
const createServer = (options: AgentServerOptions) => createAgentServer({...options, apiToken: TEST_API_TOKEN});

type ProviderOutput = Record<string, unknown>;
interface SseEvent {event: string; data: Record<string, unknown>}

function completedEvent(output: ProviderOutput[], id: string): ProviderOutput {
  return {
    type: "response.completed",
    response: {
      id,
      object: "response",
      status: "completed",
      model: "fake-model",
      usage: {input_tokens: 10, output_tokens: 4},
      output,
    },
  };
}

function frameEvent(block: string): SseEvent | undefined {
  let event = "message";
  let data = "";
  for (const line of block.split("\n")) {
    if (line.startsWith("event:")) event = line.slice(6).trim();
    else if (line.startsWith("data:")) data += line.slice(5).trim();
  }
  if (data.length === 0) return undefined;
  return {event, data: JSON.parse(data) as Record<string, unknown>};
}

function sseReader(reader: ReadableStreamDefaultReader<Uint8Array>) {
  const decoder = new TextDecoder();
  let buffered = "";
  return {
    async next(): Promise<SseEvent> {
      for (;;) {
        const boundary = buffered.indexOf("\n\n");
        if (boundary >= 0) {
          const block = buffered.slice(0, boundary);
          buffered = buffered.slice(boundary + 2);
          const event = frameEvent(block);
          if (event !== undefined) return event;
          continue;
        }
        const next = await reader.read();
        if (next.done) throw new Error("run stream ended before the expected event");
        buffered += decoder.decode(next.value, {stream: true});
      }
    },
  };
}

async function createFixture(options: {
  nativeSettings: Record<string, unknown> | ((sessionDir: string) => Record<string, unknown>);
  definitions: unknown[];
  responses: ProviderOutput[][];
}) {
  const root = await mkdtemp(join(tmpdir(), "agent-harness-request-draft-scheduling-"));
  const sessionDir = join(root, "session1");
  await mkdir(sessionDir, {recursive: true});
  await writeFile(
    join(sessionDir, "model.json"),
    JSON.stringify({schema_version: 1, provider: "fake", api_key_env: "FAKE_API_KEY", model: "fake-model"}),
  );
  const nativeSettings = typeof options.nativeSettings === "function"
    ? options.nativeSettings(sessionDir)
    : options.nativeSettings;
  await writeFile(
    join(sessionDir, "setting.json"),
    JSON.stringify({
      schema_version: 1,
      session_name: "request-draft-scheduling",
      cwd: sessionDir,
      tool_definition_paths: ["request_draft.json"],
      tools: {native: nativeSettings},
    }),
  );
  await writeFile(
    join(root, "request_draft.json"),
    JSON.stringify({schema_version: 1, tools: options.definitions}),
  );

  let providerCalls = 0;
  const fakeClient = {
    responses: {
      create: async () => {
        const output = options.responses[providerCalls];
        assert.ok(output, `unexpected provider request ${providerCalls + 1}`);
        providerCalls += 1;
        return (async function* () {
          for (const [output_index, item] of output.entries()) {
            yield {type: "response.output_item.done", output_index, item};
          }
          yield completedEvent(output, `response-${providerCalls}`);
        })();
      },
    },
  } as unknown as OpenAI;
  const running = createServer({sessionsDir: root, client: fakeClient});
  await new Promise<void>((resolve, reject) => {
    running.server.once("error", reject);
    running.server.listen(0, "127.0.0.1", () => resolve());
  });
  const address = running.server.address();
  assert.ok(address !== null && typeof address !== "string");
  return {
    root,
    sessionDir,
    running,
    baseUrl: `http://127.0.0.1:${address.port}`,
    providerCalls: () => providerCalls,
    async close() {
      await running.close();
      await new Promise<void>((resolve) => running.server.close(() => resolve()));
      await rm(root, {recursive: true, force: true});
    },
  };
}

function writeCall(callId: string, path: string, content: string): ProviderOutput {
  return {
    id: `fc-${callId}`,
    type: "function_call",
    call_id: callId,
    name: "write",
    arguments: JSON.stringify({path, content}),
  };
}

function draftCall(): ProviderOutput {
  return {
    id: "fc-draft",
    type: "function_call",
    call_id: "call-draft",
    name: "request_draft",
    arguments: JSON.stringify({draft: {goal: "write original.txt"}}),
  };
}

const interactiveSettings = {
  request_draft: {
    timeout_ms: 30_000,
    max_timeout_ms: 60_000,
    extend_ms: 10_000,
    headless_auto_accept_timeout_ms: 600_000,
  },
  write: {paths: {allow: [] as string[]}},
};

async function startInteractiveRun(baseUrl: string, input: string) {
  return fetch(`${baseUrl}/api/sessions/session1/run`, {
    method: "POST",
    headers: {
      authorization: `Bearer ${TEST_API_TOKEN}`,
      "content-type": "application/json",
    },
    body: JSON.stringify({input, supports_interaction: true}),
  });
}

async function readPending(events: ReturnType<typeof sseReader>): Promise<{id: string}> {
  for (;;) {
    const event = await events.next();
    if (event.event !== "ui.message" || !isRecord(event.data.message)) continue;
    const pending = event.data.message.pending;
    if (isRecord(pending) && typeof pending.id === "string") return {id: pending.id};
  }
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

test("declining a draft skips every co-batched write and ends the task run", async () => {
  const fixture = await createFixture({
    nativeSettings: (sessionDir) => ({
      ...interactiveSettings,
      write: {paths: {allow: [sessionDir]}},
    }),
    definitions: [nativeRequestDraftDefinition, nativeWriteDefinition],
    responses: [[
      writeCall("call-write-before", "before.txt", "unapproved"),
      draftCall(),
      writeCall("call-write-after", "after.txt", "unapproved"),
    ]],
  });
  const before = join(fixture.sessionDir, "before.txt");
  const after = join(fixture.sessionDir, "after.txt");
  let reader: ReadableStreamDefaultReader<Uint8Array> | undefined;
  try {
    const run = await startInteractiveRun(fixture.baseUrl, "write a file");
    assert.equal(run.status, 200);
    reader = run.body!.getReader();
    const events = sseReader(reader);
    const pending = await readPending(events);
    assert.equal(pending.id.length > 0, true);
    await assert.rejects(() => access(before), {code: "ENOENT"});
    await assert.rejects(() => access(after), {code: "ENOENT"});

    const answer = await fetch(`${fixture.baseUrl}/api/sessions/session1/run/interaction`, {
      method: "POST",
      headers: {authorization: `Bearer ${TEST_API_TOKEN}`, "content-type": "application/json"},
      body: JSON.stringify({interaction_id: pending.id, decision: "decline", reason: "Do not write files."}),
    });
    assert.equal(answer.status, 200);
    for (;;) {
      if ((await events.next()).event === "done") break;
    }
    assert.equal(fixture.providerCalls(), 1, "decline stops before another model/tool round");
    await assert.rejects(() => access(before), {code: "ENOENT"});
    await assert.rejects(() => access(after), {code: "ENOENT"});
  } finally {
    await reader?.cancel().catch(() => undefined);
    await fixture.close();
  }
});

test("an edited draft forces a fresh model plan before any co-batched write", async () => {
  const finalMessage: ProviderOutput = {
    id: "assistant-final",
    type: "message",
    role: "assistant",
    content: [{type: "output_text", text: "Wrote the approved file."}],
  };
  const fixture = await createFixture({
    nativeSettings: (sessionDir) => ({
      ...interactiveSettings,
      write: {paths: {allow: [sessionDir]}},
    }),
    definitions: [nativeRequestDraftDefinition, nativeWriteDefinition],
    responses: [
      [
        writeCall("call-stale-before", "original.txt", "stale"),
        draftCall(),
        writeCall("call-stale-after", "after.txt", "stale"),
      ],
      [writeCall("call-approved-write", "approved.txt", "approved")],
      [finalMessage],
    ],
  });
  const original = join(fixture.sessionDir, "original.txt");
  const after = join(fixture.sessionDir, "after.txt");
  const approved = join(fixture.sessionDir, "approved.txt");
  let reader: ReadableStreamDefaultReader<Uint8Array> | undefined;
  try {
    const run = await startInteractiveRun(fixture.baseUrl, "write the requested file");
    assert.equal(run.status, 200);
    reader = run.body!.getReader();
    const events = sseReader(reader);
    const pending = await readPending(events);
    await assert.rejects(() => access(original), {code: "ENOENT"});
    await assert.rejects(() => access(after), {code: "ENOENT"});
    await assert.rejects(() => access(approved), {code: "ENOENT"});

    const edited = {goal: "write approved.txt"};
    const answer = await fetch(`${fixture.baseUrl}/api/sessions/session1/run/interaction`, {
      method: "POST",
      headers: {authorization: `Bearer ${TEST_API_TOKEN}`, "content-type": "application/json"},
      body: JSON.stringify({interaction_id: pending.id, decision: "update", draft: edited}),
    });
    assert.equal(answer.status, 200);
    for (;;) {
      if ((await events.next()).event === "done") break;
    }

    assert.equal(fixture.providerCalls(), 3);
    await assert.rejects(() => access(original), {code: "ENOENT"});
    await assert.rejects(() => access(after), {code: "ENOENT"});
    assert.equal(await readFile(approved, "utf8"), "approved");
  } finally {
    await reader?.cancel().catch(() => undefined);
    await fixture.close();
  }
});

test("a run without frontend interaction support auto-accepts after its configured delay", async () => {
  const draft = {goal: "continue in headless mode"};
  const toolCall: ProviderOutput = {
    ...draftCall(),
    arguments: JSON.stringify({draft}),
  };
  const finalMessage: ProviderOutput = {
    id: "headless-final",
    type: "message",
    role: "assistant",
    content: [{type: "output_text", text: "Proceeding."}],
  };
  const fixture = await createFixture({
    nativeSettings: {
      request_draft: {
        timeout_ms: 30_000,
        max_timeout_ms: 60_000,
        extend_ms: 10_000,
        headless_auto_accept_timeout_ms: 5,
      },
    },
    definitions: [nativeRequestDraftDefinition],
    responses: [[toolCall], [finalMessage]],
  });
  let reader: ReadableStreamDefaultReader<Uint8Array> | undefined;
  try {
    const run = await fetch(`${fixture.baseUrl}/api/sessions/session1/run`, {
      method: "POST",
      headers: {authorization: `Bearer ${TEST_API_TOKEN}`, "content-type": "application/json"},
      body: JSON.stringify({input: "continue this work"}),
    });
    assert.equal(run.status, 200);
    reader = run.body!.getReader();
    const events = sseReader(reader);
    let sawPending = false;
    for (;;) {
      const event = await events.next();
      if (event.event === "ui.message" && isRecord(event.data.message) && event.data.message.pending !== undefined) {
        sawPending = true;
      }
      if (event.event === "done") break;
    }
    assert.equal(sawPending, false, "headless requests are not published as answerable UI prompts");
    assert.equal(fixture.providerCalls(), 2);
    const accepted = JSON.parse(await readFile(join(fixture.sessionDir, "accepted-request.json"), "utf8")) as {
      source: string;
      action: string;
      draft: Record<string, unknown>;
    };
    assert.equal(accepted.source, "timeout");
    assert.equal(accepted.action, "accept");
    assert.deepEqual(accepted.draft, draft);
  } finally {
    await reader?.cancel().catch(() => undefined);
    await fixture.close();
  }
});
