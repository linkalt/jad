import { readdir, readFile, stat } from "node:fs/promises";
import { dirname, resolve } from "node:path";

import { ConfigError } from "./config.js";

const ROOT_ID_PATTERN = /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/;
const DEFAULT_ROOT_ID = "default";
const DEFAULT_ROOT_LABEL = "Local sessions";
const MAX_SERVER_PORT = 65_535;
const MAX_SERVER_BODY_BYTES = 64 * 1024 * 1024;
const MAX_RUN_STATE_RETENTION_MS = 24 * 60 * 60 * 1000;
const MAX_TOOL_STREAM_INTERVAL_MS = 60 * 1000;
const MAX_SSE_HEARTBEAT_INTERVAL_MS = 5 * 60 * 1000;
const MAX_RUN_CONTENT_MEMORY_BYTES = 16 * 1024 * 1024;
const MAX_RUN_CONTENT_WRITE_BATCH_BYTES = 16 * 1024 * 1024;
const MAX_RUN_CONTENT_FLUSH_INTERVAL_MS = 60 * 1000;
const MAX_RUN_CONTENT_INDEX_SEGMENTS = 65_536;
export const DEFAULT_RUN_CONTENT_MEMORY_BYTES = 100 * 1024;
export const DEFAULT_RUN_CONTENT_WRITE_BATCH_BYTES = 32 * 1024;
export const DEFAULT_RUN_CONTENT_FLUSH_INTERVAL_MS = 25;
export const DEFAULT_RUN_CONTENT_MAX_INDEX_SEGMENTS = 1_024;
const MAX_SNAPSHOT_TURNS = 100_000;
const MAX_PAGE_TURNS = 1_000;
const MAX_INLINE_PART_BYTES = 16 * 1024 * 1024;
const MIN_SSE_BUFFERED_BYTES = 1024;
const MAX_SSE_BUFFERED_BYTES = 64 * 1024 * 1024;
/** Security floor, not a tunable feature flag. */
export const MIN_API_TOKEN_LENGTH = 32;

export const DEFAULT_ROOT_SERVER_SETTINGS = {
  host: "127.0.0.1",
  port: 8787,
  max_body_bytes: 1024 * 1024,
  run_state_retention_ms: 30_000,
  tool_stream_interval_ms: 120,
  run_content_memory_bytes: DEFAULT_RUN_CONTENT_MEMORY_BYTES,
  run_content_write_batch_bytes: DEFAULT_RUN_CONTENT_WRITE_BATCH_BYTES,
  run_content_flush_interval_ms: DEFAULT_RUN_CONTENT_FLUSH_INTERVAL_MS,
  run_content_max_index_segments: DEFAULT_RUN_CONTENT_MAX_INDEX_SEGMENTS,
  sse_max_buffered_bytes: 1024 * 1024,
  sse_heartbeat_interval_ms: 5_000,
  conditional_requests: true,
  snapshot_turns: 24,
  max_page_turns: 50,
  max_inline_part_bytes: 16 * 1024,
  allowed_origins: [] as string[],
} as const;

export const DEFAULT_ROOT_LIBRARY_DIRECTORIES = {
  prompt_dirs: [".agent/prompts"],
  template_dirs: [".agent/templates"],
  skill_dirs: [".agent/skills"],
} as const;

export interface RootLibraryDirectories {
  prompt_dirs: string[];
  template_dirs: string[];
  skill_dirs: string[];
}

