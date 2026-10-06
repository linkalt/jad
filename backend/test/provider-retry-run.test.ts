import assert from "node:assert/strict";
import {mkdir, mkdtemp, readFile, rm, writeFile} from "node:fs/promises";
import {tmpdir} from "node:os";
import {join} from "node:path";
import test from "node:test";

import OpenAI from "openai";

import {createAgentServer} from "../src/server.js";
import {nativeReadDefinition} from "../src/tools/native/read.js";

const TOKEN = "test-api-token-0123456789abcdef0123456789abcdef";
const AUTH = {authorization: `Bearer ${TOKEN}`, "content-type": "application/json"};

type Step =
  | {error: Error}
  | {text: string}
  /** Opens the stream, then fails after the first event: a failure after streaming started. */
  | {midStreamError: Error};

const rateLimit = (headers: Record<string, string> = {"retry-after-ms": "1"}) =>
  OpenAI.APIError.generate(429, {error: {message: "slow down"}}, "slow down", new Headers(headers));

async function fixture(options: {steps: Step[]; providerRetry?: unknown}) {
  const root = await mkdtemp(join(tmpdir(), "agent-harness-provider-retry-run-"));
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
      session_name: "provider-retry-run",
      cwd: sessionDir,
      tool_definition_paths: ["tools.json"],
      tools: {native: {read: {paths: {allow: [sessionDir]}}}},
      ...(options.providerRetry === undefined ? {} : {provider_retry: options.providerRetry}),
    }),
  );
  await writeFile(join(root, "tools.json"), JSON.stringify({schema_version: 1, tools: [nativeReadDefinition]}));

  const calls: Array<{maxRetries: unknown}> = [];
  const client = {
    responses: {
      create: async (_request: unknown, requestOptions?: {maxRetries?: unknown}) => {
        calls.push({maxRetries: requestOptions?.maxRetries});
        const step = options.steps[Math.min(calls.length - 1, options.steps.length - 1)]!;
        if ("error" in step) throw step.error;
        const output = [{id: "final", type: "message", role: "assistant", content: [{type: "output_text", text: "text" in step ? step.text : ""}]}];
        return (async function* () {
          if ("midStreamError" in step) {
            yield {type: "response.created", response: {id: "r", status: "in_progress", output: []}};
            throw step.midStreamError;
          }
          yield {type: "response.output_item.done", output_index: 0, item: output[0]};
          yield {
            type: "response.completed",
            response: {id: `response-${calls.length}`, object: "response", status: "completed", model: "fake-model", usage: {input_tokens: 10, output_tokens: 4}, output},
          };
        })();
      },
    },
  } as unknown as OpenAI;
  const running = createAgentServer({sessionsDir: root, client, apiToken: TOKEN});
  await new Promise<void>((resolve, reject) => {
    running.server.once("error", reject);
    running.server.listen(0, "127.0.0.1", () => resolve());
  });
  const address = running.server.address();
  assert.ok(address !== null && typeof address !== "string");
  return {
    sessionDir,
    baseUrl: `http://127.0.0.1:${address.port}`,
    calls,
    async close() {
      await running.close();
      await new Promise<void>((resolve) => running.server.close(() => resolve()));
      await rm(root, {recursive: true, force: true});
    },
  };
}

/** Runs one turn and returns every SSE event up to `done`. */
async function run(baseUrl: string): Promise<Array<{event: string; data: Record<string, unknown>}>> {
  const response = await fetch(`${baseUrl}/api/sessions/session1/run`, {
    method: "POST",
    headers: AUTH,
    body: JSON.stringify({input: "go"}),
  });
  assert.equal(response.status, 200);
  const text = await response.text();
  const events: Array<{event: string; data: Record<string, unknown>}> = [];
  for (const block of text.split("\n\n")) {
    let event = "message";
    let data = "";
    for (const line of block.split("\n")) {
      if (line.startsWith("event:")) event = line.slice(6).trim();
      else if (line.startsWith("data:")) data += line.slice(5).trim();
    }
    if (data) events.push({event, data: JSON.parse(data) as Record<string, unknown>});
  }
  return events;
}

const names = (events: Array<{event: string}>) => events.map((e) => e.event);
const errorMessage = (events: Array<{event: string; data: Record<string, unknown>}>) =>
  events.find((e) => e.event === "error")?.data.message;

async function transcript(sessionDir: string): Promise<string> {
  return readFile(join(sessionDir, "messages.jsonl"), "utf8").catch(() => "");
}

