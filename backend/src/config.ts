import { createHash } from "node:crypto";
import { readFile, stat } from "node:fs/promises";
import { homedir } from "node:os";
import { basename, dirname, isAbsolute, join, relative, resolve } from "node:path";

import {CURSOR_CACHE_FILE, HISTORY_CACHE_FILE, USAGE_CACHE_FILE} from "./persistence.js";
import type {DurabilityMode, JsonRecord, ResponseMetadataMode} from "./persistence.js";
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
  /**
   * True when the tool may reach outside cwd without an allow rule. The `paths`
   * object form sets it whenever its allow list is empty; the rule array form
   * keeps the workspace-only default.
   */
  unrestricted_paths: boolean;
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
  /** Resolved output directory, absent when deprecated legacy templates are active. */
  storage_dir?: string;
  /** Deprecated per-stream path templates, retained for existing settings. */
  stdout_full?: string;
  stderr_full?: string;
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

export type NativeTaskStateContextStatus = "todo" | "in-progress" | "blocked" | "done";
export interface NativeTaskStateContextSettings extends JsonRecord {
  statuses: NativeTaskStateContextStatus[];
  limit: number;
}

export interface NativeProcessStateContextSettings extends JsonRecord {
  states: Array<"running" | "exited" | "stopped" | "failed" | "lost">;
  limit: number;
}

export interface NativeMemoryStateContextSelector extends JsonRecord {
  type: string;
  filter: JsonRecord;
  limit: number;
}

export interface NativeTaskToolSettings extends JsonRecord {
  max_tasks: number;
  list_limit: number;
  max_list_limit: number;
  max_title_chars: number;
  max_tags: number;
  max_tag_chars: number;
  max_documents: number;
  max_document_chars: number;
  max_storage_bytes: number;
  max_list_bytes: number;
  /** Resolved task-plan directory, relative to cwd when configured relatively. */
  storage_dir: string;
  statecontext: NativeTaskStateContextSettings;
}

export interface NativeFetchToolSettings extends JsonRecord {
  storage_dir: string;
  default_limit: number;
  max_limit: number;
  max_response_bytes: number;
  max_document_chars: number;
  max_storage_bytes: number;
  timeout_ms: number;
  max_redirects: number;
  allowed_hosts: string[];
  denied_hosts: string[];
}

export interface NativeMemoryToolSettings extends JsonRecord {
  /** Resolved memory-root directory, relative to cwd when configured relatively. */
  storage_dir: string;
  statecontext: NativeMemoryStateContextSelector[];
  max_types: number;
  max_records_per_type: number;
  max_total_records: number;
  max_record_bytes: number;
  max_total_bytes: number;
  list_limit: number;
  max_list_limit: number;
  max_list_bytes: number;
  max_scan_records: number;
  max_scan_bytes: number;
  max_filter_fields: number;
  max_filter_depth: number;
  max_json_depth: number;
  max_cursors: number;
  cursor_ttl_ms: number;
  max_cursor_bytes: number;
  allowed_types?: string[];
}

export interface NativeProcessToolSettings extends NativePathPolicy {
  shell: string;
  login_shell: boolean;
  kill_grace_ms: number;
  max_running: number;
  max_completed: number;
  retention_days: number;
  wait_timeout_ms: number;
  max_wait_timeout_ms: number;
  /** Bounded stdout excerpt returned by status-like calls. */
  stdout: NativeBashStreamBudget;
  /** Bounded stderr excerpt returned by status-like calls. */
  stderr: NativeBashStreamBudget;
  log_cap_bytes: number;
  max_line_chars: number;
  strip_ansi: boolean;
  env: Record<string, string>;
  env_deny: string[];
  /** Resolved managed-process directory, relative to cwd when configured relatively. */
  storage_dir: string;
  statecontext: NativeProcessStateContextSettings;
}

export interface NativeToolSettings extends JsonRecord {
  read?: NativeReadToolSettings;
  write?: NativeWriteToolSettings;
  edit?: NativeEditToolSettings;
  grep?: NativeGrepToolSettings;
  bash?: NativeBashToolSettings;
  glob?: NativeGlobToolSettings;
  process?: NativeProcessToolSettings;
  task?: NativeTaskToolSettings;
  memory?: NativeMemoryToolSettings;
  fetch?: NativeFetchToolSettings;
}

export interface ToolSettings extends JsonRecord {
  native: NativeToolSettings;
  mcp: JsonRecord;
}

export interface SessionStoragePaths extends JsonRecord {
  active_profile?: string;
  messages: string;
  messages_backup: string;
  compaction_cache: string;
  history_cache: string;
  usage_cache: string;
  cursor_cache: string;
  model: string;
  staging: string;
  http_debug: string;
  statecontext: string;
}

export type StateContextToolName = "task" | "memory" | "process";
export type StateContextTrigger = "compaction";
export interface StateContextSettings extends JsonRecord {
  enabled: boolean;
  trigger: StateContextTrigger[];
  tools: StateContextToolName[];
  role: PromptRole;
  max_bytes: number;
}

export interface CompactionSettings extends JsonRecord {
  auto_enabled: boolean;
  trigger_tokens: number;
  keepbatch: {
    tools: number;
    turn: {type: "assistant_final" | "full"; keep: number};
  };
  /** Turns of the pre-compaction backup the web UI shows; `0` hides them. */
  keepbackup: {turns: number};
  backup_mode: "replace" | "append";
}

export interface PersistenceSettings extends JsonRecord {
  durability: DurabilityMode;
  max_item_bytes: number;
  response_metadata: ResponseMetadataMode;
}

export type PromptRole = "system" | "developer";

export interface McpServerReference extends JsonRecord {
  id: string;
  config_path: string;
  enabled: boolean;
  deny_tools: string[];
}

export interface SessionSettings extends JsonRecord {
  schema_version: 1;
  session_name: string;
  cwd: string;
  /** Resolved prompt-document paths in configured order. */
  prompt_documents: string[];
  prompt_role: PromptRole;
  /** Loaded prompt-document content in configured order. */
  prompt_text: string;
  max_tool_rounds: number;
  persistence: PersistenceSettings;
  compaction: CompactionSettings;
  statecontext: StateContextSettings;
  http_debug: HttpDebugSettings;
  paths: SessionStoragePaths;
  tool_definition_paths: string[];
  mcp_servers: McpServerReference[];
  tools: ToolSettings;
}

