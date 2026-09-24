import { createHash, randomBytes } from "node:crypto";
import { createReadStream } from "node:fs";
import {
  mkdir,
  open,
  readdir,
  readFile,
  rename,
  stat,
  rm,
} from "node:fs/promises";
import type { FileHandle } from "node:fs/promises";
import type { Stats } from "node:fs";
import { createInterface } from "node:readline";
import { dirname, isAbsolute, join, resolve } from "node:path";

import { AsyncMutex } from "./concurrency.js";

export type JsonRecord = Record<string, unknown>;
export type ResponseInputItem = JsonRecord;

export type DurabilityMode = "process" | "turn" | "strict";

/**
 * How much provider response metadata is stored beside its output items.
 *
 * Usage is compacted in both modes to the counters used by the usage cache.
 * `minimal` also filters other response metadata; `full` keeps other provider
 * fields, including the request echo.
 */
export type ResponseMetadataMode = "minimal" | "full";

/** A transcript or backup record, addressed by its byte offset. */
export interface OffsetRecord {
  offset: number;
  line: string;
}

/** Byte range of the backup's last turns, with the records it holds. */
export interface BackupTail {
  /** Offset where the oldest returned turn starts. */
  start: number;
  /** Visible end of the backup — the compaction seam. */
  end: number;
  /** Complete records in `[start, end)`, in transcript order. */
  records: OffsetRecord[];
}

export interface SessionResponseStoreOptions {
  /** Defaults to `turn`. */
  durability?: DurabilityMode;
  /** Provider response metadata mode; usage counters are compacted in both modes. Defaults to `minimal`. */
  responseMetadata?: ResponseMetadataMode;
  /** Maximum serialized output-item size in bytes. */
  maxItemBytes?: number;
  /** Transcript path; relative values resolve from the session directory. */
  messagesPath?: string;
  /** Staging directory; relative values resolve from the session directory. */
  stagingDir?: string;
  /** Replaceable compacted request-context cache. */
  compactionCachePath?: string;
  /** Replaceable normalized input-history cache; relative paths resolve from the session directory. */
  historyCachePath?: string;
  /** Durable usage-summary cache; relative paths resolve from the session directory. */
  usageCachePath?: string;
  /** Replaceable provider-run offset index; relative paths resolve from the session directory. */
  cursorCachePath?: string;
  /** Latest pre-compaction transcript backup. */
  messagesBackupPath?: string;
  /** Replaceable state-context snapshot file. */
  stateContextPath?: string;
}

export interface StateContextSnapshot extends JsonRecord {
  schema_version: 1;
  generated_at: string;
  trigger: string;
  text: string;
}

export interface ResponseUsageMetrics extends JsonRecord {
  input_tokens?: number;
  output_tokens?: number;
  total_tokens?: number;
  cached_input_tokens?: number;
}

export interface CurrentResponseUsage extends ResponseUsageMetrics {
  turn: number;
  step: number;
  response_id?: string;
}

export interface SessionUsageSummary extends ResponseUsageMetrics {
  completed_responses: number;
  responses_with_usage: number;
  turn_count: number;
  history_partial: boolean;
  current?: CurrentResponseUsage;
}

export interface ResponseOutputItemDoneEvent {
  type: "response.output_item.done";
  output_index?: number;
  item: JsonRecord;
}

export interface ResponseCompletedEvent {
  type: "response.completed";
  response: JsonRecord;
  /** Session-level user-submission number; stored outside the provider response. */
  session_turn?: number;
  /** Model-response step within the session turn. */
  session_step?: number;
}

export interface StagedOutputItemRecord {
  output_index: number;
  item: JsonRecord;
}

export type TurnStatus =
  | "streaming"
  | "ready"
  | "committing"
  | "committed"
  | "failed";

export interface TurnManifest {
  schema_version: 1;
  turn_id: string;
  response_id?: string;
  status: TurnStatus;
  item_count: number;
  next_sequence: number;
  canonical_line_sha256?: string;
}

export interface CommitResult {
  line_hash: string;
  bytes: number;
  already_present: boolean;
}

export interface RecoveryReport {
  repaired_message_bytes: number;
  inspected_turns: number;
  retained_turns: number;
  committed_turns: number;
  removed_turns: number;
  errors: string[];
}

interface StagedItemIndexEntry {
  output_index: number;
  offset: number;
  length: number;
}

interface RenderedLine {
  hash: string;
  bytes: number;
}

interface TranscriptMetadata {
  device: number;
  inode: number;
  size: number;
  modifiedAt: number;
  changedAt: number;
}

interface UsageTranscriptCheckpoint {
  device: number;
  inode: number;
  offset: number;
  modified_at_ms: number;
  changed_at_ms: number;
  checkpoint_start: number;
  checkpoint_sha256: string;
}

interface UsageCache {
  schema_version: 1;
  transcript: UsageTranscriptCheckpoint;
  summary: SessionUsageSummary;
  last_response_key?: string;
}

interface CompactionCache {
  schema_version: 1;
  source_sha256: string;
  items: ResponseInputItem[];
  assistant_final_items: string[];
  active_tail: JsonRecord[];
}

interface InputHistoryCache {
  schema_version: typeof HISTORY_CACHE_SCHEMA_VERSION;
  transcript: {
    device: number;
    inode: number;
    offset: number;
    modified_at_ms: number;
    changed_at_ms: number;
    checkpoint_start: number;
    checkpoint_sha256: string;
  };
  items: ResponseInputItem[];
}

/** One provider run: its start offset and the session turn/step it belongs to. */
export interface TranscriptRunCursor {
  offset: number;
  turn: number;
  step: number;
}

interface CursorCache {
  schema_version: typeof CURSOR_CACHE_SCHEMA_VERSION;
  transcript: {
    device: number;
    inode: number;
    offset: number;
    modified_at_ms: number;
    changed_at_ms: number;
    checkpoint_start: number;
    checkpoint_sha256: string;
  };
  runs: TranscriptRunCursor[];
}

/** The provider runs recorded in a transcript, with the state they were read at. */
export interface TranscriptCursorIndex {
  device: number;
  inode: number;
  size: number;
  runs: TranscriptRunCursor[];
}

const STAGING_DIRECTORY = "staging";
const MESSAGES_FILE = "messages.jsonl";
const MANIFEST_FILE = "manifest.json";
const ITEMS_FILE = "items.jsonl";
const ITEMS_INDEX_FILE = "items.index.jsonl";
const FINAL_FILE = "final.jsonl";
export const HISTORY_CACHE_FILE = "history-cache.json";
export const USAGE_CACHE_FILE = "usage-cache.json";
export const CURSOR_CACHE_FILE = "cursor-cache.json";
const COMPACTION_CACHE_FILE = "compaction-cache.json";
const HISTORY_CACHE_SCHEMA_VERSION = 1;
const USAGE_CACHE_SCHEMA_VERSION = 1;
const CURSOR_CACHE_SCHEMA_VERSION = 1;
const HISTORY_CHECKPOINT_BYTES = 64 * 1024;
const LINE_BUFFER_SIZE = 64 * 1024;
/**
 * Cheap prefilter for the run scan: a committed response always contains this
 * literal somewhere in its line, so everything else can be skipped without a
 * JSON parse. Whitespace inside the record is not assumed, because a transcript
 * may also have been written by something other than this store.
 */
const COMPLETED_RECORD_MARKER = '"response.completed"';
/** The same idea for a turn-opening record, counted when the UI walks back. */
const USER_RECORD_MARKER = /"role"\s*:\s*"user"/;
/** Internal transcript metadata, stripped before items are sent back to a provider. */
export const SESSION_INPUT_BATCH_ID_FIELD = "__agent_session_input_batch_id";
export const SESSION_INPUT_BATCH_INDEX_FIELD = "__agent_session_input_batch_index";
/**
 * Completed-response fields the transcript and its readers use: identity,
 * status, timing, and compact usage counters. Every other field of a completed
 * response is request echo — the provider repeats the request (tool definitions
 * included) on each response, and nothing reads it back.
 */
const RESPONSE_METADATA_KEYS: readonly string[] = [
  "id",
  "object",
  "status",
  "model",
  "created_at",
  "completed_at",
  "incomplete_details",
  "usage",
];
/** Bytes read per backward step while locating the backup's last turns. */
export const BACKUP_TAIL_WINDOW_BYTES = 256 * 1024;
/** Transcript records the compaction seam compares against the backup's end. */
const BACKUP_SEAM_RECORDS = 4;
/** Ceiling for one seam line, so an unfinished record cannot stall the check. */
const MAX_SEAM_LINE_BYTES = 64 * 1024 * 1024;

export class PersistenceError extends Error {
  public override name = "PersistenceError";
}

function isRecord(value: unknown): value is JsonRecord {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function asObject(value: unknown, description: string): JsonRecord {
  if (!isRecord(value)) {
    throw new PersistenceError(`${description} must be a JSON object`);
  }
  return value;
}

function stringifyJson(value: unknown, description: string): string {
  let serialized: string | undefined;
  try {
    serialized = JSON.stringify(value);
  } catch (error) {
    throw new PersistenceError(
      `Unable to serialize ${description}: ${error instanceof Error ? error.message : String(error)}`,
    );
  }

  if (serialized === undefined) {
    throw new PersistenceError(`Unable to serialize ${description}`);
  }
  return serialized;
}

function responseId(response: JsonRecord): string | undefined {
  const id = response.id;
  return typeof id === "string" && id.length > 0 ? id : undefined;
}

function validateTurnId(turnId: string): void {
  if (!/^[A-Za-z0-9._-]+$/.test(turnId) || turnId.length === 0 || turnId.length > 128) {
    throw new PersistenceError(
      "turnId must contain only letters, numbers, '.', '_' or '-' and be at most 128 characters",
    );
  }
}

function validateOutputItem(item: unknown): JsonRecord {
  const record = asObject(item, "output item");
  if (typeof record.type !== "string" || record.type.length === 0) {
    throw new PersistenceError("output item must contain a non-empty string type");
  }
  return record;
}

function validateInputItem(item: unknown): JsonRecord {
  const record = asObject(item, "input item");
  if (typeof record.type !== "string" || record.type.length === 0) {
    throw new PersistenceError("input item must contain a non-empty string type");
  }
  if (record.type === "response.completed" || record.type.startsWith("response.")) {
    throw new PersistenceError("response events cannot be appended as input items");
  }
  return record;
}

function validateCompletedEvent(event: ResponseCompletedEvent): JsonRecord {
  if (!isRecord(event) || event.type !== "response.completed") {
    throw new PersistenceError("expected a response.completed event");
  }
  for (const [field, value] of [
    ["session_turn", event.session_turn],
    ["session_step", event.session_step],
  ] as const) {
    if (value !== undefined && (!Number.isSafeInteger(value) || value <= 0)) {
      throw new PersistenceError(`${field} must be a positive safe integer`);
    }
  }

  const response = asObject(event.response, "completed response");
  if (response.output !== undefined) {
    if (!Array.isArray(response.output)) {
      throw new PersistenceError("completed response.output must be an array when present");
    }
    for (const item of response.output) {
      validateOutputItem(item);
    }
  }
  return response;
}

function validateOutputIndex(outputIndex: number): void {
  if (!Number.isSafeInteger(outputIndex) || outputIndex < 0) {
    throw new PersistenceError("output_index must be a non-negative safe integer");
  }
}

async function writeAll(
  handle: FileHandle,
  data: Buffer,
  position: number | null = null,
): Promise<void> {
  let offset = 0;
  while (offset < data.length) {
    const result = await handle.write(
      data,
      offset,
      data.length - offset,
      position === null ? null : position + offset,
    );
    if (result.bytesWritten <= 0) {
      throw new PersistenceError("filesystem write made no progress");
    }
    offset += result.bytesWritten;
  }
}

async function readExactly(
  handle: FileHandle,
  length: number,
  position: number,
): Promise<Buffer> {
  const result = Buffer.alloc(length);
  let offset = 0;
  while (offset < length) {
    const read = await handle.read(result, offset, length - offset, position + offset);
    if (read.bytesRead <= 0) {
      throw new PersistenceError("unexpected end of staging file");
    }
    offset += read.bytesRead;
  }
  return result;
}

async function syncIfRequired(
  handle: FileHandle,
  durability: DurabilityMode,
): Promise<void> {
  if (durability !== "process") {
    await handle.sync();
  }
}

async function writeJsonAtomic(
  path: string,
  value: unknown,
  durability: DurabilityMode,
): Promise<void> {
  const temporaryPath = `${path}.tmp-${process.pid}-${randomBytes(6).toString("hex")}`;
  let handle: FileHandle | undefined;
  try {
    handle = await open(temporaryPath, "w");
    const line = `${stringifyJson(value, path)}\n`;
    await writeAll(handle, Buffer.from(line, "utf8"));
    if (durability === "strict") {
      await handle.sync();
    }
    await handle.close();
    handle = undefined;
    await rename(temporaryPath, path);
  } finally {
    if (handle !== undefined) {
      await handle.close().catch(() => undefined);
    }
    await rm(temporaryPath, { force: true }).catch(() => undefined);
  }
}

/**
 * Statistics for a transcript file, creating it when the session has not
 * produced a committed response yet. A missing `messages.jsonl` means "empty
 * transcript", not a broken session: the same file is created lazily by
 * `repairJsonlTail` on the paths that read it.
 */
async function statOrCreate(path: string): Promise<Stats> {
  try {
    return await stat(path);
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code !== "ENOENT") {
      throw error;
    }
    const handle = await open(path, "a");
    try {
      return await handle.stat();
    } finally {
      await handle.close();
    }
  }
}

