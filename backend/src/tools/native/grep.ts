/**
 * @file grep.ts
 * @input Validated tool arguments and the session grep policy
 * @output Exports the native `grep` definition and executor
 * @position Tool layer: policy, flags, and result metadata around the ripgrep runner
 */

import type {NativeGrepToolSettings} from "../../config.js";
import type {JsonRecord} from "../../persistence.js";
import {executionError, renderToolResult} from "../output.js";
import type {ToolDefinition, ToolExecutionContext} from "../types.js";
import {displayPath, resolveExistingTarget, withinRoot} from "./path.js";
import {renderRipgrepGroups, runRipgrep} from "./ripgrep.js";
import type {RipgrepGroup, RipgrepResult} from "./ripgrep.js";

export const NATIVE_GREP_ID = "native.grep";

export const nativeGrepDefinition: ToolDefinition = {
  id: NATIVE_GREP_ID,
  source: "native",
  name: "grep",
  description: "Search workspace file contents and group matching lines by file.",
  inputSchema: {
    type: "object",
    properties: {
      pattern: {
        type: "string",
        minLength: 1,
        description:
          "ripgrep regular expression (Rust syntax: no lookaround or backreferences). Use literal when the text contains regex characters.",
      },
      path: {
        type: "string",
        minLength: 1,
        description:
          "File or directory to search, relative to cwd or absolute. Defaults to the workspace root. Explicit paths are searched even when ignored or hidden.",
      },
      include: {
        type: "string",
        minLength: 1,
        description:
          "Only search files matching this glob, for example \"*.ts\" or \"src/**/*.{ts,tsx}\".",
      },
      literal: {
        type: "boolean",
        description: "Treat the pattern as exact text instead of a regular expression.",
      },
      context: {
        type: "integer",
        minimum: 0,
        maximum: 20,
        description: "Lines of context to show before and after each match.",
      },
    },
    required: ["pattern"],
    additionalProperties: false,
  },
};

interface GrepArgs {
  pattern: string;
  path?: string;
  include?: string;
  literal?: boolean;
  context?: number;
}

interface GrepContext extends ToolExecutionContext {
  policy: NativeGrepToolSettings;
}

export function normalizeGrepArguments(args: JsonRecord): JsonRecord {
  const normalized = {...args};
  if (typeof args.context === "number" && Number.isSafeInteger(args.context)) {
    normalized.context = Math.min(Math.max(args.context, 0), 20);
  }
  return normalized;
}

function searchFlags(args: GrepArgs): string[] {
  const flags: string[] = [];
  if (args.literal === true) {
    flags.push("--fixed-strings");
  }
  if (args.include !== undefined) {
    flags.push("--glob", args.include);
  }
  const context = args.context ?? 0;
  if (context > 0) {
    flags.push("--context", String(Math.min(context, 20)));
  }
  return flags;
}

/**
 * ripgrep applies glob exclusions while walking, preventing denied files from
 * consuming match, byte, or timeout budgets before post-search policy checks.
 * Rules outside the workspace stay post-filtered because an external root's
 * ripgrep-relative spelling is platform-specific.
 */
export function deniedTraversalFlags(
  policy: NativeGrepToolSettings,
  workspaceRoot: string,
): string[] {
  const flags: string[] = [];
  for (const rule of policy.paths) {
    if (rule.action !== "deny" || !withinRoot(workspaceRoot, rule.path)) {
      continue;
    }
    const path = displayPath(workspaceRoot, rule.path);
    if (path === ".") {
      continue;
    }
    // ripgrep does not apply a brace alternative to a file exclusion, so emit
    // separate rules for the exact path and its potential descendants.
    flags.push("--glob", `!${path}`, "--glob", `!${path}/**`);
  }
  return flags;
}

function truncationSummary(
  result: RipgrepResult,
  pattern: string,
  files: number,
  matches: number,
): string {
  if (matches === 0) {
    return result.truncated
      ? `No matches for ${pattern} before the search stopped`
      : `No matches for ${pattern}`;
  }
  if (result.truncated) {
    return `Showing the first ${matches} matches in ${files} files for ${pattern}`;
  }
  return `${matches} matches in ${files} files for ${pattern}`;
}

function searchHints(result: RipgrepResult, matches: number): string[] {
  const hints: string[] = [];
  if (result.truncated) {
    hints.push(
      result.truncationReason === "timeout"
        ? "the search timed out; narrow the path, include, or pattern"
        : "narrow the path, include, or pattern",
    );
  } else if (matches === 0) {
    hints.push(
      "hidden files and files ignored by git are not searched; pass an explicit path to search them",
    );
  }
  if (result.perFileLimitReached) {
    hints.push(
      "a file reached max_matches_per_file; search that path directly for the rest",
    );
  }
  return hints;
}