export interface SessionConfigPaths {
  global_jad_root: string;
  session_dir: string;
  model_path: string;
  setting_path: string;
  messages_path: string;
  messages_backup_path: string;
  compaction_cache_path: string;
  history_cache_path: string;
  usage_cache_path: string;
  cursor_cache_path: string;
  statecontext_path: string;
  staging_dir: string;
  http_debug_dir: string;
  active_profile?: string;
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
/**
 * Completed-response fields kept in the transcript. Providers echo the whole
 * request on every response, so `full` stores that echo verbatim.
 */
export const DEFAULT_PERSISTENCE_RESPONSE_METADATA: ResponseMetadataMode = "minimal";
/** Pre-compaction turns the web UI renders from the backup. */
export const DEFAULT_KEEP_BACKUP_TURNS = 20;
export const MAX_KEEP_BACKUP_TURNS = 1000;

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
  const rawCwd = expandHomePrefix(requiredString(value, "cwd", path));
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
/** Directory for complete streams, relative to the session cwd. */
export const DEFAULT_NATIVE_BASH_SPILL_STORAGE_DIR = ".bash-output";
/** Generated filename for each call and output stream. */
export const DEFAULT_NATIVE_BASH_SPILL_FILENAME = "bash-{call_id}.{stream}.log";
/** Deprecated template retained for legacy settings without a storage_dir. */
export const DEFAULT_NATIVE_BASH_SPILL_PATH = "{cwd}/.bash-output/bash-{call_id}.{stream}.log";
export const DEFAULT_NATIVE_BASH_MAX_LINE_CHARS = 2000;
export const DEFAULT_NATIVE_BASH_STRIP_ANSI = true;
export const DEFAULT_NATIVE_FETCH_STORAGE_DIR = ".fetch";
export const DEFAULT_NATIVE_FETCH_LIMIT = 12_000;
export const DEFAULT_NATIVE_FETCH_MAX_LIMIT = 50_000;
export const DEFAULT_NATIVE_FETCH_MAX_RESPONSE_BYTES = 2_000_000;
export const DEFAULT_NATIVE_FETCH_MAX_DOCUMENT_CHARS = 1_000_000;
export const DEFAULT_NATIVE_FETCH_MAX_STORAGE_BYTES = 64 * 1024 * 1024;
export const DEFAULT_NATIVE_FETCH_TIMEOUT_MS = 15_000;
export const DEFAULT_NATIVE_FETCH_MAX_REDIRECTS = 5;
export const DEFAULT_NATIVE_MEMORY_STORAGE_DIR = ".memory";
export const DEFAULT_NATIVE_MEMORY_MAX_TYPES = 64;
export const DEFAULT_NATIVE_MEMORY_MAX_RECORDS_PER_TYPE = 1_000;
export const DEFAULT_NATIVE_MEMORY_MAX_TOTAL_RECORDS = 10_000;
export const DEFAULT_NATIVE_MEMORY_MAX_RECORD_BYTES = 256 * 1024;
export const DEFAULT_NATIVE_MEMORY_MAX_TOTAL_BYTES = 64 * 1024 * 1024;
export const DEFAULT_NATIVE_MEMORY_LIST_LIMIT = 20;
export const DEFAULT_NATIVE_MEMORY_MAX_LIST_LIMIT = 100;
export const DEFAULT_NATIVE_MEMORY_MAX_LIST_BYTES = 64 * 1024;
export const DEFAULT_NATIVE_MEMORY_MAX_SCAN_RECORDS = 2_000;
export const DEFAULT_NATIVE_MEMORY_MAX_SCAN_BYTES = 8 * 1024 * 1024;
export const DEFAULT_NATIVE_MEMORY_MAX_FILTER_FIELDS = 8;
export const DEFAULT_NATIVE_MEMORY_MAX_FILTER_DEPTH = 8;
export const DEFAULT_NATIVE_MEMORY_MAX_JSON_DEPTH = 32;
export const DEFAULT_NATIVE_MEMORY_MAX_CURSORS = 1_000;
export const DEFAULT_NATIVE_MEMORY_CURSOR_TTL_MS = 24 * 60 * 60 * 1000;
export const DEFAULT_NATIVE_MEMORY_MAX_CURSOR_BYTES = 64 * 1024;
export const DEFAULT_NATIVE_PROCESS_STORAGE_DIR = ".processes";
export const DEFAULT_NATIVE_TASK_STORAGE_DIR = ".tasks";
export const DEFAULT_NATIVE_PROCESS_MAX_RUNNING = 8;
export const DEFAULT_NATIVE_PROCESS_WAIT_TIMEOUT_MS = 10_000;
export const DEFAULT_NATIVE_PROCESS_MAX_WAIT_TIMEOUT_MS = 120_000;
export const DEFAULT_NATIVE_PROCESS_LOG_CAP_BYTES = 64 * 1024 * 1024;
export const DEFAULT_NATIVE_PROCESS_MAX_COMPLETED = 100;
export const DEFAULT_NATIVE_TASK_MAX_TASKS = 1000;
export const DEFAULT_NATIVE_TASK_LIST_LIMIT = 100;
export const DEFAULT_NATIVE_TASK_MAX_LIST_LIMIT = 500;
export const DEFAULT_NATIVE_TASK_MAX_TITLE_CHARS = 500;
export const DEFAULT_NATIVE_TASK_MAX_TAGS = 32;
export const DEFAULT_NATIVE_TASK_MAX_TAG_CHARS = 50;
export const DEFAULT_NATIVE_TASK_MAX_DOCUMENTS = 32;
export const DEFAULT_NATIVE_TASK_MAX_DOCUMENT_CHARS = 1024;
export const DEFAULT_NATIVE_TASK_MAX_STORAGE_BYTES = 1024 * 1024;
export const DEFAULT_NATIVE_TASK_MAX_LIST_BYTES = 64 * 1024;
export const DEFAULT_NATIVE_TASK_STATECONTEXT_STATUSES: NativeTaskStateContextStatus[] = ["in-progress", "done"];
export const DEFAULT_NATIVE_TASK_STATECONTEXT_LIMIT = 25;
export const DEFAULT_NATIVE_PROCESS_STATECONTEXT_STATES: NativeProcessStateContextSettings["states"] = ["running"];
export const DEFAULT_NATIVE_PROCESS_STATECONTEXT_LIMIT = 20;
export const DEFAULT_NATIVE_MEMORY_STATECONTEXT_LIMIT = 10;
export const DEFAULT_STATECONTEXT_MAX_BYTES = 64 * 1024;
const MAX_STATECONTEXT_BYTES = 10 * 1024 * 1024;
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
const MAX_NATIVE_TASKS = 100_000;
const MAX_NATIVE_TASK_LIST_LIMIT = 10_000;
const MAX_NATIVE_TASK_TITLE_CHARS = 10_000;
const MAX_NATIVE_TASK_TAGS = 1000;
const MAX_NATIVE_TASK_TAG_CHARS = 1000;
const MAX_NATIVE_TASK_DOCUMENTS = 1000;
const MAX_NATIVE_TASK_DOCUMENT_CHARS = 16_384;

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
  const responseMetadata = persistence.response_metadata ?? DEFAULT_PERSISTENCE_RESPONSE_METADATA;
  if (responseMetadata !== "minimal" && responseMetadata !== "full") {
    throw new ConfigError(`${path}: persistence.response_metadata must be minimal or full`);
  }
  return {
    ...persistence,
    durability,
    max_item_bytes: maxItemBytes,
    response_metadata: responseMetadata,
  } as PersistenceSettings;
}

