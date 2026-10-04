import {createHash} from "node:crypto";
import {readFile, stat} from "node:fs/promises";
import {basename, resolve} from "node:path";

import {Ajv} from "ajv";
import {Ajv2020} from "ajv/dist/2020.js";
import {Client} from "@modelcontextprotocol/sdk/client/index.js";
import {StdioClientTransport, getDefaultEnvironment} from "@modelcontextprotocol/sdk/client/stdio.js";
import {StreamableHTTPClientTransport} from "@modelcontextprotocol/sdk/client/streamableHttp.js";
import type {ResponseFunctionCallOutputItemList} from "openai/resources/responses/responses.js";

import {AsyncMutex} from "../../concurrency.js";
import {ConfigError, type McpServerReference} from "../../config.js";
import type {JsonRecord} from "../../persistence.js";
import type {LoadedTool, ToolDefinition, ToolExecutionContext} from "../types.js";

const DEFAULT_TIMEOUT_MS = 30_000;
const MAX_TIMEOUT_MS = 10 * 60_000;
const DEFAULT_MAX_RESULT_BYTES = 1_048_576;
const MIN_MAX_RESULT_BYTES = 512;
const MAX_MAX_RESULT_BYTES = 16 * 1024 * 1024;
const DRAFT_2020_12_SCHEMA_URI = "https://json-schema.org/draft/2020-12/schema";
const ajv = new Ajv({allErrors: true, strict: false, coerceTypes: false, removeAdditional: false});
const ajv2020 = new Ajv2020({allErrors: true, strict: false, coerceTypes: false, removeAdditional: false});

// Ajv defaults to Draft 7; select the declared dialect rather than dropping $schema and risking ignored keywords.
function validatorForInputSchema(schema: Record<string, unknown>): Ajv | Ajv2020 {
  const dialect = schema.$schema;
  if (dialect === DRAFT_2020_12_SCHEMA_URI || dialect === `${DRAFT_2020_12_SCHEMA_URI}#`) {
    return ajv2020;
  }
  return ajv;
}

interface StdioConfig {
  type: "stdio";
  command: string;
  args: string[];
  cwd: string;
  env: Record<string, string>;
  env_from: Record<string, string>;
}

interface HttpConfig {
  type: "streamable_http";
  url: URL;
  headers_from: Record<string, string>;
}

interface LoadedMcpConfig {
  transport: StdioConfig | HttpConfig;
  timeout_ms: number;
  max_result_bytes: number;
  fingerprint: string;
}

interface Connection {
  fingerprint: string;
  client: Client;
  transport: StdioClientTransport | StreamableHTTPClientTransport;
  timeoutMs: number;
  maxResultBytes: number;
}

