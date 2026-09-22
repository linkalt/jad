import {mkdtemp, writeFile} from "node:fs/promises";
import {tmpdir} from "node:os";
import {join} from "node:path";

import {nativeBashDefinition} from "../src/tools/native/bash.js";
import {nativeEditDefinition} from "../src/tools/native/edit.js";
import {nativeGlobDefinition} from "../src/tools/native/glob.js";
import {nativeGrepDefinition} from "../src/tools/native/grep.js";
import {nativeReadDefinition} from "../src/tools/native/read.js";
import {nativeWriteDefinition} from "../src/tools/native/write.js";

export interface NativeReadSessionOptions {
  max_bytes?: number;
  max_file_bytes?: number;
  default_limit?: number;
  paths?: unknown[];
}

export interface NativeWriteSessionOptions {
  max_bytes?: number;
  backup_max_count?: number;
  backup_max_total_bytes?: number;
  backup_dir?: string | null;
  fsync?: boolean;
  paths?: unknown[];
  cwd?: string;
}

export interface NativeEditSessionOptions {
  max_bytes?: number;
  backup_max_count?: number;
  backup_max_total_bytes?: number;
  backup_dir?: string | null;
  fsync?: boolean;
  paths?: unknown[];
  cwd?: string;
}

export interface NativeGrepSessionOptions {
  binary?: string;
  max_results?: number;
  max_matches_per_file?: number;
  max_bytes?: number;
  max_line_chars?: number;
  max_file_bytes?: number;
  timeout_ms?: number;
  paths?: unknown[];
}

export interface NativeBashSessionOptions {
  shell?: string;
  login_shell?: boolean;
  timeout_ms?: number;
  max_timeout_ms?: number;
  kill_grace_ms?: number;
  kill_process_group?: "always" | "timeout_only";
  max_bytes?: number;
  stdout?: {head_bytes?: number; tail_bytes?: number};
  stderr?: {head_bytes?: number; tail_bytes?: number};
  spill?: {
    enabled?: boolean;
    keep?: "always" | "truncated_only";
    cap_bytes?: number;
    retention_days?: number;
    stdout_full?: string;
    stderr_full?: string;
    allow_unreadable?: boolean;
  };
  max_line_chars?: number;
  strip_ansi?: boolean;
  env?: Record<string, string>;
  env_deny?: string[];
  paths?: unknown[];
}

export interface NativeGlobSessionOptions {
  binary?: string;
  include_hidden?: boolean;
  max_results?: number;
  max_bytes?: number;
  max_line_chars?: number;
  timeout_ms?: number;
  paths?: unknown[];
}

export const DEFAULT_TEST_WRITE_MAX_BYTES = 64 * 1024;
export const DEFAULT_TEST_EDIT_MAX_BYTES = 64 * 1024;
export const DEFAULT_TEST_READ_MAX_FILE_BYTES = 64 * 1024 * 1024;

function readToolSettings(options: NativeReadSessionOptions): Record<string, unknown> {
  return {
    native: {
      read: {
        max_bytes: options.max_bytes ?? 8,
        max_file_bytes: options.max_file_bytes ?? DEFAULT_TEST_READ_MAX_FILE_BYTES,
        default_limit: options.default_limit ?? 250,
        ...(options.paths !== undefined ? {paths: options.paths} : {}),
      },
    },
  };
}

function writeToolSettings(options: NativeWriteSessionOptions): Record<string, unknown> {
  return {
    native: {
      write: {
        max_bytes: options.max_bytes ?? DEFAULT_TEST_WRITE_MAX_BYTES,
        ...(options.backup_max_count !== undefined
          ? {backup_max_count: options.backup_max_count}
          : {}),
        ...(options.backup_max_total_bytes !== undefined
          ? {backup_max_total_bytes: options.backup_max_total_bytes}
          : {}),
        ...(options.backup_dir !== undefined ? {backup_dir: options.backup_dir} : {}),
        ...(options.fsync !== undefined ? {fsync: options.fsync} : {}),
        ...(options.paths !== undefined ? {paths: options.paths} : {}),
      },
    },
  };
}

async function writeSetting(
  sessionDir: string,
  definitionFile: string,
  tools: Record<string, unknown>,
  cwd: string = sessionDir,
): Promise<void> {
  await writeFile(
    join(sessionDir, "setting.json"),
    JSON.stringify({
      cwd,
      tool_definition_paths: [join(sessionDir, definitionFile)],
      tools,
    }),
  );
}

async function makeToolSession(
  prefix: string,
  definitionFile: string,
  definition: unknown,
  tools: Record<string, unknown>,
  cwd?: string,
): Promise<string> {
  const sessionDir = await mkdtemp(join(tmpdir(), prefix));
  await writeFile(
    join(sessionDir, "model.json"),
    JSON.stringify({model: "gpt-test"}),
  );
  await writeFile(
    join(sessionDir, definitionFile),
    JSON.stringify({schema_version: 1, tools: [definition]}),
  );
  await writeSetting(sessionDir, definitionFile, tools, cwd);
  return sessionDir;
}

