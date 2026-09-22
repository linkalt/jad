import { createHash } from "node:crypto";
import { readFile, stat } from "node:fs/promises";
import { basename, dirname, isAbsolute, join, relative, resolve } from "node:path";

import type { DurabilityMode, JsonRecord } from "./persistence.js";
import {
  SPILL_PLACEHOLDERS,
  resolveSpillPath,
  spillPlaceholders,
  type SpillStream,
  type SpillVars,
} from "./tools/native/spill.js";

export interface ModelConfig extends JsonRecord {
  schema_version: 1;
  provider: string;
  model: string;
  api_key_env: string;
  base_url?: string;
  request: JsonRecord;
}

export interface HttpDebugSettings extends JsonRecord {
  enabled: boolean;
  include_request_headers: boolean;
  include_request_body: boolean;
  include_response_headers: boolean;
  include_response_body: boolean;
  max_body_bytes: number;
}

export type NativeReadPathAction = "allow" | "deny";

export interface NativePathRule extends JsonRecord {
  action: NativeReadPathAction;
  path: string;
}

export type NativeReadPathRule = NativePathRule;

export interface NativePathPolicy extends JsonRecord {
  paths: NativePathRule[];
}

export interface NativeReadToolSettings extends NativePathPolicy {
  max_bytes: number;
  /** Files larger than this are refused instead of being loaded into memory. */
  max_file_bytes: number;
  default_limit: number;
}

export interface NativeGrepToolSettings extends NativePathPolicy {
  /** ripgrep executable: a PATH name or an absolute path. */
  binary: string;
  /** Total matches returned before the search is stopped. */
  max_results: number;
  /** Matches returned per file (ripgrep --max-count). */
  max_matches_per_file: number;
  /** Payload bytes returned before the search is stopped. */
  max_bytes: number;
  /** Longest rendered match line before it is cut. */
  max_line_chars: number;
  /** Files larger than this are not searched (ripgrep --max-filesize). */
  max_file_bytes: number;
  /** Wall-clock budget for one search. */
  timeout_ms: number;
}

export interface NativeGlobToolSettings extends NativePathPolicy {
  /** ripgrep executable: a PATH name or an absolute path. */
  binary: string;
  /** List hidden files; files ignored by git stay invisible either way. */
  include_hidden: boolean;
  /** Files returned before the listing is stopped. */
  max_results: number;
  /** Payload bytes returned before the listing is stopped. */
  max_bytes: number;
  /** Longest rendered path before it is cut. */
  max_line_chars: number;
  /** Wall-clock budget for one listing. */
  timeout_ms: number;
}

/** Backup and durability policy shared by every mutating native tool. */
export interface NativeMutationSettings extends JsonRecord {
  /** Replacements of one file keep at most this many backups. */
  backup_max_count: number;
  /** Kept backups of one file may use this many bytes; 0 disables the limit. */
  backup_max_total_bytes: number;
  /**
   * Directory that collects the backups of every replaced file, or null to keep
   * each backup beside its file. Always absolute once normalized: a configured
   * relative path resolves against the session cwd.
   */
  backup_dir: string | null;
  /** Sync the new file and its directory before reporting success. */
  fsync: boolean;
}

export interface NativeWriteToolSettings extends NativePathPolicy, NativeMutationSettings {
  max_bytes: number;
}

export interface NativeEditToolSettings extends NativePathPolicy, NativeMutationSettings {
  max_bytes: number;
}

/** How the bash tool signals leftover descendants when a command finishes. */
export type NativeBashProcessGroupPolicy = "always" | "timeout_only";

/** What the model sees of one stream: bytes from its start and from its end. */
export interface NativeBashStreamBudget {
  head_bytes: number;
  tail_bytes: number;
}

/** Whether a spill file survives a call whose output the model saw in full. */
export type NativeBashSpillKeep = "always" | "truncated_only";

export interface NativeBashSpillSettings {
  /** Write the complete streams to disk while they are read. */
  enabled: boolean;
  /** Keep every spill file, or only files for output the model did not see. */
  keep: NativeBashSpillKeep;
  /** Bytes written per stream before the file is left incomplete. */
  cap_bytes: number;
  /** Days a spill file survives; 0 disables pruning. */
  retention_days: number;
  /** Template for the complete stdout stream. */
  stdout_full: string;
  /** Template for the complete stderr stream. */
  stderr_full: string;
  /** Allow a location the model could not read back. */
  allow_unreadable: boolean;
}

export interface NativeBashToolSettings extends NativePathPolicy {
  /** Shell executable: a PATH name or an absolute path. */
  shell: string;
  /** Add -l so profile-provided PATH entries exist (profiles may print and cd). */
  login_shell: boolean;
  /** Wall-clock budget for one command. */
  timeout_ms: number;
  /** Upper clamp for the per-call timeout_ms argument. */
  max_timeout_ms: number;
  /** Time between SIGTERM and SIGKILL of the process group. */
  kill_grace_ms: number;
  /** Signal the process group when a command finishes, or only on timeout. */
  kill_process_group: NativeBashProcessGroupPolicy;
  /** Ceiling for the four stream budgets together. */
  max_bytes: number;
  /** What the model sees of stdout. */
  stdout: NativeBashStreamBudget;
  /** What the model sees of stderr. */
  stderr: NativeBashStreamBudget;
  /** Complete streams written to disk while they are read. */
  spill: NativeBashSpillSettings;
  /** Longest rendered output line before it is cut. */
  max_line_chars: number;
  /** Remove ANSI escape sequences and other control characters. */
  strip_ansi: boolean;
  /** Resolved dotenv file loaded before inline environment overrides. */
  env_file?: string;
  /** Environment additions or overrides for every command. */
  env: Record<string, string>;
  /** Environment variables removed before the command runs. */
  env_deny: string[];
}

export interface NativeToolSettings extends JsonRecord {
  read?: NativeReadToolSettings;
  write?: NativeWriteToolSettings;
  edit?: NativeEditToolSettings;
  grep?: NativeGrepToolSettings;
  bash?: NativeBashToolSettings;
  glob?: NativeGlobToolSettings;
}

export interface ToolSettings extends JsonRecord {
  native: NativeToolSettings;
  mcp: JsonRecord;
}

export interface SessionStoragePaths extends JsonRecord {
  messages: string;
  model: string;
  staging: string;
  http_debug: string;
}

export interface PersistenceSettings extends JsonRecord {
  durability: DurabilityMode;
  max_item_bytes: number;
}

export interface SessionSettings extends JsonRecord {
  schema_version: 1;
  session_name: string;
  cwd: string;
  system_prompt: string;
  max_tool_rounds: number;
  persistence: PersistenceSettings;
  http_debug: HttpDebugSettings;
  paths: SessionStoragePaths;
  tool_definition_paths: string[];
  tools: ToolSettings;
}

export interface SessionConfigPaths {
  global_jad_root: string;
  session_dir: string;
  model_path: string;
  setting_path: string;
  messages_path: string;
  staging_dir: string;
  http_debug_dir: string;
}

export interface SessionConfigLoaderOptions {
  globalJadRoot?: string;
}

export interface LoadedSessionConfig {
  model: ModelConfig;
  settings: SessionSettings;
  paths: SessionConfigPaths;
  model_sha256: string;
  settings_sha256: string;
}

export const DEFAULT_MAX_TOOL_ROUNDS = 8;
export const MAX_CONFIGURED_TOOL_ROUNDS = 50_000;
export const DEFAULT_PERSISTENCE_MAX_ITEM_BYTES = 10 * 1024 * 1024;
export const MAX_CONFIGURED_PERSISTENCE_ITEM_BYTES = 64 * 1024 * 1024;

export class ConfigError extends Error {
  public override name = "ConfigError";
}