export interface RootServerSettings {
  host: string;
  port: number;
  max_body_bytes: number;
  run_state_retention_ms: number;
  /** Coalescing window for in-progress tool-call snapshots; 0 streams every delta. */
  tool_stream_interval_ms: number;
  /** UTF-8 bytes of active-response deltas retained before spilling to responses.partial. */
  run_content_memory_bytes: number;
  /** UTF-8 target size for each asynchronous responses.partial append batch. */
  run_content_write_batch_bytes: number;
  /** Maximum delay before flushing a partial content append batch. */
  run_content_flush_interval_ms: number;
  /** Maximum redundant content ranges retained before the shared partial file is compacted. */
  run_content_max_index_segments: number;
  /** Maximum response-buffered SSE bytes per subscriber, before reconnecting to a snapshot. */
  sse_max_buffered_bytes: number;
  /** Gap between keep-alive frames on a quiet run stream; 0 disables them. */
  sse_heartbeat_interval_ms: number;
  /** Answer with `304` when a session snapshot is unchanged. */
  conditional_requests: boolean;
  /** Turns in a session snapshot; 0 returns the whole transcript. */
  snapshot_turns: number;
  /** Largest number of turns one page request may return. */
  max_page_turns: number;
  /**
   * Inline text kept per transcript part, in bytes. A larger part is cut for
   * transport, reports its full size, and can be re-fetched whole.
   */
  max_inline_part_bytes: number;
  /** Browser origins permitted to call the API. Empty means no cross-origin browser access. */
  allowed_origins: string[];
}

export interface SessionRoot {
  id: string;
  label: string;
  directory: string;
  globalJadRoot: string;
}

export interface SessionRootOptions {
  sessionsDir?: string;
  rootSettingPath?: string;
  environment?: NodeJS.ProcessEnv;
}

interface RootSettingRecord {
  schema_version?: unknown;
  session_roots?: unknown;
  session_paths?: unknown;
  server?: unknown;
  libraries?: unknown;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function requiredString(value: unknown, field: string, path: string): string {
  if (typeof value !== "string" || value.trim().length === 0) {
    throw new ConfigError(`${path}: ${field} must be a non-empty string`);
  }
  return value.trim();
}

function validateRootId(value: string, path: string): string {
  if (!ROOT_ID_PATTERN.test(value)) {
    throw new ConfigError(
      `${path}: id must match ${ROOT_ID_PATTERN.source}`,
    );
  }
  return value;
}

async function defaultSessionRoots(): Promise<SessionRoot[]> {
  const candidates = [
    {
      directory: resolve(process.cwd(), ".jad/sessions"),
      globalJadRoot: resolve(process.cwd()),
    },
    {
      directory: resolve(process.cwd(), "../.jad/sessions"),
      globalJadRoot: resolve(process.cwd(), ".."),
    },
  ];
  let selected = candidates[0]!;
  for (const candidate of candidates) {
    try {
      const info = await stat(candidate.directory);
      if (info.isDirectory()) {
        selected = candidate;
        break;
      }
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code !== "ENOENT") {
        throw error;
      }
    }
  }
  return [{
    id: DEFAULT_ROOT_ID,
    label: DEFAULT_ROOT_LABEL,
    ...selected,
  }];
}