/**
 * Truncates a JSONL file to its last newline. This is intentionally byte-based:
 * a partial UTF-8 code point can only occur in the incomplete trailing record,
 * which is removed in its entirety.
 */
async function repairJsonlTail(path: string): Promise<number> {
  const handle = await open(path, "a+");
  try {
    const size = Number((await handle.stat()).size);
    if (size === 0) {
      return 0;
    }

    let cursor = size;
    let keepBytes = 0;
    while (cursor > 0) {
      const start = Math.max(0, cursor - LINE_BUFFER_SIZE);
      const length = cursor - start;
      const buffer = Buffer.alloc(length);
      let readOffset = 0;
      while (readOffset < length) {
        const result = await handle.read(
          buffer,
          readOffset,
          length - readOffset,
          start + readOffset,
        );
        if (result.bytesRead <= 0) {
          break;
        }
        readOffset += result.bytesRead;
      }

      const newlineOffset = buffer.lastIndexOf(0x0a);
      if (newlineOffset >= 0) {
        keepBytes = start + newlineOffset + 1;
        break;
      }
      cursor = start;
    }

    if (keepBytes !== size) {
      await handle.truncate(keepBytes);
    }
    return size - keepBytes;
  } finally {
    await handle.close();
  }
}

async function hashFileRange(path: string, start: number, end: number): Promise<string> {
  if (!Number.isSafeInteger(start) || !Number.isSafeInteger(end) || start < 0 || end < start) {
    throw new PersistenceError("invalid transcript hash range");
  }
  const handle = await open(path, "r");
  const hash = createHash("sha256");
  const buffer = Buffer.alloc(LINE_BUFFER_SIZE);
  try {
    let position = start;
    while (position < end) {
      const length = Math.min(buffer.length, end - position);
      const result = await handle.read(buffer, 0, length, position);
      if (result.bytesRead <= 0) {
        throw new PersistenceError("transcript ended before cache checkpoint");
      }
      hash.update(buffer.subarray(0, result.bytesRead));
      position += result.bytesRead;
    }
    return hash.digest("hex");
  } finally {
    await handle.close();
  }
}

async function hashFile(path: string): Promise<string> {
  const handle = await open(path, "r");
  const hash = createHash("sha256");
  const buffer = Buffer.alloc(LINE_BUFFER_SIZE);
  try {
    let position = 0;
    while (true) {
      const result = await handle.read(buffer, 0, buffer.length, position);
      if (result.bytesRead === 0) {
        break;
      }
      hash.update(buffer.subarray(0, result.bytesRead));
      position += result.bytesRead;
    }
    return hash.digest("hex");
  } finally {
    await handle.close();
  }
}

async function isNewlineTerminated(path: string): Promise<boolean> {
  const handle = await open(path, "r");
  try {
    const size = Number((await handle.stat()).size);
    if (size === 0) {
      return false;
    }
    const lastByte = Buffer.alloc(1);
    const result = await handle.read(lastByte, 0, 1, size - 1);
    return result.bytesRead === 1 && lastByte[0] === 0x0a;
  } finally {
    await handle.close();
  }
}

/** Finds an exact complete JSONL record by hash without parsing full records. */
async function containsLineHash(path: string, targetHash: string): Promise<boolean> {
  const stream = createReadStream(path);
  let lineHash = createHash("sha256");

  try {
    for await (const chunk of stream) {
      const buffer = Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk);
      let start = 0;
      while (start < buffer.length) {
        const newline = buffer.indexOf(0x0a, start);
        if (newline < 0) {
          lineHash.update(buffer.subarray(start));
          break;
        }

        lineHash.update(buffer.subarray(start, newline + 1));
        if (lineHash.digest("hex") === targetHash) {
          stream.destroy();
          return true;
        }
        lineHash = createHash("sha256");
        start = newline + 1;
      }
    }
    return false;
  } finally {
    stream.destroy();
  }
}

async function appendFileIncrementally(
  sourcePath: string,
  destinationPath: string,
  durability: DurabilityMode,
): Promise<void> {
  const source = await open(sourcePath, "r");
  const destination = await open(destinationPath, "a");
  const buffer = Buffer.alloc(LINE_BUFFER_SIZE);
  try {
    let position = 0;
    while (true) {
      const result = await source.read(buffer, 0, buffer.length, position);
      if (result.bytesRead === 0) {
        break;
      }
      await writeAll(destination, buffer.subarray(0, result.bytesRead));
      position += result.bytesRead;
    }
    await syncIfRequired(destination, durability);
  } finally {
    await source.close();
    await destination.close();
  }
}

async function readJsonLines(
  path: string,
  onLine: (line: string) => void | Promise<void>,
  start = 0,
): Promise<void> {
  const input = createReadStream(path, { encoding: "utf8", start });
  const lines = createInterface({ input, crlfDelay: Infinity });
  try {
    for await (const line of lines) {
      if (line.length > 0) {
        await onLine(line);
      }
    }
  } finally {
    lines.close();
    input.destroy();
  }
}

/**
 * Reads complete JSONL records in `[start, end)` and reports each one's byte
 * offset, so callers can address records without scanning from the beginning.
 *
 * A record counts as complete only when its terminating newline was read: an
 * appended-but-unfinished tail is left for the next call, which is what keeps an
 * offset index valid while the writer is appending. Returns the offset just past
 * the last complete record.
 */
export async function readTranscriptRecords(
  path: string,
  start: number,
  end: number,
  onRecord: (offset: number, line: string) => void | Promise<void>,
): Promise<number> {
  if (!Number.isSafeInteger(start) || !Number.isSafeInteger(end) || start < 0 || end < start) {
    throw new PersistenceError("invalid transcript read range");
  }
  if (end === start) {
    return start;
  }

  const handle = await open(path, "r");
  const buffer = Buffer.alloc(LINE_BUFFER_SIZE);
  try {
    let position = start;
    let lineStart = start;
    let carry: Buffer = Buffer.alloc(0);
    while (position < end) {
      const length = Math.min(buffer.length, end - position);
      const read = await handle.read(buffer, 0, length, position);
      if (read.bytesRead <= 0) {
        break;
      }
      position += read.bytesRead;
      const chunk = carry.length === 0
        ? buffer.subarray(0, read.bytesRead)
        : Buffer.concat([carry, buffer.subarray(0, read.bytesRead)]);
      const chunkStart = lineStart;
      let consumed = 0;
      let newline = chunk.indexOf(0x0a, consumed);
      while (newline >= 0) {
        await onRecord(lineStart, chunk.subarray(consumed, newline).toString("utf8"));
        consumed = newline + 1;
        lineStart = chunkStart + consumed;
        newline = chunk.indexOf(0x0a, consumed);
      }
      const remainder = chunk.subarray(consumed);
      // The remainder must not alias the shared read buffer: the next read
      // overwrites it, and the carry-out line has to stay intact. `chunk`
      // aliases `buffer` whenever there was no carry into this read, which is
      // exactly the case that carries a line across the buffer boundary.
      carry = remainder.length === 0 ? remainder : Buffer.from(remainder);
    }
    return lineStart;
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") {
      return start;
    }
    throw error;
  } finally {
    await handle.close();
  }
}

function isUserTurnRecordLine(line: string): boolean {
  if (!USER_RECORD_MARKER.test(line)) return false;
  try {
    const value = JSON.parse(line) as unknown;
    return isRecord(value) && value.type === "message" && value.role === "user";
  } catch {
    return false;
  }
}

function expandTranscriptRecord(value: unknown): ResponseInputItem[] {
  const record = asObject(value, "messages.jsonl record");
  if (record.type === "response.completed") {
    const response = validateCompletedEvent(record as unknown as ResponseCompletedEvent);
    if (!Array.isArray(response.output)) {
      throw new PersistenceError("committed response has no output array");
    }
    return response.output.map((item) => validateOutputItem(item));
  }
  if (typeof record.type === "string" && record.type.startsWith("response.")) {
    throw new PersistenceError(`unsupported response event in messages.jsonl: ${record.type}`);
  }
  if (
    Object.prototype.hasOwnProperty.call(record, SESSION_INPUT_BATCH_ID_FIELD) ||
    Object.prototype.hasOwnProperty.call(record, SESSION_INPUT_BATCH_INDEX_FIELD)
  ) {
    const item = {...record};
    delete item[SESSION_INPUT_BATCH_ID_FIELD];
    delete item[SESSION_INPUT_BATCH_INDEX_FIELD];
    return [validateInputItem(item)];
  }
  return [validateInputItem(record)];
}

function parseHistoryCache(value: unknown): InputHistoryCache {
  const record = asObject(value, "history cache");
  const transcript = asObject(record.transcript, "history cache transcript");
  if (
    record.schema_version !== HISTORY_CACHE_SCHEMA_VERSION ||
    !Array.isArray(record.items) ||
    typeof transcript.device !== "number" ||
    typeof transcript.inode !== "number" ||
    typeof transcript.offset !== "number" ||
    typeof transcript.modified_at_ms !== "number" ||
    typeof transcript.changed_at_ms !== "number" ||
    typeof transcript.checkpoint_start !== "number" ||
    typeof transcript.checkpoint_sha256 !== "string" ||
    !/^[a-f0-9]{64}$/.test(transcript.checkpoint_sha256)
  ) {
    throw new PersistenceError("invalid history cache");
  }
  for (const number of [
    transcript.device,
    transcript.inode,
    transcript.offset,
    transcript.modified_at_ms,
    transcript.changed_at_ms,
    transcript.checkpoint_start,
  ]) {
    if (!Number.isSafeInteger(number) || number < 0) {
      throw new PersistenceError("history cache has invalid transcript metadata");
    }
  }
  if (transcript.checkpoint_start > transcript.offset) {
    throw new PersistenceError("history cache checkpoint begins after its offset");
  }
  return {
    schema_version: HISTORY_CACHE_SCHEMA_VERSION,
    transcript: {
      device: transcript.device,
      inode: transcript.inode,
      offset: transcript.offset,
      modified_at_ms: transcript.modified_at_ms,
      changed_at_ms: transcript.changed_at_ms,
      checkpoint_start: transcript.checkpoint_start,
      checkpoint_sha256: transcript.checkpoint_sha256,
    },
    items: record.items.map((item) => validateInputItem(item)),
  };
}

