import assert from "node:assert/strict";
import {mkdtemp, rm, writeFile} from "node:fs/promises";
import {tmpdir} from "node:os";
import {join} from "node:path";
import test from "node:test";

import {CallToolRequestSchema, ListToolsRequestSchema} from "@modelcontextprotocol/sdk/types.js";
import {Server} from "@modelcontextprotocol/sdk/server/index.js";
import {StdioServerTransport} from "@modelcontextprotocol/sdk/server/stdio.js";

import {McpServerManager} from "../src/tools/mcp/manager.js";
import {ToolCatalog} from "../src/tools/catalog.js";
import {AgentSession} from "../src/session.js";
import {toolResultPartFromOutput} from "../src/plugins/web-ui/tool-read-web-ui.js";
import type {McpServerReference} from "../src/config.js";

async function withTempDir(run: (path: string) => Promise<void>): Promise<void> {
  const path = await mkdtemp(join(tmpdir(), "agent-harness-mcp-"));
  try { await run(path); } finally { await rm(path, {recursive: true, force: true}); }
}

const inProcessServerSource = `
import {Server} from "@modelcontextprotocol/sdk/server/index.js";
import {StdioServerTransport} from "@modelcontextprotocol/sdk/server/stdio.js";
import {CallToolRequestSchema, ListToolsRequestSchema} from "@modelcontextprotocol/sdk/types.js";
const server = new Server({name:"fixture", version:"1.0.0"}, {capabilities:{tools:{}}});
server.setRequestHandler(ListToolsRequestSchema, async () => ({tools:[
  {name:"echo", description:"Echo text and return an image; env=" + (process.env.MCP_TEST_SETTING ?? "unset"), inputSchema:{type:"object", properties:{text:{type:"string"}}, required:["text"]}},
  {name:"hidden", description:"must be denied", inputSchema:{type:"object", properties:{}, additionalProperties:false}}
]}));
server.setRequestHandler(CallToolRequestSchema, async (request) => ({content:[
  {type:"text", text:request.params.arguments.text},
  {type:"image", mimeType:"image/png", data:"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+/p9sAAAAASUVORK5CYII="},
  {type:"audio", mimeType:"audio/wav", data:"UklGRg=="},
  {type:"resource", resource:{uri:"file:///tmp/guide.pdf", mimeType:"application/pdf", blob:"JVBERi0xLjQ="}}
]}));
await server.connect(new StdioServerTransport());
`;

test("disabled MCP server is neither read nor started", async () => withTempDir(async (cwd) => {
  const manager = new McpServerManager({});
  const tools = await manager.loadTools([
    {id: "disabled", config_path: join(cwd, "missing.json"), enabled: false, deny_tools: []},
  ], cwd);
  assert.deepEqual(tools, []);
  await manager.close();
}));

test("connects to stdio MCP, denies selected tools, namespaces calls, and adapts text and image output", async () => withTempDir(async (cwd) => {
  const configPath = join(cwd, "server.json");
  const serverConfig = {
    schema_version: 1,
    transport: {type: "stdio", command: process.execPath, args: ["-e", inProcessServerSource], cwd: process.cwd(), env: {MCP_TEST_SETTING: "fixture-env-value"}},
    timeout_ms: 10_000,
    max_result_bytes: 4096,
  };
  await writeFile(configPath, JSON.stringify(serverConfig));
  const secondConfigPath = join(cwd, "second-server.json");
  await writeFile(secondConfigPath, JSON.stringify(serverConfig));
  const server: McpServerReference = {
    id: "fixture",
    config_path: configPath,
    enabled: true,
    deny_tools: ["hidden"],
  };
  const secondServer: McpServerReference = {
    id: "other",
    config_path: secondConfigPath,
    enabled: true,
    deny_tools: [],
  };
  const manager = new McpServerManager(process.env);
  try {
    const loaded = await manager.loadTools([server, secondServer], cwd);
    assert.equal(loaded.length, 3);
    const fixtureEcho = loaded.find((tool) => tool.definition.id === "mcp:fixture:echo");
    const otherEcho = loaded.find((tool) => tool.definition.id === "mcp:other:echo");
    assert.ok(fixtureEcho);
    assert.ok(otherEcho);
    assert.equal(fixtureEcho.definition.source, "mcp");
    assert.equal(fixtureEcho.definition.server, "fixture");
    assert.match(fixtureEcho.definition.name, /^mcp_fixture_echo_/);
    assert.match(fixtureEcho.definition.description, /fixture-env-value/);
    assert.match(otherEcho.definition.name, /^mcp_other_echo_/);
    assert.notEqual(fixtureEcho.definition.name, otherEcho.definition.name);
    assert.equal(loaded.some((tool) => tool.definition.id === "mcp:fixture:hidden"), false);
    assert.equal(loaded.some((tool) => tool.definition.id === "mcp:other:hidden"), true);
    const catalog = new ToolCatalog(loaded);
    const call = {
      type: "function_call",
      call_id: "call-1",
      name: fixtureEcho.definition.name,
      arguments: JSON.stringify({text: "hello from MCP"}),
    };
    const output = await catalog.executeFunctionCall(call, {workspaceRoot: cwd, sessionDir: cwd});
    assert.equal(output.type, "function_call_output");
    assert.equal(output.call_id, "call-1");
    assert.ok(Array.isArray(output.output));
    assert.equal(output.output[0]?.type, "input_text");
    assert.equal(output.output[0]?.text, "hello from MCP");
    assert.equal(output.output[1]?.type, "input_image");
    assert.match(String(output.output[1]?.image_url), /^data:image\/png;base64,/);
    assert.match(String(output.output[2]?.text), /audio input is not enabled/);
    assert.equal(output.output[3]?.type, "input_file");
    assert.equal(output.output[3]?.filename, "guide.pdf");
    const uiPart = toolResultPartFromOutput(output, 0);
    assert.ok(uiPart);
    assert.doesNotMatch(uiPart.output, /iVBOR|JVBERi0/);
    assert.match(uiPart.output, /MCP image content/);
    assert.equal(uiPart.status, "success");
    const largeOutput = await catalog.executeFunctionCall({
      ...call,
      call_id: "call-large",
      arguments: JSON.stringify({text: "x".repeat(10_000)}),
    }, {workspaceRoot: cwd, sessionDir: cwd});
    assert.ok(Array.isArray(largeOutput.output));
    assert.ok(Buffer.byteLength(JSON.stringify(largeOutput.output)) <= 4096);
    assert.match((largeOutput.output as Array<Record<string, unknown>>).map((part) => String(part.text ?? "")).join("\n"), /omitted at the configured max_result_bytes limit/);
  } finally {
    await manager.close();
  }
}));

