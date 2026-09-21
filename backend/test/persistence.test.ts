import { createHash } from "node:crypto";
import { appendFile, mkdir, readFile, readdir, rm, writeFile } from "node:fs/promises";
import { mkdtemp } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";
import assert from "node:assert/strict";

import {
  PersistenceError,
  SessionResponseStore,
  type ResponseCompletedEvent,
} from "../src/persistence.js";
import { persistResponseStream } from "../src/responses.js";
import { ConfigError, SessionConfigLoader, resolveApiKey, sha256Json } from "../src/config.js";
import { AgentSession } from "../src/session.js";
import type OpenAI from "openai";

async function makeSession(): Promise<string> {
  return mkdtemp(join(tmpdir(), "agent-harness-persistence-"));
}

async function removeSession(path: string): Promise<void> {
  await rm(path, { recursive: true, force: true });
}

function completedEvent(
  output: Array<Record<string, unknown>>,
  responseId = "resp_test",
): ResponseCompletedEvent {
  return {
    type: "response.completed",
    response: {
      id: responseId,
      object: "response",
      status: "completed",
      model: "gpt-test",
      usage: { input_tokens: 10, output_tokens: 4 },
      output,
    },
  };
}

test("stages completed items and commits one canonical response event", async () => {
  const sessionDir = await makeSession();
  try {
    const store = await SessionResponseStore.open(sessionDir, { durability: "strict" });
    const turn = await store.beginTurn("turn-out-of-order");
    const messageItem = {
      type: "message",
      role: "assistant",
      content: [{ type: "output_text", text: "Hello" }],
    };
    const functionItem = {
      type: "function_call",
      call_id: "call_123",
      name: "search",
      arguments: "{}",
    };

    await turn.stageOutputItem({
      type: "response.output_item.done",
      output_index: 1,
      item: functionItem,
    });
    await turn.stageOutputItem({
      type: "response.output_item.done",
      output_index: 0,
      item: messageItem,
    });

    const result = await turn.commit(
      completedEvent([messageItem, functionItem]),
    );
    assert.ok(result.bytes > 0);
    assert.equal(result.already_present, false);

    const raw = await readFile(join(sessionDir, "messages.jsonl"), "utf8");
    const lines = raw.trimEnd().split("\n");
    assert.equal(lines.length, 1);
    assert.equal(raw.endsWith("\n"), true);

    const persisted = JSON.parse(lines[0]!) as ResponseCompletedEvent;
    assert.equal(persisted.type, "response.completed");
    assert.deepEqual(persisted.response.output, [messageItem, functionItem]);
    assert.deepEqual(persisted.response.usage, { input_tokens: 10, output_tokens: 4 });
    assert.equal(raw.includes("output_index"), false);

    const stagingEntries = await readdir(join(sessionDir, "staging"));
    assert.deepEqual(stagingEntries, []);
    await store.close();
  } finally {
    await removeSession(sessionDir);
  }
});

test("stores raw input items and reconstructs request history", async () => {
  const sessionDir = await makeSession();
  try {
    const store = await SessionResponseStore.open(sessionDir);
    const userItem = {
      type: "message",
      role: "user",
      content: [{ type: "input_text", text: "Find the answer" }],
    };
    const functionCall = {
      type: "function_call",
      call_id: "call_history",
      name: "lookup",
      arguments: "{}",
    };
    const toolOutput = {
      type: "function_call_output",
      call_id: "call_history",
      output: "answer",
    };

    await store.appendInputItem(userItem);
    const turn = await store.beginTurn("turn-history");
    await turn.stageOutputItem({
      type: "response.output_item.done",
      output_index: 0,
      item: functionCall,
    });
    await turn.commit(completedEvent([functionCall], "resp_history"));
    await store.appendInputItem(toolOutput);

    const history = [];
    for await (const item of store.readInputItems()) history.push(item);
    assert.deepEqual(history, [userItem, functionCall, toolOutput]);

    const lines = (await readFile(join(sessionDir, "messages.jsonl"), "utf8"))
      .trimEnd()
      .split("\n");
    assert.equal(JSON.parse(lines[0]!).type, "message");
    assert.equal(JSON.parse(lines[1]!).type, "response.completed");
    assert.equal(JSON.parse(lines[2]!).type, "function_call_output");
    await store.close();
  } finally {
    await removeSession(sessionDir);
  }
});

