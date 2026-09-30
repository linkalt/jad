import assert from "node:assert/strict";
import {mkdir, mkdtemp, readFile, rm, writeFile} from "node:fs/promises";
import {tmpdir} from "node:os";
import {join} from "node:path";
import test from "node:test";

import type OpenAI from "openai";

import {createAgentServer as createServer, type AgentServerOptions} from "../src/server.js";
import {nativeRequestDraftDefinition} from "../src/tools/native/request-draft.js";
import type {WebUiInteraction, WebUiMessage} from "../src/plugins/web-ui/types.js";

const TEST_API_TOKEN = "test-api-token-0123456789abcdef0123456789abcdef";
const createAgentServer = (options: AgentServerOptions) => createServer({...options, apiToken: TEST_API_TOKEN});

interface SseEvent {
  event: string;
  data: Record<string, unknown>;
}

/** Every complete frame the stream has produced so far; a partial one is ignored. */
function parseSse(stream: string): SseEvent[] {
  const events: SseEvent[] = [];
  for (const block of stream.split("\n\n")) {
    const lines = block.split("\n").filter((line) => line.length > 0);
    let event = "message";
    let data = "";
    for (const line of lines) {
      if (line.startsWith("event:")) {
        event = line.slice("event:".length).trim();
      } else if (line.startsWith("data:")) {
        data += line.slice("data:".length).trim();
      }
    }
    if (data.length === 0) {
      continue;
    }
    try {
      events.push({event, data: JSON.parse(data) as Record<string, unknown>});
    } catch {
      // A frame still arriving; the next read parses it whole.
    }
  }
  return events;
}

