import { createHash } from "node:crypto";
import { appendFile, mkdir, readFile, readdir, rm, writeFile } from "node:fs/promises";
import { mkdtemp } from "node:fs/promises";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
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
import {TaskStore} from "../src/tools/native/task-store.js";
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

function usageEvent(
  output: Array<Record<string, unknown>>,
  responseId: string,
  usage: Record<string, unknown>,
  sessionTurn: number,
  sessionStep: number,
): ResponseCompletedEvent {
  const event = completedEvent(output, responseId);
  return {
    ...event,
    session_turn: sessionTurn,
    session_step: sessionStep,
    response: {...event.response, usage},
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
    assert.deepEqual(stagingEntries, ["usage-cache.json"]);
    await store.close();
  } finally {
    await removeSession(sessionDir);
  }
});

test("keeps current and cumulative usage in an incremental cache", async () => {
  const sessionDir = await makeSession();
  const usageCachePath = join(sessionDir, "staging", "usage-cache.json");
  const expected = {
    completed_responses: 3,
    responses_with_usage: 3,
    turn_count: 2,
    history_partial: false,
    input_tokens: 152,
    output_tokens: 33,
    total_tokens: 185,
    cached_input_tokens: 10,
    current: {
      input_tokens: 12,
      output_tokens: 3,
      total_tokens: 15,
      turn: 2,
      step: 1,
      response_id: "usage-three",
    },
  };
  try {
    const store = await SessionResponseStore.open(sessionDir);
    const user = (text: string) => ({
      type: "message",
      role: "user",
      content: [{type: "input_text", text}],
    });
    await store.appendInputItem(user("first turn"));

    const first = await store.beginTurn("usage-first");
    await first.commit(usageEvent([], "usage-one", {
      input_tokens: 100,
      output_tokens: 25,
      total_tokens: 125,
      input_tokens_details: {cached_tokens: 10},
    }, 1, 1));
    const second = await store.beginTurn("usage-second");
    await second.commit(usageEvent([], "usage-two", {
      input_tokens: 40,
      output_tokens: 5,
    }, 1, 2));

    await store.appendInputItems([user("second turn part one"), user("second turn part two")]);
    const third = await store.beginTurn("usage-third");
    await third.commit(usageEvent([], "usage-three", {
      input_tokens: 12,
      output_tokens: 3,
    }, 2, 1));

    assert.deepEqual(await store.readUsageSummary(), expected);
    const cache = JSON.parse(await readFile(usageCachePath, "utf8")) as {
      transcript: {offset: number};
      summary: unknown;
    };
    assert.deepEqual(cache.summary, expected);
    assert.equal(cache.transcript.offset, (await readFile(store.messagesPath)).byteLength);
    await store.close();

    // The cache-only reader must not need to open or walk the transcript.
    await rm(store.messagesPath);
    assert.deepEqual(await SessionResponseStore.readCachedUsageSummary(usageCachePath), expected);
  } finally {
    await removeSession(sessionDir);
  }
});

test("recovers an uncheckpointed usage transcript append without double-counting", async () => {
  const sessionDir = await makeSession();
  const user = {type: "message", role: "user", content: [{type: "input_text", text: "first"}]};
  try {
    const store = await SessionResponseStore.open(sessionDir);
    await store.appendInputItem(user);
    const first = await store.beginTurn("usage-recovery-first");
    await first.commit(usageEvent([], "usage-recovery-one", {
      input_tokens: 20,
      output_tokens: 8,
    }, 1, 1));
    const checkpoint = JSON.parse(await readFile(store.usageCachePath, "utf8")) as {
      transcript: {offset: number};
    };

    const secondUser = {type: "message", role: "user", content: [{type: "input_text", text: "second"}]};
    const secondEvent = usageEvent([], "usage-recovery-two", {
      input_tokens: 15,
      output_tokens: 5,
    }, 2, 1);
    await appendFile(
      store.messagesPath,
      `${JSON.stringify(secondUser)}\n${JSON.stringify(secondEvent)}\n`,
    );
    const recovered = await store.readUsageSummary();
    assert.equal(recovered.completed_responses, 2);
    assert.equal(recovered.responses_with_usage, 2);
    assert.equal(recovered.turn_count, 2);
    assert.equal(recovered.input_tokens, 35);
    assert.equal(recovered.output_tokens, 13);
    assert.equal(recovered.total_tokens, 48);
    assert.deepEqual(recovered.current, {
      input_tokens: 15,
      output_tokens: 5,
      total_tokens: 20,
      turn: 2,
      step: 1,
      response_id: "usage-recovery-two",
    });
    assert.ok(checkpoint.transcript.offset < (await readFile(store.messagesPath)).byteLength);
    assert.deepEqual(await store.readUsageSummary(), recovered);
    await store.close();

    const reopened = await SessionResponseStore.open(sessionDir);
    assert.deepEqual(await reopened.readUsageSummary(), recovered);
    await reopened.close();
  } finally {
    await removeSession(sessionDir);
  }
});

