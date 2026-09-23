/**
 * @file glob.ts
 * @input Raw `glob` arguments plus the session settings, workspace, and policy
 * @output Exports the `glob` definition and executeNativeGlob()
 * @position Native tool that lists files matching a name pattern
 *
 * `glob` answers "which files exist?" — the question `read` (one directory level)
 * and `grep` (files containing text) cannot. It lists ripgrep's file set, which
 * already applies gitignore rules and skips symlinks, and matches the pattern in
 * process rather than passing it to ripgrep: an inclusion glob would make matching
 * files explicit and silently override ignore rules, which is exactly the surprise
 * `grep`'s `include` has.
 */

import {access, stat} from "node:fs/promises";
import {basename, dirname, isAbsolute, join} from "node:path";

import type {NativeGlobToolSettings} from "../../config.js";
import type {JsonRecord} from "../../persistence.js";
import {executionError, renderToolResult} from "../output.js";
import type {ToolDefinition, ToolExecutionContext} from "../types.js";
import {runChildProcess} from "./child.js";
import {displayPath, pathPolicyError, resolveExistingTarget} from "./path.js";

export const NATIVE_GLOB_ID = "native.glob";

/**
 * Grace between SIGTERM and SIGKILL. A listing starts no children of its own, so
 * this only matters for the interrupt itself.
 */
const GLOB_KILL_GRACE_MS = 2000;
/** Brace expansion happens before matching; refuse pathological patterns. */
const PATTERN_ALTERNATIVE_LIMIT = 64;
const REGEXP_SPECIAL = /[.+^${}()|[\]\\]/;

export const nativeGlobDefinition: ToolDefinition = {
  id: NATIVE_GLOB_ID,
  source: "native",
  name: "glob",
  description: "List workspace files by glob; use grep to search file contents.",
  inputSchema: {
    type: "object",
    properties: {
      pattern: {
        type: "string",
        description:
          "File name or path pattern to match. Omit it to list every visible file.",
      },
      path: {
        type: "string",
        minLength: 1,
        description:
          "Directory to search, relative to the workspace root or absolute. Defaults to the workspace root.",
      },
    },
    required: [],
    additionalProperties: false,
  },
};

export interface NativeGlobExecutionContext extends ToolExecutionContext {
  policy: NativeGlobToolSettings;
}

interface CompiledGlob {
  test(name: string, path: string): boolean;
}

function escapeLiteral(value: string): string {
  return Array.from(value)
    .map((character) => (REGEXP_SPECIAL.test(character) ? `\\${character}` : character))
    .join("");
}

/** Expands `{a,b}` alternatives; malformed braces stay literal. */
export function expandBraceAlternatives(
  pattern: string,
  limit = PATTERN_ALTERNATIVE_LIMIT,
): string[] {
  const open = pattern.indexOf("{");
  if (open === -1) {
    return [pattern];
  }
  const close = pattern.indexOf("}", open + 1);
  if (close === -1) {
    return [pattern];
  }
  const options = pattern.slice(open + 1, close).split(",");
  if (options.length < 2) {
    return [pattern];
  }
  const results: string[] = [];
  for (const option of options) {
    const next = `${pattern.slice(0, open)}${option}${pattern.slice(close + 1)}`;
    for (const expanded of expandBraceAlternatives(next, limit)) {
      results.push(expanded);
      if (results.length >= limit) {
        return results;
      }
    }
  }
  return results;
}

/** Translates one alternative into an anchored regular expression. */
function globToRegExpSource(pattern: string): string {
  let source = "^";
  let index = 0;
  while (index < pattern.length) {
    const character = pattern.charAt(index);
    if (character === "*") {
      if (pattern.charAt(index + 1) === "*") {
        if (pattern.charAt(index + 2) === "/") {
          // "**/" also matches no directory at all.
          source += "(?:.*/)?";
          index += 3;
          continue;
        }
        source += ".*";
        index += 2;
        continue;
      }
      source += "[^/]*";
      index += 1;
      continue;
    }
    if (character === "?") {
      source += "[^/]";
      index += 1;
      continue;
    }
    source += escapeLiteral(character);
    index += 1;
  }
  return `${source}$`;
}

