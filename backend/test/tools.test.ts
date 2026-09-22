import assert from "node:assert/strict";
import {mkdir, mkdtemp, readFile, rm, writeFile} from "node:fs/promises";
import {tmpdir} from "node:os";
import {join} from "node:path";
import test from "node:test";

import type OpenAI from "openai";

import {DEFAULT_NATIVE_READ_MAX_FILE_BYTES, SessionConfigLoader} from "../src/config.js";
import {AgentSession} from "../src/session.js";
import {loadToolCatalog} from "../src/tools/index.js";
import {nativeReadDefinition} from "../src/tools/native/read.js";
import {
  firstLine,
  makeReadToolSession as makeToolSession,
  outputText,
  refreshReadSession,
} from "./tool-session.js";

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
        description: "Read file content or inspect a directory.",
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

test("rejects a definition document that changes a native tool schema", async () => {
  const sessionDir = await makeToolSession();
  try {
    const tampered = structuredClone(nativeReadDefinition);
    const properties = tampered.inputSchema.properties as Record<string, unknown>;
    (properties.path as Record<string, unknown>).minLength = 2;
    await writeFile(
      join(sessionDir, "read.json"),
      JSON.stringify({schema_version: 1, tools: [tampered]}),
    );
    await assert.rejects(
      loadToolCatalog(await new SessionConfigLoader(sessionDir).load()),
      /native\.read inputSchema must exactly match the native tool schema/,
    );
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
    assert.match(schemaOutput, /example_operation: \{"path":"\."\}/);

    const metadataOnly = await catalog.executeFunctionCall(
      {
        type: "function_call",
        call_id: "read-metadata-only",
        name: "read",
        arguments: JSON.stringify({path: "sample.txt", includeContent: false}),
      },
      {workspaceRoot: sessionDir, sessionDir},
    );
    const metadataOutput = outputText(metadataOnly.output);
    assert.equal(firstLine(metadataOutput).status, "ok");
    assert.match(metadataOutput, /metadata_only: true/);
    assert.match(metadataOutput, /size_bytes: 14/);
    assert.doesNotMatch(metadataOutput, /--- CONTENT token=/);
    assert.doesNotMatch(metadataOutput, /total_line_count:/);

    const absoluteSuccess = await catalog.executeFunctionCall(
      {
        type: "function_call",
        call_id: "read-absolute-success",
        name: "read",
        arguments: JSON.stringify({path: join(sessionDir, "sample.txt")}),
      },
      {workspaceRoot: sessionDir, sessionDir},
    );
    assert.equal(firstLine(outputText(absoluteSuccess.output)).status, "ok");

    const byteOffsetFailure = await catalog.executeFunctionCall(
      {
        type: "function_call",
        call_id: "read-byte-offset-eof",
        name: "read",
        arguments: JSON.stringify({
          path: "sample.txt",
          rangeType: "bytes",
          offset: 100,
          limit: 10,
        }),
      },
      {workspaceRoot: sessionDir, sessionDir},
    );
    const byteOffsetOutput = outputText(byteOffsetFailure.output);
    assert.equal(firstLine(byteOffsetOutput).status, "ok");
    assert.match(byteOffsetOutput, /No content; offset is at or beyond EOF/);
    assert.match(byteOffsetOutput, /returned_bytes: 0/);
    assert.match(byteOffsetOutput, /offset_clamped: true/);
    assert.match(byteOffsetOutput, /eof: true/);
    assert.doesNotMatch(byteOffsetOutput, /--- CONTENT token=/);

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

test("applies native read allow and deny path policies", async () => {
  const sessionDir = await makeToolSession();
  const externalRoot = await mkdtemp(join(tmpdir(), "agent-harness-read-policy-"));
  const outsidePath = join(externalRoot, "outside.txt");
  try {
    await writeFile(outsidePath, "outside content\n", "utf8");
    const readCall = (callId: string) => ({
      type: "function_call",
      call_id: callId,
      name: "read",
      arguments: JSON.stringify({path: outsidePath}),
    });
    const execute = async (paths: unknown[]) => {
      await refreshReadSession(sessionDir, {
        max_bytes: 65536,
        default_limit: 250,
        paths,
      });
      const config = await new SessionConfigLoader(sessionDir).load();
      const catalog = await loadToolCatalog(config);
      const result = await catalog.executeFunctionCall(
        readCall(`read-policy-${paths.length}`),
        {workspaceRoot: sessionDir, sessionDir},
      );
      return outputText(result.output);
    };

    const defaultDenied = await execute([]);
    assert.equal(firstLine(defaultDenied).status, "forbidden");
    assert.match(defaultDenied, /error_code: path_outside_workspace/);
    assert.match(defaultDenied, /path: \.\.\/outside\.txt/);
    assert.doesNotMatch(defaultDenied, new RegExp(externalRoot.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")));

    const rootAllowed = await execute([{action: "allow", path: "/"}]);
    assert.equal(firstLine(rootAllowed).status, "ok");

    const denyWins = await execute([
      {action: "allow", path: "/"},
      {action: "deny", path: outsidePath},
    ]);
    assert.equal(firstLine(denyWins).status, "forbidden");
    assert.match(denyWins, /error_code: path_denied/);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
    await rm(externalRoot, {recursive: true, force: true});
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
        description: "Read file content or inspect a directory.",
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

test("forwards the final configured tool-round result before stopping", async () => {
  const sessionDir = await makeToolSession();
  try {
    await writeFile(join(sessionDir, "sample.txt"), "native tool output\n", "utf8");
    await writeFile(
      join(sessionDir, "setting.json"),
      JSON.stringify({
        cwd: sessionDir,
        max_tool_rounds: 2,
        tool_definition_paths: [join(sessionDir, "read.json")],
        tools: {native: {read: {max_bytes: 8, default_limit: 250}}},
      }),
    );

    const requests: Record<string, unknown>[] = [];
    let responseNumber = 0;
    const fakeClient = {
      responses: {
        create: async (request: unknown) => {
          responseNumber += 1;
          requests.push(request as Record<string, unknown>);
          const output = responseNumber <= 2
            ? {
                type: "function_call",
                call_id: `call_read_${responseNumber}`,
                name: "read",
                arguments: JSON.stringify({path: "sample.txt"}),
              }
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
    const result = await session.run("read twice", {turnId: "round-limit-forward"});
    assert.equal(result.response.event.response.id, "response-3");
    assert.equal(requests.length, 3);
    assert.deepEqual(
      (requests[2]?.input as Array<Record<string, unknown>>).slice(-2).map((item) => item.type),
      ["function_call", "function_call_output"],
    );
    assert.equal(
      (requests[2]?.input as Array<Record<string, unknown>>).at(-1)?.call_id,
      "call_read_2",
    );
    await session.close();
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("enforces the configured and per-run hard tool-round limit", async () => {
  const sessionDir = await makeToolSession();
  try {
    await writeFile(join(sessionDir, "sample.txt"), "native tool output\n", "utf8");
    await writeFile(
      join(sessionDir, "setting.json"),
      JSON.stringify({
        cwd: sessionDir,
        max_tool_rounds: 4,
        tool_definition_paths: [join(sessionDir, "read.json")],
        tools: {native: {read: {max_bytes: 8, default_limit: 250}}},
      }),
    );

    const requests: Record<string, unknown>[] = [];
    let responseNumber = 0;
    const fakeClient = {
      responses: {
        create: async (request: unknown) => {
          responseNumber += 1;
          requests.push(request as Record<string, unknown>);
          const output = {
            type: "function_call",
            call_id: `call_read_${responseNumber}`,
            name: "read",
            arguments: JSON.stringify({path: "sample.txt"}),
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
    await assert.rejects(
      () => session.run("keep reading", {turnId: "round-limit-hard", maxToolRounds: 1}),
      /tool execution exceeded 1 tool rounds/,
    );
    assert.equal(requests.length, 2);
    assert.equal(
      (requests[1]?.input as Array<Record<string, unknown>>).at(-1)?.call_id,
      "call_read_1",
    );
    await session.close();
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

function readRunner(catalog: Awaited<ReturnType<typeof loadToolCatalog>>, sessionDir: string) {
  return async (callId: string, args: Record<string, unknown>) =>
    outputText(
      (
        await catalog.executeFunctionCall(
          {
            type: "function_call",
            call_id: callId,
            name: "read",
            arguments: JSON.stringify(args),
          },
          {workspaceRoot: sessionDir, sessionDir},
        )
      ).output,
    );
}

test("clamps a line offset below the start instead of rejecting it", async () => {
  const sessionDir = await makeToolSession({max_bytes: 4096});
  try {
    await writeFile(join(sessionDir, "sample.txt"), "one\ntwo\nthree\n", "utf8");
    const catalog = await loadToolCatalog(await new SessionConfigLoader(sessionDir).load());
    const output = await readRunner(catalog, sessionDir)("read-offset-zero", {
      path: "sample.txt",
      offset: 0,
    });
    assert.equal(firstLine(output).status, "ok");
    assert.match(output, /Read lines 1-3 from sample\.txt/);
    assert.match(output, /offset_clamped: true/);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("accepts near-miss enum spellings instead of rejecting them", async () => {
  const sessionDir = await makeToolSession({max_bytes: 4096});
  try {
    await writeFile(join(sessionDir, "sample.txt"), "one\ntwo\n", "utf8");
    const catalog = await loadToolCatalog(await new SessionConfigLoader(sessionDir).load());
    const run = readRunner(catalog, sessionDir);

    const dashed = await run("read-dashed-encoding", {
      path: "sample.txt",
      contentEncoding: "utf-8",
    });
    assert.equal(firstLine(dashed).status, "ok");
    assert.match(dashed, /encoding: utf8/);
    assert.match(dashed, /one\ntwo/);

    const shouted = await run("read-shouted-range", {
      path: "sample.txt",
      rangeType: "LINES",
      limit: 1,
    });
    assert.equal(firstLine(shouted).status, "ok");
    assert.match(shouted, /range_type: lines/);
    assert.match(shouted, /Read lines 1-1 from sample\.txt/);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("chooses base64 for binary content and reports why", async () => {
  const sessionDir = await makeToolSession({max_bytes: 4096});
  try {
    const binary = Buffer.from([0x89, 0x50, 0x4e, 0x47, 0xff, 0xfe]);
    await writeFile(join(sessionDir, "image.png"), binary);
    const catalog = await loadToolCatalog(await new SessionConfigLoader(sessionDir).load());
    const run = readRunner(catalog, sessionDir);

    const automatic = await run("read-binary-default", {path: "image.png"});
    assert.equal(firstLine(automatic).status, "ok");
    assert.match(automatic, /encoding: base64/);
    assert.match(automatic, /encoding_reason: invalid_utf8/);
    assert.ok(automatic.includes(binary.toString("base64")));

    const explicitBinary = await run("read-binary-explicit", {
      path: "image.png",
      contentEncoding: "base64",
    });
    assert.equal(firstLine(explicitBinary).status, "ok");
    assert.doesNotMatch(explicitBinary, /encoding_reason:/);

    const explicitText = await run("read-binary-as-text", {
      path: "image.png",
      contentEncoding: "utf8",
    });
    assert.equal(firstLine(explicitText).status, "unsupported_encoding");
    assert.match(explicitText, /error_code: binary_file/);
    assert.match(explicitText, /suggestion: omit contentEncoding/);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("lists a directory as a normal successful read", async () => {
  const sessionDir = await makeToolSession({max_bytes: 4096});
  try {
    await mkdir(join(sessionDir, "folder"));
    await writeFile(join(sessionDir, "folder/a.txt"), "a\n", "utf8");
    await writeFile(join(sessionDir, "folder/b.txt"), "bb\n", "utf8");
    const catalog = await loadToolCatalog(await new SessionConfigLoader(sessionDir).load());
    const output = await readRunner(catalog, sessionDir)("read-directory", {path: "folder"});
    assert.equal(firstLine(output).status, "ok");
    assert.match(output, /Listed directory folder/);
    assert.match(output, /kind: directory/);
    assert.match(output, /entry_count: 2/);
    assert.match(output, /"name":"a\.txt","kind":"file","size_bytes":2/);
    assert.doesNotMatch(output, /error_code/);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("does not disclose denied children when listing a directory", async () => {
  const sessionDir = await makeToolSession({
    max_bytes: 4096,
    default_limit: 1,
  });
  try {
    await refreshReadSession(sessionDir, {
      max_bytes: 4096,
      default_limit: 1,
      paths: [{action: "deny", path: join(sessionDir, "folder/a-secret.txt")}],
    });
    await mkdir(join(sessionDir, "folder"));
    await writeFile(join(sessionDir, "folder/a-secret.txt"), "secret\n", "utf8");
    await writeFile(join(sessionDir, "folder/b-visible.txt"), "visible\n", "utf8");
    const catalog = await loadToolCatalog(await new SessionConfigLoader(sessionDir).load());
    const output = await readRunner(catalog, sessionDir)("read-directory-policy", {path: "folder"});
    assert.equal(firstLine(output).status, "ok");
    assert.match(output, /"name":"b-visible\.txt"/);
    assert.doesNotMatch(output, /a-secret\.txt/);
    assert.match(output, /entry_count: 1/);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("refuses to load a file beyond max_file_bytes but still reports metadata", async () => {
  const sessionDir = await makeToolSession({max_bytes: 4096, max_file_bytes: 16});
  try {
    await writeFile(join(sessionDir, "big.txt"), "x".repeat(64), "utf8");
    const catalog = await loadToolCatalog(await new SessionConfigLoader(sessionDir).load());
    const run = readRunner(catalog, sessionDir);

    const blocked = await run("read-too-big", {path: "big.txt"});
    assert.equal(firstLine(blocked).status, "blocked");
    assert.match(blocked, /error_code: size_limit/);
    assert.match(blocked, /bytes: 64/);
    assert.match(blocked, /max_bytes: 16/);
    assert.match(blocked, /suggestion: read metadata with includeContent false/);

    const metadata = await run("read-too-big-metadata", {
      path: "big.txt",
      includeContent: false,
    });
    assert.equal(firstLine(metadata).status, "ok");
    assert.match(metadata, /metadata_only: true/);
    assert.match(metadata, /size_bytes: 64/);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("validates the read byte limits with configurable defaults", async () => {
  const sessionDir = await makeToolSession();
  try {
    const config = await new SessionConfigLoader(sessionDir).load();
    assert.equal(
      config.settings.tools.native.read?.max_file_bytes,
      DEFAULT_NATIVE_READ_MAX_FILE_BYTES,
    );
    for (const maxFileBytes of [0, 1.5, 1024 * 1024 * 1024 + 1]) {
      await refreshReadSession(sessionDir, {max_file_bytes: maxFileBytes});
      await assert.rejects(
        new SessionConfigLoader(sessionDir).load(),
        /tools\.native\.read\.max_file_bytes/,
      );
    }
    await refreshReadSession(sessionDir, {max_file_bytes: 1024});
    const limited = await new SessionConfigLoader(sessionDir).load();
    assert.equal(limited.settings.tools.native.read?.max_file_bytes, 1024);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});
