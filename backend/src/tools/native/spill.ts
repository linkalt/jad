/**
 * @file spill.ts
 * @input A spill storage directory or legacy path template, the call's identities, and a retention window
 * @output Exports path-safe filename encoding, legacy template helpers, directory preparation, and retention pruning
 * @position Per-stream full-output files behind the native bash tool
 *
 * The middle of a truncated stream exists nowhere else — the collector dropped it
 * and the model only sees the head and the tail — so the complete bytes are teed
 * to a file while they are read. Generated filenames are fixed and path-safe;
 * legacy locations come from a template validated at configuration time. Pruning
 * only ever removes files matching those generated or legacy names, because the
 * directory usually belongs to a user's project.
 */

import {createHash} from "node:crypto";
import {mkdir, readdir, stat, unlink} from "node:fs/promises";
import {basename, dirname} from "node:path";

export type SpillStream = "stdout" | "stderr";

/** Values a spill template may interpolate. */
export interface SpillVars {
  /** Session working directory: the root every native tool can read. */
  cwd: string;
  session_dir: string;
  session_name: string;
  call_id: string;
  stream: SpillStream;
}

export const SPILL_PLACEHOLDERS = [
  "cwd",
  "session_dir",
  "session_name",
  "call_id",
  "stream",
] as const;

export type SpillPlaceholder = (typeof SPILL_PLACEHOLDERS)[number];

/** Keeps generated filenames path-safe and bounded even for unusual call IDs. */
export function encodeSpillCallId(callId: string): string {
  let encoded: string;
  try {
    encoded = encodeURIComponent(callId);
  } catch {
    encoded = Buffer.from(callId, "utf8").toString("base64url");
  }
  if (encoded.length <= 180) return encoded || "empty";
  const digest = createHash("sha256").update(callId).digest("hex").slice(0, 16);
  return `${encoded.slice(0, 120)}-${digest}`;
}

const PLACEHOLDER_PATTERN = /\{([a-z_]+)\}/g;

/** Every placeholder name used by a template, in order of appearance. */
export function spillPlaceholders(template: string): string[] {
  const found: string[] = [];
  for (const match of template.matchAll(PLACEHOLDER_PATTERN)) {
    const name = match[1]!;
    if (!found.includes(name)) {
      found.push(name);
    }
  }
  return found;
}

export function resolveSpillPath(template: string, vars: SpillVars): string {
  return template.replace(PLACEHOLDER_PATTERN, (whole, name: string) => {
    const value = (vars as unknown as Record<string, string | undefined>)[name];
    return value === undefined ? whole : value;
  });
}

/**
 * Matcher for files this template could have written, used by pruning. The
 * per-call and per-stream placeholders become wildcards so every file of the same
 * location stays in scope while unrelated files never match. Directory
 * placeholders are resolved first, because they are not part of the file name.
 */
export function spillPrunePattern(template: string, vars: SpillVars): RegExp {
  const located = template
    .replace(/\{cwd\}/g, vars.cwd)
    .replace(/\{session_dir\}/g, vars.session_dir)
    .replace(/\{session_name\}/g, vars.session_name);
  const source = basename(located)
    .replace(/[.*+?^${}()|[\]\\]/g, "\\$&")
    .replace(/\\\{call_id\\\}/g, "[^/]+")
    .replace(/\\\{stream\\\}/g, "(?:stdout|stderr)");
  return new RegExp(`^${source}$`);
}

/** Creates the directory a spill file lives in. */
export async function prepareSpillDir(path: string): Promise<void> {
  await mkdir(dirname(path), {recursive: true});
}

export interface PruneOptions {
  directory: string;
  pattern: RegExp;
  /** Files older than this many days are removed; 0 disables pruning. */
  retentionDays: number;
  now?: number;
}

/**
 * Removes expired spill files, and only those: matching is limited to the
 * template's own shape, failures are ignored, and pruning is off when the
 * retention window is 0.
 */
export async function pruneSpillFiles(options: PruneOptions): Promise<number> {
  if (options.retentionDays <= 0) {
    return 0;
  }
  const cutoff = (options.now ?? Date.now()) - options.retentionDays * 24 * 60 * 60 * 1000;
  let removed = 0;
  let entries: string[];
  try {
    entries = await readdir(options.directory);
  } catch {
    return 0;
  }
  for (const entry of entries) {
    if (!options.pattern.test(entry)) {
      continue;
    }
    const path = `${options.directory}/${entry}`;
    try {
      const info = await stat(path);
      if (info.isFile() && info.mtimeMs < cutoff) {
        await unlink(path);
        removed += 1;
      }
    } catch {
      // A file that vanished or cannot be inspected is left alone.
    }
  }
  return removed;
}