function isRecord(value: unknown): value is JsonRecord {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function configError(path: string, message: string): ConfigError {
  return new ConfigError(`${path}: ${message}`);
}

function safeErrorType(error: unknown): string {
  const name = error instanceof Error ? error.name : "Error";
  return /^[A-Za-z][A-Za-z0-9_]{0,63}$/.test(name) ? name : "Error";
}

function requiredString(value: unknown, path: string): string {
  if (typeof value !== "string" || value.trim().length === 0) {
    throw new ConfigError(`${path} must be a non-empty string`);
  }
  return value.trim();
}

function normalizeEnvironment(value: unknown, path: string): Record<string, string> {
  if (value === undefined) return {};
  if (!isRecord(value)) throw new ConfigError(`${path} must be an object mapping environment variable names to values`);
  const normalized: Record<string, string> = {};
  for (const [name, entry] of Object.entries(value)) {
    if (!/^[A-Za-z_][A-Za-z0-9_]*$/.test(name) || typeof entry !== "string") {
      throw new ConfigError(`${path} must contain environment variable names and string values`);
    }
    normalized[name] = entry;
  }
  return normalized;
}

function normalizeStringMap(
  value: unknown,
  path: string,
  targetPattern = /^[A-Za-z_][A-Za-z0-9_]*$/,
): Record<string, string> {
  if (value === undefined) return {};
  if (!isRecord(value)) throw new ConfigError(`${path} must be an object mapping target names to environment variable names`);
  const normalized: Record<string, string> = {};
  for (const [target, sourceValue] of Object.entries(value)) {
    const source = requiredString(sourceValue, `${path}.${target}`);
    if (!targetPattern.test(target) || !/^[A-Za-z_][A-Za-z0-9_]*$/.test(source)) {
      throw new ConfigError(`${path} keys and values must be environment variable names`);
    }
    normalized[target] = source;
  }
  return normalized;
}

async function normalizeConfig(
  value: unknown,
  reference: McpServerReference,
  cwd: string,
  environment: NodeJS.ProcessEnv,
): Promise<LoadedMcpConfig> {
  const path = reference.config_path;
  if (!isRecord(value)) throw configError(path, "expected a JSON object");
  if (value.schema_version !== undefined && value.schema_version !== 1) {
    throw configError(path, `unsupported schema_version ${String(value.schema_version)}`);
  }
  if (!isRecord(value.transport)) throw configError(path, "transport must be an object");
  const rawTransport = value.transport;
  let transport: StdioConfig | HttpConfig;
  let fingerprintTransport: JsonRecord;
  if (rawTransport.type === "stdio") {
    const command = requiredString(rawTransport.command, `${path}: transport.command`);
    const argsValue = rawTransport.args ?? [];
    if (!Array.isArray(argsValue) || argsValue.some((arg) => typeof arg !== "string")) {
      throw configError(path, "transport.args must be an array of strings");
    }
    const serverCwd = rawTransport.cwd === undefined ? cwd : resolve(cwd, requiredString(rawTransport.cwd, `${path}: transport.cwd`));
    let cwdInfo;
    try { cwdInfo = await stat(serverCwd); } catch { throw configError(path, `stdio cwd does not exist: ${serverCwd}`); }
    if (!cwdInfo.isDirectory()) throw configError(path, `stdio cwd is not a directory: ${serverCwd}`);
    const env = normalizeEnvironment(rawTransport.env, `${path}: transport.env`);
    const envFrom = normalizeStringMap(rawTransport.env_from, `${path}: transport.env_from`);
    if (Object.keys(env).some((name) => Object.hasOwn(envFrom, name))) {
      throw configError(path, "transport.env and transport.env_from must not set the same variable");
    }
    const resolvedEnv: Record<string, string> = {...env};
    for (const [target, source] of Object.entries(envFrom)) {
      const secret = environment[source];
      if (typeof secret !== "string" || secret.length === 0) {
        throw configError(path, `environment variable ${source} required by transport.env_from.${target} is not set`);
      }
      resolvedEnv[target] = secret;
    }
    transport = {type: "stdio", command, args: [...argsValue] as string[], cwd: serverCwd, env, env_from: envFrom};
    fingerprintTransport = {type: "stdio", command, args: argsValue, cwd: serverCwd, env: resolvedEnv};
  } else if (rawTransport.type === "streamable_http") {
    const urlText = requiredString(rawTransport.url, `${path}: transport.url`);
    let url: URL;
    try { url = new URL(urlText); } catch { throw configError(path, "transport.url must be a valid absolute URL"); }
    if ((url.protocol !== "https:" && url.protocol !== "http:") || url.username || url.password || url.hash) {
      throw configError(path, "transport.url must use HTTP(S) and must not contain credentials or a fragment");
    }
    const headersFrom = normalizeStringMap(
      rawTransport.headers_from,
      `${path}: transport.headers_from`,
      /^[!#$%&'*+.^_`|~0-9A-Za-z-]+$/,
    );
    const resolvedHeaders: Record<string, string> = {};
    for (const [header, source] of Object.entries(headersFrom)) {
      const secret = environment[source];
      if (typeof secret !== "string" || secret.length === 0) {
        throw configError(path, `environment variable ${source} required by transport.headers_from.${header} is not set`);
      }
      resolvedHeaders[header] = secret;
    }
    transport = {type: "streamable_http", url, headers_from: headersFrom};
    fingerprintTransport = {type: "streamable_http", url: url.toString(), headers: resolvedHeaders};
  } else {
    throw configError(path, "transport.type must be stdio or streamable_http");
  }
  const timeoutMs = value.timeout_ms ?? DEFAULT_TIMEOUT_MS;
  if (!Number.isSafeInteger(timeoutMs) || (timeoutMs as number) < 1 || (timeoutMs as number) > MAX_TIMEOUT_MS) {
    throw configError(path, `timeout_ms must be an integer from 1 to ${MAX_TIMEOUT_MS}`);
  }
  const maxResultBytes = value.max_result_bytes ?? DEFAULT_MAX_RESULT_BYTES;
  if (!Number.isSafeInteger(maxResultBytes) || (maxResultBytes as number) < MIN_MAX_RESULT_BYTES || (maxResultBytes as number) > MAX_MAX_RESULT_BYTES) {
    throw configError(path, `max_result_bytes must be an integer from ${MIN_MAX_RESULT_BYTES} to ${MAX_MAX_RESULT_BYTES}`);
  }
  const fingerprint = createHash("sha256").update(JSON.stringify({
    transport: fingerprintTransport,
    timeout_ms: timeoutMs,
    max_result_bytes: maxResultBytes,
  })).digest("hex");
  return {transport, timeout_ms: timeoutMs as number, max_result_bytes: maxResultBytes as number, fingerprint};
}

function exposedToolName(serverId: string, toolName: string): string {
  const slug = (value: string) => value.toLowerCase().replace(/[^a-z0-9_-]+/g, "_").replace(/^_+|_+$/g, "") || "tool";
  const digest = createHash("sha256").update(`${serverId}\0${toolName}`).digest("hex").slice(0, 10);
  const readable = `mcp_${slug(serverId)}_${slug(toolName)}`;
  return `${readable.slice(0, 53)}_${digest}`;
}

function validMimeType(value: unknown): value is string {
  return typeof value === "string" && /^[a-zA-Z0-9!#$&^_.+-]+\/[a-zA-Z0-9!#$&^_.+-]+$/.test(value);
}

function validBase64(value: unknown): value is string {
  if (typeof value !== "string" || value.length === 0 || value.length % 4 !== 0) return false;
  return /^(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$/.test(value);
}

function resourceFilename(uri: unknown): string {
  if (typeof uri !== "string") return "mcp-resource.bin";
  const name = basename(uri.split(/[?#]/, 1)[0] ?? "").replace(/[^A-Za-z0-9._-]/g, "_");
  return name || "mcp-resource.bin";
}

function contentParts(result: unknown): ResponseFunctionCallOutputItemList {
  if (!isRecord(result)) return [{type: "input_text", text: "MCP server returned an invalid tool result."}];
  const parts: ResponseFunctionCallOutputItemList = [];
  if (result.isError === true) parts.push({type: "input_text", text: "MCP server reported a tool error."});
  if (Array.isArray(result.content)) {
    for (const block of result.content) {
      if (!isRecord(block)) continue;
      if (block.type === "text" && typeof block.text === "string") {
        parts.push({type: "input_text", text: block.text});
      } else if (block.type === "image") {
        if (validBase64(block.data) && validMimeType(block.mimeType) && block.mimeType.startsWith("image/")) {
          parts.push({type: "input_image", image_url: `data:${block.mimeType};base64,${block.data}`, detail: "auto"});
        } else {
          parts.push({type: "input_text", text: "MCP image content was omitted because its MIME type or base64 data was invalid."});
        }
      } else if (block.type === "resource" && isRecord(block.resource)) {
        const resource = block.resource;
        if (typeof resource.text === "string") {
          parts.push({type: "input_text", text: `${typeof resource.uri === "string" ? `Resource ${resource.uri}:\n` : ""}${resource.text}`});
        } else if (validBase64(resource.blob) && validMimeType(resource.mimeType)) {
          if (resource.mimeType.startsWith("image/")) {
            parts.push({type: "input_image", image_url: `data:${resource.mimeType};base64,${resource.blob}`, detail: "auto"});
          } else if (resource.mimeType.startsWith("audio/")) {
            parts.push({type: "input_text", text: `MCP audio resource omitted (MIME type ${resource.mimeType}); audio input is not enabled.`});
          } else {
            parts.push({type: "input_file", file_data: resource.blob, filename: resourceFilename(resource.uri)});
          }
        } else {
          parts.push({type: "input_text", text: "MCP embedded resource had no supported text or binary content."});
        }
      } else if (block.type === "resource_link") {
        const label = typeof block.name === "string" ? block.name : "MCP resource";
        const uri = typeof block.uri === "string" ? block.uri : "";
        const mime = typeof block.mimeType === "string" ? ` (${block.mimeType})` : "";
        parts.push({type: "input_text", text: `${label}${mime}${uri ? `: ${uri}` : ""}`});
      } else if (block.type === "audio") {
        parts.push({type: "input_text", text: `MCP audio content omitted (MIME type ${typeof block.mimeType === "string" ? block.mimeType : "unknown"}); audio input is not enabled.`});
      } else {
        parts.push({type: "input_text", text: `Unsupported MCP content block: ${String(block.type ?? "unknown")}.`});
      }
    }
  }
  if (result.structuredContent !== undefined) {
    let serialized: string;
    try { serialized = JSON.stringify(result.structuredContent); } catch { serialized = "[unserializable structured content]"; }
    parts.push({type: "input_text", text: `Structured content:\n${serialized}`});
  }
  return parts.length > 0 ? parts : [{type: "input_text", text: "MCP tool returned no content."}];
}

function truncateUtf8(value: string, maxBytes: number): string {
  if (maxBytes <= 0) return "";
  const bytes = Buffer.from(value, "utf8");
  if (bytes.byteLength <= maxBytes) return value;
  let end = maxBytes;
  while (end > 0) {
    const truncated = bytes.subarray(0, end).toString("utf8");
    if (Buffer.byteLength(truncated, "utf8") <= maxBytes) return truncated;
    end -= 1;
  }
  return "";
}

function boundedContent(result: unknown, maxBytes: number): ResponseFunctionCallOutputItemList {
  const parts = contentParts(result);
  const bounded: ResponseFunctionCallOutputItemList = [];
  let used = 0;
  const notice = "[Additional MCP result content omitted at the configured max_result_bytes limit.]";
  const noticeBytes = Buffer.byteLength(JSON.stringify({type: "input_text", text: notice}));
  for (const part of parts) {
    const size = Buffer.byteLength(JSON.stringify(part));
    if (used + size + noticeBytes > maxBytes) {
      if (part.type === "input_text" && bounded.length === 0) {
        const room = Math.max(0, Math.floor((maxBytes - used - noticeBytes - 128) / 6));
        const text = truncateUtf8(part.text, room);
        if (text.length > 0) {
          bounded.push({type: "input_text", text});
          used = Buffer.byteLength(JSON.stringify({type: "input_text", text}));
        }
      }
      if (used + noticeBytes <= maxBytes) bounded.push({type: "input_text", text: notice});
      break;
    }
    bounded.push(part);
    used += size;
  }
  return bounded.length > 0 ? bounded : [{type: "input_text", text: notice}];
}

export class McpServerManager {
  private readonly mutex = new AsyncMutex();
  private readonly connections = new Map<string, Connection>();

  public constructor(private readonly environment: NodeJS.ProcessEnv = process.env) {}

  public async loadTools(
    servers: readonly McpServerReference[],
    cwd: string,
  ): Promise<LoadedTool[]> {
    return this.mutex.runExclusive(async () => {
      const enabled = servers.filter((server) => server.enabled);
      const enabledIds = new Set(enabled.map((server) => server.id));
      for (const [id, connection] of this.connections) {
        if (!enabledIds.has(id)) {
          await connection.client.close().catch(() => undefined);
          this.connections.delete(id);
        }
      }
      const tools: LoadedTool[] = [];
      for (const server of enabled) {
        const connection = await this.getConnection(server, cwd);
        let listed;
        try {
          listed = await connection.client.listTools({}, {timeout: connection.timeoutMs});
        } catch (error) {
          if (this.connections.get(server.id) === connection) this.connections.delete(server.id);
          await connection.client.close().catch(() => undefined);
          throw configError(server.config_path, `failed to list tools from MCP server ${server.id} (${safeErrorType(error)})`);
        }
        const denied = new Set(server.deny_tools);
        const seen = new Set<string>();
        for (const remoteTool of listed.tools) {
          if (typeof remoteTool.name !== "string" || remoteTool.name.trim().length === 0) {
            throw configError(server.config_path, `MCP server ${server.id} returned a tool with no name`);
          }
          if (denied.has(remoteTool.name)) continue;
          if (seen.has(remoteTool.name)) throw configError(server.config_path, `MCP server ${server.id} returned duplicate tool ${remoteTool.name}`);
          seen.add(remoteTool.name);
          if (!isRecord(remoteTool.inputSchema) || remoteTool.inputSchema.type !== "object") {
            throw configError(server.config_path, `MCP tool ${server.id}/${remoteTool.name} must provide an object inputSchema`);
          }
          let validate;
          try { validate = validatorForInputSchema(remoteTool.inputSchema).compile(remoteTool.inputSchema); } catch (error) {
            throw configError(server.config_path, `invalid inputSchema for ${server.id}/${remoteTool.name}: ${error instanceof Error ? error.message : String(error)}`);
          }
          const name = exposedToolName(server.id, remoteTool.name);
          const definition: ToolDefinition = {
            id: `mcp:${server.id}:${remoteTool.name}`,
            source: "mcp",
            server: server.id,
            name,
            description: typeof remoteTool.description === "string" ? remoteTool.description : `MCP tool ${remoteTool.name} from ${server.id}`,
            inputSchema: remoteTool.inputSchema,
          };
          const client = connection.client;
          tools.push({
            definition,
            validate,
            execute: async (args: JsonRecord, context: ToolExecutionContext) => {
              let result: unknown;
              try {
                result = await client.callTool(
                  {name: remoteTool.name, arguments: args},
                  undefined,
                  {timeout: connection.timeoutMs, signal: context.signal},
                );
              } catch (error) {
                return [{type: "input_text", text: JSON.stringify({status: "error", server: server.id, tool: remoteTool.name, error: safeErrorType(error)})}];
              }
              return boundedContent(result, connection.maxResultBytes);
            },
          });
        }
      }
      return tools;
    });
  }

  private async getConnection(server: McpServerReference, cwd: string): Promise<Connection> {
    let raw: unknown;
    try { raw = JSON.parse(await readFile(server.config_path, "utf8")); } catch (error) {
      throw configError(server.config_path, `could not read MCP server config: ${error instanceof Error ? error.message : String(error)}`);
    }
    const config = await normalizeConfig(raw, server, cwd, this.environment);
    const current = this.connections.get(server.id);
    if (current?.fingerprint === config.fingerprint) return current;
    if (current) {
      await current.client.close().catch(() => undefined);
      this.connections.delete(server.id);
    }
    let transport: Connection["transport"];
    if (config.transport.type === "stdio") {
      const env = {...getDefaultEnvironment()};
      for (const [target, source] of Object.entries(config.transport.env_from)) {
        const value = this.environment[source];
        if (typeof value !== "string" || value.length === 0) throw configError(server.config_path, `environment variable ${source} is not set`);
        env[target] = value;
      }
      Object.assign(env, config.transport.env);
      transport = new StdioClientTransport({
        command: config.transport.command,
        args: config.transport.args,
        cwd: config.transport.cwd,
        env,
      });
    } else {
      const headers: Record<string, string> = {};
      for (const [header, source] of Object.entries(config.transport.headers_from)) {
        const value = this.environment[source];
        if (typeof value !== "string" || value.length === 0) throw configError(server.config_path, `environment variable ${source} is not set`);
        headers[header] = value;
      }
      transport = new StreamableHTTPClientTransport(config.transport.url, {requestInit: {headers}});
    }
    const client = new Client({name: `agent-harness-${server.id}`, version: "1.0.0"});
    try {
      await client.connect(transport, {timeout: config.timeout_ms});
    } catch (error) {
      await client.close().catch(() => undefined);
      throw configError(server.config_path, `failed to connect MCP server ${server.id} (${safeErrorType(error)})`);
    }
    const connection: Connection = {
      fingerprint: config.fingerprint,
      client,
      transport,
      timeoutMs: config.timeout_ms,
      maxResultBytes: config.max_result_bytes,
    };
    this.connections.set(server.id, connection);
    return connection;
  }

  public async close(): Promise<void> {
    await this.mutex.runExclusive(async () => {
      const connections = [...this.connections.values()];
      this.connections.clear();
      await Promise.all(connections.map(async ({client}) => client.close().catch(() => undefined)));
    });
  }
}