function normalizeRootServerSettings(
  value: unknown,
  path: string,
): RootServerSettings {
  if (value === undefined) {
    return {...DEFAULT_ROOT_SERVER_SETTINGS};
  }
  if (!isRecord(value)) {
    throw new ConfigError(`${path}: server must be an object`);
  }

  const host = value.host === undefined
    ? DEFAULT_ROOT_SERVER_SETTINGS.host
    : requiredString(value.host, "host", path);
  const port = value.port ?? DEFAULT_ROOT_SERVER_SETTINGS.port;
  if (
    typeof port !== "number" ||
    !Number.isSafeInteger(port) ||
    port < 1 ||
    port > MAX_SERVER_PORT
  ) {
    throw new ConfigError(`${path}: server.port must be an integer between 1 and ${MAX_SERVER_PORT}`);
  }
  const maxBodyBytes = value.max_body_bytes ?? DEFAULT_ROOT_SERVER_SETTINGS.max_body_bytes;
  if (
    typeof maxBodyBytes !== "number" ||
    !Number.isSafeInteger(maxBodyBytes) ||
    maxBodyBytes < 1 ||
    maxBodyBytes > MAX_SERVER_BODY_BYTES
  ) {
    throw new ConfigError(
      `${path}: server.max_body_bytes must be a safe integer between 1 and ${MAX_SERVER_BODY_BYTES}`,
    );
  }
  const runStateRetentionMs =
    value.run_state_retention_ms ?? DEFAULT_ROOT_SERVER_SETTINGS.run_state_retention_ms;
  if (
    typeof runStateRetentionMs !== "number" ||
    !Number.isSafeInteger(runStateRetentionMs) ||
    runStateRetentionMs < 0 ||
    runStateRetentionMs > MAX_RUN_STATE_RETENTION_MS
  ) {
    throw new ConfigError(
      `${path}: server.run_state_retention_ms must be a safe integer between 0 and ${MAX_RUN_STATE_RETENTION_MS}`,
    );
  }
  const toolStreamIntervalMs =
    value.tool_stream_interval_ms ?? DEFAULT_ROOT_SERVER_SETTINGS.tool_stream_interval_ms;
  if (
    typeof toolStreamIntervalMs !== "number" ||
    !Number.isSafeInteger(toolStreamIntervalMs) ||
    toolStreamIntervalMs < 0 ||
    toolStreamIntervalMs > MAX_TOOL_STREAM_INTERVAL_MS
  ) {
    throw new ConfigError(
      `${path}: server.tool_stream_interval_ms must be a safe integer between 0 and ${MAX_TOOL_STREAM_INTERVAL_MS}`,
    );
  }
  const runContentMemoryBytes =
    value.run_content_memory_bytes ?? DEFAULT_ROOT_SERVER_SETTINGS.run_content_memory_bytes;
  if (
    typeof runContentMemoryBytes !== "number" ||
    !Number.isSafeInteger(runContentMemoryBytes) ||
    runContentMemoryBytes < 0 ||
    runContentMemoryBytes > MAX_RUN_CONTENT_MEMORY_BYTES
  ) {
    throw new ConfigError(
      `${path}: server.run_content_memory_bytes must be a safe integer between 0 and ${MAX_RUN_CONTENT_MEMORY_BYTES}`,
    );
  }
  const runContentWriteBatchBytes =
    value.run_content_write_batch_bytes ?? DEFAULT_ROOT_SERVER_SETTINGS.run_content_write_batch_bytes;
  if (
    typeof runContentWriteBatchBytes !== "number" ||
    !Number.isSafeInteger(runContentWriteBatchBytes) ||
    runContentWriteBatchBytes < 2 ||
    runContentWriteBatchBytes % 2 !== 0 ||
    runContentWriteBatchBytes > MAX_RUN_CONTENT_WRITE_BATCH_BYTES
  ) {
    throw new ConfigError(
      `${path}: server.run_content_write_batch_bytes must be an even safe integer between 2 and ${MAX_RUN_CONTENT_WRITE_BATCH_BYTES}`,
    );
  }
  const runContentFlushIntervalMs =
    value.run_content_flush_interval_ms ?? DEFAULT_ROOT_SERVER_SETTINGS.run_content_flush_interval_ms;
  if (
    typeof runContentFlushIntervalMs !== "number" ||
    !Number.isSafeInteger(runContentFlushIntervalMs) ||
    runContentFlushIntervalMs < 0 ||
    runContentFlushIntervalMs > MAX_RUN_CONTENT_FLUSH_INTERVAL_MS
  ) {
    throw new ConfigError(
      `${path}: server.run_content_flush_interval_ms must be a safe integer between 0 and ${MAX_RUN_CONTENT_FLUSH_INTERVAL_MS}`,
    );
  }
  const runContentMaxIndexSegments =
    value.run_content_max_index_segments ?? DEFAULT_ROOT_SERVER_SETTINGS.run_content_max_index_segments;
  if (
    typeof runContentMaxIndexSegments !== "number" ||
    !Number.isSafeInteger(runContentMaxIndexSegments) ||
    runContentMaxIndexSegments < 1 ||
    runContentMaxIndexSegments > MAX_RUN_CONTENT_INDEX_SEGMENTS
  ) {
    throw new ConfigError(
      `${path}: server.run_content_max_index_segments must be a safe integer between 1 and ${MAX_RUN_CONTENT_INDEX_SEGMENTS}`,
    );
  }
  const sseMaxBufferedBytes =
    value.sse_max_buffered_bytes ?? DEFAULT_ROOT_SERVER_SETTINGS.sse_max_buffered_bytes;
  if (
    typeof sseMaxBufferedBytes !== "number" ||
    !Number.isSafeInteger(sseMaxBufferedBytes) ||
    sseMaxBufferedBytes < MIN_SSE_BUFFERED_BYTES ||
    sseMaxBufferedBytes > MAX_SSE_BUFFERED_BYTES
  ) {
    throw new ConfigError(
      `${path}: server.sse_max_buffered_bytes must be a safe integer between ${MIN_SSE_BUFFERED_BYTES} and ${MAX_SSE_BUFFERED_BYTES}`,
    );
  }
  const sseHeartbeatIntervalMs =
    value.sse_heartbeat_interval_ms ?? DEFAULT_ROOT_SERVER_SETTINGS.sse_heartbeat_interval_ms;
  if (
    typeof sseHeartbeatIntervalMs !== "number" ||
    !Number.isSafeInteger(sseHeartbeatIntervalMs) ||
    sseHeartbeatIntervalMs < 0 ||
    sseHeartbeatIntervalMs > MAX_SSE_HEARTBEAT_INTERVAL_MS
  ) {
    throw new ConfigError(
      `${path}: server.sse_heartbeat_interval_ms must be a safe integer between 0 and ${MAX_SSE_HEARTBEAT_INTERVAL_MS}`,
    );
  }
  const conditionalRequests =
    value.conditional_requests ?? DEFAULT_ROOT_SERVER_SETTINGS.conditional_requests;
  if (typeof conditionalRequests !== "boolean") {
    throw new ConfigError(`${path}: server.conditional_requests must be a boolean`);
  }
  const snapshotTurns = value.snapshot_turns ?? DEFAULT_ROOT_SERVER_SETTINGS.snapshot_turns;
  if (
    typeof snapshotTurns !== "number" ||
    !Number.isSafeInteger(snapshotTurns) ||
    snapshotTurns < 0 ||
    snapshotTurns > MAX_SNAPSHOT_TURNS
  ) {
    throw new ConfigError(
      `${path}: server.snapshot_turns must be a safe integer between 0 and ${MAX_SNAPSHOT_TURNS}`,
    );
  }
  const maxPageTurns = value.max_page_turns ?? DEFAULT_ROOT_SERVER_SETTINGS.max_page_turns;
  if (
    typeof maxPageTurns !== "number" ||
    !Number.isSafeInteger(maxPageTurns) ||
    maxPageTurns < 1 ||
    maxPageTurns > MAX_PAGE_TURNS
  ) {
    throw new ConfigError(
      `${path}: server.max_page_turns must be a safe integer between 1 and ${MAX_PAGE_TURNS}`,
    );
  }
  const maxInlinePartBytes =
    value.max_inline_part_bytes ?? DEFAULT_ROOT_SERVER_SETTINGS.max_inline_part_bytes;
  if (
    typeof maxInlinePartBytes !== "number" ||
    !Number.isSafeInteger(maxInlinePartBytes) ||
    maxInlinePartBytes < 0 ||
    maxInlinePartBytes > MAX_INLINE_PART_BYTES
  ) {
    throw new ConfigError(
      `${path}: server.max_inline_part_bytes must be a safe integer between 0 and ${MAX_INLINE_PART_BYTES}`,
    );
  }
  const allowedOrigins = value.allowed_origins ?? DEFAULT_ROOT_SERVER_SETTINGS.allowed_origins;
  if (!Array.isArray(allowedOrigins) || !allowedOrigins.every((origin) => {
    if (typeof origin !== "string") return false;
    try {
      const url = new URL(origin);
      return (url.protocol === "http:" || url.protocol === "https:") &&
        !url.username && !url.password && url.origin === origin && url.pathname === "/" &&
        !url.search && !url.hash;
    } catch {
      return false;
    }
  })) {
    throw new ConfigError(`${path}: server.allowed_origins must be an array of exact http(s) origins`);
  }
  return {
    host,
    port,
    max_body_bytes: maxBodyBytes,
    run_state_retention_ms: runStateRetentionMs,
    tool_stream_interval_ms: toolStreamIntervalMs,
    run_content_memory_bytes: runContentMemoryBytes,
    run_content_write_batch_bytes: runContentWriteBatchBytes,
    run_content_flush_interval_ms: runContentFlushIntervalMs,
    run_content_max_index_segments: runContentMaxIndexSegments,
    sse_max_buffered_bytes: sseMaxBufferedBytes,
    sse_heartbeat_interval_ms: sseHeartbeatIntervalMs,
    conditional_requests: conditionalRequests,
    snapshot_turns: snapshotTurns,
    max_page_turns: maxPageTurns,
    max_inline_part_bytes: maxInlinePartBytes,
    allowed_origins: [...allowedOrigins],
  };
}

