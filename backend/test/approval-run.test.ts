import assert from "node:assert/strict";
import {access, mkdir, mkdtemp, readFile, rm, writeFile} from "node:fs/promises";
import {tmpdir} from "node:os";
import {join} from "node:path";
import test from "node:test";

import type OpenAI from "openai";

import {createAgentServer, type AgentServerOptions} from "../src/server.js";
import {nativeReadDefinition} from "../src/tools/native/read.js";
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

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

async function createFixture(options: {
  nativeSettings: (sessionDir: string) => Record<string, unknown>;
  approval?: Record<string, unknown>;
  responses: ProviderOutput[][];
}) {
  const root = await mkdtemp(join(tmpdir(), "agent-harness-approval-run-"));
  const sessionDir = join(root, "session1");
  await mkdir(sessionDir, {recursive: true});
  await writeFile(
    join(sessionDir, "model.json"),
    JSON.stringify({schema_version: 1, provider: "fake", api_key_env: "FAKE_API_KEY", model: "fake-model"}),
  );
  await writeFile(
    join(sessionDir, "setting.json"),
    JSON.stringify({
      schema_version: 1,
      session_name: "approval-run",
      cwd: sessionDir,
      tool_definition_paths: ["tools.json"],
      tools: {native: options.nativeSettings(sessionDir)},
      ...(options.approval === undefined ? {} : {approval: options.approval}),
    }),
  );
  await writeFile(
    join(root, "tools.json"),
    JSON.stringify({schema_version: 1, tools: [nativeReadDefinition, nativeWriteDefinition]}),
  );

  let providerCalls = 0;
  const requests: unknown[] = [];
  const fakeClient = {
    responses: {
      create: async (request: unknown) => {
        requests.push(request);
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
    baseUrl: `http://127.0.0.1:${address.port}`,
    providerCalls: () => providerCalls,
    requests,
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

function readCall(callId: string, path: string): ProviderOutput {
  return {id: `fc-${callId}`, type: "function_call", call_id: callId, name: "read", arguments: JSON.stringify({path})};
}

function finalMessage(text: string): ProviderOutput {
  return {id: "final", type: "message", role: "assistant", content: [{type: "output_text", text}]};
}

const AUTH = {authorization: `Bearer ${TEST_API_TOKEN}`, "content-type": "application/json"};

function startRun(baseUrl: string, supportsInteraction: boolean) {
  return fetch(`${baseUrl}/api/sessions/session1/run`, {
    method: "POST",
    headers: AUTH,
    body: JSON.stringify({input: "go", ...(supportsInteraction ? {supports_interaction: true} : {})}),
  });
}

async function readPending(events: ReturnType<typeof sseReader>): Promise<Record<string, unknown>> {
  for (;;) {
    const event = await events.next();
    if (event.event !== "interaction") continue;
    const pending = event.data.pending;
    if (isRecord(pending) && typeof pending.id === "string") return pending;
  }
}

async function untilDone(events: ReturnType<typeof sseReader>): Promise<void> {
  for (;;) {
    if ((await events.next()).event === "done") return;
  }
}

function reply(baseUrl: string, body: Record<string, unknown>) {
  return fetch(`${baseUrl}/api/sessions/session1/run/interaction`, {
    method: "POST",
    headers: AUTH,
    body: JSON.stringify(body),
  });
}

/** The tool output the provider was sent on its second request, as text. */
function secondRequestToolOutputs(requests: unknown[]): string[] {
  const second = requests[1] as {input?: unknown[]} | undefined;
  assert.ok(second && Array.isArray(second.input), "the provider got a second request");
  return second.input
    .filter((item): item is Record<string, unknown> => isRecord(item) && item.type === "function_call_output")
    .map((item) => String(item.output));
}

const allowSession = (sessionDir: string) => ({
  read: {paths: {allow: [sessionDir]}},
  write: {paths: {allow: [sessionDir]}},
});

test("with approval off a write runs at once and no question is published", async () => {
  const fixture = await createFixture({
    nativeSettings: allowSession,
    responses: [[writeCall("w1", "a.txt", "hello")], [finalMessage("done")]],
  });
  let reader: ReadableStreamDefaultReader<Uint8Array> | undefined;
  try {
    const run = await startRun(fixture.baseUrl, true);
    reader = run.body!.getReader();
    const events = sseReader(reader);
    let sawQuestion = false;
    for (;;) {
      const event = await events.next();
      if (event.event === "interaction" && isRecord(event.data.pending)) sawQuestion = true;
      if (event.event === "done") break;
    }
    assert.equal(sawQuestion, false);
    assert.equal(await readFile(join(fixture.sessionDir, "a.txt"), "utf8"), "hello");
  } finally {
    await reader?.cancel().catch(() => undefined);
    await fixture.close();
  }
});

test("approval ask holds the write until accepted, then runs it", async () => {
  const fixture = await createFixture({
    nativeSettings: (dir) => ({...allowSession(dir), write: {paths: {allow: [dir]}, approval: "ask"}}),
    responses: [[writeCall("w1", "a.txt", "hello")], [finalMessage("done")]],
  });
  const target = join(fixture.sessionDir, "a.txt");
  let reader: ReadableStreamDefaultReader<Uint8Array> | undefined;
  try {
    const run = await startRun(fixture.baseUrl, true);
    reader = run.body!.getReader();
    const events = sseReader(reader);
    const pending = await readPending(events);
    assert.equal(pending.kind, "approval");
    assert.equal(pending.tool, "write");
    assert.equal(pending.tool_call_id, "w1");
    assert.equal((pending.draft as Record<string, unknown>).path, "a.txt");
    await assert.rejects(() => access(target), {code: "ENOENT"}, "nothing is written while the question is open");

    const answer = await reply(fixture.baseUrl, {interaction_id: pending.id, decision: "accept"});
    assert.equal(answer.status, 200);
    await untilDone(events);
    assert.equal(await readFile(target, "utf8"), "hello");
    assert.equal(fixture.providerCalls(), 2);
  } finally {
    await reader?.cancel().catch(() => undefined);
    await fixture.close();
  }
});

test("declining gives the model a blocked result with the reason and writes nothing", async () => {
  const fixture = await createFixture({
    nativeSettings: (dir) => ({...allowSession(dir), write: {paths: {allow: [dir]}, approval: "ask"}}),
    responses: [[writeCall("w1", "a.txt", "hello")], [finalMessage("understood")]],
  });
  let reader: ReadableStreamDefaultReader<Uint8Array> | undefined;
  try {
    const run = await startRun(fixture.baseUrl, true);
    reader = run.body!.getReader();
    const events = sseReader(reader);
    const pending = await readPending(events);
    const answer = await reply(fixture.baseUrl, {interaction_id: pending.id, decision: "decline", reason: "not that file"});
    assert.equal(answer.status, 200);
    await untilDone(events);
    await assert.rejects(() => access(join(fixture.sessionDir, "a.txt")), {code: "ENOENT"});
    assert.equal(fixture.providerCalls(), 2, "the run continues so the model can re-plan");
    const [output] = secondRequestToolOutputs(fixture.requests);
    assert.match(output!, /status.*blocked|blocked/);
    assert.match(output!, /approval_denied/);
    assert.match(output!, /not that file/);
  } finally {
    await reader?.cancel().catch(() => undefined);
    await fixture.close();
  }
});

test("update is refused for an approval and the question stays open", async () => {
  const fixture = await createFixture({
    nativeSettings: (dir) => ({...allowSession(dir), write: {paths: {allow: [dir]}, approval: "ask"}}),
    responses: [[writeCall("w1", "a.txt", "hello")], [finalMessage("done")]],
  });
  let reader: ReadableStreamDefaultReader<Uint8Array> | undefined;
  try {
    const run = await startRun(fixture.baseUrl, true);
    reader = run.body!.getReader();
    const events = sseReader(reader);
    const pending = await readPending(events);
    const refused = await reply(fixture.baseUrl, {
      interaction_id: pending.id,
      decision: "update",
      draft: {path: "elsewhere.txt"},
    });
    assert.equal(refused.status, 400);
    await assert.rejects(() => access(join(fixture.sessionDir, "a.txt")), {code: "ENOENT"});
    const accepted = await reply(fixture.baseUrl, {interaction_id: pending.id, decision: "accept"});
    assert.equal(accepted.status, 200);
    await untilDone(events);
    assert.equal(await readFile(join(fixture.sessionDir, "a.txt"), "utf8"), "hello");
  } finally {
    await reader?.cancel().catch(() => undefined);
    await fixture.close();
  }
});

test("a headless run with approval ask and headless deny blocks the call", async () => {
  const fixture = await createFixture({
    nativeSettings: (dir) => ({...allowSession(dir), write: {paths: {allow: [dir]}, approval: "ask"}}),
    responses: [[writeCall("w1", "a.txt", "hello")], [finalMessage("ok")]],
  });
  let reader: ReadableStreamDefaultReader<Uint8Array> | undefined;
  try {
    const run = await startRun(fixture.baseUrl, false);
    reader = run.body!.getReader();
    await untilDone(sseReader(reader));
    await assert.rejects(() => access(join(fixture.sessionDir, "a.txt")), {code: "ENOENT"});
    const [output] = secondRequestToolOutputs(fixture.requests);
    assert.match(output!, /approval_unavailable/);
  } finally {
    await reader?.cancel().catch(() => undefined);
    await fixture.close();
  }
});

test("a headless run with headless allow runs the call", async () => {
  const fixture = await createFixture({
    nativeSettings: (dir) => ({...allowSession(dir), write: {paths: {allow: [dir]}, approval: "ask"}}),
    approval: {headless: "allow"},
    responses: [[writeCall("w1", "a.txt", "hello")], [finalMessage("ok")]],
  });
  let reader: ReadableStreamDefaultReader<Uint8Array> | undefined;
  try {
    const run = await startRun(fixture.baseUrl, false);
    reader = run.body!.getReader();
    await untilDone(sseReader(reader));
    assert.equal(await readFile(join(fixture.sessionDir, "a.txt"), "utf8"), "hello");
  } finally {
    await reader?.cancel().catch(() => undefined);
    await fixture.close();
  }
});

test("a read in the same response does not wait for the write's approval", async () => {
  const fixture = await createFixture({
    nativeSettings: (dir) => ({...allowSession(dir), write: {paths: {allow: [dir]}, approval: "ask"}}),
    responses: [[writeCall("w1", "a.txt", "hello"), readCall("r1", ".")], [finalMessage("done")]],
  });
  let reader: ReadableStreamDefaultReader<Uint8Array> | undefined;
  try {
    const run = await startRun(fixture.baseUrl, true);
    reader = run.body!.getReader();
    const events = sseReader(reader);
    let readFinishedBeforeAnswer = false;
    let pending: Record<string, unknown> | undefined;
    for (;;) {
      const event = await events.next();
      if (event.event === "interaction" && isRecord(event.data.pending)) {
        pending = event.data.pending;
        break;
      }
      if (event.event === "ui.message" && JSON.stringify(event.data).includes('"r1"')) readFinishedBeforeAnswer = true;
    }
    // The read result is published as soon as it settles; keep reading events for it while the write waits.
    const deadline = Date.now() + 2000;
    while (!readFinishedBeforeAnswer && Date.now() < deadline) {
      const next = await Promise.race([
        events.next(),
        new Promise<undefined>((resolve) => setTimeout(() => resolve(undefined), 300)),
      ]);
      if (next === undefined) break;
      if (JSON.stringify(next.data).includes("r1")) readFinishedBeforeAnswer = true;
    }
    assert.ok(pending);
    await reply(fixture.baseUrl, {interaction_id: pending.id, decision: "accept"});
    await untilDone(events);
    assert.equal(readFinishedBeforeAnswer, true, "the read result was published while the write still waited");
  } finally {
    await reader?.cancel().catch(() => undefined);
    await fixture.close();
  }
});

test("approval.default ask makes a write with no approval key of its own wait for the user", async () => {
  const fixture = await createFixture({
    nativeSettings: allowSession,
    approval: {default: "ask"},
    responses: [[writeCall("w1", "a.txt", "hello")], [finalMessage("done")]],
  });
  const target = join(fixture.sessionDir, "a.txt");
  let reader: ReadableStreamDefaultReader<Uint8Array> | undefined;
  try {
    const run = await startRun(fixture.baseUrl, true);
    reader = run.body!.getReader();
    const events = sseReader(reader);
    const pending = await readPending(events);
    assert.equal(pending.kind, "approval");
    assert.equal(pending.tool, "write");
    await assert.rejects(() => access(target), {code: "ENOENT"}, "nothing is written while the question is open");
    const answer = await reply(fixture.baseUrl, {interaction_id: pending.id, decision: "decline", reason: "no"});
    assert.equal(answer.status, 200);
    await untilDone(events);
    await assert.rejects(() => access(target), {code: "ENOENT"});
    const [output] = secondRequestToolOutputs(fixture.requests);
    assert.match(output!, /approval_denied/);
  } finally {
    await reader?.cancel().catch(() => undefined);
    await fixture.close();
  }
});

test("an explicit approval off on the tool wins over approval.default ask", async () => {
  const fixture = await createFixture({
    nativeSettings: (dir) => ({...allowSession(dir), write: {paths: {allow: [dir]}, approval: "off"}}),
    approval: {default: "ask"},
    responses: [[writeCall("w1", "a.txt", "hello")], [finalMessage("done")]],
  });
  let reader: ReadableStreamDefaultReader<Uint8Array> | undefined;
  try {
    const run = await startRun(fixture.baseUrl, true);
    reader = run.body!.getReader();
    const events = sseReader(reader);
    let sawQuestion = false;
    for (;;) {
      const event = await events.next();
      if (event.event === "interaction" && isRecord(event.data.pending)) sawQuestion = true;
      if (event.event === "done") break;
    }
    assert.equal(sawQuestion, false);
    assert.equal(await readFile(join(fixture.sessionDir, "a.txt"), "utf8"), "hello");
  } finally {
    await reader?.cancel().catch(() => undefined);
    await fixture.close();
  }
});
