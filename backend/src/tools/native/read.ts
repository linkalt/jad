import {lstat, readdir, stat} from "node:fs/promises";
import { join } from "node:path";

import type { NativeReadToolSettings } from "../../config.js";
import type { JsonRecord } from "../../persistence.js";
import { executionError, renderToolResult } from "../output.js";
import {
  pathPolicyError,
  readVerifiedRegularFile,
  resolveExistingTarget,
  sameFileIdentity,
} from "./path.js";
import type {ResolvedTarget} from "./path.js";
import {decodeUtf8, normalizeEncoding, normalizeRangeType} from "./text.js";
import type {ContentEncoding, RangeType} from "./text.js";
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
        description:
          "Start position: a 1-based line number, or a 0-based byte offset when rangeType is bytes. Values below the start mean the beginning. Defaults to the beginning.",
      },
      limit: {
        type: "integer",
        minimum: 1,
        description: "Maximum lines or bytes to return. Defaults to the configured content limit.",
      },
      rangeType: {
        type: "string",
        enum: ["lines", "bytes"],
        description: "Unit used by offset and limit. Defaults to lines.",
      },
      contentEncoding: {
        type: "string",
        enum: ["utf8", "base64"],
        description:
          "utf8 for text, base64 for binary content. Defaults to the encoding that decodes the content.",
      },
      includeContent: {
        type: "boolean",
        description: "When false, return file metadata without reading file content.",
      },
    },
    required: ["path"],
    additionalProperties: false,
  },
};

interface ReadArgs {
  path: string;
  offset?: number;
  limit?: number;
  rangeType?: RangeType;
  contentEncoding?: ContentEncoding;
  includeContent?: boolean;
}

interface ReadContext extends ToolExecutionContext {
  policy: NativeReadToolSettings;
}

interface LineSpan {
  start: number;
  end: number;
  fullEnd: number;
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
  encodingReason?: "invalid_utf8",
): Array<[string, unknown]> {
  return [
    ["path", target.displayPath],
    ["kind", "file"],
    ["size_bytes", data.length],
    ["encoding", encoding],
    ...(encodingReason
      ? [["encoding_reason", encodingReason]] as Array<[string, unknown]>
      : []),
    ...(lineEnding(data) ? [["line_ending", lineEnding(data)]] as Array<[string, unknown]> : []),
    ["total_line_count", spans.length],
    ["range_type", rangeType],
  ];
}

function renderFileMetadata(
  id: string,
  target: ResolvedTarget,
  sizeBytes: number,
): string {
  return renderToolResult({
    id,
    status: "ok",
    summary: "Inspected file metadata",
    information: [
      ["path", target.displayPath],
      ["kind", "file"],
      ["size_bytes", sizeBytes],
      ["metadata_only", true],
    ],
  });
}

function renderFileRange(
  id: string,
  target: ResolvedTarget,
  data: Buffer,
  args: ReadArgs,
  policy: NativeReadToolSettings,
  encoding: ContentEncoding,
  encodingReason?: "invalid_utf8",
): string {
  const rangeType = normalizeRangeType(args.rangeType) ?? "lines";
  const limit = args.limit ?? policy.default_limit;
  const spans = lineSpans(data);
  const information = commonFileInformation(
    target,
    data,
    encoding,
    rangeType,
    spans,
    encodingReason,
  );

  if (rangeType === "lines") {
    const requestedOffset = args.offset ?? 1;
    const offset = Math.max(1, requestedOffset);
    const offsetAdjusted = offset !== requestedOffset;
    const offsetClamped = offset - 1 > spans.length;
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
        "binary_file",
        `Could not decode ${target.errorPath} as UTF-8`,
        [["path", target.errorPath]],
      );
    }

    information.push(
      ["returned_lines", endIndex - startIndex],
      ["returned_bytes", returnedBytes],
      ["next_offset", remaining ? endIndex + 1 : null],
      ["eof", !remaining],
      ...(offsetClamped || offsetAdjusted
        ? [["offset_clamped", true] as [string, unknown]]
        : []),
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
        : offsetClamped
          ? "No content; offset is at or beyond EOF"
          : "No content at EOF",
      information,
      ...(selected.length > 0 && contentText !== undefined
        ? {content: {text: contentText, encoding}}
        : {}),
    });
  }

  const offset = Math.max(0, args.offset ?? 0);
  const offsetAdjusted = offset !== (args.offset ?? 0);
  const offsetClamped = offset > data.length;
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
      "binary_file",
      `Could not decode ${target.errorPath} as UTF-8`,
      [["path", target.errorPath]],
    );
  }

  const limitedByPolicy = cappedEnd < requestedEnd || range.end < cappedEnd;
  const remaining = range.nextOffset < data.length;
  information.push(
    ["returned_bytes", selected.length],
    ["next_offset", remaining ? range.nextOffset : null],
    ["eof", !remaining],
    ...(offsetClamped || offsetAdjusted
      ? [["offset_clamped", true] as [string, unknown]]
      : []),
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
    summary: selected.length > 0
      ? `Read bytes ${range.start}-${range.end - 1} from ${target.displayPath}`
      : offsetClamped
        ? "No content; offset is at or beyond EOF"
        : "No content at EOF",
    information,
    ...(selected.length > 0 && contentText !== undefined
      ? {content: {text: contentText, encoding}}
      : {}),
  });
}

