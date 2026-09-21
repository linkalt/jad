import { readdir, readFile, stat } from "node:fs/promises";
import { dirname, resolve } from "node:path";

import { ConfigError } from "./config.js";

const ROOT_ID_PATTERN = /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/;
const DEFAULT_ROOT_ID = "default";
const DEFAULT_ROOT_LABEL = "Local sessions";

export interface SessionRoot {
  id: string;
  label: string;
  directory: string;
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
    resolve(process.cwd(), ".jad/sessions"),
    resolve(process.cwd(), "../.jad/sessions"),
  ];
  let directory = candidates[0]!;
  for (const candidate of candidates) {
    try {
      const info = await stat(candidate);
      if (info.isDirectory()) {
        directory = candidate;
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
    directory,
  }];
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
    roots.push({id: entry.id, label: entry.label, directory});
  }
  return roots;
}

export async function loadSessionRoots(options: SessionRootOptions = {}): Promise<SessionRoot[]> {
  const environment = options.environment ?? process.env;
  if (options.sessionsDir !== undefined) {
    return [
      {
        id: DEFAULT_ROOT_ID,
        label: DEFAULT_ROOT_LABEL,
        directory: resolve(options.sessionsDir),
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
      },
    ];
  }

  const configuredSettingPath = options.rootSettingPath?.trim() || environment.JAD_ROOT_SETTING_PATH?.trim();
  if (configuredSettingPath) {
    return rootsFromSetting(resolve(configuredSettingPath));
  }

  const candidates = [
    resolve(process.cwd(), "setting.json"),
    resolve(process.cwd(), "../setting.json"),
  ];
  const settingPath = await firstExistingPath(candidates);
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
