import { createHash, randomUUID } from "node:crypto";
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
  type ModelConfig,
  type NativePathPolicy,
  type SessionConfigPaths,
} from "./config.js";
import { AgentSession } from "./session.js";
import {
  SessionResponseStore,
  type JsonRecord,
  type SessionUsageSummary,
} from "./persistence.js";
import {
  readSessionPathProfiles,
  SessionPathProfileError,
  setSessionPathModelDefault,
  setSessionPathProfile,
} from "./session-path-profiles.js";
import {
  DEFAULT_ROOT_SERVER_SETTINGS,
  directoryEntries,
  loadRootServerSettings,
  loadSessionRoots,
  type SessionRoot,
} from "./session-roots.js";
import {WebUiNormalizedRun} from "./plugins/web-ui/normalize-web-ui.js";
import { capWebUiMessages } from "./plugins/web-ui/part-cap-web-ui.js";
import { sessionSettingsFromConfig } from "./plugins/web-ui/setting-web-ui.js";
import { sessionUsageFromSummary } from "./plugins/web-ui/usage-web-ui.js";
import type {
  WebUiMessage,
  WebUiSessionConfig,
  WebUiSessionHistory,
  WebUiSessionRun,
  WebUiSessionSnapshot,
} from "./plugins/web-ui/types.js";
import {WebUiSessionViewCache} from "./plugins/web-ui/view-cache.js";
import { SessionRunRegistry, type SessionRun } from "./run-registry.js";
import { collectSessionFiles } from "./session-files.js";
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
  /** Answer with `304` when a session snapshot is unchanged. */
  conditionalRequests?: boolean;
  /** Turns in a session snapshot; 0 returns the whole transcript. */
  snapshotTurns?: number;
  /** Largest number of turns one page request may return. */
  maxPageTurns?: number;
  /**
   * Inline text kept per transcript part, in bytes. A larger part is cut for
   * transport and can be re-fetched whole; `0` sends parts uncut.
   */
  maxInlinePartBytes?: number;
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
  /** Incremental transcript projection, reused across snapshot requests. */
  view: WebUiSessionViewCache;
}

/** Everything a snapshot's content is derived from, so a change is visible. */
interface SessionStatePaths {
  messages: string;
  messagesBackup: string;
  compactionCache: string;
  usageCache: string;
  setting: string;
  model: string;
}

interface SessionPageInfo {
  has_more: boolean;
  next_before: number | null;
  generation: string;
  stale_cursor: boolean;
}

function sessionStatePaths(paths: SessionConfigPaths): SessionStatePaths {
  return {
    messages: paths.messages_path,
    messagesBackup: paths.messages_backup_path,
    compactionCache: paths.compaction_cache_path,
    usageCache: paths.usage_cache_path,
    setting: paths.setting_path,
    model: paths.model_path,
  };
}

/**
 * Fingerprint of the state a response was built from.
 *
 * Only `stat` is used, so revalidation costs a handful of syscalls instead of the
 * transcript read it guards. The run descriptor is included because a run starts
 * and stops without touching a file, but its in-flight snapshot is not: that
 * belongs to the SSE stream, and the snapshot payload never contains live
 * content.
 */
