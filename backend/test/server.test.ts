import assert from "node:assert/strict";
import { appendFile, mkdir, mkdtemp, readFile, rename, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";

import type OpenAI from "openai";

import {createAgentServer as createServer, type AgentServerOptions} from "../src/server.js";

const TEST_API_TOKEN = "test-api-token-0123456789abcdef0123456789abcdef";
const createAgentServer = (options: AgentServerOptions) => createServer({...options, apiToken: TEST_API_TOKEN});
const fetch: typeof globalThis.fetch = (input, init) => {
  const headers = new Headers(init?.headers);
  headers.set("Authorization", `Bearer ${TEST_API_TOKEN}`);
  return globalThis.fetch(input, {...init, headers});
};
import {
  SESSION_INPUT_BATCH_ID_FIELD,
  SESSION_INPUT_BATCH_INDEX_FIELD,
  SessionResponseStore,
} from "../src/persistence.js";
import {
  normalizeWebUiSession,
  WebUiNormalizedRun,
} from "../src/plugins/web-ui-normalized.js";
import type {WebUiMessage, WebUiPart} from "../src/plugins/web-ui/types.js";
import type { LoadedSessionConfig } from "../src/config.js";
import {nativeReadDefinition} from "../src/tools/native/read.js";
import {nativeBashDefinition} from "../src/tools/native/bash.js";
import {loadRootServerSettings} from "../src/session-roots.js";

function completedEvent(
  output: Record<string, unknown>[],
  id: string,
  usage?: Record<string, unknown>,
): Record<string, unknown> {
  return {
    type: "response.completed",
    response: {
      id,
      object: "response",
      status: "completed",
      model: "fake-model",
      output,
      ...(usage === undefined ? {} : { usage }),
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

/**
 * The messages a client renders from a run stream: `ui.message` replaces the
 * message it holds, `ui.delta` appends to a part and `ui.part` sets one — the
 * same merge the viewer performs.
 */
function runStreamStates(stream: string): WebUiMessage[] {
  const states: WebUiMessage[] = [];
  let current: WebUiMessage | undefined;
  for (const block of stream.split("\n\n")) {
    const name = block
      .split("\n")
      .find((line) => line.startsWith("event:"))
      ?.slice("event:".length)
      .trim();
    const dataLine = block
      .split("\n")
      .find((line) => line.startsWith("data:"))
      ?.slice("data:".length)
      .trimStart();
    if (name === undefined || !dataLine) {
      continue;
    }
    const data = JSON.parse(dataLine) as Record<string, unknown>;
    if (name === "ui.message") {
      current = data.message as WebUiMessage;
      states.push(current);
      continue;
    }
    if (current === undefined) {
      continue;
    }
    if (name === "ui.part") {
      const parts = current.parts.slice();
      parts[data.index as number] = data.part as WebUiPart;
      current = {...current, parts};
      states.push(current);
      continue;
    }
    if (name === "ui.delta") {
      const index = data.index as number;
      const part = current.parts[index];
      if (part === undefined) {
        continue;
      }
      const append = data.append as string;
      const parts = current.parts.slice();
      parts[index] =
        part.type === "tool_call"
          ? {...part, arguments: part.arguments + append}
          : part.type === "tool_result"
            ? {...part, output: part.output + append}
            : {...part, text: part.text + append};
      current = {...current, parts};
      states.push(current);
    }
  }
  return states;
}

test("loads configurable root server settings", async () => {
  const root = await mkdtemp(join(tmpdir(), "agent-harness-root-settings-"));
  const settingPath = join(root, "setting.json");
  try {
    await writeFile(
      settingPath,
      JSON.stringify({
        server: {
          host: "0.0.0.0",
          port: 9876,
          max_body_bytes: 2 * 1024 * 1024,
          run_content_memory_bytes: 4096,
          sse_max_buffered_bytes: 4096,
        },
        session_roots: [{path: ".jad/sessions"}],
      }),
    );
    assert.deepEqual(await loadRootServerSettings({rootSettingPath: settingPath}), {
      host: "0.0.0.0",
      port: 9876,
      max_body_bytes: 2 * 1024 * 1024,
      run_state_retention_ms: 30_000,
      tool_stream_interval_ms: 120,
      run_content_memory_bytes: 4096,
      run_content_write_batch_bytes: 32 * 1024,
      run_content_flush_interval_ms: 25,
      run_content_max_index_segments: 1_024,
      sse_max_buffered_bytes: 4096,
      sse_heartbeat_interval_ms: 5_000,
      read_header_timeout_ms: 60_000,
      idle_timeout_ms: 5_000,
      conditional_requests: true,
      snapshot_turns: 24,
      max_page_turns: 50,
      max_inline_part_bytes: 16 * 1024,
      allowed_origins: [],
    });

    await writeFile(
      settingPath,
      JSON.stringify({server: {port: 65536}, session_roots: [{path: ".jad/sessions"}]}),
    );
    await assert.rejects(
      () => loadRootServerSettings({rootSettingPath: settingPath}),
      /server\.port/,
    );

    await writeFile(
      settingPath,
      JSON.stringify({server: {run_state_retention_ms: -1}, session_roots: [{path: ".jad/sessions"}]}),
    );
    await assert.rejects(
      () => loadRootServerSettings({rootSettingPath: settingPath}),
      /server\.run_state_retention_ms/,
    );

    await writeFile(
      settingPath,
      JSON.stringify({server: {run_content_memory_bytes: -1}, session_roots: [{path: ".jad/sessions"}]}),
    );
    await assert.rejects(
      () => loadRootServerSettings({rootSettingPath: settingPath}),
      /server\.run_content_memory_bytes/,
    );

    await writeFile(
      settingPath,
      JSON.stringify({server: {run_content_write_batch_bytes: 3}, session_roots: [{path: ".jad/sessions"}]}),
    );
    await assert.rejects(
      () => loadRootServerSettings({rootSettingPath: settingPath}),
      /server\.run_content_write_batch_bytes/,
    );

    await writeFile(
      settingPath,
      JSON.stringify({server: {run_content_flush_interval_ms: 60_001}, session_roots: [{path: ".jad/sessions"}]}),
    );
    await assert.rejects(
      () => loadRootServerSettings({rootSettingPath: settingPath}),
      /server\.run_content_flush_interval_ms/,
    );

    await writeFile(
      settingPath,
      JSON.stringify({server: {run_content_max_index_segments: 0}, session_roots: [{path: ".jad/sessions"}]}),
    );
    await assert.rejects(
      () => loadRootServerSettings({rootSettingPath: settingPath}),
      /server\.run_content_max_index_segments/,
    );

    await writeFile(
      settingPath,
      JSON.stringify({
        server: {conditional_requests: "yes", snapshot_turns: -1, max_page_turns: 0},
        session_roots: [{path: ".jad/sessions"}],
      }),
    );
    await assert.rejects(
      () => loadRootServerSettings({rootSettingPath: settingPath}),
      /server\.conditional_requests/,
    );
    await writeFile(
      settingPath,
      JSON.stringify({
        server: {snapshot_turns: -1},
        session_roots: [{path: ".jad/sessions"}],
      }),
    );
    await assert.rejects(
      () => loadRootServerSettings({rootSettingPath: settingPath}),
      /server\.snapshot_turns/,
    );
    await writeFile(
      settingPath,
      JSON.stringify({
        server: {max_page_turns: 0},
        session_roots: [{path: ".jad/sessions"}],
      }),
    );
    await assert.rejects(
      () => loadRootServerSettings({rootSettingPath: settingPath}),
      /server\.max_page_turns/,
    );
    await writeFile(
      settingPath,
      JSON.stringify({
        server: {max_inline_part_bytes: -1},
        session_roots: [{path: ".jad/sessions"}],
      }),
    );
    await assert.rejects(
      () => loadRootServerSettings({rootSettingPath: settingPath}),
      /server\.max_inline_part_bytes/,
    );
    await writeFile(
      settingPath,
      JSON.stringify({
        server: {sse_max_buffered_bytes: 1023},
        session_roots: [{path: ".jad/sessions"}],
      }),
    );
    await assert.rejects(
      () => loadRootServerSettings({rootSettingPath: settingPath}),
      /server\.sse_max_buffered_bytes/,
    );
    for (const [key, bad, limit] of [
      ["read_header_timeout_ms", -1, 300_000],
      ["read_header_timeout_ms", 300_001, 300_000],
      ["idle_timeout_ms", 1.5, 3_600_000],
      ["idle_timeout_ms", 3_600_001, 3_600_000],
    ] as const) {
      await writeFile(
        settingPath,
        JSON.stringify({server: {[key]: bad}, session_roots: [{path: ".jad/sessions"}]}),
      );
      await assert.rejects(
        () => loadRootServerSettings({rootSettingPath: settingPath}),
        new RegExp(`server\\.${key} must be a safe integer between 0 and ${limit}`),
      );
    }
    await writeFile(
      settingPath,
      JSON.stringify({
        server: {read_header_timeout_ms: 0, idle_timeout_ms: 3_600_000},
        session_roots: [{path: ".jad/sessions"}],
      }),
    );
    const timeouts = await loadRootServerSettings({rootSettingPath: settingPath});
    assert.equal(timeouts.read_header_timeout_ms, 0);
    assert.equal(timeouts.idle_timeout_ms, 3_600_000);
    // The Go backend's log_level, log_format and log_file share this section. The
    // TypeScript backend must keep loading a setting.json that carries them, even
    // with values it would never validate.
    for (const server of [
      {log_level: "debug", log_format: "json", log_file: "logs/agent.log"},
      {log_level: "verbose", log_format: 42, log_file: 7},
    ]) {
      await writeFile(settingPath, JSON.stringify({server, session_roots: [{path: ".jad/sessions"}]}));
      const ignored = await loadRootServerSettings({rootSettingPath: settingPath});
      assert.equal(ignored.idle_timeout_ms, 5_000);
      assert.equal("log_level" in ignored, false);
    }
    await writeFile(settingPath, JSON.stringify({server: {allowed_origins: ["*", "http://localhost:5173"]}}));
    await assert.rejects(
      () => loadRootServerSettings({rootSettingPath: settingPath}),
      /server\.allowed_origins/,
    );
  } finally {
    await rm(root, {recursive: true, force: true});
  }
});

test("requires an API token and an explicitly allowed browser origin", async () => {
  const root = await mkdtemp(join(tmpdir(), "agent-api-auth-"));
  assert.throws(() => createServer({sessionsDir: root, environment: {}}), /AGENT_API_TOKEN/);
  const running = createServer({sessionsDir: root, environment: {AGENT_API_TOKEN: TEST_API_TOKEN}, allowedOrigins: ["http://localhost:5173"]});
  await new Promise<void>((resolve, reject) => {
    running.server.once("error", reject);
    running.server.listen(0, "127.0.0.1", resolve);
  });
  try {
    const address = running.server.address();
    assert.ok(address && typeof address !== "string");
    const base = `http://127.0.0.1:${address.port}/api`;
    assert.equal((await globalThis.fetch(`${base}/health`)).status, 200);
    assert.equal((await globalThis.fetch(`${base}/sessions`)).status, 401);
    assert.equal((await globalThis.fetch(`${base}/sessions`, {headers: {Authorization: "Bearer wrong"}})).status, 401);
    const hostile = await globalThis.fetch(`${base}/sessions`, {headers: {Origin: "https://hostile.example", Authorization: `Bearer ${TEST_API_TOKEN}`}});
    assert.equal(hostile.status, 403);
    assert.equal(hostile.headers.get("access-control-allow-origin"), null);
    const preflight = await globalThis.fetch(`${base}/sessions`, {method: "OPTIONS", headers: {Origin: "http://localhost:5173", "Access-Control-Request-Method": "POST", "Access-Control-Request-Headers": "authorization,content-type"}});
    assert.equal(preflight.status, 204);
    assert.equal(preflight.headers.get("access-control-allow-origin"), "http://localhost:5173");
    assert.match(preflight.headers.get("access-control-allow-headers") ?? "", /Authorization/);
    const authorized = await globalThis.fetch(`${base}/sessions`, {headers: {Origin: "http://localhost:5173", Authorization: `Bearer ${TEST_API_TOKEN}`}});
    assert.equal(authorized.status, 200);
  } finally {
    await running.close();
    await rm(root, {recursive: true, force: true});
  }
});

test("serves file-backed libraries with live setting and Markdown edits", async () => {
  const globalRoot = await mkdtemp(join(tmpdir(), "agent-library-api-"));
  const settingPath = join(globalRoot, "setting.json");
  const promptRoot = join(globalRoot, ".agent/prompts");
  const replacementRoot = join(globalRoot, "shared-prompts");
  await mkdir(promptRoot, {recursive: true});
  await mkdir(replacementRoot, {recursive: true});
  const promptPath = join(promptRoot, "first.md");
  await writeFile(promptPath, "---\nname: First prompt\n---\nInitial content.");
  await writeFile(
    settingPath,
    JSON.stringify({
      schema_version: 1,
      session_roots: [],
      libraries: {
        prompt_dirs: [".agent/prompts"],
        template_dirs: [],
        skill_dirs: [],
      },
    }),
  );
  const running = createAgentServer({rootSettingPath: settingPath, environment: {}});
  await new Promise<void>((resolve, reject) => {
    running.server.once("error", reject);
    running.server.listen(0, "127.0.0.1", resolve);
  });
  try {
    const address = running.server.address();
    assert.ok(address && typeof address !== "string");
    const base = `http://127.0.0.1:${address.port}/api/library`;
    assert.equal((await globalThis.fetch(base)).status, 401);

    const initialResponse = await fetch(base);
    assert.equal(initialResponse.status, 200);
    assert.equal(initialResponse.headers.get("cache-control"), "no-store");
    const initial = (await initialResponse.json()) as {
      prompts: Array<{id: string; name: string}>;
      templates: unknown[];
      skills: unknown[];
    };
    assert.deepEqual(initial.prompts.map((item) => item.name), ["First prompt"]);

    await writeFile(promptPath, "---\nname: First prompt\n---\nUpdated content.");
    const updatedBody = await fetch(`${base}/prompt/${initial.prompts[0]!.id}`);
    assert.equal(updatedBody.status, 200);
    assert.equal(((await updatedBody.json()) as {asset: {content: string}}).asset.content, "Updated content.");

    await writeFile(join(replacementRoot, "second.md"), "Second prompt content.");
    await writeFile(
      settingPath,
      JSON.stringify({
        schema_version: 1,
        session_roots: [],
        libraries: {
          prompt_dirs: [replacementRoot],
          template_dirs: [],
          skill_dirs: [],
        },
      }),
    );
    const reloaded = (await (await fetch(base)).json()) as {
      prompts: Array<{name: string}>;
    };
    assert.deepEqual(reloaded.prompts.map((item) => item.name), ["second"]);
  } finally {
    await running.close();
    await rm(globalRoot, {recursive: true, force: true});
  }
});

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

test("uses latest and cumulative usage from the cached session summary", () => {
  const snapshot = normalizeWebUiSession(
    "session1",
    {
      model: { provider: "fake", model: "fake-model" },
      settings: { session_name: "usage" },
    } as unknown as LoadedSessionConfig,
    [
      completedEvent([], "response-usage-one", {
        input_tokens: 100,
        output_tokens: 25,
        total_tokens: 125,
        input_tokens_details: { cached_tokens: 10 },
      }),
      completedEvent([], "response-usage-two", {
        input_tokens: 40,
        output_tokens: 5,
      }),
      completedEvent([], "response-without-usage"),
    ],
    {
      completed_responses: 3,
      responses_with_usage: 2,
      turn_count: 2,
      history_partial: false,
      input_tokens: 140,
      output_tokens: 30,
      total_tokens: 170,
      cached_input_tokens: 10,
      current: {
        turn: 2,
        step: 2,
        response_id: "response-usage-two",
        input_tokens: 40,
        output_tokens: 5,
        total_tokens: 45,
      },
    },
  );

  assert.deepEqual(snapshot.usage, {
    completed_responses: 3,
    responses_with_usage: 2,
    turn_count: 2,
    history_partial: false,
    input_tokens: 140,
    output_tokens: 30,
    total_tokens: 170,
    cached_input_tokens: 10,
    current: {
      turn: 2,
      step: 2,
      response_id: "response-usage-two",
      input_tokens: 40,
      output_tokens: 5,
      total_tokens: 45,
    },
  });
  assert.equal(snapshot.turns, 3);
});

test("does not recalculate usage from the transcript when no cache summary is provided", () => {
  const snapshot = normalizeWebUiSession(
    "session1",
    {
      model: { provider: "fake", model: "fake-model" },
      settings: { session_name: "usage" },
    } as unknown as LoadedSessionConfig,
    [
      completedEvent([], "response-without-usage"),
      completedEvent([], "response-invalid-usage", {
        input_tokens: "100",
        output_tokens: -1,
        total_tokens: 3.5,
        input_tokens_details: { cached_tokens: null },
      }),
    ],
  );

  assert.equal(snapshot.usage, undefined);
});

test("preserves partial token fields from the cached usage summary", () => {
  const snapshot = normalizeWebUiSession(
    "session1",
    {
      model: { provider: "fake", model: "fake-model" },
      settings: { session_name: "usage" },
    } as unknown as LoadedSessionConfig,
    [completedEvent([], "response-partial-usage", { input_tokens: 12 })],
    {
      completed_responses: 1,
      responses_with_usage: 1,
      turn_count: 1,
      history_partial: false,
      input_tokens: 12,
      current: {
        turn: 1,
        step: 1,
        response_id: "response-partial-usage",
        input_tokens: 12,
      },
    },
  );

  assert.deepEqual(snapshot.usage, {
    completed_responses: 1,
    responses_with_usage: 1,
    turn_count: 1,
    history_partial: false,
    input_tokens: 12,
    current: {
      turn: 1,
      step: 1,
      response_id: "response-partial-usage",
      input_tokens: 12,
    },
  });
});

test("preserves interleaved reasoning and tool-call order in the live UI stream", () => {
  const normalizedRun = new WebUiNormalizedRun("interleaved-tool-turn");
  const callOne = {
    type: "function_call",
    call_id: "call_one",
    name: "read",
    arguments: "{\"path\":\"one\"}",
  };
  const callTwo = {
    type: "function_call",
    call_id: "call_two",
    name: "write",
    arguments: "{\"path\":\"two\",\"content\":\"two\"}",
  };
  const callThree = {
    type: "function_call",
    call_id: "call_three",
    name: "read",
    arguments: "{\"path\":\"three\"}",
  };
  const events = [
    {type: "response.reasoning_summary_text.delta", delta: "r1"},
    {type: "response.output_item.done", output_index: 1, item: callOne},
    {type: "response.reasoning_summary_text.delta", delta: "r2"},
    {type: "response.output_item.done", output_index: 3, item: callTwo},
    {type: "response.output_item.done", output_index: 4, item: callThree},
    {type: "response.reasoning_summary_text.delta", delta: "r3"},
  ];
  for (const event of events) {
    normalizedRun.update(event);
  }

  assert.deepEqual(
    normalizedRun.complete({id: "response-interleaved-tools", output: []}).parts,
    [
      {type: "reasoning", text: "r1"},
      {type: "tool_call", callId: "call_one", name: "read", arguments: "{\"path\":\"one\"}"},
      {type: "reasoning", text: "r2"},
      {type: "tool_call", callId: "call_two", name: "write", arguments: "{\"path\":\"two\",\"content\":\"two\"}"},
      {type: "tool_call", callId: "call_three", name: "read", arguments: "{\"path\":\"three\"}"},
      {type: "reasoning", text: "r3"},
    ],
  );
});

test("marks only append-only normalizer updates for the streaming fast path", () => {
  const normalizedRun = new WebUiNormalizedRun("explicit-stream-update");
  const firstText = normalizedRun.updateForStream({
    type: "response.output_text.delta",
    delta: "hello",
  });
  assert.equal(firstText?.append, undefined);

  const textGrowth = normalizedRun.updateForStream({
    type: "response.output_text.delta",
    delta: " world",
  });
  assert.deepEqual(textGrowth?.append, {index: 0, append: " world"});

  const firstReasoning = normalizedRun.updateForStream({
    type: "response.reasoning_summary_text.delta",
    delta: "thinking",
  });
  assert.equal(firstReasoning?.append, undefined);
  const reasoningGrowth = normalizedRun.updateForStream({
    type: "response.reasoning_summary_text.delta",
    delta: " more",
  });
  assert.deepEqual(reasoningGrowth?.append, {index: 1, append: " more"});

  const addedTool = normalizedRun.updateForStream({
    type: "response.output_item.added",
    output_index: 0,
    item: {id: "fc-explicit", type: "function_call", call_id: "call-explicit", name: "read", arguments: ""},
  });
  assert.equal(addedTool?.append, undefined);
  const argumentGrowth = normalizedRun.updateForStream({
    type: "response.function_call_arguments.delta",
    item_id: "fc-explicit",
    output_index: 0,
    delta: "{}",
  });
  assert.deepEqual(argumentGrowth?.append, {index: 2, append: "{}", outputIndex: 0});
});

test("correlates streamed deltas and staged output items with normalized parts", () => {
  const normalizedRun = new WebUiNormalizedRun("staged-output-index");
  const first = normalizedRun.updateForStream({
    type: "response.output_text.delta",
    output_index: 0,
    delta: "first ",
  });
  const second = normalizedRun.updateForStream({
    type: "response.output_text.delta",
    output_index: 1,
    delta: "second",
  });
  assert.equal(first?.outputIndex, 0);
  assert.equal(second?.outputIndex, 1);
  assert.deepEqual(second?.append, {index: 0, append: "second", outputIndex: 1});

  const firstItem = {
    id: "message-0",
    type: "message",
    role: "assistant",
    content: [{type: "output_text", text: "first "}],
  };
  const secondItem = {
    id: "message-1",
    type: "message",
    role: "assistant",
    content: [{type: "output_text", text: "second"}],
  };
  assert.equal(normalizedRun.partIndexForOutputItem(0, firstItem), 0);
  assert.equal(normalizedRun.partIndexForOutputItem(1, secondItem), 0);
  assert.equal(normalizedRun.stagedOutputContent(firstItem, 0), "first ");
  assert.equal(normalizedRun.stagedOutputContent(secondItem, 0), "second");
});

test("ignores encrypted or empty reasoning summaries in the UI stream", () => {
  const normalizedRun = new WebUiNormalizedRun("opaque-reasoning-turn");
  assert.equal(
    normalizedRun.update({
      type: "response.reasoning_summary_text.delta",
      delta: "   ",
    }),
    undefined,
  );
  normalizedRun.update({type: "response.output_text.delta", delta: "answer"});
  const completed = normalizedRun.complete({
    id: "response-opaque-reasoning",
    output: [
      {type: "reasoning", encrypted_content: "opaque"},
      {type: "message", role: "assistant", content: [{type: "output_text", text: "answer"}]},
    ],
  });
  assert.deepEqual(completed.parts, [{type: "text", text: "answer"}]);
  assert.equal(completed.reasoningContent, undefined);
});

test("drops whitespace-only assistant content between tool calls", () => {
  const output = [
    {
      type: "function_call",
      call_id: "call_first",
      name: "read",
      arguments: "{\"path\":\"first\"}",
    },
    {type: "message", role: "assistant", content: [{type: "output_text", text: "\n"}]},
    {
      type: "function_call",
      call_id: "call_second",
      name: "read",
      arguments: "{\"path\":\"second\"}",
    },
  ];
  const expectedParts = [
    {type: "tool_call", callId: "call_first", name: "read", arguments: "{\"path\":\"first\"}"},
    {type: "tool_call", callId: "call_second", name: "read", arguments: "{\"path\":\"second\"}"},
  ];
  const snapshot = normalizeWebUiSession(
    "session1",
    {
      model: { provider: "fake", model: "fake-model" },
      settings: { session_name: "tools" },
    } as unknown as LoadedSessionConfig,
    [completedEvent(output, "response-whitespace-between-tools")],
  );
  assert.deepEqual(snapshot.messages[0]?.parts, expectedParts);

  const normalizedRun = new WebUiNormalizedRun("live-whitespace-between-tools");
  assert.deepEqual(
    normalizedRun.update(completedEvent(output, "response-whitespace-between-tools"))?.parts,
    expectedParts,
  );
});

test("normalizes tool call and result parts for snapshots and live runs", () => {
  const call = {
    type: "function_call",
    call_id: "call_read",
    name: "read",
    arguments: JSON.stringify({path: "README.md"}),
  };
  const result = {
    type: "function_call_output",
    call_id: "call_read",
    output: '{"status":"ok"}\n--- CONTENT token=abc ---\nhello',
  };
  const snapshot = normalizeWebUiSession(
    "session1",
    {
      model: { provider: "fake", model: "fake-model" },
      settings: { session_name: "tools" },
    } as unknown as LoadedSessionConfig,
    [
      completedEvent([call], "response-tool"),
      result,
      completedEvent(
        [{type: "message", role: "assistant", content: [{type: "output_text", text: "Done"}]}],
        "response-final",
      ),
    ],
  );
  assert.equal(snapshot.messages.length, 1);
  assert.deepEqual(snapshot.messages[0]?.parts, [
    {
      type: "tool_call",
      callId: "call_read",
      name: "read",
      arguments: JSON.stringify({path: "README.md"}),
    },
    {
      type: "tool_result",
      callId: "call_read",
      output: '{"status":"ok"}\n--- CONTENT token=abc ---\nhello',
      status: "success",
    },
    {type: "text", text: "Done"},
  ]);

  const normalizedRun = new WebUiNormalizedRun("live-tool-turn");
  assert.deepEqual(
    normalizedRun.update(completedEvent([call], "response-tool"))?.parts,
    [{
      type: "tool_call",
      callId: "call_read",
      name: "read",
      arguments: JSON.stringify({path: "README.md"}),
    }],
  );
  assert.deepEqual(normalizedRun.toolResult(result)?.parts, [
    {
      type: "tool_call",
      callId: "call_read",
      name: "read",
      arguments: JSON.stringify({path: "README.md"}),
    },
    {
      type: "tool_result",
      callId: "call_read",
      output: '{"status":"ok"}\n--- CONTENT token=abc ---\nhello',
      status: "success",
    },
  ]);
  assert.deepEqual(normalizedRun.complete({id: "response-final", output: [
    {type: "message", role: "assistant", content: [{type: "output_text", text: "Done"}]},
  ]}).parts, [
    {
      type: "tool_call",
      callId: "call_read",
      name: "read",
      arguments: JSON.stringify({path: "README.md"}),
    },
    {
      type: "tool_result",
      callId: "call_read",
      output: '{"status":"ok"}\n--- CONTENT token=abc ---\nhello',
      status: "success",
    },
    {type: "text", text: "Done"},
  ]);
});

test("preserves provider step ranges and leaves the final assistant summary visible", () => {
  const config = {
    model: {provider: "fake", model: "fake-model"},
    settings: {session_name: "step-ranges"},
  } as unknown as LoadedSessionConfig;
  const firstCall = {
    type: "function_call",
    call_id: "call-step-one",
    name: "read",
    arguments: "{}",
  };
  const secondCall = {
    type: "function_call",
    call_id: "call-step-two",
    name: "grep",
    arguments: "{}",
  };
  const snapshot = normalizeWebUiSession("session1", config, [
    completedEvent([
      {type: "reasoning", summary: [{type: "summary_text", text: "Plan one"}]},
      {type: "message", role: "assistant", content: [{type: "output_text", text: "Checking one"}]},
      firstCall,
    ], "response-step-one"),
    {type: "function_call_output", call_id: "call-step-one", output: "first result"},
    completedEvent([
      {type: "reasoning", summary: [{type: "summary_text", text: "Plan two"}]},
      {type: "message", role: "assistant", content: [{type: "output_text", text: "Checking two"}]},
      secondCall,
    ], "response-step-two"),
    {type: "function_call_output", call_id: "call-step-two", output: "second result"},
    completedEvent([
      {type: "message", role: "assistant", content: [{type: "output_text", text: "Final summary"}]},
    ], "response-summary"),
  ]);
  const message = snapshot.messages[0];
  assert.ok(message);
  assert.deepEqual(message.steps, [
    {id: "response-step-one", startPart: 0, endPart: 4, hasTools: true},
    {id: "response-step-two", startPart: 4, endPart: 8, hasTools: true},
    {id: "response-summary", startPart: 8, endPart: 9, hasTools: false},
  ]);
  assert.deepEqual(message.parts.slice(0, 4).map((part) => part.type), [
    "reasoning", "text", "tool_call", "tool_result",
  ]);
  assert.deepEqual(message.parts.slice(8).map((part) => part.type), ["text"]);

  const normalizedRun = new WebUiNormalizedRun("live-step-ranges");
  normalizedRun.update({type: "response.created", response: {id: "live-step-one"}});
  normalizedRun.update({type: "response.reasoning_summary_text.delta", delta: "Plan"});
  normalizedRun.update({type: "response.output_text.delta", delta: "Checking"});
  normalizedRun.update({
    type: "response.output_item.added",
    output_index: 0,
    item: {...firstCall, id: "fc-live-step-one"},
  });
  normalizedRun.toolResult({
    type: "function_call_output",
    call_id: "call-step-one",
    output: "first result",
  });
  normalizedRun.update({type: "response.created", response: {id: "live-summary"}});
  normalizedRun.update({type: "response.output_text.delta", delta: "Final summary"});
  const live = normalizedRun.complete({
    id: "live-summary",
    output: [{
      type: "message",
      role: "assistant",
      content: [{type: "output_text", text: "Final summary"}],
    }],
  });
  assert.deepEqual(live.steps, [
    {id: "live-step-one", startPart: 0, endPart: 4, hasTools: true},
    {id: "live-summary", startPart: 4, endPart: 5, hasTools: false},
  ]);
});

test("streams function-call arguments before the item completes", () => {
  const call = {
    id: "fc_1",
    type: "function_call",
    call_id: "call_write",
    name: "write",
    arguments: "",
  };
  const normalizedRun = new WebUiNormalizedRun("streaming-tool-turn");

  assert.deepEqual(
    normalizedRun.update({type: "response.output_item.added", output_index: 0, item: call})?.parts,
    [{type: "tool_call", callId: "call_write", name: "write", arguments: "", partial: true}],
  );

  // An added message item is not a tool call and must not create a part.
  assert.equal(
    normalizedRun.update({
      type: "response.output_item.added",
      output_index: 1,
      item: {id: "msg_1", type: "message", role: "assistant", content: []},
    }),
    undefined,
  );

  assert.deepEqual(
    normalizedRun.update({
      type: "response.function_call_arguments.delta",
      item_id: "fc_1",
      output_index: 0,
      delta: '{"path":"notes.md","content":"hel',
    })?.parts,
    [{
      type: "tool_call",
      callId: "call_write",
      name: "write",
      arguments: '{"path":"notes.md","content":"hel',
      partial: true,
    }],
  );

  assert.deepEqual(
    normalizedRun.update({
      type: "response.function_call_arguments.delta",
      item_id: "fc_1",
      output_index: 0,
      delta: 'lo"}',
    })?.parts,
    [{
      type: "tool_call",
      callId: "call_write",
      name: "write",
      arguments: '{"path":"notes.md","content":"hello"}',
      partial: true,
    }],
  );

  // `arguments.done` is the last word on the argument text and clears the flag.
  assert.deepEqual(
    normalizedRun.update({
      type: "response.function_call_arguments.done",
      item_id: "fc_1",
      output_index: 0,
      arguments: '{"path":"notes.md","content":"hello"}',
    })?.parts,
    [{
      type: "tool_call",
      callId: "call_write",
      name: "write",
      arguments: '{"path":"notes.md","content":"hello"}',
    }],
  );

  // The finished item replaces the streamed call instead of adding a second one.
  normalizedRun.update({
    type: "response.output_item.done",
    output_index: 0,
    item: {...call, arguments: '{"path":"notes.md","content":"hello"}'},
  });
  assert.deepEqual(
    normalizedRun.complete({
      id: "response-write",
      output: [{...call, arguments: '{"path":"notes.md","content":"hello"}'}],
    }).parts,
    [{
      type: "tool_call",
      callId: "call_write",
      name: "write",
      arguments: '{"path":"notes.md","content":"hello"}',
    }],
  );
});

test("a call interrupted mid-arguments is not left marked partial", () => {
  const normalizedRun = new WebUiNormalizedRun("interrupted-tool-turn");
  normalizedRun.update({
    type: "response.output_item.added",
    output_index: 0,
    item: {id: "fc_2", type: "function_call", call_id: "call_edit", name: "edit", arguments: ""},
  });
  normalizedRun.update({
    type: "response.function_call_arguments.delta",
    item_id: "fc_2",
    output_index: 0,
    delta: '{"path":"notes.md"',
  });

  const complete = normalizedRun.complete({
    id: "response-interrupted",
    output: [{
      id: "fc_2",
      type: "function_call",
      call_id: "call_edit",
      name: "edit",
      arguments: '{"path":"notes.md"}',
    }],
  });
  assert.deepEqual(complete.parts, [{
    type: "tool_call",
    callId: "call_edit",
    name: "edit",
    arguments: '{"path":"notes.md"}',
  }]);
});

test("lists sessions with valid settings and namespaces duplicate ids", async () => {
  const workspace = await mkdtemp(join(tmpdir(), "agent-harness-roots-"));
  const primaryDir = join(workspace, "roots", "primary", "shared");
  const secondaryDir = join(workspace, "roots", "secondary", "shared");
  const incompleteDir = join(workspace, "roots", "primary", "luna");
  const missingSettingDir = join(workspace, "roots", "primary", "missing-setting");
  const rootSettingPath = join(workspace, "setting.json");

  for (const [directory, name] of [
    [primaryDir, "Primary session"],
    [secondaryDir, "Secondary session"],
  ] as const) {
    await mkdir(directory, {recursive: true});
    await writeFile(
      join(directory, "model.json"),
      JSON.stringify({
        schema_version: 1,
        provider: "fake",
        api_key_env: "FAKE_API_KEY",
        model: "fake-model",
      }),
    );
    await writeFile(
      join(directory, "setting.json"),
      JSON.stringify({
        schema_version: 1,
        session_name: name,
        cwd: directory,
      }),
    );
  }
  await mkdir(incompleteDir, {recursive: true});
  await mkdir(missingSettingDir, {recursive: true});
  await writeFile(
    join(incompleteDir, "setting.json"),
    JSON.stringify({
      schema_version: 1,
      session_name: "Luna session",
      cwd: primaryDir,
    }),
  );
  await writeFile(
    join(incompleteDir, "model.json"),
    JSON.stringify({
      schema_version: 1,
      provider: {only: ["openai/flex"]},
      model: "openai/gpt-5.6-luna",
    }),
  );
  await writeFile(
    rootSettingPath,
    JSON.stringify({
      schema_version: 1,
      session_roots: [
        {id: "primary", label: "Primary", path: "roots/primary"},
        {id: "secondary", label: "Secondary", path: "roots/secondary"},
      ],
    }),
  );

  const running = createAgentServer({
    rootSettingPath,
    environment: {FAKE_API_KEY: "test-key"},
  });
  await new Promise<void>((resolve, reject) => {
    running.server.once("error", reject);
    running.server.listen(0, "127.0.0.1", () => resolve());
  });

  try {
    const address = running.server.address();
    assert.ok(address && typeof address !== "string");
    const baseUrl = `http://127.0.0.1:${address.port}`;

    const list = await fetch(`${baseUrl}/api/sessions`);
    assert.equal(list.status, 200);
    const body = (await list.json()) as {
      sessions: Array<{
        id: string;
        session_id?: string;
        root_id?: string;
        root_label?: string;
        session_name: string;
      }>;
    };
    assert.deepEqual(body.sessions.map((session) => ({
      id: session.id,
      session_id: session.session_id,
      root_id: session.root_id,
      root_label: session.root_label,
      session_name: session.session_name,
    })), [
      {
        id: "primary:luna",
        session_id: "luna",
        root_id: "primary",
        root_label: "Primary",
        session_name: "Luna session",
      },
      {
        id: "primary:shared",
        session_id: "shared",
        root_id: "primary",
        root_label: "Primary",
        session_name: "Primary session",
      },
      {
        id: "secondary:shared",
        session_id: "shared",
        root_id: "secondary",
        root_label: "Secondary",
        session_name: "Secondary session",
      },
    ]);

    const snapshot = await fetch(
      `${baseUrl}/api/sessions/${encodeURIComponent("secondary:shared")}`,
    );
    assert.equal(snapshot.status, 200);
    const selected = (await snapshot.json()) as {
      id: string;
      session_name: string;
      root_id?: string;
    };
    assert.equal(selected.id, "secondary:shared");
    assert.equal(selected.session_name, "Secondary session");
    assert.equal(selected.root_id, "secondary");

    const ambiguous = await fetch(`${baseUrl}/api/sessions/shared`);
    assert.equal(ambiguous.status, 400);
  } finally {
    await running.close();
    await rm(workspace, {recursive: true, force: true});
  }
});

test("serves sessions using profile model-config and transcript paths", async () => {
  const root = await mkdtemp(join(tmpdir(), "agent-harness-configured-paths-"));
  const sessionDir = join(root, "session1");
  const modelPath = join(sessionDir, "chatgpt", "model.json");
  const messagesPath = join(sessionDir, "data", "transcript.jsonl");
  await mkdir(sessionDir, {recursive: true});
  await mkdir(join(sessionDir, "chatgpt"), {recursive: true});
  await mkdir(join(sessionDir, "data"), {recursive: true});
  await writeFile(
    modelPath,
    JSON.stringify({provider: "configured-provider", model: "configured-model"}),
  );
  await writeFile(
    join(sessionDir, "setting.json"),
    JSON.stringify({
      session_name: "configured paths",
      cwd: sessionDir,
      paths: {
        messages: "data/transcript.jsonl",
        staging: "runtime/staging",
        http_debug: "diagnostics/http",
        active_profile: "chatgpt",
        profiles: {
          chatgpt: {
            model_default: "chatgpt/model.json",
            models: ["chatgpt/model.json"],
          },
        },
      },
    }),
  );
  await writeFile(
    messagesPath,
    `${JSON.stringify({
      type: "response.completed",
      response: {
        id: "configured-response",
        status: "completed",
        usage: {
          input_tokens: 23,
          output_tokens: 7,
          total_tokens: 30,
          input_tokens_details: {cached_tokens: 3},
        },
        output: [
          {type: "reasoning", content: [{type: "reasoning_text", text: "configured reasoning"}]},
          {
            type: "message",
            role: "assistant",
            content: [{type: "output_text", text: "from configured transcript"}],
          },
        ],
      },
    })}\n`,
  );

  const cacheStore = await SessionResponseStore.open(sessionDir, {
    messagesPath,
    stagingDir: "runtime/staging",
  });
  await cacheStore.close();

  const running = createAgentServer({sessionsDir: root});
  await new Promise<void>((resolve, reject) => {
    running.server.once("error", reject);
    running.server.listen(0, "127.0.0.1", () => resolve());
  });

  try {
    const address = running.server.address();
    assert.ok(address && typeof address !== "string");
    const baseUrl = `http://127.0.0.1:${address.port}`;
    const listResponse = await fetch(`${baseUrl}/api/sessions`);
    assert.equal(listResponse.status, 200);
    const list = (await listResponse.json()) as {
      sessions: Array<{
        id: string;
        model: {provider: string; model: string};
        turns: number;
        usage?: Record<string, unknown>;
      }>;
    };
    assert.equal(list.sessions[0]?.id, "session1");
    assert.deepEqual(list.sessions[0]?.model, {
      provider: "configured-provider",
      model: "configured-model",
    });
    // The list is metadata only: a transcript is fetched per session.
    assert.equal(
      Object.hasOwn(list.sessions[0] ?? {}, "messages"),
      false,
    );
    assert.deepEqual(list.sessions[0]?.usage, {
      completed_responses: 1,
      responses_with_usage: 1,
      turn_count: 1,
      history_partial: false,
      input_tokens: 23,
      output_tokens: 7,
      total_tokens: 30,
      cached_input_tokens: 3,
      current: {
        input_tokens: 23,
        output_tokens: 7,
        total_tokens: 30,
        cached_input_tokens: 3,
        turn: 1,
        step: 1,
        response_id: "configured-response",
      },
    });

    const snapshotResponse = await fetch(`${baseUrl}/api/sessions/session1`);
    assert.equal(snapshotResponse.status, 200);
    const snapshot = (await snapshotResponse.json()) as {
      model: {provider: string; model: string};
      messages: Array<{text: string}>;
      usage?: Record<string, unknown>;
    };
    assert.deepEqual(snapshot.model, {
      provider: "configured-provider",
      model: "configured-model",
    });
    assert.equal(snapshot.messages[0]?.text, "from configured transcript");
    assert.deepEqual(snapshot.usage, list.sessions[0]?.usage);

    const compactResponse = await fetch(`${baseUrl}/api/sessions/session1/compact`, {method: "POST"});
    assert.equal(compactResponse.status, 200);
    const compacted = await compactResponse.json() as {
      compaction: {compacted: boolean};
      usage: {
        input_tokens?: number;
        output_tokens?: number;
        total_tokens?: number;
        cached_input_tokens?: number;
        current?: {
          input_tokens?: number;
          output_tokens?: number;
          total_tokens?: number;
          cached_input_tokens?: number;
          turn: number;
          step: number;
        };
      };
    };
    assert.equal(compacted.compaction.compacted, true);
    assert.equal(compacted.usage.input_tokens, list.sessions[0]?.usage?.input_tokens);
    assert.equal(compacted.usage.output_tokens, list.sessions[0]?.usage?.output_tokens);
    assert.equal(compacted.usage.total_tokens, list.sessions[0]?.usage?.total_tokens);
    assert.equal(compacted.usage.current?.input_tokens, 0);
    assert.equal(compacted.usage.current?.output_tokens, 0);
    assert.equal(compacted.usage.current?.total_tokens, 0);
    assert.equal(compacted.usage.current?.cached_input_tokens, 0);
    assert.equal(await readFile(messagesPath, "utf8"), "");
    assert.match(await readFile(join(sessionDir, "data/messages.backup.jsonl"), "utf8"), /configured transcript/);
    const compactionCachePath = join(sessionDir, "runtime/staging/compaction-cache.json");
    const cachedContext = await readFile(compactionCachePath, "utf8");
    const noOpCompactResponse = await fetch(`${baseUrl}/api/sessions/session1/compact`, {method: "POST"});
    assert.equal(noOpCompactResponse.status, 200);
    const noOpCompacted = await noOpCompactResponse.json() as {compaction: {compacted: boolean}};
    assert.equal(noOpCompacted.compaction.compacted, false);
    assert.equal(await readFile(compactionCachePath, "utf8"), cachedContext);
    const compactedSnapshot = await fetch(`${baseUrl}/api/sessions/session1`);
    const compactedBody = await compactedSnapshot.json() as {
      messages: Array<{text: string}>;
      history?: {etag: string | null; count: number};
      usage?: Record<string, unknown>;
    };
    // The transcript half is empty after compaction: the removed turn is the
    // block the snapshot refers to, fetched (and revalidated) on its own.
    assert.deepEqual(compactedBody.messages, []);
    assert.equal(typeof compactedBody.history?.etag, "string");
    const historyResponse = await fetch(`${baseUrl}/api/sessions/session1/history`);
    assert.equal(historyResponse.status, 200);
    const history = await historyResponse.json() as {messages: Array<{text: string}>};
    assert.equal(history.messages[0]?.text, "from configured transcript");
    assert.deepEqual(compactedBody.usage, compacted.usage);
  } finally {
    await running.close();
    await rm(root, {recursive: true, force: true});
  }
});

test("persists user input before a tool configuration failure", async () => {
  const root = await mkdtemp(join(tmpdir(), "agent-harness-input-persist-"));
  const sessionDir = join(root, "session1");
  await mkdir(sessionDir, {recursive: true});
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
      session_name: "persist-input",
      cwd: sessionDir,
      tool_definition_paths: ["missing-tools.json"],
      tools: {
        native: {
          read: {max_bytes: 65536, default_limit: 250},
        },
      },
    }),
  );

  const running = createAgentServer({sessionsDir: root});
  await new Promise<void>((resolve, reject) => {
    running.server.once("error", reject);
    running.server.listen(0, "127.0.0.1", () => resolve());
  });

  try {
    const address = running.server.address();
    assert.ok(address && typeof address !== "string");
    const response = await fetch(
      `http://127.0.0.1:${address.port}/api/sessions/session1/run`,
      {
        method: "POST",
        headers: {"content-type": "application/json"},
        body: JSON.stringify({input: "persist this input"}),
      },
    );
    assert.equal(response.status, 200);
    const errorStream = await response.text();
    assert.match(errorStream, /The run failed\. Response details were withheld/);
    assert.doesNotMatch(errorStream, /missing-tools\.json/);

    const records = (await readFile(join(sessionDir, "messages.jsonl"), "utf8"))
      .trim()
      .split("\n")
      .map((line) => JSON.parse(line) as Record<string, unknown>);
    const batchId = records[0]?.[SESSION_INPUT_BATCH_ID_FIELD];
    assert.equal(typeof batchId, "string");
    assert.deepEqual(records[0], {
      type: "message",
      role: "user",
      content: [{type: "input_text", text: "persist this input"}],
      [SESSION_INPUT_BATCH_ID_FIELD]: batchId,
      [SESSION_INPUT_BATCH_INDEX_FIELD]: 0,
    });
  } finally {
    await running.close();
    await rm(root, {recursive: true, force: true});
  }
});

test("a change to the run-error cache alone busts the snapshot etag", async () => {
  const root = await mkdtemp(join(tmpdir(), "agent-harness-run-error-etag-"));
  const sessionDir = join(root, "session1");
  await mkdir(join(sessionDir, "staging"), {recursive: true});
  await writeFile(
    join(sessionDir, "model.json"),
    JSON.stringify({schema_version: 1, provider: "fake", api_key_env: "FAKE_API_KEY", model: "fake-model"}),
  );
  await writeFile(
    join(sessionDir, "setting.json"),
    JSON.stringify({schema_version: 1, session_name: "etag", cwd: sessionDir}),
  );
  await writeFile(join(sessionDir, "messages.jsonl"), `${JSON.stringify({
    type: "message",
    role: "user",
    content: [{type: "input_text", text: "unanswered"}],
  })}\n`);

  const running = createAgentServer({sessionsDir: root});
  await new Promise<void>((resolve, reject) => {
    running.server.once("error", reject);
    running.server.listen(0, "127.0.0.1", () => resolve());
  });

  try {
    const address = running.server.address();
    assert.ok(address && typeof address !== "string");
    const url = `http://127.0.0.1:${address.port}/api/sessions/session1`;
    const first = await fetch(url);
    const etag = first.headers.get("etag");
    assert.ok(etag);
    assert.equal(Object.hasOwn((await first.json()) as object, "failures"), false);
    assert.equal((await fetch(url, {headers: {"if-none-match": etag}})).status, 304);

    // Only the cache file changes; messages.jsonl and the run state do not.
    await writeFile(
      join(sessionDir, "staging", "run-error-cache.json"),
      JSON.stringify({
        schema_version: 1,
        errors: [{turn: 1, turn_id: "t", at: "2026-01-01T00:00:00.000Z", message: "boom"}],
      }),
    );
    const changed = await fetch(url, {headers: {"if-none-match": etag}});
    assert.equal(changed.status, 200);
    assert.notEqual(changed.headers.get("etag"), etag);
    const body = (await changed.json()) as {failures?: Array<{message: string}>};
    assert.equal(body.failures?.[0]?.message, "boom");
  } finally {
    await running.close();
    await rm(root, {recursive: true, force: true});
  }
});

test("records a failed run outside messages.jsonl and leaves the transcript untouched", async () => {
  const root = await mkdtemp(join(tmpdir(), "agent-harness-run-error-"));
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
      session_name: "run-error",
      cwd: sessionDir,
      tool_definition_paths: ["missing-tools.json"],
      tools: {native: {read: {max_bytes: 65536, default_limit: 250}}},
    }),
  );

  const running = createAgentServer({sessionsDir: root});
  await new Promise<void>((resolve, reject) => {
    running.server.once("error", reject);
    running.server.listen(0, "127.0.0.1", () => resolve());
  });

  try {
    const address = running.server.address();
    assert.ok(address && typeof address !== "string");
    const response = await fetch(`http://127.0.0.1:${address.port}/api/sessions/session1/run`, {
      method: "POST",
      headers: {"content-type": "application/json"},
      body: JSON.stringify({input: "this run will fail"}),
    });
    assert.equal(response.status, 200);
    assert.match(await response.text(), /The run failed\. Response details were withheld/);

    // The transcript is exactly what it was before this change: the user record only.
    const transcript = await readFile(join(sessionDir, "messages.jsonl"), "utf8");
    assert.equal(transcript.trim().split("\n").length, 1);
    assert.doesNotMatch(transcript, /run failed|Response details were withheld|missing-tools/i);

    const cache = JSON.parse(await readFile(join(sessionDir, "staging", "run-error-cache.json"), "utf8")) as {
      schema_version: number;
      errors: Array<{turn: number; turn_id: string; at: string; message: string}>;
    };
    assert.equal(cache.schema_version, 1);
    assert.equal(cache.errors.length, 1);
    assert.equal(cache.errors[0]?.turn, 1);
    assert.match(cache.errors[0]?.message ?? "", /The run failed\. Response details were withheld/);
    // The cache carries the client-safe message, never the underlying cause.
    assert.doesNotMatch(JSON.stringify(cache), /missing-tools/);
    assert.ok(!Number.isNaN(Date.parse(cache.errors[0]?.at ?? "")));

    // A reload sees the failure even after the in-memory run state is gone.
    const baseUrl = `http://127.0.0.1:${address.port}`;
    const snapshotResponse = await fetch(`${baseUrl}/api/sessions/session1`);
    assert.equal(snapshotResponse.status, 200);
    const snapshotEtag = snapshotResponse.headers.get("etag");
    assert.ok(snapshotEtag);
    const snapshot = (await snapshotResponse.json()) as {
      failures?: Array<{turn: number; message_id?: string; message: string}>;
      messages: Array<{id: string; sender: string}>;
    };
    assert.equal(snapshot.failures?.length, 1);
    assert.equal(snapshot.failures?.[0]?.turn, 1);
    // The failure names the user message it belongs to, by the id the projection gave it.
    assert.equal(snapshot.failures?.[0]?.message_id, snapshot.messages[0]?.id);
    assert.match(snapshot.failures?.[0]?.message_id ?? "", /^session-user-o(0|[1-9][0-9]*)$/);
    assert.match(snapshot.failures?.[0]?.message ?? "", /The run failed/);
    // The failure is not a message: the transcript still shows only the user turn.
    assert.deepEqual(snapshot.messages.map((message) => message.sender), ["user"]);

    // Revalidation holds while nothing changes...
    const unchanged = await fetch(`${baseUrl}/api/sessions/session1`, {headers: {"if-none-match": snapshotEtag}});
    assert.equal(unchanged.status, 304);

    // ...and a second failure changes only the cache file, which must still bust the etag.
    const second = await fetch(`${baseUrl}/api/sessions/session1/run`, {
      method: "POST",
      headers: {"content-type": "application/json"},
      body: JSON.stringify({input: "this one fails too"}),
    });
    await second.text();
    const afterSecond = await fetch(`${baseUrl}/api/sessions/session1`, {headers: {"if-none-match": snapshotEtag}});
    assert.equal(afterSecond.status, 200);
    assert.notEqual(afterSecond.headers.get("etag"), snapshotEtag);
    const secondBody = (await afterSecond.json()) as {
      failures?: Array<{turn: number; message_id?: string}>;
      messages: Array<{id: string; sender: string}>;
    };
    assert.deepEqual(secondBody.failures?.map((failure) => failure.turn), [1, 2]);
    // The second failure points at the second user message, not the first.
    const userIds = secondBody.messages.filter((message) => message.sender === "user").map((message) => message.id);
    assert.equal(userIds.length, 2);
    assert.deepEqual(secondBody.failures?.map((failure) => failure.message_id), userIds);
  } finally {
    await running.close();
    await rm(root, {recursive: true, force: true});
  }
});