function parseTranscriptCheckpoint(value: unknown, description: string): CursorCache["transcript"] {
  const transcript = asObject(value, description);
  if (
    typeof transcript.device !== "number" ||
    typeof transcript.inode !== "number" ||
    typeof transcript.offset !== "number" ||
    typeof transcript.modified_at_ms !== "number" ||
    typeof transcript.changed_at_ms !== "number" ||
    typeof transcript.checkpoint_start !== "number" ||
    typeof transcript.checkpoint_sha256 !== "string" ||
    !/^[a-f0-9]{64}$/.test(transcript.checkpoint_sha256)
  ) {
    throw new PersistenceError(`invalid ${description}`);
  }
  for (const number of [
    transcript.device,
    transcript.inode,
    transcript.offset,
    transcript.modified_at_ms,
    transcript.changed_at_ms,
    transcript.checkpoint_start,
  ]) {
    if (!Number.isSafeInteger(number) || number < 0) {
      throw new PersistenceError(`${description} has invalid transcript metadata`);
    }
  }
  if (transcript.checkpoint_start > transcript.offset) {
    throw new PersistenceError(`${description} begins after its offset`);
  }
  return {
    device: transcript.device,
    inode: transcript.inode,
    offset: transcript.offset,
    modified_at_ms: transcript.modified_at_ms,
    changed_at_ms: transcript.changed_at_ms,
    checkpoint_start: transcript.checkpoint_start,
    checkpoint_sha256: transcript.checkpoint_sha256,
  };
}

function parseCursorCache(value: unknown): CursorCache {
  const record = asObject(value, "cursor cache");
  if (record.schema_version !== CURSOR_CACHE_SCHEMA_VERSION || !Array.isArray(record.runs)) {
    throw new PersistenceError("invalid cursor cache");
  }
  const transcript = parseTranscriptCheckpoint(record.transcript, "cursor cache transcript");
  const runs: TranscriptRunCursor[] = [];
  let previousOffset = -1;
  for (const entry of record.runs) {
    const run = asObject(entry, "cursor cache run");
    if (
      !Number.isSafeInteger(run.offset) ||
      !Number.isSafeInteger(run.turn) ||
      !Number.isSafeInteger(run.step) ||
      (run.offset as number) < 0 ||
      (run.turn as number) < 1 ||
      (run.step as number) < 1 ||
      (run.offset as number) <= previousOffset ||
      (run.offset as number) >= transcript.offset
    ) {
      throw new PersistenceError("cursor cache run has invalid offset, turn, or step");
    }
    previousOffset = run.offset as number;
    runs.push({offset: run.offset as number, turn: run.turn as number, step: run.step as number});
  }
  return {schema_version: CURSOR_CACHE_SCHEMA_VERSION, transcript, runs};
}

const USAGE_TOKEN_FIELDS = [
  "input_tokens",
  "output_tokens",
  "total_tokens",
  "cached_input_tokens",
] as const;

type UsageTokenField = (typeof USAGE_TOKEN_FIELDS)[number];

function isUsageTokenCount(value: unknown): value is number {
  return typeof value === "number" && Number.isSafeInteger(value) && value >= 0;
}

function parseUsageTokenFields(
  record: JsonRecord,
  description: string,
): ResponseUsageMetrics {
  const metrics: ResponseUsageMetrics = {};
  for (const field of USAGE_TOKEN_FIELDS) {
    const value = record[field];
    if (value === undefined) continue;
    if (!isUsageTokenCount(value)) {
      throw new PersistenceError(`${description}.${field} must be a non-negative safe integer`);
    }
    metrics[field] = value;
  }
  return metrics;
}

function parseUsageSummary(value: unknown): SessionUsageSummary {
  const record = asObject(value, "usage cache summary");
  const completedResponses = record.completed_responses;
  const responsesWithUsage = record.responses_with_usage;
  const turnCount = record.turn_count;
  if (
    !isUsageTokenCount(completedResponses) ||
    !isUsageTokenCount(responsesWithUsage) ||
    !isUsageTokenCount(turnCount)
  ) {
    throw new PersistenceError("usage cache summary counters must be non-negative safe integers");
  }
  if (
    typeof record.history_partial !== "boolean" ||
    responsesWithUsage > completedResponses
  ) {
    throw new PersistenceError("invalid usage cache summary counters");
  }

  let current: CurrentResponseUsage | undefined;
  if (record.current !== undefined) {
    const currentRecord = asObject(record.current, "usage cache current response");
    if (
      !Number.isSafeInteger(currentRecord.turn) ||
      (currentRecord.turn as number) <= 0 ||
      !Number.isSafeInteger(currentRecord.step) ||
      (currentRecord.step as number) <= 0 ||
      (currentRecord.response_id !== undefined && typeof currentRecord.response_id !== "string")
    ) {
      throw new PersistenceError("invalid usage cache current response metadata");
    }
    current = {
      ...parseUsageTokenFields(currentRecord, "usage cache current response"),
      turn: currentRecord.turn as number,
      step: currentRecord.step as number,
      ...(typeof currentRecord.response_id === "string"
        ? {response_id: currentRecord.response_id}
        : {}),
    };
  }

  return {
    ...parseUsageTokenFields(record, "usage cache summary"),
    completed_responses: record.completed_responses as number,
    responses_with_usage: record.responses_with_usage as number,
    turn_count: record.turn_count as number,
    history_partial: record.history_partial,
    ...(current === undefined ? {} : {current}),
  };
}

function parseUsageCache(value: unknown): UsageCache {
  const record = asObject(value, "usage cache");
  const transcript = asObject(record.transcript, "usage cache transcript");
  if (
    record.schema_version !== USAGE_CACHE_SCHEMA_VERSION ||
    typeof transcript.device !== "number" ||
    typeof transcript.inode !== "number" ||
    typeof transcript.offset !== "number" ||
    typeof transcript.modified_at_ms !== "number" ||
    typeof transcript.changed_at_ms !== "number" ||
    typeof transcript.checkpoint_start !== "number" ||
    typeof transcript.checkpoint_sha256 !== "string" ||
    !/^[a-f0-9]{64}$/.test(transcript.checkpoint_sha256)
  ) {
    throw new PersistenceError("invalid usage cache");
  }
  for (const number of [
    transcript.device,
    transcript.inode,
    transcript.offset,
    transcript.modified_at_ms,
    transcript.changed_at_ms,
    transcript.checkpoint_start,
  ]) {
    if (!Number.isSafeInteger(number) || number < 0) {
      throw new PersistenceError("usage cache has invalid transcript metadata");
    }
  }
  if (transcript.checkpoint_start > transcript.offset) {
    throw new PersistenceError("usage cache checkpoint begins after its offset");
  }
  const lastResponseKey = record.last_response_key;
  if (
    lastResponseKey !== undefined &&
    (typeof lastResponseKey !== "string" || !/^[a-f0-9]{64}$/.test(lastResponseKey))
  ) {
    throw new PersistenceError("usage cache has an invalid response checkpoint");
  }
  const summary = parseUsageSummary(record.summary);
  if ((summary.completed_responses > 0) !== (lastResponseKey !== undefined)) {
    throw new PersistenceError("usage cache response checkpoint does not match its summary");
  }
  return {
    schema_version: USAGE_CACHE_SCHEMA_VERSION,
    transcript: {
      device: transcript.device,
      inode: transcript.inode,
      offset: transcript.offset,
      modified_at_ms: transcript.modified_at_ms,
      changed_at_ms: transcript.changed_at_ms,
      checkpoint_start: transcript.checkpoint_start,
      checkpoint_sha256: transcript.checkpoint_sha256,
    },
    summary,
    ...(typeof lastResponseKey === "string" ? {last_response_key: lastResponseKey} : {}),
  };
}

function usageMetricsFromResponse(response: JsonRecord): ResponseUsageMetrics {
  if (!isRecord(response.usage)) return {};
  const usage = response.usage;
  const metrics: ResponseUsageMetrics = {};
  const inputTokens = isUsageTokenCount(usage.input_tokens) ? usage.input_tokens : undefined;
  const outputTokens = isUsageTokenCount(usage.output_tokens) ? usage.output_tokens : undefined;
  const reportedTotalTokens = isUsageTokenCount(usage.total_tokens) ? usage.total_tokens : undefined;
  const inputDetails = isRecord(usage.input_tokens_details)
    ? usage.input_tokens_details
    : undefined;
  const cachedInputTokens = isUsageTokenCount(inputDetails?.cached_tokens)
    ? inputDetails.cached_tokens
    : undefined;
  if (inputTokens !== undefined) metrics.input_tokens = inputTokens;
  if (outputTokens !== undefined) metrics.output_tokens = outputTokens;
  if (reportedTotalTokens !== undefined) {
    metrics.total_tokens = reportedTotalTokens;
  } else if (inputTokens !== undefined && outputTokens !== undefined) {
    const derivedTotal = inputTokens + outputTokens;
    if (Number.isSafeInteger(derivedTotal)) metrics.total_tokens = derivedTotal;
  }
  if (cachedInputTokens !== undefined) metrics.cached_input_tokens = cachedInputTokens;
  return metrics;
}

function usageResponseKey(record: JsonRecord): string {
  const response = isRecord(record.response) ? record.response : undefined;
  const id = response?.id;
  const identity = typeof id === "string" && id.length > 0
    ? `response:${id}`
    : `record:${stringifyJson(record, "completed response record")}`;
  return createHash("sha256").update(identity).digest("hex");
}

function addUsageValue(
  summary: SessionUsageSummary,
  field: UsageTokenField,
  value: number,
): void {
  const total = (summary[field] ?? 0) + value;
  if (Number.isSafeInteger(total)) summary[field] = total;
  else delete summary[field];
}

function addUsageTranscriptRecord(
  cache: UsageCache,
  record: JsonRecord,
  seenResponses?: Set<string>,
  countUserMessage = true,
  seenInputBatches?: Set<string>,
): void {
  const summary = cache.summary;
  if (countUserMessage && record.role === "user" && summary.turn_count < Number.MAX_SAFE_INTEGER) {
    const batchId = record[SESSION_INPUT_BATCH_ID_FIELD];
    if (typeof batchId !== "string" || !seenInputBatches?.has(batchId)) {
      if (typeof batchId === "string") seenInputBatches?.add(batchId);
      summary.turn_count += 1;
    }
  }
  if (record.type !== "response.completed" || !isRecord(record.response)) return;

  const responseKey = usageResponseKey(record);
  if (seenResponses?.has(responseKey) || cache.last_response_key === responseKey) return;
  seenResponses?.add(responseKey);

  const response = record.response;
  const metrics = usageMetricsFromResponse(response);
  const hasUsage = USAGE_TOKEN_FIELDS.some((field) => metrics[field] !== undefined);
  const explicitTurn = isUsageTokenCount(record.session_turn) && record.session_turn > 0
    ? record.session_turn
    : undefined;
  const turn = explicitTurn ?? Math.max(summary.turn_count, 1);
  if (explicitTurn !== undefined) summary.turn_count = explicitTurn;
  else if (summary.turn_count < turn) summary.turn_count = turn;
  const previous = summary.current;
  const explicitStep = isUsageTokenCount(record.session_step) && record.session_step > 0
    ? record.session_step
    : undefined;
  const step = explicitStep ?? (previous?.turn === turn ? previous.step + 1 : 1);

  summary.completed_responses += 1;
  if (hasUsage) {
    summary.responses_with_usage += 1;
    for (const field of USAGE_TOKEN_FIELDS) {
      const value = metrics[field];
      if (value !== undefined) addUsageValue(summary, field, value);
    }
  }
  summary.current = {
    ...metrics,
    turn,
    step,
    ...(typeof response.id === "string" && response.id.length > 0
      ? {response_id: response.id}
      : {}),
  };
  cache.last_response_key = responseKey;
}

async function loadItemIndex(path: string): Promise<Map<number, StagedItemIndexEntry>> {
  const index = new Map<number, StagedItemIndexEntry>();
  await readJsonLines(path, (line) => {
    let value: unknown;
    try {
      value = JSON.parse(line);
    } catch (error) {
      throw new PersistenceError(
        `invalid staging index JSON: ${error instanceof Error ? error.message : String(error)}`,
      );
    }

    const record = asObject(value, "staging index record");
    const outputIndex = record.output_index;
    const offset = record.offset;
    const length = record.length;
    if (
      typeof outputIndex !== "number" ||
      typeof offset !== "number" ||
      typeof length !== "number"
    ) {
      throw new PersistenceError("staging index record has invalid numeric fields");
    }
    validateOutputIndex(outputIndex);
    if (!Number.isSafeInteger(offset) || offset < 0 || !Number.isSafeInteger(length) || length <= 0) {
      throw new PersistenceError("staging index record has invalid offset or length");
    }
    if (index.has(outputIndex)) {
      throw new PersistenceError(`duplicate staged output_index ${outputIndex}`);
    }
    index.set(outputIndex, { output_index: outputIndex, offset, length });
  });
  return index;
}

