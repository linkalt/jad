import {randomUUID} from "node:crypto";
import {environmentWithoutApiToken} from "../../server-secrets.js";
import {spawn, type ChildProcess} from "node:child_process";
import {mkdir, open, readdir, readFile, rename, rm, stat, writeFile} from "node:fs/promises";
import {join} from "node:path";

import type {NativeProcessToolSettings} from "../../config.js";
import {OutputCollector, SpillWriter, type RenderedOutput, type StreamLimits} from "./child.js";

export type ProcessState = "running" | "exited" | "stopped" | "failed" | "lost";

interface ProcessManifest {
  schema_version: 1;
  id: string;
  state: ProcessState;
  cwd: string;
  started_at: string;
  ended_at?: string;
  pid?: number;
  exit_code?: number | null;
  signal?: string | null;
  stop_reason?: string;
  left_running?: boolean;
  stdout_produced_bytes?: number;
  stderr_produced_bytes?: number;
  stdout_stored_bytes?: number;
  stderr_stored_bytes?: number;
  stdout_capped?: boolean;
  stderr_capped?: boolean;
}

interface ManagedProcess {
  manifest: ProcessManifest;
  child?: ChildProcess;
  collector: OutputCollector;
  stdout: SpillWriter;
  stderr: SpillWriter;
  done: Promise<void>;
  resolveDone: () => void;
  finalizing?: Promise<void>;
  killGraceMs: number;
  maxCompleted: number;
  retentionDays: number;
}

export interface ProcessSnapshot extends ProcessManifest {
  output: RenderedOutput;
  stdout_stored_bytes: number;
  stderr_stored_bytes: number;
  stdout_capped: boolean;
  stderr_capped: boolean;
}

function terminal(state: ProcessState): boolean {
  return state !== "running";
}

function isManifest(value: unknown): value is ProcessManifest {
  if (typeof value !== "object" || value === null || Array.isArray(value)) return false;
  const record = value as Record<string, unknown>;
  return record.schema_version === 1 && typeof record.id === "string" &&
    typeof record.cwd === "string" && typeof record.started_at === "string" &&
    typeof record.state === "string";
}

function now(): string {
  return new Date().toISOString();
}

function signalGroup(pid: number, signal: NodeJS.Signals): boolean {
  try {
    process.kill(-pid, signal);
    return true;
  } catch {
    return false;
  }
}

/** Owns live child handles for one session. It is intentionally not restart-durable. */
export class ProcessManager {
  private readonly entries = new Map<string, ManagedProcess>();
  private readonly manifests = new Map<string, ProcessManifest>();
  private persistQueue: Promise<void> = Promise.resolve();
  private closed = false;
  /** Starts that passed the capacity check but have not registered an entry yet. */
  private starting = 0;

  public constructor(private readonly directory: string) {}

  public async recover(): Promise<void> {
    await mkdir(this.directory, {recursive: true});
    let entries: string[];
    try {
      entries = await readdir(this.directory);
    } catch {
      return;
    }
    for (const id of entries) {
      const manifest = await this.readManifest(id);
      if (!manifest) continue;
      if (manifest.state === "running") {
        manifest.state = "lost";
        manifest.ended_at = now();
        manifest.stop_reason = "backend_restarted";
        await this.persistManifest(manifest);
      }
      this.manifests.set(manifest.id, manifest);
    }
  }

  public async start(input: {
    cwd: string;
    displayCwd: string;
    command: string;
    policy: NativeProcessToolSettings;
    environment: NodeJS.ProcessEnv;
    apiKeyEnv: string;
  }): Promise<ProcessSnapshot> {
    if (this.closed) throw new Error("process manager is closed");
    if (process.platform === "win32") throw new Error("managed process groups require a POSIX platform");
    const running = [...this.entries.values()].filter((entry) => entry.manifest.state === "running").length;
    if (running + this.starting >= input.policy.max_running) throw new Error("process capacity reached");
    // Concurrent tool calls start here; hold the slot until the entry is
    // registered, otherwise every caller passes the check above together.
    this.starting += 1;
    let reserved = true;
    const releaseSlot = (): void => {
      if (reserved) {
        reserved = false;
        this.starting -= 1;
      }
    };
    try {
      return await this.startReserved(input, releaseSlot);
    } finally {
      releaseSlot();
    }
  }