test("loads active path-profile model configs for defaults and selections", async () => {
  const root = await mkdtemp(join(tmpdir(), "agent-harness-model-profile-"));
  const sessionDir = join(root, "session1");
  await mkdir(sessionDir, {recursive: true});
  await writeFile(join(sessionDir, "model.json"), JSON.stringify({
    schema_version: 1,
    provider: "fake",
    api_key_env: "FAKE_API_KEY",
    model: "configured-model",
  }));
  await mkdir(join(sessionDir, "chatgpt"), {recursive: true});
  await mkdir(join(sessionDir, "openai"), {recursive: true});
  await writeFile(join(sessionDir, "chatgpt/model.json"), JSON.stringify({
    schema_version: 1,
    provider: "fake",
    api_key_env: "FAKE_API_KEY",
    model: "chatgpt-configured-model",
  }));
  await writeFile(join(sessionDir, "openai/model.json"), JSON.stringify({
    schema_version: 1,
    provider: "fake",
    api_key_env: "FAKE_API_KEY",
    model: "openai-configured-model",
  }));
  await writeFile(join(sessionDir, "setting.json"), JSON.stringify({
    schema_version: 1,
    session_name: "model-profile",
    cwd: sessionDir,
    paths: {
      active_profile: "primary",
      profiles: {
        primary: {
          model_default: "chatgpt/model.json",
          models: ["chatgpt/model.json", "openai/model.json"],
        },
        alternate: {
          model_default: "openai/model.json",
          models: ["openai/model.json"],
        },
      },
    },
  }));

  const requestedModels: string[] = [];
  let responseIndex = 0;
  const output = {
    type: "message",
    role: "assistant",
    content: [{type: "output_text", text: "done"}],
  };
  const fakeClient = {
    responses: {
      create: async (request: {model?: string}) => {
        requestedModels.push(request.model ?? "");
        const responseId = `resp_model_profile_${responseIndex++}`;
        return (async function* () {
          yield {type: "response.output_item.done", output_index: 0, item: output};
          yield completedEvent([output], responseId);
        })();
      },
    },
  } as unknown as OpenAI;

  const running = createAgentServer({sessionsDir: root, client: fakeClient});
  await new Promise<void>((resolve, reject) => {
    running.server.once("error", reject);
    running.server.listen(0, "127.0.0.1", () => resolve());
  });

  try {
    const address = running.server.address();
    assert.ok(address && typeof address !== "string");
    const baseUrl = `http://127.0.0.1:${address.port}`;
    const listResponse = await fetch(`${baseUrl}/api/sessions`);
    assert.equal(listResponse.status, 200);
    const list = (await listResponse.json()) as {
      sessions: Array<{
        id: string;
        active_profile?: string;
        path_profiles?: string[];
        model_default?: string;
        models?: string[];
        model?: {model: string};
      }>;
    };
    const listedSession = list.sessions.find((session) => session.id === "session1");
    assert.equal(listedSession?.active_profile, "primary");
    assert.deepEqual(listedSession?.path_profiles, ["primary", "alternate"]);
    assert.equal(listedSession?.model_default, "chatgpt/model.json");
    assert.deepEqual(listedSession?.models, ["chatgpt/model.json", "openai/model.json"]);
    assert.equal(listedSession?.model?.model, "chatgpt-configured-model");

    const snapshotResponse = await fetch(`${baseUrl}/api/sessions/session1`);
    assert.equal(snapshotResponse.status, 200);
    const snapshot = (await snapshotResponse.json()) as {
      active_profile?: string;
      path_profiles?: string[];
      model_default?: string;
      models?: string[];
      model?: {model: string};
    };
    assert.equal(snapshot.active_profile, "primary");
    assert.deepEqual(snapshot.path_profiles, ["primary", "alternate"]);
    assert.equal(snapshot.model_default, "chatgpt/model.json");
    assert.deepEqual(snapshot.models, ["chatgpt/model.json", "openai/model.json"]);
    assert.equal(snapshot.model?.model, "chatgpt-configured-model");

    const runUrl = `${baseUrl}/api/sessions/session1/run`;
    const defaultRun = await fetch(runUrl, {
      method: "POST",
      headers: {"content-type": "application/json"},
      body: JSON.stringify({input: "use the profile default"}),
    });
    assert.equal(defaultRun.status, 200);
    await defaultRun.text();
    assert.deepEqual(requestedModels, ["chatgpt-configured-model"]);

    const selectedRun = await fetch(runUrl, {
      method: "POST",
      headers: {"content-type": "application/json"},
      body: JSON.stringify({input: "use my selection", model: "openai/model.json"}),
    });
    assert.equal(selectedRun.status, 200);
    await selectedRun.text();
    assert.deepEqual(requestedModels, ["chatgpt-configured-model", "openai-configured-model"]);

    const updatedSettings = JSON.parse(await readFile(join(sessionDir, "setting.json"), "utf8")) as {
      paths: {profiles: Record<string, {model_default: string}>};
    };
    updatedSettings.paths.profiles.primary!.model_default = "openai/model.json";
    await writeFile(join(sessionDir, "setting.json"), JSON.stringify(updatedSettings));

    const etag = listResponse.headers.get("etag");
    assert.ok(etag);
    const refreshedListResponse = await fetch(`${baseUrl}/api/sessions`, {
      headers: {"if-none-match": etag},
    });
    assert.equal(refreshedListResponse.status, 200);
    const refreshedList = (await refreshedListResponse.json()) as {
      sessions: Array<{id: string; model_default?: string}>;
    };
    assert.equal(
      refreshedList.sessions.find((session) => session.id === "session1")?.model_default,
      "openai/model.json",
    );

    const updatedDefaultRun = await fetch(runUrl, {
      method: "POST",
      headers: {"content-type": "application/json"},
      body: JSON.stringify({input: "use the changed profile default"}),
    });
    assert.equal(updatedDefaultRun.status, 200);
    await updatedDefaultRun.text();
    assert.deepEqual(requestedModels, [
      "chatgpt-configured-model",
      "openai-configured-model",
      "openai-configured-model",
    ]);

    const modelDefaultResponse = await fetch(`${baseUrl}/api/sessions/session1/model-default`, {
      method: "POST",
      headers: {"content-type": "application/json"},
      body: JSON.stringify({model_default: "chatgpt/model.json"}),
    });
    assert.equal(modelDefaultResponse.status, 200);
    const modelDefaultUpdate = (await modelDefaultResponse.json()) as {
      active_profile: string;
      model_default: string;
      models: string[];
      model: {provider: string; model: string};
      changed: boolean;
    };
    assert.equal(modelDefaultUpdate.active_profile, "primary");
    assert.equal(modelDefaultUpdate.model_default, "chatgpt/model.json");
    assert.deepEqual(modelDefaultUpdate.models, ["chatgpt/model.json", "openai/model.json"]);
    assert.deepEqual(modelDefaultUpdate.model, {provider: "fake", model: "chatgpt-configured-model"});
    assert.equal(modelDefaultUpdate.changed, true);
    const savedModelSettings = JSON.parse(await readFile(join(sessionDir, "setting.json"), "utf8")) as {
      paths: {profiles: Record<string, {model_default: string}>};
    };
    assert.equal(savedModelSettings.paths.profiles.primary?.model_default, "chatgpt/model.json");

    const selectedDefaultRun = await fetch(runUrl, {
      method: "POST",
      headers: {"content-type": "application/json"},
      body: JSON.stringify({input: "use the persisted model default"}),
    });
    assert.equal(selectedDefaultRun.status, 200);
    await selectedDefaultRun.text();
    assert.deepEqual(requestedModels, [
      "chatgpt-configured-model",
      "openai-configured-model",
      "openai-configured-model",
      "chatgpt-configured-model",
    ]);

    const invalidModelDefault = await fetch(`${baseUrl}/api/sessions/session1/model-default`, {
      method: "POST",
      headers: {"content-type": "application/json"},
      body: JSON.stringify({model_default: "missing/model.json"}),
    });
    assert.equal(invalidModelDefault.status, 400);

    const invalidRun = await fetch(runUrl, {
      method: "POST",
      headers: {"content-type": "application/json"},
      body: JSON.stringify({input: "reject this model", model: "missing/model.json"}),
    });
    assert.equal(invalidRun.status, 400);
    assert.deepEqual(requestedModels, [
      "chatgpt-configured-model",
      "openai-configured-model",
      "openai-configured-model",
      "chatgpt-configured-model",
    ]);

    const profileResponse = await fetch(`${baseUrl}/api/sessions/session1/profile`, {
      method: "POST",
      headers: {"content-type": "application/json"},
      body: JSON.stringify({active_profile: "alternate"}),
    });
    assert.equal(profileResponse.status, 200);
    const profile = (await profileResponse.json()) as {
      active_profile: string;
      path_profiles: string[];
      model_default?: string;
      models?: string[];
      changed: boolean;
    };
    assert.equal(profile.active_profile, "alternate");
    assert.deepEqual(profile.path_profiles, ["primary", "alternate"]);
    assert.equal(profile.model_default, "openai/model.json");
    assert.deepEqual(profile.models, ["openai/model.json"]);
    assert.equal(profile.changed, true);
    const savedSettings = JSON.parse(await readFile(join(sessionDir, "setting.json"), "utf8")) as {
      paths: {
        active_profile: string;
        profiles: Record<string, {messages?: string; staging?: string}>;
      };
    };
    assert.equal(savedSettings.paths.active_profile, "alternate");
    assert.equal(savedSettings.paths.profiles.primary?.messages, join(sessionDir, "messages.jsonl"));
    assert.equal(savedSettings.paths.profiles.primary?.staging, join(sessionDir, "staging"));
    assert.equal(
      savedSettings.paths.profiles.alternate?.messages,
      join(sessionDir, "profiles/alternate/messages.jsonl"),
    );
    assert.equal(
      savedSettings.paths.profiles.alternate?.staging,
      join(sessionDir, "profiles/alternate/staging"),
    );

    const alternateRun = await fetch(runUrl, {
      method: "POST",
      headers: {"content-type": "application/json"},
      body: JSON.stringify({input: "use the alternate profile default"}),
    });
    assert.equal(alternateRun.status, 200);
    await alternateRun.text();
    assert.deepEqual(requestedModels, [
      "chatgpt-configured-model",
      "openai-configured-model",
      "openai-configured-model",
      "chatgpt-configured-model",
      "openai-configured-model",
    ]);
    const primaryTranscript = await readFile(join(sessionDir, "messages.jsonl"), "utf8");
    const alternateTranscript = await readFile(
      join(sessionDir, "profiles/alternate/messages.jsonl"),
      "utf8",
    );
    assert.match(primaryTranscript, /use the persisted model default/);
    assert.doesNotMatch(primaryTranscript, /use the alternate profile default/);
    assert.match(alternateTranscript, /use the alternate profile default/);
    assert.doesNotMatch(alternateTranscript, /use the profile default/);

    const invalidProfile = await fetch(`${baseUrl}/api/sessions/session1/profile`, {
      method: "POST",
      headers: {"content-type": "application/json"},
      body: JSON.stringify({active_profile: "missing"}),
    });
    assert.equal(invalidProfile.status, 400);
    const unchangedSettings = JSON.parse(await readFile(join(sessionDir, "setting.json"), "utf8")) as {
      paths: {active_profile: string};
    };
    assert.equal(unchangedSettings.paths.active_profile, "alternate");
  } finally {
    await running.close();
    await rm(root, {recursive: true, force: true});
  }
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
      tool_definition_paths: ["read.json"],
      tools: {
        native: {
          read: {max_bytes: 65536, default_limit: 250},
        },
      },
    }),
  );
  await writeFile(
    join(root, "read.json"),
    JSON.stringify({schema_version: 1, tools: [nativeReadDefinition]}),
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

    const tools = await fetch(`${baseUrl}/api/sessions/session1/tools`);
    assert.equal(tools.status, 200);
    assert.deepEqual(await tools.json(), {
      tools: [nativeReadDefinition],
    });

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
    assert.match(stream, /event: usage/);
    assert.match(stream, /event: done/);
    const usageEvents = ssePayloads(stream, "usage");
    assert.equal(usageEvents.length, 1);
    assert.deepEqual(usageEvents[0]?.usage, {
      completed_responses: 1,
      responses_with_usage: 0,
      turn_count: 1,
      history_partial: false,
      current: {turn: 1, step: 1, response_id: "resp_bridge"},
    });

    const uiMessages = runStreamStates(stream);
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
      usage?: Record<string, unknown>;
      messages: Array<{
        sender: string;
        text: string;
        parts: Array<{ type: string; text: string }>;
        status?: string;
      }>;
    };
    assert.equal(final.turns, 1);
    assert.deepEqual(final.usage, usageEvents[0]?.usage);
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
    assert.equal(persisted[1]?.session_turn, 1);
    assert.equal(persisted[1]?.session_step, 1);
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

