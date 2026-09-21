import {lstat, mkdir, realpath} from "node:fs/promises";
import {basename, dirname, isAbsolute, relative, resolve, sep} from "node:path";

import type {NativePathPolicy} from "../../config.js";
import {executionError} from "../output.js";

interface FsError {
  code?: string;
}

export interface ResolvedTarget {
  workspaceRoot: string;
  realPath: string;
  displayPath: string;
  errorPath: string;
}

export interface ResolvedWriteTarget extends ResolvedTarget {
  existed: boolean;
  mode?: number;
}

function errorCode(error: unknown): string | undefined {
  return typeof error === "object" && error !== null && "code" in error
    ? (error as FsError).code
    : undefined;
}

export function withinRoot(root: string, target: string): boolean {
  if (root === sep) {
    return target.startsWith(sep);
  }
  return target === root || target.startsWith(`${root}${sep}`);
}

export function displayPath(root: string, target: string): string {
  if (!withinRoot(root, target)) {
    return target;
  }
  const value = relative(root, target).split(sep).join("/");
  return value.length > 0 ? value : ".";
}

export function errorPath(root: string, target: string): string {
  if (withinRoot(root, target)) {
    return displayPath(root, target);
  }
  const name = basename(target);
  return name.length > 0 ? `../${name}` : "..";
}

