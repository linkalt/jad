import {constants as fsConstants} from "node:fs";
import {chmod, copyFile, rename, rm, writeFile} from "node:fs/promises";
import {randomBytes} from "node:crypto";

import type {NativeWriteToolSettings} from "../../config.js";
import type {JsonRecord} from "../../persistence.js";
import {executionError, renderToolResult} from "../output.js";
import type {ToolDefinition, ToolExecutionContext} from "../types.js";
import {displayPath, resolveWriteTarget} from "./path.js";

export const NATIVE_WRITE_ID = "native.write";

export const nativeWriteDefinition: ToolDefinition = {
  id: NATIVE_WRITE_ID,
  source: "native",
  name: "write",
  description: "Write file content, creating parent directories as needed.",
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
      },
      backup: {
        type: "boolean",
        description: "Back up an existing file before replacing it. Defaults to true.",
      },
    },
    required: ["path", "content"],
    additionalProperties: false,
  },
};

type ContentEncoding = "utf8" | "base64";

interface WriteArgs {
  path: string;
  content: string;
  contentEncoding?: ContentEncoding;
  backup?: boolean;
}

interface WriteContext extends ToolExecutionContext {
  policy: NativeWriteToolSettings;
}

function errorCode(error: unknown): string | undefined {
  return typeof error === "object" && error !== null && "code" in error
    ? String((error as {code?: unknown}).code)
    : undefined;
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

async function createBackup(targetPath: string, mode: number | undefined): Promise<string> {
  for (let attempt = 0; attempt < 5; attempt += 1) {
    const backupPath = `${targetPath}.bak-${Date.now()}-${randomBytes(8).toString("hex")}`;
    try {
      await copyFile(targetPath, backupPath, fsConstants.COPYFILE_EXCL);
      if (mode !== undefined) {
        await chmod(backupPath, mode);
      }
      return backupPath;
    } catch (error) {
      if (errorCode(error) !== "EEXIST" || attempt === 4) {
        throw error;
      }
    }
  }
  throw new Error("could not create a unique backup path");
}

async function writeAtomically(
  targetPath: string,
  data: Buffer,
  mode: number | undefined,
): Promise<void> {
  const temporaryPath = `${targetPath}.tmp-${randomBytes(8).toString("hex")}`;
  try {
    await writeFile(temporaryPath, data, {
      flag: "wx",
      mode: mode ?? 0o666,
    });
    if (mode !== undefined) {
      await chmod(temporaryPath, mode);
    }
    await rename(temporaryPath, targetPath);
  } catch (error) {
    await rm(temporaryPath, {force: true}).catch(() => undefined);
    throw error;
  }
}

function failure(
  callId: string,
  targetPath: string,
  errorCodeValue: string,
  summary: string,
  extra: Array<[string, unknown]> = [],
): string {
  return executionError(
    callId,
    "error",
    errorCodeValue,
    summary,
    [["path", targetPath], ...extra],
  );
}

export async function executeNativeWrite(
  args: JsonRecord,
  context: WriteContext,
): Promise<string> {
  const writeArgs = args as unknown as WriteArgs;
  const encoding = writeArgs.contentEncoding ?? "utf8";
  const content = contentBuffer(writeArgs.content, encoding);
  if (content.error) {
    return failure(context.callId, writeArgs.path, "invalid_base64", content.error);
  }
  const data = content.data!;
  if (data.length > context.policy.max_bytes) {
    return executionError(
      context.callId,
      "blocked",
      "size_limit",
      "The write exceeds the configured byte limit",
      [
        ["path", writeArgs.path],
        ["size_bytes", data.length],
        ["max_bytes", context.policy.max_bytes],
      ],
    );
  }

  const resolved = await resolveWriteTarget(
    writeArgs.path,
    context.workspaceRoot,
    context.policy,
    context.callId,
  );
  if (resolved.error) {
    return resolved.error;
  }
  const target = resolved.target!;
  const shouldBackup = writeArgs.backup ?? true;
  let backupPath: string | undefined;

  if (shouldBackup && target.existed) {
    try {
      backupPath = await createBackup(target.realPath, target.mode);
    } catch {
      return failure(
        context.callId,
        target.displayPath,
        "backup_failed",
        "Could not create a backup before writing",
      );
    }
  }

  try {
    await writeAtomically(target.realPath, data, target.mode);
  } catch (error) {
    return failure(
      context.callId,
      target.displayPath,
      "write_failed",
      "Could not write the file",
      [
        ...(errorCode(error) ? [["system_error", errorCode(error)] as [string, unknown]] : []),
        ...(backupPath
          ? [["backup_path", displayPath(target.workspaceRoot, backupPath)] as [string, unknown]]
          : []),
      ],
    );
  }

  return renderToolResult({
    id: context.callId,
    status: "ok",
    summary: target.existed
      ? `Wrote ${target.displayPath}`
      : `Created ${target.displayPath}`,
    information: [
      ["path", target.displayPath],
      ["kind", "file"],
      ["size_bytes", data.length],
      ["encoding", encoding === "utf8" ? "utf-8" : "base64"],
      ["created", !target.existed],
      ...(backupPath
        ? [["backup_path", displayPath(target.workspaceRoot, backupPath)] as [string, unknown]]
        : []),
    ],
  });
}