test("a started run names its own turn on the stream that carries it", async () => {
  const root = await mkdtemp(join(tmpdir(), "agent-harness-run-frame-"));
  const sessionDir = join(root, "session1");
  await mkdir(sessionDir, {recursive: true});
  await writeFile(join(sessionDir, "model.json"), JSON.stringify({model: "fake-model"}));
  await writeFile(
    join(sessionDir, "setting.json"),
    JSON.stringify({session_name: "run-frame-test", cwd: sessionDir}),
  );

  const answer = {type: "message", role: "assistant", content: [{type: "output_text", text: "Ok"}]};
  const fakeClient = {
    responses: {
      create: async () => (async function* () {
        yield {type: "response.output_item.done", output_index: 0, item: answer};
        yield completedEvent([answer], "response-run-frame");
      })(),
    },
  } as unknown as OpenAI;
  const running = createAgentServer({sessionsDir: root, client: fakeClient});
  await new Promise<void>((resolve, reject) => {
    running.server.once("error", reject);
    running.server.listen(0, "127.0.0.1", resolve);
  });

  try {
    const address = running.server.address();
    assert.ok(address && typeof address !== "string");
    const response = await fetch(`http://127.0.0.1:${address.port}/api/sessions/session1/run`, {
      method: "POST",
      headers: {"content-type": "application/json"},
      body: JSON.stringify({input: "say ok"}),
    });
    assert.equal(response.status, 200);
    const stream = await response.text();
    const names = stream
      .split("\n\n")
      .map((block) => block.split("\n").find((line) => line.startsWith("event:"))?.slice("event:".length).trim())
      .filter((name): name is string => name !== undefined && name.length > 0);
    // The stream names the run it carries before anything else, exactly like the
    // attach route: a client that later reads a session snapshot has to recognize
    // which run the status it sees belongs to.
    assert.equal(names[0], "run");
    const runFrame = ssePayloads(stream, "run")[0];
    assert.equal(typeof runFrame?.turn_id, "string");
    assert.equal(runFrame?.status, "running");
    assert.deepEqual(names, ["run", "usage", "ui.message", "completed", "done"]);
  } finally {
    await running.close();
    await rm(root, {recursive: true, force: true});
  }
});