test("uses fallback sequence numbers when output_index is absent", async () => {
  const sessionDir = await makeSession();
  try {
    const store = await SessionResponseStore.open(sessionDir);
    const turn = await store.beginTurn("turn-sequence");
    const first = { type: "message", role: "assistant", content: [] };
    const second = { type: "message", role: "assistant", content: [] };

    await turn.stageOutputItem({ type: "response.output_item.done", item: first });
    await turn.stageOutputItem({ type: "response.output_item.done", item: second });
    await turn.commit(completedEvent([first, second], "resp_sequence"));

    const [line] = (await readFile(join(sessionDir, "messages.jsonl"), "utf8"))
      .trimEnd()
      .split("\n");
    const persisted = JSON.parse(line!) as ResponseCompletedEvent;
    assert.deepEqual(persisted.response.output, [first, second]);
    await store.close();
  } finally {
    await removeSession(sessionDir);
  }
});

test("repairs an incomplete messages.jsonl trailing record on open", async () => {
  const sessionDir = await makeSession();
  try {
    await mkdir(sessionDir, { recursive: true });
    await writeFile(
      join(sessionDir, "messages.jsonl"),
      '{"type":"response.completed","response":{"id":"resp_ok"}}\n{"type":"response.completed"',
    );

    const store = await SessionResponseStore.open(sessionDir);
    const raw = await readFile(join(sessionDir, "messages.jsonl"), "utf8");
    assert.equal(raw, '{"type":"response.completed","response":{"id":"resp_ok"}}\n');
    await store.close();
  } finally {
    await removeSession(sessionDir);
  }
});

test("retains a streaming turn but removes a partial staging line during recovery", async () => {
  const sessionDir = await makeSession();
  try {
    const store = await SessionResponseStore.open(sessionDir);
    const turn = await store.beginTurn("turn-partial-stage");
    await turn.stageOutputItem({
      type: "response.output_item.done",
      output_index: 0,
      item: { type: "message", role: "assistant", content: [] },
    });
    await turn.close();

    const itemsPath = join(sessionDir, "staging", "turn-partial-stage", "items.jsonl");
    await appendFile(itemsPath, '{"output_index":1,"item":{"type":"message"');
    await store.close();

    const reopened = await SessionResponseStore.open(sessionDir);
    const report = await reopened.recover();
    assert.equal(report.inspected_turns, 1);
    assert.equal(report.retained_turns, 1);
    assert.deepEqual(report.errors, []);

    const repaired = await readFile(itemsPath, "utf8");
    assert.equal(repaired.endsWith("\n"), true);
    assert.equal(repaired.includes('"output_index":1'), false);
    await reopened.close();
  } finally {
    await removeSession(sessionDir);
  }
});

test("recovers a rendered final record after a commit interruption", async () => {
  const sessionDir = await makeSession();
  try {
    const turnDir = join(sessionDir, "staging", "turn-recover-commit");
    await mkdir(turnDir, { recursive: true });
    const finalLine =
      '{"type":"response.completed","response":{"id":"resp_recover","status":"completed","output":[]}}\n';
    const hash = createHash("sha256").update(finalLine).digest("hex");
    await writeFile(join(turnDir, "manifest.json"), `${JSON.stringify({
      schema_version: 1,
      turn_id: "turn-recover-commit",
      response_id: "resp_recover",
      status: "committing",
      item_count: 0,
      next_sequence: 0,
      canonical_line_sha256: hash,
    })}\n`);
    await writeFile(join(turnDir, "items.jsonl"), "");
    await writeFile(join(turnDir, "items.index.jsonl"), "");
    await writeFile(join(turnDir, "final.jsonl"), finalLine);

    const store = await SessionResponseStore.open(sessionDir);
    const report = await store.recover();
    assert.equal(report.committed_turns, 1);
    assert.equal(report.removed_turns, 1);
    assert.deepEqual(report.errors, []);
    assert.equal(await readFile(join(sessionDir, "messages.jsonl"), "utf8"), finalLine);
    assert.deepEqual(await readdir(join(sessionDir, "staging")), []);
    await store.close();
  } finally {
    await removeSession(sessionDir);
  }
});