async function readStagedItem(
  itemsPath: string,
  indexEntry: StagedItemIndexEntry,
  expectedOutputIndex: number,
): Promise<JsonRecord> {
  const handle = await open(itemsPath, "r");
  try {
    const line = await readExactly(handle, indexEntry.length, indexEntry.offset);
    if (line[line.length - 1] !== 0x0a) {
      throw new PersistenceError("staged item record is not newline terminated");
    }

    let value: unknown;
    try {
      value = JSON.parse(line.subarray(0, line.length - 1).toString("utf8"));
    } catch (error) {
      throw new PersistenceError(
        `invalid staged item JSON: ${error instanceof Error ? error.message : String(error)}`,
      );
    }

    const record = asObject(value, "staged item record");
    if (record.output_index !== expectedOutputIndex) {
      throw new PersistenceError(
        `staged item index mismatch: expected ${expectedOutputIndex}, got ${String(record.output_index)}`,
      );
    }
    return validateOutputItem(record.item);
  } finally {
    await handle.close();
  }
}

function compactUsageMetadata(value: unknown): JsonRecord | undefined {
  if (!isRecord(value)) return undefined;
  const compact: JsonRecord = {};
  for (const key of ["input_tokens", "output_tokens", "total_tokens"] as const) {
    const count = value[key];
    if (isUsageTokenCount(count)) compact[key] = count;
  }
  const inputDetails = isRecord(value.input_tokens_details) ? value.input_tokens_details : undefined;
  const cachedTokens = inputDetails?.cached_tokens;
  if (isUsageTokenCount(cachedTokens)) compact.input_tokens_details = {cached_tokens: cachedTokens};
  return Object.keys(compact).length > 0 ? compact : undefined;
}

async function renderCanonicalCompletion(
  response: JsonRecord,
  itemIndex: Map<number, StagedItemIndexEntry>,
  itemsPath: string,
  finalPath: string,
  expectedItemCount: number,
  durability: DurabilityMode,
  responseMetadata: ResponseMetadataMode,
  sessionTurn?: number,
  sessionStep?: number,
): Promise<RenderedLine> {
  const metadata: JsonRecord = {};
  for (const [key, value] of Object.entries(response)) {
    if (key === "output") {
      continue;
    }
    if (key === "usage") {
      const compactUsage = compactUsageMetadata(value);
      if (compactUsage !== undefined) metadata[key] = compactUsage;
      continue;
    }
    if (responseMetadata === "minimal" && !RESPONSE_METADATA_KEYS.includes(key)) {
      continue;
    }
    metadata[key] = value;
  }

  const metadataJson = stringifyJson(metadata, "response metadata");
  if (!metadataJson.endsWith("}")) {
    throw new PersistenceError("serialized response metadata is not an object");
  }

  const eventMetadata: JsonRecord = {type: "response.completed"};
  if (sessionTurn !== undefined) eventMetadata.session_turn = sessionTurn;
  if (sessionStep !== undefined) eventMetadata.session_step = sessionStep;
  const eventMetadataJson = stringifyJson(eventMetadata, "response event metadata");
  const prefix =
    `${eventMetadataJson.slice(0, -1)},\"response\":${metadataJson.slice(0, -1)}` +
    `${metadataJson.length > 2 ? "," : ""}\"output\":[`;
  const suffix = "]}}\n";
  const temporaryPath = `${finalPath}.tmp-${process.pid}-${randomBytes(6).toString("hex")}`;
  const output = await open(temporaryPath, "w");
  const hash = createHash("sha256");
  let bytes = 0;

  const writeChunk = async (chunk: string | Buffer): Promise<void> => {
    const buffer = typeof chunk === "string" ? Buffer.from(chunk, "utf8") : chunk;
    await writeAll(output, buffer);
    hash.update(buffer);
    bytes += buffer.length;
  };

  try {
    await writeChunk(prefix);
    for (let outputIndex = 0; outputIndex < expectedItemCount; outputIndex += 1) {
      const indexEntry = itemIndex.get(outputIndex);
      if (indexEntry === undefined) {
        throw new PersistenceError(`missing staged output_index ${outputIndex}`);
      }
      if (outputIndex > 0) {
        await writeChunk(",");
      }
      const item = await readStagedItem(itemsPath, indexEntry, outputIndex);
      await writeChunk(stringifyJson(item, "staged output item"));
    }
    await writeChunk(suffix);
    await syncIfRequired(output, durability);
    await output.close();
    await rename(temporaryPath, finalPath);
    return { hash: hash.digest("hex"), bytes };
  } catch (error) {
    await output.close().catch(() => undefined);
    throw error;
  } finally {
    await rm(temporaryPath, { force: true }).catch(() => undefined);
  }
}

function makeTurnId(): string {
  return `turn-${Date.now().toString(36)}-${randomBytes(8).toString("hex")}`;
}

function parseManifest(value: unknown, path: string): TurnManifest {
  const record = asObject(value, path);
  if (
    record.schema_version !== 1 ||
    typeof record.turn_id !== "string" ||
    typeof record.status !== "string" ||
    typeof record.item_count !== "number" ||
    typeof record.next_sequence !== "number"
  ) {
    throw new PersistenceError(`invalid turn manifest: ${path}`);
  }

  const statuses: TurnStatus[] = ["streaming", "ready", "committing", "committed", "failed"];
  if (!statuses.includes(record.status as TurnStatus)) {
    throw new PersistenceError(`invalid turn status in manifest: ${path}`);
  }
  if (!Number.isSafeInteger(record.item_count) || record.item_count < 0) {
    throw new PersistenceError(`invalid item_count in manifest: ${path}`);
  }
  if (!Number.isSafeInteger(record.next_sequence) || record.next_sequence < 0) {
    throw new PersistenceError(`invalid next_sequence in manifest: ${path}`);
  }
  if (record.response_id !== undefined && typeof record.response_id !== "string") {
    throw new PersistenceError(`invalid response_id in manifest: ${path}`);
  }
  if (
    record.canonical_line_sha256 !== undefined &&
    (typeof record.canonical_line_sha256 !== "string" || !/^[a-f0-9]{64}$/.test(record.canonical_line_sha256))
  ) {
    throw new PersistenceError(`invalid canonical line hash in manifest: ${path}`);
  }

  return {
    schema_version: 1,
    turn_id: record.turn_id,
    response_id: record.response_id as string | undefined,
    status: record.status as TurnStatus,
    item_count: record.item_count,
    next_sequence: record.next_sequence,
    canonical_line_sha256: record.canonical_line_sha256 as string | undefined,
  };
}

export class SessionResponseStore {
  public readonly sessionDir: string;
  public readonly messagesPath: string;
  public readonly stagingDir: string;
  public readonly historyCachePath: string;
  public readonly usageCachePath: string;
  public readonly cursorCachePath: string;
  public readonly compactionCachePath: string;
  public readonly compactionCacheBackupPath: string;
  public readonly messagesBackupPath: string;
  public readonly stateContextPath: string;

  private readonly options: Required<Pick<SessionResponseStoreOptions, "durability" | "maxItemBytes" | "responseMetadata">>;
  private historyCache: InputHistoryCache | undefined;
  private usageCache: UsageCache | undefined;
  private cursorCache: CursorCache | undefined;
  private readonly messageMutex = new AsyncMutex();
  private readonly activeTurns = new Set<ResponseTurn>();
  private closed = false;

  private constructor(sessionDir: string, options: SessionResponseStoreOptions) {
    const absoluteSessionDir = resolve(sessionDir);
    this.sessionDir = absoluteSessionDir;
    this.messagesPath = options.messagesPath
      ? isAbsolute(options.messagesPath)
        ? resolve(options.messagesPath)
        : resolve(absoluteSessionDir, options.messagesPath)
      : join(absoluteSessionDir, MESSAGES_FILE);
    this.stagingDir = options.stagingDir
      ? isAbsolute(options.stagingDir)
        ? resolve(options.stagingDir)
        : resolve(absoluteSessionDir, options.stagingDir)
      : join(absoluteSessionDir, STAGING_DIRECTORY);
    this.historyCachePath = options.historyCachePath
      ? isAbsolute(options.historyCachePath)
        ? resolve(options.historyCachePath)
        : resolve(absoluteSessionDir, options.historyCachePath)
      : join(this.stagingDir, HISTORY_CACHE_FILE);
    this.usageCachePath = options.usageCachePath
      ? isAbsolute(options.usageCachePath)
        ? resolve(options.usageCachePath)
        : resolve(absoluteSessionDir, options.usageCachePath)
      : join(this.stagingDir, USAGE_CACHE_FILE);
    this.cursorCachePath = options.cursorCachePath
      ? isAbsolute(options.cursorCachePath)
        ? resolve(options.cursorCachePath)
        : resolve(absoluteSessionDir, options.cursorCachePath)
      : join(this.stagingDir, CURSOR_CACHE_FILE);
    this.compactionCachePath = options.compactionCachePath
      ? isAbsolute(options.compactionCachePath)
        ? resolve(options.compactionCachePath)
        : resolve(absoluteSessionDir, options.compactionCachePath)
      : join(this.stagingDir, COMPACTION_CACHE_FILE);
    this.compactionCacheBackupPath = `${this.compactionCachePath}.backup`;
    this.messagesBackupPath = options.messagesBackupPath
      ? isAbsolute(options.messagesBackupPath)
        ? resolve(options.messagesBackupPath)
        : resolve(absoluteSessionDir, options.messagesBackupPath)
      : join(absoluteSessionDir, "messages.backup.jsonl");
    this.stateContextPath = options.stateContextPath
      ? isAbsolute(options.stateContextPath)
        ? resolve(options.stateContextPath)
        : resolve(absoluteSessionDir, options.stateContextPath)
      : join(this.stagingDir, "statecontext.json");
    this.options = {
      durability: options.durability ?? "turn",
      maxItemBytes: options.maxItemBytes ?? Number.MAX_SAFE_INTEGER,
      responseMetadata: options.responseMetadata ?? "minimal",
    };

    if (!Number.isSafeInteger(this.options.maxItemBytes) || this.options.maxItemBytes <= 0) {
      throw new PersistenceError("maxItemBytes must be a positive safe integer");
    }
    if (
      this.options.responseMetadata !== "minimal" &&
      this.options.responseMetadata !== "full"
    ) {
      throw new PersistenceError("responseMetadata must be minimal or full");
    }
  }

  public static async open(
    sessionDir: string,
    options: SessionResponseStoreOptions = {},
  ): Promise<SessionResponseStore> {
    const store = new SessionResponseStore(sessionDir, options);
    await mkdir(store.sessionDir, { recursive: true });
    await mkdir(dirname(store.messagesPath), { recursive: true });
    await mkdir(store.stagingDir, { recursive: true });
    await mkdir(dirname(store.historyCachePath), { recursive: true });
    await mkdir(dirname(store.usageCachePath), { recursive: true });
    await repairJsonlTail(store.messagesPath);
    await store.finishPendingCompaction();
    await store.initializeUsageCache();
    return store;
  }

  /** Reads only usage-cache.json for listings; it never scans the transcript. */
  public static async readCachedUsageSummary(
    path: string,
  ): Promise<SessionUsageSummary | undefined> {
    try {
      const cache = parseUsageCache(JSON.parse(await readFile(path, "utf8")) as unknown);
      return {
        ...cache.summary,
        ...(cache.summary.current === undefined
          ? {}
          : {current: {...cache.summary.current}}),
      };
    } catch {
      return undefined;
    }
  }

  public get durability(): DurabilityMode {
    return this.options.durability;
  }

  public async beginTurn(turnId = makeTurnId()): Promise<ResponseTurn> {
    this.assertOpen();
    validateTurnId(turnId);

    const turnDir = join(this.stagingDir, turnId);
    try {
      await mkdir(turnDir);
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code === "EEXIST") {
        throw new PersistenceError(`turn already exists: ${turnId}`);
      }
      throw error;
    }

    const itemsPath = join(turnDir, ITEMS_FILE);
    const indexPath = join(turnDir, ITEMS_INDEX_FILE);
    const manifestPath = join(turnDir, MANIFEST_FILE);
    await repairJsonlTail(itemsPath);
    await repairJsonlTail(indexPath);