  private async startReserved(input: {
    cwd: string;
    displayCwd: string;
    command: string;
    policy: NativeProcessToolSettings;
    environment: NodeJS.ProcessEnv;
    apiKeyEnv: string;
  }, releaseSlot: () => void): Promise<ProcessSnapshot> {
    const id = `proc_${randomUUID()}`;
    const manifest: ProcessManifest = {
      schema_version: 1,
      id,
      state: "running",
      cwd: input.displayCwd,
      started_at: now(),
    };
    const processDir = join(this.directory, id);
    await mkdir(processDir, {recursive: true});
    await this.persistManifest(manifest);

    const collector = new OutputCollector({
      stdout: {headBytes: input.policy.stdout.head_bytes, tailBytes: input.policy.stdout.tail_bytes},
      stderr: {headBytes: input.policy.stderr.head_bytes, tailBytes: input.policy.stderr.tail_bytes},
      maxLineChars: input.policy.max_line_chars,
      stripAnsi: input.policy.strip_ansi,
    });
    const stdout = new SpillWriter({path: join(processDir, "stdout.log"), capBytes: input.policy.log_cap_bytes});
    const stderr = new SpillWriter({path: join(processDir, "stderr.log"), capBytes: input.policy.log_cap_bytes});
    let resolveDone!: () => void;
    const done = new Promise<void>((resolve) => { resolveDone = resolve; });
    const entry: ManagedProcess = {
      manifest,
      collector,
      stdout,
      stderr,
      done,
      resolveDone,
      killGraceMs: input.policy.kill_grace_ms,
      maxCompleted: input.policy.max_completed,
      retentionDays: input.policy.retention_days,
    }; 
    this.entries.set(id, entry);
    this.manifests.set(id, manifest);
    // The entry is now counted as running, so the reservation is redundant.
    releaseSlot();

    const environment = environmentWithoutApiToken({...input.environment, ...input.policy.env});
    delete environment[input.apiKeyEnv];
    for (const name of input.policy.env_deny) delete environment[name];
    let child: ChildProcess;
    try {
      child = spawn(input.policy.shell, input.policy.login_shell ? ["-l", "-c", input.command] : ["-c", input.command], {
        cwd: input.cwd,
        env: environment,
        detached: true,
        stdio: ["ignore", "pipe", "pipe"],
      });
    } catch (error) {
      await this.finish(entry, "failed", {stop_reason: error instanceof Error ? error.message : String(error)});
      return this.snapshotEntry(entry);
    }
    entry.child = child;
    if (typeof child.pid === "number") manifest.pid = child.pid;
    this.attachStream(child.stdout, "stdout", entry);
    this.attachStream(child.stderr, "stderr", entry);
    child.once("error", (error) => { void this.finish(entry, "failed", {stop_reason: error.message}); });
    child.once("close", (exitCode, exitSignal) => {
      void this.finish(entry, manifest.stop_reason ? "stopped" : "exited", {
        exit_code: exitCode,
        signal: exitSignal,
      });
    });
    await this.persistManifest(manifest);
    return this.snapshotEntry(entry);
  }

  public snapshot(id: string): ProcessSnapshot | undefined {
    const entry = this.entries.get(id);
    if (entry) return this.snapshotEntry(entry);
    const manifest = this.manifests.get(id);
    if (!manifest) return undefined;
    return {
      ...manifest,
      output: {text: "", stdout: {text: "", producedBytes: 0, shownBytes: 0, omittedBytes: 0, truncated: false}, stderr: {text: "", producedBytes: 0, shownBytes: 0, omittedBytes: 0, truncated: false}, truncated: false, omittedBytes: 0, binary: false, returnedBytes: 0},
      stdout_stored_bytes: manifest.stdout_stored_bytes ?? 0,
      stderr_stored_bytes: manifest.stderr_stored_bytes ?? 0,
      stdout_capped: manifest.stdout_capped ?? false,
      stderr_capped: manifest.stderr_capped ?? false,
    };
  }

