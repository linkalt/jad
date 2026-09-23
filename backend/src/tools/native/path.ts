import {constants as fsConstants, type Stats} from "node:fs";
import {lstat, mkdir, open, realpath, stat} from "node:fs/promises";
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

/** Shared by both mutating tools; "write" in these helpers means "mutation". */
export interface ResolvedMutatePath {
  workspaceRoot: string;
  /** Canonical absolute target; no filesystem side effect has happened yet. */
  candidate: string;
  displayPath: string;
  errorPath: string;
}

export interface FileIdentity {
  device: number;
  inode: number;
}

export interface MutateTarget extends ResolvedTarget {
  existed: boolean;
  mode?: number;
  size?: number;
  identity?: FileIdentity;
}

export interface VerifiedFile {
  data: Buffer;
  stats: Stats;
}

export function sameFileIdentity(
  left: Pick<Stats, "dev" | "ino">,
  right: FileIdentity,
): boolean {
  return left.dev === right.device && left.ino === right.inode;
}

/**
 * Opens one regular file without following a final symlink, then verifies that
 * the descriptor is the file inspected by the caller. File reads use the
 * descriptor thereafter, closing the check-to-use window for the final path.
 */
export async function readVerifiedRegularFile(
  path: string,
  expected: FileIdentity,
): Promise<VerifiedFile | undefined> {
  let handle;
  try {
    handle = await open(path, fsConstants.O_RDONLY | fsConstants.O_NOFOLLOW);
    const stats = await handle.stat();
    if (!stats.isFile() || !sameFileIdentity(stats, expected)) {
      return undefined;
    }
    const data = await handle.readFile();
    const after = await handle.stat();
    if (!sameFileIdentity(after, expected) || after.size !== data.length) {
      return undefined;
    }
    return {data, stats: after};
  } catch {
    return undefined;
  } finally {
    await handle?.close().catch(() => undefined);
  }
}

function errorCode(error: unknown): string | undefined {
  return typeof error === "object" && error !== null && "code" in error
    ? (error as FsError).code
    : undefined;
}