test("publishes zero latest-response usage after automatic compaction", async () => {
  const root = await mkdtemp(join(tmpdir(), "agent-harness-auto-compaction-usage-"));
  const sessionDir = join(root, "session1");
  await mkdir(sessionDir, {recursive: true});
  await writeFile(join(sessionDir, "model.json"), JSON.stringify({model: "fake-model"}));
  await writeFile(join(sessionDir, "setting.json"), JSON.stringify({
    session_name: "auto-compaction-usage-test",
    cwd: sessionDir,
    compaction: {
      trigger_tokens: 1,
      keepbatch: {tools: 0, turn: {type: "assistant_final", keep: 1}},
    },
  }));

  const reasoning = {type: "reasoning", summary: [{type: "summary_text", text: "internal notes"}]};
  const answer = {type: "message", role: "assistant", content: [{type: "output_text", text: "Answer"}]};
  const fakeClient = {
    responses: {
      create: async () => (async function* () {
        yield {type: "response.output_item.done", output_index: 0, item: reasoning};
        yield {type: "response.output_item.done", output_index: 1, item: answer};
        yield completedEvent([reasoning, answer], "response-after-compaction", {
          input_tokens: 21,
          output_tokens: 7,
          total_tokens: 28,
        });
      })(),
    },
  } as unknown as OpenAI;
  const running = createAgentServer({sessionsDir: root, client: fakeClient});
  await new Promise<void>((resolve, reject) => {
    running.server.once("error", reject);
    running.server.listen(0, "127.0.0.1", resolve);
  });

  try {
    const address = running.server.address();
    assert.ok(address && typeof address !== "string");
    const response = await fetch(`http://127.0.0.1:${address.port}/api/sessions/session1/run`, {
      method: "POST",
      headers: {"content-type": "application/json"},
      body: JSON.stringify({input: "answer briefly"}),
    });
    assert.equal(response.status, 200);
    const events = ssePayloads(await response.text(), "usage");
    assert.equal(events.length, 2);
    assert.deepEqual(events[0]?.usage, {
      completed_responses: 1,
      responses_with_usage: 1,
      turn_count: 1,
      history_partial: false,
      input_tokens: 21,
      output_tokens: 7,
      total_tokens: 28,
      current: {
        input_tokens: 21,
        output_tokens: 7,
        total_tokens: 28,
        turn: 1,
        step: 1,
        response_id: "response-after-compaction",
      },
    });
    assert.deepEqual(events[1]?.usage, {
      completed_responses: 1,
      responses_with_usage: 1,
      turn_count: 1,
      history_partial: false,
      input_tokens: 21,
      output_tokens: 7,
      total_tokens: 28,
      current: {
        input_tokens: 0,
        output_tokens: 0,
        total_tokens: 0,
        turn: 1,
        step: 1,
        response_id: "response-after-compaction",
        cached_input_tokens: 0,
      },
    });
  } finally {
    await running.close();
    await rm(root, {recursive: true, force: true});
  }
});

test("sanitizes provider error details before sending a run failure over SSE", async () => {
  const root = await mkdtemp(join(tmpdir(), "agent-harness-provider-error-"));
  const sessionDir = join(root, "session1");
  await mkdir(sessionDir, {recursive: true});
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
    JSON.stringify({schema_version: 1, session_name: "provider-error-test", cwd: sessionDir}),
  );

  const privateMarker = "private-provider-body-and-header-marker";
  const providerError = Object.assign(
    new Error(`status: 401,\\n headers: Headers {\\n 'x-codex-plan-type': 'plus'\\n}\\n${privateMarker}`),
    {status: 401},
  );
  const fakeClient = {
    responses: {
      create: async () => {
        throw providerError;
      },
    },
  } as unknown as OpenAI;
  const running = createAgentServer({sessionsDir: root, client: fakeClient});
  await new Promise<void>((resolve, reject) => {
    running.server.once("error", reject);
    running.server.listen(0, "127.0.0.1", () => resolve());
  });

  try {
    const address = running.server.address();
    assert.ok(address && typeof address !== "string");
    const baseUrl = `http://127.0.0.1:${address.port}`;
    const response = await fetch(`${baseUrl}/api/sessions/session1/run`, {
      method: "POST",
      headers: {"content-type": "application/json"},
      body: JSON.stringify({input: "trigger provider error"}),
    });
    assert.equal(response.status, 200);
    const stream = await response.text();
    const errors = ssePayloads(stream, "error");
    assert.deepEqual(errors, [{
      message: "The provider request failed (HTTP 401). Response details were withheld.",
    }]);
    assert.match(stream, /event: done/);
    assert.doesNotMatch(stream, /private-provider-body-and-header-marker|x-codex-plan-type|Headers/);
    assert.doesNotMatch(stream, /event: ui\.message|event: completed/);

    // The durable record carries the same client-safe text and never the private body.
    const cacheText = await readFile(join(sessionDir, "staging", "run-error-cache.json"), "utf8");
    assert.doesNotMatch(cacheText, /private-provider-body-and-header-marker|x-codex-plan-type|Headers/);
    const recorded = (JSON.parse(cacheText) as {errors: Array<{turn: number; message: string}>}).errors;
    assert.deepEqual(recorded.map((item) => item.message), [
      "The provider request failed (HTTP 401). Response details were withheld.",
    ]);
    assert.equal(recorded[0]?.turn, 1);
  } finally {
    await running.close();
    await rm(root, {recursive: true, force: true});
  }
});

test("streams per-response usage, including incomplete responses, with tool blocks over SSE", async () => {
  const root = await mkdtemp(join(tmpdir(), "agent-harness-server-tools-"));
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
      session_name: "server-tools-test",
      cwd: sessionDir,
      tool_definition_paths: ["read.json"],
      tools: {native: {read: {max_bytes: 65536, default_limit: 250}}},
    }),
  );
  await writeFile(
    join(root, "read.json"),
    JSON.stringify({schema_version: 1, tools: [nativeReadDefinition]}),
  );
  await writeFile(join(sessionDir, "sample.txt"), "tool result content\\n", "utf8");

  const call = {
    type: "function_call",
    call_id: "call_read",
    name: "read",
    arguments: JSON.stringify({path: "sample.txt"}),
  };
  const finalMessage = {
    type: "message",
    role: "assistant",
    content: [{type: "output_text", text: "Tool finished."}],
  };
  let responseNumber = 0;
  const fakeClient = {
    responses: {
      create: async () => {
        responseNumber += 1;
        const currentResponse = responseNumber;
        const output = currentResponse === 1 ? call : finalMessage;
        return (async function* () {
          yield {type: "response.output_item.done", output_index: 0, item: output};
          if (currentResponse === 1) {
            yield completedEvent([output], "response-tool-1", {
              input_tokens: 10,
              output_tokens: 3,
            });
            return;
          }
          const terminal = completedEvent([output], "response-tool-2", {
            input_tokens: 15,
            output_tokens: 4,
          });
          yield {
            type: "response.incomplete",
            response: {
              ...(terminal.response as Record<string, unknown>),
              status: "incomplete",
              incomplete_details: {reason: "max_output_tokens"},
            },
          };
        })();
      },
    },
  } as unknown as OpenAI;

  const running = createAgentServer({sessionsDir: root, client: fakeClient});
  await new Promise<void>((resolve, reject) => {
    running.server.once("error", reject);
    running.server.listen(0, "127.0.0.1", () => resolve());
  });

  try {
    const address = running.server.address();
    assert.ok(address && typeof address !== "string");
    const run = await fetch(
      `http://127.0.0.1:${address.port}/api/sessions/session1/run`,
      {
        method: "POST",
        headers: {"content-type": "application/json"},
        body: JSON.stringify({input: "read the sample"}),
      },
    );
    assert.equal(run.status, 200);
    const stream = await run.text();
    const uiMessages = runStreamStates(stream);
    assert.deepEqual(uiMessages.map((message) => message.status), [
      "streaming",
      "streaming",
      "complete",
    ]);
    assert.deepEqual(uiMessages[0]?.parts, [
      {
        type: "tool_call",
        callId: "call_read",
        name: "read",
        arguments: JSON.stringify({path: "sample.txt"}),
      },
    ]);
    assert.equal(uiMessages[1]?.parts[1]?.type, "tool_result");
    assert.equal((uiMessages[1]?.parts[1] as {status?: string}).status, "success");
    assert.equal(uiMessages[2]?.parts[2]?.type, "text");
    assert.equal((uiMessages[2]?.parts[2] as {text?: string}).text, "Tool finished.");

    const snapshot = await fetch("http://127.0.0.1:" + address.port + "/api/sessions/session1");
    const body = (await snapshot.json()) as {
      messages: Array<{parts: Array<Record<string, unknown>>}>;
      usage?: {current?: {turn: number; step: number}};
    };
    assert.deepEqual(body.messages[1]?.parts.map((part) => part.type), [
      "tool_call",
      "tool_result",
      "text",
    ]);
    assert.deepEqual(body.usage?.current, {
      input_tokens: 15,
      output_tokens: 4,
      total_tokens: 19,
      turn: 1,
      step: 2,
      response_id: "response-tool-2",
    });
    const usageEvents = ssePayloads(stream, "usage");
    assert.equal(usageEvents.length, 2);
    assert.deepEqual(usageEvents[0]?.usage, {
      completed_responses: 1,
      responses_with_usage: 1,
      turn_count: 1,
      history_partial: false,
      input_tokens: 10,
      output_tokens: 3,
      total_tokens: 13,
      current: {
        input_tokens: 10,
        output_tokens: 3,
        total_tokens: 13,
        turn: 1,
        step: 1,
        response_id: "response-tool-1",
      },
    });
    assert.deepEqual(usageEvents[1]?.usage, {
      completed_responses: 2,
      responses_with_usage: 2,
      turn_count: 1,
      history_partial: false,
      input_tokens: 25,
      output_tokens: 7,
      total_tokens: 32,
      current: {
        input_tokens: 15,
        output_tokens: 4,
        total_tokens: 19,
        turn: 1,
        step: 2,
        response_id: "response-tool-2",
      },
    });
    assert.ok(stream.indexOf("event: usage") < stream.indexOf('"type":"tool_result"'));
    assert.ok(stream.lastIndexOf("event: usage") < stream.lastIndexOf('"status":"complete"'));
    const persistedResponses = (await readFile(join(sessionDir, "messages.jsonl"), "utf8"))
      .trimEnd()
      .split("\n")
      .map((line) => JSON.parse(line) as Record<string, unknown>)
      .filter((record) => record.type === "response.completed");
    assert.deepEqual(persistedResponses.map(({session_turn, session_step}) => [session_turn, session_step]), [
      [1, 1],
      [1, 2],
    ]);
    const incompleteResponse = persistedResponses[1]?.response as Record<string, unknown>;
    assert.equal(incompleteResponse.status, "incomplete");
    assert.deepEqual(incompleteResponse.incomplete_details, {reason: "max_output_tokens"});
  } finally {
    await running.close();
    await rm(root, {recursive: true, force: true});
  }
});