async function firstExistingPath(paths: readonly string[]): Promise<string | undefined> {
  for (const path of paths) {
    try {
      const info = await stat(path);
      if (info.isFile()) {
        return path;
      }
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code !== "ENOENT") {
        throw error;
      }
    }
  }
  return undefined;
}

async function readRootSetting(path: string): Promise<RootSettingRecord> {
  let contents: string;
  try {
    contents = await readFile(path, "utf8");
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") {
      throw new ConfigError(`${path}: file is required`);
    }
    throw error;
  }

  if (contents.trim().length === 0) {
    throw new ConfigError(`${path}: file is empty`);
  }

  let value: unknown;
  try {
    value = JSON.parse(contents);
  } catch (error) {
    throw new ConfigError(
      `${path}: invalid JSON: ${error instanceof Error ? error.message : String(error)}`,
    );
  }
  if (!isRecord(value)) {
    throw new ConfigError(`${path}: expected a JSON object`);
  }
  return value;
}

function rootEntries(setting: RootSettingRecord, path: string): unknown[] {
  if (setting.schema_version !== undefined && setting.schema_version !== 1) {
    throw new ConfigError(
      `${path}: unsupported schema_version ${String(setting.schema_version)}`,
    );
  }
  if (setting.session_roots !== undefined && setting.session_paths !== undefined) {
    throw new ConfigError(`${path}: use only one of session_roots or session_paths`);
  }
  const value = setting.session_roots ?? setting.session_paths;
  if (!Array.isArray(value)) {
    throw new ConfigError(`${path}: session_roots must be an array`);
  }
  return value;
}

