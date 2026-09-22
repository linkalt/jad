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
import { createInterface } from "node:readline";
import { dirname, isAbsolute, join, resolve } from "node:path";

import { AsyncMutex } from "./concurrency.js";

export type JsonRecord = Record<string, unknown>;
export type ResponseInputItem = JsonRecord;

export type DurabilityMode = "process" | "turn" | "strict";

export interface SessionResponseStoreOptions {
  /** Defaults to `turn`. */
  durability?: DurabilityMode;
  /** Maximum serialized output-item size in bytes. */
  maxItemBytes?: number;
  /** Transcript path; relative values resolve from the session directory. */
  messagesPath?: string;
  /** Staging directory; relative values resolve from the session directory. */
  stagingDir?: string;
}

export interface ResponseOutputItemDoneEvent {
  type: "response.output_item.done";
  output_index?: number;
  item: JsonRecord;
}

export interface ResponseCompletedEvent {
  type: "response.completed";
  response: JsonRecord;
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

const STAGING_DIRECTORY = "staging";
const MESSAGES_FILE = "messages.jsonl";
const MANIFEST_FILE = "manifest.json";
const ITEMS_FILE = "items.jsonl";
const ITEMS_INDEX_FILE = "items.index.jsonl";
const FINAL_FILE = "final.jsonl";
const HISTORY_CACHE_FILE = "history-cache.json";
const HISTORY_CACHE_SCHEMA_VERSION = 1;
const HISTORY_CHECKPOINT_BYTES = 64 * 1024;
const LINE_BUFFER_SIZE = 64 * 1024;

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

async function renderCanonicalCompletion(
  response: JsonRecord,
  itemIndex: Map<number, StagedItemIndexEntry>,
  itemsPath: string,
  finalPath: string,
  expectedItemCount: number,
  durability: DurabilityMode,
): Promise<RenderedLine> {
  const metadata: JsonRecord = {};
  for (const [key, value] of Object.entries(response)) {
    if (key !== "output") {
      metadata[key] = value;
    }
  }

  const metadataJson = stringifyJson(metadata, "response metadata");
  if (!metadataJson.endsWith("}")) {
    throw new PersistenceError("serialized response metadata is not an object");
  }

  const prefix =
    `{"type":"response.completed","response":${metadataJson.slice(0, -1)}` +
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

  private readonly options: Required<Pick<SessionResponseStoreOptions, "durability" | "maxItemBytes">>;
  private historyCache: InputHistoryCache | undefined;
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
    this.historyCachePath = join(this.stagingDir, HISTORY_CACHE_FILE);
    this.options = {
      durability: options.durability ?? "turn",
      maxItemBytes: options.maxItemBytes ?? Number.MAX_SAFE_INTEGER,
    };

    if (!Number.isSafeInteger(this.options.maxItemBytes) || this.options.maxItemBytes <= 0) {
      throw new PersistenceError("maxItemBytes must be a positive safe integer");
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
    await repairJsonlTail(store.messagesPath);
    return store;
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
   * Appends raw Responses API input items. Input lines intentionally have no
   * harness envelope so they can be used directly when reconstructing the next
   * Responses API request.
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
    await this.messageMutex.runExclusive(async () => {
      await repairJsonlTail(this.messagesPath);
      const handle = await open(this.messagesPath, "a");
      try {
        for (const item of validatedItems) {
          const line = `${stringifyJson(item, "input item")}\n`;
          await writeAll(handle, Buffer.from(line, "utf8"));
        }
        await syncIfRequired(handle, this.options.durability);
      } finally {
        await handle.close();
      }
    });
  }

  /**
   * Reconstructs request history from the append-only transcript. Raw input
   * items are yielded as-is; each committed response contributes its ordered
   * `response.output` items.
   */
  public async *readInputItems(): AsyncGenerator<ResponseInputItem> {
    this.assertOpen();
    const items = await this.loadInputHistory();
    yield* items;
  }

  private async loadInputHistory(): Promise<ResponseInputItem[]> {
    return this.messageMutex.runExclusive(async () => {
      await repairJsonlTail(this.messagesPath);
      const current = await this.readTranscriptMetadata();
      const cached = this.historyCache ?? (await this.readHistoryCache());
      const valid = cached !== undefined && (await this.cacheMatches(cached, current));
      if (valid && current.size === cached!.transcript.offset) {
        return cached!.items;
      }
      const start = valid ? cached!.transcript.offset : 0;
      // Never mutate the prior cache until its replacement is durable.
      const items = valid ? [...cached!.items] : [];

      if (current.size > start) {
        items.push(...(await this.readTranscriptItems(start)));
      }

      const cache = await this.makeHistoryCache(items, current);
      this.historyCache = cache;
      await writeJsonAtomic(this.historyCachePath, cache, this.options.durability);
      return cache.items;
    });
  }

  private async readHistoryCache(): Promise<InputHistoryCache | undefined> {
    try {
      return parseHistoryCache(JSON.parse(await readFile(this.historyCachePath, "utf8")));
    } catch {
      return undefined;
    }
  }

  private async readTranscriptMetadata(): Promise<TranscriptMetadata> {
    const current = await stat(this.messagesPath, {bigint: false});
    if (current === undefined) {
      throw new PersistenceError("unable to stat messages.jsonl");
    }
    return {
      device: current.dev,
      inode: current.ino,
      size: Number(current.size),
      modifiedAt: Math.trunc(current.mtimeMs),
      changedAt: Math.trunc(current.ctimeMs),
    };
  }

  private async cacheMatches(
    cache: InputHistoryCache,
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
    if (await containsLineHash(this.messagesPath, lineHash)) {
      return false;
    }
    await appendFileIncrementally(finalPath, this.messagesPath, this.options.durability);
    return true;
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