async function renderDirectory(
  id: string,
  target: ResolvedTarget,
  policy: NativeReadToolSettings,
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
      `Could not inspect ${target.errorPath}`,
      [["path", target.errorPath]],
    );
  }
  entries.sort((left, right) => left.name.localeCompare(right.name));
  // Filter before applying the result limit, otherwise denied names can both
  // leak through truncation behavior and crowd allowed children out of a page.
  const allowed = entries.filter(
    (entry) => !pathPolicyError(join(target.realPath, entry.name), target.workspaceRoot, policy, id),
  );
  const visible = allowed.slice(0, limit);
  const entryLines: Array<[string, unknown]> = [];
  let usedBytes = 0;
  let truncated = allowed.length > visible.length;
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
    status: "ok",
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
  const resolved = await resolveExistingTarget(
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
    before = await lstat(target.realPath);
  } catch {
    return executionError(
      context.callId,
      "not_found",
      "not_found",
      `Could not find ${target.errorPath}`,
      [["path", target.errorPath]],
    );
  }
  if (before.isDirectory()) {
    return renderDirectory(
      context.callId,
      target,
      context.policy,
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
      [["path", target.errorPath]],
    );
  }

  if (readArgs.includeContent === false) {
    return renderFileMetadata(context.callId, target, before.size);
  }

  // Bounded work: refuse to load a file far larger than what can be returned.
  // Metadata-only reads are not affected.
  if (before.size > context.policy.max_file_bytes) {
    return executionError(
      context.callId,
      "blocked",
      "size_limit",
      "The file exceeds the configured read size limit",
      [
        ["path", target.errorPath],
        ["bytes", before.size],
        ["max_bytes", context.policy.max_file_bytes],
        [
          "suggestion",
          "read metadata with includeContent false, or have the session raise tools.native.read.max_file_bytes",
        ],
      ],
    );
  }

  const identity = {device: before.dev, inode: before.ino};
  const verified = await readVerifiedRegularFile(target.realPath, identity);
  if (!verified) {
    return executionError(
      context.callId,
      "error",
      "changed_during_read",
      `The file changed while it was being read`,
      [["path", target.errorPath]],
    );
  }
  let after;
  try {
    after = await lstat(target.realPath);
  } catch {
    return executionError(
      context.callId,
      "error",
      "changed_during_read",
      `The file changed while it was being read`,
      [["path", target.errorPath]],
    );
  }
  if (!after.isFile() || !sameFileIdentity(after, identity)) {
    return executionError(
      context.callId,
      "error",
      "changed_during_read",
      `The file changed while it was being read`,
      [["path", target.errorPath]],
    );
  }
  const data = verified.data;

  // Default to the representation that decodes the content: UTF-8 text when it
  // is valid, base64 when it is not. An explicit contentEncoding is honoured,
  // and asking for utf8 on binary content stays an error with a suggestion.
  const requestedEncoding = normalizeEncoding(readArgs.contentEncoding);
  const text = decodeUtf8(data);
  if (requestedEncoding === "utf8" && text === undefined) {
    return executionError(
      context.callId,
      "unsupported_encoding",
      "binary_file",
      `Could not decode ${target.errorPath} as UTF-8`,
      [
        ["path", target.errorPath],
        ["suggestion", "omit contentEncoding to receive the content as base64"],
      ],
    );
  }
  const encoding: ContentEncoding = requestedEncoding ?? (text === undefined ? "base64" : "utf8");
  const encodingReason = requestedEncoding === undefined && encoding === "base64"
    ? "invalid_utf8" as const
    : undefined;

  try {
    return renderFileRange(
      context.callId,
      target,
      data,
      readArgs,
      context.policy,
      encoding,
      encodingReason,
    );
  } catch {
    return executionError(
      context.callId,
      "error",
      "internal_error",
      `The read operation failed`,
    );
  }
}