test("rejects incomplete output sets and oversized items without committing", async () => {
  const sessionDir = await makeSession();
  try {
    const store = await SessionResponseStore.open(sessionDir, { maxItemBytes: 64 });
    const turn = await store.beginTurn("turn-validation");
    await assert.rejects(
      () =>
        turn.stageOutputItem({
          type: "response.output_item.done",
          output_index: 0,
          item: { type: "message", content: [{ type: "output_text", text: "x".repeat(128) }] },
        }),
      (error: unknown) => error instanceof PersistenceError,
    );

    await turn.stageOutputItem({
      type: "response.output_item.done",
      output_index: 0,
      item: { type: "message", role: "assistant", content: [] },
    });
    await assert.rejects(
      () => turn.commit(completedEvent([{ type: "message" }, { type: "message" }], "resp_bad")),
      (error: unknown) => error instanceof PersistenceError,
    );
    assert.equal((await readFile(join(sessionDir, "messages.jsonl"), "utf8")), "");
    await turn.close();
    await store.close();
  } finally {
    await removeSession(sessionDir);
  }
});

test("adapts a Responses-compatible stream into staging and commit", async () => {
  const sessionDir = await makeSession();
  try {
    async function* events(): AsyncGenerator<unknown> {
      yield {
        type: "response.output_item.done",
        output_index: 0,
        item: { type: "message", role: "assistant", content: [] },
      };
      yield completedEvent([{ type: "message", role: "assistant", content: [] }], "resp_adapter");
    }

    const store = await SessionResponseStore.open(sessionDir);
    const result = await persistResponseStream(store, events(), { turnId: "turn-adapter" });
    assert.equal(result.already_present, false);
    const records = [];
    for await (const record of store.readCompletedResponses()) {
      records.push(record);
    }
    assert.equal(records.length, 1);
    assert.equal(records[0]!.response.id, "resp_adapter");
    await store.close();
  } finally {
    await removeSession(sessionDir);
  }
});

test("loads model and session settings with normalized paths and hashes", async () => {
  const sessionDir = await makeSession();
  try {
    await writeFile(
      join(sessionDir, "model.json"),
      JSON.stringify({
        request: { max_output_tokens: 1024 },
        model: "inclusionai/ling-3.0-flash-fin:free",
        base_url: "https://openrouter.ai/api/v1/",
        provider: "openrouter",
        api_key_env: "OPENROUTER_API_KEY",
      }),
    );
    await writeFile(
      join(sessionDir, "setting.json"),
      JSON.stringify({
        system_prompt: "Be concise.",
        cwd: sessionDir,
        session_name: "config-test",
      }),
    );

    const config = await new SessionConfigLoader(sessionDir).load();
    assert.equal(config.model.schema_version, 1);
    assert.equal(config.model.base_url, "https://openrouter.ai/api/v1");
    assert.equal(config.settings.cwd, sessionDir);
    assert.equal(config.settings.system_prompt, "Be concise.");
    assert.deepEqual(config.settings.http_debug, {
      enabled: false,
      include_request_headers: false,
      include_request_body: false,
      include_response_headers: true,
      include_response_body: false,
      max_body_bytes: 65536,
    });
    assert.match(config.model_sha256, /^[a-f0-9]{64}$/);
    assert.match(config.settings_sha256, /^[a-f0-9]{64}$/);
    assert.equal(resolveApiKey(config.model, { OPENROUTER_API_KEY: "secret" }), "secret");
    assert.equal(sha256Json({ b: 2, a: 1 }), sha256Json({ a: 1, b: 2 }));
  } finally {
    await removeSession(sessionDir);
  }
});

