import assert from "node:assert/strict";
import {mkdtemp, readFile, rm, writeFile} from "node:fs/promises";
import {tmpdir} from "node:os";
import {join} from "node:path";
import test from "node:test";

import type OpenAI from "openai";

import {SessionConfigLoader} from "../src/config.js";
import {AgentSession} from "../src/session.js";
import {loadToolCatalog} from "../src/tools/index.js";
import {nativeReadDefinition} from "../src/tools/native/read.js";

async function makeToolSession(): Promise<string> {
  const sessionDir = await mkdtemp(join(tmpdir(), "agent-harness-tools-"));
  await writeFile(
    join(sessionDir, "model.json"),
    JSON.stringify({model: "gpt-test"}),
  );
  await writeFile(
    join(sessionDir, "read.json"),
    JSON.stringify({schema_version: 1, tools: [nativeReadDefinition]}),
  );
  await writeFile(
    join(sessionDir, "setting.json"),
    JSON.stringify({
      cwd: sessionDir,
      tool_definition_paths: [join(sessionDir, "read.json")],
      tools: {
        native: {
          read: {
            max_bytes: 8,
            default_limit: 250,
          },
        },
      },
    }),
  );
  return sessionDir;
}

function outputText(value: unknown): string {
  if (typeof value !== "string") {
    throw new TypeError("expected tool output to be a string");
  }
  return value;
}

function firstLine(value: string): Record<string, unknown> {
  return JSON.parse(value.split("\n", 1)[0]!) as Record<string, unknown>;
}

test("loads native read only when selected and exposes an OpenAI-compatible definition", async () => {
  const sessionDir = await makeToolSession();
  try {
    const config = await new SessionConfigLoader(sessionDir).load();
    const catalog = await loadToolCatalog(config);
    assert.deepEqual(catalog.definitions().map((definition) => definition.id), ["native.read"]);
    assert.deepEqual(catalog.responseTools(), [
      {
        type: "function",
        name: "read",
        description: "Read workspace file content or inspect a directory.",
        parameters: nativeReadDefinition.inputSchema,
        strict: false,
      },
    ]);

    await writeFile(
      join(sessionDir, "setting.json"),
      JSON.stringify({
        cwd: sessionDir,
        tool_definition_paths: [join(sessionDir, "read.json")],
      }),
    );
    const disabledConfig = await new SessionConfigLoader(sessionDir).load();
    assert.deepEqual((await loadToolCatalog(disabledConfig)).definitions(), []);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("executes a minimal read call, applies the byte policy, and separates errors", async () => {
  const sessionDir = await makeToolSession();
  try {
    await writeFile(join(sessionDir, "sample.txt"), "one\ntwo\nthree\n", "utf8");
    const config = await new SessionConfigLoader(sessionDir).load();
    const catalog = await loadToolCatalog(config);

    const success = await catalog.executeFunctionCall(
      {
        type: "function_call",
        call_id: "read-success",
        name: "read",
        arguments: JSON.stringify({path: "sample.txt"}),
      },
      {workspaceRoot: sessionDir, sessionDir},
    );
    const successOutput = outputText(success.output);
    assert.equal(firstLine(successOutput).status, "ok");
    assert.match(successOutput, /--- CONTENT token=/);
    assert.match(successOutput, /one\ntwo/);
    assert.match(successOutput, /content_truncated: true/);
    assert.match(successOutput, /max_content_bytes: 8/);

    const schemaFailure = await catalog.executeFunctionCall(
      {
        type: "function_call",
        call_id: "read-schema-error",
        name: "read",
        arguments: JSON.stringify({}),
      },
      {workspaceRoot: sessionDir, sessionDir},
    );
    const schemaOutput = outputText(schemaFailure.output);
    assert.equal(firstLine(schemaOutput).status, "invalid_input");
    assert.match(schemaOutput, /error_type: schema/);
    assert.match(schemaOutput, /missingProperty/);

    const executionFailure = await catalog.executeFunctionCall(
      {
        type: "function_call",
        call_id: "read-execution-error",
        name: "read",
        arguments: JSON.stringify({path: "missing.txt"}),
      },
      {workspaceRoot: sessionDir, sessionDir},
    );
    const executionOutput = outputText(executionFailure.output);
    assert.equal(firstLine(executionOutput).status, "not_found");
    assert.match(executionOutput, /error_type: execution/);
    assert.match(executionOutput, /error_code: not_found/);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("executes native read calls between Responses API turns", async () => {
  const sessionDir = await makeToolSession();
  try {
    await writeFile(join(sessionDir, "sample.txt"), "native tool output\n", "utf8");
    const requests: Record<string, unknown>[] = [];
    let responseNumber = 0;
    const functionCall = {
      type: "function_call",
      call_id: "call_read",
      name: "read",
      arguments: JSON.stringify({path: "sample.txt"}),
    };
    const fakeClient = {
      responses: {
        create: async (request: unknown) => {
          responseNumber += 1;
          requests.push(request as Record<string, unknown>);
          const output = responseNumber === 1
            ? functionCall
            : {
                type: "message",
                role: "assistant",
                content: [{type: "output_text", text: "READ_DONE"}],
              };
          return (async function* () {
            yield {type: "response.output_item.done", output_index: 0, item: output};
            yield {
              type: "response.completed",
              response: {
                id: `response-${responseNumber}`,
                object: "response",
                status: "completed",
                model: "gpt-test",
                output: [output],
              },
            };
          })();
        },
      },
    } as unknown as OpenAI;

    const session = await AgentSession.open(sessionDir, {client: fakeClient});
    const result = await session.run("read the sample", {turnId: "read-turn"});
    assert.equal(result.response.event.response.id, "response-2");
    assert.equal(requests.length, 2);
    assert.deepEqual(requests[0]?.tools, [
      {
        type: "function",
        name: "read",
        description: "Read workspace file content or inspect a directory.",
        parameters: nativeReadDefinition.inputSchema,
        strict: false,
      },
    ]);
    assert.deepEqual(
      (requests[1]?.input as Array<Record<string, unknown>>).map((item) => item.type),
      ["message", "function_call", "function_call_output"],
    );
    assert.equal((requests[1]?.input as Array<Record<string, unknown>>)[2]?.call_id, "call_read");

    const messages = (await readFile(join(sessionDir, "messages.jsonl"), "utf8"))
      .trimEnd()
      .split("\n")
      .map((line) => JSON.parse(line) as Record<string, unknown>);
    assert.equal(messages.length, 4);
    assert.equal(messages[2]?.type, "function_call_output");
    await session.close();
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});
