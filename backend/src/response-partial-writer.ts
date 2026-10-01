import {mkdir, open, rename, rm, type FileHandle} from "node:fs/promises";
import {dirname, join} from "node:path";

import {
  acquireResponsePartialLease,
  releaseResponsePartialLease,
  RESPONSE_PARTIAL_COMPACT_FILE,
} from "./response-partial-files.js";

export interface ResponsePartialWriterOptions {
  path: string;
  /** Target batch size, measured using conservative UTF-8 content bytes. */
  batchBytes: number;
  /** Maximum buffered plus in-flight UTF-8 content charge. */
  maxBufferedBytes: number;
  /** Maximum delay before flushing a partial content append batch. */
  flushIntervalMs: number;
  /** Compact the in-memory range index after this many redundant ranges accrue. */
  maxIndexSegments: number;
  onError?: (error: unknown) => void;
}

interface Utf8Slice {
  value: string;
  codeUnits: number;
  bytes: number;
}

interface BufferedChunk {
  itemKey: string;
  value: string;
  bytes: number;
}

interface FileRange {
  start: number;
  length: number;
}

interface BatchRange extends FileRange {
  itemKey: string;
}

/** One content-only append file with a bounded in-memory range index per item. */
export class ResponsePartialWriter {
  private file: FileHandle | undefined;
  private chunks: BufferedChunk[] = [];
  private batchBytes = 0;
  private queuedBytes = 0;
  private fileBytes = 0;
  private readonly ranges = new Map<string, FileRange[]>();
  private writeTail: Promise<void> = Promise.resolve();
  private admissionTail: Promise<void> = Promise.resolve();
  private flushTimer: NodeJS.Timeout | undefined;
  private failure: unknown;
  private disposed = false;
  private leaseAcquired = false;
  private failureReported = false;
  private readonly capacityWaiters = new Set<{
    resolve: () => void;
    reject: (error: unknown) => void;
  }>();

  public constructor(private readonly options: ResponsePartialWriterOptions) {}

  /** Accepts an ordered item delta and waits only when the shared buffer is full. */
  public append(itemKey: string, value: string): Promise<void> {
    if (value.length === 0) return Promise.resolve();
    return this.enqueueAdmission(() => this.appendOrdered(itemKey, value));
  }

  /** Admits initial in-memory contents sequentially as one writer operation. */
  public appendItems(items: readonly {itemKey: string; takeContent: () => string}[]): Promise<void> {
    if (items.length === 0) return Promise.resolve();
    return this.enqueueAdmission(async () => {
      for (const item of items) {
        const content = item.takeContent();
        if (content.length > 0) await this.appendOrdered(item.itemKey, content);
      }
    });
  }

  /** Replaces one active item's content without exposing stale file ranges. */
  public replace(itemKey: string, value: string): Promise<void> {
    return this.enqueueAdmission(() => this.replaceOrdered(itemKey, value));
  }

  /** Reads a linearized snapshot of all active items. */
  public readItems(): Promise<ReadonlyMap<string, string>> {
    return this.enqueueAdmission(async () => {
      this.clearFlushTimer();
      this.flushBatch();
      await this.writeTail;
      this.assertHealthy();
      return await this.readItemsOrdered();
    });
  }

  /** Discards staged items and compacts remaining content into the same file. */
  public removeItems(itemKeys: ReadonlySet<string>): Promise<void> {
    if (itemKeys.size === 0) return Promise.resolve();
    return this.enqueueAdmission(async () => {
      this.clearFlushTimer();
      this.discardBufferedItems(itemKeys);
      this.flushBatch();
      await this.writeTail;
      this.assertHealthy();

      for (const key of itemKeys) this.ranges.delete(key);
      if (this.ranges.size === 0) {
        await this.clearDataFiles();
      } else {
        await this.compactFile();
      }
    });
  }

  /** Completes all accepted appends and starts the required disk work. */
  public async flush(): Promise<void> {
    this.clearFlushTimer();
    await this.admissionTail;
    this.assertHealthy();
    this.flushBatch();
    await this.writeTail;
    this.assertHealthy();
  }