function normalizeCompaction(value: unknown, path: string): CompactionSettings {
  if (value !== undefined && !isRecord(value)) {
    throw new ConfigError(`${path}: compaction must be an object`);
  }
  const raw = value ?? {};
  const autoEnabled = raw.auto_enabled ?? true;
  if (typeof autoEnabled !== "boolean") {
    throw new ConfigError(`${path}: compaction.auto_enabled must be a boolean`);
  }
  const triggerTokens = raw.trigger_tokens ?? 200_000;
  if (typeof triggerTokens !== "number" || !Number.isSafeInteger(triggerTokens) || triggerTokens < 1 || triggerTokens > 10_000_000) {
    throw new ConfigError(`${path}: compaction.trigger_tokens must be a safe integer between 1 and 10000000`);
  }
  const keepbatch = raw.keepbatch ?? {};
  if (!isRecord(keepbatch)) throw new ConfigError(`${path}: compaction.keepbatch must be an object`);
  const tools = keepbatch.tools ?? 0;
  if (typeof tools !== "number" || !Number.isSafeInteger(tools) || tools < 0 || tools > 1000) {
    throw new ConfigError(`${path}: compaction.keepbatch.tools must be an integer between 0 and 1000`);
  }
  const turn = keepbatch.turn ?? {};
  if (!isRecord(turn)) throw new ConfigError(`${path}: compaction.keepbatch.turn must be an object`);
  const type = turn.type ?? "assistant_final";
  if (type !== "assistant_final" && type !== "full") {
    throw new ConfigError(`${path}: compaction.keepbatch.turn.type must be assistant_final or full`);
  }
  const keep = turn.keep ?? 1;
  if (typeof keep !== "number" || !Number.isSafeInteger(keep) || keep < 1 || keep > 1000) {
    throw new ConfigError(`${path}: compaction.keepbatch.turn.keep must be an integer between 1 and 1000`);
  }
  const keepbackup = raw.keepbackup ?? {};
  if (!isRecord(keepbackup)) {
    throw new ConfigError(`${path}: compaction.keepbackup must be an object`);
  }
  const backupTurns = keepbackup.turns ?? DEFAULT_KEEP_BACKUP_TURNS;
  if (
    typeof backupTurns !== "number" ||
    !Number.isSafeInteger(backupTurns) ||
    backupTurns < 0 ||
    backupTurns > MAX_KEEP_BACKUP_TURNS
  ) {
    throw new ConfigError(
      `${path}: compaction.keepbackup.turns must be an integer between 0 and ${MAX_KEEP_BACKUP_TURNS}`,
    );
  }
  const backupMode = raw.backup_mode ?? "replace";
  if (backupMode !== "replace" && backupMode !== "append") {
    throw new ConfigError(`${path}: compaction.backup_mode must be replace or append`);
  }
  return {
    ...raw,
    auto_enabled: autoEnabled,
    trigger_tokens: triggerTokens,
    keepbatch: {tools, turn: {type, keep}},
    keepbackup: {turns: backupTurns},
    backup_mode: backupMode,
  } as CompactionSettings;
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

function normalizeMcpServers(value: unknown, cwd: string, path: string): McpServerReference[] {
  if (value === undefined) return [];
  if (!Array.isArray(value)) throw new ConfigError(`${path}: mcp_servers must be an array`);
  const ids = new Set<string>();
  return value.map((entry, index) => {
    const entryPath = `${path}: mcp_servers[${index}]`;
    if (!isRecord(entry)) throw new ConfigError(`${entryPath} must be an object`);
    const id = requiredString(entry.id, `mcp_servers[${index}].id`, path);
    if (!/^[A-Za-z0-9_-]+$/.test(id)) {
      throw new ConfigError(`${entryPath}.id may contain only letters, digits, underscores, and hyphens`);
    }
    if (ids.has(id)) throw new ConfigError(`${entryPath}.id duplicates ${id}`);
    ids.add(id);
    const configuredPath = requiredString(entry.config_path, `mcp_servers[${index}].config_path`, path);
    const enabled = entry.enabled ?? false;
    if (typeof enabled !== "boolean") throw new ConfigError(`${entryPath}.enabled must be a boolean`);
    const rawDenyTools = entry.deny_tools ?? [];
    if (!Array.isArray(rawDenyTools)) throw new ConfigError(`${entryPath}.deny_tools must be an array`);
    const denyTools = rawDenyTools.map((name, denyIndex) =>
      requiredString(name, `mcp_servers[${index}].deny_tools[${denyIndex}]`, path),
    );
    if (new Set(denyTools).size !== denyTools.length) {
      throw new ConfigError(`${entryPath}.deny_tools must not contain duplicates`);
    }
    return {
      id,
      config_path: isAbsolute(configuredPath) ? resolve(configuredPath) : resolve(cwd, configuredPath),
      enabled,
      deny_tools: denyTools,
    };
  });
}

function resolveToolStoragePath(
  value: unknown,
  fallback: string,
  cwd: string,
  path: string,
): string {
  const configured = expandHomePrefix(requiredString(value ?? fallback, "storage path", path));
  return isAbsolute(configured) ? resolve(configured) : resolve(cwd, configured);
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
  readUnrestricted: boolean;
  grepUnrestricted: boolean;
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
  const legacyTemplatesConfigured = raw.stdout_full !== undefined || raw.stderr_full !== undefined;
  if (
    legacyTemplatesConfigured &&
    raw.storage_dir !== undefined &&
    raw.storage_dir !== DEFAULT_NATIVE_BASH_SPILL_STORAGE_DIR
  ) {
    throw new ConfigError(
      `${path}: tools.native.bash.spill.storage_dir cannot be combined with deprecated stdout_full/stderr_full templates; remove the templates to use storage_dir`,
    );
  }
  if (legacyTemplatesConfigured) {
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
        if (spillIsReadable(resolved[stream], context)) continue;
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

  const storageDir = resolveToolStoragePath(
    raw.storage_dir,
    DEFAULT_NATIVE_BASH_SPILL_STORAGE_DIR,
    context.cwd,
    `${path}: tools.native.bash.spill.storage_dir`,
  );
  if (!allowUnreadable) {
    for (const stream of streams) {
      const fileName = resolveSpillPath(DEFAULT_NATIVE_BASH_SPILL_FILENAME, spillSampleVars(stream, context));
      if (spillIsReadable(join(storageDir, fileName), context)) continue;
      throw new ConfigError(
        `${path}: tools.native.bash.spill.storage_dir resolves to a location the model could not read back; keep it under the session cwd (${context.cwd}) or extend tools.native.read.paths, or set tools.native.bash.spill.allow_unreadable`,
      );
    }
  }
  return {
    enabled,
    keep,
    cap_bytes: capBytes,
    retention_days: retentionDays,
    storage_dir: storageDir,
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
    readableWithRules(withinCwd, target, context.readPaths, context.readUnrestricted) ||
    readableWithRules(withinCwd, target, context.grepPaths, context.grepUnrestricted)
  );
}

function readableWithRules(
  withinCwd: boolean,
  target: string,
  rules: NativePathRule[],
  unrestricted: boolean,
): boolean {
  const matches = rules.filter((rule) => containsPath(rule.path, target));
  if (matches.some((rule) => rule.action === "deny")) {
    return false;
  }
  return unrestricted || withinCwd || matches.some((rule) => rule.action === "allow");
}

function containsPath(root: string, target: string): boolean {
  const value = relative(root, target);
  return value === "" || (!value.startsWith("..") && !isAbsolute(value));
}

/**
 * Path rules come in two shapes: the `[{action, path}]` array, and the shorter
 * `{allow: [...], deny: [...]}` object whose entries are path strings. Both
 * normalize to the same rule list; only the object form can lift the
 * workspace-only default, and it does so with an empty allow list.
 */
/**
 * Expands a leading `~` to the process home directory so a rule can read
 * `~/.ssh` instead of a literal `<jad root>/~/.ssh`. `~user` stays literal.
 */
function expandHomePrefix(configuredPath: string): string {
  if (configuredPath === "~") {
    return homedir();
  }
  return /^~[\\/]/.test(configuredPath)
    ? join(homedir(), configuredPath.slice(2))
    : configuredPath;
}

function resolveConfiguredRulePath(configuredPath: string, globalJadRoot: string): string {
  const expanded = expandHomePrefix(configuredPath);
  return isAbsolute(expanded)
    ? resolve(expanded)
    : resolve(globalJadRoot, expanded);
}

function normalizeNativePathRuleList(
  value: unknown,
  action: NativeReadPathAction,
  fieldPath: string,
  globalJadRoot: string,
): NativePathRule[] {
  if (value === undefined) {
    return [];
  }
  const entries = Array.isArray(value) ? value : [value];
  return entries.map((entry, index) => {
    const itemPath = `${fieldPath}[${index}]`;
    const configuredPath = typeof entry === "string"
      ? requiredString(entry, action, itemPath)
      : isRecord(entry)
        ? requiredString(entry.path, "path", itemPath)
        : undefined;
    if (configuredPath === undefined) {
      throw new ConfigError(`${itemPath} must be a path string or an object with a path`);
    }
    return {
      action,
      path: resolveConfiguredRulePath(configuredPath, globalJadRoot),
    } as NativePathRule;
  });
}

function inheritNativePathPolicy(source: NativePathPolicy | undefined): NativePathPolicy {
  return {
    paths: source?.paths ?? [],
    unrestricted_paths: source?.unrestricted_paths ?? false,
  };
}

function normalizeNativePathPolicy(
  value: unknown,
  path: string,
  globalJadRoot: string,
  toolName: "read" | "write" | "edit" | "grep" | "bash" | "glob" | "process",
): NativePathPolicy {
  const fieldPath = `${path}: tools.native.${toolName}.paths`;
  if (value === undefined) {
    return {paths: [], unrestricted_paths: false};
  }
  if (Array.isArray(value)) {
    return {
      paths: value.map((item, index) => {
        const itemPath = `${fieldPath}[${index}]`;
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
          path: resolveConfiguredRulePath(configuredPath, globalJadRoot),
        } as NativePathRule;
      }),
      unrestricted_paths: false,
    };
  }
  if (!isRecord(value)) {
    throw new ConfigError(
      `${fieldPath} must be an array of {action, path} rules or an object with allow and deny path lists`,
    );
  }
  for (const key of Object.keys(value)) {
    if (key !== "allow" && key !== "deny") {
      throw new ConfigError(`${fieldPath}.${key} is not supported; use allow and deny`);
    }
  }
  const allow = normalizeNativePathRuleList(value.allow, "allow", `${fieldPath}.allow`, globalJadRoot);
  const deny = normalizeNativePathRuleList(value.deny, "deny", `${fieldPath}.deny`, globalJadRoot);
  return {
    paths: [...allow, ...deny],
    unrestricted_paths: allow.length === 0,
  };
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
  const expanded = expandHomePrefix(configured);
  return isAbsolute(expanded) ? resolve(expanded) : resolve(cwd, expanded);
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

function normalizeTaskStateContext(value: unknown, path: string, maxLimit: number): NativeTaskStateContextSettings {
  if (value !== undefined && !isRecord(value)) throw new ConfigError(`${path} must be an object`);
  const raw = value ?? {};
  const allowed = new Set<NativeTaskStateContextStatus>(["todo", "in-progress", "blocked", "done"]);
  const statusesValue = raw.statuses ?? DEFAULT_NATIVE_TASK_STATECONTEXT_STATUSES;
  if (!Array.isArray(statusesValue) || statusesValue.some((status) => typeof status !== "string" || !allowed.has(status as NativeTaskStateContextStatus))) {
    throw new ConfigError(`${path}.statuses must contain task statuses`);
  }
  const limit = raw.limit ?? Math.min(DEFAULT_NATIVE_TASK_STATECONTEXT_LIMIT, maxLimit);
  if (typeof limit !== "number" || !Number.isSafeInteger(limit) || limit < 1 || limit > maxLimit) {
    throw new ConfigError(`${path}.limit must be a safe integer between 1 and ${maxLimit}`);
  }
  return {...raw, statuses: [...new Set(statusesValue as NativeTaskStateContextStatus[])], limit};
}

function normalizeProcessStateContext(value: unknown, path: string): NativeProcessStateContextSettings {
  if (value !== undefined && !isRecord(value)) throw new ConfigError(`${path} must be an object`);
  const raw = value ?? {};
  const allowed = new Set(["running", "exited", "stopped", "failed", "lost"]);
  const statesValue = raw.states ?? DEFAULT_NATIVE_PROCESS_STATECONTEXT_STATES;
  if (!Array.isArray(statesValue) || statesValue.some((state) => typeof state !== "string" || !allowed.has(state))) {
    throw new ConfigError(`${path}.states must contain process states`);
  }
  const limit = raw.limit ?? DEFAULT_NATIVE_PROCESS_STATECONTEXT_LIMIT;
  if (typeof limit !== "number" || !Number.isSafeInteger(limit) || limit < 1 || limit > MAX_NATIVE_TASKS) {
    throw new ConfigError(`${path}.limit must be a safe integer between 1 and ${MAX_NATIVE_TASKS}`);
  }
  return {...raw, states: [...new Set(statesValue as NativeProcessStateContextSettings["states"])], limit};
}

function normalizeMemoryStateContext(value: unknown, path: string, maxLimit: number, maxFilterFields: number, maxFilterDepth: number, allowedTypes?: string[]): NativeMemoryStateContextSelector[] {
  if (value === undefined) return [];
  if (!Array.isArray(value) || value.length > 128) throw new ConfigError(`${path} must be an array of at most 128 {type, filter?, limit} selectors`);
  return value.map((entry, index) => {
    const entryPath = `${path}[${index}]`;
    if (!isRecord(entry)) throw new ConfigError(`${entryPath} must be an object`);
    const type = requiredString(entry.type, "type", entryPath);
    if (!/^[a-z][a-z0-9_-]{0,63}$/.test(type)) throw new ConfigError(`${entryPath}.type is invalid`);
    if (allowedTypes && !allowedTypes.includes(type)) throw new ConfigError(`${entryPath}.type is not in tools.native.memory.allowed_types`);
    const filter = entry.filter ?? {};
    if (!isRecord(filter)) throw new ConfigError(`${entryPath}.filter must be an object`);
    const filterKeys = Object.keys(filter);
    if (filterKeys.length > maxFilterFields || filterKeys.some((key) => key.split(".").length > maxFilterDepth || key.split(".").some((part) => !/^[A-Za-z0-9_-]+$/.test(part)))) {
      throw new ConfigError(`${entryPath}.filter exceeds tools.native.memory filter limits`);
    }
    const limit = entry.limit ?? Math.min(DEFAULT_NATIVE_MEMORY_STATECONTEXT_LIMIT, maxLimit);
    if (typeof limit !== "number" || !Number.isSafeInteger(limit) || limit < 1 || limit > maxLimit) {
      throw new ConfigError(`${entryPath}.limit must be a safe integer between 1 and ${maxLimit}`);
    }
    return {type, filter, limit};
  });
}

function normalizeStateContext(value: unknown, path: string, tools: ToolSettings): StateContextSettings {
  if (value !== undefined && !isRecord(value)) throw new ConfigError(`${path}: statecontext must be an object`);
  const raw = value ?? {};
  const enabled = raw.enabled ?? false;
  if (typeof enabled !== "boolean") throw new ConfigError(`${path}: statecontext.enabled must be a boolean`);
  const triggerValue = raw.trigger ?? ["compaction"];
  if (!Array.isArray(triggerValue) || triggerValue.some((trigger) => trigger !== "compaction")) {
    throw new ConfigError(`${path}: statecontext.trigger currently supports only compaction`);
  }
  const trigger = [...new Set(triggerValue as StateContextTrigger[])];
  if (enabled && trigger.length === 0) throw new ConfigError(`${path}: statecontext.trigger must not be empty when statecontext is enabled`);
  const toolsValue = raw.tools ?? [];
  const allowedTools = new Set<StateContextToolName>(["task", "memory", "process"]);
  if (!Array.isArray(toolsValue) || toolsValue.some((name) => typeof name !== "string" || !allowedTools.has(name as StateContextToolName))) {
    throw new ConfigError(`${path}: statecontext.tools must contain only task, memory, or process`);
  }
  const providers = [...new Set(toolsValue as StateContextToolName[])];
  if (enabled && providers.length === 0) throw new ConfigError(`${path}: statecontext.tools must not be empty when statecontext is enabled`);
  const role = raw.role ?? "developer";
  if (role !== "system" && role !== "developer") throw new ConfigError(`${path}: statecontext.role must be system or developer`);
  const maxBytes = raw.max_bytes ?? DEFAULT_STATECONTEXT_MAX_BYTES;
  if (typeof maxBytes !== "number" || !Number.isSafeInteger(maxBytes) || maxBytes < 1024 || maxBytes > MAX_STATECONTEXT_BYTES) {
    throw new ConfigError(`${path}: statecontext.max_bytes must be a safe integer between 1024 and ${MAX_STATECONTEXT_BYTES}`);
  }
  if (enabled) {
    for (const name of providers) {
      const toolSettings = tools.native[name];
      if (toolSettings === undefined) throw new ConfigError(`${path}: statecontext provider ${name} requires tools.native.${name}`);
      if (name === "memory" && isRecord(toolSettings) && Array.isArray(toolSettings.statecontext) && toolSettings.statecontext.length === 0) {
        throw new ConfigError(`${path}: statecontext provider memory requires at least one tools.native.memory.statecontext selector`);
      }
    }
  }
  return {...raw, enabled, trigger, tools: providers, role, max_bytes: maxBytes};
}

/** Native tools whose settings `normalizeTools` understands. */
type NativeToolName =
  | "read"
  | "write"
  | "edit"
  | "grep"
  | "glob"
  | "bash"
  | "process"
  | "task"
  | "memory"
  | "fetch";

/**
 * Every key each native tool reads. A key outside this list is refused at load
 * time: silently ignoring a misspelled key (a singular `path` beside `paths`,
 * or a `tools.native.mcp` block) made configured tools look broken.
 * Removed keys stay listed so they keep their dedicated replacement message.
 */
const NATIVE_TOOL_KEYS: Record<NativeToolName, readonly string[]> = {
  read: ["default_limit", "max_bytes", "max_file_bytes", "paths"],
  write: ["backup_dir", "backup_max_count", "backup_max_total_bytes", "fsync", "max_bytes", "paths"],
  edit: ["backup_dir", "backup_max_count", "backup_max_total_bytes", "fsync", "max_bytes", "paths"],
  grep: ["binary", "max_bytes", "max_file_bytes", "max_line_chars", "max_matches_per_file", "max_results", "paths", "timeout_ms"],
  glob: ["binary", "include_hidden", "max_bytes", "max_line_chars", "max_results", "paths", "timeout_ms"],
  bash: [
    "env", "env_deny", "env_file", "kill_grace_ms", "kill_process_group", "login_shell",
    "max_bytes", "max_line_chars", "max_timeout_ms", "paths", "shell", "spill", "stderr",
    "stdout", "strip_ansi", "timeout_ms",
    // Removed keys, kept so their "was removed" errors below still fire.
    "capture_bytes", "head_ratio",
  ],
  process: [
    "env", "env_deny", "kill_grace_ms", "log_cap_bytes", "login_shell", "max_completed",
    "max_line_chars", "max_running", "max_wait_timeout_ms", "paths", "retention_days",
    "shell", "statecontext", "status_log_bytes", "stderr", "stdout", "storage_dir",
    "strip_ansi", "wait_timeout_ms",
  ],
  task: [
    "list_limit", "max_document_chars", "max_documents", "max_list_bytes", "max_list_limit",
    "max_storage_bytes", "max_tag_chars", "max_tags", "max_tasks", "max_title_chars",
    "statecontext", "storage_dir", "storage_path",
  ],
  memory: [
    "allowed_types", "cursor_ttl_ms", "list_limit", "max_cursor_bytes", "max_cursors",
    "max_filter_depth", "max_filter_fields", "max_json_depth", "max_list_bytes",
    "max_list_limit", "max_record_bytes", "max_records_per_type", "max_scan_bytes",
    "max_scan_records", "max_total_bytes", "max_total_records", "max_types",
    "statecontext", "storage_dir", "storage_path",
  ],
  fetch: [
    "allowed_hosts", "default_limit", "denied_hosts", "max_document_chars", "max_limit",
    "max_redirects", "max_response_bytes", "max_storage_bytes", "storage_dir", "timeout_ms",
  ],
};

const NATIVE_TOOL_NAMES = Object.keys(NATIVE_TOOL_KEYS) as NativeToolName[];

/** Nodes the `tools` block reads; any other key is a typo the loader refuses. */
const TOOL_NODE_NAMES = ["native", "mcp"] as const;

function editDistance(left: string, right: string): number {
  const previous = Array.from({length: right.length + 1}, (_entry, index) => index);
  for (let row = 1; row <= left.length; row += 1) {
    let diagonal = previous[0]!;
    previous[0] = row;
    for (let column = 1; column <= right.length; column += 1) {
      const current = previous[column]!;
      previous[column] = Math.min(
        previous[column]! + 1,
        previous[column - 1]! + 1,
        diagonal + (left[row - 1] === right[column - 1] ? 0 : 1),
      );
      diagonal = current;
    }
  }
  return previous[right.length]!;
}

/** Closest supported key within two edits, when a typo has an obvious fix. */
function closestKey(key: string, candidates: readonly string[]): string | undefined {
  let best: {name: string; distance: number} | undefined;
  for (const candidate of candidates) {
    const distance = editDistance(key, candidate);
    if (distance > 2 || (best !== undefined && distance >= best.distance)) continue;
    best = {name: candidate, distance};
  }
  return best?.name;
}

/** Refuses keys a tool does not read instead of ignoring them silently. */
function assertSupportedKeys(
  value: JsonRecord,
  keys: readonly string[],
  fieldPath: string,
  hint?: string,
): void {
  for (const key of Object.keys(value)) {
    if (keys.includes(key)) continue;
    const suggestion = closestKey(key, keys);
    const detail = suggestion
      ? `did you mean ${suggestion}?`
      : `supported keys are ${[...keys].sort().join(", ")}${hint === undefined ? "" : `; ${hint}`}`;
    throw new ConfigError(`${fieldPath}.${key} is not supported; ${detail}`);
  }
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
  assertSupportedKeys(
    tools as JsonRecord,
    TOOL_NODE_NAMES,
    `${path}: tools`,
    "tool definitions are configured with tool_definition_paths",
  );
  const nativeValue = (tools as JsonRecord).native ?? {};
  if (!isRecord(nativeValue)) {
    throw new ConfigError(`${path}: tools.native must be an object`);
  }
  assertSupportedKeys(
    nativeValue,
    NATIVE_TOOL_NAMES,
    `${path}: tools.native`,
    "MCP servers are configured under tools.mcp",
  );

  const readValue = nativeValue.read;
  let read: NativeReadToolSettings | undefined;
  if (readValue !== undefined) {
    if (!isRecord(readValue)) {
      throw new ConfigError(`${path}: tools.native.read must be an object`);
    }
    assertSupportedKeys(readValue, NATIVE_TOOL_KEYS.read, `${path}: tools.native.read`);
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
      ...normalizeNativePathPolicy(readValue.paths, path, globalJadRoot, "read"),
    };
  }

  const grepValue = nativeValue.grep;
  let grep: NativeGrepToolSettings | undefined;
  if (grepValue !== undefined) {
    if (!isRecord(grepValue)) {
      throw new ConfigError(`${path}: tools.native.grep must be an object`);
    }
    assertSupportedKeys(grepValue, NATIVE_TOOL_KEYS.grep, `${path}: tools.native.grep`);
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
      ...(grepValue.paths === undefined
        ? inheritNativePathPolicy(read)
        : normalizeNativePathPolicy(grepValue.paths, path, globalJadRoot, "grep")),
    };
  }

  const writeValue = nativeValue.write;
  let write: NativeWriteToolSettings | undefined;
  if (writeValue !== undefined) {
    if (!isRecord(writeValue)) {
      throw new ConfigError(`${path}: tools.native.write must be an object`);
    }
    assertSupportedKeys(writeValue, NATIVE_TOOL_KEYS.write, `${path}: tools.native.write`);
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
      ...normalizeNativePathPolicy(writeValue.paths, path, globalJadRoot, "write"),
    };
  }

  const editValue = nativeValue.edit;
  let edit: NativeEditToolSettings | undefined;
  if (editValue !== undefined) {
    if (!isRecord(editValue)) {
      throw new ConfigError(`${path}: tools.native.edit must be an object`);
    }
    assertSupportedKeys(editValue, NATIVE_TOOL_KEYS.edit, `${path}: tools.native.edit`);
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
      ...(editValue.paths === undefined
        ? inheritNativePathPolicy(write)
        : normalizeNativePathPolicy(editValue.paths, path, globalJadRoot, "edit")),
    };
  }

  const bashValue = nativeValue.bash;
  let bash: NativeBashToolSettings | undefined;
  if (bashValue !== undefined) {
    if (!isRecord(bashValue)) {
      throw new ConfigError(`${path}: tools.native.bash must be an object`);
    }
    assertSupportedKeys(bashValue, NATIVE_TOOL_KEYS.bash, `${path}: tools.native.bash`);
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
      readUnrestricted: read?.unrestricted_paths ?? false,
      grepUnrestricted: grep?.unrestricted_paths ?? false,
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
      ...(bashValue.paths === undefined
        ? inheritNativePathPolicy(write)
        : normalizeNativePathPolicy(bashValue.paths, path, globalJadRoot, "bash")),
    };
  }

  const taskValue = nativeValue.task;
  let task: NativeTaskToolSettings | undefined;
  if (taskValue !== undefined) {
    if (!isRecord(taskValue)) {
      throw new ConfigError(`${path}: tools.native.task must be an object`);
    }
    assertSupportedKeys(taskValue, NATIVE_TOOL_KEYS.task, `${path}: tools.native.task`);
    const positiveInteger = (field: string, fallback: number, maximum: number): number => {
      const value = taskValue[field] ?? fallback;
      if (typeof value !== "number" || !Number.isSafeInteger(value) || value < 1 || value > maximum) {
        throw new ConfigError(`${path}: tools.native.task.${field} must be a safe integer between 1 and ${maximum}`);
      }
      return value;
    };
    if (taskValue.storage_path !== undefined) {
      throw new ConfigError(`${path}: tools.native.task.storage_path was removed; use storage_dir`);
    }
    const storageDir = resolveToolStoragePath(
      taskValue.storage_dir,
      DEFAULT_NATIVE_TASK_STORAGE_DIR,
      cwd,
      `${path}: tools.native.task.storage_dir`,
    );
    const taskMaxListLimit = positiveInteger("max_list_limit", DEFAULT_NATIVE_TASK_MAX_LIST_LIMIT, MAX_NATIVE_TASK_LIST_LIMIT);
    task = {
      ...taskValue,
      max_tasks: positiveInteger("max_tasks", DEFAULT_NATIVE_TASK_MAX_TASKS, MAX_NATIVE_TASKS),
      list_limit: positiveInteger("list_limit", DEFAULT_NATIVE_TASK_LIST_LIMIT, MAX_NATIVE_TASK_LIST_LIMIT),
      max_list_limit: taskMaxListLimit,
      max_title_chars: positiveInteger("max_title_chars", DEFAULT_NATIVE_TASK_MAX_TITLE_CHARS, MAX_NATIVE_TASK_TITLE_CHARS),
      max_tags: positiveInteger("max_tags", DEFAULT_NATIVE_TASK_MAX_TAGS, MAX_NATIVE_TASK_TAGS),
      max_tag_chars: positiveInteger("max_tag_chars", DEFAULT_NATIVE_TASK_MAX_TAG_CHARS, MAX_NATIVE_TASK_TAG_CHARS),
      max_documents: positiveInteger("max_documents", DEFAULT_NATIVE_TASK_MAX_DOCUMENTS, MAX_NATIVE_TASK_DOCUMENTS),
      max_document_chars: positiveInteger("max_document_chars", DEFAULT_NATIVE_TASK_MAX_DOCUMENT_CHARS, MAX_NATIVE_TASK_DOCUMENT_CHARS),
      max_storage_bytes: positiveInteger("max_storage_bytes", DEFAULT_NATIVE_TASK_MAX_STORAGE_BYTES, MAX_NATIVE_TOOL_BYTES),
      max_list_bytes: positiveInteger("max_list_bytes", DEFAULT_NATIVE_TASK_MAX_LIST_BYTES, MAX_NATIVE_TOOL_BYTES),
      storage_dir: storageDir,
      statecontext: normalizeTaskStateContext(taskValue.statecontext, `${path}: tools.native.task.statecontext`, taskMaxListLimit),
    };
    if (task.list_limit > task.max_list_limit) {
      throw new ConfigError(`${path}: tools.native.task.list_limit must not exceed max_list_limit`);
    }
  }

  const memoryValue = nativeValue.memory;
  let memory: NativeMemoryToolSettings | undefined;
  if (memoryValue !== undefined) {
    if (!isRecord(memoryValue)) throw new ConfigError(`${path}: tools.native.memory must be an object`);
    assertSupportedKeys(memoryValue, NATIVE_TOOL_KEYS.memory, `${path}: tools.native.memory`);
    const positiveInteger = (field: string, fallback: number, maximum: number): number => {
      const value = memoryValue[field] ?? fallback;
      if (typeof value !== "number" || !Number.isSafeInteger(value) || value < 1 || value > maximum) {
        throw new ConfigError(`${path}: tools.native.memory.${field} must be a safe integer between 1 and ${maximum}`);
      }
      return value;
    };
    const allowed = memoryValue.allowed_types;
    if (allowed !== undefined && (!Array.isArray(allowed) || allowed.some((type) => typeof type !== "string" || !/^[a-z][a-z0-9_-]{0,63}$/.test(type)))) {
      throw new ConfigError(`${path}: tools.native.memory.allowed_types must contain safe type names`);
    }
    if (memoryValue.storage_path !== undefined) {
      throw new ConfigError(`${path}: tools.native.memory.storage_path was removed; use storage_dir`);
    }
    const memoryMaxListLimit = positiveInteger("max_list_limit", DEFAULT_NATIVE_MEMORY_MAX_LIST_LIMIT, MAX_NATIVE_TASKS);
    const memoryMaxFilterFields = positiveInteger("max_filter_fields", DEFAULT_NATIVE_MEMORY_MAX_FILTER_FIELDS, 64);
    const memoryMaxFilterDepth = positiveInteger("max_filter_depth", DEFAULT_NATIVE_MEMORY_MAX_FILTER_DEPTH, 32);
    const allowedTypes = allowed ? [...new Set(allowed as string[])] : undefined;
    memory = {
      ...memoryValue,
      storage_dir: resolveToolStoragePath(memoryValue.storage_dir, DEFAULT_NATIVE_MEMORY_STORAGE_DIR, cwd, `${path}: tools.native.memory.storage_dir`),
      statecontext: normalizeMemoryStateContext(memoryValue.statecontext, `${path}: tools.native.memory.statecontext`, memoryMaxListLimit, memoryMaxFilterFields, memoryMaxFilterDepth, allowedTypes),
      max_types: positiveInteger("max_types", DEFAULT_NATIVE_MEMORY_MAX_TYPES, MAX_NATIVE_TASKS),
      max_records_per_type: positiveInteger("max_records_per_type", DEFAULT_NATIVE_MEMORY_MAX_RECORDS_PER_TYPE, MAX_NATIVE_TASKS),
      max_total_records: positiveInteger("max_total_records", DEFAULT_NATIVE_MEMORY_MAX_TOTAL_RECORDS, MAX_NATIVE_TASKS * 100),
      max_record_bytes: positiveInteger("max_record_bytes", DEFAULT_NATIVE_MEMORY_MAX_RECORD_BYTES, MAX_NATIVE_TOOL_BYTES),
      max_total_bytes: positiveInteger("max_total_bytes", DEFAULT_NATIVE_MEMORY_MAX_TOTAL_BYTES, MAX_NATIVE_TOOL_BYTES * 100),
      list_limit: positiveInteger("list_limit", DEFAULT_NATIVE_MEMORY_LIST_LIMIT, DEFAULT_NATIVE_MEMORY_MAX_LIST_LIMIT),
      max_list_limit: memoryMaxListLimit,
      max_list_bytes: positiveInteger("max_list_bytes", DEFAULT_NATIVE_MEMORY_MAX_LIST_BYTES, MAX_NATIVE_TOOL_BYTES),
      max_scan_records: positiveInteger("max_scan_records", DEFAULT_NATIVE_MEMORY_MAX_SCAN_RECORDS, MAX_NATIVE_TASKS * 100),
      max_scan_bytes: positiveInteger("max_scan_bytes", DEFAULT_NATIVE_MEMORY_MAX_SCAN_BYTES, MAX_NATIVE_TOOL_BYTES * 100),
      max_filter_fields: memoryMaxFilterFields,
      max_filter_depth: memoryMaxFilterDepth,
      max_json_depth: positiveInteger("max_json_depth", DEFAULT_NATIVE_MEMORY_MAX_JSON_DEPTH, 128),
      max_cursors: positiveInteger("max_cursors", DEFAULT_NATIVE_MEMORY_MAX_CURSORS, MAX_NATIVE_TASKS),
      cursor_ttl_ms: positiveInteger("cursor_ttl_ms", DEFAULT_NATIVE_MEMORY_CURSOR_TTL_MS, 30 * 24 * 60 * 60 * 1000),
      max_cursor_bytes: positiveInteger("max_cursor_bytes", DEFAULT_NATIVE_MEMORY_MAX_CURSOR_BYTES, MAX_NATIVE_TOOL_BYTES),
      ...(allowedTypes ? {allowed_types: allowedTypes} : {}),
    };
    if (memory.list_limit > memory.max_list_limit || memory.max_record_bytes > memory.max_total_bytes) {
      throw new ConfigError(`${path}: tools.native.memory limits are incompatible`);
    }
  }

  const fetchValue = nativeValue.fetch;
  let fetchTool: NativeFetchToolSettings | undefined;
  if (fetchValue !== undefined) {
    if (!isRecord(fetchValue)) throw new ConfigError(`${path}: tools.native.fetch must be an object`);
    assertSupportedKeys(fetchValue, NATIVE_TOOL_KEYS.fetch, `${path}: tools.native.fetch`);
    const positiveInteger = (field: string, fallback: number, maximum: number): number => {
      const value = fetchValue[field] ?? fallback;
      if (typeof value !== "number" || !Number.isSafeInteger(value) || value < 1 || value > maximum) {
        throw new ConfigError(`${path}: tools.native.fetch.${field} must be a safe integer between 1 and ${maximum}`);
      }
      return value;
    };
    const normalizeHosts = (field: "allowed_hosts" | "denied_hosts"): string[] => {
      const value = fetchValue[field] ?? [];
      if (!Array.isArray(value) || value.some((host) => typeof host !== "string" || !/^[a-z0-9](?:[a-z0-9.-]{0,251}[a-z0-9])?$/i.test(host))) {
        throw new ConfigError(`${path}: tools.native.fetch.${field} must contain host names or IP addresses`);
      }
      return [...new Set(value.map((host) => host.toLowerCase()))];
    };
    fetchTool = {
      ...fetchValue,
      storage_dir: resolveToolStoragePath(fetchValue.storage_dir, DEFAULT_NATIVE_FETCH_STORAGE_DIR, cwd, `${path}: tools.native.fetch.storage_dir`),
      default_limit: positiveInteger("default_limit", DEFAULT_NATIVE_FETCH_LIMIT, DEFAULT_NATIVE_FETCH_MAX_DOCUMENT_CHARS),
      max_limit: positiveInteger("max_limit", DEFAULT_NATIVE_FETCH_MAX_LIMIT, DEFAULT_NATIVE_FETCH_MAX_DOCUMENT_CHARS),
      max_response_bytes: positiveInteger("max_response_bytes", DEFAULT_NATIVE_FETCH_MAX_RESPONSE_BYTES, MAX_NATIVE_TOOL_BYTES),
      max_document_chars: positiveInteger("max_document_chars", DEFAULT_NATIVE_FETCH_MAX_DOCUMENT_CHARS, MAX_NATIVE_TOOL_BYTES),
      max_storage_bytes: positiveInteger("max_storage_bytes", DEFAULT_NATIVE_FETCH_MAX_STORAGE_BYTES, MAX_NATIVE_TOOL_BYTES * 100),
      timeout_ms: positiveInteger("timeout_ms", DEFAULT_NATIVE_FETCH_TIMEOUT_MS, 300_000),
      max_redirects: positiveInteger("max_redirects", DEFAULT_NATIVE_FETCH_MAX_REDIRECTS, 20),
      allowed_hosts: normalizeHosts("allowed_hosts"),
      denied_hosts: normalizeHosts("denied_hosts"),
    };
    if (fetchTool.default_limit > fetchTool.max_limit || fetchTool.max_limit > fetchTool.max_document_chars || fetchTool.max_document_chars > fetchTool.max_storage_bytes) {
      throw new ConfigError(`${path}: tools.native.fetch limits are incompatible`);
    }
  }

  const processValue = nativeValue.process;
  let processTool: NativeProcessToolSettings | undefined;
  if (processValue !== undefined) {
    if (!isRecord(processValue)) {
      throw new ConfigError(`${path}: tools.native.process must be an object`);
    }
    assertSupportedKeys(processValue, NATIVE_TOOL_KEYS.process, `${path}: tools.native.process`);
    const positiveInteger = (field: string, fallback: number, maximum: number): number => {
      const value = processValue[field] ?? fallback;
      if (typeof value !== "number" || !Number.isSafeInteger(value) || value < 1 || value > maximum) {
        throw new ConfigError(`${path}: tools.native.process.${field} must be a safe integer between 1 and ${maximum}`);
      }
      return value;
    };
    const shell = processValue.shell ?? DEFAULT_NATIVE_BASH_SHELL;
    if (typeof shell !== "string" || shell.trim().length === 0) {
      throw new ConfigError(`${path}: tools.native.process.shell must be a non-empty string`);
    }
    const loginShell = processValue.login_shell ?? DEFAULT_NATIVE_BASH_LOGIN_SHELL;
    if (typeof loginShell !== "boolean") {
      throw new ConfigError(`${path}: tools.native.process.login_shell must be a boolean`);
    }
    const killGraceMs = processValue.kill_grace_ms ?? DEFAULT_NATIVE_BASH_KILL_GRACE_MS;
    if (typeof killGraceMs !== "number" || !Number.isSafeInteger(killGraceMs) || killGraceMs < 0 || killGraceMs > MAX_NATIVE_BASH_KILL_GRACE_MS) {
      throw new ConfigError(`${path}: tools.native.process.kill_grace_ms must be a safe integer between 0 and ${MAX_NATIVE_BASH_KILL_GRACE_MS}`);
    }
    if (processValue.status_log_bytes !== undefined && (processValue.stdout !== undefined || processValue.stderr !== undefined)) {
      throw new ConfigError(`${path}: tools.native.process.status_log_bytes cannot be combined with stdout or stderr budgets`);
    }
    const legacyStatusLogBytes = processValue.status_log_bytes === undefined
      ? undefined
      : positiveInteger("status_log_bytes", 1, MAX_NATIVE_TOOL_BYTES);
    const stdout = legacyStatusLogBytes === undefined
      ? normalizeBashStreamBudget(processValue.stdout, bash?.stdout ?? DEFAULT_NATIVE_BASH_STDOUT_BUDGET, `${path}: tools.native.process.stdout`)
      : {head_bytes: 0, tail_bytes: legacyStatusLogBytes};
    const stderr = legacyStatusLogBytes === undefined
      ? normalizeBashStreamBudget(processValue.stderr, bash?.stderr ?? DEFAULT_NATIVE_BASH_STDERR_BUDGET, `${path}: tools.native.process.stderr`)
      : {head_bytes: 0, tail_bytes: legacyStatusLogBytes};
    const statusOutputBytes = stdout.head_bytes + stdout.tail_bytes + stderr.head_bytes + stderr.tail_bytes;
    if (statusOutputBytes > MAX_NATIVE_TOOL_BYTES) {
      throw new ConfigError(`${path}: tools.native.process stdout and stderr budgets total ${statusOutputBytes} bytes, exceeding ${MAX_NATIVE_TOOL_BYTES}`);
    }
    const logCapBytes = positiveInteger("log_cap_bytes", DEFAULT_NATIVE_PROCESS_LOG_CAP_BYTES, 1024 * 1024 * 1024);
    if (Math.max(stdout.head_bytes + stdout.tail_bytes, stderr.head_bytes + stderr.tail_bytes) > logCapBytes) {
      throw new ConfigError(`${path}: tools.native.process stream budgets must not exceed log_cap_bytes`);
    }
    const maxLineChars = positiveInteger("max_line_chars", DEFAULT_NATIVE_BASH_MAX_LINE_CHARS, MAX_NATIVE_BASH_LINE_CHARS);
    if (maxLineChars < 20) {
      throw new ConfigError(`${path}: tools.native.process.max_line_chars must be at least 20`);
    }
    const stripAnsi = processValue.strip_ansi ?? DEFAULT_NATIVE_BASH_STRIP_ANSI;
    if (typeof stripAnsi !== "boolean") {
      throw new ConfigError(`${path}: tools.native.process.strip_ansi must be a boolean`);
    }
    const envValue = processValue.env ?? {};
    if (!isRecord(envValue)) {
      throw new ConfigError(`${path}: tools.native.process.env must be an object`);
    }
    const env: Record<string, string> = {...DEFAULT_NATIVE_BASH_ENV};
    for (const [name, value] of Object.entries(envValue)) {
      if (!ENVIRONMENT_NAME_PATTERN.test(name) || typeof value !== "string") {
        throw new ConfigError(`${path}: tools.native.process.env entries must be valid string environment variables`);
      }
      env[name] = value;
    }
    const envDenyValue = processValue.env_deny ?? [];
    if (!Array.isArray(envDenyValue) || envDenyValue.some((value) => typeof value !== "string" || !ENVIRONMENT_NAME_PATTERN.test(value))) {
      throw new ConfigError(`${path}: tools.native.process.env_deny must contain environment variable names`);
    }
    const storageDir = resolveToolStoragePath(
      processValue.storage_dir,
      DEFAULT_NATIVE_PROCESS_STORAGE_DIR,
      cwd,
      `${path}: tools.native.process.storage_dir`,
    );
    processTool = {
      ...processValue,
      shell: shell.trim(),
      login_shell: loginShell,
      kill_grace_ms: killGraceMs,
      max_running: positiveInteger("max_running", DEFAULT_NATIVE_PROCESS_MAX_RUNNING, 1024),
      max_completed: positiveInteger("max_completed", DEFAULT_NATIVE_PROCESS_MAX_COMPLETED, 100_000),
      retention_days: (() => {
        const value = processValue.retention_days ?? DEFAULT_NATIVE_BASH_SPILL_RETENTION_DAYS;
        if (typeof value !== "number" || !Number.isSafeInteger(value) || value < 0 || value > MAX_NATIVE_BASH_SPILL_RETENTION_DAYS) {
          throw new ConfigError(`${path}: tools.native.process.retention_days must be a safe integer between 0 and ${MAX_NATIVE_BASH_SPILL_RETENTION_DAYS}`);
        }
        return value;
      })(),
      wait_timeout_ms: positiveInteger("wait_timeout_ms", DEFAULT_NATIVE_PROCESS_WAIT_TIMEOUT_MS, MAX_NATIVE_BASH_MAX_TIMEOUT_MS),
      max_wait_timeout_ms: positiveInteger("max_wait_timeout_ms", DEFAULT_NATIVE_PROCESS_MAX_WAIT_TIMEOUT_MS, MAX_NATIVE_BASH_MAX_TIMEOUT_MS),
      stdout,
      stderr,
      log_cap_bytes: logCapBytes,
      max_line_chars: maxLineChars,
      strip_ansi: stripAnsi,
      env,
      env_deny: [...envDenyValue] as string[],
      storage_dir: storageDir,
      statecontext: normalizeProcessStateContext(processValue.statecontext, `${path}: tools.native.process.statecontext`),
      ...(processValue.paths === undefined
        ? inheritNativePathPolicy(bash ?? write)
        : normalizeNativePathPolicy(processValue.paths, path, globalJadRoot, "process")),
    };
    if (processTool.max_wait_timeout_ms < processTool.wait_timeout_ms) {
      throw new ConfigError(`${path}: tools.native.process.max_wait_timeout_ms must be greater than or equal to wait_timeout_ms`);
    }
  }

  const globValue = nativeValue.glob;
  let glob: NativeGlobToolSettings | undefined;
  if (globValue !== undefined) {
    if (!isRecord(globValue)) {
      throw new ConfigError(`${path}: tools.native.glob must be an object`);
    }
    assertSupportedKeys(globValue, NATIVE_TOOL_KEYS.glob, `${path}: tools.native.glob`);
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
      ...(globValue.paths === undefined
        ? inheritNativePathPolicy(read)
        : normalizeNativePathPolicy(globValue.paths, path, globalJadRoot, "glob")),
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
      ...(processTool ? {process: processTool} : {}),
      ...(task ? {task} : {}),
      ...(memory ? {memory} : {}),
      ...(fetchTool ? {fetch: fetchTool} : {}),
    },
    mcp: mcpValue,
  } as ToolSettings;
}

