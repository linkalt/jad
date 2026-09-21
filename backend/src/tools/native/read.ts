import { readFile, readdir, realpath, stat } from "node:fs/promises";
import { isAbsolute, join, relative, resolve, sep } from "node:path";
import { TextDecoder } from "node:util";

import type { NativeReadToolSettings } from "../../config.js";
import type { JsonRecord } from "../../persistence.js";
import { executionError, renderToolResult } from "../output.js";
import type { ToolDefinition, ToolExecutionContext } from "../types.js";

export const NATIVE_READ_ID = "native.read";

export const nativeReadDefinition: ToolDefinition = {
  id: NATIVE_READ_ID,
  source: "native",
  name: "read",
  description: "Read file content or inspect a directory.",
  inputSchema: {
    type: "object",
    properties: {
      path: {
        type: "string",
        minLength: 1,
        description: "Path relative to cwd or an absolute path.",
      },
      offset: {
        type: "integer",
        minimum: 0,
      },
      limit: {
        type: "integer",
        minimum: 1,
      },
      rangeType: {
        type: "string",
        enum: ["lines", "bytes"],
      },
      contentEncoding: {
        type: "string",
        enum: ["utf8", "base64"],
      },
    },
    required: ["path"],
    additionalProperties: false,
    allOf: [
      {
        if: {
          properties: {rangeType: {const: "lines"}},
        },
        then: {
          properties: {offset: {type: "integer", minimum: 1}},
        },
      },
      {
        if: {
          properties: {rangeType: {const: "bytes"}},
        },
        then: {
          properties: {offset: {type: "integer", minimum: 0}},
        },
      },
    ],
  },
};

type RangeType = "lines" | "bytes";
type ContentEncoding = "utf8" | "base64";

interface ReadArgs {
  path: string;
  offset?: number;
  limit?: number;
  rangeType?: RangeType;
  contentEncoding?: ContentEncoding;
}

interface ReadContext extends ToolExecutionContext {
  policy: NativeReadToolSettings;
}

interface LineSpan {
  start: number;
  end: number;
  fullEnd: number;
}

interface ResolvedTarget {
  realPath: string;
  displayPath: string;
}