test("refreshes uncheckpointed usage before compaction moves the transcript checkpoint", async () => {
  const sessionDir = await makeSession();
  try {
    const store = await SessionResponseStore.open(sessionDir);
    const user = {type: "message", role: "user", content: [{type: "input_text", text: "compact this"}]};
    const assistant = {type: "message", role: "assistant", content: [{type: "output_text", text: "answer"}]};
    await store.appendInputItem(user);
    const first = await store.beginTurn("usage-compact-first");
    await first.stageOutputItem({type: "response.output_item.done", output_index: 0, item: assistant});
    await first.commit(usageEvent([assistant], "usage-compact-one", {
      input_tokens: 30,
      output_tokens: 8,
    }, 1, 1));
    await store.appendInputItem({
      type: "function_call_output",
      call_id: "large-compact-output",
      output: "tool data ".repeat(500),
    });

    const uncheckpointed = usageEvent([], "usage-compact-two", {
      input_tokens: 12,
      output_tokens: 4,
    }, 1, 2);
    await appendFile(store.messagesPath, `${JSON.stringify(uncheckpointed)}\n`);
    const compacted = await store.compactHistory({
      turnType: "full",
      keepTurns: 0,
      keepToolPairs: 0,
      backupMode: "replace",
    });
    assert.equal(compacted.compacted, true);
    assert.equal(await readFile(store.messagesPath, "utf8"), "");
    assert.deepEqual(await store.readUsageSummary(), {
      completed_responses: 2,
      responses_with_usage: 2,
      turn_count: 1,
      history_partial: false,
      input_tokens: 42,
      output_tokens: 12,
      total_tokens: 54,
      current: {
        input_tokens: 12,
        output_tokens: 4,
        total_tokens: 16,
        turn: 1,
        step: 2,
        response_id: "usage-compact-two",
      },
    });
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

test("writes normalized history to its configured cache path", async () => {
  const sessionDir = await makeSession();
  const cachePath = join(sessionDir, "separate-cache", "history.json");
  try {
    const store = await SessionResponseStore.open(sessionDir, {historyCachePath: "separate-cache/history.json"});
    assert.equal(store.historyCachePath, cachePath);
    const item = {type: "message", role: "user", content: [{type: "input_text", text: "cache me"}]};
    await store.appendInputItem(item);
    const history = [];
    for await (const input of store.readInputItems()) history.push(input);
    assert.deepEqual(history, [item]);
    const cache = JSON.parse(await readFile(cachePath, "utf8")) as {items: unknown[]};
    assert.deepEqual(cache.items, [item]);
    await assert.rejects(() => readFile(join(sessionDir, "staging/history-cache.json"), "utf8"), {code: "ENOENT"});
    await store.close();
  } finally {
    await removeSession(sessionDir);
  }
});

test("persists normalized request history and refreshes only appended transcript records", async () => {
  const sessionDir = await makeSession();
  try {
    const store = await SessionResponseStore.open(sessionDir);
    const first = {
      type: "message",
      role: "user",
      content: [{type: "input_text", text: "first"}],
    };
    const second = {
      type: "message",
      role: "user",
      content: [{type: "input_text", text: "second"}],
    };
    await store.appendInputItem(first);
    const firstHistory = [];
    for await (const item of store.readInputItems()) firstHistory.push(item);
    assert.deepEqual(firstHistory, [first]);

    const firstCache = JSON.parse(
      await readFile(join(sessionDir, "staging", "history-cache.json"), "utf8"),
    ) as {schema_version: number; transcript: {offset: number}; items: unknown[]};
    assert.equal(firstCache.schema_version, 1);
    assert.equal(firstCache.transcript.offset, (await readFile(join(sessionDir, "messages.jsonl"))).length);
    assert.deepEqual(firstCache.items, [first]);

    await store.appendInputItem(second);
    const secondHistory = [];
    for await (const item of store.readInputItems()) secondHistory.push(item);
    assert.deepEqual(secondHistory, [first, second]);
    const secondCache = JSON.parse(
      await readFile(join(sessionDir, "staging", "history-cache.json"), "utf8"),
    ) as {transcript: {offset: number}; items: unknown[]};
    assert.equal(secondCache.transcript.offset, (await readFile(join(sessionDir, "messages.jsonl"))).length);
    assert.deepEqual(secondCache.items, [first, second]);
    await store.close();
  } finally {
    await removeSession(sessionDir);
  }
});

test("rebuilds cached history when the transcript prefix changes", async () => {
  const sessionDir = await makeSession();
  try {
    const store = await SessionResponseStore.open(sessionDir);
    const original = {
      type: "message",
      role: "user",
      content: [{type: "input_text", text: "original"}],
    };
    const replacement = {
      type: "message",
      role: "user",
      content: [{type: "input_text", text: "a substantially longer replacement"}],
    };
    await store.appendInputItem(original);
    for await (const _item of store.readInputItems()) {
      // Populate the staged cache before replacing the authoritative transcript.
    }
    await writeFile(join(sessionDir, "messages.jsonl"), `${JSON.stringify(replacement)}\n`);
    const history = [];
    for await (const item of store.readInputItems()) history.push(item);
    assert.deepEqual(history, [replacement]);
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
    assert.deepEqual(await readdir(join(sessionDir, "staging")), ["usage-cache.json"]);
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

test("commits completed output items when a provider response is incomplete", async () => {
  const sessionDir = await makeSession();
  try {
    const functionCall = {
      type: "function_call",
      call_id: "call_incomplete",
      name: "read",
      arguments: JSON.stringify({path: "sample.txt"}),
    };
    async function* events(): AsyncGenerator<unknown> {
      yield {
        type: "response.output_item.done",
        output_index: 0,
        item: functionCall,
      };
      yield {
        type: "response.incomplete",
        response: {
          id: "resp_incomplete",
          object: "response",
          status: "incomplete",
          incomplete_details: {reason: "max_output_tokens"},
          output: [functionCall],
        },
      };
    }

    const store = await SessionResponseStore.open(sessionDir);
    const result = await persistResponseStream(store, events(), {turnId: "turn-incomplete"});
    assert.equal(result.event.type, "response.completed");
    assert.equal(result.event.response.status, "incomplete");
    assert.deepEqual(result.event.response.incomplete_details, {reason: "max_output_tokens"});

    const history = [];
    for await (const item of store.readInputItems()) {
      history.push(item);
    }
    assert.deepEqual(history, [functionCall]);
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
    await writeFile(join(sessionDir, "base.md"), "Be concise.");
    const absolutePrompt = join(sessionDir, "product.md");
    await writeFile(absolutePrompt, "Use tools.");
    await writeFile(
      join(sessionDir, "setting.json"),
      JSON.stringify({
        prompt_documents: ["base.md", absolutePrompt],
        prompt_role: "developer",
        cwd: sessionDir,
        session_name: "config-test",
        mcp_servers: [
          {id: "docs", config_path: "mcp/docs.json", enabled: true, deny_tools: ["delete"]},
          {id: "optional", config_path: absolutePrompt},
        ],
      }),
    );

    const config = await new SessionConfigLoader(sessionDir).load();
    assert.equal(config.model.schema_version, 1);
    assert.equal(config.model.base_url, "https://openrouter.ai/api/v1");
    assert.equal(config.settings.cwd, sessionDir);
    assert.deepEqual(config.settings.prompt_documents, [join(sessionDir, "base.md"), absolutePrompt]);
    assert.equal(config.settings.prompt_role, "developer");
    assert.equal(config.settings.prompt_text, "Be concise.\n\nUse tools.");
    assert.deepEqual(config.settings.mcp_servers, [
      {id: "docs", config_path: join(sessionDir, "mcp/docs.json"), enabled: true, deny_tools: ["delete"]},
      {id: "optional", config_path: absolutePrompt, enabled: false, deny_tools: []},
    ]);
    assert.equal(config.settings.max_tool_rounds, 8);
    assert.deepEqual(config.settings.compaction, {
      auto_enabled: true,
      trigger_tokens: 200000,
      keepbatch: {tools: 0, turn: {type: "assistant_final", keep: 1}},
      backup_mode: "replace",
    });
    assert.deepEqual(config.settings.persistence, {
      durability: "turn",
      max_item_bytes: 10 * 1024 * 1024,
    });
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

test("validates configurable tool-round limits", async () => {
  const sessionDir = await makeSession();
  try {
    await writeFile(join(sessionDir, "model.json"), JSON.stringify({model: "gpt-test"}));
    await writeFile(
      join(sessionDir, "setting.json"),
      JSON.stringify({cwd: sessionDir, max_tool_rounds: 50000}),
    );
    assert.equal((await new SessionConfigLoader(sessionDir).loadSettings()).max_tool_rounds, 50000);

    for (const value of [-1, 50001, 1.5, "12"]) {
      await writeFile(
        join(sessionDir, "setting.json"),
        JSON.stringify({cwd: sessionDir, max_tool_rounds: value}),
      );
      await assert.rejects(
        () => new SessionConfigLoader(sessionDir).loadSettings(),
        (error: unknown) => error instanceof ConfigError && /max_tool_rounds/.test(error.message),
      );
    }
  } finally {
    await removeSession(sessionDir);
  }
});

test("validates MCP server references and exact deny lists", async () => {
  const sessionDir = await makeSession();
  try {
    for (const mcp_servers of [
      [{id: "bad id", config_path: "server.json"}],
      [{id: "duplicate", config_path: "one.json"}, {id: "duplicate", config_path: "two.json"}],
      [{id: "server", config_path: "server.json", enabled: "yes"}],
      [{id: "server", config_path: "server.json", deny_tools: ["hidden", "hidden"]}],
    ]) {
      await writeFile(join(sessionDir, "setting.json"), JSON.stringify({cwd: sessionDir, mcp_servers}));
      await assert.rejects(() => new SessionConfigLoader(sessionDir).loadSettings(), /mcp_servers/);
    }
  } finally {
    await removeSession(sessionDir);
  }
});

test("normalizes and validates compaction settings", async () => {
  const sessionDir = await makeSession();
  try {
    await writeFile(join(sessionDir, "setting.json"), JSON.stringify({
      compaction: {auto_enabled: false, trigger_tokens: 1234, backup_mode: "append", keepbatch: {tools: 2, turn: {type: "full", keep: 3}}},
    }));
    const settings = await new SessionConfigLoader(sessionDir).loadSettings();
    assert.deepEqual(settings.compaction, {
      auto_enabled: false,
      trigger_tokens: 1234,
      backup_mode: "append",
      keepbatch: {tools: 2, turn: {type: "full", keep: 3}},
    });
    for (const compaction of [
      {trigger_tokens: 0},
      {keepbatch: {tools: -1}},
      {keepbatch: {turn: {type: "summary"}}},
      {backup_mode: "archive"},
    ]) {
      await writeFile(join(sessionDir, "setting.json"), JSON.stringify({compaction}));
      await assert.rejects(() => new SessionConfigLoader(sessionDir).loadSettings(), /compaction/);
    }
    await writeFile(join(sessionDir, "setting.json"), JSON.stringify({paths: {messages: "same", usage_cache: "same"}}));
    await assert.rejects(() => new SessionConfigLoader(sessionDir).loadSettings(), /must be different files/);
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
  const usageCachePath = join(sessionDir, "data", "usage-summary.json");
  const httpDebugPath = join(sessionDir, "diagnostics", "http");
  try {
    await writeFile(modelPath, JSON.stringify({model: "configured-model"}));
    await writeFile(
      join(sessionDir, "setting.json"),
      JSON.stringify({
        cwd: sessionDir,
        paths: {
          messages: "data/transcript.jsonl",
          messages_backup: "data/transcript.backup.jsonl",
          compaction_cache: "data/compact.json",
          history_cache: "data/input-history.json",
          usage_cache: "data/usage-summary.json",
          model: modelPath,
          staging: stagingPath,
          http_debug: "diagnostics/http",
        },
      }),
    );

    const loader = new SessionConfigLoader(sessionDir);
    const settings = await loader.loadSettings();
    assert.equal(settings.paths.messages, messagesPath);
    assert.equal(settings.paths.messages_backup, join(sessionDir, "data/transcript.backup.jsonl"));
    assert.equal(settings.paths.compaction_cache, join(sessionDir, "data/compact.json"));
    assert.equal(settings.paths.history_cache, join(sessionDir, "data/input-history.json"));
    assert.equal(settings.paths.usage_cache, usageCachePath);
    assert.equal(settings.paths.model, modelPath);
    assert.equal(settings.paths.staging, stagingPath);
    assert.equal(settings.paths.http_debug, httpDebugPath);

    const config = await loader.load();
    assert.equal(config.model.model, "configured-model");
    assert.equal(config.paths.messages_path, messagesPath);
    assert.equal(config.paths.messages_backup_path, join(sessionDir, "data/transcript.backup.jsonl"));
    assert.equal(config.paths.compaction_cache_path, join(sessionDir, "data/compact.json"));
    assert.equal(config.paths.history_cache_path, join(sessionDir, "data/input-history.json"));
    assert.equal(config.paths.usage_cache_path, usageCachePath);
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
      usageCachePath,
    });
    assert.equal(store.usageCachePath, usageCachePath);
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

test("selects named path profiles, merges overrides, and pins the choice to the loader", async () => {
  const sessionDir = await makeSession();
  const profileModelPath = join(sessionDir, "profiles/blue/model.json");
  const profileStagingPath = join(sessionDir, "profiles/blue/staging");
  const profileUsageCachePath = join(sessionDir, "profiles/blue/custom-usage-cache.json");
  try {
    await mkdir(dirname(profileModelPath), {recursive: true});
    await writeFile(profileModelPath, JSON.stringify({model: "profile-blue-model"}));
    await writeFile(join(sessionDir, "setting.json"), JSON.stringify({
      cwd: sessionDir,
      paths: {
        messages: "base/messages.jsonl",
        messages_backup: "base/messages.backup.jsonl",
        compaction_cache: "base/staging/compaction-cache.json",
        statecontext: "base/staging/statecontext.json",
        model: "base/model.json",
        staging: "base/staging",
        http_debug: "base/debug/http",
        active_profile: "blue",
        profiles: {
          blue: {
            messages: "profiles/blue/messages.jsonl",
            messages_backup: "profiles/blue/messages.backup.jsonl",
            compaction_cache: "profiles/blue/staging/compaction-cache.json",
            statecontext: "profiles/blue/staging/statecontext.json",
            usage_cache: profileUsageCachePath,
            model: "profiles/blue/model.json",
            staging: "profiles/blue/staging",
            http_debug: "profiles/blue/debug/http",
          },
          green: {model: "profiles/green/model.json"},
        },
      },
    }));

    const loader = new SessionConfigLoader(sessionDir);
    const settings = await loader.loadSettings();
    assert.equal(settings.paths.active_profile, "blue");
    assert.equal(settings.paths.messages, join(sessionDir, "profiles/blue/messages.jsonl"));
    assert.equal(settings.paths.history_cache, join(profileStagingPath, "history-cache.json"));
    assert.equal(settings.paths.usage_cache, profileUsageCachePath);
    assert.equal(settings.paths.http_debug, join(sessionDir, "profiles/blue/debug/http"));
    assert.equal((await loader.load()).model.model, "profile-blue-model");
    assert.equal(loader.paths.statecontext_path, join(profileStagingPath, "statecontext.json"));
    assert.equal(loader.paths.usage_cache_path, profileUsageCachePath);
    assert.equal(loader.paths.active_profile, "blue");

    await writeFile(join(sessionDir, "setting.json"), JSON.stringify({
      cwd: sessionDir,
      paths: {
        active_profile: "green",
        profiles: {
          green: {model: "profiles/green/model.json"},
        },
      },
    }));
    await assert.rejects(() => loader.loadSettings(), /paths cannot change while this config loader is active/);
  } finally {
    await removeSession(sessionDir);
  }
});

test("rejects malformed and conflicting path profiles", async () => {
  const sessionDir = await makeSession();
  try {
    const loader = new SessionConfigLoader(sessionDir);
    await writeFile(join(sessionDir, "setting.json"), JSON.stringify({paths: {profiles: {profile1: {}}}}));
    await assert.rejects(() => loader.loadSettings(), /active_profile/);

    await writeFile(join(sessionDir, "setting.json"), JSON.stringify({paths: {active_profile: "missing", profiles: {profile1: {}}}}));
    await assert.rejects(() => loader.loadSettings(), /not defined in paths\.profiles/);

    await writeFile(join(sessionDir, "setting.json"), JSON.stringify({
      paths: {active_profile: "profile1", profiles: {profile1: {messages: "same.jsonl", messages_backup: "same.jsonl"}}},
    }));
    await assert.rejects(() => loader.loadSettings(), /must be different files/);
  } finally {
    await removeSession(sessionDir);
  }
});

test("compacts to the final assistant response, backs up transcript, and rebuilds request history", async () => {
  const sessionDir = await makeSession();
  try {
    const store = await SessionResponseStore.open(sessionDir, {durability: "strict"});
    const user = {type: "message", role: "user", content: [{type: "input_text", text: "question"}]};
    const reasoning = {type: "reasoning", summary: [{type: "summary_text", text: "private reasoning"}]};
    const call = {type: "function_call", call_id: "call-large", name: "search", arguments: "{}"};
    const partial = {type: "message", role: "assistant", content: [{type: "output_text", text: "Searching"}]};
    const output = {type: "function_call_output", call_id: "call-large", output: "x".repeat(10000)};
    const final = {type: "message", role: "assistant", content: [{type: "output_text", text: "Answer"}]};
    await store.appendInputItem(user);
    const toolTurn = await store.beginTurn("compact-tool-call");
    for (const [output_index, item] of [reasoning, partial, call].entries()) {
      await toolTurn.stageOutputItem({type: "response.output_item.done", output_index, item});
    }
    await toolTurn.commit(usageEvent([reasoning, partial, call], "resp-tool-call", {
      input_tokens: 100,
      output_tokens: 20,
      input_tokens_details: {cached_tokens: 7},
    }, 1, 1));
    await store.appendInputItem(output);
    const finalTurn = await store.beginTurn("compact-final");
    await finalTurn.stageOutputItem({type: "response.output_item.done", output_index: 0, item: final});
    await finalTurn.commit(usageEvent([final], "resp-final", {
      input_tokens: 30,
      output_tokens: 5,
    }, 1, 2));

    const beforeUsage = await store.readUsageSummary();
    const before = await readFile(store.messagesPath, "utf8");
    const result = await store.compactHistory({turnType: "assistant_final", keepTurns: 1, keepToolPairs: 0, backupMode: "replace"});
    assert.equal(result.compacted, true);
    assert.equal((await readFile(store.messagesPath, "utf8")), "");
    assert.deepEqual(await store.readUsageSummary(), beforeUsage);
    assert.equal(await readFile(store.messagesBackupPath, "utf8"), before);
    const history = [];
    for await (const item of store.readInputItems()) history.push(item);
    assert.deepEqual(history, [user, final]);
    await store.appendInputItem({type: "message", role: "user", content: [{type: "input_text", text: "follow-up"}]});
    const combined = [];
    for await (const item of store.readInputItems()) combined.push(item);
    const followUp = {type: "message", role: "user", content: [{type: "input_text", text: "follow-up"}]};
    assert.deepEqual(combined, [user, final, followUp]);
    const afterFollowUp = await store.readUsageSummary();
    assert.equal(afterFollowUp.turn_count, 2);
    assert.equal(afterFollowUp.completed_responses, 2);
    const appended = await store.compactHistory({turnType: "assistant_final", keepTurns: 1, keepToolPairs: 0, backupMode: "append"});
    assert.equal(appended.compacted, true);
    assert.equal(await readFile(store.messagesBackupPath, "utf8"), `${before}${JSON.stringify(followUp)}\n`);
    assert.deepEqual(await store.readUsageSummary(), afterFollowUp);
    await assert.rejects(() => readFile(store.historyCachePath, "utf8"), {code: "ENOENT"});
    await store.close();

    await rm(store.usageCachePath);
    const rebuilt = await SessionResponseStore.open(sessionDir);
    const rebuiltUsage = await rebuilt.readUsageSummary();
    assert.equal(rebuiltUsage.completed_responses, 2);
    assert.equal(rebuiltUsage.responses_with_usage, 2);
    assert.equal(rebuiltUsage.turn_count, 2);
    assert.equal(rebuiltUsage.input_tokens, 130);
    assert.equal(rebuiltUsage.output_tokens, 25);
    assert.equal(rebuiltUsage.total_tokens, 155);
    assert.equal(rebuiltUsage.cached_input_tokens, 7);
    assert.equal(rebuiltUsage.current?.turn, 1);
    assert.equal(rebuiltUsage.current?.step, 2);
    assert.equal(rebuiltUsage.history_partial, true);
    await rebuilt.close();
  } finally {
    await removeSession(sessionDir);
  }
});

test("compacts at a tool-call boundary while leaving that response active for its result", async () => {
  const sessionDir = await makeSession();
  try {
    const store = await SessionResponseStore.open(sessionDir);
    const oldUser = {type: "message", role: "user", content: [{type: "input_text", text: "old turn"}]};
    const oldCall = {type: "function_call", call_id: "old-call", name: "lookup", arguments: "{}"};
    const oldOutput = {type: "function_call_output", call_id: "old-call", output: "large tool data ".repeat(500)};
    const oldFinal = {type: "message", role: "assistant", content: [{type: "output_text", text: "old answer"}]};
    const currentUser = {type: "message", role: "user", content: [{type: "input_text", text: "new turn"}]};
    const currentCall = {type: "function_call", call_id: "current-call", name: "lookup", arguments: "{}"};
    await store.appendInputItem(oldUser);
    const oldToolTurn = await store.beginTurn("old-tool-response");
    await oldToolTurn.stageOutputItem({type: "response.output_item.done", output_index: 0, item: oldCall});
    await oldToolTurn.commit(completedEvent([oldCall], "resp-old-call"));
    await store.appendInputItem(oldOutput);
    const oldFinalTurn = await store.beginTurn("old-final-response");
    await oldFinalTurn.stageOutputItem({type: "response.output_item.done", output_index: 0, item: oldFinal});
    await oldFinalTurn.commit(completedEvent([oldFinal], "resp-old-final"));
    await store.appendInputItem(currentUser);
    const currentToolTurn = await store.beginTurn("current-tool-response");
    await currentToolTurn.stageOutputItem({type: "response.output_item.done", output_index: 0, item: currentCall});
    await currentToolTurn.commit(completedEvent([currentCall], "resp-current-call"));

    const compacted = await store.compactHistory({
      turnType: "assistant_final", keepTurns: 1, keepToolPairs: 0, backupMode: "replace",
      preserveResponseId: "resp-current-call",
    });
    assert.equal(compacted.compacted, true);
    const history = [];
    for await (const item of store.readInputItems()) history.push(item);
    assert.deepEqual(history, [currentUser, currentCall]);
    assert.doesNotMatch(await readFile(store.messagesPath, "utf8"), /old-call|large tool data/);
    assert.match(await readFile(store.messagesPath, "utf8"), /resp-current-call/);
    await store.appendInputItem({type: "function_call_output", call_id: "current-call", output: "result"});
    const nextRequestHistory = [];
    for await (const item of store.readInputItems()) nextRequestHistory.push(item);
    assert.deepEqual(nextRequestHistory, [currentUser, currentCall, {type: "function_call_output", call_id: "current-call", output: "result"}]);
    await store.close();
  } finally {
    await removeSession(sessionDir);
  }
});

test("recovers an interrupted compaction before new transcript records are appended", async () => {
  const sessionDir = await makeSession();
  try {
    const store = await SessionResponseStore.open(sessionDir);
    const user = {type: "message", role: "user", content: [{type: "input_text", text: "before crash"}]};
    const reasoning = {type: "reasoning", summary: [{type: "summary_text", text: "long reasoning ".repeat(100)}]};
    const assistant = {type: "message", role: "assistant", content: [{type: "output_text", text: "answer"}]};
    await store.appendInputItem(user);
    const turn = await store.beginTurn("crash-compaction");
    await turn.stageOutputItem({type: "response.output_item.done", output_index: 0, item: reasoning});
    await turn.stageOutputItem({type: "response.output_item.done", output_index: 1, item: assistant});
    await turn.commit(completedEvent([reasoning, assistant], "resp-crash-compaction"));
    const originalTranscript = await readFile(store.messagesPath, "utf8");
    assert.equal((await store.compactHistory({turnType: "assistant_final", keepTurns: 1, keepToolPairs: 0, backupMode: "replace"})).compacted, true);
    await store.close();

    await writeFile(store.messagesPath, originalTranscript, "utf8");
    const recovered = await SessionResponseStore.open(sessionDir);
    assert.equal(await readFile(recovered.messagesPath, "utf8"), "");
    await recovered.appendInputItem({type: "message", role: "user", content: [{type: "input_text", text: "after recovery"}]});
    const history = [];
    for await (const item of recovered.readInputItems()) history.push(item);
    assert.deepEqual(history, [user, assistant, {type: "message", role: "user", content: [{type: "input_text", text: "after recovery"}]}]);
    await recovered.close();
  } finally {
    await removeSession(sessionDir);
  }
});

test("assistant_final never mistakes a tool-call response for the turn summary", async () => {
  const sessionDir = await makeSession();
  try {
    const store = await SessionResponseStore.open(sessionDir);
    const firstUser = {type: "message", role: "user", content: [{type: "input_text", text: "first"}]};
    const oldFinal = {type: "message", role: "assistant", content: [{type: "output_text", text: "first answer"}]};
    const secondUser = {type: "message", role: "user", content: [{type: "input_text", text: "second"}]};
    const intermediate = {type: "message", role: "assistant", content: [{type: "output_text", text: "calling tool"}]};
    const call = {type: "function_call", call_id: "pending-call", name: "lookup", arguments: "{}"};
    await store.appendInputItem(firstUser);
    const first = await store.beginTurn("prior-final");
    await first.stageOutputItem({type: "response.output_item.done", output_index: 0, item: oldFinal});
    await first.commit(completedEvent([oldFinal], "resp-prior-final"));
    await store.appendInputItem(secondUser);
    const toolTurn = await store.beginTurn("pending-tool-turn");
    for (const [output_index, item] of [intermediate, call].entries()) {
      await toolTurn.stageOutputItem({type: "response.output_item.done", output_index, item});
    }
    await toolTurn.commit(completedEvent([intermediate, call], "resp-pending-call"));
    const result = await store.compactHistory({turnType: "assistant_final", keepTurns: 2, keepToolPairs: 0, backupMode: "replace"});
    assert.equal(result.compacted, true);
    const history = [];
    for await (const item of store.readInputItems()) history.push(item);
    assert.deepEqual(history, [firstUser, oldFinal, secondUser]);
    await store.close();
  } finally {
    await removeSession(sessionDir);
  }
});

test("full compaction keeps user, assistant, and reasoning but drops tool traffic", async () => {
  const sessionDir = await makeSession();
  try {
    const store = await SessionResponseStore.open(sessionDir);
    const user = {type: "message", role: "user", content: [{type: "input_text", text: "question"}]};
    const reasoning = {type: "reasoning", summary: [{type: "summary_text", text: "reasoning"}]};
    const assistant = {type: "message", role: "assistant", content: [{type: "output_text", text: "answer"}]};
    const call = {type: "function_call", call_id: "call-full", name: "search", arguments: "{}"};
    await store.appendInputItem(user);
    const turn = await store.beginTurn("compact-full");
    for (const [output_index, item] of [reasoning, assistant, call].entries()) {
      await turn.stageOutputItem({type: "response.output_item.done", output_index, item});
    }
    await turn.commit(completedEvent([reasoning, assistant, call]));
    await store.appendInputItem({type: "function_call_output", call_id: "call-full", output: "large result"});
    const result = await store.compactHistory({turnType: "full", keepTurns: 1, keepToolPairs: 1, backupMode: "replace"});
    assert.equal(result.compacted, true);
    const history = [];
    for await (const item of store.readInputItems()) history.push(item);
    assert.deepEqual(history, [user, reasoning, assistant]);
    await store.close();
  } finally {
    await removeSession(sessionDir);
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
    assert.deepEqual(settings.prompt_documents, []);
    assert.equal(settings.prompt_role, "system");
    assert.equal(settings.prompt_text, "");
    assert.deepEqual(settings.persistence, {
      durability: "turn",
      max_item_bytes: 10 * 1024 * 1024,
    });
    assert.equal(settings.http_debug.enabled, false);
    assert.equal(settings.paths.history_cache, join(sessionDir, "staging/history-cache.json"));
    assert.equal(loader.paths.history_cache_path, join(sessionDir, "staging/history-cache.json"));
    assert.equal(settings.paths.usage_cache, join(sessionDir, "staging/usage-cache.json"));
    assert.equal(loader.paths.usage_cache_path, join(sessionDir, "staging/usage-cache.json"));
    assert.equal(settings.http_debug.include_response_headers, true);
    assert.equal(settings.http_debug.max_body_bytes, 65536);

    await writeFile(join(sessionDir, "setting.json"), JSON.stringify({cwd: sessionDir, system_prompt: "legacy"}));
    await assert.rejects(() => loader.loadSettings(), /system_prompt was removed/);
    await writeFile(join(sessionDir, "setting.json"), JSON.stringify({cwd: sessionDir, prompt_documents: "base.md"}));
    await assert.rejects(() => loader.loadSettings(), /prompt_documents must be an array/);

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

test("auto-compacts at a tool-call stream end and preserves the in-flight call", async () => {
  const sessionDir = await makeSession();
  try {
    await writeFile(join(sessionDir, "model.json"), JSON.stringify({model: "gpt-test"}));
    await writeFile(join(sessionDir, "setting.json"), JSON.stringify({cwd: sessionDir, max_tool_rounds: 0, compaction: {trigger_tokens: 1}}));
    const oldUser = {type: "message", role: "user", content: [{type: "input_text", text: "old question"}]};
    const oldCall = {type: "function_call", call_id: "old-call", name: "lookup", arguments: "{}"};
    const oldOutput = {type: "function_call_output", call_id: "old-call", output: "very large tool output ".repeat(500)};
    const oldAssistant = {type: "message", role: "assistant", content: [{type: "output_text", text: "old answer"}]};
    await writeFile(join(sessionDir, "messages.jsonl"), [
      oldUser,
      {type: "response.completed", response: {id: "old-call-response", status: "completed", output: [oldCall]}},
      oldOutput,
      {type: "response.completed", response: {id: "old-final-response", status: "completed", output: [oldAssistant]}},
    ].map((record) => JSON.stringify(record)).join("\n") + "\n");
    const currentCall = {type: "function_call", call_id: "current-call", name: "not-configured", arguments: "{}"};
    const fakeClient = {
      responses: {create: async () => (async function* () {
        yield {type: "response.output_item.done", output_index: 0, item: currentCall};
        yield completedEvent([currentCall], "resp-auto-tool-call");
      })()},
    } as unknown as OpenAI;
    const session = await AgentSession.open(sessionDir, {client: fakeClient});
    await assert.rejects(() => session.run("new question"), /exceeded 0 tool rounds/);
    const activeTranscript = await readFile(join(sessionDir, "messages.jsonl"), "utf8");
    assert.match(activeTranscript, /resp-auto-tool-call/);
    assert.doesNotMatch(activeTranscript, /very large tool output|old-call/);
    const store = await SessionResponseStore.open(sessionDir);
    const history = [];
    for await (const item of store.readInputItems()) history.push(item);
    assert.equal(history[0]?.role, "user");
    assert.equal(history[1]?.type, "function_call");
    assert.equal(history[1]?.call_id, "current-call");
    assert.equal(history[2]?.type, "function_call_output");
    await store.close();
    await session.close();
  } finally {
    await removeSession(sessionDir);
  }
});

test("auto-compacts after a completed response and reuses the compacted context", async () => {
  const sessionDir = await makeSession();
  try {
    await writeFile(join(sessionDir, "model.json"), JSON.stringify({model: "gpt-test"}));
    await writeFile(join(sessionDir, "setting.json"), JSON.stringify({
      cwd: sessionDir,
      compaction: {trigger_tokens: 1},
      statecontext: {enabled: true, trigger: ["compaction"], tools: ["task"], role: "developer"},
      tool_definition_paths: [join(process.cwd(), "../sample/tools/native/task.json")],
      paths: {statecontext: "snapshots/statecontext.json"},
      tools: {native: {task: {storage_dir: ".tasks"}}},
    }));
    const configured = await new SessionConfigLoader(sessionDir).load();
    const taskPolicy = configured.settings.tools.native.task!;
    const taskStore = new TaskStore(taskPolicy.storage_dir);
    const task = await taskStore.create({title: "Keep the feature in progress", status: "in-progress", priority: "high", tags: [], documents: []}, taskPolicy);
    const requests: Array<Record<string, unknown>> = [];
    let sequence = 0;
    const fakeClient = {
      responses: {create: async (request: unknown) => {
        requests.push(request as Record<string, unknown>);
        sequence += 1;
        const reasoning = {type: "reasoning", summary: [{type: "summary_text", text: "reasoning ".repeat(100)}]};
        const item = {type: "message", role: "assistant", content: [{type: "output_text", text: `answer-${sequence}`}]};
        return (async function* () {
          yield {type: "response.output_item.done", output_index: 0, item: reasoning};
          yield {type: "response.output_item.done", output_index: 1, item};
          yield completedEvent([reasoning, item], `resp-auto-${sequence}`);
        })();
      }},
    } as unknown as OpenAI;
    const session = await AgentSession.open(sessionDir, {client: fakeClient});
    await session.run("first request");
    assert.equal(await readFile(join(sessionDir, "messages.jsonl"), "utf8"), "");
    assert.match(await readFile(join(sessionDir, "snapshots/statecontext.json"), "utf8"), /Keep the feature in progress/);
    await taskStore.update(task.id, {title: "Replaced after the next compaction"}, taskPolicy);
    await session.run("second request");
    const secondInput = requests[1]!.input as Array<Record<string, unknown>>;
    assert.equal(secondInput.length, 4);
    assert.deepEqual(secondInput.map((item) => item.role), ["user", "assistant", "user", "developer"]);
    assert.match(JSON.stringify(secondInput[3]), /Keep the feature in progress/);
    await session.run("third request");
    const thirdInput = requests[2]!.input as Array<Record<string, unknown>>;
    assert.equal(thirdInput.at(-1)?.role, "developer");
    assert.match(JSON.stringify(thirdInput.at(-1)), /Replaced after the next compaction/);
    assert.doesNotMatch(JSON.stringify(thirdInput.at(-1)), /Keep the feature in progress/);
    assert.equal(await readFile(join(sessionDir, "messages.jsonl"), "utf8"), "");
    await session.close();
  } finally {
    await removeSession(sessionDir);
  }
});

test("refreshes statecontext after tool results from an auto-compacted call", async () => {
  const sessionDir = await makeSession();
  try {
    await writeFile(join(sessionDir, "model.json"), JSON.stringify({model: "gpt-test"}));
    await writeFile(join(sessionDir, "setting.json"), JSON.stringify({
      cwd: sessionDir,
      compaction: {trigger_tokens: 1},
      statecontext: {enabled: true, trigger: ["compaction"], tools: ["task"], role: "developer"},
      tool_definition_paths: [join(process.cwd(), "../sample/tools/native/task.json")],
      tools: {native: {task: {storage_dir: ".tasks"}}},
    }));
    const configured = await new SessionConfigLoader(sessionDir).load();
    const taskPolicy = configured.settings.tools.native.task!;
    const task = await new TaskStore(taskPolicy.storage_dir).create({title: "Transition this task", status: "in-progress", priority: "normal", tags: [], documents: []}, taskPolicy);
    const seed = await SessionResponseStore.open(sessionDir);
    await seed.appendInputItem({type: "message", role: "user", content: [{type: "input_text", text: "older question"}]});
    const oldTurn = await seed.beginTurn("statecontext-old-turn");
    const oldFinal = {type: "message", role: "assistant", content: [{type: "output_text", text: "older answer"}]};
    await oldTurn.stageOutputItem({type: "response.output_item.done", output_index: 0, item: oldFinal});
    await oldTurn.commit(completedEvent([oldFinal], "resp-statecontext-old"));
    await seed.close();
    const requests: Array<Record<string, unknown>> = [];
    let sequence = 0;
    const fakeClient = {
      responses: {create: async (request: unknown) => {
        requests.push(request as Record<string, unknown>);
        sequence += 1;
        const item = sequence === 1
          ? {type: "function_call", call_id: "complete-task", name: "task", arguments: JSON.stringify({action: "done", id: task.id})}
          : {type: "message", role: "assistant", content: [{type: "output_text", text: "Task completed."}]};
        const event = completedEvent([item], `resp-statecontext-${sequence}`);
        if (sequence > 1) event.response.usage = {input_tokens: 0, output_tokens: 1};
        return (async function* () {
          yield {type: "response.output_item.done", output_index: 0, item};
          yield event;
        })();
      }},
    } as unknown as OpenAI;
    const session = await AgentSession.open(sessionDir, {client: fakeClient});
    await session.run("complete the task");
    const input = requests[1]!.input as Array<Record<string, unknown>>;
    const outputIndex = input.findIndex((item) => item.type === "function_call_output");
    const snapshotIndex = input.findIndex((item) => item.role === "developer" && typeof item.content === "object");
    assert.ok(outputIndex >= 0);
    assert.ok(snapshotIndex > outputIndex);
    const snapshotContent = input[snapshotIndex]!.content as Array<{text: string}>;
    assert.match(snapshotContent[0]!.text, /\"status\": \"done\"/);
    await session.close();
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
    await writeFile(join(sessionDir, "base.md"), "Be concise.");
    await writeFile(
      join(sessionDir, "setting.json"),
      JSON.stringify({ cwd: sessionDir, prompt_documents: ["base.md"], prompt_role: "developer" }),
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
    assert.equal(first.response.event.session_turn, 1);
    assert.equal(first.response.event.session_step, 1);
    assert.equal(second.response.event.response.id, "resp_runner_2");
    assert.equal(second.response.event.session_turn, 2);
    assert.equal(second.response.event.session_step, 1);

    const firstRequest = requests[0] as Record<string, unknown>;
    const secondRequest = requests[1] as Record<string, unknown>;
    assert.equal(firstRequest.model, "gpt-test");
    assert.equal(firstRequest.stream, true);
    assert.equal(firstRequest.instructions, undefined);
    const firstInput = firstRequest.input as Array<Record<string, unknown>>;
    assert.deepEqual(firstInput[0], {role: "developer", content: [{type: "input_text", text: "Be concise."}]});
    assert.equal(firstInput.length, 2);
    assert.equal((secondRequest.input as unknown[]).length, 4);

    const sessionMessages = (await readFile(join(sessionDir, "messages.jsonl"), "utf8"))
      .trimEnd()
      .split("\n");
    assert.equal(sessionMessages.length, 4);
    const persistedResponses = sessionMessages
      .map((line) => JSON.parse(line) as Record<string, unknown>)
      .filter((record) => record.type === "response.completed");
    assert.deepEqual(persistedResponses.map(({session_turn, session_step}) => [session_turn, session_step]), [
      [1, 1],
      [2, 1],
    ]);
    const usage = await session.readUsageSummary();
    assert.equal(usage.completed_responses, 2);
    assert.equal(usage.turn_count, 2);
    assert.equal(usage.current?.turn, 2);
    assert.equal(usage.current?.step, 1);
    await session.close();
  } finally {
    await removeSession(sessionDir);
  }
});