interface CollectedEvent {
  event: string;
  data: Record<string, unknown>;
}

function parseSseBlock(block: string): CollectedEvent | undefined {
  const event = block
    .split("\n")
    .find((line) => line.startsWith("event:"))
    ?.slice("event:".length)
    .trim();
  const data = block
    .split("\n")
    .find((line) => line.startsWith("data:"))
    ?.slice("data:".length)
    .trimStart();
  if (event === undefined || data === undefined || data.length === 0) {
    return undefined;
  }
  return {event, data: JSON.parse(data) as Record<string, unknown>};
}

/**
 * Reads a run stream while the test keeps producing provider events, so a test
 * can assert what a client sees at a specific moment of a run.
 */
class RunStreamReader {
  public readonly events: CollectedEvent[] = [];
  private readonly finished: Promise<void>;

  public constructor(response: Response) {
    this.finished = this.consume(response);
  }

  public async waitFor(event: string, timeoutMs = 4000): Promise<CollectedEvent> {
    const deadline = Date.now() + timeoutMs;
    while (Date.now() < deadline) {
      const found = this.events.find((candidate) => candidate.event === event);
      if (found !== undefined) {
        return found;
      }
      await new Promise((resolve) => setTimeout(resolve, 5));
    }
    throw new Error(
      `timed out waiting for ${event}; saw ${this.events.map((item) => item.event).join(",") || "nothing"}`,
    );
  }

  public async waitForEnd(timeoutMs = 4000): Promise<void> {
    await Promise.race([
      this.finished,
      new Promise((_resolve, reject) =>
        setTimeout(() => reject(new Error("stream did not end")), timeoutMs).unref?.(),
      ),
    ]);
  }

  private async consume(response: Response): Promise<void> {
    const reader = response.body!.getReader();
    const decoder = new TextDecoder();
    let buffer = "";
    while (true) {
      const chunk = await reader.read();
      buffer += decoder.decode(chunk.value, {stream: !chunk.done}).replace(/\r\n/g, "\n");
      let separator = buffer.indexOf("\n\n");
      while (separator >= 0) {
        const block = buffer.slice(0, separator);
        buffer = buffer.slice(separator + 2);
        const parsed = parseSseBlock(block);
        if (parsed !== undefined) {
          this.events.push(parsed);
        }
        separator = buffer.indexOf("\n\n");
      }
      if (chunk.done) {
        return;
      }
    }
  }
}

async function waitForCondition(
  predicate: () => boolean,
  description: string,
  timeoutMs = 4000,
): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    if (predicate()) {
      return;
    }
    await new Promise((resolve) => setTimeout(resolve, 5));
  }
  throw new Error(`timed out waiting for ${description}`);
}

interface ManualProviderStream {
  iterator: AsyncGenerator<unknown>;
  push: (event: unknown) => void;
  end: () => void;
  fail: (error: unknown) => void;
}

/**
 * A provider stream the test drives: events are pushed on demand, so a run can
 * be held open while another client attaches to it.
 */
function createManualProviderStream(): ManualProviderStream {
  const queue: unknown[] = [];
  let wake: (() => void) | undefined;
  let failure: unknown;
  let ended = false;

  const iterator = (async function* () {
    while (true) {
      if (queue.length > 0) {
        yield queue.shift();
        continue;
      }
      if (failure !== undefined) {
        throw failure;
      }
      if (ended) {
        return;
      }
      await new Promise<void>((resolve) => {
        wake = resolve;
      });
      wake = undefined;
    }
  })();

  const notify = (): void => {
    wake?.();
  };

  return {
    iterator,
    push: (event) => {
      queue.push(event);
      notify();
    },
    end: () => {
      ended = true;
      notify();
    },
    fail: (error) => {
      failure = error;
      notify();
    },
  };
}

/**
 * A session with two path profiles, each with its own transcript and its own
 * model config, and `primary` stored as the default.
 */
async function writeProfileSession(root: string): Promise<string> {
  const sessionDir = join(root, "session1");
  await mkdir(join(sessionDir, "primary"), {recursive: true});
  await mkdir(join(sessionDir, "alternate"), {recursive: true});
  await writeFile(
    join(sessionDir, "primary", "model.json"),
    JSON.stringify({
      schema_version: 1,
      provider: "fake",
      api_key_env: "FAKE_API_KEY",
      model: "primary-model",
    }),
  );
  await writeFile(
    join(sessionDir, "alternate", "model.json"),
    JSON.stringify({
      schema_version: 1,
      provider: "fake",
      api_key_env: "FAKE_API_KEY",
      model: "alternate-model",
    }),
  );
  await writeFile(
    join(sessionDir, "setting.json"),
    JSON.stringify({
      schema_version: 1,
      session_name: "profiles",
      cwd: sessionDir,
      paths: {
        active_profile: "primary",
        profiles: {
          primary: {
            messages: "profiles/primary/messages.jsonl",
            staging: "profiles/primary/staging",
            model_default: "primary/model.json",
            models: ["primary/model.json"],
          },
          alternate: {
            messages: "profiles/alternate/messages.jsonl",
            staging: "profiles/alternate/staging",
            model_default: "alternate/model.json",
            models: ["alternate/model.json"],
          },
        },
      },
    }),
  );
  return sessionDir;
}

/** One answer per provider request, so two runs can be driven independently. */
function createManualProviderStreams(): {
  streams: ManualProviderStream[];
  client: OpenAI;
  openResponses: Response[];
} {
  const streams: ManualProviderStream[] = [];
  const openResponses: Response[] = [];
  const client = {
    responses: {
      create: async () => {
        const stream = createManualProviderStream();
        streams.push(stream);
        return stream.iterator;
      },
    },
  } as unknown as OpenAI;
  return {streams, client, openResponses};
}

/**
 * Ends every provider stream and cancels any run response still being read.
 * A held SSE connection keeps `server.close()` waiting, so a failing assertion
 * must not leave one behind.
 */
async function closeRuns(
  streams: ManualProviderStream[],
  openResponses: Response[],
  running: {close: () => Promise<void>},
): Promise<void> {
  for (const stream of streams) {
    stream.end();
  }
  await Promise.allSettled(
    openResponses.map(async (response) => {
      await response.body?.cancel();
    }),
  );
  await running.close();
}

function assistantOutput(text: string): Record<string, unknown> {
  return {
    type: "message",
    role: "assistant",
    content: [{type: "output_text", text}],
  };
}

test("uses separate native memory stores for each session profile", async () => {
  const root = await mkdtemp(join(tmpdir(), "agent-harness-profile-memory-storage-"));
  const sessionDir = await writeProfileSession(root);
  const settingPath = join(sessionDir, "setting.json");
  const settings = JSON.parse(await readFile(settingPath, "utf8")) as Record<string, unknown>;
  const profiles = (settings.paths as {profiles: Record<string, Record<string, unknown>>}).profiles;
  profiles.primary!.tool_storage = "runtime/primary-tools";
  profiles.alternate!.tool_storage = "runtime/alternate-tools";
  settings.tool_definition_paths = [join(process.cwd(), "../sample/tools/native/memory.json")];
  settings.tools = {native: {memory: {storage_dir: "memory"}}};
  await writeFile(settingPath, JSON.stringify(settings));

  const callsByModel = new Map<string, number>();
  const client = {
    responses: {
      create: async (request: {model?: string}) => {
        const model = request.model ?? "unknown";
        const round = callsByModel.get(model) ?? 0;
        callsByModel.set(model, round + 1);
        const output = round % 2 === 0
          ? [{
              type: "function_call",
              call_id: `call-memory-${model}`,
              name: "memory",
              arguments: JSON.stringify({
                operation: "create",
                type: "profile_test",
                data: {model},
              }),
            }]
          : [assistantOutput(`stored memory for ${model}`)];
        return (async function* () {
          for (const [output_index, item] of output.entries()) {
            yield {type: "response.output_item.done", output_index, item};
          }
          yield completedEvent(output, `response-memory-${model}-${round}`);
        })();
      },
    },
  } as unknown as OpenAI;
  const running = createAgentServer({sessionsDir: root, client});
  await new Promise<void>((resolve, reject) => {
    running.server.once("error", reject);
    running.server.listen(0, "127.0.0.1", () => resolve());
  });

  try {
    const address = running.server.address();
    assert.ok(address && typeof address !== "string");
    const baseUrl = `http://127.0.0.1:${address.port}`;
    const primaryResponse = await fetch(`${baseUrl}/api/sessions/session1/run`, {
      method: "POST",
      headers: {"content-type": "application/json"},
      body: JSON.stringify({input: "store primary memory"}),
    });
    assert.equal(primaryResponse.status, 200);
    const primaryBody = await primaryResponse.text();
    assert.ok(primaryBody.includes("Memory record created"), primaryBody);

    const alternateResponse = await fetch(`${baseUrl}/api/sessions/session1/run?profile=alternate`, {
      method: "POST",
      headers: {"content-type": "application/json"},
      body: JSON.stringify({input: "store alternate memory"}),
    });
    assert.equal(alternateResponse.status, 200);
    const alternateBody = await alternateResponse.text();
    assert.ok(alternateBody.includes("Memory record created"), alternateBody);

    const primarySequence = join(sessionDir, "runtime", "primary-tools", "memory", ".memory-sequence");
    const alternateSequence = join(sessionDir, "runtime", "alternate-tools", "memory", ".memory-sequence");
    assert.equal(await readFile(primarySequence, "utf8"), "1\n");
    assert.equal(await readFile(alternateSequence, "utf8"), "1\n");
  } finally {
    await running.close();
    await rm(root, {recursive: true, force: true});
  }
});

test("resets only the selected profile row and rejects reset during its run", async () => {
  const root = await mkdtemp(join(tmpdir(), "agent-harness-reset-profile-"));
  const sessionDir = await writeProfileSession(root);
  const primaryMessagesPath = join(sessionDir, "profiles", "primary", "messages.jsonl");
  const alternateMessagesPath = join(sessionDir, "profiles", "alternate", "messages.jsonl");
  const alternateBefore = `${JSON.stringify({
    type: "message",
    role: "user",
    content: [{type: "input_text", text: "alternate before reset"}],
  })}\n`;
  await mkdir(join(sessionDir, "profiles", "alternate"), {recursive: true});
  await writeFile(alternateMessagesPath, alternateBefore);

  const {streams, client, openResponses} = createManualProviderStreams();
  const running = createAgentServer({sessionsDir: root, client});
  await new Promise<void>((resolve, reject) => {
    running.server.once("error", reject);
    running.server.listen(0, "127.0.0.1", () => resolve());
  });

  try {
    const address = running.server.address();
    assert.ok(address && typeof address !== "string");
    const baseUrl = `http://127.0.0.1:${address.port}`;
    const primaryRun = fetch(`${baseUrl}/api/sessions/session1/run`, {
      method: "POST",
      headers: {"content-type": "application/json"},
      body: JSON.stringify({input: "primary run stays"}),
    });
    await waitForCondition(() => streams.length === 1, "the primary run to reach the provider");
    const primaryResponse = await primaryRun;
    openResponses.push(primaryResponse);
    assert.equal(primaryResponse.status, 200);

    const runningReset = await fetch(`${baseUrl}/api/sessions/session1/reset?profile=primary`, {
      method: "POST",
    });
    assert.equal(runningReset.status, 409);

    const alternateReset = await fetch(`${baseUrl}/api/sessions/session1/reset?profile=alternate`, {
      method: "POST",
    });
    assert.equal(alternateReset.status, 200);
    assert.deepEqual(await alternateReset.json(), {reset: true});
    assert.equal(await readFile(alternateMessagesPath, "utf8"), "");
    await assert.rejects(readFile(alternateMessagesPath.replace("messages.jsonl", "messages.backup.jsonl"), "utf8"), {code: "ENOENT"});

    const primaryText = await readFile(primaryMessagesPath, "utf8");
    assert.match(primaryText, /primary run stays/);
    assert.equal(primaryText.includes("alternate before reset"), false);

    const alternateSnapshot = await fetch(`${baseUrl}/api/sessions/session1?profile=alternate`);
    assert.equal(alternateSnapshot.status, 200);
    const alternateBody = await alternateSnapshot.json() as {
      messages: Array<{text: string}>;
      usage: {turn_count: number};
      history?: {count: number; etag: string | null};
    };
    assert.deepEqual(alternateBody.messages, []);
    assert.equal(alternateBody.usage.turn_count, 0);
    assert.equal(alternateBody.history?.count ?? 0, 0);
    assert.equal(alternateBody.history?.etag ?? null, null);
  } finally {
    await closeRuns(streams, openResponses, running);
    await rm(root, {recursive: true, force: true});
  }
});

test("front compaction truncates only the selected profile and honors session settings", async () => {
  const root = await mkdtemp(join(tmpdir(), "agent-harness-front-compaction-profile-"));
  const sessionDir = await writeProfileSession(root);
  const settingPath = join(sessionDir, "setting.json");
  const settings = JSON.parse(await readFile(settingPath, "utf8")) as Record<string, unknown>;
  settings.front_compaction = {enabled: true, delete_mode: "recoverable"};
  await writeFile(settingPath, JSON.stringify(settings));

  const primaryMessagesPath = join(sessionDir, "profiles", "primary", "messages.jsonl");
  const alternateMessagesPath = join(sessionDir, "profiles", "alternate", "messages.jsonl");
  const user = (text: string): Record<string, unknown> => ({
    type: "message",
    role: "user",
    content: [{type: "input_text", text}],
  });
  const assistantEvent = (id: string, text: string): Record<string, unknown> => ({
    type: "response.completed",
    response: {
      id,
      object: "response",
      status: "completed",
      model: "fake-model",
      usage: {input_tokens: 10, output_tokens: 2},
      output: [{type: "message", role: "assistant", content: [{type: "output_text", text}]}],
    },
  });
  const primaryTranscript = `${JSON.stringify(user("primary stays"))}\n${JSON.stringify(assistantEvent("primary-response", "primary answer"))}\n`;
  const alternateRecords = [
    user("alternate stays"),
    assistantEvent("alternate-one", "alternate answer one"),
    user("alternate removed"),
    assistantEvent("alternate-two", "alternate answer two"),
  ];
  const alternateTranscript = `${alternateRecords.map(record => JSON.stringify(record)).join("\n")}\n`;
  const preservedAlternatePrefix = `${alternateRecords.slice(0, 2).map(record => JSON.stringify(record)).join("\n")}\n`;
  const removedAlternateTail = alternateTranscript.slice(Buffer.byteLength(preservedAlternatePrefix, "utf8"));
  const removedUserOffset = Buffer.byteLength(preservedAlternatePrefix, "utf8");
  await mkdir(join(sessionDir, "profiles", "primary"), {recursive: true});
  await mkdir(join(sessionDir, "profiles", "alternate"), {recursive: true});
  await writeFile(primaryMessagesPath, primaryTranscript);
  await writeFile(alternateMessagesPath, alternateTranscript);

  const {client} = createManualProviderStreams();
  const running = createAgentServer({sessionsDir: root, client});
  await new Promise<void>((resolve, reject) => {
    running.server.once("error", reject);
    running.server.listen(0, "127.0.0.1", () => resolve());
  });

  try {
    const address = running.server.address();
    assert.ok(address && typeof address !== "string");
    const baseUrl = `http://127.0.0.1:${address.port}`;
    const httpResponse = await fetch(`${baseUrl}/api/sessions/session1/front-compact?profile=alternate`, {
      method: "POST",
      headers: {"content-type": "application/json"},
      body: JSON.stringify({message_id: `session-user-o${removedUserOffset}`}),
    });
    const responseText = await httpResponse.text();
    assert.equal(httpResponse.status, 200, responseText);
    const result = JSON.parse(responseText) as {
      front_compaction: {delete_mode: string; archive_name?: string};
    };
    assert.equal(result.front_compaction.delete_mode, "recoverable");
    assert.match(result.front_compaction.archive_name ?? "", /^messages-\d{8}T\d{9}Z-[0-9a-f]{8}\.jsonl$/);
    assert.equal(await readFile(alternateMessagesPath, "utf8"), preservedAlternatePrefix);
    assert.equal(await readFile(primaryMessagesPath, "utf8"), primaryTranscript);
    assert.equal(
      await readFile(join(`${alternateMessagesPath}.deleted-history`, result.front_compaction.archive_name!), "utf8"),
      removedAlternateTail,
    );

    settings.front_compaction = {enabled: false, delete_mode: "permanent"};
    await writeFile(settingPath, JSON.stringify(settings));
    const disabledResponse = await fetch(`${baseUrl}/api/sessions/session1/front-compact?profile=primary`, {
      method: "POST",
      headers: {"content-type": "application/json"},
      body: JSON.stringify({message_id: "session-user-o0"}),
    });
    assert.equal(disabledResponse.status, 403);
    assert.equal(await readFile(primaryMessagesPath, "utf8"), primaryTranscript);
  } finally {
    await running.close();
    await rm(root, {recursive: true, force: true});
  }
});

