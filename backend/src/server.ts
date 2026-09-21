import { randomUUID } from "node:crypto";
import { existsSync } from "node:fs";
import { readdir, readFile } from "node:fs/promises";
import {
  createServer,
  type IncomingMessage,
  type Server,
  type ServerResponse,
} from "node:http";
import { fileURLToPath } from "node:url";
import { join, resolve } from "node:path";

import type OpenAI from "openai";

import {
  ConfigError,
  SessionConfigLoader,
  type LoadedSessionConfig,
} from "./config.js";
import { AgentSession } from "./session.js";
import type { JsonRecord } from "./persistence.js";
import {
  normalizeWebUiSession,
  WebUiNormalizedRun,
  type WebUiSessionSnapshot,
} from "./plugins/web-ui-normalized.js";

const DEFAULT_PORT = 8787;
const DEFAULT_HOST = "127.0.0.1";
const MAX_BODY_BYTES = 1024 * 1024;
const SESSION_ID_PATTERN = /^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$/;

export interface AgentServerOptions {
  sessionsDir?: string;
  client?: OpenAI;
  environment?: NodeJS.ProcessEnv;
  host?: string;
  port?: number;
}

export interface RunningAgentServer {
  server: Server;
  close(): Promise<void>;
}

class HttpError extends Error {
  public constructor(
    public readonly statusCode: number,
    message: string,
  ) {
    super(message);
    this.name = "HttpError";
  }
}

interface SessionHandle {
  directory: string;
  session: AgentSession;
  loader: SessionConfigLoader;
}

function isRecord(value: unknown): value is JsonRecord {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function defaultSessionsDirectory(): string {
  const configured = process.env.JAD_SESSIONS_DIR?.trim();
  if (configured) {
    return resolve(configured);
  }

  const candidates = [
    resolve(process.cwd(), ".jad/sessions"),
    resolve(process.cwd(), "../.jad/sessions"),
  ];
  return candidates.find((candidate) => existsSync(candidate)) ?? candidates[0]!;
}

function validateSessionId(value: string): string {
  if (!SESSION_ID_PATTERN.test(value) || value === "." || value === "..") {
    throw new HttpError(400, "invalid session id");
  }
  return value;
}

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

function sendJson(response: ServerResponse, statusCode: number, value: unknown): void {
  if (response.writableEnded) {
    return;
  }
  const body = JSON.stringify(value);
  response.writeHead(statusCode, {
    "Content-Type": "application/json; charset=utf-8",
    "Cache-Control": "no-store",
    "Content-Length": Buffer.byteLength(body),
  });
  response.end(body);
}

function writeSse(response: ServerResponse, event: string, value: unknown): void {
  if (response.writableEnded || response.destroyed) {
    return;
  }
  try {
    response.write(`event: ${event}\ndata: ${JSON.stringify(value)}\n\n`);
  } catch {
    // A disconnected browser must not abort the provider request or persistence.
  }
}

async function readJsonBody(request: IncomingMessage): Promise<unknown> {
  const chunks: Buffer[] = [];
  let size = 0;
  for await (const chunk of request) {
    const buffer = Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk);
    size += buffer.length;
    if (size > MAX_BODY_BYTES) {
      throw new HttpError(413, "request body is too large");
    }
    chunks.push(buffer);
  }

  const text = Buffer.concat(chunks).toString("utf8");
  if (text.trim().length === 0) {
    throw new HttpError(400, "request body must be JSON");
  }
  try {
    return JSON.parse(text);
  } catch {
    throw new HttpError(400, "request body contains invalid JSON");
  }
}

async function readJsonl(path: string): Promise<JsonRecord[]> {
  let contents: string;
  try {
    contents = await readFile(path, "utf8");
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") {
      return [];
    }
    throw error;
  }

  const records: JsonRecord[] = [];
  for (const line of contents.split(/\r?\n/)) {
    if (line.trim().length === 0) {
      continue;
    }
    let value: unknown;
    try {
      value = JSON.parse(line);
    } catch (error) {
      throw new Error(`invalid messages.jsonl record: ${errorMessage(error)}`);
    }
    if (!isRecord(value)) {
      throw new Error("messages.jsonl record must be a JSON object");
    }
    records.push(value);
  }
  return records;
}

