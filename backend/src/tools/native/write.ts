import type {NativeWriteToolSettings} from "../../config.js";
import type {JsonRecord} from "../../persistence.js";
import {executionError, renderToolResult} from "../output.js";
import type {ToolDefinition, ToolExecutionContext} from "../types.js";
import {commitFile, errorCode, hasSameContent, pathLocks} from "./mutate.js";
import {
  displayPath,
  inspectMutateTarget,
  prepareMutateParent,
  resolveMutatePath,
} from "./path.js";
import type {ResolvedMutatePath} from "./path.js";
import {normalizeEncoding} from "./text.js";

export const NATIVE_WRITE_ID = "native.write";

export const nativeWriteDefinition: ToolDefinition = {
  id: NATIVE_WRITE_ID,
  source: "native",
  name: "write",
  description:
    "Write file content, creating parent directories as needed. Identical content is left unchanged.",
  inputSchema: {
    type: "object",
    properties: {
      path: {
        type: "string",
        minLength: 1,
        description: "Path relative to cwd or an absolute path.",
      },
      content: {
        type: "string",
      },
      contentEncoding: {
        type: "string",
        enum: ["utf8", "base64"],
        description: "Encoding of content. Defaults to utf8.",
      },
    },
    required: ["path", "content"],
    additionalProperties: false,
  },
};

type ContentEncoding = "utf8" | "base64";

type WriteAction = "created" | "updated" | "unchanged";

interface WriteArgs {
  path: string;
  content: string;
  contentEncoding?: ContentEncoding;
}

interface WriteContext extends ToolExecutionContext {
  policy: NativeWriteToolSettings;
}

function decodeBase64(value: string): Buffer | undefined {
  const normalized = value.replace(/\s+/g, "");
  if (normalized.length === 0) {
    return Buffer.alloc(0);
  }
  if (!/^[A-Za-z0-9+/]*={0,2}$/.test(normalized) || normalized.length % 4 === 1) {
    return undefined;
  }
  const data = Buffer.from(normalized, "base64");
  const unpadded = normalized.replace(/=+$/, "");
  const canonical = data.toString("base64").replace(/=+$/, "");
  return canonical === unpadded ? data : undefined;
}

function contentBuffer(
  content: string,
  encoding: ContentEncoding,
): {data?: Buffer; error?: string} {
  if (encoding === "utf8") {
    return {data: Buffer.from(content, "utf8")};
  }
  const data = decodeBase64(content);
  if (!data) {
    return {
      error: "The content is not valid base64",
    };
  }
  return {data};
}

function actionSummary(action: WriteAction, targetPath: string): string {
  if (action === "created") {
    return `Created ${targetPath}`;
  }
  if (action === "updated") {
    return `Updated ${targetPath}`;
  }
  return `Unchanged ${targetPath}`;
}

function failure(
  callId: string,
  errorPath: string,
  errorCodeValue: string,
  summary: string,
  extra: Array<[string, unknown]> = [],
): string {
  return executionError(
    callId,
    "error",
    errorCodeValue,
    summary,
    [["path", errorPath], ...extra],
  );
}

async function applyWrite(
  path: ResolvedMutatePath,
  data: Buffer,
  writeArgs: WriteArgs,
  context: WriteContext,
  policy: NativeWriteToolSettings,
): Promise<string> {
  const parentError = await prepareMutateParent(path, policy, context.callId);
  if (parentError) {
    return parentError;
  }

  const inspected = await inspectMutateTarget(path, policy, context.callId);
  if (inspected.error) {
    return inspected.error;
  }
  const target = inspected.target!;

  const information = (
    action: WriteAction,
    backupPath?: string,
  ): Array<[string, unknown]> => [
    ["path", target.displayPath],
    ["action", action],
    ["bytes", data.length],
    ...(backupPath
      ? [[
          "backup_path",
          displayPath(target.workspaceRoot, backupPath),
        ] as [string, unknown]]
      : []),
  ];

  if (target.existed && (await hasSameContent(target, data))) {
    return renderToolResult({
      id: context.callId,
      status: "ok",
      summary: actionSummary("unchanged", target.displayPath),
      information: information("unchanged"),
    });
  }

  const action: WriteAction = target.existed ? "updated" : "created";
  const commit = await commitFile({
    target,
    settings: policy,
    data,
    backup: true,
  });

  if (commit.error === "backup_failed") {
    return failure(
      context.callId,
      target.errorPath,
      "backup_failed",
      "Could not create a backup before writing",
    );
  }
  if (commit.error === "target_changed") {
    return failure(
      context.callId,
      target.errorPath,
      "changed_during_write",
      "The target changed while it was being written",
    );
  }
  if (commit.error === "write_failed") {
    return failure(
      context.callId,
      target.errorPath,
      "write_failed",
      "Could not write the file",
      [
        ...(commit.systemError
          ? [["system_error", commit.systemError] as [string, unknown]]
          : []),
        ...(commit.backupPath
          ? [[
              "backup_path",
              displayPath(target.workspaceRoot, commit.backupPath),
            ] as [string, unknown]]
          : []),
      ],
    );
  }

  return renderToolResult({
    id: context.callId,
    status: "ok",
    summary: actionSummary(action, target.displayPath),
    information: information(action, commit.backupPath),
  });
}

export async function executeNativeWrite(
  args: JsonRecord,
  context: WriteContext,
): Promise<string> {
  const writeArgs = args as unknown as WriteArgs;
  const policy = context.policy;

  // Path resolution comes first and has no filesystem side effect, so every
  // error reports the canonical path and an invalid request never creates
  // directories, backups, or temporary files.
  const resolved = await resolveMutatePath(
    writeArgs.path,
    context.workspaceRoot,
    policy,
    context.callId,
  );
  if (resolved.error) {
    return resolved.error;
  }
  const path = resolved.path!;

  const encoding = normalizeEncoding(writeArgs.contentEncoding) ?? "utf8";
  const content = contentBuffer(writeArgs.content, encoding);
  if (content.error) {
    return failure(context.callId, path.errorPath, "invalid_base64", content.error, [
      [
        "suggestion",
        "base64 content must use the standard alphabet and padding; use contentEncoding \"utf8\" for text",
      ],
    ]);
  }
  const data = content.data!;
  if (data.length > policy.max_bytes) {
    return executionError(
      context.callId,
      "blocked",
      "size_limit",
      "The write exceeds the configured byte limit",
      [
        ["path", path.errorPath],
        ["bytes", data.length],
        ["max_bytes", policy.max_bytes],
      ],
    );
  }

  return pathLocks.runExclusive(path.candidate, () =>
    applyWrite(path, data, writeArgs, context, policy),
  );
}
