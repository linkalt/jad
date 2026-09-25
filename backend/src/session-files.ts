/**
 * The file list behind the web UI's `@` menu.
 *
 * Two rules shape it. The walk never leaves the session's cwd, and every path
 * it is about to offer is checked against the same policy the native `read`
 * tool applies — so a menu entry is always something the agent itself would be
 * allowed to open, and the menu cannot promise what the tool will refuse.
 * Names only: this never reads file contents.
 */
import { readdir } from "node:fs/promises";
import { join } from "node:path";

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
  /** cwd-relative POSIX paths: shallow files first, alphabetical inside a directory. */
  files: string[];
  /** True when a cap stopped the walk, so the list is known to be incomplete. */
  truncated: boolean;
}

/**
 * Walks `root` breadth-first and returns the files a `@path` reference may
 * name.
 *
 * Breadth-first on purpose: when a cap stops the walk, the list holds the
 * shallowest files — the ones a prompt refers to most — instead of whichever
 * deep branch happened to sort first.
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

  for (let depth = 1; depth <= settings.max_depth && level.length > 0; depth += 1) {
    const nextLevel: string[] = [];
    for (const directory of level) {
      if (stopped) {
        break;
      }
      let entries;
      try {
        entries = await readdir(directory === "" ? root : join(root, directory), {
          withFileTypes: true,
        });
      } catch {
        // A directory can vanish between listing and walking, or be unreadable.
        // One bad branch must not fail the whole menu.
        continue;
      }
      entries.sort((left, right) => left.name.localeCompare(right.name));
      for (const entry of entries) {
        if (visited >= settings.max_entries || files.length >= settings.max_results) {
          truncated = true;
          stopped = true;
          break;
        }
        if (!settings.include_hidden && entry.name.startsWith(".")) {
          continue;
        }
        // Never followed: a symlink can leave the workspace or close a cycle.
        if (entry.isSymbolicLink()) {
          continue;
        }
        const relativePath = directory === "" ? entry.name : `${directory}/${entry.name}`;
        if (entry.isDirectory()) {
          visited += 1;
          if (!ignored.has(entry.name)) {
            nextLevel.push(relativePath);
          }
          continue;
        }
        if (!entry.isFile()) {
          // Sockets, fifos, devices: nothing an `@path` reference can name.
          continue;
        }
        visited += 1;
        if (pathPolicyError(join(root, relativePath), root, policy, POLICY_CALL_ID) !== undefined) {
          continue;
        }
        files.push(relativePath);
      }
    }
    level = nextLevel;
  }

  return { files, truncated };
}