test("runs two profiles of one session at the same time, on separate transcripts", async () => {
  const root = await mkdtemp(join(tmpdir(), "agent-harness-profiles-"));
  const sessionDir = await writeProfileSession(root);
  const {streams, client, openResponses} = createManualProviderStreams();
  const running = createAgentServer({sessionsDir: root, client, conditionalRequests: true});
  await new Promise<void>((resolve, reject) => {
    running.server.once("error", reject);
    running.server.listen(0, "127.0.0.1", () => resolve());
  });

  try {
    const address = running.server.address();
    assert.ok(address && typeof address !== "string");
    const baseUrl = `http://127.0.0.1:${address.port}`;
    const runUrl = `${baseUrl}/api/sessions/session1/run`;
    const post = (url: string, input: string): Promise<Response> =>
      fetch(url, {
        method: "POST",
        headers: {"content-type": "application/json"},
        body: JSON.stringify({input}),
      });

    // The stored default (primary) and the other profile run concurrently: the
    // second is accepted while the first is still in flight.
    const primaryRequest = post(runUrl, "primary input");
    await waitForCondition(() => streams.length === 1, "the primary run to reach the provider");
    const alternateRequest = post(`${runUrl}?profile=alternate`, "alternate input");
    await waitForCondition(() => streams.length === 2, "the alternate run to reach the provider");
    const [primaryResponse, alternateResponse] = await Promise.all([
      primaryRequest,
      alternateRequest,
    ]);
    openResponses.push(primaryResponse, alternateResponse);
    assert.equal(primaryResponse.status, 200);
    assert.equal(alternateResponse.status, 200);

    streams[0]!.push({type: "response.output_text.delta", delta: "PRIMARY_TEXT"});
    streams[1]!.push({type: "response.output_text.delta", delta: "ALTERNATE_TEXT"});

    // Both runs are registered under their own profile while both are in flight.
    const listResponse = await fetch(`${baseUrl}/api/sessions`);
    const list = (await listResponse.json()) as {
      sessions: Array<{
        id: string;
        active_profile?: string;
        run?: {status: string; turn_id: string};
        runs_by_profile?: Record<string, {status: string; turn_id: string}>;
      }>;
    };
    const listed = list.sessions.find((session) => session.id === "session1");
    assert.equal(listed?.active_profile, "primary");
    assert.equal(listed?.run?.status, "running");
    assert.equal(listed?.runs_by_profile?.primary?.status, "running");
    assert.equal(listed?.runs_by_profile?.alternate?.status, "running");
    assert.notEqual(listed?.run?.turn_id, listed?.runs_by_profile?.alternate?.turn_id);

    // Finishing one profile leaves the other streaming.
    streams[0]!.push({type: "response.output_item.done", output_index: 0, item: assistantOutput("PRIMARY_TEXT")});
    streams[0]!.push(completedEvent([assistantOutput("PRIMARY_TEXT")], "resp_primary"));
    streams[0]!.end();
    const primaryStream = await (await primaryResponse).text();
    assert.match(primaryStream, /PRIMARY_TEXT/);
    const midway = await (await fetch(`${baseUrl}/api/sessions`)).json() as {
      sessions: Array<{run?: {status: string}; runs_by_profile?: Record<string, {status: string}>}>;
    };
    assert.equal(midway.sessions[0]?.run?.status, "completed");
    assert.equal(midway.sessions[0]?.runs_by_profile?.alternate?.status, "running");

    const halfwaySnapshotResponse = await fetch(`${baseUrl}/api/sessions/session1`);
    assert.equal(halfwaySnapshotResponse.status, 200);
    const halfwaySnapshotEtag = halfwaySnapshotResponse.headers.get("etag");
    assert.ok(halfwaySnapshotEtag);
    const halfwaySnapshot = await halfwaySnapshotResponse.json() as {
      runs_by_profile?: Record<string, {status: string}>;
    };
    assert.equal(halfwaySnapshot.runs_by_profile?.alternate?.status, "running");

    // Each profile's transcript holds its own turn and nothing of the other's.
    const primarySnapshot = await (
      await fetch(`${baseUrl}/api/sessions/session1?profile=primary`)
    ).json() as {messages: Array<{text: string}>; active_profile?: string; default_profile?: string};
    assert.equal(primarySnapshot.active_profile, "primary");
    assert.equal(primarySnapshot.default_profile, "primary");
    assert.deepEqual(
      primarySnapshot.messages.map((message) => message.text),
      ["primary input", "PRIMARY_TEXT"],
    );
    const primaryTranscript = await readFile(
      join(sessionDir, "profiles/primary/messages.jsonl"),
      "utf8",
    );
    assert.match(primaryTranscript, /primary input/);
    assert.doesNotMatch(primaryTranscript, /alternate input/);

    streams[1]!.push({type: "response.output_item.done", output_index: 0, item: assistantOutput("ALTERNATE_TEXT")});
    streams[1]!.push(completedEvent([assistantOutput("ALTERNATE_TEXT")], "resp_alternate"));
    streams[1]!.end();
    const alternateStream = await (await alternateResponse).text();
    assert.match(alternateStream, /ALTERNATE_TEXT/);

    const completedRunSnapshotResponse = await fetch(`${baseUrl}/api/sessions/session1`, {
      headers: {"if-none-match": halfwaySnapshotEtag},
    });
    assert.equal(completedRunSnapshotResponse.status, 200);
    const completedRunSnapshot = await completedRunSnapshotResponse.json() as {
      runs_by_profile?: Record<string, {status: string}>;
    };
    assert.equal(completedRunSnapshot.runs_by_profile?.alternate?.status, "completed");

    const alternateSnapshot = await (
      await fetch(`${baseUrl}/api/sessions/session1?profile=alternate`)
    ).json() as {messages: Array<{text: string}>; active_profile?: string; default_profile?: string};
    assert.equal(alternateSnapshot.active_profile, "alternate");
    assert.equal(alternateSnapshot.default_profile, "primary");
    assert.deepEqual(
      alternateSnapshot.messages.map((message) => message.text),
      ["alternate input", "ALTERNATE_TEXT"],
    );
    const alternateTranscript = await readFile(
      join(sessionDir, "profiles/alternate/messages.jsonl"),
      "utf8",
    );
    assert.match(alternateTranscript, /alternate input/);
    assert.doesNotMatch(alternateTranscript, /primary input/);

    // The unqualified snapshot is still the stored default's transcript.
    const defaultSnapshot = await (
      await fetch(`${baseUrl}/api/sessions/session1`)
    ).json() as {messages: Array<{text: string}>};
    assert.deepEqual(
      defaultSnapshot.messages.map((message) => message.text),
      ["primary input", "PRIMARY_TEXT"],
    );
  } finally {
    await closeRuns(streams, openResponses, running);
    await rm(root, {recursive: true, force: true});
  }
});

test("switching the stored profile refuses only the profile being switched away from", async () => {
  const root = await mkdtemp(join(tmpdir(), "agent-harness-profile-switch-"));
  const sessionDir = await writeProfileSession(root);
  const {streams, client, openResponses} = createManualProviderStreams();
  const running = createAgentServer({sessionsDir: root, client});
  await new Promise<void>((resolve, reject) => {
    running.server.once("error", reject);
    running.server.listen(0, "127.0.0.1", () => resolve());
  });

  try {
    const address = running.server.address();
    assert.ok(address && typeof address !== "string");
    const baseUrl = `http://127.0.0.1:${address.port}`;
    const switchTo = (activeProfile: string): Promise<Response> =>
      fetch(`${baseUrl}/api/sessions/session1/profile`, {
        method: "POST",
        headers: {"content-type": "application/json"},
        body: JSON.stringify({active_profile: activeProfile}),
      });

    // A run in `alternate`, which is not the stored default.
    const heldRequest = fetch(`${baseUrl}/api/sessions/session1/run?profile=alternate`, {
      method: "POST",
      headers: {"content-type": "application/json"},
      body: JSON.stringify({input: "held alternate input"}),
    });
    await waitForCondition(() => streams.length === 1, "the held run to reach the provider");

    // Moving the stored default to `alternate` is allowed: neither profile's
    // handle is disturbed, so the run keeps going.
    const switched = await switchTo("alternate");
    assert.equal(switched.status, 200);
    const switchedBody = await switched.json() as {active_profile: string; changed: boolean};
    assert.equal(switchedBody.active_profile, "alternate");
    assert.equal(switchedBody.changed, true);
    const heldResponse = await heldRequest;
    assert.equal(heldResponse.status, 200);
    openResponses.push(heldResponse);

    const afterSwitch = JSON.parse(await readFile(join(sessionDir, "setting.json"), "utf8")) as {
      paths: {active_profile: string};
    };
    assert.equal(afterSwitch.paths.active_profile, "alternate");
    // The unqualified view follows the new default without a reopen, and the
    // held run is still attached to the same turn.
    const list = await (await fetch(`${baseUrl}/api/sessions`)).json() as {
      sessions: Array<{active_profile?: string; run?: {status: string}}>;
    };
    assert.equal(list.sessions[0]?.active_profile, "alternate");
    assert.equal(list.sessions[0]?.run?.status, "running");

    // Switching away from a profile with a run in flight is still refused: that
    // handle is the one whose paths would move under the run.
    const refused = await switchTo("primary");
    assert.equal(refused.status, 409);
    const aliasRefused = await fetch(`${baseUrl}/api/sessions/default%3Asession1/profile`, {
      method: "POST",
      headers: {"content-type": "application/json"},
      body: JSON.stringify({active_profile: "primary"}),
    });
    assert.equal(aliasRefused.status, 409, "root-qualified alias must see the same running profile");
    const afterRefusal = JSON.parse(await readFile(join(sessionDir, "setting.json"), "utf8")) as {
      paths: {active_profile: string};
    };
    assert.equal(afterRefusal.paths.active_profile, "alternate");

    streams[0]!.push({type: "response.output_item.done", output_index: 0, item: assistantOutput("ALTERNATE_DONE")});
    streams[0]!.push(completedEvent([assistantOutput("ALTERNATE_DONE")], "resp_held"));
    streams[0]!.end();
    const heldStream = await (await heldResponse).text();
    assert.match(heldStream, /ALTERNATE_DONE/);

    // With nothing running, the switch back is allowed.
    const beforeAliasSwitch = await (await fetch(`${baseUrl}/api/sessions/session1`)).json() as {active_profile?: string};
    assert.equal(beforeAliasSwitch.active_profile, "alternate");
    const back = await fetch(`${baseUrl}/api/sessions/default%3Asession1/profile`, {
      method: "POST",
      headers: {"content-type": "application/json"},
      body: JSON.stringify({active_profile: "primary"}),
    });
    assert.equal(back.status, 200);
    const backBody = await back.json() as {changed: boolean};
    assert.equal(backBody.changed, true);
    const afterAliasSwitch = await (await fetch(`${baseUrl}/api/sessions/session1`)).json() as {active_profile?: string};
    assert.equal(afterAliasSwitch.active_profile, "primary", "canonical default handle must be invalidated by alias switch");
  } finally {
    await closeRuns(streams, openResponses, running);
    await rm(root, {recursive: true, force: true});
  }
});

test("serializes concurrent stored-profile setting updates", async () => {
  const root = await mkdtemp(join(tmpdir(), "agent-harness-profile-setting-race-"));
  const sessionDir = await writeProfileSession(root);
  const running = createAgentServer({sessionsDir: root});
  await new Promise<void>((resolve, reject) => {
    running.server.once("error", reject);
    running.server.listen(0, "127.0.0.1", () => resolve());
  });

  try {
    const address = running.server.address();
    assert.ok(address && typeof address !== "string");
    const profileUrl = `http://127.0.0.1:${address.port}/api/sessions/session1/profile`;
    const update = (profile: string): Promise<Response> => fetch(profileUrl, {
      method: "POST",
      headers: {"content-type": "application/json"},
      body: JSON.stringify({active_profile: profile}),
    });
    const responses = await Promise.all([update("alternate"), update("primary")]);
    assert.deepEqual(responses.map((response) => response.status).sort(), [200, 409]);
    const successfulIndex = responses.findIndex((response) => response.status === 200);
    const successfulBody = await responses[successfulIndex]!.json() as {active_profile: string};
    const settings = JSON.parse(await readFile(join(sessionDir, "setting.json"), "utf8")) as {
      paths: {active_profile: string};
    };
    assert.equal(settings.paths.active_profile, successfulBody.active_profile);
  } finally {
    await running.close();
    await rm(root, {recursive: true, force: true});
  }
});

test("addresses one profile's model and settings through the profile parameter", async () => {
  const root = await mkdtemp(join(tmpdir(), "agent-harness-profile-address-"));
  await writeProfileSession(root);
  const running = createAgentServer({sessionsDir: root});
  await new Promise<void>((resolve, reject) => {
    running.server.once("error", reject);
    running.server.listen(0, "127.0.0.1", () => resolve());
  });

  try {
    const address = running.server.address();
    assert.ok(address && typeof address !== "string");
    const baseUrl = `http://127.0.0.1:${address.port}`;

    const primary = await (await fetch(`${baseUrl}/api/sessions/session1`)).json() as {
      active_profile?: string;
      default_profile?: string;
      path_profiles?: string[];
      model?: {model: string};
      model_default?: string;
    };
    assert.equal(primary.active_profile, "primary");
    assert.equal(primary.default_profile, "primary");
    assert.deepEqual(primary.path_profiles, ["primary", "alternate"]);
    assert.equal(primary.model?.model, "primary-model");
    assert.equal(primary.model_default, "primary/model.json");

    const alternate = await (
      await fetch(`${baseUrl}/api/sessions/session1?profile=alternate`)
    ).json() as {
      active_profile?: string;
      default_profile?: string;
      model?: {model: string};
      model_default?: string;
    };
    assert.equal(alternate.active_profile, "alternate");
    // The stored default is reported beside the resolved profile.
    assert.equal(alternate.default_profile, "primary");
    assert.equal(alternate.model?.model, "alternate-model");
    assert.equal(alternate.model_default, "alternate/model.json");

    // The list keeps reporting the stored default's model and profile.
    const list = await (await fetch(`${baseUrl}/api/sessions`)).json() as {
      sessions: Array<{active_profile?: string; model?: {model: string}; runs_by_profile?: unknown}>;
    };
    assert.equal(list.sessions[0]?.active_profile, "primary");
    assert.equal(list.sessions[0]?.model?.model, "primary-model");
    assert.equal(list.sessions[0]?.runs_by_profile, undefined);

    // A profile the session does not configure cannot be addressed, and a
    // malformed name never reaches the config.
    const unknown = await fetch(`${baseUrl}/api/sessions/session1?profile=missing`);
    assert.equal(unknown.status, 422);
    const malformed = await fetch(`${baseUrl}/api/sessions/session1?profile=bad%20name`);
    assert.equal(malformed.status, 400);

    // A pinned request may select the model inside its own profile.
    const modelDefault = await fetch(
      `${baseUrl}/api/sessions/session1/model-default?profile=alternate`,
      {
        method: "POST",
        headers: {"content-type": "application/json"},
        body: JSON.stringify({model_default: "alternate/model.json"}),
      },
    );
    assert.equal(modelDefault.status, 200);
    const modelDefaultBody = await modelDefault.json() as {
      active_profile: string;
      model_default: string;
      changed: boolean;
    };
    assert.equal(modelDefaultBody.active_profile, "alternate");
    assert.equal(modelDefaultBody.model_default, "alternate/model.json");
    assert.equal(modelDefaultBody.changed, false);
  } finally {
    await running.close();
    await rm(root, {recursive: true, force: true});
  }
});

async function writePlainSession(root: string): Promise<string> {
  const sessionDir = join(root, "session1");
  await mkdir(sessionDir, {recursive: true});
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
      session_name: "server-run-test",
      cwd: sessionDir,
    }),
  );
  return sessionDir;
}

test("a client that attaches mid-run sees the current snapshot and the completion", async () => {
  const root = await mkdtemp(join(tmpdir(), "agent-harness-attach-"));
  await writePlainSession(root);

  const streams: ManualProviderStream[] = [];
  const fakeClient = {
    responses: {
      create: async () => {
        const stream = createManualProviderStream();
        streams.push(stream);
        return stream.iterator;
      },
    },
  } as unknown as OpenAI;

  const running = createAgentServer({
    sessionsDir: root,
    client: fakeClient,
    runStateRetentionMs: 30_000,
  });
  await new Promise<void>((resolve, reject) => {
    running.server.once("error", reject);
    running.server.listen(0, "127.0.0.1", () => resolve());
  });

  try {
    const address = running.server.address();
    assert.ok(address && typeof address !== "string");
    const baseUrl = `http://127.0.0.1:${address.port}`;

    const runResponse = await fetch(`${baseUrl}/api/sessions/session1/run`, {
      method: "POST",
      headers: {"content-type": "application/json"},
      body: JSON.stringify({input: "hello"}),
    });
    assert.equal(runResponse.status, 200);
    const runReader = new RunStreamReader(runResponse);
    await waitForCondition(() => streams.length === 1, "the provider stream");

    const provider = streams[0]!;
    provider.push({type: "response.output_text.delta", delta: "half "});
    await runReader.waitFor("ui.message");
    assert.equal(
      (runReader.events.find((item) => item.event === "ui.message")!.data.message as Record<string, unknown>).text,
      "half ",
    );

    // A second client (the reloaded page) discovers the run from the snapshot…
    const snapshot = await fetch(`${baseUrl}/api/sessions/session1`);
    const snapshotBody = (await snapshot.json()) as Record<string, unknown>;
    const run = snapshotBody.run as Record<string, unknown> | undefined;
    assert.ok(run !== undefined, "the snapshot must report the run in flight");
    assert.equal(run.status, "running");
    assert.equal(typeof run.turn_id, "string");

    // …and attaches to it, starting with the message as it stands right now.
    const attachResponse = await fetch(
      `${baseUrl}/api/sessions/session1/run/stream`,
    );
    assert.equal(attachResponse.status, 200);
    const attachReader = new RunStreamReader(attachResponse);
    const attached = await attachReader.waitFor("ui.message");
    assert.equal((attached.data.message as Record<string, unknown>).status, "streaming");
    assert.equal((attached.data.message as Record<string, unknown>).text, "half ");
    assert.equal(
      (attachReader.events[0]!.data as Record<string, unknown>).turn_id,
      run.turn_id,
    );

    // The rest of the run reaches both clients.
    provider.push({type: "response.output_text.delta", delta: "answer"});
    const answerItem = {
      type: "message",
      role: "assistant",
      content: [{type: "output_text", text: "half answer"}],
    };
    provider.push({type: "response.output_item.done", output_index: 0, item: answerItem});
    provider.push(completedEvent([answerItem], "response-attach"));
    provider.end();
    const completed = await attachReader.waitFor("completed");
    assert.equal(
      ((completed.data.message as Record<string, unknown>).text),
      "half answer",
    );
    await attachReader.waitForEnd();
    await runReader.waitForEnd();
    assert.deepEqual(
      attachReader.events.map((item) => item.event).filter((event) => event === "done"),
      ["done"],
    );

    const after = (await (await fetch(`${baseUrl}/api/sessions/session1`)).json()) as Record<string, unknown>;
    assert.equal((after.run as Record<string, unknown>).status, "completed");
    assert.equal(after.turns, 1);
  } finally {
    await running.close();
    await rm(root, {recursive: true, force: true});
  }
});

test("an attaching client receives the retained completion after a reload", async () => {
  const root = await mkdtemp(join(tmpdir(), "agent-harness-late-attach-"));
  await writePlainSession(root);

  const fakeClient = {
    responses: {
      create: async () =>
        (async function* () {
          const item = {
            type: "message",
            role: "assistant",
            content: [{type: "output_text", text: "done "}],
          };
          yield {type: "response.output_text.delta", delta: "done "};
          yield {type: "response.output_item.done", output_index: 0, item};
          yield completedEvent([item], "response-late");
        })(),
    },
  } as unknown as OpenAI;

  const running = createAgentServer({
    sessionsDir: root,
    client: fakeClient,
    runStateRetentionMs: 30_000,
  });
  await new Promise<void>((resolve, reject) => {
    running.server.once("error", reject);
    running.server.listen(0, "127.0.0.1", () => resolve());
  });

  try {
    const address = running.server.address();
    assert.ok(address && typeof address !== "string");
    const baseUrl = `http://127.0.0.1:${address.port}`;

    const runReader = new RunStreamReader(
      await fetch(`${baseUrl}/api/sessions/session1/run`, {
        method: "POST",
        headers: {"content-type": "application/json"},
        body: JSON.stringify({input: "hello"}),
      }),
    );
    await runReader.waitForEnd();

    const attachReader = new RunStreamReader(
      await fetch(`${baseUrl}/api/sessions/session1/run/stream`),
    );
    const replayed = await attachReader.waitFor("completed");
    assert.equal(
      ((replayed.data.message as Record<string, unknown>).text),
      "done ",
    );
    await attachReader.waitForEnd();
    assert.equal(attachReader.events[0]!.event, "run");
    assert.equal(
      (attachReader.events[0]!.data as Record<string, unknown>).status,
      "completed",
    );
  } finally {
    await running.close();
    await rm(root, {recursive: true, force: true});
  }
});

test("a session with no run answers idle and a fresh session loads without a transcript", async () => {
  const root = await mkdtemp(join(tmpdir(), "agent-harness-idle-"));
  const sessionDir = await writePlainSession(root);

  const running = createAgentServer({sessionsDir: root});
  await new Promise<void>((resolve, reject) => {
    running.server.once("error", reject);
    running.server.listen(0, "127.0.0.1", () => resolve());
  });

  try {
    const address = running.server.address();
    assert.ok(address && typeof address !== "string");
    const baseUrl = `http://127.0.0.1:${address.port}`;

    const attachReader = new RunStreamReader(
      await fetch(`${baseUrl}/api/sessions/session1/run/stream`),
    );
    await attachReader.waitForEnd();
    assert.deepEqual(attachReader.events.map((item) => item.event), ["idle"]);

    // A session that has never committed a response has no messages.jsonl; the
    // snapshot reports an empty transcript instead of failing.
    const snapshot = await fetch(`${baseUrl}/api/sessions/session1`);
    assert.equal(snapshot.status, 200);
    const body = (await snapshot.json()) as Record<string, unknown>;
    assert.equal(body.turns, 0);
    assert.deepEqual(body.messages, []);
    assert.equal(body.run, undefined);
    assert.equal(
      (await readFile(join(sessionDir, "messages.jsonl"), "utf8")),
      "",
    );

    const stopped = await fetch(`${baseUrl}/api/sessions/session1/run/stop`, {method: "POST"});
    assert.equal(stopped.status, 200);
    assert.deepEqual(await stopped.json(), {stop: {stopped: false}});
  } finally {
    await running.close();
    await rm(root, {recursive: true, force: true});
  }
});