  /** Closes the writer and removes its ephemeral content and ownership files. */
  public async dispose(): Promise<void> {
    if (this.disposed) return;
    this.disposed = true;
    this.clearFlushTimer();
    this.discardBufferedItems(new Set(this.chunks.map((chunk) => chunk.itemKey)));
    try {
      await this.writeTail;
    } catch {
      // Cache cleanup is best-effort; the failure was already reported.
    }
    try {
      await this.file?.close();
    } catch (error) {
      this.reportError(error);
    }
    this.file = undefined;
    await this.clearDataFiles();
    this.releaseCapacityWaiters();
  }

  private enqueueAdmission<T>(operation: () => Promise<T>): Promise<T> {
    const accepted = this.admissionTail.then(() => {
      this.assertHealthy();
      if (this.disposed) throw new Error("response partial writer is disposed");
      return operation();
    });
    this.admissionTail = accepted.then(() => undefined, () => undefined);
    return accepted;
  }

  private async appendOrdered(itemKey: string, value: string): Promise<void> {
    let offset = 0;
    const capacity = Math.max(1, this.options.maxBufferedBytes);
    const batchLimit = Math.max(2, Math.min(this.options.batchBytes, capacity));

    while (offset < value.length) {
      this.assertHealthy();
      if (this.queuedBytes >= capacity) {
        this.flushBatch();
        await this.waitForCapacity(capacity);
        continue;
      }

      const available = Math.max(1, Math.min(batchLimit - this.batchBytes, capacity - this.queuedBytes));
      const slice = utf8Slice(value, offset, available);
      this.chunks.push({itemKey, value: slice.value, bytes: slice.bytes});
      this.batchBytes += slice.bytes;
      this.queuedBytes += slice.bytes;
      offset += slice.codeUnits;

      if (this.batchBytes >= batchLimit || this.queuedBytes >= capacity) {
        this.clearFlushTimer();
        this.flushBatch();
      } else {
        this.scheduleFlush();
      }

      if (this.queuedBytes >= capacity && offset < value.length) {
        await this.waitForCapacity(capacity);
      }
    }
  }

  private async replaceOrdered(itemKey: string, value: string): Promise<void> {
    this.clearFlushTimer();
    this.discardBufferedItems(new Set([itemKey]));
    this.flushBatch();
    await this.writeTail;
    this.assertHealthy();
    this.ranges.delete(itemKey);
    if (this.ranges.size === 0) await this.clearDataFiles();
    else await this.compactFile();
    if (value.length > 0) await this.appendOrdered(itemKey, value);
  }

  private flushBatch(): void {
    if (this.chunks.length === 0 || this.batchBytes === 0) return;
    const chunks = this.chunks;
    const chargedBytes = this.batchBytes;
    this.chunks = [];
    this.batchBytes = 0;

    const batchRanges: BatchRange[] = [];
    const values: string[] = [];
    let batchOffset = 0;
    for (const chunk of chunks) {
      const length = chunk.value.length * 2;
      const previous = batchRanges.at(-1);
      if (previous?.itemKey === chunk.itemKey && previous.start + previous.length === batchOffset) {
        previous.length += length;
      } else {
        batchRanges.push({itemKey: chunk.itemKey, start: batchOffset, length});
      }
      values.push(chunk.value);
      batchOffset += length;
    }
    const bytes = Buffer.from(values.join(""), "utf16le");

    const write = this.writeTail.then(async () => {
      this.assertHealthy();
      await this.ensureFile();
      const fileStart = this.fileBytes;
      await this.writeAt(this.file!, bytes, fileStart);
      this.fileBytes += bytes.byteLength;
      for (const range of batchRanges) {
        this.addRange(range.itemKey, {
          start: fileStart + range.start,
          length: range.length,
        });
      }
      const redundantRanges = this.rangeCount() - this.ranges.size;
      if (redundantRanges >= this.options.maxIndexSegments) await this.compactFile();
    });

    this.writeTail = write.then(
      () => {
        this.queuedBytes = Math.max(0, this.queuedBytes - chargedBytes);
        this.releaseCapacityWaiters();
      },
      (error: unknown) => {
        this.queuedBytes = Math.max(0, this.queuedBytes - chargedBytes);
        this.fail(error);
        this.releaseCapacityWaiters(error);
      },
    );
    void this.writeTail.catch((error: unknown) => this.reportError(error));
  }