const SESSION_PATH_FIELDS = [
  "messages",
  "messages_backup",
  "compaction_cache",
  "history_cache",
  "usage_cache",
  "cursor_cache",
  "statecontext",
  "model",
  "staging",
  "http_debug",
] as const;
const SESSION_PATH_FIELD_SET = new Set<string>(SESSION_PATH_FIELDS);
const SESSION_PATH_PROFILE_NAME = /^[A-Za-z0-9_-]+$/;

function normalizeStoragePaths(
  value: unknown,
  path: string,
  settingPath: string,
): SessionStoragePaths {
  if (value !== undefined && !isRecord(value)) {
    throw new ConfigError(`${path}: paths must be a JSON object`);
  }
  const paths = (value ?? {}) as JsonRecord;
  const profilesValue = paths.profiles;
  const activeProfileValue = paths.active_profile;
  const basePaths = Object.fromEntries(Object.entries(paths).filter(([field]) => field !== "profiles" && field !== "active_profile"));
  if (basePaths.processes !== undefined || basePaths.tasks !== undefined) {
    throw new ConfigError(`${path}: paths.processes and paths.tasks were removed; use tools.native.process.storage_dir and tools.native.task.storage_dir`);
  }

  const normalizePathSet = (configuredPaths: JsonRecord, profilePath: string): SessionStoragePaths => {
    if (configuredPaths.processes !== undefined || configuredPaths.tasks !== undefined) {
      throw new ConfigError(`${profilePath}: paths.processes and paths.tasks were removed; use tools.native.process.storage_dir and tools.native.task.storage_dir`);
    }
    const pathValue = (field: string, fallback: string): string => resolve(
      dirname(settingPath),
      expandHomePrefix(requiredString(configuredPaths[field] ?? fallback, field, profilePath)),
    );

    const staging = pathValue("staging", "staging");
    const messages = pathValue("messages", "messages.jsonl");
    const messagesBackup = pathValue("messages_backup", "messages.backup.jsonl");
    const compactionCache = pathValue("compaction_cache", "staging/compaction-cache.json");
    const historyCache = pathValue("history_cache", join(staging, HISTORY_CACHE_FILE));
    const usageCache = pathValue("usage_cache", join(staging, USAGE_CACHE_FILE));
    const cursorCache = pathValue("cursor_cache", join(staging, CURSOR_CACHE_FILE));
    const statecontext = pathValue("statecontext", "staging/statecontext.json");
    const model = pathValue("model", "model.json");
    const distinctFilePaths = new Set([messages, messagesBackup, compactionCache, historyCache, usageCache, cursorCache, statecontext, model]);
    if (distinctFilePaths.size !== 8) {
      throw new ConfigError(`${profilePath}: paths.messages, paths.messages_backup, paths.compaction_cache, paths.history_cache, paths.usage_cache, paths.cursor_cache, paths.statecontext, and paths.model must be different files`);
    }
    const extras = Object.fromEntries(Object.entries(configuredPaths).filter(([field]) => !SESSION_PATH_FIELD_SET.has(field)));
    return {
      ...extras,
      messages,
      messages_backup: messagesBackup,
      compaction_cache: compactionCache,
      history_cache: historyCache,
      usage_cache: usageCache,
      cursor_cache: cursorCache,
      statecontext,
      model,
      staging,
      http_debug: pathValue("http_debug", "debug/http"),
    } as SessionStoragePaths;
  };

  if (profilesValue === undefined) {
    if (activeProfileValue !== undefined) {
      throw new ConfigError(`${path}: paths.active_profile requires paths.profiles`);
    }
    return normalizePathSet(basePaths, `${path}: paths`);
  }
  if (!isRecord(profilesValue) || Object.keys(profilesValue).length === 0) {
    throw new ConfigError(`${path}: paths.profiles must be a non-empty object`);
  }
  const activeProfile = requiredString(activeProfileValue, "active_profile", `${path}: paths`);
  if (!SESSION_PATH_PROFILE_NAME.test(activeProfile)) {
    throw new ConfigError(`${path}: paths.active_profile must contain only letters, numbers, underscores, or hyphens`);
  }
  const selectedProfile = profilesValue[activeProfile];
  if (selectedProfile === undefined) {
    throw new ConfigError(`${path}: paths.active_profile '${activeProfile}' is not defined in paths.profiles`);
  }

  let normalizedSelectedProfile: SessionStoragePaths | undefined;
  for (const [profileName, profileValue] of Object.entries(profilesValue)) {
    if (!SESSION_PATH_PROFILE_NAME.test(profileName)) {
      throw new ConfigError(`${path}: invalid paths.profiles key '${profileName}'`);
    }
    if (!isRecord(profileValue)) {
      throw new ConfigError(`${path}: paths.profiles.${profileName} must be an object`);
    }
    for (const [field, fieldValue] of Object.entries(profileValue)) {
      if (field === "processes" || field === "tasks") {
        throw new ConfigError(`${path}: paths.processes and paths.tasks were removed; use tools.native.process.storage_dir and tools.native.task.storage_dir`);
      }
      if (!SESSION_PATH_FIELD_SET.has(field)) {
        throw new ConfigError(`${path}: paths.profiles.${profileName}.${field} is not a supported session path`);
      }
      requiredString(fieldValue, field, `${path}: paths.profiles.${profileName}`);
    }
    const normalized = normalizePathSet({...basePaths, ...profileValue}, `${path}: paths.profiles.${profileName}`);
    if (profileName === activeProfile) normalizedSelectedProfile = normalized;
  }

  return {...normalizedSelectedProfile!, active_profile: activeProfile};
}