export async function makeReadToolSession(
  options: NativeReadSessionOptions = {},
): Promise<string> {
  return makeToolSession(
    "agent-harness-tools-",
    "read.json",
    nativeReadDefinition,
    readToolSettings(options),
  );
}

export async function makeWriteToolSession(
  options: NativeWriteSessionOptions = {},
): Promise<string> {
  return makeToolSession(
    "agent-harness-write-tools-",
    "write.json",
    nativeWriteDefinition,
    writeToolSettings(options),
    options.cwd,
  );
}

export async function refreshReadSession(
  sessionDir: string,
  options: NativeReadSessionOptions = {},
): Promise<void> {
  await writeSetting(sessionDir, "read.json", readToolSettings(options));
}

export async function refreshWriteSession(
  sessionDir: string,
  options: NativeWriteSessionOptions = {},
): Promise<void> {
  await writeSetting(sessionDir, "write.json", writeToolSettings(options), options.cwd);
}

function editToolSettings(options: NativeEditSessionOptions): Record<string, unknown> {
  return {
    native: {
      edit: {
        max_bytes: options.max_bytes ?? DEFAULT_TEST_EDIT_MAX_BYTES,
        ...(options.backup_max_count !== undefined
          ? {backup_max_count: options.backup_max_count}
          : {}),
        ...(options.backup_max_total_bytes !== undefined
          ? {backup_max_total_bytes: options.backup_max_total_bytes}
          : {}),
        ...(options.backup_dir !== undefined ? {backup_dir: options.backup_dir} : {}),
        ...(options.fsync !== undefined ? {fsync: options.fsync} : {}),
        ...(options.paths !== undefined ? {paths: options.paths} : {}),
      },
    },
  };
}

export async function makeEditToolSession(
  options: NativeEditSessionOptions = {},
): Promise<string> {
  return makeToolSession(
    "agent-harness-edit-tools-",
    "edit.json",
    nativeEditDefinition,
    editToolSettings(options),
    options.cwd,
  );
}

export async function refreshEditSession(
  sessionDir: string,
  options: NativeEditSessionOptions = {},
): Promise<void> {
  await writeSetting(sessionDir, "edit.json", editToolSettings(options), options.cwd);
}

function grepToolSettings(options: NativeGrepSessionOptions): Record<string, unknown> {
  return {
    native: {
      grep: {
        ...(options.binary !== undefined ? {binary: options.binary} : {}),
        ...(options.max_results !== undefined ? {max_results: options.max_results} : {}),
        ...(options.max_matches_per_file !== undefined
          ? {max_matches_per_file: options.max_matches_per_file}
          : {}),
        ...(options.max_bytes !== undefined ? {max_bytes: options.max_bytes} : {}),
        ...(options.max_line_chars !== undefined
          ? {max_line_chars: options.max_line_chars}
          : {}),
        ...(options.max_file_bytes !== undefined
          ? {max_file_bytes: options.max_file_bytes}
          : {}),
        ...(options.timeout_ms !== undefined ? {timeout_ms: options.timeout_ms} : {}),
        ...(options.paths !== undefined ? {paths: options.paths} : {}),
      },
    },
  };
}

/**
 * Session with `grep` enabled. Omitted options fall back to the production
 * defaults through the settings validator, so tests exercise real values.
 */
export async function makeGrepToolSession(
  options: NativeGrepSessionOptions = {},
): Promise<string> {
  return makeToolSession(
    "agent-harness-grep-tools-",
    "grep.json",
    nativeGrepDefinition,
    grepToolSettings(options),
  );
}

export async function refreshGrepSession(
  sessionDir: string,
  options: NativeGrepSessionOptions = {},
): Promise<void> {
  await writeSetting(sessionDir, "grep.json", grepToolSettings(options));
}

function bashToolSettings(options: NativeBashSessionOptions): Record<string, unknown> {
  return {
    native: {
      bash: {
        ...(options.shell !== undefined ? {shell: options.shell} : {}),
        ...(options.login_shell !== undefined ? {login_shell: options.login_shell} : {}),
        ...(options.timeout_ms !== undefined ? {timeout_ms: options.timeout_ms} : {}),
        ...(options.max_timeout_ms !== undefined
          ? {max_timeout_ms: options.max_timeout_ms}
          : {}),
        ...(options.kill_grace_ms !== undefined ? {kill_grace_ms: options.kill_grace_ms} : {}),
        ...(options.kill_process_group !== undefined
          ? {kill_process_group: options.kill_process_group}
          : {}),
        ...(options.max_bytes !== undefined ? {max_bytes: options.max_bytes} : {}),
        ...(options.stdout !== undefined ? {stdout: options.stdout} : {}),
        ...(options.stderr !== undefined ? {stderr: options.stderr} : {}),
        ...(options.spill !== undefined ? {spill: options.spill} : {}),
        ...(options.max_line_chars !== undefined
          ? {max_line_chars: options.max_line_chars}
          : {}),
        ...(options.strip_ansi !== undefined ? {strip_ansi: options.strip_ansi} : {}),
        ...(options.env !== undefined ? {env: options.env} : {}),
        ...(options.env_deny !== undefined ? {env_deny: options.env_deny} : {}),
        ...(options.paths !== undefined ? {paths: options.paths} : {}),
      },
    },
  };
}