function completedEvent(output: Record<string, unknown>[], id: string): Record<string, unknown> {
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

test("a run publishes the draft question, waits, and continues with the user's answer", async () => {
  const root = await mkdtemp(join(tmpdir(), "agent-harness-interaction-"));
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
      session_name: "interaction-test",
      cwd: sessionDir,
      tool_definition_paths: ["request_draft.json"],
      tools: {native: {request_draft: {timeout_ms: 30_000, extend_ms: 20_000, max_timeout_ms: 50_000}}},
    }),
  );
  await writeFile(
    join(root, "request_draft.json"),
    JSON.stringify({schema_version: 1, tools: [nativeRequestDraftDefinition]}),
  );

  const toolCall = {
    id: "fc_draft",
    type: "function_call",
    call_id: "call_draft",
    name: "request_draft",
    arguments: JSON.stringify({title: "How I read it", draft: {goal: "Make the page better"}}),
  };
  const requests: Array<{input?: Array<Record<string, unknown>>}> = [];
  let responseNumber = 0;
  const fakeClient = {
    responses: {
      create: async (request: {input?: Array<Record<string, unknown>>}) => {
        requests.push(request);
        responseNumber += 1;
        if (responseNumber === 1) {
          return (async function* () {
            yield {type: "response.output_item.done", output_index: 0, item: toolCall};
            yield completedEvent([toolCall], "response-draft-call");
          })();
        }
        const finalMessage = {
          type: "message",
          role: "assistant",
          content: [{type: "output_text", text: "Proceeding with the edited draft."}],
        };
        return (async function* () {
          yield {type: "response.output_item.done", output_index: 0, item: finalMessage};
          yield completedEvent([finalMessage], "response-final");
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
    assert.ok(address !== null && typeof address !== "string");
    const baseUrl = `http://127.0.0.1:${address.port}`;
    const authorized = {authorization: `Bearer ${TEST_API_TOKEN}`, "content-type": "application/json"};

    const runResponse = await fetch(`${baseUrl}/api/sessions/session1/run`, {
      method: "POST",
      headers: authorized,
      body: JSON.stringify({input: "make the page better", supports_interaction: true}),
    });
    assert.equal(runResponse.status, 200);
    const reader = runResponse.body!.getReader();
    const decoder = new TextDecoder();
    let stream = "";

    /** Reads until `stop` sees what it is looking for, or the stream ends. */
    const readUntil = async (
      stop: (events: SseEvent[]) => boolean,
      label: string,
    ): Promise<SseEvent[]> => {
      for (;;) {
        const {value, done} = await reader.read();
        if (done) {
          break;
        }
        stream += decoder.decode(value, {stream: true});
        const events = parseSse(stream);
        if (stop(events)) {
          return events;
        }
      }
      throw new Error(`the stream ended before ${label}`);
    };

    const pendingOf = (events: SseEvent[]): WebUiInteraction | undefined =>
      events
        .filter((event) => event.event === "ui.message")
        .map((event) => (event.data.message as WebUiMessage | undefined)?.pending)
        .find((pending) => pending !== undefined);

    const waiting = await readUntil((events) => pendingOf(events) !== undefined, "the question was asked");
    const question = pendingOf(waiting);
    assert.ok(question, "the run published the question");
    assert.equal(question.tool, "request_draft");
    assert.equal(question.tool_call_id, "call_draft");
    assert.deepEqual(question.draft, {goal: "Make the page better"});
    assert.equal(question.title, "How I read it");
    // The published deadline is the configured timeout, not a hard-coded wait.
    assert.equal(
      new Date(question.expires_at).getTime() - new Date(question.created_at).getTime(),
      30_000,
    );
    assert.equal(question.extend_ms, 20_000, "the question carries the extension step");
    assert.equal(
      new Date(question.max_expires_at).getTime() - new Date(question.created_at).getTime(),
      50_000,
      "and the ceiling the whole wait may reach",
    );

    // The user can push the deadline out without answering; the run republishes it.
    const extendedResponse = await fetch(`${baseUrl}/api/sessions/session1/run/interaction/extend`, {
      method: "POST",
      headers: authorized,
      body: JSON.stringify({interaction_id: question.id}),
    });
    assert.equal(extendedResponse.status, 200);
    const extended = (await extendedResponse.json()) as {
      interaction: {interaction_id: string; status: string; expires_at: string; max_expires_at: string};
    };
    assert.equal(extended.interaction.status, "pending");
    assert.equal(extended.interaction.max_expires_at, question.max_expires_at);
    assert.equal(
      new Date(extended.interaction.expires_at).getTime() - new Date(question.created_at).getTime(),
      50_000,
      "the step is added to the deadline",
    );
    await readUntil(
      (events) => latestPendingExpiry(events) === extended.interaction.expires_at,
      "the new deadline was published to the stream",
    );

    const atMax = await fetch(`${baseUrl}/api/sessions/session1/run/interaction/extend`, {
      method: "POST",
      headers: authorized,
      body: JSON.stringify({interaction_id: question.id}),
    });
    assert.equal(atMax.status, 409, "the whole wait is capped at max_timeout_ms");
    assert.match(((await atMax.json()) as {error: string}).error, /longest wait/);

    const malformedExtend = await fetch(`${baseUrl}/api/sessions/session1/run/interaction/extend`, {
      method: "POST",
      headers: authorized,
      body: JSON.stringify({}),
    });
    assert.equal(malformedExtend.status, 400, "an extension has to name its question");

    const edited = {goal: "Rewrite the intro paragraph"};
    const answer = await fetch(`${baseUrl}/api/sessions/session1/run/interaction`, {
      method: "POST",
      headers: authorized,
      body: JSON.stringify({interaction_id: question.id, decision: "update", draft: edited}),
    });
    assert.equal(answer.status, 200);
    assert.deepEqual(await answer.json(), {
      interaction: {interaction_id: question.id, decision: "update", status: "answered"},
    });

    const finished = await readUntil((events) => events.some((event) => event.event === "done"), "the run finished");
    const lastMessage = finished
      .filter((event) => event.event === "ui.message")
      .map((event) => event.data.message as WebUiMessage)
      .at(-1);
    assert.equal(lastMessage?.pending, undefined, "the answered question leaves the run state");

    const secondRequest = requests[1];
    assert.ok(secondRequest?.input, "the run asked the provider again");
    const toolResult = secondRequest.input.find(
      (item) => item.type === "function_call_output" && item.call_id === "call_draft",
    );
    assert.ok(toolResult, "the model receives the decision as a tool result");
    assert.deepEqual(JSON.parse(String(toolResult.output)), {
      decision: "update",
      status: "answered",
      draft: edited,
      changed: ["goal"],
    });

    // The answer is persisted as an ordinary tool result: the transcript keeps
    // its item types, and the call is recorded like any other tool call.
    const transcript = (await readFile(join(sessionDir, "messages.jsonl"), "utf8"))
      .trim()
      .split("\n")
      .map((line) => JSON.parse(line) as Record<string, unknown>);
    assert.deepEqual(
      [...new Set(transcript.map((item) => item.type))].sort(),
      ["function_call_output", "message", "response.completed"],
      "an interaction adds no transcript item type",
    );
    assert.ok(
      transcript.some(
        (item) => item.type === "response.completed" && JSON.stringify(item).includes('"name":"request_draft"'),
      ),
      "the call is recorded in the response the model produced",
    );
    const persistedOutput = transcript.find((item) => item.type === "function_call_output");
    assert.deepEqual(JSON.parse(String(persistedOutput?.output)), {
      decision: "update",
      status: "answered",
      draft: edited,
      changed: ["goal"],
    });
    const accepted = JSON.parse(await readFile(join(sessionDir, "accepted-request.json"), "utf8")) as {
      source: string;
      action: string;
      call_id: string;
      draft: Record<string, unknown>;
    };
    assert.equal(accepted.source, "user");
    assert.equal(accepted.action, "update");
    assert.equal(accepted.call_id, "call_draft");
    assert.deepEqual(accepted.draft, edited);

    // A second answer for the same question changes nothing.
    const repeated = await fetch(`${baseUrl}/api/sessions/session1/run/interaction`, {
      method: "POST",
      headers: authorized,
      body: JSON.stringify({interaction_id: question.id, decision: "update", draft: edited}),
    });
    assert.equal(repeated.status, 409);

    const malformed = await fetch(`${baseUrl}/api/sessions/session1/run/interaction`, {
      method: "POST",
      headers: authorized,
      body: JSON.stringify({interaction_id: question.id, decision: "decline"}),
    });
    assert.equal(malformed.status, 400, "a decline without a reason is refused");
  } finally {
    await running.close();
    await new Promise<void>((resolve) => {
      running.server.close(() => resolve());
    });
  }
});

test("a declined request_draft keeps the user's reason in its agent-facing tool result", async () => {
  const root = await mkdtemp(join(tmpdir(), "agent-harness-interaction-decline-"));
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
      session_name: "interaction-decline",
      cwd: sessionDir,
      tool_definition_paths: ["request_draft.json"],
      tools: {native: {request_draft: {timeout_ms: 30_000}}},
    }),
  );
  await writeFile(
    join(root, "request_draft.json"),
    JSON.stringify({schema_version: 1, tools: [nativeRequestDraftDefinition]}),
  );

  const toolCall = {
    id: "fc_decline",
    type: "function_call",
    call_id: "call_decline",
    name: "request_draft",
    arguments: JSON.stringify({draft: {goal: "Make the page better"}}),
  };
  const fakeClient = {
    responses: {
      create: async () => (async function* () {
        yield {type: "response.output_item.done", output_index: 0, item: toolCall};
        yield completedEvent([toolCall], "response-decline");
      })(),
    },
  } as unknown as OpenAI;

  const running = createAgentServer({sessionsDir: root, client: fakeClient});
  await new Promise<void>((resolve, reject) => {
    running.server.once("error", reject);
    running.server.listen(0, "127.0.0.1", () => resolve());
  });

  try {
    const address = running.server.address();
    assert.ok(address !== null && typeof address !== "string");
    const baseUrl = `http://127.0.0.1:${address.port}`;
    const authorized = {authorization: `Bearer ${TEST_API_TOKEN}`, "content-type": "application/json"};
    const runResponse = await fetch(`${baseUrl}/api/sessions/session1/run`, {
      method: "POST",
      headers: authorized,
      body: JSON.stringify({input: "make the page better", supports_interaction: true}),
    });
    assert.equal(runResponse.status, 200);

    const reader = runResponse.body!.getReader();
    const decoder = new TextDecoder();
    let stream = "";
    const readUntil = async (
      stop: (events: SseEvent[]) => boolean,
      label: string,
    ): Promise<SseEvent[]> => {
      for (;;) {
        const {value, done} = await reader.read();
        if (done) break;
        stream += decoder.decode(value, {stream: true});
        const events = parseSse(stream);
        if (stop(events)) return events;
      }
      throw new Error(`the stream ended before ${label}`);
    };
    const waiting = await readUntil((events) => events.some((event) =>
      event.event === "ui.message" &&
      (event.data.message as WebUiMessage | undefined)?.pending !== undefined,
    ), "the question was asked");
    const question = waiting
      .filter((event) => event.event === "ui.message")
      .map((event) => (event.data.message as WebUiMessage | undefined)?.pending)
      .find((pending) => pending !== undefined);
    assert.ok(question);

    const reason = "The requested scope is too broad to approve.";
    const answer = await fetch(`${baseUrl}/api/sessions/session1/run/interaction`, {
      method: "POST",
      headers: authorized,
      body: JSON.stringify({interaction_id: question.id, decision: "decline", reason}),
    });
    assert.equal(answer.status, 200);
    assert.deepEqual(await answer.json(), {
      interaction: {interaction_id: question.id, decision: "decline", status: "answered"},
    });
    await readUntil((events) => events.some((event) => event.event === "done"), "the declined result was recorded");

    const transcript = (await readFile(join(sessionDir, "messages.jsonl"), "utf8"))
      .trim()
      .split("\n")
      .map((line) => JSON.parse(line) as Record<string, unknown>);
    const toolResult = transcript.find((item) =>
      item.type === "function_call_output" && item.call_id === "call_decline",
    );
    assert.ok(toolResult, "the tool result is persisted for the agent");
    assert.deepEqual(JSON.parse(String(toolResult.output)), {
      decision: "decline",
      status: "answered",
      reason,
    });
  } finally {
    await running.close();
    await new Promise<void>((resolve) => {
      running.server.close(() => resolve());
    });
    await rm(root, {recursive: true, force: true});
  }
});