/**
 * Compiles a name pattern. Returns undefined for an empty pattern, which means
 * "every file". Patterns with a slash match the workspace-relative path, all
 * others match the file name at any depth.
 */
export function compileGlob(pattern: string): CompiledGlob | undefined {
  const trimmed = pattern.trim();
  if (trimmed.length === 0) {
    return undefined;
  }
  const onPath = trimmed.includes("/");
  const expressions = expandBraceAlternatives(trimmed).map(
    (alternative) => new RegExp(globToRegExpSource(alternative)),
  );
  return {
    test: (name, path) =>
      expressions.some((expression) => expression.test(onPath ? path : name)),
  };
}

/** Nearest ancestor containing a `.git` entry, if any: ignore rules live there. */
async function inRepository(directory: string): Promise<boolean> {
  let current = directory;
  for (;;) {
    try {
      await access(join(current, ".git"));
      return true;
    } catch {
      const parent = dirname(current);
      if (parent === current) {
        return false;
      }
      current = parent;
    }
  }
}

interface ListedFile {
  name: string;
  path: string;
  absolute: string;
}

/**
 * Splits ripgrep's NUL-delimited file list without changing a single filename
 * byte. When the capture window ends mid-path, the incomplete final fragment is
 * discarded rather than being returned as a path that `read` cannot open.
 */
export function parseListedFiles(text: string, truncated: boolean): string[] {
  const entries = text.split("\u0000");
  if (truncated && !text.endsWith("\u0000")) {
    entries.pop();
  }
  // ripgrep terminates each entry, so the final split item is normally empty.
  if (entries[entries.length - 1] === "") {
    entries.pop();
  }
  return entries;
}

/** First non-empty line of a diagnostic, bounded for the result. */
function firstDiagnosticLine(text: string): string {
  const line = text
    .split("\n")
    .map((entry) => entry.trim())
    .find((entry) => entry.length > 0);
  return (line ?? "").slice(0, 200);
}