    const manifest: TurnManifest = {
      schema_version: 1,
      turn_id: turnId,
      status: "streaming",
      item_count: 0,
      next_sequence: 0,
    };
    await writeJsonAtomic(manifestPath, manifest, this.options.durability);

    const turn = new ResponseTurn(
      this,
      turnDir,
      itemsPath,
      indexPath,
      manifestPath,
      manifest,
      new Map<number, StagedItemIndexEntry>(),
    );
    this.activeTurns.add(turn);
    return turn;
  }

  /**
   * Appends Responses API input items with an internal submission id used for
   * usage reconstruction and pagination. The id is stripped when history is
   * reconstructed for the provider.
   */
  public async appendInputItem(item: ResponseInputItem): Promise<void> {
    await this.appendInputItems([item]);
  }

  public async appendInputItems(items: readonly ResponseInputItem[]): Promise<void> {
    this.assertOpen();
    if (this.activeTurns.size > 0) {
      throw new PersistenceError("cannot append input while a response turn is active");
    }
    if (items.length === 0) {
      return;
    }

    const validatedItems = items.map((item) => validateInputItem(item));
    if (validatedItems.some((item) =>
      Object.prototype.hasOwnProperty.call(item, SESSION_INPUT_BATCH_ID_FIELD) ||
      Object.prototype.hasOwnProperty.call(item, SESSION_INPUT_BATCH_INDEX_FIELD)
    )) {
      throw new PersistenceError("input items may not use reserved session batch metadata fields");
    }
    const batchId = randomBytes(16).toString("hex");
    const persistedItems = validatedItems.map((item, index) => ({
      ...item,
      [SESSION_INPUT_BATCH_ID_FIELD]: batchId,
      [SESSION_INPUT_BATCH_INDEX_FIELD]: index,
    }));
    await this.messageMutex.runExclusive(async () => {
      await repairJsonlTail(this.messagesPath);
      const handle = await open(this.messagesPath, "a");
      try {
        for (const item of persistedItems) {
          const line = `${stringifyJson(item, "input item")}\n`;
          await writeAll(handle, Buffer.from(line, "utf8"));
        }
        await syncIfRequired(handle, this.options.durability);
      } finally {
        await handle.close();
      }
      await this.refreshUsageCacheUnlocked();
    });
  }

  public async readStateContextSnapshot(): Promise<StateContextSnapshot | undefined> {
    this.assertOpen();
    return this.messageMutex.runExclusive(async () => {
      let text: string;
      try {
        text = await readFile(this.stateContextPath, "utf8");
      } catch (error) {
        if ((error as NodeJS.ErrnoException).code === "ENOENT") return undefined;
        throw error;
      }
      let value: unknown;
      try { value = JSON.parse(text); } catch { return undefined; }
      if (!isRecord(value) || value.schema_version !== 1 || typeof value.generated_at !== "string" || typeof value.trigger !== "string" || typeof value.text !== "string") {
        return undefined;
      }
      return value as StateContextSnapshot;
    });
  }

  public async writeStateContextSnapshot(snapshot: StateContextSnapshot): Promise<void> {
    this.assertOpen();
    await this.messageMutex.runExclusive(async () => {
      await mkdir(dirname(this.stateContextPath), {recursive: true});
      await writeJsonAtomic(this.stateContextPath, snapshot, this.options.durability);
    });
  }

  /**
   * Reconstructs request history from the replaceable compaction prefix and
   * active transcript. Raw input items are yielded as-is; each committed
   * response contributes its ordered `response.output` items.
   */
  public async *readInputItems(): AsyncGenerator<ResponseInputItem> {
    this.assertOpen();
    const items = await this.loadInputHistory();
    yield* items;
  }

  private async loadInputHistory(): Promise<ResponseInputItem[]> {
    return this.messageMutex.runExclusive(async () => {
      await repairJsonlTail(this.messagesPath);
      return this.loadInputHistoryUnlocked();
    });
  }

  private async loadInputHistoryUnlocked(): Promise<ResponseInputItem[]> {
    await repairJsonlTail(this.messagesPath);
    const current = await this.readTranscriptMetadata();
    const compaction = await this.readCompactionCache();
    const cached = this.historyCache ?? (await this.readHistoryCache());
    const valid = cached !== undefined && (await this.cacheMatches(cached, current));
    let tail: ResponseInputItem[];
    if (valid && current.size === cached!.transcript.offset) {
      tail = cached!.items;
    } else {
      const start = valid ? cached!.transcript.offset : 0;
      // Never mutate the prior cache until its replacement is durable.
      const items = valid ? [...cached!.items] : [];

      if (current.size > start) {
        items.push(...(await this.readTranscriptItems(start)));
      }

      const historyCache = await this.makeHistoryCache(items, current);
      this.historyCache = historyCache;
      await writeJsonAtomic(this.historyCachePath, historyCache, this.options.durability);
      tail = historyCache.items;
    }
    if (compaction === undefined) {
      if (await this.hasUnrestoredCompactionHistory()) {
        throw new PersistenceError(
          "compacted request history is unavailable; restore the compaction cache or its backup before continuing",
        );
      }
      return tail;
    }
    if ((await hashFile(this.messagesPath).catch(() => "")) === compaction.source_sha256) {
      return [...compaction.items, ...compaction.active_tail.flatMap((record) => expandTranscriptRecord(record))];
    }
    return [...compaction.items, ...tail];
  }

  private async hasUnrestoredCompactionHistory(): Promise<boolean> {
    const backup = await stat(this.messagesBackupPath).catch(() => undefined);
    if (backup === undefined || backup.size === 0) return false;
    const [backupHash, transcriptHash] = await Promise.all([
      hashFile(this.messagesBackupPath).catch(() => ""),
      hashFile(this.messagesPath).catch(() => ""),
    ]);
    return backupHash.length > 0 && backupHash !== transcriptHash;
  }

  private async finishPendingCompaction(): Promise<void> {
    const cache = await this.readCompactionCache();
    if (cache === undefined) return;
    const currentHash = await hashFile(this.messagesPath).catch(() => "");
    if (currentHash === cache.source_sha256) {
      await this.replaceMessagesWithRecords(cache.active_tail);
      await rm(this.historyCachePath, {force: true});
      return;
    }
    if ((await stat(this.messagesPath)).size === 0) {
      await rm(this.historyCachePath, {force: true});
    }
  }

  private async replaceMessagesWithRecords(records: readonly JsonRecord[]): Promise<void> {
    const temporaryPath = `${this.messagesPath}.tmp-${process.pid}-${randomBytes(6).toString("hex")}`;
    let handle: FileHandle | undefined;
    try {
      handle = await open(temporaryPath, "wx");
      for (const record of records) {
        await writeAll(handle, Buffer.from(`${stringifyJson(record, "transcript record")}\n`, "utf8"));
      }
      await syncIfRequired(handle, this.options.durability);
      await handle.close();
      handle = undefined;
      await rename(temporaryPath, this.messagesPath);
    } finally {
      if (handle !== undefined) await handle.close().catch(() => undefined);
      await rm(temporaryPath, {force: true}).catch(() => undefined);
    }
  }

  private async readCompactionCache(): Promise<CompactionCache | undefined> {
    return await this.readCompactionCacheAt(this.compactionCachePath)
      ?? await this.readCompactionCacheAt(this.compactionCacheBackupPath);
  }

  private async readCompactionCacheAt(path: string): Promise<CompactionCache | undefined> {
    try {
      const value = JSON.parse(await readFile(path, "utf8")) as unknown;
      if (
        !isRecord(value) ||
        value.schema_version !== 1 ||
        typeof value.source_sha256 !== "string" ||
        !/^[a-f0-9]{64}$/.test(value.source_sha256) ||
        !Array.isArray(value.items)
      ) {
        return undefined;
      }
      const assistantFinalItems = value.assistant_final_items ?? [];
      const activeTail = value.active_tail ?? [];
      if (
        !Array.isArray(assistantFinalItems) ||
        assistantFinalItems.some((item) => typeof item !== "string") ||
        !Array.isArray(activeTail) ||
        activeTail.some((record) => {
          try { expandTranscriptRecord(record); return !isRecord(record); } catch { return true; }
        })
      ) {
        return undefined;
      }
      return {
        schema_version: 1,
        source_sha256: value.source_sha256,
        items: value.items.map((item) => validateInputItem(item)),
        assistant_final_items: assistantFinalItems as string[],
        active_tail: activeTail as JsonRecord[],
      };
    } catch {
      return undefined;
    }
  }

  private async readHistoryCache(): Promise<InputHistoryCache | undefined> {
    try {
      return parseHistoryCache(JSON.parse(await readFile(this.historyCachePath, "utf8")));
    } catch {
      return undefined;
    }
  }

  private async initializeUsageCache(): Promise<void> {
    this.usageCache = await this.readUsageCache();
    if (this.usageCache === undefined) {
      this.usageCache = await this.rebuildUsageCache();
      await this.writeUsageCache();
      return;
    }
    await this.refreshUsageCacheUnlocked();
  }

  private async readUsageCache(): Promise<UsageCache | undefined> {
    try {
      return parseUsageCache(JSON.parse(await readFile(this.usageCachePath, "utf8")) as unknown);
    } catch {
      return undefined;
    }
  }

  private async writeUsageCache(): Promise<void> {
    if (this.usageCache === undefined) {
      throw new PersistenceError("usage cache is not initialized");
    }
    await mkdir(dirname(this.usageCachePath), {recursive: true});
    await writeJsonAtomic(this.usageCachePath, this.usageCache, this.options.durability);
  }

  private async makeUsageCheckpoint(
    current: TranscriptMetadata,
  ): Promise<UsageTranscriptCheckpoint> {
    const checkpointStart = Math.max(0, current.size - HISTORY_CHECKPOINT_BYTES);
    return {
      device: current.device,
      inode: current.inode,
      offset: current.size,
      modified_at_ms: current.modifiedAt,
      changed_at_ms: current.changedAt,
      checkpoint_start: checkpointStart,
      checkpoint_sha256: await hashFileRange(this.messagesPath, checkpointStart, current.size),
    };
  }

  private async rebuildUsageCache(): Promise<UsageCache> {
    const summary: SessionUsageSummary = {
      completed_responses: 0,
      responses_with_usage: 0,
      turn_count: 0,
      history_partial: (await this.readCompactionCache()) !== undefined,
    };
    const cache: UsageCache = {
      schema_version: USAGE_CACHE_SCHEMA_VERSION,
      transcript: await this.makeUsageCheckpoint(await this.readTranscriptMetadata()),
      summary,
    };
    const seenResponses = new Set<string>();
    const seenInputBatches = new Set<string>();
    const processFile = async (path: string): Promise<void> => {
      try {
        await readJsonLines(path, (line) => {
          try {
            const record = asObject(JSON.parse(line) as unknown, "usage transcript record");
            addUsageTranscriptRecord(cache, record, seenResponses, true, seenInputBatches);
          } catch {
            summary.history_partial = true;
          }
        });
      } catch (error) {
        if ((error as NodeJS.ErrnoException).code !== "ENOENT") {
          summary.history_partial = true;
        }
      }
    };
    await processFile(this.messagesBackupPath);
    await processFile(this.messagesPath);
    cache.transcript = await this.makeUsageCheckpoint(await this.readTranscriptMetadata());
    return cache;
  }

  private async usageCacheMatchesTranscript(
    cache: UsageCache,
    current: TranscriptMetadata,
  ): Promise<boolean> {
    return this.cacheMatches({transcript: cache.transcript}, current);
  }

  private async refreshUsageCacheUnlocked(): Promise<void> {
    if (this.usageCache === undefined) {
      this.usageCache = await this.rebuildUsageCache();
      await this.writeUsageCache();
      return;
    }

    const cache = this.usageCache;
    const current = await this.readTranscriptMetadata();
    if (await this.usageCacheMatchesTranscript(cache, current)) {
      if (current.size > cache.transcript.offset) {
        let countedUserSubmission = false;
        await readJsonLines(this.messagesPath, (line) => {
          try {
            const record = asObject(JSON.parse(line) as unknown, "usage transcript record");
            if (
              !countedUserSubmission &&
              record.role === "user" &&
              cache.summary.turn_count < Number.MAX_SAFE_INTEGER
            ) {
              cache.summary.turn_count += 1;
              countedUserSubmission = true;
            }
            addUsageTranscriptRecord(cache, record, undefined, false);
          } catch {
            cache.summary.history_partial = true;
          }
        }, cache.transcript.offset);
      }
      if (
        current.size !== cache.transcript.offset ||
        current.modifiedAt !== cache.transcript.modified_at_ms ||
        current.changedAt !== cache.transcript.changed_at_ms
      ) {
        cache.transcript = await this.makeUsageCheckpoint(await this.readTranscriptMetadata());
        await this.writeUsageCache();
      }
      return;
    }

    const compaction = await this.readCompactionCache();
    if (compaction !== undefined) {
      const compactedTranscriptMatches = await this.transcriptMatchesRecords(compaction.active_tail);
      if (!compactedTranscriptMatches) cache.summary.history_partial = true;
      cache.transcript = await this.makeUsageCheckpoint(await this.readTranscriptMetadata());
      await this.writeUsageCache();
      return;
    }

    this.usageCache = await this.rebuildUsageCache();
    await this.writeUsageCache();
  }

  private async transcriptMatchesRecords(records: readonly JsonRecord[]): Promise<boolean> {
    let index = 0;
    let matches = true;
    await readJsonLines(this.messagesPath, (line) => {
      if (!matches) return;
      try {
        const record = asObject(JSON.parse(line) as unknown, "compacted transcript record");
        if (index >= records.length || stringifyJson(record, "compacted transcript record") !== stringifyJson(records[index], "compaction cache record")) {
          matches = false;
          return;
        }
        index += 1;
      } catch {
        matches = false;
      }
    });
    return matches && index === records.length;
  }

  private async resetUsageCheckpointAfterCompactionUnlocked(): Promise<void> {
    if (this.usageCache === undefined) return;
    this.usageCache.transcript = await this.makeUsageCheckpoint(await this.readTranscriptMetadata());
    await this.writeUsageCache();
  }

  public async readUsageSummary(): Promise<SessionUsageSummary> {
    this.assertOpen();
    return this.messageMutex.runExclusive(async () => {
      await this.refreshUsageCacheUnlocked();
      const summary = this.usageCache!.summary;
      return {
        ...summary,
        ...(summary.current === undefined ? {} : {current: {...summary.current}}),
      };
    });
  }

  private async readTranscriptMetadata(): Promise<TranscriptMetadata> {
    const current = await statOrCreate(this.messagesPath);
    return {
      device: current.dev,
      inode: current.ino,
      size: Number(current.size),
      modifiedAt: Math.trunc(current.mtimeMs),
      changedAt: Math.trunc(current.ctimeMs),
    };
  }

  private async cacheMatches(
    cache: {transcript: UsageTranscriptCheckpoint},
    current: TranscriptMetadata,
  ): Promise<boolean> {
    if (
      current.size < cache.transcript.offset ||
      current.device !== cache.transcript.device ||
      current.inode !== cache.transcript.inode
    ) {
      return false;
    }
    if (current.size === cache.transcript.offset) {
      return (
        current.modifiedAt === cache.transcript.modified_at_ms &&
        current.changedAt === cache.transcript.changed_at_ms
      );
    }
    try {
      return (
        (await hashFileRange(
          this.messagesPath,
          cache.transcript.checkpoint_start,
          cache.transcript.offset,
        )) === cache.transcript.checkpoint_sha256
      );
    } catch {
      return false;
    }
  }

  private async readTranscriptItems(start: number): Promise<ResponseInputItem[]> {
    const items: ResponseInputItem[] = [];
    await readJsonLines(this.messagesPath, async (line) => {
      if (line.length === 0) {
        return;
      }
      let value: unknown;
      try {
        value = JSON.parse(line);
      } catch (error) {
        throw new PersistenceError(
          `invalid messages.jsonl record: ${error instanceof Error ? error.message : String(error)}`,
        );
      }
      items.push(...expandTranscriptRecord(value));
    }, start);
    return items;
  }

  private async makeHistoryCache(
    items: ResponseInputItem[],
    current: TranscriptMetadata,
  ): Promise<InputHistoryCache> {
    const offset = current.size;
    const checkpointStart = Math.max(0, offset - HISTORY_CHECKPOINT_BYTES);
    return {
      schema_version: HISTORY_CACHE_SCHEMA_VERSION,
      transcript: {
        device: current.device,
        inode: current.inode,
        offset,
        modified_at_ms: current.modifiedAt,
        changed_at_ms: current.changedAt,
        checkpoint_start: checkpointStart,
        checkpoint_sha256: await hashFileRange(this.messagesPath, checkpointStart, offset),
      },
      items,
    };
  }

  /**
   * Provider runs recorded in the transcript, one entry per committed response.
   *
   * Built once by a full scan, then maintained by reading only the bytes appended
   * since the last read. A run's extent is the next run's offset, so appends do
   * not invalidate existing entries and tool outputs need no entry of their own.
   */
  public async readCursorIndex(): Promise<TranscriptCursorIndex> {
    this.assertOpen();
    return this.messageMutex.runExclusive(async () => {
      const current = await this.readTranscriptMetadata();
      const cache = await this.refreshCursorCacheUnlocked(current);
      return {
        device: current.device,
        inode: current.inode,
        size: current.size,
        runs: cache.runs.map((run) => ({...run})),
      };
    });
  }

  /** The compacted request prefix, or `undefined` when the session never compacted. */
  public async readCompactionItems(): Promise<ResponseInputItem[] | undefined> {
    this.assertOpen();
    const cache = await this.readCompactionCache();
    return cache?.items.map((item) => ({...item}));
  }

  /** Transcript identity and size; creates an empty transcript when absent. */
  public async readTranscriptStamp(): Promise<TranscriptMetadata> {
    this.assertOpen();
    return this.readTranscriptMetadata();
  }

  /** Reads complete records in `[start, end)` with their byte offsets. */
  public async readRecords(
    start: number,
    end: number,
    onRecord: (offset: number, line: string) => void | Promise<void>,
  ): Promise<number> {
    this.assertOpen();
    return readTranscriptRecords(this.messagesPath, start, end, onRecord);
  }

  /**
   * Offset where the record preceding `offset` starts, found by scanning
   * backwards for the newline that ends the record before it. Used to include
   * the user message that opens a turn without walking the transcript from the
   * beginning. Returns `0` for the first record of the file.
   */
  public async readRecordStartBefore(offset: number): Promise<number | undefined> {
    this.assertOpen();
    return this.recordStartBefore(this.messagesPath, offset);
  }

  /**
   * Identity of the live transcript file, as opposed to its contents: it changes
   * when compaction replaces the file, and not when a record is appended. Used
   * to invalidate projections whose boundary is a compaction seam.
   */
  public async readTranscriptGeneration(): Promise<string | undefined> {
    this.assertOpen();
    const info = await stat(this.messagesPath).catch(() => undefined);
    return info === undefined
      ? undefined
      : [info.dev, info.ino, Math.trunc(info.birthtimeMs)].join(":");
  }

  /** Identity of the pre-compaction backup, or `undefined` when there is none. */
  public async readBackupStamp(): Promise<string | undefined> {
    this.assertOpen();
    const info = await stat(this.messagesBackupPath).catch(() => undefined);
    return info === undefined || info.size <= 0
      ? undefined
      : [info.dev, info.ino, info.size, Math.trunc(info.mtimeMs)].join(":");
  }

  /**
   * Byte range of the backup's last `turns` turns that the transcript does not
   * already carry, together with the records it holds.
   *
   * The end is the compaction seam: compaction rewrites the transcript with the
   * records it kept in flight, and the backup still holds those records at its
   * end, so rendering both would show the same message twice. The start is found
   * by walking backwards from the seam in growing windows until `turns`
   * turn-opening records are seen — proportional to the range that is about to
   * be read anyway. Those windows are what the caller renders, so the walk
   * returns them instead of leaving the caller to read the same bytes again;
   * each window starts on a record boundary, so no record is read in halves.
   * The records are held only for the call, and only for the returned range.
   */
  public async readBackupTail(turns: number): Promise<BackupTail | undefined> {
    this.assertOpen();
    if (!Number.isSafeInteger(turns) || turns < 0) {
      throw new PersistenceError("invalid backup tail length");
    }
    const info = await stat(this.messagesBackupPath).catch(() => undefined);
    if (info === undefined || info.size <= 0) {
      return undefined;
    }
    const end = await this.backupVisibleEnd(info.size);
    if (turns === 0) {
      return {start: end, end, records: []};
    }

    const records = new Map<number, string>();
    const userOffsets = new Set<number>();
    let upper = end;
    let window = BACKUP_TAIL_WINDOW_BYTES;
    let start = 0;
    while (upper > 0) {
      const raw = Math.max(0, upper - window);
      const from = raw === 0
        ? 0
        : (await this.recordStartBefore(this.messagesBackupPath, raw)) ?? 0;
      await readTranscriptRecords(this.messagesBackupPath, from, upper, (offset, line) => {
        // Windows can overlap once a start is aligned to a record boundary; the
        // first copy of a record is the one from the newer window.
        if (records.has(offset)) {
          return;
        }
        records.set(offset, line);
        if (isUserTurnRecordLine(line)) {
          userOffsets.add(offset);
        }
      });

      const ordered = [...userOffsets].sort((left, right) => right - left);
      if (ordered.length >= turns) {
        start = ordered[turns - 1]!;
        break;
      }
      if (from === 0) {
        break;
      }
      upper = from;
      window *= 2;
    }

    const tail: OffsetRecord[] = [];
    for (const offset of [...records.keys()].sort((left, right) => left - right)) {
      if (offset >= start) {
        tail.push({offset, line: records.get(offset)!});
      }
    }
    return {start, end, records: tail};
  }

  /**
   * The complete record ending just before `offset`, with its own offset.
   *
   * Readers that address the transcript by byte offset — a run cursor, a turn
   * boundary — use this to see the record in front of a known boundary without
   * walking the file from the beginning.
   */
  public async readRecordBefore(offset: number): Promise<OffsetRecord | undefined> {
    this.assertOpen();
    const record = await this.readRecordEndingAt(this.messagesPath, offset);
    if (record === undefined) {
      return undefined;
    }
    const length = record.bytes[record.bytes.length - 1] === 0x0a
      ? record.bytes.length - 1
      : record.bytes.length;
    return {offset: record.offset, line: record.bytes.subarray(0, length).toString("utf8")};
  }

  /**
   * End of the backup a reader may render.
   *
   * A compaction moves a suffix of the transcript into the new file verbatim,
   * and the backup keeps its copy at the end. The transcript's leading records
   * are compared with the backup's trailing ones byte for byte, and the matching
   * suffix is left out of the visible range.
   */
  private async backupVisibleEnd(size: number): Promise<number> {
    let end = size;
    let offset = 0;
    for (let matched = 0; matched < BACKUP_SEAM_RECORDS; matched += 1) {
      const leading = await this.readLineAt(this.messagesPath, offset);
      if (leading === undefined) {
        break;
      }
      const trailing = await this.lastLineEndingAt(this.messagesBackupPath, end);
      if (trailing === undefined || !leading.equals(trailing)) {
        break;
      }
      end -= leading.length;
      offset += leading.length;
      if (end <= 0) {
        break;
      }
    }
    return end;
  }

  /** Backward newline scan shared by the transcript and its backup. */
  private async recordStartBefore(path: string, offset: number): Promise<number | undefined> {
    if (!Number.isSafeInteger(offset) || offset <= 0) {
      return undefined;
    }
    // The byte before a record boundary terminates the previous record; the
    // newline searched for is the one before *that*.
    const limit = offset - 1;
    if (limit <= 0) {
      return 0;
    }
    const handle = await open(path, "r");
    try {
      const buffer = Buffer.alloc(LINE_BUFFER_SIZE);
      let cursor = limit;
      while (cursor > 0) {
        const start = Math.max(0, cursor - buffer.length);
        const length = cursor - start;
        const read = await handle.read(buffer, 0, length, start);
        if (read.bytesRead <= 0) {
          break;
        }
        const newline = buffer.subarray(0, read.bytesRead).lastIndexOf(0x0a);
        if (newline >= 0) {
          return start + newline + 1;
        }
        cursor = start;
      }
      return 0;
    } finally {
      await handle.close();
    }
  }

  /** One record read from `offset`, including its newline; `undefined` at EOF. */
  private async readLineAt(path: string, offset: number): Promise<Buffer | undefined> {
    const handle = await open(path, "r");
    try {
      const chunks: Buffer[] = [];
      const buffer = Buffer.alloc(LINE_BUFFER_SIZE);
      let position = offset;
      let total = 0;
      while (total <= MAX_SEAM_LINE_BYTES) {
        const read = await handle.read(buffer, 0, buffer.length, position);
        if (read.bytesRead <= 0) {
          break;
        }
        const newline = buffer.subarray(0, read.bytesRead).indexOf(0x0a);
        const take = newline >= 0 ? newline + 1 : read.bytesRead;
        chunks.push(Buffer.from(buffer.subarray(0, take)));
        position += take;
        total += take;
        if (newline >= 0) {
          return Buffer.concat(chunks, total);
        }
      }
      return undefined;
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code === "ENOENT") {
        return undefined;
      }
      throw error;
    } finally {
      await handle.close();
    }
  }

  /** The last complete record ending at `end`, including its newline. */
  private async lastLineEndingAt(path: string, end: number): Promise<Buffer | undefined> {
    const record = await this.readRecordEndingAt(path, end);
    return record?.bytes;
  }

  /** Byte range of the last complete record ending at `end`, with its offset. */
  private async readRecordEndingAt(
    path: string,
    end: number,
  ): Promise<{offset: number; bytes: Buffer} | undefined> {
    if (end <= 0) {
      return undefined;
    }
    const start = await this.recordStartBefore(path, end);
    if (start === undefined || start >= end) {
      return undefined;
    }
    const handle = await open(path, "r");
    try {
      const bytes = Buffer.alloc(end - start);
      let filled = 0;
      while (filled < bytes.length) {
        const read = await handle.read(bytes, filled, bytes.length - filled, start + filled);
        if (read.bytesRead <= 0) {
          break;
        }
        filled += read.bytesRead;
      }
      return filled === bytes.length ? {offset: start, bytes} : undefined;
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code === "ENOENT") {
        return undefined;
      }
      throw error;
    } finally {
      await handle.close();
    }
  }

  private async readCursorCacheFile(): Promise<CursorCache | undefined> {
    try {
      return parseCursorCache(JSON.parse(await readFile(this.cursorCachePath, "utf8")) as unknown);
    } catch {
      return undefined;
    }
  }

  private async makeCursorCache(
    runs: readonly TranscriptRunCursor[],
    offset: number,
    current: TranscriptMetadata,
  ): Promise<CursorCache> {
    const checkpointStart = Math.max(0, offset - HISTORY_CHECKPOINT_BYTES);
    return {
      schema_version: CURSOR_CACHE_SCHEMA_VERSION,
      transcript: {
        device: current.device,
        inode: current.inode,
        offset,
        modified_at_ms: current.modifiedAt,
        changed_at_ms: current.changedAt,
        checkpoint_start: checkpointStart,
        checkpoint_sha256: await hashFileRange(this.messagesPath, checkpointStart, offset),
      },
      runs: [...runs],
    };
  }

  /** Collects `response.completed` records in `[start, end)` as run cursors. */
  private async scanRuns(
    start: number,
    end: number,
    onRun: (run: TranscriptRunCursor) => void,
  ): Promise<number> {
    return readTranscriptRecords(this.messagesPath, start, end, (offset, line) => {
      // A committed response is the only record that opens a provider run, and
      // the marker keeps the scan from parsing every other line.
      if (!line.includes(COMPLETED_RECORD_MARKER)) {
        return;
      }
      let value: unknown;
      try {
        value = JSON.parse(line) as unknown;
      } catch {
        return;
      }
      if (!isRecord(value) || value.type !== "response.completed") {
        return;
      }
      const turn = value.session_turn;
      const step = value.session_step;
      if (
        !Number.isSafeInteger(turn) ||
        !Number.isSafeInteger(step) ||
        (turn as number) < 1 ||
        (step as number) < 1
      ) {
        return;
      }
      onRun({offset, turn: turn as number, step: step as number});
    });
  }

  private async writeCursorCacheFile(cache: CursorCache): Promise<void> {
    await mkdir(dirname(this.cursorCachePath), {recursive: true});
    await writeJsonAtomic(this.cursorCachePath, cache, this.options.durability);
  }

  private async refreshCursorCacheUnlocked(current: TranscriptMetadata): Promise<CursorCache> {
    const cached = this.cursorCache ?? (await this.readCursorCacheFile());
    if (cached !== undefined && (await this.cacheMatches(cached, current))) {
      if (current.size === cached.transcript.offset) {
        this.cursorCache = cached;
        return cached;
      }
      const runs = [...cached.runs];
      const offset = await this.scanRuns(
        cached.transcript.offset,
        current.size,
        (run) => runs.push(run),
      );
      const appended = await this.makeCursorCache(runs, offset, current);
      this.cursorCache = appended;
      await this.writeCursorCacheFile(appended);
      return appended;
    }

    const runs: TranscriptRunCursor[] = [];
    const offset = await this.scanRuns(0, current.size, (run) => runs.push(run));
    const rebuilt = await this.makeCursorCache(runs, offset, current);
    this.cursorCache = rebuilt;
    await this.writeCursorCacheFile(rebuilt);
    return rebuilt;
  }

  public async compactHistory(options: {
    turnType: "assistant_final" | "full";
    keepTurns: number;
    keepToolPairs: number;
    backupMode: "replace" | "append";
    preserveResponseId?: string;
  }): Promise<{compacted: boolean; compacted_items: number; retained_items: number; backup_path: string}> {
    this.assertOpen();
    if (!Number.isSafeInteger(options.keepTurns) || options.keepTurns < 0) {
      throw new PersistenceError("keepTurns must be a non-negative safe integer");
    }
    if (!Number.isSafeInteger(options.keepToolPairs) || options.keepToolPairs < 0) {
      throw new PersistenceError("keepToolPairs must be a non-negative safe integer");
    }
    if (options.turnType !== "assistant_final" && options.turnType !== "full") {
      throw new PersistenceError("turnType must be assistant_final or full");
    }
    if (options.backupMode !== "replace" && options.backupMode !== "append") {
      throw new PersistenceError("backupMode must be replace or append");
    }
    if (this.activeTurns.size > 0) throw new PersistenceError("cannot compact while a response turn is active");
    return this.messageMutex.runExclusive(async () => {
      await repairJsonlTail(this.messagesPath);
      await this.refreshUsageCacheUnlocked();
      const history = await this.loadInputHistoryUnlocked();
      const sourceHash = await hashFile(this.messagesPath).catch(() => createHash("sha256").update("").digest("hex"));
      const previousCompaction = await this.readCompactionCache();
      const finalByTurn = new Map<number, string>();
      const priorFinalItems = new Set(previousCompaction?.assistant_final_items ?? []);
      let turnIndex = -1;
      for (const item of previousCompaction?.items ?? []) {
        if (item.role === "user") turnIndex += 1;
        if (turnIndex < 0) turnIndex = 0;
        if (item.type === "message" && item.role === "assistant") {
          const key = stringifyJson(item, "cached assistant message");
          if (priorFinalItems.has(key)) finalByTurn.set(turnIndex, key);
        }
      }

      const activeTail: JsonRecord[] = [];
      const excludedCounts = new Map<string, number>();
      await readJsonLines(this.messagesPath, (line) => {
        let record: unknown;
        try { record = JSON.parse(line); } catch { throw new PersistenceError("invalid messages.jsonl record during compaction"); }
        if (isRecord(record) && record.role === "user") {
          turnIndex += 1;
          return;
        }
        if (!isRecord(record) || record.type !== "response.completed" || !isRecord(record.response)) return;
        const response = record.response;
        if (response.id === options.preserveResponseId) {
          activeTail.push(record);
          if (Array.isArray(response.output)) {
            for (const item of response.output) {
              const key = stringifyJson(item, "in-flight response item");
              excludedCounts.set(key, (excludedCounts.get(key) ?? 0) + 1);
            }
          }
        }
        if (!Array.isArray(response.output)) return;
        if (response.output.some((item) => isRecord(item) && item.type === "function_call")) {
          finalByTurn.delete(Math.max(turnIndex, 0));
          return;
        }
        if (response.status !== "completed") return;
        const assistant = [...response.output].reverse().find((item) => isRecord(item) && item.type === "message" && item.role === "assistant");
        if (isRecord(assistant)) finalByTurn.set(Math.max(turnIndex, 0), stringifyJson(assistant, "assistant final"));
      });
      if (options.preserveResponseId && activeTail.length === 0) {
        throw new PersistenceError("cannot preserve a response that is missing from messages.jsonl");
      }
      const finalAssistant = new Set(finalByTurn.values());
      const compactableHistory = history.filter((item) => {
        const key = stringifyJson(item, "history item");
        const remaining = excludedCounts.get(key) ?? 0;
        if (remaining === 0) return true;
        excludedCounts.set(key, remaining - 1);
        return false;
      });
      const activeTailItems = activeTail.flatMap((record) => expandTranscriptRecord(record));

      const turns: ResponseInputItem[][] = [];
      for (const item of compactableHistory) {
        if (item.role === "user") turns.push([]);
        if (turns.length === 0) turns.push([]);
        turns[turns.length - 1]!.push(item);
      }
      const selectedTurns = options.keepTurns === 0 ? [] : turns.slice(-options.keepTurns);
      const callById = new Map<string, ResponseInputItem>();
      const outputById = new Map<string, ResponseInputItem>();
      for (const turn of selectedTurns) for (const item of turn) {
        if (item.type === "function_call" && typeof item.call_id === "string") callById.set(item.call_id, item);
        if (item.type === "function_call_output" && typeof item.call_id === "string") outputById.set(item.call_id, item);
      }
      const completePairIds = options.turnType === "full" || options.keepToolPairs === 0
        ? []
        : [...callById.keys()].filter((id) => outputById.has(id)).slice(-options.keepToolPairs);
      const retained = selectedTurns.flatMap((turn) => {
        const assistantFinal = [...turn].reverse().find((item) => item.type === "message" && item.role === "assistant" && finalAssistant.has(stringifyJson(item, "assistant message")));
        return turn.filter((item) => {
          if (item.type === "function_call" || item.type === "function_call_output") return typeof item.call_id === "string" && completePairIds.includes(item.call_id);
          if (options.turnType === "assistant_final") return item.role === "user" || item === assistantFinal;
          return item.role === "user" || item.role === "assistant" || item.type === "reasoning";
        });
      });

      const nextHistory = [...retained, ...activeTailItems];
      const retainedBytes = Buffer.byteLength(stringifyJson(nextHistory, "compacted request history"), "utf8");
      const historyBytes = Buffer.byteLength(stringifyJson(history, "history items"), "utf8");
      if (retainedBytes >= historyBytes) {
        return {compacted: false, compacted_items: history.length, retained_items: history.length, backup_path: this.messagesBackupPath};
      }

      await mkdir(dirname(this.messagesBackupPath), {recursive: true});
      const backupTemp = `${this.messagesBackupPath}.tmp-${process.pid}-${randomBytes(6).toString("hex")}`;
      try {
        if (options.backupMode === "append") {
          await appendFileIncrementally(this.messagesBackupPath, backupTemp, this.options.durability).catch((error) => {
            if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error;
          });
        }
        await appendFileIncrementally(this.messagesPath, backupTemp, this.options.durability);
        await rename(backupTemp, this.messagesBackupPath);
      } finally { await rm(backupTemp, {force: true}).catch(() => undefined); }

      const retainedAssistantFinals = retained
        .filter((item) => item.type === "message" && item.role === "assistant")
        .map((item) => stringifyJson(item, "retained assistant message"))
        .filter((item) => finalAssistant.has(item));
      const cache: CompactionCache = {
        schema_version: 1,
        source_sha256: sourceHash,
        items: retained,
        assistant_final_items: retainedAssistantFinals,
        active_tail: activeTail,
      };
      await mkdir(dirname(this.compactionCachePath), {recursive: true});
      // Publish the recovery copy before the primary cache. If the transcript
      // replacement is interrupted, either valid cache can finish the transaction.
      await writeJsonAtomic(this.compactionCacheBackupPath, cache, this.options.durability);
      await writeJsonAtomic(this.compactionCachePath, cache, this.options.durability);
      await this.replaceMessagesWithRecords(activeTail);
      this.historyCache = undefined;
      this.cursorCache = undefined;
      await rm(this.historyCachePath, {force: true});
      await rm(this.cursorCachePath, {force: true});
      await this.resetUsageCheckpointAfterCompactionUnlocked();
      return {compacted: true, compacted_items: history.length, retained_items: nextHistory.length, backup_path: this.messagesBackupPath};
    });
  }

  public async recover(): Promise<RecoveryReport> {
    this.assertOpen();
    if (this.activeTurns.size > 0) {
      throw new PersistenceError("close active turns before recovery");
    }

    return this.messageMutex.runExclusive(async () => {
      const repairedMessageBytes = await repairJsonlTail(this.messagesPath);
      const report: RecoveryReport = {
        repaired_message_bytes: repairedMessageBytes,
        inspected_turns: 0,
        retained_turns: 0,
        committed_turns: 0,
        removed_turns: 0,
        errors: [],
      };

      const entries = await readdir(this.stagingDir, { withFileTypes: true });
      for (const entry of entries) {
        if (!entry.isDirectory()) {
          continue;
        }
        report.inspected_turns += 1;
        try {
          const result = await this.recoverTurnUnlocked(join(this.stagingDir, entry.name));
          if (result === "committed") {
            report.committed_turns += 1;
            report.removed_turns += 1;
          } else if (result === "removed") {
            report.removed_turns += 1;
          } else {
            report.retained_turns += 1;
          }
        } catch (error) {
          report.retained_turns += 1;
          report.errors.push(
            `${entry.name}: ${error instanceof Error ? error.message : String(error)}`,
          );
        }
      }
      await this.refreshUsageCacheUnlocked();
      return report;
    });
  }

  public async *readCompletedResponses(): AsyncGenerator<ResponseCompletedEvent> {
    this.assertOpen();
    await repairJsonlTail(this.messagesPath);
    const input = createReadStream(this.messagesPath, { encoding: "utf8" });
    const lines = createInterface({ input, crlfDelay: Infinity });
    try {
      for await (const line of lines) {
        if (line.length === 0) {
          continue;
        }
        let value: unknown;
        try {
          value = JSON.parse(line);
        } catch (error) {
          throw new PersistenceError(
            `invalid messages.jsonl record: ${error instanceof Error ? error.message : String(error)}`,
          );
        }
        if (isRecord(value) && value.type === "response.completed") {
          yield value as unknown as ResponseCompletedEvent;
        }
      }
    } finally {
      lines.close();
      input.destroy();
    }
  }

  public async close(): Promise<void> {
    if (this.closed) {
      return;
    }
    for (const turn of [...this.activeTurns]) {
      await turn.close();
    }
    this.closed = true;
  }

  public unregister(turn: ResponseTurn): void {
    this.activeTurns.delete(turn);
  }

  public async appendRenderedLine(
    finalPath: string,
    lineHash: string,
  ): Promise<boolean> {
    this.assertOpen();
    return this.messageMutex.runExclusive(() =>
      this.appendRenderedLineUnlocked(finalPath, lineHash),
    );
  }

  private async appendRenderedLineUnlocked(
    finalPath: string,
    lineHash: string,
  ): Promise<boolean> {
    await repairJsonlTail(this.messagesPath);
    const alreadyPresent = await containsLineHash(this.messagesPath, lineHash);
    if (!alreadyPresent) {
      await appendFileIncrementally(finalPath, this.messagesPath, this.options.durability);
    }
    await this.refreshUsageCacheUnlocked();
    return !alreadyPresent;
  }

  private async recoverTurnUnlocked(turnDir: string): Promise<"committed" | "removed" | "retained"> {
    const manifestPath = join(turnDir, MANIFEST_FILE);
    const itemsPath = join(turnDir, ITEMS_FILE);
    const indexPath = join(turnDir, ITEMS_INDEX_FILE);
    const finalPath = join(turnDir, FINAL_FILE);

    const manifest = parseManifest(
      JSON.parse(await readFile(manifestPath, "utf8")),
      manifestPath,
    );

    await repairJsonlTail(itemsPath);
    await repairJsonlTail(indexPath);

    const finalExists = await fileExists(finalPath);
    if (finalExists) {
      if (!(await isNewlineTerminated(finalPath))) {
        throw new PersistenceError("staged final record is incomplete");
      }
      const actualHash = await hashFile(finalPath);
      if (
        manifest.canonical_line_sha256 !== undefined &&
        manifest.canonical_line_sha256 !== actualHash
      ) {
        throw new PersistenceError("staged final record hash does not match manifest");
      }

      if (!(await containsLineHash(this.messagesPath, actualHash))) {
        await appendFileIncrementally(finalPath, this.messagesPath, this.options.durability);
      }

      manifest.canonical_line_sha256 = actualHash;
      manifest.status = "committed";
      await writeJsonAtomic(manifestPath, manifest, this.options.durability);
      await rm(turnDir, { recursive: true, force: true });
      return "committed";
    }

    if (manifest.status === "committed" && manifest.canonical_line_sha256 !== undefined) {
      if (await containsLineHash(this.messagesPath, manifest.canonical_line_sha256)) {
        await rm(turnDir, { recursive: true, force: true });
        return "removed";
      }
      throw new PersistenceError("committed manifest has no matching messages.jsonl record");
    }

    return "retained";
  }

  private assertOpen(): void {
    if (this.closed) {
      throw new PersistenceError("session response store is closed");
    }
  }

  public get maxItemBytes(): number {
    return this.options.maxItemBytes;
  }

  public get responseMetadata(): ResponseMetadataMode {
    return this.options.responseMetadata;
  }
}