  private async ensureFile(): Promise<void> {
    if (this.file !== undefined) return;
    await mkdir(dirname(this.options.path), {recursive: true});
    if (!this.leaseAcquired) {
      await acquireResponsePartialLease(this.options.path);
      this.leaseAcquired = true;
    }
    this.file = await open(this.options.path, "w+", 0o600);
    this.fileBytes = 0;
  }

  private async readItemsOrdered(): Promise<ReadonlyMap<string, string>> {
    if (this.file === undefined || this.ranges.size === 0) return new Map();
    const result = new Map<string, string>();
    for (const [key, ranges] of this.ranges) {
      const totalBytes = ranges.reduce((total, range) => total + range.length, 0);
      const content = Buffer.allocUnsafe(totalBytes);
      let targetOffset = 0;
      for (const range of ranges) {
        let sourceOffset = range.start;
        const rangeEnd = range.start + range.length;
        while (sourceOffset < rangeEnd) {
          const length = Math.min(rangeEnd - sourceOffset, content.byteLength - targetOffset);
          const read = await this.file.read(content, targetOffset, length, sourceOffset);
          if (read.bytesRead <= 0) throw new Error("incomplete response partial read");
          sourceOffset += read.bytesRead;
          targetOffset += read.bytesRead;
        }
      }
      result.set(key, content.toString("utf16le"));
    }
    return result;
  }

  private addRange(itemKey: string, range: FileRange): void {
    const itemRanges = this.ranges.get(itemKey) ?? [];
    const previous = itemRanges.at(-1);
    if (previous !== undefined && previous.start + previous.length === range.start) {
      previous.length += range.length;
    } else {
      itemRanges.push(range);
    }
    this.ranges.set(itemKey, itemRanges);
  }

  private rangeCount(): number {
    let count = 0;
    for (const ranges of this.ranges.values()) count += ranges.length;
    return count;
  }

  private discardBufferedItems(itemKeys: ReadonlySet<string>): void {
    if (this.chunks.length === 0 || itemKeys.size === 0) return;
    const retained: BufferedChunk[] = [];
    let removedBytes = 0;
    for (const chunk of this.chunks) {
      if (itemKeys.has(chunk.itemKey)) removedBytes += chunk.bytes;
      else retained.push(chunk);
    }
    if (removedBytes === 0) return;
    this.chunks = retained;
    this.batchBytes = Math.max(0, this.batchBytes - removedBytes);
    this.queuedBytes = Math.max(0, this.queuedBytes - removedBytes);
    this.releaseCapacityWaiters();
  }

  private async compactFile(): Promise<void> {
    if (this.file === undefined || this.ranges.size === 0) return;
    const tempPath = join(dirname(this.options.path), RESPONSE_PARTIAL_COMPACT_FILE);
    await rm(tempPath, {force: true});
    const source = this.file;
    const target = await open(tempPath, "w+", 0o600);
    const nextRanges = new Map<string, FileRange[]>();
    const copyBytes = Math.max(2, Math.floor(Math.min(
      this.options.batchBytes,
      Math.max(2, this.options.maxBufferedBytes),
    ) / 2) * 2);
    const buffer = Buffer.allocUnsafe(copyBytes);
    let targetOffset = 0;
    let sourceClosed = false;
    let renamed = false;

    try {
      for (const [key, ranges] of this.ranges) {
        const itemStart = targetOffset;
        for (const range of ranges) {
          let copied = 0;
          while (copied < range.length) {
            const requestBytes = Math.min(buffer.byteLength, range.length - copied);
            const read = await source.read(buffer, 0, requestBytes, range.start + copied);
            if (read.bytesRead <= 0) throw new Error("incomplete response partial compaction read");
            await this.writeAt(target, buffer.subarray(0, read.bytesRead), targetOffset);
            copied += read.bytesRead;
            targetOffset += read.bytesRead;
          }
        }
        if (targetOffset > itemStart) nextRanges.set(key, [{start: itemStart, length: targetOffset - itemStart}]);
      }

      await target.close();
      await source.close();
      sourceClosed = true;
      this.file = undefined;
      try {
        await rename(tempPath, this.options.path);
      } catch (error) {
        if ((error as NodeJS.ErrnoException).code !== "EEXIST" &&
            (error as NodeJS.ErrnoException).code !== "EPERM") throw error;
        await rm(this.options.path, {force: true});
        await rename(tempPath, this.options.path);
      }
      renamed = true;
      this.file = await open(this.options.path, "r+");
      this.fileBytes = targetOffset;
      this.ranges.clear();
      for (const [key, ranges] of nextRanges) this.ranges.set(key, ranges);
    } catch (error) {
      await target.close().catch(() => undefined);
      await rm(tempPath, {force: true}).catch(() => undefined);
      if (sourceClosed && !renamed) {
        this.file = await open(this.options.path, "r+").catch(() => undefined);
      }
      throw error;
    }
  }