  public async snapshotWithLogs(
    id: string,
    stdoutLimits: StreamLimits,
    stderrLimits: StreamLimits,
    maxLineChars: number,
    stripAnsi: boolean,
  ): Promise<ProcessSnapshot | undefined> {
    const live = this.entries.get(id);
    if (live) return this.snapshotEntry(live);
    const snapshot = this.snapshot(id);
    if (!snapshot) return undefined;
    const [stdout, stderr] = await Promise.all([
      this.readWindows(id, "stdout.log", stdoutLimits),
      this.readWindows(id, "stderr.log", stderrLimits),
    ]);
    const collector = new OutputCollector({stdout: stdoutLimits, stderr: stderrLimits, maxLineChars, stripAnsi});
    for (const chunk of stdout.chunks) collector.push("stdout", chunk);
    for (const chunk of stderr.chunks) collector.push("stderr", chunk);
    collector.end();
    return {
      ...snapshot,
      output: collector.render({
        stdout: snapshot.stdout_produced_bytes ?? stdout.storedBytes,
        stderr: snapshot.stderr_produced_bytes ?? stderr.storedBytes,
      }),
    };
  }

  public list(filter?: ProcessState): ProcessSnapshot[] {
    return [...this.manifests.keys()]
      .map((id) => this.snapshot(id))
      .filter((snapshot): snapshot is ProcessSnapshot => snapshot !== undefined && (filter === undefined || snapshot.state === filter))
      .sort((left, right) => right.started_at.localeCompare(left.started_at));
  }

  public async wait(id: string, timeoutMs: number): Promise<{snapshot?: ProcessSnapshot; waited: boolean}> {
    const entry = this.entries.get(id);
    if (!entry) return {snapshot: this.snapshot(id), waited: true};
    let timer: NodeJS.Timeout | undefined;
    const timedOut = new Promise<boolean>((resolve) => { timer = setTimeout(() => resolve(true), timeoutMs); });
    // A terminal manifest may be visible before persistence and retention pruning finish.
    const finished = entry.done.then(() => false);
    const timeout = await Promise.race([timedOut, finished]);
    if (timer) clearTimeout(timer);
    return {snapshot: this.snapshot(id), waited: !timeout};
  }

  public async stop(id: string, reason = "stopped_by_agent"): Promise<ProcessSnapshot | undefined> {
    const entry = this.entries.get(id);
    if (!entry) return this.snapshot(id);
    if (terminal(entry.manifest.state)) return this.snapshotEntry(entry);
    entry.manifest.stop_reason = reason;
    const pid = entry.manifest.pid;
    if (pid === undefined || !signalGroup(pid, "SIGTERM")) {
      entry.manifest.left_running = true;
      await this.persistManifest(entry.manifest);
      return this.snapshotEntry(entry);
    }
    const exited = await Promise.race([
      entry.done.then(() => true),
      new Promise<boolean>((resolve) => setTimeout(() => resolve(false), entry.killGraceMs)),
    ]);
    if (!exited) {
      const killed = signalGroup(pid, "SIGKILL");
      const reaped = killed && await Promise.race([
        entry.done.then(() => true),
        new Promise<boolean>((resolve) => setTimeout(() => resolve(false), entry.killGraceMs)),
      ]);
      if (!reaped) {
        entry.manifest.left_running = true;
        await this.persistManifest(entry.manifest);
      }
    }
    return this.snapshot(id);
  }

  public async close(): Promise<void> {
    if (this.closed) return;
    this.closed = true;
    await Promise.all([...this.entries.keys()].map((id) => this.stop(id, "session_closed")));
  }

  private attachStream(stream: NodeJS.ReadableStream | null, name: "stdout" | "stderr", entry: ManagedProcess): void {
    if (!stream) return;
    const spill = name === "stdout" ? entry.stdout : entry.stderr;
    stream.on("data", (chunk: Buffer) => {
      entry.collector.push(name, chunk);
      const pending = spill.write(chunk);
      if (pending) {
        stream.pause();
        void pending.finally(() => stream.resume());
      }
    });
  }

