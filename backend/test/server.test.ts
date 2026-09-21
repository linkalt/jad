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
import {nativeReadDefinition} from "../src/tools/native/read.js";

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

test("serves sessions using configured model and transcript paths", async () => {
  const root = await mkdtemp(join(tmpdir(), "agent-harness-configured-paths-"));
  const sessionDir = join(root, "session1");
  const modelPath = join(sessionDir, "config", "provider.json");
  const messagesPath = join(sessionDir, "data", "transcript.jsonl");
  await mkdir(sessionDir, {recursive: true});
  await mkdir(join(sessionDir, "config"), {recursive: true});
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
        model: "config/provider.json",
        staging: "runtime/staging",
        http_debug: "diagnostics/http",
      },
    }),
  );
  await writeFile(
    messagesPath,
    `${JSON.stringify({
      type: "response.completed",
      response: {
        id: "configured-response",
        output: [{
          type: "message",
          role: "assistant",
          content: [{type: "output_text", text: "from configured transcript"}],
        }],
      },
    })}\n`,
  );

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
        messages: Array<{text: string}>;
      }>;
    };
    assert.equal(list.sessions[0]?.id, "session1");
    assert.deepEqual(list.sessions[0]?.model, {
      provider: "configured-provider",
      model: "configured-model",
    });
    assert.equal(list.sessions[0]?.messages[0]?.text, "from configured transcript");

    const snapshotResponse = await fetch(`${baseUrl}/api/sessions/session1`);
    assert.equal(snapshotResponse.status, 200);
    const snapshot = (await snapshotResponse.json()) as {
      model: {provider: string; model: string};
      messages: Array<{text: string}>;
    };
    assert.deepEqual(snapshot.model, {
      provider: "configured-provider",
      model: "configured-model",
    });
    assert.equal(snapshot.messages[0]?.text, "from configured transcript");
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
    assert.match(await response.text(), /missing-tools\.json/);

    const records = (await readFile(join(sessionDir, "messages.jsonl"), "utf8"))
      .trim()
      .split("\n")
      .map((line) => JSON.parse(line) as Record<string, unknown>);
    assert.deepEqual(records[0], {
      type: "message",
      role: "user",
      content: [{type: "input_text", text: "persist this input"}],
    });
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
      system_prompt: "Be concise.",
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

test("streams tool call and result blocks over SSE", async () => {
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
        const output = responseNumber === 1 ? call : finalMessage;
        return (async function* () {
          yield {type: "response.output_item.done", output_index: 0, item: output};
          yield completedEvent([output], `response-tool-${responseNumber}`);
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
    const uiMessages = ssePayloads(stream, "ui.message").map(
      (payload) => payload.message as {
        status?: string;
        parts: Array<Record<string, unknown>>;
      },
    );
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
    assert.equal(uiMessages[1]?.parts[1]?.status, "success");
    assert.equal(uiMessages[2]?.parts[2]?.type, "text");
    assert.equal(uiMessages[2]?.parts[2]?.text, "Tool finished.");

    const snapshot = await fetch("http://127.0.0.1:" + address.port + "/api/sessions/session1");
    const body = (await snapshot.json()) as {messages: Array<{parts: Array<Record<string, unknown>>}>};
    assert.deepEqual(body.messages[1]?.parts.map((part) => part.type), [
      "tool_call",
      "tool_result",
      "text",
    ]);
  } finally {
    await running.close();
    await rm(root, {recursive: true, force: true});
  }
});
