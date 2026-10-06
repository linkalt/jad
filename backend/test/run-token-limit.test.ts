import assert from "node:assert/strict";
import {mkdir, mkdtemp, readFile, rm, writeFile} from "node:fs/promises";
import {tmpdir} from "node:os";
import {join} from "node:path";
import test from "node:test";

import {Ajv2020} from "ajv/dist/2020.js";
import OpenAI from "openai";

import {ConfigError, MAX_CONFIGURED_RUN_TOKENS, SessionConfigLoader} from "../src/config.js";
import {RunTokenLimitError, responseTokens} from "../src/run-token-budget.js";
import {createAgentServer} from "../src/server.js";
import {nativeReadDefinition} from "../src/tools/native/read.js";

const TOKEN = "test-api-token-0123456789abcdef0123456789abcdef";
const AUTH = {authorization: `Bearer ${TOKEN}`, "content-type": "application/json"};

// ---- unit: how a response is counted -------------------------------------------------

test("responseTokens uses total_tokens, else input plus output, else 0", () => {
  assert.equal(responseTokens({usage: {input_tokens: 10, output_tokens: 4, total_tokens: 20}}), 20, "the reported total wins");
  assert.equal(responseTokens({usage: {input_tokens: 10, output_tokens: 4}}), 14);
  assert.equal(responseTokens({usage: {input_tokens: 10}}), 0, "half a pair is not guessed");
  assert.equal(responseTokens({usage: {total_tokens: -1, input_tokens: 3, output_tokens: 2}}), 5, "an invalid total falls back");
  assert.equal(responseTokens({usage: {total_tokens: 1.5}}), 0);
  assert.equal(responseTokens({usage: "lots"}), 0);
  assert.equal(responseTokens({usage: null}), 0);
  assert.equal(responseTokens({}), 0);
});

test("RunTokenLimitError names the count and the limit, nothing else", () => {
  const error = new RunTokenLimitError(120, 100);
  assert.match(error.message, /used 120 tokens/);
  assert.match(error.message, /max_run_tokens limit of 100/);
});

// ---- config --------------------------------------------------------------------------

async function withSession<T>(
  setting: Record<string, unknown>,
  body: (load: () => ReturnType<SessionConfigLoader["loadSettings"]>) => Promise<T>,
): Promise<T> {
  const dir = await mkdtemp(join(tmpdir(), "agent-harness-run-token-config-"));
  try {
    await writeFile(join(dir, "model.json"), JSON.stringify({model: "gpt-test"}));
    await writeFile(join(dir, "setting.json"), JSON.stringify({cwd: dir, ...setting}));
    return await body(() => new SessionConfigLoader(dir).loadSettings());
  } finally {
    await rm(dir, {recursive: true, force: true});
  }
}

const rejects = (load: () => Promise<unknown>, pattern: RegExp) =>
  assert.rejects(load, (error: unknown) => error instanceof ConfigError && pattern.test(error.message));

test("max_run_tokens is absent by default, which means no cap", async () => {
  await withSession({}, async (load) => {
    const settings = await load();
    assert.equal("max_run_tokens" in settings, false);
    assert.equal(settings.max_run_tokens, undefined);
  });
});

test("max_run_tokens accepts 1 and the ceiling", async () => {
  for (const value of [1, 5000, MAX_CONFIGURED_RUN_TOKENS]) {
    await withSession({max_run_tokens: value}, async (load) => assert.equal((await load()).max_run_tokens, value));
  }
});

test("max_run_tokens rejects null, zero, negatives, fractions, strings and values above the ceiling", async () => {
  const message = /max_run_tokens must be a safe integer between 1 and 1000000000000/;
  for (const value of [null, 0, -5, 1.5, "100", true, [], {}, MAX_CONFIGURED_RUN_TOKENS + 1, Number.MAX_SAFE_INTEGER]) {
    await withSession({max_run_tokens: value}, (load) => rejects(load, message));
  }
});

test("the schema accepts max_run_tokens and rejects the same bad values", async () => {
  const schema = JSON.parse(await readFile(new URL("../../../sample/schemes/setting.schema.json", import.meta.url), "utf8"));
  const validate = new Ajv2020({strict: false, allErrors: true}).compile(schema);
  const base = {schema_version: 1, session_name: "s", cwd: "/tmp"};
  const ok = (max_run_tokens: unknown) => validate({...base, max_run_tokens});
  assert.equal(validate(base), true, JSON.stringify(validate.errors));
  assert.equal(ok(1), true, JSON.stringify(validate.errors));
  assert.equal(ok(200000), true);
  for (const bad of [0, -1, 1.5, "100", null, 1_000_000_000_001]) {
    assert.equal(ok(bad), false, `schema accepted ${JSON.stringify(bad)}`);
  }
});