export class ResponseTurn {
  private readonly operationMutex = new AsyncMutex();
  private itemHandle: FileHandle | undefined;
  private indexHandle: FileHandle | undefined;
  private closed = false;

  public constructor(
    private readonly store: SessionResponseStore,
    public readonly turnDir: string,
    private readonly itemsPath: string,
    private readonly indexPath: string,
    private readonly manifestPath: string,
    private manifest: TurnManifest,
    private readonly itemIndex: Map<number, StagedItemIndexEntry>,
  ) {}

  public get status(): TurnStatus {
    return this.manifest.status;
  }

  public get responseId(): string | undefined {
    return this.manifest.response_id;
  }

  public async stageOutputItem(event: ResponseOutputItemDoneEvent): Promise<void> {
    await this.operationMutex.runExclusive(async () => {
      this.assertWritable();
      if (!isRecord(event) || event.type !== "response.output_item.done") {
        throw new PersistenceError("expected a response.output_item.done event");
      }
      const item = validateOutputItem(event.item);
      const itemJson = stringifyJson(item, "output item");
      const serializedItemBytes = Buffer.byteLength(itemJson, "utf8");
      if (serializedItemBytes > this.store.maxItemBytes) {
        throw new PersistenceError(
          `output item is ${serializedItemBytes} bytes, exceeding maxItemBytes ${this.store.maxItemBytes}`,
        );
      }

      let outputIndex = event.output_index;
      if (outputIndex === undefined) {
        outputIndex = this.manifest.next_sequence;
        while (this.itemIndex.has(outputIndex)) {
          outputIndex += 1;
        }
      }
      validateOutputIndex(outputIndex);
      if (this.itemIndex.has(outputIndex)) {
        throw new PersistenceError(`output_index ${outputIndex} has already been staged`);
      }

      await this.ensureHandles();
      const itemRecord: StagedOutputItemRecord = {
        output_index: outputIndex,
        item,
      };
      const itemLine = `${stringifyJson(itemRecord, "staged output item")}\n`;
      const itemBytes = Buffer.from(itemLine, "utf8");
      const itemStat = await this.itemHandle!.stat();
      const itemOffset = Number(itemStat.size);
      await writeAll(this.itemHandle!, itemBytes);

      const indexRecord: StagedItemIndexEntry = {
        output_index: outputIndex,
        offset: itemOffset,
        length: itemBytes.length,
      };
      const indexLine = `${stringifyJson(indexRecord, "staging index record")}\n`;
      await writeAll(this.indexHandle!, Buffer.from(indexLine, "utf8"));
      await syncIfRequired(this.itemHandle!, this.store.durability === "strict" ? "strict" : "process");
      await syncIfRequired(this.indexHandle!, this.store.durability === "strict" ? "strict" : "process");

      this.itemIndex.set(outputIndex, indexRecord);
      this.manifest.item_count = this.itemIndex.size;
      this.manifest.next_sequence = Math.max(this.manifest.next_sequence, outputIndex + 1);
      await this.persistManifest();
    });
  }

