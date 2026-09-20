import { createHash } from "node:crypto";
import { readFile, stat } from "node:fs/promises";
import { basename, join, resolve } from "node:path";

import type { JsonRecord } from "./persistence.js";

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

export interface SessionSettings extends JsonRecord {
  schema_version: 1;
  session_name: string;
  cwd: string;
  system_prompt: string;
  http_debug: HttpDebugSettings;
}

export interface SessionConfigPaths {
  session_dir: string;
  model_path: string;
  setting_path: string;
}

export interface LoadedSessionConfig {
  model: ModelConfig;
  settings: SessionSettings;
  paths: SessionConfigPaths;
  model_sha256: string;
  settings_sha256: string;
}

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
  if (!/^[A-Za-z_][A-Za-z0-9_]*$/.test(apiKeyEnv)) {
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
  const cwd = resolve(requiredString(value, "cwd", path));
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

async function normalizeSettings(
  raw: JsonRecord,
  path: string,
  sessionDir: string,
): Promise<SessionSettings> {
  const version = schemaVersion(raw.schema_version, path);
  const sessionName = requiredString(
    raw.session_name ?? basename(resolve(sessionDir)),
    "session_name",
    path,
  );
  const cwd = await normalizeCwd(raw.cwd ?? process.cwd(), path);
  const systemPrompt = raw.system_prompt ?? "";
  if (typeof systemPrompt !== "string") {
    throw new ConfigError(`${path}: system_prompt must be a string`);
  }
  const httpDebug = normalizeHttpDebug(raw.http_debug, path);

  return {
    ...raw,
    schema_version: version,
    session_name: sessionName,
    cwd,
    system_prompt: systemPrompt,
    http_debug: httpDebug,
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

  public constructor(sessionDir: string) {
    const absoluteSessionDir = resolve(sessionDir);
    this.paths = {
      session_dir: absoluteSessionDir,
      model_path: join(absoluteSessionDir, "model.json"),
      setting_path: join(absoluteSessionDir, "setting.json"),
    };
  }

  public async loadModel(): Promise<ModelConfig> {
    const raw = await readObject(this.paths.model_path, false);
    return normalizeModel(raw!, this.paths.model_path);
  }

  public async loadSettings(): Promise<SessionSettings> {
    const raw = (await readObject(this.paths.setting_path, true)) ?? {};
    return normalizeSettings(raw, this.paths.setting_path, this.paths.session_dir);
  }

  public async load(): Promise<LoadedSessionConfig> {
    const [model, settings] = await Promise.all([this.loadModel(), this.loadSettings()]);
    return {
      model,
      settings,
      paths: this.paths,
      model_sha256: sha256Json(model),
      settings_sha256: sha256Json(settings),
    };
  }
}