// ---- end to end: a run through the real server ---------------------------------------

interface Round {
  /** Tool calls the model asks for; empty makes it a final answer. */
  readCalls?: number;
  usage?: Record<string, number> | null;
}

async function fixture(options: {rounds: Round[]; maxRunTokens?: unknown; maxToolRounds?: number}) {
  const root = await mkdtemp(join(tmpdir(), "agent-harness-run-token-run-"));
  const sessionDir = join(root, "session1");
  await mkdir(sessionDir, {recursive: true});
  await writeFile(join(sessionDir, "notes.txt"), "hello\n");
  await writeFile(
    join(sessionDir, "model.json"),
    JSON.stringify({schema_version: 1, provider: "fake", api_key_env: "FAKE_API_KEY", model: "fake-model"}),
  );
  await writeFile(
    join(sessionDir, "setting.json"),
    JSON.stringify({
      schema_version: 1,
      session_name: "run-token-limit",
      cwd: sessionDir,
      max_tool_rounds: options.maxToolRounds ?? 10,
      tool_definition_paths: ["tools.json"],
      tools: {native: {read: {paths: {allow: [sessionDir]}}}},
      ...(options.maxRunTokens === undefined ? {} : {max_run_tokens: options.maxRunTokens}),
    }),
  );
  await writeFile(join(root, "tools.json"), JSON.stringify({schema_version: 1, tools: [nativeReadDefinition]}));

  let requests = 0;
  const client = {
    responses: {
      create: async () => {
        const round = options.rounds[Math.min(requests, options.rounds.length - 1)]!;
        requests += 1;
        const n = requests;
        const calls = Array.from({length: round.readCalls ?? 0}, (_, i) => ({
          type: "function_call",
          call_id: `call_${n}_${i}`,
          name: "read",
          arguments: JSON.stringify({path: "notes.txt"}),
        }));
        const output = calls.length > 0
          ? calls
          : [{id: `msg_${n}`, type: "message", role: "assistant", content: [{type: "output_text", text: "all done"}]}];
        return (async function* () {
          for (const [index, item] of output.entries()) yield {type: "response.output_item.done", output_index: index, item};
          yield {
            type: "response.completed",
            response: {
              id: `response-${n}`,
              object: "response",
              status: "completed",
              model: "fake-model",
              ...(round.usage === null ? {} : {usage: round.usage ?? {input_tokens: 60, output_tokens: 10}}),
              output,
            },
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
    requests: () => requests,
    async close() {
      await running.close();
      await new Promise<void>((resolve) => running.server.close(() => resolve()));
      await rm(root, {recursive: true, force: true});
    },
  };
}

async function run(baseUrl: string, input = "go"): Promise<Array<{event: string; data: Record<string, unknown>}>> {
  const response = await fetch(`${baseUrl}/api/sessions/session1/run`, {
    method: "POST",
    headers: AUTH,
    body: JSON.stringify({input}),
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

async function records(sessionDir: string): Promise<Array<Record<string, unknown>>> {
  const text = await readFile(join(sessionDir, "messages.jsonl"), "utf8").catch(() => "");
  return text.split("\n").filter(Boolean).map((line) => JSON.parse(line) as Record<string, unknown>);
}

// Each round reports 70 tokens (60 in, 10 out), so after n rounds a run has used 70 * n.

test("a run that reaches the cap before another tool round stops, with a result for every call", async () => {
  const f = await fixture({rounds: [{readCalls: 2}, {readCalls: 2}, {}], maxRunTokens: 100});
  try {
    const events = await run(f.baseUrl);
    assert.equal(f.requests(), 2, "round 1 (70) is under 100, round 2 (140) reaches it, no third request");
    assert.equal(names(events).includes("completed"), false);
    assert.equal(
      errorMessage(events),
      "The run stopped before its next round: it used 140 tokens, which reached the max_run_tokens limit of 100.",
    );
    const outputs = (await records(f.sessionDir)).filter((r) => r.type === "function_call_output");
    assert.equal(outputs.length, 4, "every call has an output, so the history stays valid");
    assert.deepEqual(outputs.slice(0, 2).map((r) => r.call_id), ["call_1_0", "call_1_1"]);
    assert.match(String(outputs[2]?.output), /run_token_limit/);
    assert.match(String(outputs[3]?.output), /run_token_limit/);
    assert.doesNotMatch(String(outputs[0]?.output), /run_token_limit/, "the round that ran is not marked");
  } finally {
    await f.close();
  }
});

test("a cap below the first response stops after one provider request", async () => {
  const f = await fixture({rounds: [{readCalls: 1}, {}], maxRunTokens: 50});
  try {
    const events = await run(f.baseUrl);
    assert.equal(f.requests(), 1);
    assert.match(String(errorMessage(events)), /used 70 tokens, which reached the max_run_tokens limit of 50/);
  } finally {
    await f.close();
  }
});

test("usage exactly at the cap stops the run: the cap is reached, not exceeded", async () => {
  const f = await fixture({rounds: [{readCalls: 1}, {}], maxRunTokens: 70});
  try {
    const events = await run(f.baseUrl);
    assert.equal(f.requests(), 1);
    assert.match(String(errorMessage(events)), /used 70 tokens/);
  } finally {
    await f.close();
  }
});

test("a final answer completes even when the cap was passed, because no round is left to start", async () => {
  const f = await fixture({rounds: [{usage: {input_tokens: 900, output_tokens: 100}}], maxRunTokens: 100});
  try {
    const events = await run(f.baseUrl);
    assert.ok(names(events).includes("completed"), names(events).join(","));
    assert.equal(errorMessage(events), undefined);
    assert.equal(f.requests(), 1);
  } finally {
    await f.close();
  }
});

test("a cap above what the run uses changes nothing", async () => {
  const f = await fixture({rounds: [{readCalls: 1}, {readCalls: 1}, {}], maxRunTokens: 100_000});
  try {
    const events = await run(f.baseUrl);
    assert.ok(names(events).includes("completed"), names(events).join(","));
    assert.equal(f.requests(), 3);
    assert.equal((await records(f.sessionDir)).some((r) => String(r.output ?? "").includes("run_token_limit")), false);
  } finally {
    await f.close();
  }
});

test("without max_run_tokens the run is not capped, however many tokens it reports", async () => {
  const f = await fixture({
    rounds: [{readCalls: 1, usage: {input_tokens: 5_000_000, output_tokens: 1}}, {readCalls: 1, usage: {input_tokens: 5_000_000, output_tokens: 1}}, {}],
  });
  try {
    const events = await run(f.baseUrl);
    assert.ok(names(events).includes("completed"), names(events).join(","));
    assert.equal(f.requests(), 3);
  } finally {
    await f.close();
  }
});

test("responses without usage count as 0, so they never trip the cap", async () => {
  const f = await fixture({rounds: [{readCalls: 1, usage: null}, {readCalls: 1, usage: null}, {usage: null}], maxRunTokens: 1});
  try {
    const events = await run(f.baseUrl);
    assert.ok(names(events).includes("completed"), names(events).join(","));
    assert.equal(f.requests(), 3);
  } finally {
    await f.close();
  }
});

test("the count starts again with the next run, and the session stays usable", async () => {
  const f = await fixture({rounds: [{readCalls: 1}, {readCalls: 1}, {}, {readCalls: 1}, {}], maxRunTokens: 100});
  try {
    const first = await run(f.baseUrl, "first");
    assert.match(String(errorMessage(first)), /used 140 tokens/);
    assert.equal(f.requests(), 2);
    // Run two: its first round (70) is under the cap, the next response is a final answer.
    const second = await run(f.baseUrl, "second");
    assert.ok(names(second).includes("completed"), `${names(second).join(",")} ${String(errorMessage(second))}`);
    assert.equal(errorMessage(second), undefined);
  } finally {
    await f.close();
  }
});

test("the tool-round limit is still reported with its own message when it comes first", async () => {
  const f = await fixture({rounds: [{readCalls: 1}], maxRunTokens: 1_000_000, maxToolRounds: 1});
  try {
    const events = await run(f.baseUrl);
    assert.match(String(errorMessage(events)), /The run failed\. Response details were withheld/);
    const outputs = (await records(f.sessionDir)).filter((r) => r.type === "function_call_output");
    assert.match(String(outputs.at(-1)?.output), /tool_round_limit/);
  } finally {
    await f.close();
  }
});

test("max_run_tokens null refuses the run and names the key", async () => {
  const f = await fixture({rounds: [{}], maxRunTokens: null});
  try {
    const response = await fetch(`${f.baseUrl}/api/sessions/session1/run`, {
      method: "POST",
      headers: AUTH,
      body: JSON.stringify({input: "go"}),
    });
    const body = await response.text();
    assert.ok(response.status >= 400, `status ${response.status}`);
    assert.match(body, /max_run_tokens must be a safe integer between 1 and 1000000000000/);
    assert.equal(f.requests(), 0);
  } finally {
    await f.close();
  }
});