test("two rate limits then success: the run completes, and the SDK's own retries are off", async () => {
  const f = await fixture({
    steps: [{error: rateLimit()}, {error: rateLimit()}, {text: "recovered"}],
    providerRetry: {max_retries: 2, initial_delay_ms: 1, max_delay_ms: 50},
  });
  try {
    const events = await run(f.baseUrl);
    assert.ok(names(events).includes("completed"), names(events).join(","));
    assert.equal(errorMessage(events), undefined);
    assert.equal(f.calls.length, 3, "one attempt plus two retries");
    assert.deepEqual(f.calls.map((c) => c.maxRetries), [0, 0, 0], "each attempt switches the SDK's retries off");
    const records = (await transcript(f.sessionDir)).trim().split("\n").map((l) => JSON.parse(l) as Record<string, unknown>);
    assert.deepEqual(records.map((r) => r.type), ["message", "response.completed"], "failed attempts leave nothing in the transcript");
  } finally {
    await f.close();
  }
});

test("retries used up: the run fails with the provider status, after max_retries + 1 attempts", async () => {
  const f = await fixture({
    steps: [{error: rateLimit()}],
    providerRetry: {max_retries: 3, initial_delay_ms: 1, max_delay_ms: 50},
  });
  try {
    const events = await run(f.baseUrl);
    assert.match(String(errorMessage(events)), /The provider request failed \(HTTP 429\)/);
    assert.equal(f.calls.length, 4);
    // The session stays usable and the user message was kept.
    const kept = (await transcript(f.sessionDir)).trim().split("\n").map((l) => JSON.parse(l) as Record<string, unknown>);
    assert.deepEqual(kept.map((r) => r.type), ["message"]);
  } finally {
    await f.close();
  }
});

test("max_retries 0 makes one attempt", async () => {
  const f = await fixture({steps: [{error: rateLimit()}], providerRetry: {max_retries: 0}});
  try {
    const events = await run(f.baseUrl);
    assert.match(String(errorMessage(events)), /HTTP 429/);
    assert.equal(f.calls.length, 1);
  } finally {
    await f.close();
  }
});

test("without a provider_retry key the default is two retries, as before the setting existed", async () => {
  // Retry-After-Ms keeps the wait short; the count is what is measured.
  const f = await fixture({steps: [{error: rateLimit()}]});
  try {
    const events = await run(f.baseUrl);
    assert.match(String(errorMessage(events)), /HTTP 429/);
    assert.equal(f.calls.length, 3);
  } finally {
    await f.close();
  }
});

test("a status that is not retryable is reported after one attempt", async () => {
  for (const status of [400, 401, 404]) {
    const f = await fixture({
      steps: [{error: OpenAI.APIError.generate(status, {error: {message: "no"}}, "no", new Headers())}],
      providerRetry: {initial_delay_ms: 1},
    });
    try {
      const events = await run(f.baseUrl);
      assert.match(String(errorMessage(events)), new RegExp(`HTTP ${status}`));
      assert.equal(f.calls.length, 1, `status ${status}`);
    } finally {
      await f.close();
    }
  }
});

test("a connection failure is retried", async () => {
  const f = await fixture({
    steps: [{error: new OpenAI.APIConnectionError({message: "ECONNRESET"})}, {text: "back"}],
    providerRetry: {initial_delay_ms: 1, max_delay_ms: 5},
  });
  try {
    const events = await run(f.baseUrl);
    assert.ok(names(events).includes("completed"), names(events).join(","));
    assert.equal(f.calls.length, 2);
  } finally {
    await f.close();
  }
});

test("a failure after the stream started is not retried", async () => {
  const f = await fixture({
    steps: [{midStreamError: OpenAI.APIError.generate(500, {error: {message: "boom"}}, "boom", new Headers())}],
    providerRetry: {max_retries: 5, initial_delay_ms: 1, max_delay_ms: 5},
  });
  try {
    const events = await run(f.baseUrl);
    assert.ok(errorMessage(events) !== undefined, names(events).join(","));
    assert.equal(f.calls.length, 1, "events had arrived, so nothing is retried");
  } finally {
    await f.close();
  }
});

test("a stop during a retry wait ends the run at once and makes no further attempt", async () => {
  const f = await fixture({
    steps: [{error: rateLimit({"retry-after-ms": "30000"})}],
    providerRetry: {max_retries: 5, initial_delay_ms: 1, max_delay_ms: 60_000},
  });
  try {
    const started = Date.now();
    const pending = run(f.baseUrl);
    // Wait for the first attempt, which is now sleeping for 30 s.
    for (let i = 0; i < 200 && f.calls.length === 0; i += 1) await new Promise((r) => setTimeout(r, 10));
    assert.equal(f.calls.length, 1);
    const stop = await fetch(`${f.baseUrl}/api/sessions/session1/run/stop`, {method: "POST", headers: AUTH, body: "{}"});
    assert.equal(stop.status, 200);
    const events = await pending;
    assert.ok(Date.now() - started < 10_000, `the run waited ${Date.now() - started} ms`);
    assert.equal(f.calls.length, 1, "no attempt after the stop");
    assert.match(String(errorMessage(events)), /stopped/i);
  } finally {
    await f.close();
  }
});