test("stop aborts the run in flight and commits no response for it", async () => {
  const root = await mkdtemp(join(tmpdir(), "agent-harness-stop-"));
  const sessionDir = await writePlainSession(root);

  const streams: ManualProviderStream[] = [];
  const fakeClient = {
    responses: {
      create: async (_params: unknown, options?: {signal?: AbortSignal}) => {
        const stream = createManualProviderStream();
        streams.push(stream);
        options?.signal?.addEventListener(
          "abort",
          () => stream.fail(new Error("aborted")),
          {once: true},
        );
        return stream.iterator;
      },
    },
  } as unknown as OpenAI;

  const running = createAgentServer({
    sessionsDir: root,
    client: fakeClient,
    runStateRetentionMs: 30_000,
  });
  await new Promise<void>((resolve, reject) => {
    running.server.once("error", reject);
    running.server.listen(0, "127.0.0.1", () => resolve());
  });

  try {
    const address = running.server.address();
    assert.ok(address && typeof address !== "string");
    const baseUrl = `http://127.0.0.1:${address.port}`;

    const runReader = new RunStreamReader(
      await fetch(`${baseUrl}/api/sessions/session1/run`, {
        method: "POST",
        headers: {"content-type": "application/json"},
        body: JSON.stringify({input: "keep going"}),
      }),
    );
    await waitForCondition(() => streams.length === 1, "the provider stream");
    streams[0]!.push({type: "response.output_text.delta", delta: "half "});
    await runReader.waitFor("ui.message");

    const attachReader = new RunStreamReader(
      await fetch(`${baseUrl}/api/sessions/session1/run/stream`),
    );
    await attachReader.waitFor("ui.message");

    const stopResponse = await fetch(`${baseUrl}/api/sessions/session1/run/stop`, {method: "POST"});
    assert.equal(stopResponse.status, 200);
    const stopped = (await stopResponse.json()) as Record<string, unknown>;
    const stop = stopped.stop as Record<string, unknown>;
    assert.equal(stop.stopped, true);
    assert.equal(typeof stop.turn_id, "string");

    await runReader.waitForEnd();
    await attachReader.waitForEnd();
    assert.deepEqual(
      attachReader.events.map((item) => item.event),
      ["run", "ui.message", "error", "done"],
    );

    const after = (await (await fetch(`${baseUrl}/api/sessions/session1`)).json()) as Record<string, unknown>;
    assert.equal((after.run as Record<string, unknown>).status, "aborted");
    // Turns count user submissions, so the stopped round still counts as one
    // even though no response was committed for it.
    assert.equal(after.turns, 1);
    // The user's own input stays in the transcript; only the assistant response
    // for the stopped round is missing.
    const afterMessages = after.messages as Record<string, unknown>[];
    assert.deepEqual(afterMessages.map((item) => item.sender), ["user"]);
    assert.equal(afterMessages[0]!.text, "keep going");

    // Nothing was committed for the stopped round.
    const transcript = await readFile(join(sessionDir, "messages.jsonl"), "utf8");
    assert.equal(
      transcript
        .split("\n")
        .filter((line) => line.includes('"type":"response.completed"'))
        .length,
      0,
    );
    // A stop is the user's own action, not a failure, so it leaves no failure record.
    await assert.rejects(
      () => readFile(join(sessionDir, "staging", "run-error-cache.json"), "utf8"),
      {code: "ENOENT"},
    );
    assert.equal(Object.hasOwn(after, "failures"), false);
  } finally {
    await running.close();
    await rm(root, {recursive: true, force: true});
  }
});

/** A session whose first provider response requests one slow bash call per entry. */
async function writeParallelBashSession(
  root: string,
  sleeps: readonly number[],
  settings: Record<string, unknown> = {},
): Promise<{sessionDir: string; client: OpenAI}> {
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
      session_name: "parallel-tools",
      cwd: sessionDir,
      tool_definition_paths: ["bash.json"],
      tools: {native: {bash: {}}},
      ...settings,
    }),
  );
  await writeFile(join(root, "bash.json"), JSON.stringify({schema_version: 1, tools: [nativeBashDefinition]}));
  const calls = sleeps.map((seconds, index) => ({
    id: `fc_${index}`,
    type: "function_call",
    call_id: `call_${index}`,
    name: "bash",
    arguments: JSON.stringify({command: `sleep ${seconds}; echo finished-${index}`}),
  }));
  let responseNumber = 0;
  const client = {
    responses: {
      create: async () => {
        responseNumber += 1;
        if (responseNumber === 1) {
          return (async function* () {
            for (const [index, call] of calls.entries()) {
              yield {type: "response.output_item.added", output_index: index, item: {...call, arguments: ""}};
              yield {type: "response.output_item.done", output_index: index, item: call};
            }
            yield completedEvent(calls, "response-parallel-calls");
          })();
        }
        const finalMessage = {type: "message", role: "assistant", content: [{type: "output_text", text: "all finished"}]};
        return (async function* () {
          yield {type: "response.output_item.done", output_index: 0, item: finalMessage};
          yield completedEvent([finalMessage], "response-parallel-final");
        })();
      },
    },
  } as unknown as OpenAI;
  return {sessionDir, client};
}

/** Frames of a run stream with the time each one arrived, relative to the first. */
async function timedRunFrames(response: Response): Promise<{event: string; data: Record<string, unknown>; at: number}[]> {
  const frames: {event: string; data: Record<string, unknown>; at: number}[] = [];
  const reader = response.body!.getReader();
  const decoder = new TextDecoder();
  const started = Date.now();
  let buffer = "";
  while (true) {
    const {value, done} = await reader.read();
    if (done) break;
    buffer += decoder.decode(value, {stream: true});
    let separator = buffer.indexOf("\n\n");
    while (separator >= 0) {
      const block = buffer.slice(0, separator);
      buffer = buffer.slice(separator + 2);
      const event = /^event: (.*)$/m.exec(block)?.[1];
      const data = /^data: (.*)$/m.exec(block)?.[1];
      if (event !== undefined && data !== undefined) {
        frames.push({event, data: JSON.parse(data) as Record<string, unknown>, at: Date.now() - started});
      }
      separator = buffer.indexOf("\n\n");
    }
  }
  return frames;
}

test("streams each parallel tool result as its call finishes, without duplicates", async () => {
  const root = await mkdtemp(join(tmpdir(), "agent-harness-parallel-tools-"));
  // Call 0 is the slowest: results must follow completion order, not call order.
  const {client} = await writeParallelBashSession(root, [1.2, 0.2, 0.6, 0.4], {tool_call_scheduling: "concurrent"});
  const running = createAgentServer({sessionsDir: root, client, toolStreamIntervalMs: 0});
  await new Promise<void>((resolve, reject) => {
    running.server.once("error", reject);
    running.server.listen(0, "127.0.0.1", () => resolve());
  });
  try {
    const address = running.server.address();
    assert.ok(address && typeof address !== "string");
    const response = await fetch(`http://127.0.0.1:${address.port}/api/sessions/session1/run`, {
      method: "POST",
      headers: {"content-type": "application/json"},
      body: JSON.stringify({input: "run four commands"}),
    });
    assert.equal(response.status, 200);
    const frames = await timedRunFrames(response);

    const results = frames
      .filter((frame) => frame.event === "ui.part" && (frame.data.part as {type?: string}).type === "tool_result")
      .map((frame) => ({callId: (frame.data.part as {callId: string}).callId, at: frame.at}));
    assert.equal(results.length, 4, `one result event per call, saw ${JSON.stringify(results)}`);
    assert.deepEqual(
      results.map((result) => result.callId),
      ["call_1", "call_3", "call_2", "call_0"],
      "results arrive in completion order",
    );
    // They are spread over the run instead of arriving as one burst at the end.
    assert.ok(
      results[3]!.at - results[0]!.at >= 600,
      `results should be spread out, saw ${JSON.stringify(results)}`,
    );

    // Replaying the stream yields one result per call in the final message, and
    // the completed message is the persisted, call-ordered one.
    const final = runStreamStates(
      frames.map((frame) => `event: ${frame.event}\ndata: ${JSON.stringify(frame.data)}\n\n`).join(""),
    ).at(-1)!;
    const resultParts = final.parts.filter((part) => part.type === "tool_result");
    assert.deepEqual(
      resultParts.map((part) => (part as {callId: string}).callId).sort(),
      ["call_0", "call_1", "call_2", "call_3"],
    );
    // The completed message holds each result exactly once. The UI pairs a
    // result with its call by call id, so the order of the result parts is
    // not significant (the persisted transcript keeps call order).
    const completed = frames.find((frame) => frame.event === "completed")!.data.message as WebUiMessage;
    assert.deepEqual(
      completed.parts
        .filter((part) => part.type === "tool_result")
        .map((part) => (part as {callId: string}).callId)
        .sort(),
      ["call_0", "call_1", "call_2", "call_3"],
    );
    assert.equal(frames.at(-1)!.event, "done");
  } finally {
    await running.close();
    await rm(root, {recursive: true, force: true});
  }
});

test("the default ordered scheduling runs state-changing calls one after another in the model's order", async () => {
  const root = await mkdtemp(join(tmpdir(), "agent-harness-ordered-tools-"));
  // Call 0 is the slowest: with concurrent scheduling it would finish last.
  const {client} = await writeParallelBashSession(root, [0.6, 0.2, 0.2]);
  const running = createAgentServer({sessionsDir: root, client, toolStreamIntervalMs: 0});
  await new Promise<void>((resolve, reject) => {
    running.server.once("error", reject);
    running.server.listen(0, "127.0.0.1", () => resolve());
  });
  try {
    const address = running.server.address();
    assert.ok(address && typeof address !== "string");
    const started = Date.now();
    const response = await fetch(`http://127.0.0.1:${address.port}/api/sessions/session1/run`, {
      method: "POST",
      headers: {"content-type": "application/json"},
      body: JSON.stringify({input: "run three commands"}),
    });
    const frames = await timedRunFrames(response);
    assert.ok(Date.now() - started >= 950, "0.6s + 0.2s + 0.2s take at least 1s when serialized");
    const order = frames
      .filter((frame) => frame.event === "ui.part" && (frame.data.part as {type?: string}).type === "tool_result")
      .map((frame) => (frame.data.part as {callId: string}).callId);
    assert.deepEqual(order, ["call_0", "call_1", "call_2"]);
  } finally {
    await running.close();
    await rm(root, {recursive: true, force: true});
  }
});

test("max_parallel_tool_calls 1 runs the calls strictly one after another", async () => {
  const root = await mkdtemp(join(tmpdir(), "agent-harness-sequential-tools-"));
  const {client} = await writeParallelBashSession(root, [0.4, 0.4, 0.4], {max_parallel_tool_calls: 1});
  const running = createAgentServer({sessionsDir: root, client, toolStreamIntervalMs: 0});
  await new Promise<void>((resolve, reject) => {
    running.server.once("error", reject);
    running.server.listen(0, "127.0.0.1", () => resolve());
  });
  try {
    const address = running.server.address();
    assert.ok(address && typeof address !== "string");
    const started = Date.now();
    const response = await fetch(`http://127.0.0.1:${address.port}/api/sessions/session1/run`, {
      method: "POST",
      headers: {"content-type": "application/json"},
      body: JSON.stringify({input: "run three commands"}),
    });
    const frames = await timedRunFrames(response);
    assert.ok(Date.now() - started >= 1_100, "three 0.4s calls take at least 1.2s when serialized");
    const order = frames
      .filter((frame) => frame.event === "ui.part" && (frame.data.part as {type?: string}).type === "tool_result")
      .map((frame) => (frame.data.part as {callId: string}).callId);
    assert.deepEqual(order, ["call_0", "call_1", "call_2"]);
  } finally {
    await running.close();
    await rm(root, {recursive: true, force: true});
  }
});

test("stopping a run cancels every in-flight parallel tool call", async () => {
  const root = await mkdtemp(join(tmpdir(), "agent-harness-stop-parallel-"));
  const {client} = await writeParallelBashSession(root, [30, 30, 30]);
  const running = createAgentServer({sessionsDir: root, client, toolStreamIntervalMs: 0});
  await new Promise<void>((resolve, reject) => {
    running.server.once("error", reject);
    running.server.listen(0, "127.0.0.1", () => resolve());
  });
  try {
    const address = running.server.address();
    assert.ok(address && typeof address !== "string");
    const baseUrl = `http://127.0.0.1:${address.port}`;
    const response = await fetch(`${baseUrl}/api/sessions/session1/run`, {
      method: "POST",
      headers: {"content-type": "application/json"},
      body: JSON.stringify({input: "three slow commands"}),
    });
    const reading = timedRunFrames(response);
    // Let all three calls start, then stop the run.
    await new Promise((resolve) => setTimeout(resolve, 600));
    const stopped = Date.now();
    const stopResponse = await fetch(`${baseUrl}/api/sessions/session1/run/stop`, {method: "POST"});
    assert.equal(stopResponse.status, 200);
    const frames = await reading;
    assert.ok(Date.now() - stopped < 5_000, "the run ends promptly instead of waiting for the 30s commands");
    assert.equal(frames.some((frame) => frame.event === "error"), true);
    assert.equal(frames.at(-1)!.event, "done");
  } finally {
    await running.close();
    await rm(root, {recursive: true, force: true});
  }
});

test("keeps a quiet run stream alive with comment frames that clients ignore", async () => {
  const root = await mkdtemp(join(tmpdir(), "agent-harness-heartbeat-"));
  const {client} = await writeParallelBashSession(root, [0.6]);
  const running = createAgentServer({
    sessionsDir: root,
    client,
    toolStreamIntervalMs: 0,
    sseHeartbeatIntervalMs: 100,
  });
  await new Promise<void>((resolve, reject) => {
    running.server.once("error", reject);
    running.server.listen(0, "127.0.0.1", () => resolve());
  });
  try {
    const address = running.server.address();
    assert.ok(address && typeof address !== "string");
    const response = await fetch(`http://127.0.0.1:${address.port}/api/sessions/session1/run`, {
      method: "POST",
      headers: {"content-type": "application/json"},
      body: JSON.stringify({input: "one slow command"}),
    });
    const body = await response.text();
    const keepAlives = body.split("\n\n").filter((block) => block === ": keep-alive");
    assert.ok(keepAlives.length >= 2, `expected keep-alive frames during the 0.6s tool call, saw ${keepAlives.length}`);
    // Real events still parse, and the run still completes normally.
    assert.equal(ssePayloads(body, "completed").length, 1);
    assert.equal(ssePayloads(body, "done").length, 1);
  } finally {
    await running.close();
    await rm(root, {recursive: true, force: true});
  }
});

test("streams tool arguments over SSE before the call completes", async () => {
  const root = await mkdtemp(join(tmpdir(), "agent-harness-tool-stream-"));
  const sessionDir = join(root, "session1");
  await mkdir(sessionDir, {recursive: true});
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
      session_name: "tool-stream-test",
      cwd: sessionDir,
      tool_definition_paths: ["read.json"],
      tools: {native: {read: {max_bytes: 65536, default_limit: 250}}},
    }),
  );
  await writeFile(
    join(root, "read.json"),
    JSON.stringify({schema_version: 1, tools: [nativeReadDefinition]}),
  );
  await writeFile(join(sessionDir, "sample.txt"), "sample content\n", "utf8");

  const started = {
    id: "fc_read",
    type: "function_call",
    call_id: "call_read",
    name: "read",
    arguments: "",
  };
  const finished = {...started, arguments: '{"path":"sample.txt"}'};
  const argumentDeltas = ['{', '"p', 'ath"', ':', '"sa', 'mple', '.txt"', '}'];
  let responseNumber = 0;
  const fakeClient = {
    responses: {
      create: async () => {
        responseNumber += 1;
        if (responseNumber === 1) {
          return (async function* () {
            yield {type: "response.output_item.added", output_index: 0, item: started};
            // Argument deltas arrive a few characters at a time; the harness
            // coalesces the resulting snapshots instead of forwarding each one.
            for (const delta of argumentDeltas) {
              yield {
                type: "response.function_call_arguments.delta",
                item_id: "fc_read",
                output_index: 0,
                delta,
              };
            }
            yield {
              type: "response.function_call_arguments.done",
              item_id: "fc_read",
              output_index: 0,
              arguments: '{"path":"sample.txt"}',
            };
            yield {type: "response.output_item.done", output_index: 0, item: finished};
            yield completedEvent([finished], "response-stream-call");
          })();
        }
        const finalMessage = {
          type: "message",
          role: "assistant",
          content: [{type: "output_text", text: "Read it."}],
        };
        return (async function* () {
          yield {type: "response.output_item.done", output_index: 0, item: finalMessage};
          yield completedEvent([finalMessage], "response-stream-final");
        })();
      },
    },
  } as unknown as OpenAI;

  // A long interval makes the coalescing observable: only the snapshot taken the
  // moment the call appears is sent before the arguments finish.
  const running = createAgentServer({
    sessionsDir: root,
    client: fakeClient,
    toolStreamIntervalMs: 60_000,
  });
  await new Promise<void>((resolve, reject) => {
    running.server.once("error", reject);
    running.server.listen(0, "127.0.0.1", () => resolve());
  });

  try {
    const address = running.server.address();
    assert.ok(address && typeof address !== "string");
    const response = await fetch(
      `http://127.0.0.1:${address.port}/api/sessions/session1/run`,
      {
        method: "POST",
        headers: {"content-type": "application/json"},
        body: JSON.stringify({input: "read sample.txt"}),
      },
    );
    assert.equal(response.status, 200);
    const stream = await response.text();

    // What the client renders, replayed from the stream: the call appears the
    // moment it starts, its arguments arrive while it is still partial, and the
    // finished call replaces the streamed part — never a whole message per delta.
    const states = runStreamStates(stream);
    const callStates = states.flatMap((state) =>
      state.parts
        .filter((part) => part.type === "tool_call")
        .map((part) => ({
          arguments: (part as {arguments?: string}).arguments ?? "",
          partial: (part as {partial?: boolean}).partial === true,
        })),
    );

    const partialStates = callStates.filter((state) => state.partial);
    assert.ok(
      partialStates.length >= 2 && partialStates.length <= 3,
      `expected the streamed call to coalesce for ${argumentDeltas.length} deltas, saw ${JSON.stringify(callStates)}`,
    );
    assert.equal(partialStates[0]?.arguments, "");
    // The coalesced state carries the newest arguments, not a stale prefix.
    assert.equal(
      partialStates[partialStates.length - 1]?.arguments,
      '{"path":"sample.txt"}',
    );
    assert.ok(
      callStates.some((state) => !state.partial && state.arguments === '{"path":"sample.txt"}'),
      "the finished call carries its whole arguments",
    );
    assert.ok(
      ssePayloads(stream, "ui.message").length <= 3,
      "increments replace the per-delta snapshots",
    );
    const last = callStates[callStates.length - 1]!;
    assert.equal(last.arguments, '{"path":"sample.txt"}');
    assert.equal(last.partial, false);

    // The call is on the wire before its result, not after the tool ran.
    const partialIndex = stream.indexOf('"partial":true');
    assert.ok(partialIndex >= 0);
    assert.ok(partialIndex < stream.indexOf("tool_result"));
  } finally {
    await running.close();
    await rm(root, {recursive: true, force: true});
  }
});