  private async clearDataFiles(): Promise<void> {
    try {
      await this.file?.close();
    } catch (error) {
      this.reportError(error);
    }
    this.file = undefined;
    this.fileBytes = 0;
    this.ranges.clear();
    await rm(this.options.path, {force: true});
    await rm(`${this.options.path}.compact`, {force: true});
    if (this.leaseAcquired) {
      await releaseResponsePartialLease(this.options.path);
      this.leaseAcquired = false;
    }
  }

  private async writeAt(file: FileHandle, bytes: Buffer, position: number): Promise<void> {
    let offset = 0;
    while (offset < bytes.byteLength) {
      const result = await file.write(bytes, offset, bytes.byteLength - offset, position + offset);
      if (result.bytesWritten <= 0) throw new Error("incomplete response partial append");
      offset += result.bytesWritten;
    }
  }

  private scheduleFlush(): void {
    if (this.flushTimer !== undefined) return;
    const flush = () => {
      this.flushTimer = undefined;
      this.flushBatch();
    };
    if (this.options.flushIntervalMs <= 0) {
      queueMicrotask(flush);
      return;
    }
    this.flushTimer = setTimeout(flush, this.options.flushIntervalMs);
    this.flushTimer.unref?.();
  }

  private waitForCapacity(capacity: number): Promise<void> {
    this.assertHealthy();
    if (this.queuedBytes < capacity) return Promise.resolve();
    return new Promise<void>((resolve, reject) => {
      this.capacityWaiters.add({resolve, reject});
    });
  }

  private releaseCapacityWaiters(error?: unknown): void {
    for (const waiter of this.capacityWaiters) {
      if (error !== undefined) waiter.reject(error);
      else if (this.queuedBytes < Math.max(1, this.options.maxBufferedBytes)) waiter.resolve();
      else continue;
      this.capacityWaiters.delete(waiter);
    }
  }

  private clearFlushTimer(): void {
    if (this.flushTimer !== undefined) {
      clearTimeout(this.flushTimer);
      this.flushTimer = undefined;
    }
  }

  private assertHealthy(): void {
    if (this.failure !== undefined) throw this.failure;
  }

  private fail(error: unknown): void {
    if (this.failure !== undefined) return;
    this.failure = error;
    this.reportError(error);
  }

  private reportError(error: unknown): void {
    if (this.failureReported) return;
    this.failureReported = true;
    try {
      this.options.onError?.(error);
    } catch (handlerError) {
      console.warn(`[web-ui-run-cache] error handler failed: ${handlerError instanceof Error ? handlerError.message : String(handlerError)}`);
    }
  }
}

function utf8Slice(value: string, start: number, maxBytes: number): Utf8Slice {
  let codeUnits = 0;
  let bytes = 0;
  while (start + codeUnits < value.length) {
    const codePoint = value.codePointAt(start + codeUnits)!;
    const width = codePoint > 0xffff ? 2 : 1;
    const encodedBytes = codePoint <= 0x7f ? 1 : codePoint <= 0x7ff ? 2 : codePoint <= 0xffff ? 3 : 4;
    if (codeUnits > 0 && bytes + encodedBytes > maxBytes) break;
    bytes += encodedBytes;
    codeUnits += width;
    if (bytes >= maxBytes) break;
  }
  return {value: value.slice(start, start + codeUnits), codeUnits, bytes};
}