function sessionSummary(
  id: string,
  config: LoadedSessionConfig,
  messages: JsonRecord[],
): WebUiSessionSnapshot {
  return normalizeWebUiSession(id, config, messages);
}

class AgentHttpApplication {
  private readonly sessions = new Map<string, Promise<SessionHandle>>();
  private readonly sessionsDir: string;
  private readonly sessionOptions: {
    client?: OpenAI;
    environment?: NodeJS.ProcessEnv;
  };

  public constructor(options: AgentServerOptions = {}) {
    this.sessionsDir = resolve(options.sessionsDir ?? defaultSessionsDirectory());
    this.sessionOptions = {
      ...(options.client ? { client: options.client } : {}),
      ...(options.environment ? { environment: options.environment } : {}),
    };
  }

  public async handle(request: IncomingMessage, response: ServerResponse): Promise<void> {
    response.setHeader("Access-Control-Allow-Origin", "*");
    response.setHeader("Access-Control-Allow-Headers", "Content-Type");
    response.setHeader("Access-Control-Allow-Methods", "GET, POST, OPTIONS");

    if (request.method === "OPTIONS") {
      response.writeHead(204);
      response.end();
      return;
    }

    try {
      const url = new URL(request.url ?? "/", "http://agent-harness.local");
      const segments = url.pathname
        .split("/")
        .filter((segment) => segment.length > 0)
        .map((segment) => {
          try {
            return decodeURIComponent(segment);
          } catch {
            throw new HttpError(400, "invalid URL encoding");
          }
        });

      if (request.method === "GET" && url.pathname === "/api/health") {
        sendJson(response, 200, { ok: true });
        return;
      }

      if (segments[0] !== "api" || segments[1] !== "sessions") {
        throw new HttpError(404, "not found");
      }

      if (request.method === "GET" && segments.length === 2) {
        await this.handleSessionList(response);
        return;
      }

      const sessionId = validateSessionId(segments[2] ?? "");
      if (request.method === "GET" && segments.length === 3) {
        await this.handleSessionSnapshot(sessionId, response);
        return;
      }

      if (
        request.method === "GET" &&
        segments.length === 4 &&
        segments[3] === "tools"
      ) {
        await this.handleTools(sessionId, response);
        return;
      }

      if (
        request.method === "POST" &&
        segments.length === 4 &&
        segments[3] === "run"
      ) {
        await this.handleRun(sessionId, request, response);
        return;
      }

      throw new HttpError(404, "not found");
    } catch (error) {
      if (response.headersSent) {
        if (!response.writableEnded && !response.destroyed) {
          writeSse(response, "error", { message: errorMessage(error) });
          response.end();
        }
        return;
      }

      if (error instanceof ConfigError) {
        sendJson(response, 422, { error: error.message });
        return;
      }
      if (error instanceof HttpError) {
        sendJson(response, error.statusCode, { error: error.message });
        return;
      }
      console.error(error);
      sendJson(response, 500, { error: "internal server error" });
    }
  }

  public async close(): Promise<void> {
    const handles = await Promise.allSettled(this.sessions.values());
    await Promise.all(
      handles.flatMap((result) =>
        result.status === "fulfilled" ? [result.value.session.close()] : [],
      ),
    );
  }

  private async getSession(id: string): Promise<SessionHandle> {
    const existing = this.sessions.get(id);
    if (existing) {
      return existing;
    }

    const directory = join(this.sessionsDir, id);
    const pending = (async (): Promise<SessionHandle> => {
      const session = await AgentSession.open(directory, this.sessionOptions);
      return {
        directory,
        session,
        loader: new SessionConfigLoader(directory),
      };
    })();
    this.sessions.set(id, pending);
    try {
      return await pending;
    } catch (error) {
      this.sessions.delete(id);
      throw error;
    }
  }

  private async handleSessionList(response: ServerResponse): Promise<void> {
    let entries;
    try {
      entries = await readdir(this.sessionsDir, { withFileTypes: true });
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code === "ENOENT") {
        sendJson(response, 200, { sessions: [] });
        return;
      }
      throw error;
    }