async function sessionStateVersion(
  paths: SessionStatePaths,
  run: WebUiSessionRun | undefined,
): Promise<string> {
  const parts: string[] = [];
  for (const path of [
    paths.messages,
    paths.messagesBackup,
    paths.compactionCache,
    paths.usageCache,
    paths.setting,
    paths.model,
  ]) {
    // A missing file is a state of its own: the payload omits whatever it holds.
    let part = "-";
    try {
      const info = await stat(path);
      part = `${info.dev}:${info.ino}:${info.size}:${info.mtimeMs}:${info.ctimeMs}`;
    } catch {
      // Keep the placeholder.
    }
    parts.push(part);
  }
  parts.push(
    run === undefined ? "-" : `${run.turn_id}:${run.status}:${run.started_at}`,
  );
  return `"${createHash("sha256").update(parts.join("|")).digest("hex")}"`;
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

function ifNoneMatchMatches(header: string | undefined, etag: string): boolean {
  if (header === undefined) {
    return false;
  }
  return header
    .split(",")
    .map((candidate) => candidate.trim())
    .some((candidate) => candidate === "*" || candidate === etag || candidate === `W/${etag}`);
}

/**
 * Answers `304` when the client already holds this state, without building the
 * body. Returns true when the response was answered.
 */
function respondNotModified(
  request: IncomingMessage,
  response: ServerResponse,
  etag: string,
): boolean {
  if (!ifNoneMatchMatches(request.headers["if-none-match"], etag)) {
    return false;
  }
  response.writeHead(304, {
    "ETag": etag,
    "Cache-Control": "no-cache",
  });
  response.end();
  return true;
}

/**
 * Sends a body with the fingerprint it was built from. The response is
 * storable-but-revalidating (`no-cache`) rather than `no-store`, because the body
 * is only correct while that fingerprint still matches.
 */
function sendVersionedJson(
  response: ServerResponse,
  value: unknown,
  etag: string,
  conditional: boolean,
): void {
  if (response.writableEnded) {
    return;
  }
  const body = JSON.stringify(value);
  response.writeHead(200, {
    "Content-Type": "application/json; charset=utf-8",
    "Cache-Control": conditional ? "no-cache" : "no-store",
    ...(conditional ? {"ETag": etag} : {}),
    "Content-Length": Buffer.byteLength(body),
  });
  response.end(body);
}

function parseCursorParameter(value: string | null, field: string): number | undefined {
  if (value === null || value.length === 0) {
    return undefined;
  }
  if (!/^\d+$/.test(value)) {
    throw new HttpError(400, `${field} must be a non-negative integer offset`);
  }
  const parsed = Number(value);
  if (!Number.isSafeInteger(parsed)) {
    throw new HttpError(400, `${field} must be a non-negative integer offset`);
  }
  return parsed;
}

function parseCursorGenerationParameter(value: string | null): string | undefined {
  if (value === null || value.length === 0) return undefined;
  if (!/^\d+:\d+:\d+$/.test(value)) {
    throw new HttpError(400, "cursor_generation must be a transcript generation");
  }
  return value;
}

function parseTurnsParameter(value: string | null): number | undefined {
  if (value === null || value.length === 0) {
    return undefined;
  }
  if (!/^\d+$/.test(value)) {
    throw new HttpError(400, "turns must be a positive integer");
  }
  const parsed = Number(value);
  if (!Number.isSafeInteger(parsed) || parsed < 1) {
    throw new HttpError(400, "turns must be a positive integer");
  }
  return parsed;
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
): Promise<{config: WebUiSessionConfig; paths: SessionStatePaths; usageCachePath: string}> {
  const loader = new SessionConfigLoader(directory, {globalJadRoot});
  const settings = await loader.loadSettings();
  const pathProfileInfo = await readSessionPathProfiles(loader.paths.setting_path);
  const model = await readJsonRecord(loader.paths.model_path);
  const provider = model.provider;
  return {
    config: {
      settings,
      pathProfiles: pathProfileInfo.path_profiles,
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
    paths: sessionStatePaths(loader.paths),
    usageCachePath: settings.paths.usage_cache,
  };
}

/** Session metadata plus whatever transcript the caller resolved. */
function sessionSummary(
  location: SessionLocation,
  config: WebUiSessionConfig,
  view: {messages: WebUiMessage[]; turns: number; history?: WebUiSessionHistory},
  usage?: SessionUsageSummary,
  run?: WebUiSessionRun,
  page?: SessionPageInfo,
): WebUiSessionSnapshot {
  const usageValue = sessionUsageFromSummary(usage);
  return {
    id: location.ref,
    ...sessionSettingsFromConfig(config),
    turns: view.turns,
    messages: view.messages,
    ...(usageValue === undefined ? {} : {usage: usageValue}),
    ...(run === undefined ? {} : {run}),
    ...(page === undefined ? {} : {page}),
    ...(view.history === undefined ? {} : {history: view.history}),
    session_id: location.sessionId,
    root_id: location.root.id,
    root_label: location.root.label,
  };
}

/**
 * A session without its transcript, which is what the list needs: the sidebar
 * shows a name, a turn count, usage, and whether a run is live. Transcripts are
 * fetched per session, and only for the one being viewed.
 */
function sessionListing(
  location: SessionLocation,
  config: WebUiSessionConfig,
  turns: number,
  usage?: SessionUsageSummary,
  run?: WebUiSessionRun,
): Omit<WebUiSessionSnapshot, "messages" | "page"> {
  const usageValue = sessionUsageFromSummary(usage);
  return {
    id: location.ref,
    ...sessionSettingsFromConfig(config),
    turns,
    ...(usageValue === undefined ? {} : {usage: usageValue}),
    ...(run === undefined ? {} : {run}),
    session_id: location.sessionId,
    root_id: location.root.id,
    root_label: location.root.label,
  };
}

class AgentHttpApplication {
  private readonly sessions = new Map<string, Promise<SessionHandle>>();
  private readonly changingSessionSettings = new Set<string>();
  private readonly compactingSessions = new Set<string>();
  private readonly sessionRoots: Promise<SessionRoot[]>;
  private readonly sessionOptions: {
    client?: OpenAI;
    environment?: NodeJS.ProcessEnv;
  };
  private readonly maxBodyBytes: number;
  private readonly toolStreamIntervalMs: number;
  private readonly conditionalRequests: boolean;
  private readonly snapshotTurns: number;
  private readonly maxPageTurns: number;
  private readonly maxInlinePartBytes: number;
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
    this.conditionalRequests =
      options.conditionalRequests ?? DEFAULT_ROOT_SERVER_SETTINGS.conditional_requests;
    this.snapshotTurns = options.snapshotTurns ?? DEFAULT_ROOT_SERVER_SETTINGS.snapshot_turns;
    this.maxPageTurns = options.maxPageTurns ?? DEFAULT_ROOT_SERVER_SETTINGS.max_page_turns;
    this.maxInlinePartBytes =
      options.maxInlinePartBytes ?? DEFAULT_ROOT_SERVER_SETTINGS.max_inline_part_bytes;
    this.runs = new SessionRunRegistry(
      options.runStateRetentionMs ?? DEFAULT_ROOT_SERVER_SETTINGS.run_state_retention_ms,
    );
  }

  public async handle(request: IncomingMessage, response: ServerResponse): Promise<void> {
    response.setHeader("Access-Control-Allow-Origin", "*");
    response.setHeader("Access-Control-Allow-Headers", "Content-Type, If-None-Match");
    response.setHeader("Access-Control-Expose-Headers", "ETag");
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
        await this.handleSessionList(request, response);
        return;
      }

      const sessionRef = validateSessionReference(segments[2] ?? "");
      const isSessionProfileRequest =
        request.method === "POST" && segments.length === 4 && segments[3] === "profile";
      const isSessionModelDefaultRequest =
        request.method === "POST" && segments.length === 4 && segments[3] === "model-default";
      const isSessionSettingsUpdateRequest = isSessionProfileRequest || isSessionModelDefaultRequest;
      if (this.changingSessionSettings.has(sessionRef) && !isSessionSettingsUpdateRequest) {
        throw new HttpError(409, "session settings are changing");
      }
      if (isSessionProfileRequest) {
        await this.handleSessionProfile(sessionRef, request, response);
        return;
      }
      if (isSessionModelDefaultRequest) {
        await this.handleSessionModelDefault(sessionRef, request, response);
        return;
      }
      if (request.method === "GET" && segments.length === 3) {
        await this.handleSessionSnapshot(sessionRef, request, response);
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
        request.method === "GET" &&
        segments.length === 4 &&
        segments[3] === "files"
      ) {
        await this.handleSessionFiles(sessionRef, response);
        return;
      }

      if (
        request.method === "GET" &&
        segments.length === 5 &&
        segments[3] === "messages"
      ) {
        await this.handleSessionMessage(sessionRef, segments[4] ?? "", url, response);
        return;
      }

      if (
        request.method === "GET" &&
        segments.length === 4 &&
        segments[3] === "history"
      ) {
        await this.handleSessionHistory(sessionRef, request, response);
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
      const loader = new SessionConfigLoader(directory, {
        globalJadRoot: location.root.globalJadRoot,
      });
      // Loading the settings rejects an unusable session before it is cached.
      await loader.loadSettings();
      return {
        location,
        directory,
        session,
        loader,
        view: new WebUiSessionViewCache(session.store, {
          snapshotTurns: this.snapshotTurns,
          maxPageTurns: this.maxPageTurns,
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

  private async handleSessionList(
    request: IncomingMessage,
    response: ServerResponse,
  ): Promise<void> {
    const roots = await this.sessionRoots;
    const sessionLocations: Array<{location: SessionLocation; directory: string}> = [];
    const versions: string[] = [];
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
        sessionLocations.push({location, directory});
        const loader = new SessionConfigLoader(directory, {
          globalJadRoot: root.globalJadRoot,
        });
        versions.push(
          `${location.ref}:${await sessionStateVersion(
            sessionStatePaths(loader.paths),
            this.runs.describe(location.ref),
          )}`,
        );
      }
    }
    versions.sort();
    const etag = `"${createHash("sha256").update(versions.join("|")).digest("hex")}"`;
    // Revalidation comes first: an idle poll must not read any session config.
    if (this.conditionalRequests && respondNotModified(request, response, etag)) {
      return;
    }

    const sessions: Omit<WebUiSessionSnapshot, "messages" | "page">[] = [];
    for (const {location, directory} of sessionLocations) {
      try {
        const listing = await sessionListingConfig(
          directory,
          location.sessionId,
          location.root.globalJadRoot,
        );
        const usage = await SessionResponseStore.readCachedUsageSummary(
          listing.usageCachePath,
        );
        sessions.push(
          sessionListing(
            location,
            listing.config,
            usage?.turn_count ?? 0,
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
    sessions.sort((left, right) => String(left.id).localeCompare(String(right.id)));
    sendVersionedJson(response, { sessions }, etag, this.conditionalRequests);
  }

  private async handleSessionSnapshot(
    id: string,
    request: IncomingMessage,
    response: ServerResponse,
  ): Promise<void> {
    const url = new URL(request.url ?? "/", "http://agent-harness.local");
    const before = parseCursorParameter(url.searchParams.get("before"), "before");
    const cursorGeneration = parseCursorGenerationParameter(url.searchParams.get("cursor_generation"));
    const turns = parseTurnsParameter(url.searchParams.get("turns"));
    const handle = await this.getSession(id);
    const run = this.runs.describe(handle.location.ref);
    const etag = await sessionStateVersion(
      sessionStatePaths(handle.loader.paths),
      run,
    );
    // Revalidation comes first: answering `304` must not read the transcript,
    // which is the whole point of a cheap poll.
    if (this.conditionalRequests && respondNotModified(request, response, etag)) {
      return;
    }

    const config = await handle.loader.load();
    const pathProfileInfo = await readSessionPathProfiles(handle.loader.paths.setting_path);
    const sessionConfig = {...config, pathProfiles: pathProfileInfo.path_profiles};
    const [view, usage] = await Promise.all([
      handle.view.read({
        ...(before === undefined ? {} : {before}),
        ...(cursorGeneration === undefined ? {} : {generation: cursorGeneration}),
        ...(turns === undefined ? {} : {turns}),
        backupTurns: config.settings.compaction.keepbackup.turns,
      }),
      handle.session.readUsageSummary(),
    ]);
    const messages = capWebUiMessages(view.messages, {
      maxInlineBytes: this.maxInlinePartBytes,
      cursors: view.cursors,
    });
    const page: SessionPageInfo = {
      has_more: view.hasMore,
      next_before: view.nextBefore,
      generation: view.generation,
      stale_cursor: view.staleCursor,
    };
    const summary = sessionSummary(
      handle.location,
      sessionConfig,
      {
        messages,
        turns: usage.turn_count,
        ...(view.history === undefined ? {} : {history: view.history}),
      },
      usage,
      run,
      page,
    );
    sendVersionedJson(response, summary, etag, this.conditionalRequests);
  }

  /**
   * One transcript message, whole.
   *
   * A capped snapshot keeps a bounded prefix of a large part, so this is how a
   * client asks for the rest: the message it already holds names the turn cursor
   * that re-reads it, or — for a compacted message — nothing, because the view
   * holds the head. Nothing is capped here: returning another capped copy would
   * leave the client exactly where it started.
   */
  private async handleSessionMessage(
    id: string,
    messageId: string,
    url: URL,
    response: ServerResponse,
  ): Promise<void> {
    const handle = await this.getSession(id);
    const cursor = parseCursorParameter(url.searchParams.get("cursor"), "cursor");
    const config = await handle.loader.load();
    const message = await handle.view.readMessage(
      messageId,
      cursor,
      config.settings.compaction.keepbackup.turns,
    );
    if (message === undefined) {
      throw new HttpError(404, "message not found");
    }
    sendJson(response, 200, {message});
  }

  /**
   * The history block a snapshot referred to.
   *
   * It is the backup's tail — turns a compaction removed, which the transcript
   * no longer carries — and static between compactions, so it carries its own
   * fingerprint: a client fetches it once per identity and revalidates cheaply,
   * instead of receiving it again with every transcript change.
   */
  private async handleSessionHistory(
    id: string,
    request: IncomingMessage,
    response: ServerResponse,
  ): Promise<void> {
    const handle = await this.getSession(id);
    const config = await handle.loader.load();
    const history = await handle.view.readHistory(config.settings.compaction.keepbackup.turns);
    if (history.etag === null) {
      sendJson(response, 200, {messages: []});
      return;
    }
    const etag = `"${history.etag}"`;
    if (this.conditionalRequests && respondNotModified(request, response, etag)) {
      return;
    }
    const messages = capWebUiMessages(history.messages, {
      maxInlineBytes: this.maxInlinePartBytes,
      cursors: new Map(),
    });
    sendVersionedJson(response, {messages}, etag, this.conditionalRequests);
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
    if (this.changingSessionSettings.has(id) || this.compactingSessions.has(id)) {
      throw new HttpError(409, "session profile or compaction is already changing");
    }
    this.compactingSessions.add(id);
    try {
      const handle = await this.getSession(id);
      if (this.changingSessionSettings.has(id)) {
        throw new HttpError(409, "session settings are changing");
      }
      const {compacted, compacted_items, retained_items} = await handle.session.compact();
      sendJson(response, 200, {compaction: {compacted, compacted_items, retained_items}});
    } finally {
      this.compactingSessions.delete(id);
    }
  }

  private async handleTools(
    id: string,
    response: ServerResponse,
  ): Promise<void> {
    const handle = await this.getSession(id);
    sendJson(response, 200, {tools: await handle.session.listTools()});
  }

  /**
   * The `@` menu's file list: names inside the session cwd, filtered through the
   * read tool's path policy so the menu never offers what the agent would refuse.
   * Contents are never returned — only paths, relative to cwd.
   */
  private async handleSessionFiles(
    id: string,
    response: ServerResponse,
  ): Promise<void> {
    const handle = await this.getSession(id);
    const settings = await handle.loader.loadSettings();
    if (!settings.session_files.enabled) {
      throw new HttpError(403, "file listing is disabled");
    }
    // A session that configures no read tool has no policy to apply; the
    // workspace-only default is the same one the tool would use.
    const policy: NativePathPolicy = settings.tools.native.read ?? {
      paths: [],
      unrestricted_paths: false,
    };
    const listing = await collectSessionFiles(settings.cwd, settings.session_files, policy);
    sendJson(response, 200, listing);
  }

  private async handleSessionProfile(
    reference: string,
    request: IncomingMessage,
    response: ServerResponse,
  ): Promise<void> {
    const body = await readJsonBody(request, this.maxBodyBytes);
    if (
      !isRecord(body) ||
      typeof body.active_profile !== "string" ||
      body.active_profile.trim().length === 0
    ) {
      throw new HttpError(400, "active_profile must be a non-empty string");
    }
    const profileName = body.active_profile.trim();
    if (this.changingSessionSettings.has(reference)) {
      throw new HttpError(409, "session path profile is already changing");
    }
    const handle = await this.getSession(reference);
    if (this.changingSessionSettings.has(reference)) {
      throw new HttpError(409, "session path profile is already changing");
    }
    if (this.runs.current(reference)?.status === "running") {
      throw new HttpError(409, "cannot change the session path profile while a run is active");
    }
    if (this.compactingSessions.has(reference)) {
      throw new HttpError(409, "cannot change the session path profile during compaction");
    }

    this.changingSessionSettings.add(reference);
    try {
      const settings = await handle.loader.loadSettings();
      let update;
      try {
        update = await setSessionPathProfile(
          handle.loader.paths.setting_path,
          profileName,
          settings.persistence.durability,
        );
      } catch (error) {
        if (error instanceof SessionPathProfileError) {
          throw new HttpError(400, error.message);
        }
        throw error;
      }

      let currentHandle = handle;
      if (update.changed) {
        this.sessions.delete(reference);
        await handle.session.close();
        currentHandle = await this.getSession(reference);
      }
      const currentSettings = await currentHandle.loader.loadSettings();
      const profileInfo = await readSessionPathProfiles(currentHandle.loader.paths.setting_path);
      sendJson(response, 200, {
        ...profileInfo,
        ...(currentSettings.paths.model_default === undefined
          ? {}
          : {model_default: currentSettings.paths.model_default}),
        ...(currentSettings.paths.models === undefined
          ? {}
          : {models: currentSettings.paths.models}),
        changed: update.changed,
      });
    } finally {
      this.changingSessionSettings.delete(reference);
    }
  }

  private async handleSessionModelDefault(
    reference: string,
    request: IncomingMessage,
    response: ServerResponse,
  ): Promise<void> {
    const body = await readJsonBody(request, this.maxBodyBytes);
    if (
      !isRecord(body) ||
      typeof body.model_default !== "string" ||
      body.model_default.trim().length === 0
    ) {
      throw new HttpError(400, "model_default must be a non-empty string");
    }
    const modelConfigPath = body.model_default.trim();
    if (this.changingSessionSettings.has(reference)) {
      throw new HttpError(409, "session settings are already changing");
    }
    const handle = await this.getSession(reference);
    if (this.changingSessionSettings.has(reference)) {
      throw new HttpError(409, "session settings are already changing");
    }

    this.changingSessionSettings.add(reference);
    try {
      const settings = await handle.loader.loadSettings();
      const activeProfile = settings.paths.active_profile;
      if (activeProfile === undefined || !settings.paths.models?.includes(modelConfigPath)) {
        throw new HttpError(400, "model config path is not available in the active session path profile");
      }

      let model: ModelConfig;
      try {
        model = await handle.loader.loadModel(modelConfigPath);
      } catch (error) {
        if (error instanceof ConfigError) {
          throw new HttpError(400, error.message);
        }
        throw error;
      }

      let update;
      try {
        update = await setSessionPathModelDefault(
          handle.loader.paths.setting_path,
          activeProfile,
          modelConfigPath,
          settings.persistence.durability,
        );
      } catch (error) {
        if (error instanceof SessionPathProfileError) {
          throw new HttpError(400, error.message);
        }
        throw error;
      }

      const currentSettings = await handle.loader.loadSettings();
      const profileInfo = await readSessionPathProfiles(handle.loader.paths.setting_path);
      sendJson(response, 200, {
        ...profileInfo,
        model_default: update.model_default,
        models: currentSettings.paths.models ?? [update.model_default],
        model: {provider: model.provider, model: model.model},
        changed: update.changed,
      });
    } finally {
      this.changingSessionSettings.delete(reference);
    }
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

    if (body.model !== undefined && (typeof body.model !== "string" || body.model.trim().length === 0)) {
      throw new HttpError(400, "model must be a non-empty string");
    }
    const requestedModelPath = typeof body.model === "string" ? body.model.trim() : undefined;
    const handle = await this.getSession(id);
    if (this.changingSessionSettings.has(id)) {
      throw new HttpError(409, "session settings are changing");
    }
    const settings = await handle.loader.loadSettings();
    if (this.changingSessionSettings.has(id)) {
      throw new HttpError(409, "session settings are changing");
    }
    if (requestedModelPath !== undefined && !settings.paths.models?.includes(requestedModelPath)) {
      throw new HttpError(400, "model config path is not available in the active session path profile");
    }
    const modelConfigPath = requestedModelPath ?? settings.paths.model_default;
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
        ...(modelConfigPath === undefined ? {} : {modelConfigPath}),
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
    conditionalRequests:
      options.conditionalRequests ?? serverSettings.conditional_requests,
    snapshotTurns: options.snapshotTurns ?? serverSettings.snapshot_turns,
    maxPageTurns: options.maxPageTurns ?? serverSettings.max_page_turns,
    maxInlinePartBytes: options.maxInlinePartBytes ?? serverSettings.max_inline_part_bytes,
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