  private async finish(entry: ManagedProcess, state: ProcessState, extra: Partial<ProcessManifest>): Promise<void> {
    if (entry.finalizing) return entry.finalizing;
    entry.finalizing = (async () => {
      entry.collector.end();
      await Promise.all([entry.stdout.close(), entry.stderr.close()]);
      const output = entry.collector.render();
      entry.manifest = {
        ...entry.manifest,
        ...extra,
        state,
        ended_at: now(),
        stdout_produced_bytes: output.stdout.producedBytes,
        stderr_produced_bytes: output.stderr.producedBytes,
        stdout_stored_bytes: entry.stdout.writtenBytes,
        stderr_stored_bytes: entry.stderr.writtenBytes,
        stdout_capped: entry.stdout.capped,
        stderr_capped: entry.stderr.capped,
      };
      this.manifests.set(entry.manifest.id, entry.manifest);
      await this.persistManifest(entry.manifest);
      await this.prune(entry.maxCompleted, entry.retentionDays);
      entry.resolveDone();
    })();
    return entry.finalizing;
  }

  private snapshotEntry(entry: ManagedProcess): ProcessSnapshot {
    const output = entry.collector.render();
    return {
      ...entry.manifest,
      output,
      stdout_stored_bytes: entry.stdout.writtenBytes,
      stderr_stored_bytes: entry.stderr.writtenBytes,
      stdout_capped: entry.stdout.capped,
      stderr_capped: entry.stderr.capped,
    };
  }

  private async prune(maxCompleted: number, retentionDays: number): Promise<void> {
    const cutoff = retentionDays === 0 ? undefined : Date.now() - retentionDays * 24 * 60 * 60 * 1000;
    const terminalEntries = [...this.manifests.values()]
      .filter((manifest) => terminal(manifest.state))
      .sort((left, right) => (right.ended_at ?? right.started_at).localeCompare(left.ended_at ?? left.started_at));
    const removable = terminalEntries.filter((manifest, index) => {
      const ended = Date.parse(manifest.ended_at ?? manifest.started_at);
      return index >= maxCompleted || (cutoff !== undefined && Number.isFinite(ended) && ended < cutoff);
    });
    await Promise.all(removable.map(async (manifest) => {
      this.entries.delete(manifest.id);
      this.manifests.delete(manifest.id);
      await rm(join(this.directory, manifest.id), {recursive: true, force: true});
    }));
  }

  private async readWindows(
    id: string,
    name: "stdout.log" | "stderr.log",
    limits: StreamLimits,
  ): Promise<{chunks: Buffer[]; storedBytes: number}> {
    try {
      const path = join(this.directory, id, name);
      const storedBytes = (await stat(path)).size;
      if (storedBytes === 0) return {chunks: [], storedBytes};
      const completeWindow = limits.headBytes + limits.tailBytes;
      const handle = await open(path, "r");
      try {
        if (storedBytes <= completeWindow) {
          const whole = Buffer.alloc(storedBytes);
          await handle.read(whole, 0, storedBytes, 0);
          return {chunks: [whole], storedBytes};
        }
        const headLength = Math.min(storedBytes, limits.headBytes);
        const tailLength = Math.min(storedBytes - headLength, limits.tailBytes);
        const head = Buffer.alloc(headLength);
        const tail = Buffer.alloc(tailLength);
        await Promise.all([
          headLength > 0 ? handle.read(head, 0, headLength, 0) : undefined,
          tailLength > 0 ? handle.read(tail, 0, tailLength, storedBytes - tailLength) : undefined,
        ]);
        return {chunks: [head, tail].filter((chunk) => chunk.length > 0), storedBytes};
      } finally {
        await handle.close();
      }
    } catch {
      return {chunks: [], storedBytes: 0};
    }
  }

  private async readManifest(id: string): Promise<ProcessManifest | undefined> {
    try {
      const raw = JSON.parse(await readFile(join(this.directory, id, "manifest.json"), "utf8")) as unknown;
      return isManifest(raw) ? raw : undefined;
    } catch {
      return undefined;
    }
  }

  private async persistManifest(manifest: ProcessManifest): Promise<void> {
    this.persistQueue = this.persistQueue.then(async () => {
      const directory = join(this.directory, manifest.id);
      await mkdir(directory, {recursive: true});
      const target = join(directory, "manifest.json");
      const temporary = `${target}.${randomUUID()}.tmp`;
      await writeFile(temporary, `${JSON.stringify(manifest)}\n`, "utf8");
      await rename(temporary, target);
    });
    return this.persistQueue;
  }
}