export function pathPolicyError(
  target: string,
  workspaceRoot: string,
  policy: NativePathPolicy,
  callId: string,
): string | undefined {
  const matches = policy.paths.filter((rule) => withinRoot(rule.path, target));
  if (matches.some((rule) => rule.action === "deny")) {
    return executionError(
      callId,
      "forbidden",
      "path_denied",
      "The requested path is denied by the native tool policy",
      [["path", errorPath(workspaceRoot, target)]],
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
    [["path", errorPath(workspaceRoot, target)]],
  );
}

async function workspaceRootOrError(
  workspaceRoot: string,
  callId: string,
): Promise<{root?: string; error?: string}> {
  try {
    return {root: await realpath(workspaceRoot)};
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
}

export async function resolveExistingTarget(
  requestedPath: string,
  workspaceRoot: string,
  policy: NativePathPolicy,
  callId: string,
): Promise<{target?: ResolvedTarget; error?: string}> {
  const workspace = await workspaceRootOrError(workspaceRoot, callId);
  if (workspace.error) {
    return {error: workspace.error};
  }
  const root = workspace.root!;
  const candidate = isAbsolute(requestedPath)
    ? resolve(requestedPath)
    : resolve(root, requestedPath);
  const candidatePolicyError = pathPolicyError(candidate, root, policy, callId);
  if (candidatePolicyError) {
    return {error: candidatePolicyError};
  }
  const safeCandidatePath = errorPath(root, candidate);

  let target: string;
  try {
    target = await realpath(candidate);
  } catch (error) {
    const code = errorCode(error);
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

  const targetPolicyError = pathPolicyError(target, root, policy, callId);
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
      workspaceRoot: root,
      realPath: target,
      displayPath: displayPath(root, target),
      errorPath: errorPath(root, target),
    },
  };
}

async function nearestExistingParent(candidate: string): Promise<{
  path: string;
  realPath: string;
} | {errorCode: string}> {
  let current = dirname(candidate);
  for (;;) {
    try {
      return {path: current, realPath: await realpath(current)};
    } catch (error) {
      const code = errorCode(error);
      if (code !== "ENOENT" && code !== "ENOTDIR") {
        return {errorCode: code ?? "parent_resolution_failed"};
      }
      const parent = dirname(current);
      if (parent === current) {
        return {errorCode: "parent_not_found"};
      }
      current = parent;
    }
  }
}

function writePathError(
  callId: string,
  root: string,
  candidate: string,
  errorCodeValue: string,
  summary: string,
  status: "forbidden" | "not_found" | "error" = "error",
): string {
  return executionError(
    callId,
    status,
    errorCodeValue,
    summary,
    [["path", errorPath(root, candidate)]],
  );
}

async function ensureWriteParent(
  candidate: string,
  root: string,
  policy: NativePathPolicy,
  callId: string,
): Promise<string | undefined> {
  const parent = dirname(candidate);
  const nearest = await nearestExistingParent(candidate);
  if ("errorCode" in nearest) {
    return writePathError(
      callId,
      root,
      candidate,
      nearest.errorCode,
      "The parent directory could not be resolved",
    );
  }
  if (nearest.path !== nearest.realPath) {
    return writePathError(
      callId,
      root,
      candidate,
      "symlink_forbidden",
      "The write path contains a symlinked directory",
      "forbidden",
    );
  }
  if (withinRoot(root, candidate)) {
    const nearestPolicyError = pathPolicyError(nearest.realPath, root, policy, callId);
    if (nearestPolicyError) {
      return nearestPolicyError;
    }
  }

  try {
    await mkdir(parent, {recursive: true});
  } catch (error) {
    const code = errorCode(error);
    return writePathError(
      callId,
      root,
      candidate,
      code === "EACCES" || code === "EPERM" ? "parent_forbidden" : "directory_create_failed",
      "Could not create the parent directory",
      code === "EACCES" || code === "EPERM" ? "forbidden" : "error",
    );
  }

  let realParent: string;
  try {
    realParent = await realpath(parent);
  } catch {
    return writePathError(
      callId,
      root,
      candidate,
      "parent_not_found",
      "The parent directory could not be resolved",
      "not_found",
    );
  }
  if (realParent !== parent) {
    return writePathError(
      callId,
      root,
      candidate,
      "symlink_forbidden",
      "The write path contains a symlinked directory",
      "forbidden",
    );
  }
  return withinRoot(root, candidate)
    ? pathPolicyError(realParent, root, policy, callId)
    : undefined;
}

export async function resolveWriteTarget(
  requestedPath: string,
  workspaceRoot: string,
  policy: NativePathPolicy,
  callId: string,
): Promise<{target?: ResolvedWriteTarget; error?: string}> {
  const workspace = await workspaceRootOrError(workspaceRoot, callId);
  if (workspace.error) {
    return {error: workspace.error};
  }
  const root = workspace.root!;
  const candidate = isAbsolute(requestedPath)
    ? resolve(requestedPath)
    : resolve(root, requestedPath);
  const candidatePolicyError = pathPolicyError(candidate, root, policy, callId);
  if (candidatePolicyError) {
    return {error: candidatePolicyError};
  }

  const parentError = await ensureWriteParent(candidate, root, policy, callId);
  if (parentError) {
    return {error: parentError};
  }

  let existing;
  try {
    existing = await lstat(candidate);
  } catch (error) {
    const code = errorCode(error);
    if (code !== "ENOENT") {
      return {
        error: writePathError(
          callId,
          root,
          candidate,
          code === "EACCES" || code === "EPERM" ? "path_forbidden" : "path_stat_failed",
          "The write target could not be inspected",
          code === "EACCES" || code === "EPERM" ? "forbidden" : "error",
        ),
      };
    }
  }

  const safeCandidatePath = errorPath(root, candidate);
  if (!existing) {
    return {
      target: {
        workspaceRoot: root,
        realPath: candidate,
        displayPath: displayPath(root, candidate),
        errorPath: safeCandidatePath,
        existed: false,
      },
    };
  }

  if (existing.isSymbolicLink()) {
    return {
      error: writePathError(
        callId,
        root,
        candidate,
        "symlink_forbidden",
        "The write target is a symlink",
        "forbidden",
      ),
    };
  }
  if (existing.isDirectory()) {
    return {
      error: writePathError(
        callId,
        root,
        candidate,
        "target_is_directory",
        "The write target is a directory",
      ),
    };
  }
  if (!existing.isFile()) {
    return {
      error: writePathError(
        callId,
        root,
        candidate,
        "unsupported_file_type",
        "The write target is not a regular file",
      ),
    };
  }

  let target: string;
  try {
    target = await realpath(candidate);
  } catch {
    return {
      error: writePathError(
        callId,
        root,
        candidate,
        "path_resolution_failed",
        "The write target could not be resolved",
      ),
    };
  }
  if (target !== candidate) {
    return {
      error: writePathError(
        callId,
        root,
        candidate,
        "symlink_forbidden",
        "The write target is a symlink",
        "forbidden",
      ),
    };
  }
  const targetPolicyError = pathPolicyError(target, root, policy, callId);
  if (targetPolicyError) {
    return {error: targetPolicyError};
  }
  return {
    target: {
      workspaceRoot: root,
      realPath: target,
      displayPath: displayPath(root, target),
      errorPath: safeCandidatePath,
      existed: true,
      mode: existing.mode & 0o7777,
    },
  };
}