function sameResolvedStoragePaths(left: SessionStoragePaths, right: SessionStoragePaths): boolean {
  return left.active_profile === right.active_profile
    && SESSION_PATH_FIELDS.every((field) => left[field] === right[field]);
}

async function normalizePromptDocuments(
  value: unknown,
  cwd: string,
  path: string,
): Promise<{paths: string[]; text: string}> {
  if (value === undefined) return {paths: [], text: ""};
  if (!Array.isArray(value)) throw new ConfigError(`${path}: prompt_documents must be an array`);
  const documents = await Promise.all(value.map(async (value, index) => {
    const configured = requiredString(value, `prompt_documents[${index}]`, path);
    const documentPath = isAbsolute(configured) ? resolve(configured) : resolve(cwd, configured);
    let info;
    try { info = await stat(documentPath); } catch { throw new ConfigError(`${path}: prompt_documents[${index}] could not be read: ${documentPath}`); }
    if (!info.isFile()) throw new ConfigError(`${path}: prompt_documents[${index}] is not a file: ${documentPath}`);
    try { return {path: documentPath, text: await readFile(documentPath, "utf8")}; } catch { throw new ConfigError(`${path}: prompt_documents[${index}] could not be read: ${documentPath}`); }
  }));
  return {paths: documents.map((document) => document.path), text: documents.map((document) => document.text).join("\n\n")};
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
  if (raw.system_prompt !== undefined) {
    throw new ConfigError(`${path}: system_prompt was removed; use prompt_documents and prompt_role`);
  }
  const promptDocuments = await normalizePromptDocuments(raw.prompt_documents, cwd, path);
  const promptRole = raw.prompt_role ?? "system";
  if (promptRole !== "system" && promptRole !== "developer") {
    throw new ConfigError(`${path}: prompt_role must be system or developer`);
  }
  const maxToolRounds = normalizeMaxToolRounds(raw.max_tool_rounds, path);
  const persistence = normalizePersistence(raw.persistence, path);
  const compaction = normalizeCompaction(raw.compaction, path);
  const httpDebug = normalizeHttpDebug(raw.http_debug, path);
  const toolDefinitionPaths = normalizeToolDefinitionPaths(
    raw.tool_definition_paths,
    path,
    globalJadRoot,
  );
  const tools = await normalizeTools(raw.tools, path, globalJadRoot, sessionDir, cwd);
  const statecontext = normalizeStateContext(raw.statecontext, path, tools);
  const mcpServers = normalizeMcpServers(raw.mcp_servers, cwd, path);

  return {
    ...raw,
    schema_version: version,
    session_name: sessionName,
    cwd,
    prompt_documents: promptDocuments.paths,
    prompt_role: promptRole,
    prompt_text: promptDocuments.text,
    max_tool_rounds: maxToolRounds,
    persistence,
    compaction,
    statecontext,
    http_debug: httpDebug,
    paths: storagePaths,
    tool_definition_paths: toolDefinitionPaths,
    mcp_servers: mcpServers,
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
  private resolvedStoragePaths: SessionStoragePaths | undefined;

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
      messages_backup_path: join(absoluteSessionDir, "messages.backup.jsonl"),
      compaction_cache_path: join(absoluteSessionDir, "staging", "compaction-cache.json"),
      history_cache_path: join(absoluteSessionDir, "staging", HISTORY_CACHE_FILE),
      usage_cache_path: join(absoluteSessionDir, "staging", USAGE_CACHE_FILE),
      cursor_cache_path: join(absoluteSessionDir, "staging", CURSOR_CACHE_FILE),
      statecontext_path: join(absoluteSessionDir, "staging", "statecontext.json"),
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
    const resolvedStoragePaths = normalizeStoragePaths(
      raw.paths,
      this.paths.setting_path,
      this.paths.setting_path,
    );
    if (this.resolvedStoragePaths && !sameResolvedStoragePaths(this.resolvedStoragePaths, resolvedStoragePaths)) {
      throw new ConfigError(`${this.paths.setting_path}: session paths cannot change while this config loader is active; reopen the session to select new paths`);
    }
    const storagePaths = this.resolvedStoragePaths ?? (this.resolvedStoragePaths = resolvedStoragePaths);
    this.paths.model_path = storagePaths.model;
    this.paths.messages_path = storagePaths.messages;
    this.paths.messages_backup_path = storagePaths.messages_backup;
    this.paths.compaction_cache_path = storagePaths.compaction_cache;
    this.paths.history_cache_path = storagePaths.history_cache;
    this.paths.usage_cache_path = storagePaths.usage_cache;
    this.paths.cursor_cache_path = storagePaths.cursor_cache;
    this.paths.statecontext_path = storagePaths.statecontext;
    this.paths.staging_dir = storagePaths.staging;
    this.paths.http_debug_dir = storagePaths.http_debug;
    if (storagePaths.active_profile !== undefined) {
      this.paths.active_profile = storagePaths.active_profile;
    } else {
      delete this.paths.active_profile;
    }
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