/** A session directory with two turns and no provider involvement. */
async function writePagingSession(
  root: string,
): Promise<{sessionDir: string; messagesPath: string}> {
  const sessionDir = join(root, "session1");
  const messagesPath = join(sessionDir, "messages.jsonl");
  await mkdir(sessionDir, { recursive: true });
  await writeFile(
    join(sessionDir, "model.json"),
    JSON.stringify({ schema_version: 1, provider: "fake", model: "fake-model" }),
  );
  await writeFile(
    join(sessionDir, "setting.json"),
    JSON.stringify({ schema_version: 1, session_name: "paging", cwd: sessionDir }),
  );
  const turnRecord = (text: string, id: string, turn: number): Record<string, unknown> => ({
    ...completedEvent(
      [{ type: "message", role: "assistant", content: [{ type: "output_text", text }] }],
      id,
    ),
    session_turn: turn,
    session_step: 1,
  });
  const records = [
    { type: "message", role: "user", content: [{ type: "input_text", text: "first question" }] },
    turnRecord("first answer", "r-1-1", 1),
    { type: "message", role: "user", content: [{ type: "input_text", text: "second question" }] },
    turnRecord("second answer", "r-2-1", 2),
  ];
  await writeFile(messagesPath, records.map((record) => `${JSON.stringify(record)}\n`).join(""));
  return { sessionDir, messagesPath };
}

test("revalidates session requests instead of resending unchanged transcripts", async () => {
  const root = await mkdtemp(join(tmpdir(), "agent-harness-conditional-"));
  const { messagesPath } = await writePagingSession(root);
  const running = createAgentServer({ sessionsDir: root, snapshotTurns: 1 });
  await new Promise<void>((resolve, reject) => {
    running.server.once("error", reject);
    running.server.listen(0, "127.0.0.1", () => resolve());
  });

  try {
    const address = running.server.address();
    assert.ok(address && typeof address !== "string");
    const baseUrl = `http://127.0.0.1:${address.port}`;

    const first = await fetch(`${baseUrl}/api/sessions/session1`);
    assert.equal(first.status, 200);
    const etag = first.headers.get("etag");
    assert.ok(etag);
    assert.equal(first.headers.get("cache-control"), "no-cache");
    const snapshot = (await first.json()) as {
      messages: Array<{ text: string }>;
      page: { has_more: boolean; next_before: number | null; generation: string; stale_cursor: boolean };
    };
    assert.equal(snapshot.page.has_more, true);
    assert.equal(typeof snapshot.page.next_before, "number");

    // Revalidation: unchanged state, no body.
    const revalidated = await fetch(`${baseUrl}/api/sessions/session1`, {
      headers: { "If-None-Match": etag },
    });
    assert.equal(revalidated.status, 304);
    assert.equal(await revalidated.text(), "");
    assert.equal(revalidated.headers.get("etag"), etag);
    assert.equal(revalidated.headers.get("content-length"), null);

    // The list revalidates the same way and carries no transcript.
    const list = await fetch(`${baseUrl}/api/sessions`);
    const listEtag = list.headers.get("etag");
    assert.ok(listEtag);
    const listBody = (await list.json()) as { sessions: Array<Record<string, unknown>> };
    assert.equal(Object.hasOwn(listBody.sessions[0]!, "messages"), false);
    assert.equal((listBody.sessions[0] as { turns: number }).turns, 2);
    const listRevalidated = await fetch(`${baseUrl}/api/sessions`, {
      headers: { "If-None-Match": listEtag },
    });
    assert.equal(listRevalidated.status, 304);

    // A new turn changes both fingerprints.
    await appendFile(messagesPath, `${JSON.stringify({
      type: "message",
      role: "user",
      content: [{ type: "input_text", text: "third question" }],
    })}\n`);
    const changed = await fetch(`${baseUrl}/api/sessions/session1`, {
      headers: { "If-None-Match": etag },
    });
    assert.equal(changed.status, 200);
    assert.notEqual(changed.headers.get("etag"), etag);
    const changedList = await fetch(`${baseUrl}/api/sessions`, {
      headers: { "If-None-Match": listEtag },
    });
    assert.equal(changedList.status, 200);
  } finally {
    await running.close();
    await rm(root, { recursive: true, force: true });
  }
});

test("reads older turns through a cursor and rejects invalid cursors", async () => {
  const root = await mkdtemp(join(tmpdir(), "agent-harness-paging-"));
  const {messagesPath} = await writePagingSession(root);
  const running = createAgentServer({ sessionsDir: root, snapshotTurns: 1, maxPageTurns: 5 });
  await new Promise<void>((resolve, reject) => {
    running.server.once("error", reject);
    running.server.listen(0, "127.0.0.1", () => resolve());
  });

  try {
    const address = running.server.address();
    assert.ok(address && typeof address !== "string");
    const baseUrl = `http://127.0.0.1:${address.port}`;

    const newest = (await (await fetch(`${baseUrl}/api/sessions/session1`)).json()) as {
      messages: Array<{ text: string }>;
      page: { has_more: boolean; next_before: number | null; generation: string; stale_cursor: boolean };
    };
    // A turn includes the user message that opens it.
    assert.deepEqual(
      newest.messages.map((message) => message.text),
      ["second question", "second answer"],
    );

    const cursor = newest.page.next_before!;
    const older = (await (await fetch(
      `${baseUrl}/api/sessions/session1?before=${cursor}&cursor_generation=${encodeURIComponent(newest.page.generation)}&turns=5`,
    )).json()) as {
      messages: Array<{ text: string }>;
      page: { has_more: boolean; next_before: number | null; generation: string; stale_cursor: boolean };
    };
    assert.deepEqual(
      older.messages.map((message) => message.text),
      ["first question", "first answer"],
    );
    assert.equal(older.page.has_more, false);
    assert.equal(older.page.next_before, null);

    const replacementPath = `${messagesPath}.replacement`;
    const replacement = [
      {type: "message", role: "user", content: [{type: "input_text", text: "replacement question"}]},
      {
        ...completedEvent([{
          type: "message", role: "assistant", content: [{type: "output_text", text: "replacement answer"}],
        }], "replacement-response"),
        session_turn: 1,
        session_step: 1,
      },
    ];
    await writeFile(replacementPath, replacement.map((record) => `${JSON.stringify(record)}\n`).join(""));
    await rename(replacementPath, messagesPath);
    const stale = (await (await fetch(
      `${baseUrl}/api/sessions/session1?before=${cursor}&cursor_generation=${encodeURIComponent(newest.page.generation)}`,
    )).json()) as {messages: unknown[]; page: {stale_cursor: boolean}};
    assert.equal(stale.page.stale_cursor, true);
    assert.deepEqual(stale.messages, []);

    const invalid = await fetch(`${baseUrl}/api/sessions/session1?before=later`);
    assert.equal(invalid.status, 400);
    const invalidTurns = await fetch(`${baseUrl}/api/sessions/session1?turns=0`);
    assert.equal(invalidTurns.status, 400);

    // A page is limited by max_page_turns, so one request cannot pull the world.
    const limited = (await (await fetch(
      `${baseUrl}/api/sessions/session1?turns=100`,
    )).json()) as { messages: unknown[]; page: { has_more: boolean } };
    assert.equal(limited.page.has_more, false);
  } finally {
    await running.close();
    await rm(root, { recursive: true, force: true });
  }
});

/**
 * A transcript whose only turn produces a tool result far larger than the
 * inline cap, so the snapshot has to cut it and the message endpoint has to
 * hand it back whole.
 */
async function writeCappedSession(
  root: string,
  outputBytes: number,
): Promise<{sessionDir: string; output: string}> {
  const sessionDir = join(root, "session1");
  await mkdir(sessionDir, { recursive: true });
  await writeFile(
    join(sessionDir, "model.json"),
    JSON.stringify({ schema_version: 1, provider: "fake", model: "fake-model" }),
  );
  await writeFile(
    join(sessionDir, "setting.json"),
    JSON.stringify({ schema_version: 1, session_name: "capped", cwd: sessionDir }),
  );
  const output = `{"status":"ok","path":"big.txt"}\n${"x".repeat(outputBytes)}`;
  const records = [
    { type: "message", role: "user", content: [{ type: "input_text", text: "read big.txt" }] },
    {
      ...completedEvent(
        [
          {
            type: "function_call",
            call_id: "call-1",
            name: "read",
            arguments: JSON.stringify({ path: "big.txt" }),
          },
          {
            type: "message",
            role: "assistant",
            content: [{ type: "output_text", text: "done reading" }],
          },
        ],
        "r-1-1",
      ),
      session_turn: 1,
      session_step: 1,
    },
    { type: "function_call_output", call_id: "call-1", output },
  ];
  await writeFile(
    join(sessionDir, "messages.jsonl"),
    records.map((record) => `${JSON.stringify(record)}\n`).join(""),
  );
  return { sessionDir, output };
}

test("a snapshot caps large parts and the message endpoint returns them whole", async () => {
  const root = await mkdtemp(join(tmpdir(), "agent-harness-capped-"));
  const { output } = await writeCappedSession(root, 20_000);

  const running = createAgentServer({ sessionsDir: root, maxInlinePartBytes: 64 });
  await new Promise<void>((resolve, reject) => {
    running.server.once("error", reject);
    running.server.listen(0, "127.0.0.1", () => resolve());
  });

  try {
    const address = running.server.address();
    assert.ok(address && typeof address !== "string");
    const baseUrl = `http://127.0.0.1:${address.port}`;

    const snapshot = (await (await fetch(`${baseUrl}/api/sessions/session1`)).json()) as {
      messages: Array<{
        id: string;
        cursor?: number;
        text: string;
        parts: Array<{ type: string; output?: string; truncated?: { bytes: number } }>;
      }>;
    };
    const assistant = snapshot.messages.find((message) => message.id === "r-1-1");
    assert.ok(assistant, "the response id names the assistant message");
    const result = assistant.parts.find((part) => part.type === "tool_result");
    assert.ok(result);
    assert.equal(Buffer.byteLength(result.output ?? "", "utf8"), 64);
    assert.deepEqual(result.truncated, { bytes: Buffer.byteLength(output, "utf8") });    assert.equal(assistant.cursor, 0, "the turn starts at the first record");
    // The final text of the same message is small enough to stay inline.
    assert.equal(assistant.text, "done reading");
    const user = snapshot.messages.find((message) => message.text === "read big.txt");
    assert.ok(user);
    assert.equal(user.cursor, undefined, "nothing was capped for the user message");

    // The rest of the part, fetched by the cursor the snapshot published.
    const hydrated = (await (await fetch(
      `${baseUrl}/api/sessions/session1/messages/r-1-1?cursor=${assistant.cursor}`,
    )).json()) as {
      message: {
        id: string;
        text: string;
        parts: Array<{ type: string; output?: string; truncated?: { bytes: number } }>;
      };
    };
    assert.equal(hydrated.message.id, "r-1-1");
    assert.equal(hydrated.message.text, "done reading");
    const hydratedResult = hydrated.message.parts.find((part) => part.type === "tool_result");
    assert.ok(hydratedResult);
    assert.equal(hydratedResult.output, output);
    assert.equal(hydratedResult.truncated, undefined);

    // A cursor that is not a turn start cannot address a message.
    const staleCursor = await fetch(
      `${baseUrl}/api/sessions/session1/messages/r-1-1?cursor=7`,
    );
    assert.equal(staleCursor.status, 404);
    const unknownMessage = await fetch(
      `${baseUrl}/api/sessions/session1/messages/r-9-9?cursor=0`,
    );
    assert.equal(unknownMessage.status, 404);
    // Without a cursor the request is answered from the compacted prefix, which
    // this transcript message is not part of.
    const noCursor = await fetch(`${baseUrl}/api/sessions/session1/messages/r-1-1`);
    assert.equal(noCursor.status, 404);
    const badCursor = await fetch(`${baseUrl}/api/sessions/session1/messages/r-1-1?cursor=x`);
    assert.equal(badCursor.status, 400);

    // The cap is a transport concern only: the transcript keeps every byte.
    const transcript = await readFile(join(root, "session1", "messages.jsonl"), "utf8");
    const stored = transcript
      .split("\n")
      .filter((line) => line.length > 0)
      .map((line) => JSON.parse(line) as { type: string; output?: string });
    const storedResult = stored.find((record) => record.type === "function_call_output");
    assert.equal(storedResult?.output, output);
  } finally {
    await running.close();
    await rm(root, { recursive: true, force: true });
  }
});

test("a limit of zero disables the cap", async () => {
  const root = await mkdtemp(join(tmpdir(), "agent-harness-uncapped-"));
  const { output } = await writeCappedSession(root, 20_000);

  const running = createAgentServer({ sessionsDir: root, maxInlinePartBytes: 0 });
  await new Promise<void>((resolve, reject) => {
    running.server.once("error", reject);
    running.server.listen(0, "127.0.0.1", () => resolve());
  });

  try {
    const address = running.server.address();
    assert.ok(address && typeof address !== "string");
    const baseUrl = `http://127.0.0.1:${address.port}`;
    const snapshot = (await (await fetch(`${baseUrl}/api/sessions/session1`)).json()) as {
      messages: Array<{ parts: Array<{ type: string; output?: string }>; cursor?: number }>;
    };
    const result = snapshot.messages
      .flatMap((message) => message.parts)
      .find((part) => part.type === "tool_result");
    assert.ok(result);
    assert.equal(result.output, output);
    assert.equal(snapshot.messages.some((message) => message.cursor !== undefined), false);
  } finally {
    await running.close();
    await rm(root, { recursive: true, force: true });
  }
});

/**
 * A session that has compacted: the transcript holds the retained tail and the
 * compacted prefix lives in the staging cache, which is what a snapshot sends
 * in front of it.
 */
async function writeCompactedSession(
  root: string,
  finalText: string,
): Promise<{sessionDir: string}> {
  const sessionDir = join(root, "session1");
  const staging = join(sessionDir, "staging");
  await mkdir(staging, { recursive: true });
  await writeFile(
    join(sessionDir, "model.json"),
    JSON.stringify({ schema_version: 1, provider: "fake", model: "fake-model" }),
  );
  await writeFile(
    join(sessionDir, "setting.json"),
    JSON.stringify({ schema_version: 1, session_name: "compacted", cwd: sessionDir }),
  );
  // The prefix a compaction retained: the user turn and the assistant final.
  await writeFile(
    join(staging, "compaction-cache.json"),
    JSON.stringify({
      schema_version: 1,
      source_sha256: "0".repeat(64),
      items: [
        { type: "message", role: "user", content: [{ type: "input_text", text: "first question" }] },
        {
          type: "message",
          role: "assistant",
          id: "gen-final-1",
          content: [{ type: "output_text", text: finalText }],
        },
      ],
      assistant_final_items: [],
      active_tail: [],
    }),
  );
  // The pre-compaction transcript: raw records, which is what the UI renders
  // (the compacted request items above are for the model, not for the view).
  await writeFile(
    join(sessionDir, "messages.backup.jsonl"),
    `${JSON.stringify({ type: "message", role: "user", content: [{ type: "input_text", text: "first question" }] })}\n${JSON.stringify({
      ...completedEvent(
        [
          {type: "reasoning", summary: [{type: "summary_text", text: "first thinking"}]},
          {
            type: "message",
            role: "assistant",
            id: "gen-final-1",
            content: [{ type: "output_text", text: finalText }],
          },
        ],
        "gen-final-1",
      ),
      session_turn: 1,
      session_step: 1,
    })}\n`,
  );
  await writeFile(
    join(sessionDir, "messages.jsonl"),
    `${JSON.stringify({ type: "message", role: "user", content: [{ type: "input_text", text: "second question" }] })}\n${JSON.stringify({
      ...completedEvent(
        [{ type: "message", role: "assistant", content: [{ type: "output_text", text: "second answer" }] }],
        "r-2-1",
      ),
      session_turn: 2,
      session_step: 1,
    })}\n`,
  );
  return { sessionDir };
}

test("a capped message from the compacted prefix is returned whole", async () => {
  const root = await mkdtemp(join(tmpdir(), "agent-harness-compacted-"));
  const finalText = `{"status":"ok"}\n${"c".repeat(30_000)}`;
  await writeCompactedSession(root, finalText);

  const running = createAgentServer({ sessionsDir: root, maxInlinePartBytes: 128 });
  await new Promise<void>((resolve, reject) => {
    running.server.once("error", reject);
    running.server.listen(0, "127.0.0.1", () => resolve());
  });

  try {
    const address = running.server.address();
    assert.ok(address && typeof address !== "string");
    const baseUrl = `http://127.0.0.1:${address.port}`;

    const snapshot = (await (await fetch(`${baseUrl}/api/sessions/session1`)).json()) as {
      messages: Array<{ id: string }>;
      history?: { etag: string | null; count: number };
    };
    // The transcript half of the snapshot carries no compacted turn; it refers to
    // the block the client fetches (and revalidates) separately.
    assert.deepEqual(snapshot.messages.map((message) => message.id), ["session-user-o0", "r-2-1"]);
    assert.ok(snapshot.history, "the snapshot refers to the history block");
    assert.equal(snapshot.history.count > 0, true);
    assert.equal(typeof snapshot.history.etag, "string");

    const historyResponse = await fetch(`${baseUrl}/api/sessions/session1/history`);
    assert.equal(historyResponse.status, 200);
    const history = (await historyResponse.json()) as {
      messages: Array<{
        id: string;
        cursor?: number;
        parts: Array<{ type: string; text?: string; truncated?: { bytes: number } }>;
      }>;
    };
    const final = history.messages.find((message) => message.id === "gen-final-1");
    assert.ok(final, "the compacted final is part of the history block");
    const part = final.parts.find((candidate) => candidate.type === "text");
    assert.ok(part);
    assert.equal(Buffer.byteLength(part.text ?? "", "utf8"), 128);
    assert.deepEqual(part.truncated, { bytes: Buffer.byteLength(finalText, "utf8") });
    assert.equal(final.cursor, undefined, "a history message has no turn cursor");

    // The block is static, so its own fingerprint revalidates it without a body.
    const revalidated = await fetch(`${baseUrl}/api/sessions/session1/history`, {
      headers: {"if-none-match": historyResponse.headers.get("etag") ?? ""},
    });
    assert.equal(revalidated.status, 304);
    assert.equal(await revalidated.text(), "");

    // No cursor: the message is served from the block the view already holds.
    const hydrated = (await (await fetch(
      `${baseUrl}/api/sessions/session1/messages/gen-final-1`,
    )).json()) as { message: { id: string; text: string } };
    assert.equal(hydrated.message.id, "gen-final-1");
    assert.equal(hydrated.message.text, finalText);
  } finally {
    await running.close();
    await rm(root, { recursive: true, force: true });
  }
});

test("applies the configured header and idle timeouts to the HTTP server", () => {
  const defaults = createAgentServer({});
  assert.equal(defaults.server.headersTimeout, 60_000);
  assert.equal(defaults.server.keepAliveTimeout, 5_000);
  const configured = createAgentServer({readHeaderTimeoutMs: 1_500, idleTimeoutMs: 0});
  assert.equal(configured.server.headersTimeout, 1_500);
  assert.equal(configured.server.keepAliveTimeout, 0);
});