function isRecord(value: unknown): value is JsonRecord {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function withinRoot(root: string, target: string): boolean {
  if (root === sep) {
    return target.startsWith(sep);
  }
  return target === root || target.startsWith(`${root}${sep}`);
}

function displayPath(root: string, target: string): string {
  if (!withinRoot(root, target)) {
    return target;
  }
  const value = relative(root, target).split(sep).join("/");
  return value.length > 0 ? value : ".";
}

function pathPolicyError(
  target: string,
  workspaceRoot: string,
  paths: NativeReadToolSettings["paths"],
  callId: string,
): string | undefined {
  const matches = paths.filter((rule) => withinRoot(rule.path, target));
  if (matches.some((rule) => rule.action === "deny")) {
    return executionError(
      callId,
      "forbidden",
      "path_denied",
      "The requested path is denied by the read policy",
      [["path", displayPath(workspaceRoot, target)]],
    );
  }
  if (withinRoot(workspaceRoot, target) || matches.some((rule) => rule.action === "allow")) {
    return undefined;
  }
  return executionError(
    callId,
    "forbidden",
    "path_outside_workspace",
    "The requested path is outside the workspace",
    [["path", displayPath(workspaceRoot, target)]],
  );
}

async function resolveTarget(
  requestedPath: string,
  workspaceRoot: string,
  policy: NativeReadToolSettings,
  callId: string,
): Promise<{target?: ResolvedTarget; error?: string}> {
  let root: string;
  try {
    root = await realpath(workspaceRoot);
  } catch {
    return {
      error: executionError(
        callId,
        "error",
        "workspace_unavailable",
        "Workspace is not available",
      ),
    };
  }

  const candidate = isAbsolute(requestedPath)
    ? resolve(requestedPath)
    : resolve(root, requestedPath);
  const candidatePolicyError = pathPolicyError(
    candidate,
    root,
    policy.paths,
    callId,
  );
  if (candidatePolicyError) {
    return {error: candidatePolicyError};
  }
  const safeCandidatePath = displayPath(root, candidate);

  let target: string;
  try {
    target = await realpath(candidate);
  } catch (error) {
    const code = isRecord(error) && typeof error.code === "string" ? error.code : undefined;
    if (code === "ENOENT" || code === "ENOTDIR") {
      return {
        error: executionError(
          callId,
          "not_found",
          "not_found",
          `Could not find ${safeCandidatePath}`,
          [["path", safeCandidatePath]],
        ),
      };
    }
    if (code === "EACCES" || code === "EPERM") {
      return {
        error: executionError(
          callId,
          "forbidden",
          "path_forbidden",
          "The requested path is not accessible",
          [["path", safeCandidatePath]],
        ),
      };
    }
    return {
      error: executionError(
        callId,
        "error",
        "path_resolution_failed",
        "The requested path could not be resolved",
        [["path", safeCandidatePath]],
      ),
    };
  }

  if (!withinRoot(root, target) && target !== candidate) {
    return {
      error: executionError(
        callId,
        "forbidden",
        "symlink_outside_workspace",
        "The requested path resolves outside the workspace",
        [["path", safeCandidatePath]],
      ),
    };
  }

  const targetPolicyError = pathPolicyError(
    target,
    root,
    policy.paths,
    callId,
  );
  if (targetPolicyError) {
    return {error: targetPolicyError};
  }

  if (target !== candidate) {
    return {
      error: executionError(
        callId,
        "forbidden",
        "symlink_forbidden",
        "The requested path is a symlink",
        [["path", safeCandidatePath]],
      ),
    };
  }

  return {
    target: {
      realPath: target,
      displayPath: displayPath(root, target),
    },
  };
}

function lineSpans(data: Buffer): LineSpan[] {
  const result: LineSpan[] = [];
  let start = 0;
  let cursor = 0;
  while (cursor < data.length) {
    const value = data[cursor];
    if (value !== 10 && value !== 13) {
      cursor += 1;
      continue;
    }
    const end = cursor;
    const fullEnd = value === 13 && data[cursor + 1] === 10 ? cursor + 2 : cursor + 1;
    result.push({start, end, fullEnd});
    start = fullEnd;
    cursor = fullEnd;
  }
  if (start < data.length) {
    result.push({start, end: data.length, fullEnd: data.length});
  }
  return result;
}

function lineEnding(data: Buffer): string | undefined {
  const endings = new Set<string>();
  for (let index = 0; index < data.length; index += 1) {
    if (data[index] === 10) {
      endings.add(index > 0 && data[index - 1] === 13 ? "crlf" : "lf");
    } else if (data[index] === 13 && data[index + 1] !== 10) {
      endings.add("cr");
    }
  }
  if (endings.size === 0) {
    return undefined;
  }
  return endings.size === 1 ? [...endings][0] : "mixed";
}

function utf8Decoder(): TextDecoder {
  return new TextDecoder("utf-8", {fatal: true});
}

function decodeUtf8(data: Buffer): string | undefined {
  try {
    return utf8Decoder().decode(data);
  } catch {
    return undefined;
  }
}

function isContinuationByte(value: number | undefined): boolean {
  return value !== undefined && (value & 0xc0) === 0x80;
}

function utf8Range(
  data: Buffer,
  requestedStart: number,
  requestedEnd: number,
): {start: number; end: number; nextOffset: number; adjusted: boolean} {
  let start = requestedStart;
  while (start < requestedEnd && isContinuationByte(data[start])) {
    start += 1;
  }

  let end = requestedEnd;
  while (end > start && decodeUtf8(data.subarray(start, end)) === undefined) {
    end -= 1;
  }

  let nextOffset = requestedEnd;
  while (nextOffset < data.length && isContinuationByte(data[nextOffset])) {
    nextOffset += 1;
  }
  return {
    start,
    end,
    nextOffset,
    adjusted: start !== requestedStart || end !== requestedEnd || nextOffset !== requestedEnd,
  };
}

function commonFileInformation(
  target: ResolvedTarget,
  data: Buffer,
  encoding: ContentEncoding,
  rangeType: RangeType,
  spans: LineSpan[],
): Array<[string, unknown]> {
  return [
    ["path", target.displayPath],
    ["kind", "file"],
    ["size_bytes", data.length],
    ["encoding", encoding === "utf8" ? "utf-8" : "base64"],
    ...(lineEnding(data) ? [["line_ending", lineEnding(data)]] as Array<[string, unknown]> : []),
    ["total_line_count", spans.length],
    ["range_type", rangeType],
  ];
}

function renderFileRange(
  id: string,
  target: ResolvedTarget,
  data: Buffer,
  args: ReadArgs,
  policy: NativeReadToolSettings,
): string {
  const rangeType = args.rangeType ?? "lines";
  const encoding = args.contentEncoding ?? "utf8";
  const limit = args.limit ?? policy.default_limit;
  const spans = lineSpans(data);
  const information = commonFileInformation(target, data, encoding, rangeType, spans);

  if (rangeType === "lines") {
    const offset = args.offset ?? 1;
    const startIndex = Math.min(offset - 1, spans.length);
    const requestedEnd = Math.min(startIndex + limit, spans.length);
    let endIndex = startIndex;
    let returnedBytes = 0;
    let contentTruncated = false;
    let lineExceedsLimit = false;

    while (endIndex < requestedEnd) {
      const span = spans[endIndex];
      if (!span) {
        break;
      }
      const spanBytes = span.fullEnd - span.start;
      if (returnedBytes + spanBytes > policy.max_bytes) {
        contentTruncated = true;
        lineExceedsLimit = endIndex === startIndex;
        break;
      }
      returnedBytes += spanBytes;
      endIndex += 1;
    }

    const remaining = endIndex < spans.length;
    const firstSpan = spans[startIndex];
    const lastSpan = endIndex > startIndex ? spans[endIndex - 1] : undefined;
    const selected = firstSpan && lastSpan
      ? data.subarray(firstSpan.start, lastSpan.fullEnd)
      : Buffer.alloc(0);
    const contentText = encoding === "base64"
      ? selected.toString("base64")
      : decodeUtf8(selected);
    if (encoding === "utf8" && contentText === undefined) {
      return executionError(
        id,
        "unsupported_encoding",
        "unsupported_encoding",
        `Could not decode ${target.displayPath} as UTF-8`,
        [["path", target.displayPath]],
      );
    }

    information.push(
      ["returned_lines", endIndex - startIndex],
      ["returned_bytes", returnedBytes],
      ["next_offset", remaining ? endIndex + 1 : null],
      ["eof", !remaining],
    );
    if (contentTruncated) {
      information.push(
        ["content_truncated", true],
        [
          "truncation_reason",
          lineExceedsLimit ? "line_exceeds_system_byte_limit" : "system_byte_limit",
        ],
        ["max_content_bytes", policy.max_bytes],
      );
    }

    return renderToolResult({
      id,
      status: "ok",
      summary: endIndex > startIndex
        ? `Read lines ${startIndex + 1}-${endIndex} from ${target.displayPath}`
        : `Read no content from ${target.displayPath}`,
      information,
      ...(selected.length > 0 && contentText !== undefined
        ? {content: {text: contentText, encoding}}
        : {}),
    });
  }

  const offset = args.offset ?? 0;
  const start = Math.min(offset, data.length);
  const requestedEnd = Math.min(start + limit, data.length);
  const cappedEnd = Math.min(requestedEnd, start + policy.max_bytes);
  const range = encoding === "utf8"
    ? utf8Range(data, start, cappedEnd)
    : {start, end: cappedEnd, nextOffset: cappedEnd, adjusted: false};
  const selected = data.subarray(range.start, range.end);
  const contentText = encoding === "base64"
    ? selected.toString("base64")
    : decodeUtf8(selected);
  if (encoding === "utf8" && contentText === undefined) {
    return executionError(
      id,
      "unsupported_encoding",
      "unsupported_encoding",
      `Could not decode ${target.displayPath} as UTF-8`,
      [["path", target.displayPath]],
    );
  }

  const limitedByPolicy = cappedEnd < requestedEnd || range.end < cappedEnd;
  const remaining = range.nextOffset < data.length;
  information.push(
    ["returned_bytes", selected.length],
    ["next_offset", remaining ? range.nextOffset : null],
    ["eof", !remaining],
  );
  if (range.adjusted) {
    information.push(
      ["encoding_warning", "partial_utf8"],
      ["requested_start_offset", start],
      ["requested_end_offset", cappedEnd],
      ["actual_start_offset", range.start],
      ["actual_end_offset", range.end],
    );
  }
  if (limitedByPolicy) {
    information.push(
      ["content_truncated", true],
      ["truncation_reason", "system_byte_limit"],
      ["max_content_bytes", policy.max_bytes],
    );
  }

  return renderToolResult({
    id,
    status: "ok",
    summary: `Read bytes ${range.start}-${Math.max(range.start, range.end - 1)} from ${target.displayPath}`,
    information,
    ...(selected.length > 0 && contentText !== undefined
      ? {content: {text: contentText, encoding}}
      : {}),
  });
}

async function renderDirectory(
  id: string,
  target: ResolvedTarget,
  limit: number,
  maxBytes: number,
): Promise<string> {
  let entries;
  try {
    entries = await readdir(target.realPath, {withFileTypes: true});
  } catch (error) {
    return executionError(
      id,
      "error",
      "directory_read_failed",
      `Could not inspect ${target.displayPath}`,
      [["path", target.displayPath]],
    );
  }
  entries.sort((left, right) => left.name.localeCompare(right.name));
  const visible = entries.slice(0, limit);
  const entryLines: Array<[string, unknown]> = [];
  let usedBytes = 0;
  let truncated = entries.length > visible.length;
  for (const entry of visible) {
    const item: JsonRecord = {
      name: entry.name,
      kind: entry.isDirectory() ? "directory" : entry.isFile() ? "file" : "other",
    };
    if (entry.isFile()) {
      try {
        item.size_bytes = (await stat(join(target.realPath, entry.name))).size;
      } catch {
        item.size_unavailable = true;
      }
    }
    const entryBytes = Buffer.byteLength(JSON.stringify(item), "utf8");
    if (usedBytes + entryBytes > maxBytes) {
      truncated = true;
      break;
    }
    usedBytes += entryBytes;
    entryLines.push(["entry", item]);
  }

  return renderToolResult({
    id,
    status: "is_directory",
    summary: `Listed directory ${target.displayPath}`,
    information: [
      ["path", target.displayPath],
      ["kind", "directory"],
      ["entry_count", entryLines.length],
      ["directory_entries_truncated", truncated],
      ...entryLines,
    ],
  });
}

export async function executeNativeRead(
  args: JsonRecord,
  context: ReadContext,
): Promise<string> {
  const readArgs = args as unknown as ReadArgs;
  const resolved = await resolveTarget(
    readArgs.path,
    context.workspaceRoot,
    context.policy,
    context.callId,
  );
  if (resolved.error) {
    return resolved.error;
  }
  const target = resolved.target!;

  let before;
  try {
    before = await stat(target.realPath);
  } catch {
    return executionError(
      context.callId,
      "not_found",
      "not_found",
      `Could not find ${target.displayPath}`,
      [["path", target.displayPath]],
    );
  }
  if (before.isDirectory()) {
    return renderDirectory(
      context.callId,
      target,
      readArgs.limit ?? context.policy.default_limit,
      context.policy.max_bytes,
    );
  }
  if (!before.isFile()) {
    return executionError(
      context.callId,
      "error",
      "unsupported_file_type",
      `The requested path is not a regular file or directory`,
      [["path", target.displayPath]],
    );
  }

  let data: Buffer;
  try {
    data = await readFile(target.realPath);
  } catch (error) {
    return executionError(
      context.callId,
      "error",
      "read_failed",
      `Could not read ${target.displayPath}`,
      [["path", target.displayPath]],
    );
  }
  let after;
  try {
    after = await stat(target.realPath);
  } catch {
    return executionError(
      context.callId,
      "error",
      "changed_during_read",
      `The file changed while it was being read`,
      [["path", target.displayPath]],
    );
  }
  if (before.size !== after.size || before.mtimeMs !== after.mtimeMs) {
    return executionError(
      context.callId,
      "error",
      "changed_during_read",
      `The file changed while it was being read`,
      [["path", target.displayPath]],
    );
  }

  if ((readArgs.contentEncoding ?? "utf8") === "utf8" && decodeUtf8(data) === undefined) {
    return executionError(
      context.callId,
      "unsupported_encoding",
      "unsupported_encoding",
      `Could not decode ${target.displayPath} as UTF-8`,
      [["path", target.displayPath]],
    );
  }

  try {
    return renderFileRange(context.callId, target, data, readArgs, context.policy);
  } catch {
    return executionError(
      context.callId,
      "error",
      "internal_error",
      `The read operation failed`,
    );
  }
}