  public async commit(event: ResponseCompletedEvent): Promise<CommitResult> {
    return this.operationMutex.runExclusive(async () => {
      this.assertWritable();
      const response = validateCompletedEvent(event);
      const completedResponseId = responseId(response);
      if (
        this.manifest.response_id !== undefined &&
        completedResponseId !== undefined &&
        this.manifest.response_id !== completedResponseId
      ) {
        throw new PersistenceError("response ID does not match the active turn");
      }
      if (completedResponseId !== undefined) {
        this.manifest.response_id = completedResponseId;
      }

      const responseOutput = response.output;
      const expectedItemCount = Array.isArray(responseOutput)
        ? responseOutput.length
        : this.itemIndex.size;
      if (this.itemIndex.size !== expectedItemCount) {
        throw new PersistenceError(
          `staged item count ${this.itemIndex.size} does not match completed output count ${expectedItemCount}`,
        );
      }
      for (let outputIndex = 0; outputIndex < expectedItemCount; outputIndex += 1) {
        if (!this.itemIndex.has(outputIndex)) {
          throw new PersistenceError(`missing staged output_index ${outputIndex}`);
        }
      }

      this.manifest.status = "ready";
      await this.persistManifest();
      await this.syncStageFiles();

      this.manifest.status = "committing";
      await this.persistManifest();
      const finalPath = join(this.turnDir, FINAL_FILE);
      const rendered = await renderCanonicalCompletion(
        response,
        this.itemIndex,
        this.itemsPath,
        finalPath,
        expectedItemCount,
        this.store.durability,
        this.store.responseMetadata,
        event.session_turn,
        event.session_step,
      );
      this.manifest.canonical_line_sha256 = rendered.hash;
      await this.persistManifest();

      const alreadyPresent = !(await this.store.appendRenderedLine(finalPath, rendered.hash));
      this.manifest.status = "committed";
      await this.persistManifest();
      await this.closeHandles();
      this.closed = true;
      this.store.unregister(this);
      await rm(this.turnDir, { recursive: true, force: true });

      return {
        line_hash: rendered.hash,
        bytes: rendered.bytes,
        already_present: alreadyPresent,
      };
    });
  }