test("resolves configurable session storage paths relative to setting.json", async () => {
  const sessionDir = await makeSession();
  const externalDir = await makeSession();
  const modelPath = join(externalDir, "provider-model.json");
  const stagingPath = join(externalDir, "response-staging");
  const messagesPath = join(sessionDir, "data", "transcript.jsonl");
  const httpDebugPath = join(sessionDir, "diagnostics", "http");
  try {
    await writeFile(modelPath, JSON.stringify({model: "configured-model"}));
    await writeFile(
      join(sessionDir, "setting.json"),
      JSON.stringify({
        cwd: sessionDir,
        paths: {
          messages: "data/transcript.jsonl",
          model: modelPath,
          staging: stagingPath,
          http_debug: "diagnostics/http",
        },
      }),
    );

    const loader = new SessionConfigLoader(sessionDir);
    const settings = await loader.loadSettings();
    assert.equal(settings.paths.messages, messagesPath);
    assert.equal(settings.paths.model, modelPath);
    assert.equal(settings.paths.staging, stagingPath);
    assert.equal(settings.paths.http_debug, httpDebugPath);

    const config = await loader.load();
    assert.equal(config.model.model, "configured-model");
    assert.equal(config.paths.messages_path, messagesPath);
    assert.equal(config.paths.model_path, modelPath);
    assert.equal(config.paths.staging_dir, stagingPath);
    assert.equal(config.paths.http_debug_dir, httpDebugPath);

    const session = await AgentSession.open(sessionDir);
    assert.deepEqual(await readdir(stagingPath), []);
    await session.close();
    await assert.rejects(() => readdir(join(sessionDir, "staging")));

    const store = await SessionResponseStore.open(sessionDir, {
      messagesPath,
      stagingDir: stagingPath,
    });
    await store.appendInputItem({
      type: "message",
      role: "user",
      content: [{type: "input_text", text: "configured storage"}],
    });
    assert.match(await readFile(messagesPath, "utf8"), /configured storage/);
    await store.close();
  } finally {
    await removeSession(sessionDir);
    await removeSession(externalDir);
  }
});

test("defaults empty settings and rejects missing or unsafe model configuration", async () => {
  const sessionDir = await makeSession();
  try {
    await writeFile(join(sessionDir, "model.json"), JSON.stringify({ model: "gpt-test" }));
    await writeFile(join(sessionDir, "setting.json"), "");
    const loader = new SessionConfigLoader(sessionDir);
    const settings = await loader.loadSettings();
    assert.equal(settings.session_name, sessionDir.split("/").pop());
    assert.equal(settings.cwd, process.cwd());
    assert.equal(settings.system_prompt, "");
    assert.equal(settings.http_debug.enabled, false);
    assert.equal(settings.http_debug.include_response_headers, true);
    assert.equal(settings.http_debug.max_body_bytes, 65536);

    await writeFile(
      join(sessionDir, "model.json"),
      JSON.stringify({ model: "gpt-test", request: { stream: true } }),
    );
    await assert.rejects(() => loader.loadModel(), (error: unknown) => error instanceof ConfigError);
  } finally {
    await removeSession(sessionDir);
  }

  const missingModelDir = await makeSession();
  try {
    await assert.rejects(
      () => new SessionConfigLoader(missingModelDir).loadModel(),
      (error: unknown) => error instanceof ConfigError,
    );
  } finally {
    await removeSession(missingModelDir);
  }
});

test("resolves configured paths from the global JAD root and rejects relative cwd", async () => {
  const globalJadRoot = await makeSession();
  const sessionDir = join(globalJadRoot, "sessions", "session1");
  try {
    await mkdir(sessionDir, {recursive: true});
    await mkdir(join(globalJadRoot, "tools"), {recursive: true});
    await writeFile(join(globalJadRoot, "tools", "read.json"), "{}");
    await writeFile(join(sessionDir, "model.json"), JSON.stringify({model: "gpt-test"}));
    await writeFile(
      join(sessionDir, "setting.json"),
      JSON.stringify({
        cwd: sessionDir,
        tool_definition_paths: ["tools/read.json"],
        tools: {
          native: {
            read: {
              paths: [{action: "allow", path: "tools"}],
            },
          },
        },
      }),
    );

    const loader = new SessionConfigLoader(sessionDir, {globalJadRoot});
    const settings = await loader.loadSettings();
    assert.equal(settings.cwd, sessionDir);
    assert.deepEqual(settings.tool_definition_paths, [
      join(globalJadRoot, "tools", "read.json"),
    ]);
    assert.deepEqual(settings.tools.native.read?.paths, [
      {action: "allow", path: join(globalJadRoot, "tools")},
    ]);
    assert.equal(loader.paths.global_jad_root, globalJadRoot);

    await writeFile(
      join(sessionDir, "setting.json"),
      JSON.stringify({cwd: "relative/workspace"}),
    );
    await assert.rejects(
      () => loader.loadSettings(),
      (error: unknown) =>
        error instanceof ConfigError && /cwd must be an absolute path/.test(error.message),
    );
  } finally {
    await removeSession(globalJadRoot);
  }
});