function failureResult(
  callId: string,
  errorPath: string,
  args: GrepArgs,
  result: RipgrepResult,
): string {
  const failure = result.failure!;
  if (failure.kind === "invalid_pattern") {
    return executionError(
      callId,
      "invalid_input",
      "invalid_pattern",
      "The search pattern is not a valid ripgrep regular expression",
      [
        ["pattern", args.pattern],
        ["system_error", failure.message],
        ["suggestion", "use literal: true to search for the exact text"],
      ],
    );
  }
  if (failure.kind === "unavailable") {
    return executionError(
      callId,
      "error",
      "ripgrep_unavailable",
      "The ripgrep executable could not be started",
      [
        ["path", errorPath],
        ["system_error", failure.message],
        [
          "suggestion",
          "install ripgrep or point tools.native.grep.binary at the executable",
        ],
      ],
    );
  }
  return executionError(
    callId,
    "error",
    "grep_failed",
    "The search could not be completed",
    [
      ["path", errorPath],
      ["system_error", failure.message],
    ],
  );
}

export async function executeNativeGrep(
  args: JsonRecord,
  context: GrepContext,
): Promise<string> {
  const grepArgs = args as unknown as GrepArgs;
  const policy = context.policy;

  const resolvedRoot = await resolveExistingTarget(
    grepArgs.path ?? ".",
    context.workspaceRoot,
    policy,
    context.callId,
  );
  if (resolvedRoot.error) {
    return resolvedRoot.error;
  }
  const root = resolvedRoot.target!;

  const result = await runRipgrep({
    binary: policy.binary,
    // Denials follow caller-supplied includes so an include cannot override a
    // policy exclusion.
    flags: [...searchFlags(grepArgs), ...deniedTraversalFlags(policy, root.workspaceRoot)],
    pattern: grepArgs.pattern,
    paths: [root.displayPath],
    cwd: context.workspaceRoot,
    timeoutMs: policy.timeout_ms,
    signal: context.signal,
    maxMatches: policy.max_results,
    maxMatchesPerFile: policy.max_matches_per_file,
    maxFileBytes: policy.max_file_bytes,
    maxBytes: policy.max_bytes,
    maxLineChars: policy.max_line_chars,
  });

  if (result.failure) {
    return failureResult(context.callId, root.errorPath, grepArgs, result);
  }

  // Every reported file goes through the same policy check as `read`, so a
  // denied file never reaches the result, whatever the traversal found.
  const allowed: RipgrepGroup[] = [];
  let matches = 0;
  for (const group of result.groups) {
    const resolved = await resolveExistingTarget(
      group.path,
      context.workspaceRoot,
      policy,
      context.callId,
    );
    if (resolved.error || !resolved.target) {
      continue;
    }
    allowed.push({...group, path: resolved.target.displayPath});
    matches += group.matches;
  }

  const text = renderRipgrepGroups(allowed);
  const files = allowed.length;
  const information: Array<[string, unknown]> = [
    ["path", root.displayPath],
    ["pattern", grepArgs.pattern],
    ...(grepArgs.include !== undefined ? [["include", grepArgs.include] as [string, unknown]] : []),
    ...(grepArgs.literal === true ? [["literal", true] as [string, unknown]] : []),
    ...((grepArgs.context ?? 0) > 0
      ? [["context", Math.min(grepArgs.context ?? 0, 20)] as [string, unknown]]
      : []),
    ["files", files],
    ["matches", matches],
    ["truncated", result.truncated],
    ...(result.truncationReason
      ? [["truncation_reason", result.truncationReason] as [string, unknown]]
      : []),
    ...(result.perFileLimitReached
      ? [["per_file_limit_reached", true] as [string, unknown]]
      : []),
    ...(result.binarySkipped > 0
      ? [["binary_files_skipped", result.binarySkipped] as [string, unknown]]
      : []),
  ];
  const hints = searchHints(result, matches);
  if (hints.length > 0) {
    information.push(["hint", hints.join("; ")]);
  }

  return renderToolResult({
    id: context.callId,
    status: "ok",
    summary: truncationSummary(result, grepArgs.pattern, files, matches),
    information,
    ...(text.length > 0 ? {content: {text, encoding: "utf8"}} : {}),
  });
}
