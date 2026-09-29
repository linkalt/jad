import { createHash, randomUUID, timingSafeEqual } from "node:crypto";
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
import {API_TOKEN_ENV_NAME, environmentWithoutApiToken} from "./server-secrets.js";

import {
  ConfigError,
  SessionConfigLoader,
  type ModelConfig,
  type NativePathPolicy,
  type SessionConfigPaths,
  type SessionSettings,
} from "./config.js";
import { AgentSession } from "./session.js";
import { ProcessManager } from "./tools/native/process-manager.js";
import { TaskStore } from "./tools/native/task-store.js";
import { MemoryStore } from "./tools/native/memory-store.js";
import {
  DEFAULT_NATIVE_MEMORY_STORAGE_DIR,
  DEFAULT_NATIVE_PROCESS_STORAGE_DIR,
  DEFAULT_NATIVE_TASK_STORAGE_DIR,
} from "./config.js";
import {
  PersistenceError,
  SessionResponseStore,
  type JsonRecord,
  type SessionUsageSummary,
} from "./persistence.js";
import {
  readSessionPathProfiles,
  SessionPathProfileError,
  setSessionPathModelDefault,
  setSessionPathProfile,
  type SessionProfileStoragePaths,
} from "./session-path-profiles.js";
import {
  DEFAULT_ROOT_SERVER_SETTINGS,
  MIN_API_TOKEN_LENGTH,
  directoryEntries,
  loadRootServerSettings,
  loadSessionRoots,
  type SessionRoot,
  type SessionRootOptions,
} from "./session-roots.js";
import {listRootLibrary, readRootLibraryAsset} from "./content-library.js";
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
import {parseInteractionExtendRequest, parseInteractionReplyRequest} from "./interactions.js";
import { SessionRunRegistry, type SessionRun } from "./run-registry.js";
import { collectSessionFiles } from "./session-files.js";
import { MessageStreamEmitter } from "./stream-emitter.js";
import {writeSse} from "./sse-writer.js";

const SESSION_ID_PATTERN = /^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$/;
const SESSION_ROOT_ID_PATTERN = /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/;
/** Session path profiles are named exactly like the keys `setting.json` accepts. */
const PROFILE_NAME_PATTERN = /^[A-Za-z0-9_-]+$/;

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
  /** Maximum queued SSE response bytes per subscriber before it must resynchronize. */
  sseMaxBufferedBytes?: number;
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
  /** Secret required on every session route; defaults to AGENT_API_TOKEN. */
  apiToken?: string;
  /** Exact browser origins that may call session routes. */
  allowedOrigins?: readonly string[];
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
  /**
   * The profile this handle is pinned to, or undefined when the session
   * configures no profiles. One handle exists per `(session, profile)`.
   */
  profile?: string;
  /** Incremental transcript projection, reused across snapshot requests. */
  view: WebUiSessionViewCache;
}

/** Tool stores shared by handles for one `(session, profile)` pair. */
interface SessionSharedStores {
  processManager: ProcessManager;
  taskStore: TaskStore;
  memoryStore: MemoryStore;
  refs: number;
}

/**
 * The registry and handle-cache key of one `(session, profile)` pair.
 *
 * A profile-less session keeps its plain reference, so every request that does
 * not name a profile behaves exactly as it did before profiles were addressable.
 */
function profileKey(sessionRef: string, profile: string | undefined): string {
  return profile === undefined ? sessionRef : `${sessionRef}|${profile}`;
}

