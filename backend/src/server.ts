import { randomUUID } from "node:crypto";
import { readFile, stat } from "node:fs/promises";
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
} from "./config.js";
import { AgentSession } from "./session.js";
import {
  SessionResponseStore,
  type JsonRecord,
  type SessionUsageSummary,
} from "./persistence.js";
import {
  DEFAULT_ROOT_SERVER_SETTINGS,
  directoryEntries,
  loadRootServerSettings,
  loadSessionRoots,
  type SessionRoot,
} from "./session-roots.js";
import {
  normalizeWebUiSession,
  WebUiNormalizedRun,
} from "./plugins/web-ui/normalize-web-ui.js";
import type {
  WebUiMessage,
  WebUiSessionConfig,
  WebUiSessionRun,
  WebUiSessionSnapshot,
} from "./plugins/web-ui/types.js";
import { SessionRunRegistry, type SessionRun } from "./run-registry.js";
import { MessageStreamEmitter } from "./stream-emitter.js";

const SESSION_ID_PATTERN = /^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$/;
const SESSION_ROOT_ID_PATTERN = /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/;

export interface AgentServerOptions {
  sessionsDir?: string;
  rootSettingPath?: string;
  client?: OpenAI;
  environment?: NodeJS.ProcessEnv;
  host?: string;
  port?: number;
  maxBodyBytes?: number;
  /** How long a finished run stays attachable, so a reload can still catch it. */
  runStateRetentionMs?: number;
  /** Coalescing window for in-progress tool-call snapshots; 0 streams every delta. */
  toolStreamIntervalMs?: number;
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

interface SessionLocation {
  ref: string;
  sessionId: string;
  root: SessionRoot;
}

interface SessionHandle {
  location: SessionLocation;
  directory: string;
  session: AgentSession;
  loader: SessionConfigLoader;
}

function isRecord(value: unknown): value is JsonRecord {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function validateSessionId(value: string): string {
  if (!SESSION_ID_PATTERN.test(value) || value === "." || value === "..") {
    throw new HttpError(400, "invalid session id");
  }
  return value;
}

function validateSessionRootId(value: string): string {
  if (!SESSION_ROOT_ID_PATTERN.test(value)) {
    throw new HttpError(400, "invalid session root id");
  }
  return value;
}

function parseSessionReference(value: string): {
  rootId?: string;
  sessionId: string;
} {
  const parts = value.split(":");
  if (parts.length === 1) {
    return {sessionId: validateSessionId(value)};
  }
  if (parts.length !== 2) {
    throw new HttpError(400, "invalid session reference");
  }
  return {
    rootId: validateSessionRootId(parts[0]!),
    sessionId: validateSessionId(parts[1]!),
  };
}

function sessionReference(
  root: SessionRoot,
  sessionId: string,
  rootCount: number,
): string {
  return rootCount === 1 ? sessionId : `${root.id}:${sessionId}`;
}

function validateSessionReference(value: string): string {
  parseSessionReference(value);
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

async function readJsonBody(
  request: IncomingMessage,
  maxBodyBytes: number,
): Promise<unknown> {
  const chunks: Buffer[] = [];
  let size = 0;
  for await (const chunk of request) {
    const buffer = Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk);
    size += buffer.length;
    if (size > maxBodyBytes) {
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

async function hasSessionSetting(directory: string): Promise<boolean> {
  for (const name of ["setting.json"]) {
    try {
      const info = await stat(join(directory, name));
      if (!info.isFile()) {
        return false;
      }
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code === "ENOENT") {
        return false;
      }
      throw error;
    }
  }
  return true;
}

async function readJsonRecord(path: string): Promise<JsonRecord> {
  try {
    const value: unknown = JSON.parse(await readFile(path, "utf8"));
    return isRecord(value) ? value : {};
  } catch {
    return {};
  }
}

async function sessionListingConfig(
  directory: string,
  sessionId: string,
  globalJadRoot: string,
): Promise<{config: WebUiSessionConfig; messagesPath: string; messagesBackupPath: string; usageCachePath: string}> {
  const loader = new SessionConfigLoader(directory, {globalJadRoot});
  const settings = await loader.loadSettings();
  const model = await readJsonRecord(loader.paths.model_path);
  const provider = model.provider;
  return {
    config: {
      settings,
      model: {
        provider:
          typeof provider === "string" && provider.trim().length > 0
            ? provider.trim()
            : "unknown",
        model:
          typeof model.model === "string" && model.model.trim().length > 0
            ? model.model.trim()
            : sessionId,
      },
    },
    messagesPath: loader.paths.messages_path,
    messagesBackupPath: settings.paths.messages_backup,
    usageCachePath: settings.paths.usage_cache,
  };
}

async function readListingMessages(messagesPath: string, backupPath?: string): Promise<JsonRecord[]> {
  const [backup, active] = await Promise.all([
    backupPath ? readJsonl(backupPath).catch(() => []) : Promise.resolve([]),
    readJsonl(messagesPath).catch(() => []),
  ]);
  if (backup.length === active.length && backup.every((item, index) => JSON.stringify(item) === JSON.stringify(active[index]))) {
    return active;
  }
  return [...backup, ...active];
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
  location: SessionLocation,
  config: WebUiSessionConfig,
  messages: JsonRecord[],
  usage?: SessionUsageSummary,
  run?: WebUiSessionRun,
): WebUiSessionSnapshot {
  return {
    ...normalizeWebUiSession(location.ref, config, messages, usage),
    ...(run === undefined ? {} : {run}),
    session_id: location.sessionId,
    root_id: location.root.id,
    root_label: location.root.label,
  };
}

class AgentHttpApplication {
  private readonly sessions = new Map<string, Promise<SessionHandle>>();
  private readonly sessionRoots: Promise<SessionRoot[]>;
  private readonly sessionOptions: {
    client?: OpenAI;
    environment?: NodeJS.ProcessEnv;
  };
  private readonly maxBodyBytes: number;
  private readonly toolStreamIntervalMs: number;
  private readonly runs: SessionRunRegistry;

  public constructor(options: AgentServerOptions = {}) {
    this.sessionRoots = loadSessionRoots({
      sessionsDir: options.sessionsDir,
      rootSettingPath: options.rootSettingPath,
      environment: options.environment,
    });
    this.sessionOptions = {
      ...(options.client ? { client: options.client } : {}),
      ...(options.environment ? { environment: options.environment } : {}),
    };
    this.maxBodyBytes = options.maxBodyBytes ?? DEFAULT_ROOT_SERVER_SETTINGS.max_body_bytes;
    this.toolStreamIntervalMs =
      options.toolStreamIntervalMs ?? DEFAULT_ROOT_SERVER_SETTINGS.tool_stream_interval_ms;
    this.runs = new SessionRunRegistry(
      options.runStateRetentionMs ?? DEFAULT_ROOT_SERVER_SETTINGS.run_state_retention_ms,
    );
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

      const sessionRef = validateSessionReference(segments[2] ?? "");
      if (request.method === "GET" && segments.length === 3) {
        await this.handleSessionSnapshot(sessionRef, response);
        return;
      }

      if (
        request.method === "GET" &&
        segments.length === 4 &&
        segments[3] === "tools"
      ) {
        await this.handleTools(sessionRef, response);
        return;
      }

      if (
        request.method === "POST" &&
        segments.length === 4 &&
        segments[3] === "compact"
      ) {
        await this.handleCompact(sessionRef, response);
        return;
      }

      if (
        request.method === "GET" &&
        segments.length === 5 &&
        segments[3] === "run" &&
        segments[4] === "stream"
      ) {
        await this.handleRunStream(sessionRef, response);
        return;
      }

      if (
        request.method === "POST" &&
        segments.length === 5 &&
        segments[3] === "run" &&
        segments[4] === "stop"
      ) {
        await this.handleRunStop(sessionRef, response);
        return;
      }

      if (
        request.method === "POST" &&
        segments.length === 4 &&
        segments[3] === "run"
      ) {
        await this.handleRun(sessionRef, request, response);
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

  /** Cancels foreground provider and tool work before server shutdown. */
  public abortActiveRuns(reason = "server_shutdown"): void {
    this.runs.abortAll(new Error(reason));
  }

  public async close(): Promise<void> {
    this.abortActiveRuns("application_close");
    this.runs.clear();
    const handles = await Promise.allSettled(this.sessions.values());
    await Promise.all(
      handles.flatMap((result) =>
        result.status === "fulfilled" ? [result.value.session.close()] : [],
      ),
    );
  }

  private async resolveSessionLocation(reference: string): Promise<SessionLocation> {
    const roots = await this.sessionRoots;
    const parsed = parseSessionReference(reference);
    if (parsed.rootId) {
      const root = roots.find((candidate) => candidate.id === parsed.rootId);
      if (!root) {
        throw new HttpError(404, "session root not found");
      }
      return {
        ref: sessionReference(root, parsed.sessionId, roots.length),
        sessionId: parsed.sessionId,
        root,
      };
    }

    if (roots.length === 1) {
      const root = roots[0]!;
      return {
        ref: sessionReference(root, parsed.sessionId, roots.length),
        sessionId: parsed.sessionId,
        root,
      };
    }

    const matches: SessionRoot[] = [];
    for (const root of roots) {
      const entries = await directoryEntries(root);
      if (entries.some((entry) => entry.isDirectory() && entry.name === parsed.sessionId)) {
        matches.push(root);
      }
    }
    if (matches.length === 0) {
      throw new HttpError(404, "session not found");
    }
    if (matches.length > 1) {
      throw new HttpError(400, "session reference must include a root id");
    }
    const root = matches[0]!;
    return {
      ref: sessionReference(root, parsed.sessionId, roots.length),
      sessionId: parsed.sessionId,
      root,
    };
  }

  private async getSession(reference: string): Promise<SessionHandle> {
    const location = await this.resolveSessionLocation(reference);
    const existing = this.sessions.get(location.ref);
    if (existing) {
      return existing;
    }

    const directory = join(location.root.directory, location.sessionId);
    const pending = (async (): Promise<SessionHandle> => {
      const session = await AgentSession.open(directory, {
        ...this.sessionOptions,
        globalJadRoot: location.root.globalJadRoot,
      });
      return {
        location,
        directory,
        session,
        loader: new SessionConfigLoader(directory, {
          globalJadRoot: location.root.globalJadRoot,
        }),
      };
    })();
    this.sessions.set(location.ref, pending);
    try {
      return await pending;
    } catch (error) {
      this.sessions.delete(location.ref);
      throw error;
    }
  }

  private async handleSessionList(response: ServerResponse): Promise<void> {
    const roots = await this.sessionRoots;
    const sessions: WebUiSessionSnapshot[] = [];
    for (const root of roots) {
      const entries = await directoryEntries(root);
      for (const entry of entries) {
        if (!entry.isDirectory() || !SESSION_ID_PATTERN.test(entry.name)) {
          continue;
        }
        const location: SessionLocation = {
          ref: sessionReference(root, entry.name, roots.length),
          sessionId: entry.name,
          root,
        };
        const directory = join(root.directory, entry.name);
        if (!(await hasSessionSetting(directory))) {
          continue;
        }
        try {
          const listing = await sessionListingConfig(
            directory,
            entry.name,
            root.globalJadRoot,
          );
          const [messages, usage] = await Promise.all([
            readListingMessages(listing.messagesPath, listing.messagesBackupPath),
            SessionResponseStore.readCachedUsageSummary(listing.usageCachePath),
          ]);
          sessions.push(
            sessionSummary(
              location,
              listing.config,
              messages,
              usage,
              this.runs.describe(location.ref),
            ),
          );
        } catch (error) {
          if (error instanceof ConfigError) {
            console.warn(`Skipping invalid session ${location.ref}: ${error.message}`);
            continue;
          }
          throw error;
        }
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
    const [messages, usage] = await Promise.all([
      readListingMessages(
        handle.loader.paths.messages_path,
        config.settings.paths.messages_backup,
      ),
      handle.session.readUsageSummary(),
    ]);
    sendJson(
      response,
      200,
      sessionSummary(
        handle.location,
        config,
        messages,
        usage,
        this.runs.describe(handle.location.ref),
      ),
    );
  }

  private async handleRunStream(id: string, response: ServerResponse): Promise<void> {
    // Resolving the session first keeps unknown references a 404 instead of an
    // empty stream.
    await this.getSession(id);
    response.writeHead(200, {
      "Content-Type": "text/event-stream; charset=utf-8",
      "Cache-Control": "no-cache, no-store, must-revalidate",
      Connection: "keep-alive",
      "X-Accel-Buffering": "no",
    });
    response.flushHeaders();

    const run = this.runs.current(id);
    if (run === undefined) {
      writeSse(response, "idle", {});
      response.end();
      return;
    }

    writeSse(response, "run", run.summary());
    let detach = (): void => {};
    response.once("close", () => detach());
    detach = run.subscribe((event) => {
      writeSse(response, event.event, event.data);
      if (event.event === "done") {
        response.end();
      }
    });
    if (run.status !== "running") {
      // The retained terminal events were replayed synchronously; the stream is
      // complete.
      response.end();
    }
  }

  private async handleRunStop(id: string, response: ServerResponse): Promise<void> {
    await this.getSession(id);
    sendJson(response, 200, {stop: this.runs.stop(id)});
  }

  private async handleCompact(id: string, response: ServerResponse): Promise<void> {
    const handle = await this.getSession(id);
    const {compacted, compacted_items, retained_items} = await handle.session.compact();
    sendJson(response, 200, {compaction: {compacted, compacted_items, retained_items}});
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
    const body = await readJsonBody(request, this.maxBodyBytes);
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
    const turnId = `web-${randomUUID()}`;
    response.once("close", () => {
      // Disconnecting a browser does not discard an accepted, persisted run: the
      // run stays registered so a reload can attach to it again.
      connected = false;
    });
    const controller = new AbortController();
    controller.signal.addEventListener(
      "abort",
      () => {
        const reason = controller.signal.reason;
        const detail = reason instanceof Error
          ? `${reason.name}: ${reason.message}`
          : String(reason ?? "unspecified");
        console.warn(`[agent-run] aborted session=${id} turn=${turnId} reason=${detail}`);
      },
      {once: true},
    );
    const run = this.runs.begin(id, turnId, controller);
    const emitter = new MessageStreamEmitter(
      this.toolStreamIntervalMs,
      (message) => run.publishMessage(message),
    );

    try {
      const normalizedRun = new WebUiNormalizedRun(turnId);
      // The response that started the run is one subscriber among others, so a
      // second tab (or the same tab after a reload) can attach to the same run.
      const detach = run.subscribe((event) => {
        writeSse(response, event.event, event.data);
        if (event.event === "done") {
          response.end();
        }
      });
      response.once("close", detach);

      const result = await handle.session.run(body.input, {
        turnId,
        signal: controller.signal,
        onEvent: (event) => {
          const message = normalizedRun.update(event);
          if (message) {
            emitter.push(message);
          }
        },
        onResponseCommitted: async () => {
          try {
            const usage = await handle.session.readUsageSummary();
            run.publish("usage", {usage});
          } catch (error) {
            console.warn(
              `[agent-run] failed to publish usage session=${id} turn=${turnId}: ${errorMessage(error)}`,
            );
          }
        },
        onToolResult: (item) => {
          const message = normalizedRun.toolResult(item);
          if (message) {
            emitter.push(message);
          }
        },
      });

      const message = normalizedRun.complete(result.response.event.response);
      // The completed message supersedes any snapshot still waiting to be sent.
      emitter.discard();
      run.finish("completed", [
        {event: "ui.message", data: {message}},
        {event: "completed", data: {message, committed: true}},
        {event: "done", data: {}},
      ], message);
    } catch (error) {
      const aborted = controller.signal.aborted;
      emitter.discard();
      console.error(
        `[agent-run] failed session=${id} turn=${turnId} connected=${connected} aborted=${aborted}: ${errorMessage(error)}`,
        error,
      );
      run.finish(aborted ? "aborted" : "failed", [
        {
          event: "error",
          data: {
            message: aborted
              ? "The run was stopped before it completed."
              : errorMessage(error),
          },
        },
        {event: "done", data: {}},
      ]);
    } finally {
      if (!response.writableEnded && !response.destroyed) {
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
      application.abortActiveRuns("server_close");
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
  const serverSettings = await loadRootServerSettings(options);
  const running = createAgentServer({
    ...options,
    maxBodyBytes: options.maxBodyBytes ?? serverSettings.max_body_bytes,
    runStateRetentionMs:
      options.runStateRetentionMs ?? serverSettings.run_state_retention_ms,
    toolStreamIntervalMs:
      options.toolStreamIntervalMs ?? serverSettings.tool_stream_interval_ms,
  });
  const environment = options.environment ?? process.env;
  const port = options.port ?? Number(environment.PORT ?? serverSettings.port);
  const host = options.host ?? environment.HOST ?? serverSettings.host;
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
