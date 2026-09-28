export interface ProcessMemorySnapshot {
  rss_bytes: number;
  heap_total_bytes: number;
  heap_used_bytes: number;
  external_bytes: number;
  array_buffers_bytes: number;
}

export interface RunMemoryTraceHandle {
  readonly enabled: boolean;
  /** Records a phase and metadata only; never pass prompt or response content. */
  mark(phase: string, details?: Readonly<Record<string, number | string | boolean>>): void;
  stop(): void;
}

export interface RunMemoryTraceOptions {
  enabled: boolean;
  sampleIntervalMs: number;
  readMemory?: () => NodeJS.MemoryUsage;
  writeLine?: (line: string) => void;
  now?: () => number;
}

type TraceRecord = {
  component: "provider-memory";
  event: "start" | "sample" | "phase" | "end";
  timestamp: string;
  pid: number;
  active_runs: number;
  run_id?: number;
  run_ids?: number[];
  phase?: string;
  details?: Readonly<Record<string, number | string | boolean>>;
  elapsed_ms?: number;
  memory?: ProcessMemorySnapshot;
  start_memory?: ProcessMemorySnapshot;
  peak_memory?: ProcessMemorySnapshot;
};

interface ActiveRun {
  startedAt: number;
  startMemory: ProcessMemorySnapshot;
  peakMemory: ProcessMemorySnapshot;
}

const TRACE_PREFIX = "[provider-memory] ";
const DEFAULT_SAMPLE_INTERVAL_MS = 250;

function positiveInteger(value: string | undefined, fallback: number): number {
  if (value === undefined || value.trim() === "") return fallback;
  const parsed = Number(value);
  return Number.isSafeInteger(parsed) && parsed > 0 ? parsed : fallback;
}

function snapshot(memory: NodeJS.MemoryUsage): ProcessMemorySnapshot {
  return {
    rss_bytes: memory.rss,
    heap_total_bytes: memory.heapTotal,
    heap_used_bytes: memory.heapUsed,
    external_bytes: memory.external,
    array_buffers_bytes: memory.arrayBuffers ?? 0,
  };
}

function maxSnapshot(
  left: ProcessMemorySnapshot,
  right: ProcessMemorySnapshot,
): ProcessMemorySnapshot {
  return {
    rss_bytes: Math.max(left.rss_bytes, right.rss_bytes),
    heap_total_bytes: Math.max(left.heap_total_bytes, right.heap_total_bytes),
    heap_used_bytes: Math.max(left.heap_used_bytes, right.heap_used_bytes),
    external_bytes: Math.max(left.external_bytes, right.external_bytes),
    array_buffers_bytes: Math.max(left.array_buffers_bytes, right.array_buffers_bytes),
  };
}

/** Process-wide, opt-in sampler for overlapping provider runs. */
export class RunMemoryTrace {
  private readonly activeRuns = new Map<number, ActiveRun>();
  private nextRunId = 1;
  private timer: NodeJS.Timeout | undefined;
  private readonly readMemory: () => NodeJS.MemoryUsage;
  private readonly writeLine: (line: string) => void;
  private readonly now: () => number;

  public constructor(private readonly options: RunMemoryTraceOptions) {
    this.readMemory = options.readMemory ?? (() => process.memoryUsage());
    this.writeLine = options.writeLine ?? ((line) => console.info(line));
    this.now = options.now ?? (() => Date.now());
  }

  public beginRun(): RunMemoryTraceHandle {
    if (!this.options.enabled) {
      return {enabled: false, mark: () => undefined, stop: () => undefined};
    }

    const runId = this.nextRunId++;
    const startMemory = snapshot(this.readMemory());
    const activeRun: ActiveRun = {
      startedAt: this.now(),
      startMemory,
      peakMemory: startMemory,
    };
    this.activeRuns.set(runId, activeRun);
    this.write({
      event: "start",
      run_id: runId,
      active_runs: this.activeRuns.size,
      memory: startMemory,
    });
    this.ensureSampling();

    return {
      enabled: true,
      mark: (phase, details) => this.markRun(runId, phase, details),
      stop: () => this.stopRun(runId),
    };
  }

  /** Exposed for deterministic tests; the interval invokes the same sampler. */
  public sampleNow(): void {
    if (!this.options.enabled || this.activeRuns.size === 0) return;
    const memory = snapshot(this.readMemory());
    this.updatePeaks(memory);
    this.write({
      event: "sample",
      run_ids: [...this.activeRuns.keys()],
      active_runs: this.activeRuns.size,
      memory,
    });
  }

  private markRun(
    runId: number,
    phase: string,
    details?: Readonly<Record<string, number | string | boolean>>,
  ): void {
    if (!this.activeRuns.has(runId)) return;
    const memory = snapshot(this.readMemory());
    this.updatePeaks(memory);
    this.write({
      event: "phase",
      run_id: runId,
      active_runs: this.activeRuns.size,
      phase,
      ...(details === undefined ? {} : {details}),
      memory,
    });
  }

  private stopRun(runId: number): void {
    const activeRun = this.activeRuns.get(runId);
    if (activeRun === undefined) return;

    const memory = snapshot(this.readMemory());
    this.updatePeaks(memory);
    this.activeRuns.delete(runId);
    this.write({
      event: "end",
      run_id: runId,
      active_runs: this.activeRuns.size,
      elapsed_ms: Math.max(0, this.now() - activeRun.startedAt),
      memory,
      start_memory: activeRun.startMemory,
      peak_memory: activeRun.peakMemory,
    });
    if (this.activeRuns.size === 0) this.stopSampling();
  }

  private updatePeaks(memory: ProcessMemorySnapshot): void {
    for (const activeRun of this.activeRuns.values()) {
      activeRun.peakMemory = maxSnapshot(activeRun.peakMemory, memory);
    }
  }

  private ensureSampling(): void {
    if (this.timer !== undefined) return;
    this.timer = setInterval(() => this.sampleNow(), this.options.sampleIntervalMs);
    this.timer.unref?.();
  }

  private stopSampling(): void {
    if (this.timer === undefined) return;
    clearInterval(this.timer);
    this.timer = undefined;
  }

  private write(record: Omit<TraceRecord, "component" | "timestamp" | "pid">): void {
    const line: TraceRecord = {
      component: "provider-memory",
      timestamp: new Date(this.now()).toISOString(),
      pid: process.pid,
      ...record,
    };
    try {
      this.writeLine(`${TRACE_PREFIX}${JSON.stringify(line)}`);
    } catch {
      // Diagnostic logging must not alter provider-run behavior.
    }
  }
}

const traceEnabled = process.env.JAD_PROVIDER_MEMORY_TRACE === "1";
const traceIntervalMs = positiveInteger(
  process.env.JAD_PROVIDER_MEMORY_TRACE_INTERVAL_MS,
  DEFAULT_SAMPLE_INTERVAL_MS,
);

export const providerRunMemoryTrace = new RunMemoryTrace({
  enabled: traceEnabled,
  sampleIntervalMs: traceIntervalMs,
});