/** Canonical storage registry key; the JSON tuple avoids delimiter collisions. */
function sharedStoresKey(directory: string, profile: string | undefined): string {
  return JSON.stringify([resolve(directory), profile ?? null]);
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

function sessionProfileStoragePaths(
  paths: SessionSettings["paths"],
): SessionProfileStoragePaths {
  return {
    messages: paths.messages,
    messages_backup: paths.messages_backup,
    compaction_cache: paths.compaction_cache,
    history_cache: paths.history_cache,
    usage_cache: paths.usage_cache,
    cursor_cache: paths.cursor_cache,
    statecontext: paths.statecontext,
    staging: paths.staging,
    http_debug: paths.http_debug,
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
  runs: readonly WebUiSessionRun[],
): Promise<string> {
  // These six metadata reads are independent. Keep the fan-out bounded to this
  // fixed per-session set while avoiding six serial filesystem round-trips.
  const parts = await Promise.all([
    paths.messages,
    paths.messagesBackup,
    paths.compactionCache,
    paths.usageCache,
    paths.setting,
    paths.model,
  ].map(async (path) => {
    // A missing file is a state of its own: the payload omits whatever it holds.
    try {
      const info = await stat(path);
      return `${info.dev}:${info.ino}:${info.size}:${info.mtimeMs}:${info.ctimeMs}`;
    } catch {
      // Keep the placeholder.
      return "-";
    }
  }));
  // A run starts and stops without touching a file; every profile's run is part
  // of the fingerprint, because the list carries all of them.
  parts.push(
    runs.length === 0
      ? "-"
      : runs
          .map((run) => `${run.turn_id}:${run.status}:${run.started_at}`)
          .sort()
          .join(","),
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

function isAuthorized(request: IncomingMessage, tokenHash: Buffer): boolean {
  const authorization = request.headers.authorization;
  if (authorization === undefined || !authorization.startsWith("Bearer ")) return false;
  const suppliedHash = createHash("sha256").update(authorization.slice("Bearer ".length)).digest();
  return timingSafeEqual(suppliedHash, tokenHash);
}

function clientRunErrorMessage(error: unknown): string {
  const status =
    error !== null && typeof error === "object" && "status" in error
      ? (error as {status?: unknown}).status
      : undefined;
  if (typeof status === "number" && Number.isSafeInteger(status) && status >= 100 && status <= 599) {
    return `The provider request failed (HTTP ${status}). Response details were withheld.`;
  }
  return "The run failed. Response details were withheld; check server logs for diagnostics.";
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

/**
 * The `profile` query parameter, which addresses one path profile of a session.
 *
 * Silence means "the session's stored default", which is what every existing
 * client sends; a profile that the session does not configure is rejected by the
 * config loader when the handle is built.
 */
function parseProfileParameter(url: URL): string | undefined {
  const value = url.searchParams.get("profile");
  if (value === null || value.length === 0) {
    return undefined;
  }
  if (!PROFILE_NAME_PATTERN.test(value)) {
    throw new HttpError(400, "profile must contain only letters, numbers, underscores, or hyphens");
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
): Promise<{
  config: WebUiSessionConfig;
  paths: SessionStatePaths;
  usageCachePath: string;
  profile?: string;
}> {
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
    ...(settings.paths.active_profile === undefined
      ? {}
      : {profile: settings.paths.active_profile}),
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
  runsByProfile?: Record<string, WebUiSessionRun>,
): WebUiSessionSnapshot {
  const usageValue = sessionUsageFromSummary(usage);
  return {
    id: location.ref,
    ...sessionSettingsFromConfig(config),
    turns: view.turns,
    messages: view.messages,
    ...(usageValue === undefined ? {} : {usage: usageValue}),
    ...(run === undefined ? {} : {run}),
    ...(runsByProfile === undefined ? {} : {runs_by_profile: runsByProfile}),
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
  runsByProfile?: Record<string, WebUiSessionRun>,
): Omit<WebUiSessionSnapshot, "messages" | "page"> {
  const usageValue = sessionUsageFromSummary(usage);
  return {
    id: location.ref,
    ...sessionSettingsFromConfig(config),
    turns,
    ...(usageValue === undefined ? {} : {usage: usageValue}),
    ...(run === undefined ? {} : {run}),
    ...(runsByProfile === undefined ? {} : {runs_by_profile: runsByProfile}),
    session_id: location.sessionId,
    root_id: location.root.id,
    root_label: location.root.label,
  };
}

class AgentHttpApplication {
  /** One entry per `(session, profile)`: the key is `profileKey(location.ref, profile)`. */
  private readonly sessions = new Map<string, Promise<SessionHandle>>();
  /**
   * The handle an unqualified request (no `?profile=`) resolves to, so the common
   * path stays a map lookup instead of reading `setting.json` per request. The
   * stored default decides what it means; a write that changes the default drops
   * the entry, exactly as the profile switch did when it reopened the session.
   */
  private readonly defaultHandles = new Map<string, Promise<SessionHandle>>();
  /** Resolved aliases; failed lookups are evicted so newly created sessions can appear. */
  private readonly locations = new Map<string, Promise<SessionLocation>>();
  /** Tool stores shared only by handles for the same session and profile. */
  private readonly sharedStores = new Map<string, Promise<SessionSharedStores>>();
  private readonly changingSessionSettings = new Set<string>();
  /** Keyed like the handles: compaction is per `(session, profile)`. */
  private readonly compactingSessions = new Set<string>();
  /** Transcript rewrites are serialized with runs and compactions for one profile row. */
  private readonly changingTranscripts = new Set<string>();
  private readonly sessionRoots: Promise<SessionRoot[]>;
  private readonly rootLibraryOptions: SessionRootOptions;
  private readonly sessionOptions: {
    client?: OpenAI;
    environment?: NodeJS.ProcessEnv;
  };
  private readonly maxBodyBytes: number;
  private readonly toolStreamIntervalMs: number;
  private readonly sseMaxBufferedBytes: number;
  private readonly conditionalRequests: boolean;
  private readonly snapshotTurns: number;
  private readonly maxPageTurns: number;
  private readonly maxInlinePartBytes: number;
  private readonly runs: SessionRunRegistry;
  private readonly apiTokenHash: Buffer;
  private readonly allowedOrigins: ReadonlySet<string>;

  public constructor(options: AgentServerOptions = {}) {
    const environment = options.environment ?? process.env;
    const token = options.apiToken ?? environment[API_TOKEN_ENV_NAME];
    if (typeof token !== "string" || token.length < MIN_API_TOKEN_LENGTH || /[\r\n]/.test(token)) {
      throw new ConfigError(`${API_TOKEN_ENV_NAME} must be a secret of at least ${MIN_API_TOKEN_LENGTH} characters`);
    }
    this.apiTokenHash = createHash("sha256").update(token).digest();
    this.allowedOrigins = new Set(options.allowedOrigins ?? DEFAULT_ROOT_SERVER_SETTINGS.allowed_origins);
    this.rootLibraryOptions = {
      ...(options.sessionsDir === undefined ? {} : {sessionsDir: options.sessionsDir}),
      ...(options.rootSettingPath === undefined ? {} : {rootSettingPath: options.rootSettingPath}),
      ...(options.environment === undefined ? {} : {environment: options.environment}),
    };
    this.sessionRoots = loadSessionRoots(this.rootLibraryOptions);
    this.sessionOptions = {
      ...(options.client ? { client: options.client } : {}),
      environment: environmentWithoutApiToken(environment),
    };
    this.maxBodyBytes = options.maxBodyBytes ?? DEFAULT_ROOT_SERVER_SETTINGS.max_body_bytes;
    this.toolStreamIntervalMs =
      options.toolStreamIntervalMs ?? DEFAULT_ROOT_SERVER_SETTINGS.tool_stream_interval_ms;
    this.sseMaxBufferedBytes =
      options.sseMaxBufferedBytes ?? DEFAULT_ROOT_SERVER_SETTINGS.sse_max_buffered_bytes;
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
    try {
      const origin = request.headers.origin;
      if (origin !== undefined) {
        if (!this.allowedOrigins.has(origin)) throw new HttpError(403, "origin is not allowed");
        response.setHeader("Access-Control-Allow-Origin", origin);
        response.setHeader("Vary", "Origin");
        response.setHeader("Access-Control-Allow-Headers", "Authorization, Content-Type, If-None-Match");
        response.setHeader("Access-Control-Expose-Headers", "ETag");
        response.setHeader("Access-Control-Allow-Methods", "GET, POST, OPTIONS");
      }
      if (request.method === "OPTIONS") {
        response.writeHead(204);
        response.end();
        return;
      }

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

      if (segments[0] === "api" && segments[1] === "library") {
        if (!isAuthorized(request, this.apiTokenHash)) {
          response.setHeader("WWW-Authenticate", 'Bearer realm="agent-harness"');
          throw new HttpError(401, "API token required");
        }
        response.setHeader("Cache-Control", "no-store");
        if (request.method === "GET" && segments.length === 2) {
          sendJson(response, 200, await listRootLibrary(this.rootLibraryOptions));
          return;
        }
        if (request.method === "GET" && segments.length === 4) {
          const kindValue = segments[2];
          if (kindValue !== "prompt" && kindValue !== "template" && kindValue !== "skill") {
            throw new HttpError(404, "not found");
          }
          const asset = await readRootLibraryAsset(
            kindValue,
            segments[3] ?? "",
            this.rootLibraryOptions,
          );
          if (asset === undefined) throw new HttpError(404, "library item not found");
          sendJson(response, 200, {asset});
          return;
        }
        throw new HttpError(404, "not found");
      }

      if (segments[0] !== "api" || segments[1] !== "sessions") {
        throw new HttpError(404, "not found");
      }
      if (!isAuthorized(request, this.apiTokenHash)) {
        response.setHeader("WWW-Authenticate", 'Bearer realm="agent-harness"');
        throw new HttpError(401, "API token required");
      }

      if (request.method === "GET" && segments.length === 2) {
        await this.handleSessionList(request, response);
        return;
      }

      const requestedRef = validateSessionReference(segments[2] ?? "");
      // Aliases (root-qualified and unqualified) must share locks and cached
      // default handles. Never use an unnormalized request reference as a key.
      const sessionRef = (await this.cachedSessionLocation(requestedRef)).ref;
      // One profile of the session, or the stored default when the request is
      // silent — which is what every client sent before profiles were
      // addressable, so an unqualified request keeps its exact meaning.
      const profile = parseProfileParameter(url);
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
        await this.handleSessionModelDefault(sessionRef, request, response, profile);
        return;
      }
      if (request.method === "GET" && segments.length === 3) {
        await this.handleSessionSnapshot(sessionRef, request, response, profile);
        return;
      }

      if (
        request.method === "GET" &&
        segments.length === 4 &&
        segments[3] === "tools"
      ) {
        await this.handleTools(sessionRef, response, profile);
        return;
      }

      if (
        request.method === "GET" &&
        segments.length === 4 &&
        segments[3] === "files"
      ) {
        await this.handleSessionFiles(sessionRef, response, profile);
        return;
      }

      if (
        request.method === "GET" &&
        segments.length === 5 &&
        segments[3] === "messages"
      ) {
        await this.handleSessionMessage(sessionRef, segments[4] ?? "", url, response, profile);
        return;
      }

      if (
        request.method === "GET" &&
        segments.length === 4 &&
        segments[3] === "history"
      ) {
        await this.handleSessionHistory(sessionRef, request, response, profile);
        return;
      }

      if (
        request.method === "POST" &&
        segments.length === 4 &&
        segments[3] === "compact"
      ) {
        await this.handleCompact(sessionRef, response, profile);
        return;
      }

      if (
        request.method === "POST" &&
        segments.length === 4 &&
        segments[3] === "front-compact"
      ) {
        await this.handleFrontCompact(sessionRef, request, response, profile);
        return;
      }

      if (
        request.method === "POST" &&
        segments.length === 4 &&
        segments[3] === "reset"
      ) {
        await this.handleReset(sessionRef, response, profile);
        return;
      }

      if (
        request.method === "GET" &&
        segments.length === 5 &&
        segments[3] === "run" &&
        segments[4] === "stream"
      ) {
        await this.handleRunStream(sessionRef, response, profile);
        return;
      }

      if (
        request.method === "POST" &&
        segments.length === 5 &&
        segments[3] === "run" &&
        segments[4] === "stop"
      ) {
        await this.handleRunStop(sessionRef, response, profile);
        return;
      }

      if (
        request.method === "POST" &&
        segments.length === 6 &&
        segments[3] === "run" &&
        segments[4] === "interaction" &&
        segments[5] === "extend"
      ) {
        await this.handleRunInteractionExtend(sessionRef, request, response, profile);
        return;
      }

      if (
        request.method === "POST" &&
        segments.length === 5 &&
        segments[3] === "run" &&
        segments[4] === "interaction"
      ) {
        await this.handleRunInteraction(sessionRef, request, response, profile);
        return;
      }

      if (
        request.method === "POST" &&
        segments.length === 4 &&
        segments[3] === "run"
      ) {
        await this.handleRun(sessionRef, request, response, profile);
        return;
      }

      throw new HttpError(404, "not found");
    } catch (error) {
      if (response.headersSent) {
        if (!response.writableEnded && !response.destroyed) {
          writeSse(response, "error", { message: errorMessage(error) }, this.sseMaxBufferedBytes);
          if (!response.writableEnded && !response.destroyed) {
            response.end();
          }
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

  private async cachedSessionLocation(reference: string): Promise<SessionLocation> {
    let pending = this.locations.get(reference);
    if (pending === undefined) {
      pending = this.resolveSessionLocation(reference);
      this.locations.set(reference, pending);
    }
    try {
      return await pending;
    } catch (error) {
      if (this.locations.get(reference) === pending) this.locations.delete(reference);
      throw error;
    }
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

  /**
   * The handle of a request.
   *
   * Without a profile it follows the session's stored default and is cached per
   * reference; with one it is the handle of that profile, which may coexist with
   * the same session's other profiles and run at the same time.
   */
  private async getSession(reference: string, profile?: string): Promise<SessionHandle> {
    if (profile !== undefined) {
      const location = await this.cachedSessionLocation(reference);
      return this.acquireHandle(location, profile);
    }
    const cached = this.defaultHandles.get(reference);
    if (cached !== undefined) {
      return cached;
    }
    const pending = (async (): Promise<SessionHandle> => {
      const location = await this.cachedSessionLocation(reference);
      const directory = join(location.root.directory, location.sessionId);
      const loader = new SessionConfigLoader(directory, {
        globalJadRoot: location.root.globalJadRoot,
      });
      const settings = await loader.loadSettings();
      // The stored default is resolved once, then pinned by name: a later switch
      // of `active_profile` cannot invalidate this handle.
      return this.acquireHandle(location, settings.paths.active_profile);
    })();
    this.defaultHandles.set(reference, pending);
    try {
      return await pending;
    } catch (error) {
      this.defaultHandles.delete(reference);
      throw error;
    }
  }

  private async acquireHandle(
    location: SessionLocation,
    profile: string | undefined,
  ): Promise<SessionHandle> {
    const key = profileKey(location.ref, profile);
    const existing = this.sessions.get(key);
    if (existing) {
      return existing;
    }
    const pending = this.openHandle(location, profile);
    this.sessions.set(key, pending);
    try {
      return await pending;
    } catch (error) {
      if (this.sessions.get(key) === pending) {
        this.sessions.delete(key);
      }
      throw error;
    }
  }

  private async openHandle(
    location: SessionLocation,
    profile: string | undefined,
  ): Promise<SessionHandle> {
    const directory = join(location.root.directory, location.sessionId);
    const loader = new SessionConfigLoader(directory, {
      globalJadRoot: location.root.globalJadRoot,
      ...(profile === undefined ? {} : {profile}),
    });
    // Loading the settings rejects an unusable session (and an unknown profile)
    // before it is cached.
    const settings = await loader.loadSettings();
    // Use the resolved profile so the implicit default handle and an explicitly
    // selected handle for that same profile share one set of store instances.
    const storageProfile = settings.paths.active_profile;
    const shared = await this.acquireSharedStores(directory, storageProfile, settings);
    let session: AgentSession;
    try {
      session = await AgentSession.open(directory, {
        ...this.sessionOptions,
        globalJadRoot: location.root.globalJadRoot,
        ...(profile === undefined ? {} : {profile}),
        shared,
        onClosed: () => this.releaseSharedStores(directory, storageProfile),
      });
    } catch (error) {
      this.releaseSharedStores(directory, storageProfile);
      throw error;
    }
    return {
      location,
      directory,
      session,
      loader,
      ...(profile === undefined ? {} : {profile}),
      view: new WebUiSessionViewCache(session.store, {
        snapshotTurns: this.snapshotTurns,
        maxPageTurns: this.maxPageTurns,
      }),
    };
  }

  /** The process, task and memory stores for one session/profile pair. */
  private async acquireSharedStores(
    directory: string,
    profile: string | undefined,
    settings: SessionSettings,
  ): Promise<SessionSharedStores> {
    const key = sharedStoresKey(directory, profile);
    const existing = this.sharedStores.get(key);
    if (existing !== undefined) {
      const stores = await existing;
      stores.refs += 1;
      return stores;
    }
    const pending = (async (): Promise<SessionSharedStores> => {
      const processManager = new ProcessManager(
        settings.tools.native.process?.storage_dir
          ?? join(settings.cwd, DEFAULT_NATIVE_PROCESS_STORAGE_DIR),
      );
      await processManager.recover();
      return {
        processManager,
        taskStore: new TaskStore(
          settings.tools.native.task?.storage_dir
            ?? join(settings.cwd, DEFAULT_NATIVE_TASK_STORAGE_DIR),
        ),
        memoryStore: new MemoryStore(
          settings.tools.native.memory?.storage_dir
            ?? join(settings.cwd, DEFAULT_NATIVE_MEMORY_STORAGE_DIR),
        ),
        refs: 1,
      };
    })();
    this.sharedStores.set(key, pending);
    try {
      return await pending;
    } catch (error) {
      this.sharedStores.delete(key);
      throw error;
    }
  }

  private async releaseSharedStores(
    directory: string,
    profile: string | undefined,
  ): Promise<void> {
    const key = sharedStoresKey(directory, profile);
    const pending = this.sharedStores.get(key);
    if (pending === undefined) {
      return;
    }
    const stores = await pending;
    stores.refs -= 1;
    if (stores.refs > 0) {
      return;
    }
    this.sharedStores.delete(key);
    await stores.processManager.close();
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
            this.runs.runsOf(location.ref),
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
            this.runs.describe(location.ref, listing.profile),
            this.runs.describeAll(location.ref),
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
    profile?: string,
  ): Promise<void> {
    const url = new URL(request.url ?? "/", "http://agent-harness.local");
    const before = parseCursorParameter(url.searchParams.get("before"), "before");
    const cursorGeneration = parseCursorGenerationParameter(url.searchParams.get("cursor_generation"));
    const turns = parseTurnsParameter(url.searchParams.get("turns"));
    const handle = await this.getSession(id, profile);
    const run = this.runs.describe(handle.location.ref, handle.profile);
    const etag = await sessionStateVersion(
      sessionStatePaths(handle.loader.paths),
      this.runs.runsOf(handle.location.ref),
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
      this.runs.describeAll(handle.location.ref),
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
    profile?: string,
  ): Promise<void> {
    const handle = await this.getSession(id, profile);
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
    profile?: string,
  ): Promise<void> {
    const handle = await this.getSession(id, profile);
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

  private async handleRunStream(id: string, response: ServerResponse, profile?: string): Promise<void> {
    // Resolving the session first keeps unknown references a 404 instead of an
    // empty stream.
    const handle = await this.getSession(id, profile);
    response.writeHead(200, {
      "Content-Type": "text/event-stream; charset=utf-8",
      "Cache-Control": "no-cache, no-store, must-revalidate",
      Connection: "keep-alive",
      "X-Accel-Buffering": "no",
    });
    response.flushHeaders();

    const run = this.runs.current(handle.location.ref, handle.profile);
    if (run === undefined) {
      writeSse(response, "idle", {}, this.sseMaxBufferedBytes);
      response.end();
      return;
    }

    writeSse(response, "run", run.summary(), this.sseMaxBufferedBytes);
    let detach = (): void => {};
    response.once("close", () => detach());
    detach = run.subscribe((event) => {
      writeSse(response, event.event, event.data, this.sseMaxBufferedBytes);
      if (event.event === "done" && !response.writableEnded && !response.destroyed) {
        response.end();
      }
    });
    if (run.status !== "running" && !response.writableEnded && !response.destroyed) {
      // The retained terminal events were replayed synchronously; the stream is
      // complete.
      response.end();
    }
  }

  private async handleRunStop(id: string, response: ServerResponse, profile?: string): Promise<void> {
    const handle = await this.getSession(id, profile);
    sendJson(response, 200, {stop: this.runs.stop(handle.location.ref, handle.profile)});
  }

  /**
   * Answers the question a run is waiting on.
   *
   * The reply names the question it answers, so a stale tab, a retry, or a
   * second click after the deadline resolves to "nothing changed" instead of
   * answering whatever question happens to be pending. The edited draft is
   * measured against the limits that travelled with the question.
   */
  private async handleRunInteraction(
    id: string,
    request: IncomingMessage,
    response: ServerResponse,
    profile?: string,
  ): Promise<void> {
    const handle = await this.getSession(id, profile);
    const run = this.runs.current(handle.location.ref, handle.profile);
    if (run === undefined) {
      throw new HttpError(404, "no run of this session is waiting for an answer");
    }
    const parsed = parseInteractionReplyRequest(
      await readJsonBody(request, this.maxBodyBytes),
      run.interactions.limits,
    );
    if ("error" in parsed) {
      throw new HttpError(400, parsed.error);
    }
    const status = run.interactions.reply(parsed.interaction_id, parsed);
    if (status !== "answered") {
      throw new HttpError(
        409,
        status === "expired"
          ? "the draft expired before the answer arrived"
          : "the run is not waiting for that question",
      );
    }
    sendJson(response, 200, {
      interaction: {
        interaction_id: parsed.interaction_id,
        decision: parsed.decision,
        status,
      },
    });
  }

  /**
   * Pushes a waiting question's deadline out by the session's own step. The
   * amount is not in the body: the step and the ceiling are the session's, so a
   * caller cannot ask for an arbitrary wait.
   */
  private async handleRunInteractionExtend(
    sessionRef: string,
    request: IncomingMessage,
    response: ServerResponse,
    profile: string | undefined,
  ): Promise<void> {
    const handle = await this.getSession(sessionRef, profile);
    const run = this.runs.current(handle.location.ref, handle.profile);
    if (run === undefined) {
      throw new HttpError(404, "no run of this session is waiting for an answer");
    }
    const parsed = parseInteractionExtendRequest(await readJsonBody(request, this.maxBodyBytes));
    if ("error" in parsed) {
      throw new HttpError(400, parsed.error);
    }
    const status = run.interactions.extend(parsed.interaction_id);
    if (status !== "extended") {
      throw new HttpError(
        409,
        status === "expired"
          ? "the draft expired before it could be extended"
          : status === "at_max"
            ? "the draft is already at the longest wait this session allows"
            : "the run is not waiting for that question",
      );
    }
    const pending = run.interactions.pending;
    if (pending === undefined) {
      throw new HttpError(409, "the run is not waiting for that question");
    }
    // The run republished the question with its new deadline; the response is the
    // same value, so a caller that cannot watch the stream still learns it.
    sendJson(response, 200, {
      interaction: {
        interaction_id: parsed.interaction_id,
        status: "pending",
        expires_at: pending.expires_at,
        max_expires_at: pending.max_expires_at,
      },
    });
  }

  private isTranscriptChanging(reference: string): boolean {
    for (const key of this.changingTranscripts) {
      if (key === reference || key.startsWith(`${reference}|`)) {
        return true;
      }
    }
    return false;
  }

  private async handleCompact(id: string, response: ServerResponse, profile?: string): Promise<void> {
    if (this.changingSessionSettings.has(id)) {
      throw new HttpError(409, "session profile or compaction is already changing");
    }
    const handle = await this.getSession(id, profile);
    // Compaction rewrites one profile's transcript, so the guard is per profile:
    // two profiles of one session may compact at the same time.
    const key = profileKey(handle.location.ref, handle.profile);
    if (this.compactingSessions.has(key) || this.changingTranscripts.has(key)) {
      throw new HttpError(409, "session profile or compaction is already changing");
    }
    this.compactingSessions.add(key);
    try {
      if (this.changingSessionSettings.has(id) || this.changingTranscripts.has(key)) {
        throw new HttpError(409, "session settings are changing");
      }
      const {compacted, compacted_items, retained_items} = await handle.session.compact();
      const usage = await handle.session.readUsageSummary();
      sendJson(response, 200, {compaction: {compacted, compacted_items, retained_items}, usage});
    } finally {
      this.compactingSessions.delete(key);
    }
  }

  private async handleFrontCompact(
    id: string,
    request: IncomingMessage,
    response: ServerResponse,
    profile?: string,
  ): Promise<void> {
    if (this.changingSessionSettings.has(id)) {
      throw new HttpError(409, "session settings are changing");
    }
    const body = await readJsonBody(request, this.maxBodyBytes);
    if (
      !isRecord(body) ||
      typeof body.message_id !== "string" ||
      !/^session-user-o(0|[1-9][0-9]*)$/.test(body.message_id)
    ) {
      throw new HttpError(400, "message_id must identify an active transcript user message");
    }
    const handle = await this.getSession(id, profile);
    const key = profileKey(handle.location.ref, handle.profile);
    if (
      this.changingSessionSettings.has(id) ||
      this.compactingSessions.has(key) ||
      this.changingTranscripts.has(key)
    ) {
      throw new HttpError(409, "session profile or transcript is already changing");
    }
    if (this.runs.current(handle.location.ref, handle.profile)?.status === "running") {
      throw new HttpError(409, "cannot front compact a profile while a run is active");
    }

    this.changingTranscripts.add(key);
    try {
      if (
        this.changingSessionSettings.has(id) ||
        this.compactingSessions.has(key) ||
        this.runs.current(handle.location.ref, handle.profile)?.status === "running"
      ) {
        throw new HttpError(409, "cannot front compact a profile while it is changing or running");
      }
      const settings = await handle.loader.loadSettings();
      if (!settings.front_compaction.enabled) {
        throw new HttpError(403, "front compaction is disabled for this session");
      }
      try {
        const result = await handle.session.frontCompactFromMessage(
          body.message_id,
          settings.front_compaction.delete_mode,
        );
        sendJson(response, 200, {front_compaction: result});
      } catch (error) {
        if (error instanceof PersistenceError) {
          throw new HttpError(409, error.message);
        }
        throw error;
      }
    } finally {
      this.changingTranscripts.delete(key);
    }
  }

  private async handleReset(id: string, response: ServerResponse, profile?: string): Promise<void> {
    if (this.changingSessionSettings.has(id)) {
      throw new HttpError(409, "session settings are changing");
    }
    const handle = await this.getSession(id, profile);
    const key = profileKey(handle.location.ref, handle.profile);
    if (
      this.changingSessionSettings.has(id) ||
      this.compactingSessions.has(key) ||
      this.changingTranscripts.has(key)
    ) {
      throw new HttpError(409, "session profile is already changing");
    }
    if (this.runs.current(handle.location.ref, handle.profile)?.status === "running") {
      throw new HttpError(409, "cannot reset a profile while a run is active");
    }

    this.changingTranscripts.add(key);
    try {
      if (
        this.changingSessionSettings.has(id) ||
        this.compactingSessions.has(key) ||
        this.runs.current(handle.location.ref, handle.profile)?.status === "running"
      ) {
        throw new HttpError(409, "cannot reset a profile while it is changing or running");
      }
      await handle.session.reset();
      sendJson(response, 200, {reset: true});
    } finally {
      this.changingTranscripts.delete(key);
    }
  }

  private async handleTools(
    id: string,
    response: ServerResponse,
    profile?: string,
  ): Promise<void> {
    const handle = await this.getSession(id, profile);
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
    profile?: string,
  ): Promise<void> {
    const handle = await this.getSession(id, profile);
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

  /**
   * Moves the session's *stored* default profile.
   *
   * The write touches one line of `setting.json`; the open handles of either
   * profile stay pinned and alive, so a run in another profile keeps going. Only
   * the alias an unqualified request resolves through is dropped, so the next one
   * builds (or reuses) the handle of the new default.
   */
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
    if (!PROFILE_NAME_PATTERN.test(profileName)) {
      throw new HttpError(
        400,
        "active_profile must contain only letters, numbers, underscores, or hyphens",
      );
    }
    if (this.changingSessionSettings.has(reference) || this.isTranscriptChanging(reference)) {
      throw new HttpError(409, "session path profile is already changing");
    }

    // Acquire the per-session settings guard before the first await. Otherwise
    // simultaneous requests can both pass the check while resolving the session
    // location, then race their read-modify-write updates to setting.json.
    this.changingSessionSettings.add(reference);
    try {
      const location = await this.cachedSessionLocation(reference);
      if (this.isTranscriptChanging(location.ref)) {
        throw new HttpError(409, "session path profile cannot change during a transcript update");
      }
      const directory = join(location.root.directory, location.sessionId);
      const loader = new SessionConfigLoader(directory, {
        globalJadRoot: location.root.globalJadRoot,
      });
      const settings = await loader.loadSettings();
      const stored = settings.paths.active_profile;
      // Only the profile being switched away from is affected: its handle keeps
      // the paths it was opened on, so its in-flight run must not be disturbed.
      if (stored !== undefined && this.runs.current(reference, stored)?.status === "running") {
        throw new HttpError(409, "cannot change the session path profile while a run is active");
      }
      if (stored !== undefined && this.compactingSessions.has(profileKey(reference, stored))) {
        throw new HttpError(409, "cannot change the session path profile during compaction");
      }

      const target = new SessionConfigLoader(directory, {
        globalJadRoot: location.root.globalJadRoot,
        profile: profileName,
      });
      let targetSettings: SessionSettings;
      try {
        targetSettings = await target.loadSettings();
      } catch (error) {
        if (error instanceof ConfigError) {
          throw new HttpError(400, error.message);
        }
        throw error;
      }
      // A profile that has never been the stored default uses its own derived
      // directory. Pin both resolved path sets before switching so neither
      // profile's storage changes when their roles reverse.
      const storagePathsByProfile: Record<string, SessionProfileStoragePaths> | undefined =
        stored === undefined || stored === profileName
          ? undefined
          : {
              [stored]: sessionProfileStoragePaths(settings.paths),
              [profileName]: sessionProfileStoragePaths(targetSettings.paths),
            };

      let update;
      try {
        update = await setSessionPathProfile(
          loader.paths.setting_path,
          profileName,
          settings.persistence.durability,
          storagePathsByProfile,
        );
      } catch (error) {
        if (error instanceof SessionPathProfileError) {
          throw new HttpError(400, error.message);
        }
        throw error;
      }

      if (update.changed) {
        this.defaultHandles.delete(reference);
      }
      // Refresh the default_profile field after the stored default changes; the
      // target's selected storage paths remain pinned to the same profile.
      targetSettings = await target.loadSettings();
      const profileInfo = await readSessionPathProfiles(target.paths.setting_path);
      sendJson(response, 200, {
        ...profileInfo,
        ...(targetSettings.paths.model_default === undefined
          ? {}
          : {model_default: targetSettings.paths.model_default}),
        ...(targetSettings.paths.models === undefined
          ? {}
          : {models: targetSettings.paths.models}),
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
    profile?: string,
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
    if (this.changingSessionSettings.has(reference) || this.isTranscriptChanging(reference)) {
      throw new HttpError(409, "session settings are already changing");
    }
    const handle = await this.getSession(reference, profile);
    if (this.changingSessionSettings.has(reference) || this.isTranscriptChanging(handle.location.ref)) {
      throw new HttpError(409, "session settings are already changing");
    }

    this.changingSessionSettings.add(reference);
    try {
      const settings = await handle.loader.loadSettings();
      const activeProfile = profile ?? settings.paths.active_profile;
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
        // The profile the selection landed in, which a pinned request names
        // itself and the stored default may not be.
        active_profile: update.active_profile,
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
    profile?: string,
  ): Promise<void> {
    const body = await readJsonBody(request, this.maxBodyBytes);
    if (!isRecord(body) || typeof body.input !== "string" || body.input.trim().length === 0) {
      throw new HttpError(400, "input must be a non-empty string");
    }

    if (body.model !== undefined && (typeof body.model !== "string" || body.model.trim().length === 0)) {
      throw new HttpError(400, "model must be a non-empty string");
    }
    const requestedModelPath = typeof body.model === "string" ? body.model.trim() : undefined;
    let connected = true;
    const started = await this.startSessionRun({
      reference: id,
      ...(profile === undefined ? {} : {profile}),
      input: body.input,
      ...(requestedModelPath === undefined ? {} : {modelConfigPath: requestedModelPath}),
      isConnected: () => connected,
    });

    response.writeHead(200, {
      "Content-Type": "text/event-stream; charset=utf-8",
      "Cache-Control": "no-cache, no-store, must-revalidate",
      Connection: "keep-alive",
      "X-Accel-Buffering": "no",
    });
    response.flushHeaders();
    response.once("close", () => {
      // Disconnecting a browser does not discard an accepted, persisted run: the
      // run stays registered so a reload can attach to it again.
      connected = false;
    });

    // The response that started the run is one subscriber among others, so a
    // second tab (or the same tab after a reload) can attach to the same run.
    const detach = started.run.subscribe((event) => {
      writeSse(response, event.event, event.data, this.sseMaxBufferedBytes);
      if (event.event === "done" && !response.writableEnded && !response.destroyed) {
        response.end();
      }
    });
    response.once("close", detach);
    await started.completion;
    if (!response.writableEnded && !response.destroyed) {
      response.end();
    }
  }

  /**
   * Accepts and registers a run in one `(session, profile)`.
   *
   * The route that a browser posts to and an in-process caller that starts a
   * sub-agent run share this: both get a run the registry exposes, so any client
   * can discover, attach to, or stop it — including after a reload. The run is
   * already executing when this resolves; `completion` settles with it.
   */
  private async startSessionRun(options: {
    reference: string;
    profile?: string;
    input: string;
    modelConfigPath?: string;
    isConnected?: () => boolean;
  }): Promise<{run: SessionRun; turnId: string; handle: SessionHandle; completion: Promise<void>}> {
    const handle = await this.getSession(options.reference, options.profile);
    const key = profileKey(handle.location.ref, handle.profile);
    if (this.changingSessionSettings.has(options.reference) || this.changingTranscripts.has(key)) {
      throw new HttpError(409, "session settings or transcript are changing");
    }
    const settings = await handle.loader.loadSettings();
    if (this.changingSessionSettings.has(options.reference) || this.changingTranscripts.has(key)) {
      throw new HttpError(409, "session settings or transcript are changing");
    }
    if (options.modelConfigPath !== undefined && !settings.paths.models?.includes(options.modelConfigPath)) {
      throw new HttpError(400, "model config path is not available in the active session path profile");
    }
    const modelConfigPath = options.modelConfigPath ?? settings.paths.model_default;
    const turnId = `web-${randomUUID()}`;
    const controller = new AbortController();
    controller.signal.addEventListener(
      "abort",
      () => {
        const reason = controller.signal.reason;
        const detail = reason instanceof Error
          ? `${reason.name}: ${reason.message}`
          : String(reason ?? "unspecified");
        console.warn(
          `[agent-run] aborted session=${options.reference} profile=${handle.profile ?? "-"} turn=${turnId} reason=${detail}`,
        );
      },
      {once: true},
    );
    const run = this.runs.begin(handle.location.ref, handle.profile, turnId, controller);
    const emitter = new MessageStreamEmitter(
      this.toolStreamIntervalMs,
      (message, append) => run.publishMessage(message, append),
    );
    const completion = this.executeSessionRun({
      handle,
      run,
      emitter,
      controller,
      input: options.input,
      turnId,
      ...(modelConfigPath === undefined ? {} : {modelConfigPath}),
      isConnected: options.isConnected ?? (() => true),
    });
    return {run, turnId, handle, completion};
  }

  /** Runs one accepted turn and closes its registry entry either way. */
  private async executeSessionRun(options: {
    handle: SessionHandle;
    run: SessionRun;
    emitter: MessageStreamEmitter;
    controller: AbortController;
    input: string;
    turnId: string;
    modelConfigPath?: string;
    isConnected: () => boolean;
  }): Promise<void> {
    const {handle, run, emitter, controller, turnId} = options;
    const reference = handle.location.ref;
    try {
      const normalizedRun = new WebUiNormalizedRun(turnId);
      const publishUsage = async () => {
        try {
          const usage = await handle.session.readUsageSummary();
          run.publish("usage", {usage});
        } catch (error) {
          console.warn(
            `[agent-run] failed to publish usage session=${reference} turn=${turnId}: ${errorMessage(error)}`,
          );
        }
      };
      const result = await handle.session.run(options.input, {
        turnId,
        signal: controller.signal,
        // A tool that must ask the user gets the run's channel: the question
        // becomes message state, and the answer arrives over HTTP.
        requestInteraction: ({tool, tool_call_id, ...question}) =>
          run.interactions.begin(question, {tool, tool_call_id}, controller.signal),
        ...(options.modelConfigPath === undefined ? {} : {modelConfigPath: options.modelConfigPath}),
        onEvent: (event) => {
          const update = normalizedRun.updateForStream(event);
          if (update) {
            emitter.push(update.message, update.append);
          }
        },
        onResponseCommitted: publishUsage,
        onUsageChanged: publishUsage,
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
        `[agent-run] failed session=${reference} profile=${handle.profile ?? "-"} turn=${turnId} connected=${options.isConnected()} aborted=${aborted}: ${errorMessage(error)}`,
        error,
      );
      run.finish(aborted ? "aborted" : "failed", [
        {
          event: "error",
          data: {
            message: aborted
              ? "The run was stopped before it completed."
              : clientRunErrorMessage(error),
          },
        },
        {event: "done", data: {}},
      ]);
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
    sseMaxBufferedBytes:
      options.sseMaxBufferedBytes ?? serverSettings.sse_max_buffered_bytes,
    conditionalRequests:
      options.conditionalRequests ?? serverSettings.conditional_requests,
    snapshotTurns: options.snapshotTurns ?? serverSettings.snapshot_turns,
    maxPageTurns: options.maxPageTurns ?? serverSettings.max_page_turns,
    maxInlinePartBytes: options.maxInlinePartBytes ?? serverSettings.max_inline_part_bytes,
    allowedOrigins: options.allowedOrigins ?? serverSettings.allowed_origins,
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