test("normalizes and validates HTTP debug settings", async () => {
  const sessionDir = await makeSession();
  try {
    await writeFile(join(sessionDir, "model.json"), JSON.stringify({ model: "gpt-test" }));
    await writeFile(
      join(sessionDir, "setting.json"),
      JSON.stringify({
        cwd: sessionDir,
        http_debug: {
          enabled: true,
          include_request_headers: true,
          include_request_body: true,
          include_response_body: true,
          max_body_bytes: 1024,
        },
      }),
    );
    const settings = await new SessionConfigLoader(sessionDir).loadSettings();
    assert.equal(settings.http_debug.enabled, true);
    assert.equal(settings.http_debug.include_request_headers, true);
    assert.equal(settings.http_debug.include_request_body, true);
    assert.equal(settings.http_debug.include_response_headers, true);
    assert.equal(settings.http_debug.include_response_body, true);
    assert.equal(settings.http_debug.max_body_bytes, 1024);

    await writeFile(
      join(sessionDir, "setting.json"),
      JSON.stringify({ http_debug: { max_body_bytes: 0 } }),
    );
    await assert.rejects(
      () => new SessionConfigLoader(sessionDir).loadSettings(),
      (error: unknown) => error instanceof ConfigError,
    );
  } finally {
    await removeSession(sessionDir);
  }
});

test("runs consecutive session turns using config and reconstructed history", async () => {
  const sessionDir = await makeSession();
  try {
    await writeFile(
      join(sessionDir, "model.json"),
      JSON.stringify({ model: "gpt-test", request: { max_output_tokens: 64 } }),
    );
    await writeFile(
      join(sessionDir, "setting.json"),
      JSON.stringify({ cwd: sessionDir, system_prompt: "Be concise." }),
    );

    const requests: unknown[] = [];
    let responseNumber = 0;
    const fakeClient = {
      responses: {
        create: async (request: unknown) => {
          requests.push(request);
          responseNumber += 1;
          const item = {
            type: "message",
            role: "assistant",
            content: [{ type: "output_text", text: `reply-${responseNumber}` }],
          };
          return (async function* () {
            yield { type: "response.output_item.done", output_index: 0, item };
            yield completedEvent([item], `resp_runner_${responseNumber}`);
          })();
        },
      },
    } as unknown as OpenAI;

    const session = await AgentSession.open(sessionDir, { client: fakeClient });
    const first = await session.run("first", { turnId: "turn-first" });
    const second = await session.run("second", { turnId: "turn-second" });
    assert.equal(first.response.event.response.id, "resp_runner_1");
    assert.equal(second.response.event.response.id, "resp_runner_2");

    const firstRequest = requests[0] as Record<string, unknown>;
    const secondRequest = requests[1] as Record<string, unknown>;
    assert.equal(firstRequest.model, "gpt-test");
    assert.equal(firstRequest.stream, true);
    assert.equal(firstRequest.instructions, "Be concise.");
    assert.equal((firstRequest.input as unknown[]).length, 1);
    assert.equal((secondRequest.input as unknown[]).length, 3);

    const sessionMessages = (await readFile(join(sessionDir, "messages.jsonl"), "utf8"))
      .trimEnd()
      .split("\n");
    assert.equal(sessionMessages.length, 4);
    await session.close();
  } finally {
    await removeSession(sessionDir);
  }
});
