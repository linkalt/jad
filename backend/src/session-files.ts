/**
 * The file list behind the web UI's `@` menu.
 *
 * The walk follows ordinary directories only, and every returned path is
 * checked against both the session cwd and the native `read` path policy.
 * Directory handles are read incrementally so a large flat directory cannot
 * allocate an unbounded Dirent array. Names only: this never reads file contents.
 */
import type { Dirent } from "node:fs";
import { lstat, opendir, realpath } from "node:fs/promises";
import { isAbsolute, join, relative, sep } from "node:path";

import type { NativePathPolicy, SessionFileListingSettings } from "./config.js";
import { pathPolicyError } from "./tools/native/path.js";

/**
 * The call id the policy renders into a denial message. No tool call stands
 * behind this walk, and the rendered error is discarded: the policy helper is
 * used as a yes/no question, the same way native `read` uses it to filter a
 * directory listing.
 */
const POLICY_CALL_ID = "session-files";

export interface SessionFileListing {
  /** cwd-relative POSIX paths: shallow files first, sorted within each inspected directory batch. */
  files: string[];
  /** True when a cap, inaccessible branch, or unsafe path left part of the walk unexamined. */
  truncated: boolean;
}

interface DirectoryEntries {
  entries: Dirent[];
  /** One lookahead entry exists beyond the bounded batch. */
  hasMore: boolean;
}

/**
 * Resolves an absolute path without allowing `..` or a sibling with a common
 * textual prefix to count as inside the workspace.
 */
function isWithinRoot(root: string, target: string): boolean {
  const relativePath = relative(root, target);
  return relativePath === "" || (
    relativePath !== ".." &&
    !relativePath.startsWith(`..${sep}`) &&
    !isAbsolute(relativePath)
  );
}

/**
 * Read at most `limit` entries, plus one lookahead to distinguish an exact fit
 * from a capped directory. Sorting only this bounded batch keeps memory bounded;
 * for oversized directories, the sampled entries depend on filesystem order.
 */
async function readDirectoryEntries(path: string, limit: number): Promise<DirectoryEntries> {
  const directory = await opendir(path);
  const entries: Dirent[] = [];
  let hasMore = false;
  try {
    while (true) {
      const entry = await directory.read();
      if (entry === null) {
        break;
      }
      if (entries.length >= limit) {
        hasMore = true;
        break;
      }
      entries.push(entry);
    }
  } finally {
    await directory.close().catch(() => undefined);
  }
  entries.sort((left, right) => left.name.localeCompare(right.name));
  return { entries, hasMore };
}

/**
 * Best-effort guard for a directory being replaced by a symlink while the walk
 * is in progress. Node's path-based directory APIs do not provide a fully
 * race-free no-follow traversal, so callers check both before and after reading.
 */
async function resolveContainedDirectory(
  path: string,
  workspaceRoot: string,
  isWorkspaceRoot: boolean,
): Promise<string | undefined> {
  try {
    if (!isWorkspaceRoot) {
      const before = await lstat(path);
      if (!before.isDirectory() || before.isSymbolicLink()) {
        return undefined;
      }
    }
    const resolved = await realpath(path);
    if (!isWithinRoot(workspaceRoot, resolved)) {
      return undefined;
    }
    if (!isWorkspaceRoot) {
      const after = await lstat(path);
      if (!after.isDirectory() || after.isSymbolicLink()) {
        return undefined;
      }
    }
    return resolved;
  } catch {
    return undefined;
  }
}

/** Revalidates a listed regular file after the directory has been enumerated. */
async function fileIsContainedRegularFile(path: string, workspaceRoot: string): Promise<boolean> {
  try {
    const before = await lstat(path);
    if (!before.isFile() || before.isSymbolicLink()) {
      return false;
    }
    const resolved = await realpath(path);
    if (!isWithinRoot(workspaceRoot, resolved)) {
      return false;
    }
    const after = await lstat(path);
    if (!after.isFile() || after.isSymbolicLink()) {
      return false;
    }
    const resolvedAfter = await realpath(path);
    return resolvedAfter === resolved && isWithinRoot(workspaceRoot, resolvedAfter);
  } catch {
    return false;
  }
}

/**
 * Walks `root` breadth-first and returns the files a `@path` reference may
 * name. A single entry may be read beyond `max_entries` as a lookahead so the
 * response can accurately report truncation.
 */
export async function collectSessionFiles(
  root: string,
  settings: SessionFileListingSettings,
  policy: NativePathPolicy,
): Promise<SessionFileListing> {
  const files: string[] = [];
  const ignored = new Set(settings.ignore_directories);
  let truncated = false;
  let stopped = false;
  let visited = 0;
  let level = [""];

  let workspaceRoot: string;
  try {
    workspaceRoot = await realpath(root);
  } catch {
    // A missing cwd is an empty listing rather than a failed menu request.
    return { files, truncated };
  }

  for (let depth = 1; depth <= settings.max_depth && level.length > 0; depth += 1) {
    const nextLevel: string[] = [];
    for (const directory of level) {
      if (stopped) {
        break;
      }
      const directoryPath = directory === "" ? root : join(root, directory);
      const resolvedBefore = await resolveContainedDirectory(
        directoryPath,
        workspaceRoot,
        directory === "",
      );
      if (resolvedBefore === undefined) {
        truncated = true;
        continue;
      }

      const remainingEntries = Math.max(0, settings.max_entries - visited);
      let directoryEntries: DirectoryEntries;
      try {
        directoryEntries = await readDirectoryEntries(directoryPath, remainingEntries);
      } catch {
        // A directory can vanish between listing and walking, or be unreadable.
        // One bad branch must not fail the whole menu, but the result is partial.
        truncated = true;
        continue;
      }
      const resolvedAfter = await resolveContainedDirectory(
        directoryPath,
        workspaceRoot,
        directory === "",
      );
      if (resolvedAfter === undefined || resolvedAfter !== resolvedBefore) {
        // Do not return names read while this path may have pointed outside cwd.
        truncated = true;
        continue;
      }

      visited += directoryEntries.entries.length + (directoryEntries.hasMore ? 1 : 0);
      for (const entry of directoryEntries.entries) {
        if (files.length >= settings.max_results) {
          truncated = true;
          stopped = true;
          break;
        }
        if (!settings.include_hidden && entry.name.startsWith(".")) {
          continue;
        }
        // Never follow a symlink entry, even when its target is inside cwd.
        if (entry.isSymbolicLink()) {
          continue;
        }
        const relativePath = directory === "" ? entry.name : `${directory}/${entry.name}`;
        if (entry.isDirectory()) {
          if (!ignored.has(entry.name)) {
            nextLevel.push(relativePath);
          }
          continue;
        }
        if (!entry.isFile()) {
          // Sockets, fifos, devices: nothing an `@path` reference can name.
          continue;
        }

        const filePath = join(root, relativePath);
        if (pathPolicyError(filePath, root, policy, POLICY_CALL_ID) !== undefined) {
          continue;
        }
        if (!(await fileIsContainedRegularFile(filePath, workspaceRoot))) {
          // A file can disappear, change type, or resolve outside cwd after
          // readdir; don't return a path whose current target could not verify.
          truncated = true;
          continue;
        }
        files.push(relativePath);
      }

      if (directoryEntries.hasMore) {
        truncated = true;
        stopped = true;
      }
    }
    level = nextLevel;
  }

  // A depth cap (including zero) can leave directories that were never walked.
  if (!stopped && level.length > 0) {
    truncated = true;
  }

  return { files, truncated };
}