/**
 * Session with `bash` enabled. Omitted options fall back to the production
 * defaults through the settings validator, so tests exercise real values.
 */
export async function makeBashToolSession(
  options: NativeBashSessionOptions = {},
): Promise<string> {
  return makeToolSession(
    "agent-harness-bash-tools-",
    "bash.json",
    nativeBashDefinition,
    bashToolSettings(options),
  );
}

export async function refreshBashSession(
  sessionDir: string,
  options: NativeBashSessionOptions = {},
): Promise<void> {
  await writeSetting(sessionDir, "bash.json", bashToolSettings(options));
}

function globToolSettings(options: NativeGlobSessionOptions): Record<string, unknown> {
  return {
    native: {
      glob: {
        ...(options.binary !== undefined ? {binary: options.binary} : {}),
        ...(options.include_hidden !== undefined
          ? {include_hidden: options.include_hidden}
          : {}),
        ...(options.max_results !== undefined ? {max_results: options.max_results} : {}),
        ...(options.max_bytes !== undefined ? {max_bytes: options.max_bytes} : {}),
        ...(options.max_line_chars !== undefined
          ? {max_line_chars: options.max_line_chars}
          : {}),
        ...(options.timeout_ms !== undefined ? {timeout_ms: options.timeout_ms} : {}),
        ...(options.paths !== undefined ? {paths: options.paths} : {}),
      },
    },
  };
}

/**
 * Session with `glob` enabled. Omitted options fall back to the production
 * defaults through the settings validator, so tests exercise real values.
 */
export async function makeGlobToolSession(
  options: NativeGlobSessionOptions = {},
): Promise<string> {
  return makeToolSession(
    "agent-harness-glob-tools-",
    "glob.json",
    nativeGlobDefinition,
    globToolSettings(options),
  );
}

export async function refreshGlobSession(
  sessionDir: string,
  options: NativeGlobSessionOptions = {},
): Promise<void> {
  await writeSetting(sessionDir, "glob.json", globToolSettings(options));
}

/**
 * Session with both mutating tools enabled, so tests can check that they share
 * one lock, one backup history and one write path policy.
 */
export async function makeWriteEditToolSession(
  options: {write?: NativeWriteSessionOptions; edit?: NativeEditSessionOptions} = {},
): Promise<string> {
  const sessionDir = await mkdtemp(join(tmpdir(), "agent-harness-mutate-tools-"));
  await writeFile(
    join(sessionDir, "model.json"),
    JSON.stringify({model: "gpt-test"}),
  );
  await writeFile(
    join(sessionDir, "tools.json"),
    JSON.stringify({
      schema_version: 1,
      tools: [nativeWriteDefinition, nativeEditDefinition],
    }),
  );
  const write = writeToolSettings(options.write ?? {}) as {
    native: Record<string, unknown>;
  };
  const edit = editToolSettings(options.edit ?? {}) as {
    native: Record<string, unknown>;
  };
  await writeFile(
    join(sessionDir, "setting.json"),
    JSON.stringify({
      cwd: options.write?.cwd ?? sessionDir,
      tool_definition_paths: [join(sessionDir, "tools.json")],
      tools: {native: {...write.native, ...edit.native}},
    }),
  );
  return sessionDir;
}

export function outputText(value: unknown): string {
  if (typeof value !== "string") {
    throw new TypeError("expected tool output to be a string");
  }
  return value;
}

export function firstLine(value: string): Record<string, unknown> {
  return JSON.parse(value.split("\n", 1)[0]!) as Record<string, unknown>;
}

/** Parses the `key: value` lines of a tool result's information block. */
export function informationFields(value: string): Record<string, string> {
  return Object.fromEntries(informationEntries(value));
}

/** Parses the information block in order, keeping repeated keys. */
export function informationEntries(value: string): Array<[string, string]> {
  const lines = value.split("\n");
  const start = lines.indexOf("--- INFORMATION ---");
  const entries: Array<[string, string]> = [];
  for (const line of lines.slice(start + 1)) {
    if (line.trim().length === 0) {
      break;
    }
    const separator = line.indexOf(": ");
    if (separator > 0) {
      entries.push([line.slice(0, separator), line.slice(separator + 2)]);
    }
  }
  return entries;
}