function parseRootEntry(
  value: unknown,
  index: number,
  settingPath: string,
): {id: string; label: string; path: string} {
  const entryPath = `${settingPath}: session_roots[${index}]`;
  const raw = typeof value === "string" ? {path: value} : value;
  if (!isRecord(raw)) {
    throw new ConfigError(`${entryPath} must be a path string or object`);
  }

  const path = requiredString(raw.path, "path", entryPath);
  const defaultId = `root-${index + 1}`;
  const id = validateRootId(
    raw.id === undefined ? defaultId : requiredString(raw.id, "id", entryPath),
    entryPath,
  );
  const label = raw.label === undefined
    ? id
    : requiredString(raw.label, "label", entryPath);
  return {id, label, path};
}

async function rootsFromSetting(settingPath: string): Promise<SessionRoot[]> {
  const setting = await readRootSetting(settingPath);
  const entries = rootEntries(setting, settingPath);
  const roots: SessionRoot[] = [];
  const ids = new Set<string>();
  const directories = new Set<string>();
  for (const [index, value] of entries.entries()) {
    const entry = parseRootEntry(value, index, settingPath);
    if (ids.has(entry.id)) {
      throw new ConfigError(`${settingPath}: duplicate session root id ${entry.id}`);
    }
    const directory = resolve(dirname(settingPath), entry.path);
    if (directories.has(directory)) {
      continue;
    }
    ids.add(entry.id);
    directories.add(directory);
    roots.push({
      id: entry.id,
      label: entry.label,
      directory,
      globalJadRoot: dirname(settingPath),
    });
  }
  return roots;
}

async function findRootSettingPath(
  options: SessionRootOptions,
): Promise<string | undefined> {
  const environment = options.environment ?? process.env;
  if (options.sessionsDir !== undefined || environment.JAD_SESSIONS_DIR?.trim()) {
    return undefined;
  }

  const configuredSettingPath = options.rootSettingPath?.trim() || environment.JAD_ROOT_SETTING_PATH?.trim();
  if (configuredSettingPath) {
    return resolve(configuredSettingPath);
  }

  return firstExistingPath([
    resolve(process.cwd(), "setting.json"),
    resolve(process.cwd(), "../setting.json"),
  ]);
}