  public async close(): Promise<void> {
    await this.operationMutex.runExclusive(async () => {
      if (this.closed) {
        return;
      }
      await this.closeHandles();
      this.closed = true;
      this.store.unregister(this);
    });
  }

  private async ensureHandles(): Promise<void> {
    this.itemHandle ??= await open(this.itemsPath, "a");
    this.indexHandle ??= await open(this.indexPath, "a");
  }

  private async syncStageFiles(): Promise<void> {
    if (this.itemHandle !== undefined) {
      await syncIfRequired(this.itemHandle, this.store.durability);
    }
    if (this.indexHandle !== undefined) {
      await syncIfRequired(this.indexHandle, this.store.durability);
    }
  }

  private async persistManifest(): Promise<void> {
    await writeJsonAtomic(this.manifestPath, this.manifest, this.store.durability);
  }

  private async closeHandles(): Promise<void> {
    const itemHandle = this.itemHandle;
    const indexHandle = this.indexHandle;
    this.itemHandle = undefined;
    this.indexHandle = undefined;
    await itemHandle?.close();
    await indexHandle?.close();
  }

  private assertWritable(): void {
    if (this.closed) {
      throw new PersistenceError("turn is closed");
    }
    if (this.manifest.status !== "streaming") {
      throw new PersistenceError(`turn is not writable in status ${this.manifest.status}`);
    }
  }
}

async function fileExists(path: string): Promise<boolean> {
  try {
    const handle = await open(path, "r");
    await handle.close();
    return true;
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") {
      return false;
    }
    throw error;
  }
}
