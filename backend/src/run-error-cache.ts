// Failed-run records.
//
// messages.jsonl is the provider request history: it holds only what a provider
// produced and what the user sent, so it stays provider-native and cache
// friendly. A run that fails leaves an orphan user turn there and nothing else.
// This file keeps the failure beside the other replaceable caches instead, so
// it survives the in-memory run-state window without touching the transcript.
//
// The cache is only a convenience for the UI: it can be deleted at any time and
// an unreadable file reads as empty.

import { mkdir, readFile } from "node:fs/promises";
import { dirname } from "node:path";

/** Atomic JSON writer supplied by the store, which owns the durability mode. */
export type RunErrorWriter = (path: string, value: unknown) => Promise<void>;

export const RUN_ERROR_CACHE_SCHEMA_VERSION = 1;

/** One failed run. `turn` is the session turn whose user message has no answer. */
export interface RunErrorEntry {
  turn: number;
  /**
   * Id of the user message in the web-ui projection (`session-user-o<offset>`).
   * Absent when the input had no visible text, and in entries written before
   * this field existed; readers then show the failure after the last message.
   */
  message_id?: string;
  turn_id: string;
  /** ISO timestamp of the failure. */
  at: string;
  /** Client-safe text: the same message the browser received as the error event. */
  message: string;
}

export interface RunErrorCache {
  schema_version: typeof RUN_ERROR_CACHE_SCHEMA_VERSION;
  errors: RunErrorEntry[];
}

function isEntry(value: unknown): value is RunErrorEntry {
  if (typeof value !== "object" || value === null) return false;
  const entry = value as Record<string, unknown>;
  return (
    typeof entry.turn === "number" &&
    Number.isSafeInteger(entry.turn) &&
    entry.turn >= 1 &&
    (entry.message_id === undefined || typeof entry.message_id === "string") &&
    typeof entry.turn_id === "string" &&
    typeof entry.at === "string" &&
    typeof entry.message === "string"
  );
}

/** Reads the cache; a missing, corrupt or foreign-version file is an empty cache. */
export async function readRunErrors(path: string): Promise<RunErrorEntry[]> {
  let text: string;
  try {
    text = await readFile(path, "utf8");
  } catch {
    return [];
  }
  try {
    const value = JSON.parse(text) as unknown;
    if (
      typeof value !== "object" || value === null ||
      (value as Record<string, unknown>).schema_version !== RUN_ERROR_CACHE_SCHEMA_VERSION ||
      !Array.isArray((value as Record<string, unknown>).errors)
    ) {
      return [];
    }
    return ((value as Record<string, unknown>).errors as unknown[]).filter(isEntry);
  } catch {
    return [];
  }
}

/**
 * Adds one entry and rewrites the file atomically. A later failure of the same
 * turn replaces the earlier one, and the oldest entries are dropped past
 * `maxEntries`.
 */
export async function appendRunError(
  path: string,
  entry: RunErrorEntry,
  maxEntries: number,
  write: RunErrorWriter,
): Promise<void> {
  const existing = await readRunErrors(path);
  const errors = [...existing.filter((item) => item.turn !== entry.turn), entry]
    .sort((left, right) => left.turn - right.turn)
    .slice(-maxEntries);
  const cache: RunErrorCache = {schema_version: RUN_ERROR_CACHE_SCHEMA_VERSION, errors};
  await mkdir(dirname(path), {recursive: true});
  await write(path, cache);
}