async function findRootLibrarySettingPath(
  options: SessionRootOptions,
): Promise<string | undefined> {
  const environment = options.environment ?? process.env;
  const configuredSettingPath = options.rootSettingPath?.trim() || environment.JAD_ROOT_SETTING_PATH?.trim();
  if (configuredSettingPath) {
    return resolve(configuredSettingPath);
  }
  if (options.sessionsDir !== undefined || environment.JAD_SESSIONS_DIR?.trim()) {
    return undefined;
  }
  return firstExistingPath([
    resolve(process.cwd(), "setting.json"),
    resolve(process.cwd(), "../setting.json"),
  ]);
}

function normalizeRootLibraryDirectories(
  value: unknown,
  settingPath: string,
  globalRoot: string,
): RootLibraryDirectories {
  if (value !== undefined && !isRecord(value)) {
    throw new ConfigError(`${settingPath}: libraries must be an object`);
  }
  const libraries = isRecord(value) ? value : {};
  const normalize = (key: keyof RootLibraryDirectories): string[] => {
    const configured = libraries[key] ?? DEFAULT_ROOT_LIBRARY_DIRECTORIES[key];
    if (!Array.isArray(configured)) {
      throw new ConfigError(`${settingPath}: libraries.${key} must be an array of directories`);
    }
    const directories = configured.map((entry, index) => {
      const path = requiredString(entry, `libraries.${key}[${index}]`, settingPath);
      return resolve(globalRoot, path);
    });
    return [...new Set(directories)];
  };
  return {
    prompt_dirs: normalize("prompt_dirs"),
    template_dirs: normalize("template_dirs"),
    skill_dirs: normalize("skill_dirs"),
  };
}

/**
 * Re-reads global library directories on every call so changes to setting.json
 * take effect without restarting the backend.
 */
export async function loadRootLibraryDirectories(
  options: SessionRootOptions = {},
): Promise<RootLibraryDirectories> {
  const settingPath = await findRootLibrarySettingPath(options);
  const globalRoot = settingPath === undefined ? resolve(process.cwd()) : dirname(settingPath);
  const setting = settingPath === undefined ? {} : await readRootSetting(settingPath);
  return normalizeRootLibraryDirectories(setting.libraries, settingPath ?? globalRoot, globalRoot);
}

export async function loadRootServerSettings(
  options: SessionRootOptions = {},
): Promise<RootServerSettings> {
  const settingPath = await findRootSettingPath(options);
  if (!settingPath) {
    return {...DEFAULT_ROOT_SERVER_SETTINGS};
  }
  const setting = await readRootSetting(settingPath);
  return normalizeRootServerSettings(setting.server, `${settingPath}: server`);
}

export async function loadSessionRoots(options: SessionRootOptions = {}): Promise<SessionRoot[]> {
  const environment = options.environment ?? process.env;
  if (options.sessionsDir !== undefined) {
    return [
      {
        id: DEFAULT_ROOT_ID,
        label: DEFAULT_ROOT_LABEL,
        directory: resolve(options.sessionsDir),
        globalJadRoot: resolve(options.sessionsDir),
      },
    ];
  }

  const configuredSessionsDir = environment.JAD_SESSIONS_DIR?.trim();
  if (configuredSessionsDir) {
    return [
      {
        id: DEFAULT_ROOT_ID,
        label: DEFAULT_ROOT_LABEL,
        directory: resolve(configuredSessionsDir),
        globalJadRoot: resolve(configuredSessionsDir),
      },
    ];
  }

  const settingPath = await findRootSettingPath(options);
  if (settingPath) {
    return rootsFromSetting(settingPath);
  }
  return defaultSessionRoots();
}

export async function directoryEntries(root: SessionRoot) {
  try {
    return await readdir(root.directory, {withFileTypes: true});
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") {
      return [];
    }
    throw error;
  }
}