    const sessions: WebUiSessionSnapshot[] = [];
    for (const entry of entries) {
      if (!entry.isDirectory() || !SESSION_ID_PATTERN.test(entry.name)) {
        continue;
      }
      try {
        const handle = await this.getSession(entry.name);
        const config = await handle.loader.load();
        const messages = await readJsonl(join(handle.directory, "messages.jsonl"));
        sessions.push(sessionSummary(entry.name, config, messages));
      } catch (error) {
        if (error instanceof ConfigError) {
          continue;
        }
        throw error;
      }
    }
    sessions.sort((left, right) => String(left.id).localeCompare(String(right.id)));
    sendJson(response, 200, { sessions });
  }

  private async handleSessionSnapshot(
    id: string,
    response: ServerResponse,
  ): Promise<void> {
    const handle = await this.getSession(id);
    const config = await handle.loader.load();
    const messages = await readJsonl(join(handle.directory, "messages.jsonl"));
    sendJson(response, 200, sessionSummary(id, config, messages));
  }

  private async handleTools(
    id: string,
    response: ServerResponse,
  ): Promise<void> {
    const handle = await this.getSession(id);
    sendJson(response, 200, {tools: await handle.session.listTools()});
  }

  private async handleRun(
    id: string,
    request: IncomingMessage,
    response: ServerResponse,
  ): Promise<void> {
    const body = await readJsonBody(request);
    if (!isRecord(body) || typeof body.input !== "string" || body.input.trim().length === 0) {
      throw new HttpError(400, "input must be a non-empty string");
    }

    const handle = await this.getSession(id);
    response.writeHead(200, {
      "Content-Type": "text/event-stream; charset=utf-8",
      "Cache-Control": "no-cache, no-store, must-revalidate",
      Connection: "keep-alive",
      "X-Accel-Buffering": "no",
    });
    response.flushHeaders();

    let connected = true;
    response.once("close", () => {
      connected = false;
    });

    try {
      const turnId = `web-${randomUUID()}`;
      const normalizedRun = new WebUiNormalizedRun(turnId);
      const result = await handle.session.run(body.input, {
        turnId,
        onEvent: (event) => {
          if (!connected) {
            return;
          }
          const message = normalizedRun.update(event);
          if (message) {
            writeSse(response, "ui.message", { message });
          }
        },
        onToolResult: (item) => {
          if (!connected) {
            return;
          }
          const message = normalizedRun.toolResult(item);
          if (message) {
            writeSse(response, "ui.message", { message });
          }
        },
      });

      if (connected) {
        const message = normalizedRun.complete(result.response.event.response);
        writeSse(response, "ui.message", { message });
        writeSse(response, "completed", {
          message,
          committed: true,
        });
        writeSse(response, "done", {});
        response.end();
      }
    } catch (error) {
      if (connected) {
        writeSse(response, "error", { message: errorMessage(error) });
        response.end();
      }
    }
  }
}

export function createAgentServer(options: AgentServerOptions = {}): RunningAgentServer {
  const application = new AgentHttpApplication(options);
  const server = createServer((request, response) => {
    void application.handle(request, response);
  });

  return {
    server,
    async close(): Promise<void> {
      await new Promise<void>((resolveClose, reject) => {
        if (!server.listening) {
          resolveClose();
          return;
        }
        server.close((error) => (error ? reject(error) : resolveClose()));
      });
      await application.close();
    },
  };
}

export async function startAgentServer(
  options: AgentServerOptions = {},
): Promise<RunningAgentServer> {
  const running = createAgentServer(options);
  const port = options.port ?? Number(process.env.PORT ?? DEFAULT_PORT);
  const host = options.host ?? process.env.HOST ?? DEFAULT_HOST;
  await new Promise<void>((resolveListen, reject) => {
    running.server.once("error", reject);
    running.server.listen(port, host, () => resolveListen());
  });
  console.log(`Agent harness API listening at http://${host}:${port}`);
  return running;
}

const entryPath = process.argv[1];
if (entryPath && resolve(entryPath) === fileURLToPath(import.meta.url)) {
  void startAgentServer().catch((error: unknown) => {
    console.error(error);
    process.exitCode = 1;
  });
}