function isRecord(value: unknown): value is JsonRecord {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function requiredString(value: unknown, field: string, path: string): string {
  if (typeof value !== "string" || value.trim().length === 0) {
    throw new ConfigError(`${path}: ${field} must be a non-empty string`);
  }
  return value.trim();
}

function schemaVersion(value: unknown, path: string): 1 {
  if (value !== undefined && value !== 1) {
    throw new ConfigError(`${path}: unsupported schema_version ${String(value)}`);
  }
  return 1;
}

function canonicalJson(value: unknown): string {
  if (value === null) {
    return "null";
  }
  if (typeof value === "string" || typeof value === "boolean") {
    return JSON.stringify(value);
  }
  if (typeof value === "number") {
    if (!Number.isFinite(value)) {
      throw new ConfigError("configuration contains a non-finite number");
    }
    return JSON.stringify(value);
  }
  if (Array.isArray(value)) {
    return `[${value.map((item) => canonicalJson(item)).join(",")}]`;
  }
  if (isRecord(value)) {
    const entries = Object.keys(value)
      .filter((key) => value[key] !== undefined)
      .sort()
      .map((key) => `${JSON.stringify(key)}:${canonicalJson(value[key])}`);
    return `{${entries.join(",")}}`;
  }
  throw new ConfigError("configuration contains a non-JSON value");
}

export function sha256Json(value: unknown): string {
  return createHash("sha256").update(canonicalJson(value), "utf8").digest("hex");
}

function normalizeBaseUrl(value: unknown, path: string): string | undefined {
  if (value === undefined) {
    return undefined;
  }
  const baseUrl = requiredString(value, "base_url", path).replace(/\/+$/, "");
  let parsed: URL;
  try {
    parsed = new URL(baseUrl);
  } catch {
    throw new ConfigError(`${path}: base_url must be a valid URL`);
  }
  if (parsed.protocol !== "http:" && parsed.protocol !== "https:") {
    throw new ConfigError(`${path}: base_url must use http or https`);
  }
  return baseUrl;
}

function defaultApiKeyEnv(provider: string, baseUrl: string | undefined): string {
  if (provider.toLowerCase() === "openrouter" || baseUrl?.includes("openrouter.ai")) {
    return "OPENROUTER_API_KEY";
  }
  return "OPENAI_API_KEY";
}

const ENVIRONMENT_NAME_PATTERN = /^[A-Za-z_][A-Za-z0-9_]*$/;

function normalizeModel(raw: JsonRecord, path: string): ModelConfig {
  const version = schemaVersion(raw.schema_version, path);
  const provider = requiredString(raw.provider ?? "openai", "provider", path);
  const model = requiredString(raw.model, "model", path);
  const baseUrl = normalizeBaseUrl(raw.base_url, path);
  const apiKeyEnv = requiredString(
    raw.api_key_env ?? defaultApiKeyEnv(provider, baseUrl),
    "api_key_env",
    path,
  );
  if (!ENVIRONMENT_NAME_PATTERN.test(apiKeyEnv)) {
    throw new ConfigError(`${path}: api_key_env is not a valid environment variable name`);
  }

  const request = raw.request ?? {};
  if (!isRecord(request)) {
    throw new ConfigError(`${path}: request must be a JSON object`);
  }
  for (const reserved of ["model", "input", "instructions", "stream", "tools"]) {
    if (reserved in request) {
      throw new ConfigError(`${path}: request.${reserved} is managed by the session runner`);
    }
  }

  const normalized: JsonRecord = {
    ...raw,
    schema_version: version,
    provider,
    model,
    api_key_env: apiKeyEnv,
    request,
  };
  if (baseUrl === undefined) {
    delete normalized.base_url;
  } else {
    normalized.base_url = baseUrl;
  }
  return normalized as ModelConfig;
}

async function normalizeCwd(value: unknown, path: string): Promise<string> {
  const rawCwd = requiredString(value, "cwd", path);
  if (!isAbsolute(rawCwd)) {
    throw new ConfigError(`${path}: cwd must be an absolute path`);
  }
  const cwd = resolve(rawCwd);
  let info;
  try {
    info = await stat(cwd);
  } catch {
    throw new ConfigError(`${path}: cwd does not exist: ${cwd}`);
  }
  if (!info.isDirectory()) {
    throw new ConfigError(`${path}: cwd is not a directory: ${cwd}`);
  }
  return cwd;
}

type HttpDebugBooleanField =
  | "enabled"
  | "include_request_headers"
  | "include_request_body"
  | "include_response_headers"
  | "include_response_body";

export const DEFAULT_NATIVE_READ_MAX_BYTES = 64 * 1024;
export const DEFAULT_NATIVE_READ_MAX_FILE_BYTES = 64 * 1024 * 1024;
export const DEFAULT_NATIVE_READ_LIMIT = 250;
const MAX_NATIVE_READ_FILE_BYTES = 1024 * 1024 * 1024;

export const DEFAULT_NATIVE_GREP_BINARY = "rg";
export const DEFAULT_NATIVE_GREP_MAX_RESULTS = 200;
export const DEFAULT_NATIVE_GREP_MAX_MATCHES_PER_FILE = 20;
export const DEFAULT_NATIVE_GREP_MAX_BYTES = 64 * 1024;
export const DEFAULT_NATIVE_GREP_MAX_LINE_CHARS = 240;
export const DEFAULT_NATIVE_GREP_MAX_FILE_BYTES = 8 * 1024 * 1024;
export const DEFAULT_NATIVE_GREP_TIMEOUT_MS = 10_000;

export const DEFAULT_NATIVE_GLOB_BINARY = "rg";
export const DEFAULT_NATIVE_GLOB_INCLUDE_HIDDEN = true;
export const DEFAULT_NATIVE_GLOB_MAX_RESULTS = 500;
export const DEFAULT_NATIVE_GLOB_MAX_BYTES = 64 * 1024;
export const DEFAULT_NATIVE_GLOB_MAX_LINE_CHARS = 1000;
export const DEFAULT_NATIVE_GLOB_TIMEOUT_MS = 10_000;
const MAX_NATIVE_GLOB_MAX_RESULTS = 100_000;
const MAX_NATIVE_GLOB_MAX_LINE_CHARS = 8000;
const MAX_NATIVE_GLOB_TIMEOUT_MS = 600_000;
const MAX_NATIVE_GREP_MAX_RESULTS = 10_000;
const MAX_NATIVE_GREP_MAX_MATCHES_PER_FILE = 1000;
const MAX_NATIVE_GREP_MAX_LINE_CHARS = 4000;
const MAX_NATIVE_GREP_TIMEOUT_MS = 600_000;
export const DEFAULT_NATIVE_WRITE_BACKUP_MAX_COUNT = 5;
export const DEFAULT_NATIVE_WRITE_BACKUP_MAX_TOTAL_BYTES = 10 * 1024 * 1024;
export const DEFAULT_NATIVE_WRITE_FSYNC = true;
/** Session subdirectory that collects backups when no backup_dir is configured. */
export const SESSION_BACKUP_DIRECTORY_NAME = "backups";
export const DEFAULT_NATIVE_EDIT_MAX_BYTES = 1024 * 1024;

export const DEFAULT_NATIVE_BASH_SHELL = "/bin/bash";
export const DEFAULT_NATIVE_BASH_LOGIN_SHELL = false;
export const DEFAULT_NATIVE_BASH_TIMEOUT_MS = 120_000;
export const DEFAULT_NATIVE_BASH_MAX_TIMEOUT_MS = 600_000;
/** Smallest per-call timeout_ms the model may ask for (a typo must not kill a command). */
export const MIN_NATIVE_BASH_CALL_TIMEOUT_MS = 1000;
export const DEFAULT_NATIVE_BASH_KILL_GRACE_MS = 2000;
export const DEFAULT_NATIVE_BASH_KILL_PROCESS_GROUP: NativeBashProcessGroupPolicy = "always";
export const DEFAULT_NATIVE_BASH_MAX_BYTES = 64 * 1024;
/** Failures are reported at the end of a stream, so the tail gets the larger share. */
export const DEFAULT_NATIVE_BASH_STDOUT_BUDGET: NativeBashStreamBudget = {
  head_bytes: 16 * 1024,
  tail_bytes: 32 * 1024,
};
export const DEFAULT_NATIVE_BASH_STDERR_BUDGET: NativeBashStreamBudget = {
  head_bytes: 4 * 1024,
  tail_bytes: 12 * 1024,
};
export const DEFAULT_NATIVE_BASH_SPILL_ENABLED = true;
export const DEFAULT_NATIVE_BASH_SPILL_KEEP: NativeBashSpillKeep = "truncated_only";
export const DEFAULT_NATIVE_BASH_SPILL_CAP_BYTES = 64 * 1024 * 1024;
export const DEFAULT_NATIVE_BASH_SPILL_RETENTION_DAYS = 14;
/** Complete streams land in the session cwd, the root every native tool can read. */
export const DEFAULT_NATIVE_BASH_SPILL_PATH = "{cwd}/.bash-output/bash-{call_id}.{stream}.log";
export const DEFAULT_NATIVE_BASH_MAX_LINE_CHARS = 2000;
export const DEFAULT_NATIVE_BASH_STRIP_ANSI = true;
/** Keeps colour, pagers, and credential prompts out of captured output. */
export const DEFAULT_NATIVE_BASH_ENV: Record<string, string> = {
  NO_COLOR: "1",
  TERM: "dumb",
  GIT_TERMINAL_PROMPT: "0",
  GIT_PAGER: "cat",
  PAGER: "cat",
};
const MAX_NATIVE_BASH_KILL_GRACE_MS = 60_000;
const MAX_NATIVE_BASH_LINE_CHARS = 8000;
const MAX_NATIVE_BASH_MAX_TIMEOUT_MS = 3_600_000;
const MAX_NATIVE_BASH_SPILL_CAP_BYTES = 1024 * 1024 * 1024;
const MAX_NATIVE_BASH_SPILL_RETENTION_DAYS = 3650;
const MAX_NATIVE_WRITE_BACKUP_MAX_COUNT = 1000;
const MAX_NATIVE_WRITE_BACKUP_MAX_TOTAL_BYTES = 1024 * 1024 * 1024;
const MAX_NATIVE_TOOL_BYTES = 10 * 1024 * 1024;

function normalizePersistence(
  value: unknown,
  path: string,
): PersistenceSettings {
  if (value !== undefined && !isRecord(value)) {
    throw new ConfigError(`${path}: persistence must be an object`);
  }
  const persistence = value ?? {};
  const durability = persistence.durability ?? "turn";
  if (durability !== "process" && durability !== "turn" && durability !== "strict") {
    throw new ConfigError(`${path}: persistence.durability must be process, turn, or strict`);
  }
  const maxItemBytes = persistence.max_item_bytes ?? DEFAULT_PERSISTENCE_MAX_ITEM_BYTES;
  if (
    typeof maxItemBytes !== "number" ||
    !Number.isSafeInteger(maxItemBytes) ||
    maxItemBytes < 1 ||
    maxItemBytes > MAX_CONFIGURED_PERSISTENCE_ITEM_BYTES
  ) {
    throw new ConfigError(
      `${path}: persistence.max_item_bytes must be a safe integer between 1 and ${MAX_CONFIGURED_PERSISTENCE_ITEM_BYTES}`,
    );
  }
  return {
    ...persistence,
    durability,
    max_item_bytes: maxItemBytes,
  } as PersistenceSettings;
}

function normalizeMaxToolRounds(value: unknown, path: string): number {
  const maxToolRounds = value ?? DEFAULT_MAX_TOOL_ROUNDS;
  if (
    typeof maxToolRounds !== "number" ||
    !Number.isSafeInteger(maxToolRounds) ||
    maxToolRounds < 0 ||
    maxToolRounds > MAX_CONFIGURED_TOOL_ROUNDS
  ) {
    throw new ConfigError(
      `${path}: max_tool_rounds must be a safe integer between 0 and ${MAX_CONFIGURED_TOOL_ROUNDS}`,
    );
  }
  return maxToolRounds;
}

const DEFAULT_HTTP_DEBUG: HttpDebugSettings = {
  enabled: false,
  include_request_headers: false,
  include_request_body: false,
  include_response_headers: true,
  include_response_body: false,
  max_body_bytes: 64 * 1024,
};

function normalizeHttpDebug(value: unknown, path: string): HttpDebugSettings {
  if (value === undefined) {
    return { ...DEFAULT_HTTP_DEBUG };
  }
  if (!isRecord(value)) {
    throw new ConfigError(`${path}: http_debug must be a JSON object`);
  }

  const booleanOption = (field: HttpDebugBooleanField): boolean => {
    const option = value[field];
    if (option === undefined) {
      return DEFAULT_HTTP_DEBUG[field];
    }
    if (typeof option !== "boolean") {
      throw new ConfigError(`${path}: http_debug.${field} must be a boolean`);
    }
    return option;
  };

  const maxBodyBytes = value.max_body_bytes ?? DEFAULT_HTTP_DEBUG.max_body_bytes;
  if (
    typeof maxBodyBytes !== "number" ||
    !Number.isSafeInteger(maxBodyBytes) ||
    maxBodyBytes < 1 ||
    maxBodyBytes > 10 * 1024 * 1024
  ) {
    throw new ConfigError(
      `${path}: http_debug.max_body_bytes must be a safe integer between 1 and 10485760`,
    );
  }

  return {
    ...value,
    enabled: booleanOption("enabled"),
    include_request_headers: booleanOption("include_request_headers"),
    include_request_body: booleanOption("include_request_body"),
    include_response_headers: booleanOption("include_response_headers"),
    include_response_body: booleanOption("include_response_body"),
    max_body_bytes: maxBodyBytes,
  } as HttpDebugSettings;
}

function normalizeToolDefinitionPaths(
  value: unknown,
  path: string,
  globalJadRoot: string,
): string[] {
  if (value === undefined) {
    return [];
  }
  if (!Array.isArray(value)) {
    throw new ConfigError(`${path}: tool_definition_paths must be an array`);
  }
  return value.map((item, index) => {
    const configuredPath = requiredString(
      item,
      `tool_definition_paths[${index}]`,
      path,
    );
    return isAbsolute(configuredPath)
      ? resolve(configuredPath)
      : resolve(globalJadRoot, configuredPath);
  });
}

function normalizeBashStreamBudget(
  value: unknown,
  fallback: NativeBashStreamBudget,
  path: string,
): NativeBashStreamBudget {
  if (value === undefined) {
    return {...fallback};
  }
  if (!isRecord(value)) {
    throw new ConfigError(`${path} must be an object with head_bytes and tail_bytes`);
  }
  return {
    head_bytes: normalizeByteBudget(value.head_bytes, fallback.head_bytes, `${path}.head_bytes`),
    tail_bytes: normalizeByteBudget(value.tail_bytes, fallback.tail_bytes, `${path}.tail_bytes`),
  };
}

function normalizeByteBudget(value: unknown, fallback: number, path: string): number {
  const bytes = value ?? fallback;
  if (
    typeof bytes !== "number" ||
    !Number.isSafeInteger(bytes) ||
    bytes < 0 ||
    bytes > MAX_NATIVE_TOOL_BYTES
  ) {
    throw new ConfigError(`${path} must be a safe integer between 0 and ${MAX_NATIVE_TOOL_BYTES}`);
  }
  return bytes;
}

interface BashSpillContext {
  cwd: string;
  sessionDir: string;
  readPaths: NativePathRule[];
  grepPaths: NativePathRule[];
}

/**
 * Validates the spill templates and answers the only question that matters for a
 * file the model is supposed to retrieve: could `read` or `grep` open it once the
 * call is over?
 */
function normalizeBashSpill(
  value: unknown,
  path: string,
  context: BashSpillContext,
): NativeBashSpillSettings {
  if (value !== undefined && !isRecord(value)) {
    throw new ConfigError(`${path}: tools.native.bash.spill must be an object`);
  }
  const raw = value ?? {};
  const enabled = raw.enabled ?? DEFAULT_NATIVE_BASH_SPILL_ENABLED;
  if (typeof enabled !== "boolean") {
    throw new ConfigError(`${path}: tools.native.bash.spill.enabled must be a boolean`);
  }
  const keep = raw.keep ?? DEFAULT_NATIVE_BASH_SPILL_KEEP;
  if (keep !== "always" && keep !== "truncated_only") {
    throw new ConfigError(`${path}: tools.native.bash.spill.keep must be always or truncated_only`);
  }
  const capBytes = raw.cap_bytes ?? DEFAULT_NATIVE_BASH_SPILL_CAP_BYTES;
  if (
    typeof capBytes !== "number" ||
    !Number.isSafeInteger(capBytes) ||
    capBytes < 1 ||
    capBytes > MAX_NATIVE_BASH_SPILL_CAP_BYTES
  ) {
    throw new ConfigError(
      `${path}: tools.native.bash.spill.cap_bytes must be a safe integer between 1 and ${MAX_NATIVE_BASH_SPILL_CAP_BYTES}`,
    );
  }
  const retentionDays = raw.retention_days ?? DEFAULT_NATIVE_BASH_SPILL_RETENTION_DAYS;
  if (
    typeof retentionDays !== "number" ||
    !Number.isSafeInteger(retentionDays) ||
    retentionDays < 0 ||
    retentionDays > MAX_NATIVE_BASH_SPILL_RETENTION_DAYS
  ) {
    throw new ConfigError(
      `${path}: tools.native.bash.spill.retention_days must be a safe integer between 0 and ${MAX_NATIVE_BASH_SPILL_RETENTION_DAYS}`,
    );
  }
  const allowUnreadable = raw.allow_unreadable ?? false;
  if (typeof allowUnreadable !== "boolean") {
    throw new ConfigError(`${path}: tools.native.bash.spill.allow_unreadable must be a boolean`);
  }
  const streams: SpillStream[] = ["stdout", "stderr"];
  const templates: Record<SpillStream, string> = {stdout: "", stderr: ""};
  const resolved: Record<SpillStream, string> = {stdout: "", stderr: ""};
  for (const stream of streams) {
    const field = `${path}: tools.native.bash.spill.${stream}_full`;
    const template = requiredString(
      raw[`${stream}_full`] ?? DEFAULT_NATIVE_BASH_SPILL_PATH,
      `${stream}_full`,
      `${path}: tools.native.bash.spill`,
    );
    templates[stream] = template;
    const names = spillPlaceholders(template);
    const unknown = names.filter(
      (name) => !(SPILL_PLACEHOLDERS as readonly string[]).includes(name),
    );
    if (unknown.length > 0) {
      throw new ConfigError(
        `${field} uses unknown placeholder ${unknown.map((name) => `{${name}}`).join(", ")}; supported are ${SPILL_PLACEHOLDERS.map((name) => `{${name}}`).join(", ")}`,
      );
    }
    if (!names.includes("call_id")) {
      throw new ConfigError(
        `${field} must contain {call_id}, otherwise every call overwrites the previous call's file`,
      );
    }
    resolved[stream] = resolveSpillPath(template, spillSampleVars(stream, context));
  }
  if (resolved.stdout === resolved.stderr) {
    throw new ConfigError(
      `${path}: tools.native.bash.spill.stdout_full and stderr_full resolve to the same file; give them different paths or include {stream}`,
    );
  }
  if (!allowUnreadable) {
    for (const stream of streams) {
      if (spillIsReadable(resolved[stream], context)) {
        continue;
      }
      throw new ConfigError(
        `${path}: tools.native.bash.spill.${stream}_full resolves to a file the model could not read back; keep it under the session cwd (${context.cwd}) or extend tools.native.read.paths, or set tools.native.bash.spill.allow_unreadable`,
      );
    }
  }
  return {
    enabled,
    keep,
    cap_bytes: capBytes,
    retention_days: retentionDays,
    stdout_full: templates.stdout,
    stderr_full: templates.stderr,
    allow_unreadable: allowUnreadable,
  };
}

function spillSampleVars(stream: SpillStream, context: BashSpillContext): SpillVars {
  return {
    cwd: context.cwd,
    session_dir: context.sessionDir,
    session_name: basename(context.sessionDir),
    call_id: "sample-call",
    stream,
  };
}

/** True when `read` or `grep` could open this file after the call. */
function spillIsReadable(target: string, context: BashSpillContext): boolean {
  const withinCwd = containsPath(context.cwd, target);
  return (
    readableWithRules(withinCwd, target, context.readPaths) ||
    readableWithRules(withinCwd, target, context.grepPaths)
  );
}

function readableWithRules(
  withinCwd: boolean,
  target: string,
  rules: NativePathRule[],
): boolean {
  const matches = rules.filter((rule) => containsPath(rule.path, target));
  if (matches.some((rule) => rule.action === "deny")) {
    return false;
  }
  return withinCwd || matches.some((rule) => rule.action === "allow");
}

function containsPath(root: string, target: string): boolean {
  const value = relative(root, target);
  return value === "" || (!value.startsWith("..") && !isAbsolute(value));
}

function normalizeNativePaths(
  value: unknown,
  path: string,
  globalJadRoot: string,
  toolName: "read" | "write" | "edit" | "grep" | "bash" | "glob",
): NativePathRule[] {
  if (value === undefined) {
    return [];
  }
  if (!Array.isArray(value)) {
    throw new ConfigError(`${path}: tools.native.${toolName}.paths must be an array`);
  }
  return value.map((item, index) => {
    const itemPath = `${path}: tools.native.${toolName}.paths[${index}]`;
    if (!isRecord(item)) {
      throw new ConfigError(`${itemPath} must be an object`);
    }
    const action = requiredString(item.action, "action", itemPath);
    if (action !== "allow" && action !== "deny") {
      throw new ConfigError(`${itemPath}: action must be allow or deny`);
    }
    const configuredPath = requiredString(item.path, "path", itemPath);
    return {
      ...item,
      action,
      path: isAbsolute(configuredPath)
        ? resolve(configuredPath)
        : resolve(globalJadRoot, configuredPath),
    } as NativePathRule;
  });
}

function mutationBackupMaxCount(
  value: unknown,
  path: string,
  toolName: "write" | "edit",
  fallback: number,
): number {
  const count = value ?? fallback;
  if (
    typeof count !== "number" ||
    !Number.isSafeInteger(count) ||
    count < 1 ||
    count > MAX_NATIVE_WRITE_BACKUP_MAX_COUNT
  ) {
    throw new ConfigError(
      `${path}: tools.native.${toolName}.backup_max_count must be a safe integer between 1 and ${MAX_NATIVE_WRITE_BACKUP_MAX_COUNT}`,
    );
  }
  return count;
}

function mutationBackupMaxTotalBytes(
  value: unknown,
  path: string,
  toolName: "write" | "edit",
  fallback: number,
): number {
  const bytes = value ?? fallback;
  if (
    typeof bytes !== "number" ||
    !Number.isSafeInteger(bytes) ||
    bytes < 0 ||
    bytes > MAX_NATIVE_WRITE_BACKUP_MAX_TOTAL_BYTES
  ) {
    throw new ConfigError(
      `${path}: tools.native.${toolName}.backup_max_total_bytes must be a safe integer between 0 and ${MAX_NATIVE_WRITE_BACKUP_MAX_TOTAL_BYTES} (0 disables the size limit)`,
    );
  }
  return bytes;
}

function mutationFsync(
  value: unknown,
  path: string,
  toolName: "write" | "edit",
  fallback: boolean,
): boolean {
  const fsync = value ?? fallback;
  if (typeof fsync !== "boolean") {
    throw new ConfigError(`${path}: tools.native.${toolName}.fsync must be a boolean`);
  }
  return fsync;
}

/**
 * Resolves the backup directory: a configured relative path is interpreted
 * against the session cwd (the workspace the tools see), an absolute path is
 * used as given, and null keeps every backup beside its own file.
 */
function mutationBackupDir(
  value: unknown,
  path: string,
  toolName: "write" | "edit",
  fallback: string | null,
  cwd: string,
): string | null {
  if (value === undefined) {
    return fallback;
  }
  if (value === null) {
    return null;
  }
  if (typeof value !== "string") {
    throw new ConfigError(
      `${path}: tools.native.${toolName}.backup_dir must be a path string, or null to keep each backup beside its file`,
    );
  }
  const configured = value.trim();
  if (configured.length === 0) {
    throw new ConfigError(
      `${path}: tools.native.${toolName}.backup_dir must not be empty`,
    );
  }
  return isAbsolute(configured) ? resolve(configured) : resolve(cwd, configured);
}

function parseBashEnvValue(value: string, path: string, lineNumber: number): string {
  const trimmed = value.trimStart();
  if (trimmed.length === 0) {
    return "";
  }
  const quote = trimmed[0];
  if (quote !== "\"" && quote !== "'") {
    return trimmed.replace(/\s+#.*$/, "").trimEnd();
  }

  let escaped = false;
  let close = -1;
  for (let index = 1; index < trimmed.length; index += 1) {
    const character = trimmed[index]!;
    if (quote === "\"" && character === "\\" && !escaped) {
      escaped = true;
      continue;
    }
    if (character === quote && !escaped) {
      close = index;
      break;
    }
    escaped = false;
  }
  if (close < 0) {
    throw new ConfigError(`${path}:${lineNumber}: unterminated quoted value`);
  }
  const suffix = trimmed.slice(close + 1).trimStart();
  if (suffix.length > 0 && !suffix.startsWith("#")) {
    throw new ConfigError(`${path}:${lineNumber}: unexpected content after quoted value`);
  }
  const quoted = trimmed.slice(1, close);
  if (quote === "'") {
    return quoted;
  }
  return quoted.replace(/\\([\\"nrt])/g, (_match, escapedCharacter: string) => {
    switch (escapedCharacter) {
      case "n": return "\n";
      case "r": return "\r";
      case "t": return "\t";
      default: return escapedCharacter;
    }
  });
}

function parseBashEnvFile(contents: string, path: string): Record<string, string> {
  const env: Record<string, string> = {};
  for (const [index, rawLine] of contents.replace(/^\uFEFF/, "").split(/\r?\n/).entries()) {
    const lineNumber = index + 1;
    const line = rawLine.trimStart();
    if (line.length === 0 || line.startsWith("#")) {
      continue;
    }
    const assignment = /^(?:export[ \t]+)?([A-Za-z_][A-Za-z0-9_]*)[ \t]*=(.*)$/.exec(line);
    if (!assignment) {
      throw new ConfigError(`${path}:${lineNumber}: expected NAME=value`);
    }
    env[assignment[1]!] = parseBashEnvValue(assignment[2] ?? "", path, lineNumber);
  }
  return env;
}

async function loadBashEnvFile(envFile: string, settingPath: string): Promise<Record<string, string>> {
  let contents: string;
  try {
    contents = await readFile(envFile, "utf8");
  } catch (error) {
    const message = error instanceof Error ? error.message : String(error);
    throw new ConfigError(`${settingPath}: could not read ${envFile}: ${message}`);
  }
  return parseBashEnvFile(contents, envFile);
}

async function normalizeTools(
  value: unknown,
  path: string,
  globalJadRoot: string,
  sessionDir: string,
  cwd: string,
): Promise<ToolSettings> {
  const defaultBackupDir = join(sessionDir, SESSION_BACKUP_DIRECTORY_NAME);
  if (value !== undefined && !isRecord(value)) {
    throw new ConfigError(`${path}: tools must be an object`);
  }
  const tools = value ?? {};
  const nativeValue = (tools as JsonRecord).native ?? {};
  if (!isRecord(nativeValue)) {
    throw new ConfigError(`${path}: tools.native must be an object`);
  }

  const readValue = nativeValue.read;
  let read: NativeReadToolSettings | undefined;
  if (readValue !== undefined) {
    if (!isRecord(readValue)) {
      throw new ConfigError(`${path}: tools.native.read must be an object`);
    }
    const maxBytes = readValue.max_bytes ?? DEFAULT_NATIVE_READ_MAX_BYTES;
    if (
      typeof maxBytes !== "number" ||
      !Number.isSafeInteger(maxBytes) ||
      maxBytes < 1 ||
      maxBytes > MAX_NATIVE_TOOL_BYTES
    ) {
      throw new ConfigError(
        `${path}: tools.native.read.max_bytes must be a safe integer between 1 and ${MAX_NATIVE_TOOL_BYTES}`,
      );
    }
    const defaultLimit = readValue.default_limit ?? DEFAULT_NATIVE_READ_LIMIT;
    if (
      typeof defaultLimit !== "number" ||
      !Number.isSafeInteger(defaultLimit) ||
      defaultLimit < 1
    ) {
      throw new ConfigError(
        `${path}: tools.native.read.default_limit must be a positive safe integer`,
      );
    }
    const maxFileBytes = readValue.max_file_bytes ?? DEFAULT_NATIVE_READ_MAX_FILE_BYTES;
    if (
      typeof maxFileBytes !== "number" ||
      !Number.isSafeInteger(maxFileBytes) ||
      maxFileBytes < 1 ||
      maxFileBytes > MAX_NATIVE_READ_FILE_BYTES
    ) {
      throw new ConfigError(
        `${path}: tools.native.read.max_file_bytes must be a safe integer between 1 and ${MAX_NATIVE_READ_FILE_BYTES}`,
      );
    }
    read = {
      ...readValue,
      max_bytes: maxBytes,
      max_file_bytes: maxFileBytes,
      default_limit: defaultLimit,
      paths: normalizeNativePaths(readValue.paths, path, globalJadRoot, "read"),
    };
  }

  const grepValue = nativeValue.grep;
  let grep: NativeGrepToolSettings | undefined;
  if (grepValue !== undefined) {
    if (!isRecord(grepValue)) {
      throw new ConfigError(`${path}: tools.native.grep must be an object`);
    }
    const binary = grepValue.binary ?? DEFAULT_NATIVE_GREP_BINARY;
    if (typeof binary !== "string" || binary.trim().length === 0) {
      throw new ConfigError(`${path}: tools.native.grep.binary must be a non-empty string`);
    }
    const maxResults = grepValue.max_results ?? DEFAULT_NATIVE_GREP_MAX_RESULTS;
    if (
      typeof maxResults !== "number" ||
      !Number.isSafeInteger(maxResults) ||
      maxResults < 1 ||
      maxResults > MAX_NATIVE_GREP_MAX_RESULTS
    ) {
      throw new ConfigError(
        `${path}: tools.native.grep.max_results must be a safe integer between 1 and ${MAX_NATIVE_GREP_MAX_RESULTS}`,
      );
    }
    const maxPerFile =
      grepValue.max_matches_per_file ?? DEFAULT_NATIVE_GREP_MAX_MATCHES_PER_FILE;
    if (
      typeof maxPerFile !== "number" ||
      !Number.isSafeInteger(maxPerFile) ||
      maxPerFile < 1 ||
      maxPerFile > MAX_NATIVE_GREP_MAX_MATCHES_PER_FILE
    ) {
      throw new ConfigError(
        `${path}: tools.native.grep.max_matches_per_file must be a safe integer between 1 and ${MAX_NATIVE_GREP_MAX_MATCHES_PER_FILE}`,
      );
    }
    const grepMaxBytes = grepValue.max_bytes ?? DEFAULT_NATIVE_GREP_MAX_BYTES;
    if (
      typeof grepMaxBytes !== "number" ||
      !Number.isSafeInteger(grepMaxBytes) ||
      grepMaxBytes < 1 ||
      grepMaxBytes > MAX_NATIVE_TOOL_BYTES
    ) {
      throw new ConfigError(
        `${path}: tools.native.grep.max_bytes must be a safe integer between 1 and ${MAX_NATIVE_TOOL_BYTES}`,
      );
    }
    const maxLineChars = grepValue.max_line_chars ?? DEFAULT_NATIVE_GREP_MAX_LINE_CHARS;
    if (
      typeof maxLineChars !== "number" ||
      !Number.isSafeInteger(maxLineChars) ||
      maxLineChars < 20 ||
      maxLineChars > MAX_NATIVE_GREP_MAX_LINE_CHARS
    ) {
      throw new ConfigError(
        `${path}: tools.native.grep.max_line_chars must be a safe integer between 20 and ${MAX_NATIVE_GREP_MAX_LINE_CHARS}`,
      );
    }
    const grepMaxFileBytes = grepValue.max_file_bytes ?? DEFAULT_NATIVE_GREP_MAX_FILE_BYTES;
    if (
      typeof grepMaxFileBytes !== "number" ||
      !Number.isSafeInteger(grepMaxFileBytes) ||
      grepMaxFileBytes < 1 ||
      grepMaxFileBytes > MAX_NATIVE_READ_FILE_BYTES
    ) {
      throw new ConfigError(
        `${path}: tools.native.grep.max_file_bytes must be a safe integer between 1 and ${MAX_NATIVE_READ_FILE_BYTES}`,
      );
    }
    const timeoutMs = grepValue.timeout_ms ?? DEFAULT_NATIVE_GREP_TIMEOUT_MS;
    if (
      typeof timeoutMs !== "number" ||
      !Number.isSafeInteger(timeoutMs) ||
      timeoutMs < 1 ||
      timeoutMs > MAX_NATIVE_GREP_TIMEOUT_MS
    ) {
      throw new ConfigError(
        `${path}: tools.native.grep.timeout_ms must be a safe integer between 1 and ${MAX_NATIVE_GREP_TIMEOUT_MS}`,
      );
    }
    // Searching reads the same files as `read`, so the read path policy applies
    // unless the session overrides it for this tool.
    grep = {
      ...grepValue,
      binary: binary.trim(),
      max_results: maxResults,
      max_matches_per_file: maxPerFile,
      max_bytes: grepMaxBytes,
      max_line_chars: maxLineChars,
      max_file_bytes: grepMaxFileBytes,
      timeout_ms: timeoutMs,
      paths:
        grepValue.paths === undefined
          ? (read?.paths ?? [])
          : normalizeNativePaths(grepValue.paths, path, globalJadRoot, "grep"),
    };
  }

  const writeValue = nativeValue.write;
  let write: NativeWriteToolSettings | undefined;
  if (writeValue !== undefined) {
    if (!isRecord(writeValue)) {
      throw new ConfigError(`${path}: tools.native.write must be an object`);
    }
    const maxBytes = writeValue.max_bytes ?? MAX_NATIVE_TOOL_BYTES;
    if (
      typeof maxBytes !== "number" ||
      !Number.isSafeInteger(maxBytes) ||
      maxBytes < 1 ||
      maxBytes > MAX_NATIVE_TOOL_BYTES
    ) {
      throw new ConfigError(
        `${path}: tools.native.write.max_bytes must be a safe integer between 1 and ${MAX_NATIVE_TOOL_BYTES}`,
      );
    }
    write = {
      ...writeValue,
      max_bytes: maxBytes,
      backup_max_count: mutationBackupMaxCount(
        writeValue.backup_max_count,
        path,
        "write",
        DEFAULT_NATIVE_WRITE_BACKUP_MAX_COUNT,
      ),
      backup_max_total_bytes: mutationBackupMaxTotalBytes(
        writeValue.backup_max_total_bytes,
        path,
        "write",
        DEFAULT_NATIVE_WRITE_BACKUP_MAX_TOTAL_BYTES,
      ),
      backup_dir: mutationBackupDir(
        writeValue.backup_dir,
        path,
        "write",
        defaultBackupDir,
        cwd,
      ),
      fsync: mutationFsync(writeValue.fsync, path, "write", DEFAULT_NATIVE_WRITE_FSYNC),
      paths: normalizeNativePaths(writeValue.paths, path, globalJadRoot, "write"),
    };
  }

  const editValue = nativeValue.edit;
  let edit: NativeEditToolSettings | undefined;
  if (editValue !== undefined) {
    if (!isRecord(editValue)) {
      throw new ConfigError(`${path}: tools.native.edit must be an object`);
    }
    const maxBytes = editValue.max_bytes ?? DEFAULT_NATIVE_EDIT_MAX_BYTES;
    if (
      typeof maxBytes !== "number" ||
      !Number.isSafeInteger(maxBytes) ||
      maxBytes < 1 ||
      maxBytes > MAX_NATIVE_TOOL_BYTES
    ) {
      throw new ConfigError(
        `${path}: tools.native.edit.max_bytes must be a safe integer between 1 and ${MAX_NATIVE_TOOL_BYTES}`,
      );
    }
    // Edits mutate the same files as writes, so the write path policy and the
    // write backup/durability settings apply unless the session overrides them.
    const mutation: NativeMutationSettings = write ?? {
      backup_max_count: DEFAULT_NATIVE_WRITE_BACKUP_MAX_COUNT,
      backup_max_total_bytes: DEFAULT_NATIVE_WRITE_BACKUP_MAX_TOTAL_BYTES,
      backup_dir: defaultBackupDir,
      fsync: DEFAULT_NATIVE_WRITE_FSYNC,
    };
    edit = {
      ...editValue,
      max_bytes: maxBytes,
      backup_max_count: mutationBackupMaxCount(
        editValue.backup_max_count,
        path,
        "edit",
        mutation.backup_max_count,
      ),
      backup_max_total_bytes: mutationBackupMaxTotalBytes(
        editValue.backup_max_total_bytes,
        path,
        "edit",
        mutation.backup_max_total_bytes,
      ),
      backup_dir: mutationBackupDir(
        editValue.backup_dir,
        path,
        "edit",
        mutation.backup_dir,
        cwd,
      ),
      fsync: mutationFsync(editValue.fsync, path, "edit", mutation.fsync),
      paths:
        editValue.paths === undefined
          ? (write?.paths ?? [])
          : normalizeNativePaths(editValue.paths, path, globalJadRoot, "edit"),
    };
  }

  const bashValue = nativeValue.bash;
  let bash: NativeBashToolSettings | undefined;
  if (bashValue !== undefined) {
    if (!isRecord(bashValue)) {
      throw new ConfigError(`${path}: tools.native.bash must be an object`);
    }
    const shell = bashValue.shell ?? DEFAULT_NATIVE_BASH_SHELL;
    if (typeof shell !== "string" || shell.trim().length === 0) {
      throw new ConfigError(`${path}: tools.native.bash.shell must be a non-empty string`);
    }
    const loginShell = bashValue.login_shell ?? DEFAULT_NATIVE_BASH_LOGIN_SHELL;
    if (typeof loginShell !== "boolean") {
      throw new ConfigError(`${path}: tools.native.bash.login_shell must be a boolean`);
    }
    const timeoutMs = bashValue.timeout_ms ?? DEFAULT_NATIVE_BASH_TIMEOUT_MS;
    if (
      typeof timeoutMs !== "number" ||
      !Number.isSafeInteger(timeoutMs) ||
      timeoutMs < 1 ||
      timeoutMs > MAX_NATIVE_BASH_MAX_TIMEOUT_MS
    ) {
      throw new ConfigError(
        `${path}: tools.native.bash.timeout_ms must be a safe integer between 1 and ${MAX_NATIVE_BASH_MAX_TIMEOUT_MS}`,
      );
    }
    const maxTimeoutMs = bashValue.max_timeout_ms ?? DEFAULT_NATIVE_BASH_MAX_TIMEOUT_MS;
    if (
      typeof maxTimeoutMs !== "number" ||
      !Number.isSafeInteger(maxTimeoutMs) ||
      maxTimeoutMs < 1 ||
      maxTimeoutMs > MAX_NATIVE_BASH_MAX_TIMEOUT_MS
    ) {
      throw new ConfigError(
        `${path}: tools.native.bash.max_timeout_ms must be a safe integer between 1 and ${MAX_NATIVE_BASH_MAX_TIMEOUT_MS}`,
      );
    }
    if (maxTimeoutMs < timeoutMs) {
      throw new ConfigError(
        `${path}: tools.native.bash.max_timeout_ms must be greater than or equal to tools.native.bash.timeout_ms`,
      );
    }
    const killGraceMs = bashValue.kill_grace_ms ?? DEFAULT_NATIVE_BASH_KILL_GRACE_MS;
    if (
      typeof killGraceMs !== "number" ||
      !Number.isSafeInteger(killGraceMs) ||
      killGraceMs < 0 ||
      killGraceMs > MAX_NATIVE_BASH_KILL_GRACE_MS
    ) {
      throw new ConfigError(
        `${path}: tools.native.bash.kill_grace_ms must be a safe integer between 0 and ${MAX_NATIVE_BASH_KILL_GRACE_MS}`,
      );
    }
    const killProcessGroup =
      bashValue.kill_process_group ?? DEFAULT_NATIVE_BASH_KILL_PROCESS_GROUP;
    if (killProcessGroup !== "always" && killProcessGroup !== "timeout_only") {
      throw new ConfigError(
        `${path}: tools.native.bash.kill_process_group must be always or timeout_only`,
      );
    }
    const bashMaxBytes = bashValue.max_bytes ?? DEFAULT_NATIVE_BASH_MAX_BYTES;
    if (
      typeof bashMaxBytes !== "number" ||
      !Number.isSafeInteger(bashMaxBytes) ||
      bashMaxBytes < 1 ||
      bashMaxBytes > MAX_NATIVE_TOOL_BYTES
    ) {
      throw new ConfigError(
        `${path}: tools.native.bash.max_bytes must be a safe integer between 1 and ${MAX_NATIVE_TOOL_BYTES}`,
      );
    }
    if (bashValue.capture_bytes !== undefined) {
      throw new ConfigError(
        `${path}: tools.native.bash.capture_bytes was removed; set tools.native.bash.stdout and tools.native.bash.stderr with head_bytes and tail_bytes instead`,
      );
    }
    if (bashValue.head_ratio !== undefined) {
      throw new ConfigError(
        `${path}: tools.native.bash.head_ratio was removed; each stream now has its own head_bytes and tail_bytes`,
      );
    }
    const stdoutBudget = normalizeBashStreamBudget(
      bashValue.stdout,
      DEFAULT_NATIVE_BASH_STDOUT_BUDGET,
      `${path}: tools.native.bash.stdout`,
    );
    const stderrBudget = normalizeBashStreamBudget(
      bashValue.stderr,
      DEFAULT_NATIVE_BASH_STDERR_BUDGET,
      `${path}: tools.native.bash.stderr`,
    );
    const streamBytes =
      stdoutBudget.head_bytes +
      stdoutBudget.tail_bytes +
      stderrBudget.head_bytes +
      stderrBudget.tail_bytes;
    if (streamBytes > bashMaxBytes) {
      throw new ConfigError(
        `${path}: tools.native.bash stream budgets total ${streamBytes} bytes, more than tools.native.bash.max_bytes (${bashMaxBytes}); raise max_bytes or lower the head_bytes and tail_bytes values`,
      );
    }
    const spill = normalizeBashSpill(bashValue.spill, path, {
      cwd,
      sessionDir,
      readPaths: read?.paths ?? [],
      grepPaths: grep?.paths ?? [],
    });
    const maxLineChars = bashValue.max_line_chars ?? DEFAULT_NATIVE_BASH_MAX_LINE_CHARS;
    if (
      typeof maxLineChars !== "number" ||
      !Number.isSafeInteger(maxLineChars) ||
      maxLineChars < 20 ||
      maxLineChars > MAX_NATIVE_BASH_LINE_CHARS
    ) {
      throw new ConfigError(
        `${path}: tools.native.bash.max_line_chars must be a safe integer between 20 and ${MAX_NATIVE_BASH_LINE_CHARS}`,
      );
    }
    const stripAnsi = bashValue.strip_ansi ?? DEFAULT_NATIVE_BASH_STRIP_ANSI;
    if (typeof stripAnsi !== "boolean") {
      throw new ConfigError(`${path}: tools.native.bash.strip_ansi must be a boolean`);
    }
    const envFileValue = bashValue.env_file;
    let envFile: string | undefined;
    let fileEnv: Record<string, string> = {};
    if (envFileValue !== undefined) {
      const configuredEnvFile = requiredString(
        envFileValue,
        "env_file",
        `${path}: tools.native.bash`,
      );
      envFile = resolve(sessionDir, configuredEnvFile);
      fileEnv = await loadBashEnvFile(envFile, `${path}: tools.native.bash.env_file`);
    }
    const envValue = bashValue.env ?? {};
    if (!isRecord(envValue)) {
      throw new ConfigError(`${path}: tools.native.bash.env must be an object`);
    }
    const env: Record<string, string> = {...DEFAULT_NATIVE_BASH_ENV, ...fileEnv};
    for (const [name, configured] of Object.entries(envValue)) {
      if (!ENVIRONMENT_NAME_PATTERN.test(name)) {
        throw new ConfigError(`${path}: tools.native.bash.env.${name} is not a valid environment variable name`);
      }
      if (typeof configured !== "string") {
        throw new ConfigError(`${path}: tools.native.bash.env.${name} must be a string`);
      }
      env[name] = configured;
    }
    const envDenyValue = bashValue.env_deny ?? [];
    if (!Array.isArray(envDenyValue)) {
      throw new ConfigError(`${path}: tools.native.bash.env_deny must be an array`);
    }
    const envDeny = envDenyValue.map((item, index) => {
      const name = requiredString(item, "name", `${path}: tools.native.bash.env_deny[${index}]`);
      if (!ENVIRONMENT_NAME_PATTERN.test(name)) {
        throw new ConfigError(
          `${path}: tools.native.bash.env_deny[${index}] must be an environment variable name`,
        );
      }
      return name;
    });
    bash = {
      ...bashValue,
      shell,
      login_shell: loginShell,
      timeout_ms: timeoutMs,
      max_timeout_ms: maxTimeoutMs,
      kill_grace_ms: killGraceMs,
      kill_process_group: killProcessGroup,
      max_bytes: bashMaxBytes,
      stdout: stdoutBudget,
      stderr: stderrBudget,
      spill,
      max_line_chars: maxLineChars,
      strip_ansi: stripAnsi,
      ...(envFile ? {env_file: envFile} : {}),
      env,
      env_deny: envDeny,
      // Commands mutate the workspace like `write` does, so the write path
      // policy applies to the working directory unless the session overrides it.
      paths:
        bashValue.paths === undefined
          ? (write?.paths ?? [])
          : normalizeNativePaths(bashValue.paths, path, globalJadRoot, "bash"),
    };
  }

  const globValue = nativeValue.glob;
  let glob: NativeGlobToolSettings | undefined;
  if (globValue !== undefined) {
    if (!isRecord(globValue)) {
      throw new ConfigError(`${path}: tools.native.glob must be an object`);
    }
    const globBinary = globValue.binary ?? DEFAULT_NATIVE_GLOB_BINARY;
    if (typeof globBinary !== "string" || globBinary.trim().length === 0) {
      throw new ConfigError(`${path}: tools.native.glob.binary must be a non-empty string`);
    }
    const includeHidden = globValue.include_hidden ?? DEFAULT_NATIVE_GLOB_INCLUDE_HIDDEN;
    if (typeof includeHidden !== "boolean") {
      throw new ConfigError(`${path}: tools.native.glob.include_hidden must be a boolean`);
    }
    const globMaxResults = globValue.max_results ?? DEFAULT_NATIVE_GLOB_MAX_RESULTS;
    if (
      typeof globMaxResults !== "number" ||
      !Number.isSafeInteger(globMaxResults) ||
      globMaxResults < 1 ||
      globMaxResults > MAX_NATIVE_GLOB_MAX_RESULTS
    ) {
      throw new ConfigError(
        `${path}: tools.native.glob.max_results must be a safe integer between 1 and ${MAX_NATIVE_GLOB_MAX_RESULTS}`,
      );
    }
    const globMaxBytes = globValue.max_bytes ?? DEFAULT_NATIVE_GLOB_MAX_BYTES;
    if (
      typeof globMaxBytes !== "number" ||
      !Number.isSafeInteger(globMaxBytes) ||
      globMaxBytes < 1 ||
      globMaxBytes > MAX_NATIVE_TOOL_BYTES
    ) {
      throw new ConfigError(
        `${path}: tools.native.glob.max_bytes must be a safe integer between 1 and ${MAX_NATIVE_TOOL_BYTES}`,
      );
    }
    const globMaxLineChars = globValue.max_line_chars ?? DEFAULT_NATIVE_GLOB_MAX_LINE_CHARS;
    if (
      typeof globMaxLineChars !== "number" ||
      !Number.isSafeInteger(globMaxLineChars) ||
      globMaxLineChars < 20 ||
      globMaxLineChars > MAX_NATIVE_GLOB_MAX_LINE_CHARS
    ) {
      throw new ConfigError(
        `${path}: tools.native.glob.max_line_chars must be a safe integer between 20 and ${MAX_NATIVE_GLOB_MAX_LINE_CHARS}`,
      );
    }
    const globTimeoutMs = globValue.timeout_ms ?? DEFAULT_NATIVE_GLOB_TIMEOUT_MS;
    if (
      typeof globTimeoutMs !== "number" ||
      !Number.isSafeInteger(globTimeoutMs) ||
      globTimeoutMs < 1 ||
      globTimeoutMs > MAX_NATIVE_GLOB_TIMEOUT_MS
    ) {
      throw new ConfigError(
        `${path}: tools.native.glob.timeout_ms must be a safe integer between 1 and ${MAX_NATIVE_GLOB_TIMEOUT_MS}`,
      );
    }
    glob = {
      ...globValue,
      binary: globBinary,
      include_hidden: includeHidden,
      max_results: globMaxResults,
      max_bytes: globMaxBytes,
      max_line_chars: globMaxLineChars,
      timeout_ms: globTimeoutMs,
      // Listing files only reads the tree, so the read path policy applies.
      paths:
        globValue.paths === undefined
          ? (read?.paths ?? [])
          : normalizeNativePaths(globValue.paths, path, globalJadRoot, "glob"),
    };
  }

  const mcpValue = (tools as JsonRecord).mcp ?? {};
  if (!isRecord(mcpValue)) {
    throw new ConfigError(`${path}: tools.mcp must be an object`);
  }
  return {
    ...(tools as JsonRecord),
    native: {
      ...nativeValue,
      ...(read ? {read} : {}),
      ...(write ? {write} : {}),
      ...(edit ? {edit} : {}),
      ...(grep ? {grep} : {}),
      ...(bash ? {bash} : {}),
      ...(glob ? {glob} : {}),
    },
    mcp: mcpValue,
  } as ToolSettings;
}

function normalizeStoragePaths(
  value: unknown,
  path: string,
  settingPath: string,
): SessionStoragePaths {
  if (value !== undefined && !isRecord(value)) {
    throw new ConfigError(`${path}: paths must be a JSON object`);
  }
  const paths = value ?? {};
  const pathValue = (field: string, fallback: string): string => {
    const configured = (paths as JsonRecord)[field] ?? fallback;
    return resolve(
      dirname(settingPath),
      requiredString(configured, field, `${path}: paths`),
    );
  };

  return {
    ...(paths as JsonRecord),
    messages: pathValue("messages", "messages.jsonl"),
    model: pathValue("model", "model.json"),
    staging: pathValue("staging", "staging"),
    http_debug: pathValue("http_debug", "debug/http"),
  } as SessionStoragePaths;
}

async function normalizeSettings(
  raw: JsonRecord,
  path: string,
  sessionDir: string,
  globalJadRoot: string,
  storagePaths: SessionStoragePaths,
): Promise<SessionSettings> {
  const version = schemaVersion(raw.schema_version, path);
  const sessionName = requiredString(
    raw.session_name ?? basename(resolve(sessionDir)),
    "session_name",
    path,
  );
  const cwd = await normalizeCwd(raw.cwd ?? globalJadRoot, path);
  const systemPrompt = raw.system_prompt ?? "";
  if (typeof systemPrompt !== "string") {
    throw new ConfigError(`${path}: system_prompt must be a string`);
  }
  const maxToolRounds = normalizeMaxToolRounds(raw.max_tool_rounds, path);
  const persistence = normalizePersistence(raw.persistence, path);
  const httpDebug = normalizeHttpDebug(raw.http_debug, path);
  const toolDefinitionPaths = normalizeToolDefinitionPaths(
    raw.tool_definition_paths,
    path,
    globalJadRoot,
  );
  const tools = await normalizeTools(raw.tools, path, globalJadRoot, sessionDir, cwd);

  return {
    ...raw,
    schema_version: version,
    session_name: sessionName,
    cwd,
    system_prompt: systemPrompt,
    max_tool_rounds: maxToolRounds,
    persistence,
    http_debug: httpDebug,
    paths: storagePaths,
    tool_definition_paths: toolDefinitionPaths,
    tools,
  } as SessionSettings;
}

async function readObject(path: string, allowEmpty: boolean): Promise<JsonRecord | undefined> {
  let contents: string;
  try {
    contents = await readFile(path, "utf8");
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") {
      if (allowEmpty) {
        return undefined;
      }
      throw new ConfigError(`${path}: file is required`);
    }
    throw error;
  }

  if (contents.trim().length === 0) {
    if (allowEmpty) {
      return undefined;
    }
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

export function resolveApiKey(
  model: Pick<ModelConfig, "api_key_env">,
  environment: NodeJS.ProcessEnv = process.env,
): string {
  const value = environment[model.api_key_env];
  if (typeof value !== "string" || value.length === 0) {
    throw new ConfigError(`environment variable ${model.api_key_env} is not set`);
  }
  return value;
}

export class SessionConfigLoader {
  public readonly paths: SessionConfigPaths;

  public constructor(
    sessionDir: string,
    options: SessionConfigLoaderOptions = {},
  ) {
    const absoluteSessionDir = resolve(sessionDir);
    const globalJadRoot = resolve(options.globalJadRoot ?? process.cwd());
    this.paths = {
      global_jad_root: globalJadRoot,
      session_dir: absoluteSessionDir,
      model_path: join(absoluteSessionDir, "model.json"),
      setting_path: join(absoluteSessionDir, "setting.json"),
      messages_path: join(absoluteSessionDir, "messages.jsonl"),
      staging_dir: join(absoluteSessionDir, "staging"),
      http_debug_dir: join(absoluteSessionDir, "debug", "http"),
    };
  }

  private async loadModelAtResolvedPath(): Promise<ModelConfig> {
    const raw = await readObject(this.paths.model_path, false);
    return normalizeModel(raw!, this.paths.model_path);
  }

  public async loadModel(): Promise<ModelConfig> {
    await this.loadSettings();
    return this.loadModelAtResolvedPath();
  }

  public async loadSettings(): Promise<SessionSettings> {
    const raw = (await readObject(this.paths.setting_path, true)) ?? {};
    const storagePaths = normalizeStoragePaths(
      raw.paths,
      this.paths.setting_path,
      this.paths.setting_path,
    );
    this.paths.model_path = storagePaths.model;
    this.paths.messages_path = storagePaths.messages;
    this.paths.staging_dir = storagePaths.staging;
    this.paths.http_debug_dir = storagePaths.http_debug;
    return normalizeSettings(
      raw,
      this.paths.setting_path,
      this.paths.session_dir,
      this.paths.global_jad_root,
      storagePaths,
    );
  }

  public async load(): Promise<LoadedSessionConfig> {
    const settings = await this.loadSettings();
    const model = await this.loadModelAtResolvedPath();
    return {
      model,
      settings,
      paths: this.paths,
      model_sha256: sha256Json(model),
      settings_sha256: sha256Json(settings),
    };
  }
}