test("the configured wait is published and an edit beyond the session's bounds is refused", async () => {
  const root = await mkdtemp(join(tmpdir(), "agent-harness-interaction-bounds-"));
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
      session_name: "interaction-bounds",
      cwd: sessionDir,
      tool_definition_paths: ["request_draft.json"],
      tools: {
        native: {
          request_draft: {
            timeout_ms: 30_000,
            max_timeout_ms: 45_000,
            extend_ms: 15_000,
            max_draft_bytes: 1024,
            max_draft_depth: 3,
          },
        },
      },
    }),
  );
  await writeFile(
    join(root, "request_draft.json"),
    JSON.stringify({schema_version: 1, tools: [nativeRequestDraftDefinition]}),
  );

  const toolCall = {
    id: "fc_draft",
    type: "function_call",
    call_id: "call_draft",
    name: "request_draft",
    arguments: JSON.stringify({draft: {goal: "Make the page better"}}),
  };
  const requests: Array<{input?: Array<Record<string, unknown>>}> = [];
  let responseNumber = 0;
  const fakeClient = {
    responses: {
      create: async (request: {input?: Array<Record<string, unknown>>}) => {
        requests.push(request);
        responseNumber += 1;
        if (responseNumber === 1) {
          return (async function* () {
            yield {type: "response.output_item.done", output_index: 0, item: toolCall};
            yield completedEvent([toolCall], "response-draft-call");
          })();
        }
        const finalMessage = {
          type: "message",
          role: "assistant",
          content: [{type: "output_text", text: "Proceeding."}],
        };
        return (async function* () {
          yield {type: "response.output_item.done", output_index: 0, item: finalMessage};
          yield completedEvent([finalMessage], "response-final");
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
    assert.ok(address !== null && typeof address !== "string");
    const baseUrl = `http://127.0.0.1:${address.port}`;
    const authorized = {authorization: `Bearer ${TEST_API_TOKEN}`, "content-type": "application/json"};

    const runResponse = await fetch(`${baseUrl}/api/sessions/session1/run`, {
      method: "POST",
      headers: authorized,
      body: JSON.stringify({input: "make it better", supports_interaction: true}),
    });
    assert.equal(runResponse.status, 200);
    const reader = runResponse.body!.getReader();
    const decoder = new TextDecoder();
    let stream = "";

    const readUntil = async (stop: (events: SseEvent[]) => boolean, label: string): Promise<SseEvent[]> => {
      for (;;) {
        const {value, done} = await reader.read();
        if (done) {
          break;
        }
        stream += decoder.decode(value, {stream: true});
        const events = parseSse(stream);
        if (stop(events)) {
          return events;
        }
      }
      throw new Error(`the stream ended before ${label}`);
    };

    const waiting = await readUntil(
      (events) => secondsSafe(events) !== undefined,
      "the question was asked",
    );
    assert.equal(secondsSafe(waiting), 30, "the configured wait is what the run publishes");
    const question = waiting
      .filter((event) => event.event === "ui.message")
      .map((event) => (event.data.message as WebUiMessage | undefined)?.pending)
      .find((candidate) => candidate !== undefined);
    assert.ok(question);

    const oversized = await fetch(`${baseUrl}/api/sessions/session1/run/interaction`, {
      method: "POST",
      headers: authorized,
      body: JSON.stringify({
        interaction_id: question.id,
        decision: "update",
        draft: {goal: "x".repeat(1500)},
      }),
    });
    assert.equal(oversized.status, 400, "an edit larger than the session's bound is refused");
    const refusal = (await oversized.json()) as {error: string};
    assert.match(refusal.error, /limit is 1024/);

    const tooDeep = await fetch(`${baseUrl}/api/sessions/session1/run/interaction`, {
      method: "POST",
      headers: authorized,
      body: JSON.stringify({
        interaction_id: question.id,
        decision: "update",
        draft: {a: {b: {c: {d: 1}}}},
      }),
    });
    assert.equal(tooDeep.status, 400, "an edit deeper than the session's bound is refused");

    const accepted = await fetch(`${baseUrl}/api/sessions/session1/run/interaction`, {
      method: "POST",
      headers: authorized,
      body: JSON.stringify({
        interaction_id: question.id,
        decision: "update",
        draft: {goal: "Rewrite the intro paragraph"},
      }),
    });
    assert.equal(accepted.status, 200, "an edit inside the bounds is applied");

    await readUntil((events) => events.some((event) => event.event === "done"), "the run finished");
    const toolResult = requests[1]?.input?.find(
      (item) => item.type === "function_call_output" && item.call_id === "call_draft",
    );
    assert.deepEqual(JSON.parse(String(toolResult?.output)), {
      decision: "update",
      status: "answered",
      draft: {goal: "Rewrite the intro paragraph"},
      changed: ["goal"],
    });
  } finally {
    await running.close();
    await new Promise<void>((resolve) => {
      running.server.close(() => resolve());
    });
  }
});

/** The newest published deadline, or undefined while the question is not out. */
function latestPendingExpiry(events: SseEvent[]): string | undefined {
  return events
    .filter((event) => event.event === "ui.message")
    .map((event) => (event.data.message as WebUiMessage | undefined)?.pending?.expires_at)
    .filter((value) => value !== undefined)
    .at(-1);
}

/** The published deadline in seconds, or undefined while the question is not out yet. */
function secondsSafe(events: SseEvent[]): number | undefined {
  const pending = events
    .filter((event) => event.event === "ui.message")
    .map((event) => (event.data.message as WebUiMessage | undefined)?.pending)
    .find((candidate) => candidate !== undefined);
  return pending === undefined
    ? undefined
    : (new Date(pending.expires_at).getTime() - new Date(pending.created_at).getTime()) / 1000;
}