async function statOrUndefined(path: string): Promise<Stats | undefined> {
  try {
    return await lstat(path);
  } catch {
    return undefined;
  }
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
  if (
    policy.unrestricted_paths ||
    withinRoot(workspaceRoot, target) ||
    matches.some((rule) => rule.action === "allow")
  ) {
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

/**
 * True when a resolved path may be used even though its spelling differs from
 * the requested one — a symlinked directory in the chain, or a symlinked final
 * component. The path policy decides: an unrestricted policy, a path inside the
 * workspace, or an allow rule covering the resolved path all permit it, while a
 * deny rule (or an outside path with no rule) keeps it forbidden.
 */
function resolvedPathAllowed(
  target: string,
  root: string,
  policy: NativePathPolicy,
  callId: string,
): boolean {
  return pathPolicyError(target, root, policy, callId) === undefined;
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

  if (target !== candidate && !withinRoot(root, target) && !resolvedPathAllowed(target, root, policy, callId)) {
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

  if (target !== candidate && !resolvedPathAllowed(target, root, policy, callId)) {
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

export async function prepareMutateParent(
  path: ResolvedMutatePath,
  policy: NativePathPolicy,
  callId: string,
): Promise<string | undefined> {
  const {workspaceRoot: root, candidate} = path;
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
      "The path contains a symlinked directory",
      "forbidden",
    );
  }
  const nearestStat = await statOrUndefined(nearest.realPath);
  if (!nearestStat?.isDirectory()) {
    return writePathError(
      callId,
      root,
      candidate,
      "parent_not_directory",
      "The parent directory is not a directory",
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
    const isNotDirectory = code === "ENOTDIR" || code === "EEXIST";
    return writePathError(
      callId,
      root,
      candidate,
      isNotDirectory
        ? "parent_not_directory"
        : code === "EACCES" || code === "EPERM"
          ? "parent_forbidden"
          : "directory_create_failed",
      isNotDirectory
        ? "The parent directory is not a directory"
        : "Could not create the parent directory",
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
  if (realParent !== parent && !resolvedPathAllowed(realParent, root, policy, callId)) {
    return writePathError(
      callId,
      root,
      candidate,
      "symlink_forbidden",
      "The path contains a symlinked directory",
      "forbidden",
    );
  }
  return withinRoot(root, candidate)
    ? pathPolicyError(realParent, root, policy, callId)
    : undefined;
}

export async function resolveMutatePath(
  requestedPath: string,
  workspaceRoot: string,
  policy: NativePathPolicy,
  callId: string,
): Promise<{path?: ResolvedMutatePath; error?: string}> {
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
  if (candidate === root) {
    return {
      error: writePathError(
        callId,
        root,
        candidate,
        "target_is_directory",
        "The target is a directory",
      ),
    };
  }
  if (requestedPath.endsWith(sep)) {
    const existing = await statOrUndefined(candidate);
    if (!existing?.isDirectory()) {
      return {
        error: writePathError(
          callId,
          root,
          candidate,
          "invalid_path",
          "The path ends with a directory separator but is not a directory",
        ),
      };
    }
  }
  return {
    path: {
      workspaceRoot: root,
      candidate,
      displayPath: displayPath(root, candidate),
      errorPath: errorPath(root, candidate),
    },
  };
}

/**
 * Inspects the resolved target. Runs after the parent chain exists so a missing
 * target is reported as a creatable file rather than an error.
 */
export async function inspectMutateTarget(
  path: ResolvedMutatePath,
  policy: NativePathPolicy,
  callId: string,
): Promise<{target?: MutateTarget; error?: string}> {
  const {
    workspaceRoot: root,
    candidate,
    displayPath: candidateDisplayPath,
    errorPath: candidateErrorPath,
  } = path;

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
          "The target could not be inspected",
          code === "EACCES" || code === "EPERM" ? "forbidden" : "error",
        ),
      };
    }
  }

  if (!existing) {
    return {
      target: {
        workspaceRoot: root,
        realPath: candidate,
        displayPath: candidateDisplayPath,
        errorPath: candidateErrorPath,
        existed: false,
      },
    };
  }

  if (existing.isSymbolicLink()) {
    // A symlinked target is allowed only when the policy covers the file it
    // resolves to. The mutation then targets that resolved file, and
    // `commitFile` still re-checks its identity before replacing it.
    const linked = await realpath(candidate).catch(() => undefined);
    if (linked === undefined || !resolvedPathAllowed(linked, root, policy, callId)) {
      return {
        error: writePathError(
          callId,
          root,
          candidate,
          "symlink_forbidden",
          "The target is a symlink",
          "forbidden",
        ),
      };
    }
    const linkedPolicyError = pathPolicyError(linked, root, policy, callId);
    if (linkedPolicyError) {
      return {error: linkedPolicyError};
    }
    let linkedStats: Stats;
    try {
      linkedStats = await stat(linked);
    } catch {
      return {
        error: writePathError(
          callId,
          root,
          candidate,
          "path_resolution_failed",
          "The target could not be resolved",
        ),
      };
    }
    if (linkedStats.isDirectory()) {
      return {
        error: writePathError(
          callId,
          root,
          candidate,
          "target_is_directory",
          "The target is a directory",
        ),
      };
    }
    if (!linkedStats.isFile()) {
      return {
        error: writePathError(
          callId,
          root,
          candidate,
          "unsupported_file_type",
          "The target is not a regular file",
        ),
      };
    }
    return {
      target: {
        workspaceRoot: root,
        realPath: linked,
        displayPath: displayPath(root, linked),
        errorPath: candidateErrorPath,
        existed: true,
        mode: linkedStats.mode & 0o7777,
        size: linkedStats.size,
        identity: {device: linkedStats.dev, inode: linkedStats.ino},
      },
    };
  }
  if (existing.isDirectory()) {
    return {
      error: writePathError(
        callId,
        root,
        candidate,
        "target_is_directory",
        "The target is a directory",
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
        "The target is not a regular file",
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
        "The target could not be resolved",
      ),
    };
  }
  if (target !== candidate && !resolvedPathAllowed(target, root, policy, callId)) {
    return {
      error: writePathError(
        callId,
        root,
        candidate,
        "symlink_forbidden",
        "The target is a symlink",
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
      errorPath: candidateErrorPath,
      existed: true,
      mode: existing.mode & 0o7777,
      size: existing.size,
      identity: {device: existing.dev, inode: existing.ino},
    },
  };
}