test("passes MCP multimodal function output into the next Responses request", async () => withTempDir(async (cwd) => {
  const serverConfigPath = join(cwd, "server.json");
  await writeFile(serverConfigPath, JSON.stringify({
    transport: {type: "stdio", command: process.execPath, args: ["-e", inProcessServerSource], cwd: process.cwd()},
    max_result_bytes: 4096,
  }));
  await writeFile(join(cwd, "model.json"), JSON.stringify({model: "gpt-test"}));
  await writeFile(join(cwd, "setting.json"), JSON.stringify({
    cwd,
    mcp_servers: [{id: "fixture", config_path: serverConfigPath, enabled: true, deny_tools: ["hidden"]}],
  }));
  const requests: Record<string, unknown>[] = [];
  let requestIndex = 0;
  const fakeClient = {
    responses: {
      create: async (request: Record<string, unknown>) => {
        requests.push(request);
        const index = requestIndex++;
        const call = {
          type: "function_call",
          call_id: "mcp-call-1",
          name: String((request.tools as Array<Record<string, unknown>>)[0]?.name),
          arguments: JSON.stringify({text: "from session"}),
        };
        const message = {type: "message", role: "assistant", content: [{type: "output_text", text: "MCP result received"}]};
        return (async function* () {
          if (index === 0) {
            yield {type: "response.output_item.done", output_index: 0, item: call};
            yield {type: "response.completed", response: {id: "provider-tool-turn", status: "completed", model: "gpt-test", usage: {input_tokens: 10}, output: [call]}};
          } else {
            yield {type: "response.output_item.done", output_index: 0, item: message};
            yield {type: "response.completed", response: {id: "provider-final-turn", status: "completed", model: "gpt-test", usage: {input_tokens: 20}, output: [message]}};
          }
        })();
      },
    },
  } as unknown as import("openai").default;
  const session = await AgentSession.open(cwd, {client: fakeClient});
  try {
    const tools = await session.listTools();
    assert.equal(tools.length, 1);
    await session.run("Use the MCP echo tool");
    assert.equal(requests.length, 2);
    const nextInput = requests[1]?.input;
    assert.ok(Array.isArray(nextInput));
    const toolOutput = nextInput.find((item) => typeof item === "object" && item !== null && (item as Record<string, unknown>).type === "function_call_output") as Record<string, unknown> | undefined;
    assert.equal(toolOutput?.call_id, "mcp-call-1");
    assert.ok(Array.isArray(toolOutput?.output));
    assert.equal((toolOutput!.output as Array<Record<string, unknown>>)[1]?.type, "input_image");
    assert.equal((toolOutput!.output as Array<Record<string, unknown>>)[3]?.type, "input_file");
  } finally {
    await session.close();
  }
}));

test("rejects missing environment references without exposing secret values", async () => withTempDir(async (cwd) => {
  const configPath = join(cwd, "server.json");
  await writeFile(configPath, JSON.stringify({
    transport: {type: "streamable_http", url: "https://mcp.example.test/mcp", headers_from: {Authorization: "MCP_AUTH_TOKEN"}},
  }));
  const manager = new McpServerManager({});
  try {
    await assert.rejects(
      () => manager.loadTools([{id: "remote", config_path: configPath, enabled: true, deny_tools: []}], cwd),
      /MCP_AUTH_TOKEN.*is not set/,
    );
  } finally {
    await manager.close();
  }
}));