export async function executeNativeGlob(
  args: JsonRecord,
  context: NativeGlobExecutionContext,
): Promise<string> {
  const {callId, workspaceRoot, policy} = context;
  const requestedPath =
    typeof args.path === "string" && args.path.trim().length > 0 ? args.path : ".";
  const pattern = typeof args.pattern === "string" ? args.pattern.trim() : "";
  const compiled = compileGlob(pattern);

  const resolved = await resolveExistingTarget(requestedPath, workspaceRoot, policy, callId);
  if (!resolved.target) {
    return resolved.error!;
  }
  const {realPath, displayPath: rootDisplay, workspaceRoot: realRoot} = resolved.target;
  let target;
  try {
    target = await stat(realPath);
  } catch {
    return executionError(callId, "error", "path_unavailable", "The directory is not readable", [
      ["path", rootDisplay],
    ]);
  }
  if (!target.isDirectory()) {
    return executionError(
      callId,
      "invalid_input",
      "not_a_directory",
      "The path is not a directory",
      [["path", rootDisplay]],
    );
  }

  const result = await runChildProcess({
    file: policy.binary,
    args: [
      "--files",
      "--no-config",
      "--no-messages",
      "--sort",
      "path",
      "--null",
      ...(policy.include_hidden ? ["--hidden"] : []),
      "--glob",
      "!.git/**",
      "--",
      rootDisplay,
    ],
    // Paths come back relative to this directory, which is also the policy root.
    cwd: realRoot,
    env: process.env,
    timeoutMs: policy.timeout_ms,
    killGraceMs: GLOB_KILL_GRACE_MS,
    // Listing is foreground work: also remove descendants that inherited a
    // pipe and would otherwise survive after ripgrep exits.
    killProcessGroupWhenDone: true,
    signal: context.signal,
    output: {
      // A listing is read from its start, so the head window is the whole budget
      // and the tail window stays empty.
      stdout: {headBytes: policy.max_bytes, tailBytes: 0},
      // Diagnostics from the binary are only read for their first line.
      stderr: {headBytes: policy.max_bytes, tailBytes: 0},
      // The NUL list is a protocol, not model-visible text. Keep it intact
      // until parseListedFiles() has reconstructed the exact paths.
      maxLineChars: policy.max_bytes,
      stripAnsi: false,
      preserveNul: true,
    },
  });

  const render = (
    status: "ok" | "error",
    summary: string,
    information: Array<[string, unknown]>,
    content?: string,
  ): string =>
    renderToolResult({
      id: callId,
      status,
      summary,
      information,
      ...(content && content.length > 0
        ? {content: {text: content, encoding: "utf8" as const}}
        : {}),
    });

  if (result.spawnError) {
    const unavailable = result.spawnError.code === "ENOENT";
    return executionError(
      callId,
      "error",
      unavailable ? "ripgrep_unavailable" : "glob_failed",
      unavailable ? "ripgrep is not available" : "The file listing could not be started",
      [
        ["binary", policy.binary],
        ...(unavailable
          ? [["suggestion", "install ripgrep or point tools.native.glob.binary at an executable"] as [string, unknown]]
          : [["system_error", result.spawnError.message] as [string, unknown]]),
      ],
    );
  }

  const failed =
    !result.timedOut &&
    result.exitCode !== null &&
    result.exitCode !== 0 &&
    result.exitCode !== 1;
  if (failed) {
    const diagnostic = firstDiagnosticLine(result.text);
    return executionError(callId, "error", "glob_failed", "The file listing failed", [
      ["path", rootDisplay],
      ["exit_code", result.exitCode],
      ...(diagnostic.length > 0 ? [["system_error", diagnostic] as [string, unknown]] : []),
    ]);
  }

  const listed: ListedFile[] = [];
  let reachedResultCap = false;
  for (const value of parseListedFiles(result.stdout.text, result.stdout.truncated)) {
    const absolute = isAbsolute(value) ? value : join(realRoot, value);
    const name = basename(absolute);
    if (compiled && !compiled.test(name, value.replace(/^\.\//, ""))) {
      continue;
    }
    // The same policy check as `read`: a denied file never reaches the result.
    if (pathPolicyError(absolute, realRoot, policy, callId)) {
      continue;
    }
    if (listed.length >= policy.max_results) {
      reachedResultCap = true;
      break;
    }
    listed.push({name, path: displayPath(realRoot, absolute), absolute});
  }

  const truncated = reachedResultCap || result.stdout.truncated || result.timedOut;
  const truncationReason = reachedResultCap
    ? "max_results"
    : result.timedOut
      ? "timeout"
      : result.stdout.truncated
        ? "max_bytes"
        : undefined;
  const content = listed
    .map((file) => /[\n\r]/.test(file.path) ? JSON.stringify(file.path) : file.path)
    .join("\n");
  const hints: string[] = [];
  if (truncated) {
    hints.push("narrow the pattern or path");
  }
  if (listed.length === 0) {
    hints.push(
      "files ignored by git are not listed inside a repository; read a known path directly",
    );
  }
  if (truncated || listed.length === 0) {
    if (!(await inRepository(realPath))) {
      hints.push("this tree is not a git repository, so ignore rules do not apply");
    }
  }

  const information: Array<[string, unknown]> = [
    ["path", rootDisplay],
    ...(pattern.length > 0 ? [["pattern", pattern] as [string, unknown]] : []),
    ["files", listed.length],
    ["truncated", truncated],
    ...(truncationReason ? [["truncation_reason", truncationReason] as [string, unknown]] : []),
    ...(hints.length > 0 ? [["hint", hints.join("; ")] as [string, unknown]] : []),
  ];

  return render(
    "ok",
    pattern.length > 0
      ? `${listed.length} files matching ${pattern}`
      : `${listed.length} files`,
    information,
    content,
  );
}
