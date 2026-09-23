import assert from "node:assert/strict";
import { appendFile, mkdir, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";

import type OpenAI from "openai";

import { createAgentServer } from "../src/server.js";
import {SessionResponseStore} from "../src/persistence.js";
import {
  normalizeWebUiSession,
  WebUiNormalizedRun,
} from "../src/plugins/web-ui-normalized.js";
import type { LoadedSessionConfig } from "../src/config.js";
import {nativeReadDefinition} from "../src/tools/native/read.js";
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

test("loads configurable root server settings", async () => {
  const root = await mkdtemp(join(tmpdir(), "agent-harness-root-settings-"));
  const settingPath = join(root, "setting.json");
  try {
    await writeFile(
      settingPath,
      JSON.stringify({
        server: {host: "0.0.0.0", port: 9876, max_body_bytes: 2 * 1024 * 1024},
        session_roots: [{path: ".jad/sessions"}],
      }),
    );
    assert.deepEqual(await loadRootServerSettings({rootSettingPath: settingPath}), {
      host: "0.0.0.0",
      port: 9876,
      max_body_bytes: 2 * 1024 * 1024,
      run_state_retention_ms: 30_000,
      tool_stream_interval_ms: 120,
      conditional_requests: true,
      snapshot_turns: 24,
      max_page_turns: 50,
      max_inline_part_bytes: 16 * 1024,
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
  } finally {
    await rm(root, {recursive: true, force: true});
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
    const compacted = await compactResponse.json() as {compaction: {compacted: boolean}};
    assert.equal(compacted.compaction.compacted, true);
    assert.equal(await readFile(messagesPath, "utf8"), "");
    assert.match(await readFile(join(sessionDir, "messages.backup.jsonl"), "utf8"), /configured transcript/);
    const compactedSnapshot = await fetch(`${baseUrl}/api/sessions/session1`);
    const compactedBody = await compactedSnapshot.json() as {
      messages: Array<{text: string}>;
      usage?: Record<string, unknown>;
    };
    assert.equal(compactedBody.messages[0]?.text, "from configured transcript");
    assert.deepEqual(compactedBody.usage, list.sessions[0]?.usage);
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

    const callSnapshots = ssePayloads(stream, "ui.message").flatMap((payload) => {
      const message = payload.message as Record<string, unknown>;
      const parts = Array.isArray(message.parts) ? message.parts : [];
      return parts
        .filter(
          (part): part is Record<string, unknown> =>
            typeof part === "object" &&
            part !== null &&
            (part as {type?: unknown}).type === "tool_call",
        )
        .map((part) => ({
          arguments: typeof part.arguments === "string" ? part.arguments : "",
          partial: part.partial === true,
        }));
    });

    const texts = callSnapshots.map((snapshot) => snapshot.arguments);
    const partialSnapshots = callSnapshots.filter((snapshot) => snapshot.partial);
    // The call itself goes out immediately and the newest coalesced state follows
    // before completion - never one snapshot per delta.
    assert.ok(
      partialSnapshots.length <= 2,
      `expected coalescing for ${argumentDeltas.length} deltas, saw ${partialSnapshots.length}: ${JSON.stringify(texts)}`,
    );
    assert.equal(partialSnapshots[0]?.arguments, "");
    // The coalesced snapshot still carries the newest state, not a stale one.
    assert.equal(
      partialSnapshots[partialSnapshots.length - 1]?.arguments,
      '{"path":"sample.txt"}',
    );
    assert.ok(texts.includes('{"path":"sample.txt"}'));
    assert.ok(
      callSnapshots.some((snapshot) => snapshot.partial),
      "at least one snapshot must be marked partial",
    );
    const last = callSnapshots[callSnapshots.length - 1]!;
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
      page: { has_more: boolean; next_before: number | null };
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
  await writePagingSession(root);
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
      page: { has_more: boolean; next_before: number | null };
    };
    // A turn includes the user message that opens it.
    assert.deepEqual(
      newest.messages.map((message) => message.text),
      ["second question", "second answer"],
    );

    const cursor = newest.page.next_before!;
    const older = (await (await fetch(
      `${baseUrl}/api/sessions/session1?before=${cursor}&turns=5`,
    )).json()) as {
      messages: Array<{ text: string }>;
      page: { has_more: boolean; next_before: number | null };
    };
    assert.deepEqual(
      older.messages.map((message) => message.text),
      ["first question", "first answer"],
    );
    assert.equal(older.page.has_more, false);
    assert.equal(older.page.next_before, null);

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
      messages: Array<{
        id: string;
        cursor?: number;
        parts: Array<{ type: string; text?: string; truncated?: { bytes: number } }>;
      }>;
    };
    const final = snapshot.messages.find((message) => message.id === "gen-final-1");
    assert.ok(final, "the compacted final is part of the snapshot");
    const part = final.parts.find((candidate) => candidate.type === "text");
    assert.ok(part);
    assert.equal(Buffer.byteLength(part.text ?? "", "utf8"), 128);
    assert.deepEqual(part.truncated, { bytes: Buffer.byteLength(finalText, "utf8") });
    assert.equal(final.cursor, undefined, "a compacted message has no turn cursor");

    // No cursor: the message is served from the head the view already holds.
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
