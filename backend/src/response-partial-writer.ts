import {mkdir, open, readFile, rm, type FileHandle} from "node:fs/promises";
import {dirname} from "node:path";

export interface ResponsePartialWriterOptions {
  path: string;
  /** Target batch size, measured using conservative UTF-8 content bytes. */
  batchBytes: number;
  /** Maximum buffered plus in-flight UTF-8 content charge. */
  maxBufferedBytes: number;
  /** Maximum time to retain a partial batch before scheduling an append. */
  flushIntervalMs: number;
  onError?: (error: unknown) => void;
}

interface Utf8Slice {
  value: string;
  codeUnits: number;
  bytes: number;
}

/** One asynchronous append-only file for the active response output item. */
export class ResponsePartialWriter {
  private file: FileHandle | undefined;
  private chunks: string[] = [];
  private batchBytes = 0;
  private queuedBytes = 0;
  private writeTail: Promise<void> = Promise.resolve();
  private admissionTail: Promise<void> = Promise.resolve();
  private flushTimer: NodeJS.Timeout | undefined;
  private failure: unknown;
  private disposed = false;
  private failureReported = false;
  private readonly capacityWaiters = new Set<{
    resolve: () => void;
    reject: (error: unknown) => void;
  }>();

  public constructor(private readonly options: ResponsePartialWriterOptions) {}

  /** Accepts content in order and waits only when the bounded buffer is full. */
  public append(value: string): Promise<void> {
    return this.enqueueAdmission(() => this.appendOrdered(value));
  }

  /** Replaces the active item's content without exposing stale file bytes. */
  public replace(value: string): Promise<void> {
    return this.enqueueAdmission(async () => {
      this.clearFlushTimer();
      this.flushBatch();
      await this.writeTail;
      this.assertHealthy();
      await this.file?.close();
      this.file = undefined;
      await rm(this.options.path, {force: true});
      await this.appendOrdered(value);
    });
  }

  /** Reads a linearized prefix; later appends queue behind this snapshot barrier. */
  public read(): Promise<string> {
    return this.enqueueAdmission(async () => {
      this.clearFlushTimer();
      this.flushBatch();
      await this.writeTail;
      this.assertHealthy();
      try {
        return await readFile(this.options.path, "utf16le");
      } catch (error) {
        if ((error as NodeJS.ErrnoException).code === "ENOENT") return "";
        throw error;
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

  /** Closes the writer and removes its one ephemeral content file. */
  public async dispose(): Promise<void> {
    if (this.disposed) return;
    this.disposed = true;
    this.clearFlushTimer();
    try {
      await this.flushPendingForDisposal();
    } catch {
      // Cache cleanup is best-effort; the failure was already reported.
    }
    try {
      await this.file?.close();
    } catch (error) {
      this.reportError(error);
    }
    this.file = undefined;
    try {
      await rm(this.options.path, {force: true});
    } catch (error) {
      this.reportError(error);
    }
    this.releaseCapacityWaiters();
  }

  private enqueueAdmission<T>(operation: () => Promise<T>): Promise<T> {
    const accepted = this.admissionTail.then(() => {
      this.assertHealthy();
      if (this.disposed) throw new Error("response partial writer is disposed");
      return operation();
    });
    // Keep subsequent admissions ordered even when one fails; callers observe
    // the failure through `accepted` and the writer's shared failure state.
    this.admissionTail = accepted.then(() => undefined, () => undefined);
    return accepted;
  }

  private async appendOrdered(value: string): Promise<void> {
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
      this.chunks.push(slice.value);
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

  private flushBatch(): void {
    if (this.chunks.length === 0 || this.batchBytes === 0) return;
    const chunks = this.chunks;
    const chargedBytes = this.batchBytes;
    this.chunks = [];
    this.batchBytes = 0;
    const bytes = Buffer.from(chunks.join(""), "utf16le");

    const write = this.writeTail.then(async () => {
      this.assertHealthy();
      await mkdir(dirname(this.options.path), {recursive: true});
      this.file ??= await open(this.options.path, "a");
      let offset = 0;
      while (offset < bytes.byteLength) {
        const result = await this.file.write(bytes, offset, bytes.byteLength - offset, null);
        if (result.bytesWritten <= 0) throw new Error("incomplete response partial append");
        offset += result.bytesWritten;
      }
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
    // Every timer and write is observed even when the caller only waits for
    // capacity, not for durable cache bytes.
    void this.writeTail.catch((error: unknown) => this.reportError(error));
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

  private async flushPendingForDisposal(): Promise<void> {
    this.clearFlushTimer();
    await this.admissionTail.catch(() => undefined);
    this.flushBatch();
    await this.writeTail.catch(() => undefined);
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
