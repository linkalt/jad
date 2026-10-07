/**
 * @file child.ts
 * @input A shell command, working directory, environment, and output limits
 * @output Exports runChildProcess(), OutputCollector, SpillWriter
 * @position Shared child-process runner behind the native bash and glob tools
 *
 * A shell command owns a whole process tree, so the runner spawns it in its own
 * process group and signals the group rather than the shell alone: SIGTERM on
 * timeout, SIGKILL after the grace period for children that ignore it, and a
 * cleanup signal when the command finishes so backgrounded children cannot
 * outlive the call. After SIGKILL a fixed deadline ends the ladder regardless of
 * whether the process was ever reaped, because a call must not hang on a process
 * that cannot die.
 *
 * stdout and stderr are captured as two separate bounded streams: each keeps its
 * own head window (the start) and tail window (the end), so a chatty stream can
 * no longer evict the other one, and each stream is rendered with its own
 * omitted-bytes marker. The middle of a long stream is dropped from memory, so
 * the complete bytes are teed to a spill file while they are read when the
 * caller asks for one; the file is the only place the middle still exists.
 */

import {spawn} from "node:child_process";
import {createWriteStream, mkdirSync, type WriteStream} from "node:fs";
import {dirname} from "node:path";
import {StringDecoder} from "node:string_decoder";

export type ChildStream = "stdout" | "stderr";

/** Bytes kept from the start and the end of one stream. */
export interface StreamLimits {
  headBytes: number;
  tailBytes: number;
}

export interface OutputLimits {
  stdout: StreamLimits;
  stderr: StreamLimits;
  /** Longest rendered line before it is cut. */
  maxLineChars: number;
  /** Remove ANSI escape sequences and non-printing control characters. */
  stripAnsi: boolean;
  /** Preserve NUL delimiters for a consumer that parses a binary-safe protocol. */
  preserveNul?: boolean;
  /**
   * When a stream is cut, end the head and start the tail on line boundaries, so no
   * line is shown half. The bytes this drops are counted in the omitted marker.
   */
  lineAware?: boolean;
}

/** What one stream produced and what the model gets to see of it. */
export interface StreamOutput {
  /** Every byte the stream produced. */
  producedBytes: number;
  /** Bytes kept for the model: head window plus tail window. */
  shownBytes: number;
  /** Produced bytes the model does not see; the marker counts these. */
  omittedBytes: number;
  truncated: boolean;
  /** Rendered head, marker, and tail — without a section label. */
  text: string;
}

export interface RenderedOutput {
  /** What the model receives: one labeled section per stream that wrote. */
  text: string;
  stdout: StreamOutput;
  stderr: StreamOutput;
  /** Either stream lost bytes. */
  truncated: boolean;
  /** Bytes the model does not see, across both streams. */
  omittedBytes: number;
  binary: boolean;
  returnedBytes: number;
}

/** File that receives every byte of one stream, up to a byte ceiling. */
export interface SpillTarget {
  path: string;
  capBytes: number;
}

export interface SpillOutcome {
  path: string;
  writtenBytes: number;
  /** The ceiling was reached, so the file holds only the start of the stream. */
  capped: boolean;
  error?: string;
}

export interface ChildProcessRequest {
  file: string;
  args: string[];
  cwd: string;
  env: NodeJS.ProcessEnv;
  timeoutMs: number;
  killGraceMs: number;
  /** Signal leftover descendants when the command finishes normally. */
  killProcessGroupWhenDone: boolean;
  output: OutputLimits;
  /** Cancels this foreground child process and its process group. */
  signal?: AbortSignal;
  /** Text written to the child's standard input, which is then closed; absent leaves it closed from the start. */
  stdin?: string;
  /** Complete streams written to disk while they are read. */
  spill?: {
    stdout?: SpillTarget;
    stderr?: SpillTarget;
  };
}

export interface ChildProcessResult extends RenderedOutput {
  exitCode: number | null;
  signal: NodeJS.Signals | null;
  timedOut: boolean;
  /** The owning run was cancelled before this process completed. */
  aborted?: boolean;
  durationMs: number;
  /** Which signal ended the command because of the timeout, when applicable. */
  termination?: "sigterm" | "sigkill";
  /**
   * The deadline expired before the process was reaped: it survived SIGKILL
   * (uninterruptible I/O), so the call returned without it and the operating
   * system still owns the process.
   */
  leftRunning?: boolean;
  /** What each spill file received, when the caller asked for one. */
  spill?: {
    stdout?: SpillOutcome;
    stderr?: SpillOutcome;
  };
  spawnError?: {code?: string; message: string};
}

const ESCAPE = 0x1b;
const BELL = 0x07;
const NEWLINE = 0x0a;
const CARRIAGE_RETURN = 0x0d;
const TAB = 0x09;
const DELETE = 0x7f;
const HIGH_SURROGATE_START = 0xd800;
const HIGH_SURROGATE_END = 0xdbff;
const LOW_SURROGATE_START = 0xdc00;
const LOW_SURROGATE_END = 0xdfff;
/** Longest escape sequence scanned before its terminator is abandoned. */
const MAX_ESCAPE_SCAN = 256;
/**
 * How long output may keep arriving after the leader exited with a pipe still
 * open (a straggler holding stdout); keeps a leaked reader from hanging a call.
 */
const DRAIN_AFTER_EXIT_MS = 500;
/**
 * How long a call keeps waiting for a process to be reaped after SIGKILL.
 *
 * SIGKILL cannot be caught, but a process blocked in uninterruptible I/O does
 * not act on it until the kernel call finishes — sometimes never. Waiting for
 * that event would hang the call, so the deadline resolves it as a bounded
 * timeout result that reports the process as left running instead.
 */
const KILL_DEADLINE_MS = 2000;

const TRUNCATED_LINE_MARKER = " [... truncated line]";

function omittedMarker(bytes: number): string {
  return `[... ${bytes} bytes omitted ...]`;
}

/**
 * Length of the escape sequence starting at `start` (which points at ESC),
 * including the ESC itself.
 */
function escapeSequenceLength(value: string, start: number): number {
  const kind = value.charCodeAt(start + 1);
  if (Number.isNaN(kind)) {
    return 1;
  }
  if (kind === 0x5b || kind === 0x5d) {
    // CSI ("ESC [") ends at a byte in 0x40-0x7e; OSC ("ESC ]") ends at BEL or
    // ST ("ESC \").
    const limit = Math.min(value.length, start + 2 + MAX_ESCAPE_SCAN);
    for (let index = start + 2; index < limit; index += 1) {
      const code = value.charCodeAt(index);
      if (kind === 0x5b && code >= 0x40 && code <= 0x7e) {
        return index - start + 1;
      }
      if (kind === 0x5d) {
        if (code === BELL) {
          return index - start + 1;
        }
        if (code === ESCAPE && value.charCodeAt(index + 1) === 0x5c) {
          return index - start + 2;
        }
      }
    }
    return 1;
  }
  // Two-character introducers ("ESC (" , "ESC )" , "ESC #") take one more byte.
  return kind === 0x28 || kind === 0x29 || kind === 0x23 ? 3 : 2;
}

/**
 * Removes escape sequences and non-printing control characters: colour codes
 * and progress redraws have no value in the model context, and control bytes
 * must not reach the UI. Lone carriage returns become newlines so overwritten
 * progress output stays readable.
 */
export function stripControlCharacters(value: string): string {
  let result = "";
  let index = 0;
  while (index < value.length) {
    const code = value.charCodeAt(index);
    if (code === ESCAPE) {
      index += escapeSequenceLength(value, index);
      continue;
    }
    if (code === NEWLINE || code === TAB) {
      result += String.fromCharCode(code);
      index += 1;
      continue;
    }
    if (code === CARRIAGE_RETURN) {
      if (value.charCodeAt(index + 1) !== NEWLINE) {
        result += "\n";
      }
      index += 1;
      continue;
    }
    if (code < 0x20 || code === DELETE || (code >= 0x80 && code <= 0x9f)) {
      index += 1;
      continue;
    }
    if (code >= HIGH_SURROGATE_START && code <= HIGH_SURROGATE_END) {
      const next = value.charCodeAt(index + 1);
      const paired = next >= LOW_SURROGATE_START && next <= LOW_SURROGATE_END;
      result += paired ? value.slice(index, index + 2) : "\uFFFD";
      index += paired ? 2 : 1;
      continue;
    }
    if (code >= LOW_SURROGATE_START && code <= LOW_SURROGATE_END) {
      result += "\uFFFD";
      index += 1;
      continue;
    }
    result += String.fromCharCode(code);
    index += 1;
  }
  return result;
}

export function cutLongLines(value: string, maxLineChars: number): string {
  return value
    .split("\n")
    .map((line) =>
      line.length > maxLineChars
        ? `${line.slice(0, maxLineChars)}${TRUNCATED_LINE_MARKER}`
        : line,
    )
    .join("\n");
}

interface Segment {
  text: string;
  bytes: number;
}

function removeNul(value: string): string {
  return value.includes("\u0000") ? value.replace(/\u0000/g, "") : value;
}

/** Moves an index to the nearest UTF-8 character boundary at or after it. */
function ceilBoundary(buffer: Buffer, index: number): number {
  let at = index;
  while (at < buffer.length && ((buffer[at] ?? 0) & 0xc0) === 0x80) {
    at += 1;
  }
  return at;
}

/** Moves an index to the nearest UTF-8 character boundary at or before it. */
function floorBoundary(buffer: Buffer, index: number): number {
  let at = index;
  while (at > 0 && ((buffer[at] ?? 0) & 0xc0) === 0x80) {
    at -= 1;
  }
  return at;
}

/** Largest prefix of `text` within `maxBytes`, never splitting a character. */
function takePrefix(text: string, maxBytes: number): string {
  const buffer = Buffer.from(text, "utf8");
  if (buffer.length <= maxBytes) {
    return text;
  }
  return buffer.subarray(0, floorBoundary(buffer, maxBytes)).toString("utf8");
}

/** Largest suffix of `text` within `maxBytes`, never splitting a character. */
function takeTail(text: string, maxBytes: number): string {
  const buffer = Buffer.from(text, "utf8");
  if (buffer.length <= maxBytes) {
    return text;
  }
  return buffer.subarray(ceilBoundary(buffer, buffer.length - maxBytes)).toString("utf8");
}

/**
 * Cuts lines without allowing the explanatory marker to exceed the bytes it
 * replaces. This keeps a pathological sequence of very short long-lines from
 * multiplying a stream beyond its retained-window budget.
 */
function cutLongLinesWithinBudget(value: string, maxLineChars: number): string {
  return value
    .split("\n")
    .map((line) => {
      if (line.length <= maxLineChars) {
        return line;
      }
      const replacement = `${line.slice(0, maxLineChars)}${TRUNCATED_LINE_MARKER}`;
      return takePrefix(replacement, Buffer.byteLength(line, "utf8"));
    })
    .join("\n");
}

/**
 * One bounded stream. The head window fills first; everything after it fills the
 * tail window, which keeps the most recent bytes. A stream shorter than the two
 * windows is therefore retained exactly once, without duplication and without
 * gaps, and rendering a stream that fits produces no marker at all.
 */
class StreamCollector {
  private readonly decoder = new StringDecoder("utf8");
  private readonly head: Segment[] = [];
  private readonly tail: Segment[] = [];
  private headBytes = 0;
  private tailBytes = 0;
  private binarySeen = false;

  public producedBytes = 0;

  public constructor(
    private readonly limits: StreamLimits,
    private readonly stripAnsi: boolean,
    private readonly preserveNul: boolean,
    private readonly lineAware = false,
  ) {}

  /** Accepts raw bytes from one pipe; safe across split multibyte sequences. */
  public push(chunk: Buffer): void {
    this.producedBytes += chunk.length;
    if (chunk.includes(0)) {
      // A NUL byte cannot be rendered and marks non-text output.
      this.binarySeen = true;
    }
    this.accept(this.decoder.write(chunk));
  }

  /** Flushes a partial multibyte sequence at the end of the stream. */
  public end(): void {
    this.accept(this.decoder.end());
  }

  public get binary(): boolean {
    return this.binarySeen;
  }

  private accept(decoded: string): void {
    if (decoded.length === 0) {
      return;
    }
    if (decoded.includes("\uFFFD")) {
      this.binarySeen = true;
    }
    const text = this.stripAnsi
      ? stripControlCharacters(decoded)
      : this.preserveNul
        ? decoded
        : removeNul(decoded);
    const bytes = Buffer.byteLength(text, "utf8");
    if (bytes === 0) {
      return;
    }
    const segment: Segment = {text, bytes};
    if (this.headBytes < this.limits.headBytes) {
      const room = this.limits.headBytes - this.headBytes;
      if (bytes <= room) {
        // The whole segment fits: the head window is filled exactly and the
        // stream keeps no gap, so rendering adds no marker.
        this.head.push(segment);
        this.headBytes += bytes;
        return;
      }
      // A chunk larger than the head window is split instead of overflowing the
      // budget; the remainder continues into the tail window.
      const prefix = takePrefix(text, room);
      const prefixBytes = Buffer.byteLength(prefix, "utf8");
      if (prefixBytes > 0) {
        this.head.push({text: prefix, bytes: prefixBytes});
        this.headBytes += prefixBytes;
      }
      segment.text = text.slice(prefix.length);
      segment.bytes = Buffer.byteLength(segment.text, "utf8");
      if (segment.bytes === 0) {
        return;
      }
    }
    if (this.limits.tailBytes === 0) {
      return;
    }
    this.tail.push(segment);
    this.tailBytes += segment.bytes;
    // Drop the oldest bytes until the window fits. Whole segments go first; the
    // first segment that is only partly over is trimmed at its front instead of
    // being dropped, so the window keeps the last tailBytes of the stream no
    // matter how the pipe reads were split.
    while (this.tailBytes > this.limits.tailBytes) {
      const first = this.tail[0];
      if (!first) {
        break;
      }
      const excess = this.tailBytes - this.limits.tailBytes;
      if (first.bytes <= excess) {
        this.tail.shift();
        this.tailBytes -= first.bytes;
        continue;
      }
      const kept = takeTail(first.text, first.bytes - excess);
      const keptBytes = Buffer.byteLength(kept, "utf8");
      this.tail[0] = {text: kept, bytes: keptBytes};
      this.tailBytes -= first.bytes - keptBytes;
    }
  }

  /**
   * Renders this stream's retained windows. The head and the tail share the
   * stream, and the bytes between them become the marker, which counts both the
   * bytes dropped from the windows and the bytes the windows never kept.
   */
  public render(maxLineChars: number, producedBytes = this.producedBytes): StreamOutput {
    let headText = this.head.map((segment) => segment.text).join("");
    let tailText = this.tail.map((segment) => segment.text).join("");
    let shownBytes = this.headBytes + this.tailBytes;
    if (this.lineAware && producedBytes > shownBytes) {
      // A window that holds no line break (one very long line) stays as it is.
      const headEnd = headText.lastIndexOf("\n") + 1;
      if (headEnd > 0 && headEnd < headText.length) {
        shownBytes -= Buffer.byteLength(headText.slice(headEnd), "utf8");
        headText = headText.slice(0, headEnd);
      }
      const tailStart = tailText.indexOf("\n") + 1;
      if (tailStart > 0 && tailStart < tailText.length) {
        shownBytes -= Buffer.byteLength(tailText.slice(0, tailStart), "utf8");
        tailText = tailText.slice(tailStart);
      }
    }
    const omittedBytes = Math.max(producedBytes - shownBytes, 0);
    const body = omittedBytes === 0
      ? `${headText}${tailText}`
      : `${headText}${headText.length > 0 && !headText.endsWith("\n") ? "\n" : ""}${omittedMarker(omittedBytes)}${tailText.length > 0 && !tailText.startsWith("\n") ? "\n" : ""}${tailText}`;
    const text = cutLongLinesWithinBudget(body, maxLineChars);
    return {
      producedBytes,
      shownBytes,
      omittedBytes,
      truncated: omittedBytes > 0,
      text,
    };
  }
}

/**
 * Two streams, two budgets. Each stream is rendered on its own and sections are
 * labeled only when both wrote, so single-stream output stays a clean block.
 */
export class OutputCollector {
  private readonly streams: Record<ChildStream, StreamCollector>;

  public constructor(private readonly limits: OutputLimits) {
    this.streams = {
      stdout: new StreamCollector(limits.stdout, limits.stripAnsi, limits.preserveNul ?? false, limits.lineAware ?? false),
      stderr: new StreamCollector(limits.stderr, limits.stripAnsi, limits.preserveNul ?? false, limits.lineAware ?? false),
    };
  }

  public push(stream: ChildStream, chunk: Buffer): void {
    this.streams[stream].push(chunk);
  }

  /** Flushes partial multibyte sequences at the end of both streams. */
  public end(): void {
    this.streams.stdout.end();
    this.streams.stderr.end();
  }

  public get binary(): boolean {
    return this.streams.stdout.binary || this.streams.stderr.binary;
  }

  public render(producedBytes: Partial<Record<ChildStream, number>> = {}): RenderedOutput {
    const stdout = this.streams.stdout.render(this.limits.maxLineChars, producedBytes.stdout);
    const stderr = this.streams.stderr.render(this.limits.maxLineChars, producedBytes.stderr);
    const labeled = stdout.text.length > 0 && stderr.text.length > 0;
    const sections: string[] = [];
    if (stdout.text.length > 0) {
      sections.push(labeled ? `[stdout]\n${stdout.text}` : stdout.text);
    }
    if (stderr.text.length > 0) {
      sections.push(labeled ? `[stderr]\n${stderr.text}` : stderr.text);
    }
    const text = sections.reduce(
      (joined, section) =>
        joined.length === 0 ? section : `${joined}${joined.endsWith("\n") ? "" : "\n"}${section}`,
      "",
    );
    return {
      text,
      stdout,
      stderr,
      truncated: stdout.truncated || stderr.truncated,
      omittedBytes: stdout.omittedBytes + stderr.omittedBytes,
      binary: this.binary,
      returnedBytes: Buffer.byteLength(text, "utf8"),
    };
  }
}

/**
 * Writes one stream's raw bytes to a file while they are read, up to a ceiling.
 * The file is opened on the first byte so a silent stream leaves no empty file,
 * a write failure is reported instead of thrown, and the ceiling stops the write
 * while the stream keeps flowing (the counters still see every byte).
 */
export class SpillWriter {
  private stream?: WriteStream;
  private failure?: string;
  private closed = false;

  public writtenBytes = 0;
  public capped = false;

  public constructor(private readonly target: SpillTarget) {}

  /**
   * Writes one chunk and returns a promise only when the stream's bounded
   * internal buffer is full. Callers pause the corresponding child pipe until
   * it settles instead of accumulating an unbounded write queue in memory.
   */
  public write(chunk: Buffer): Promise<void> | undefined {
    if (this.failure !== undefined || this.capped || this.closed) {
      return undefined;
    }
    const room = this.target.capBytes - this.writtenBytes;
    if (room <= 0) {
      this.capped = true;
      return undefined;
    }
    const slice = chunk.length <= room ? chunk : chunk.subarray(0, room);
    if (slice.length < chunk.length) {
      this.capped = true;
    }
    if (!this.open()) {
      return undefined;
    }
    try {
      const stream = this.stream!;
      const accepted = stream.write(slice);
      this.writtenBytes += slice.length;
      if (accepted) {
        return undefined;
      }
      return new Promise((resolve) => {
        const settled = (): void => {
          stream.off("drain", settled);
          stream.off("error", settled);
          stream.off("close", settled);
          resolve();
        };
        stream.once("drain", settled);
        stream.once("error", settled);
        stream.once("close", settled);
      });
    } catch (error) {
      this.failure = error instanceof Error ? error.message : String(error);
      return undefined;
    }
  }

  private open(): boolean {
    if (this.stream !== undefined) {
      return true;
    }
    try {
      mkdirSync(dirname(this.target.path), {recursive: true});
      const stream = createWriteStream(this.target.path);
      stream.on("error", (error: NodeJS.ErrnoException) => {
        this.failure = error.message;
      });
      this.stream = stream;
      return true;
    } catch (error) {
      this.failure = error instanceof Error ? error.message : String(error);
      return false;
    }
  }

  /** Flushes the file and reports what it received. */
  public async close(): Promise<SpillOutcome> {
    this.closed = true;
    const stream = this.stream;
    if (stream !== undefined) {
      await new Promise<void>((resolve) => {
        stream.end(() => resolve());
        stream.on("error", () => resolve());
      });
    }
    return {
      path: this.target.path,
      writtenBytes: this.writtenBytes,
      capped: this.capped,
      ...(this.failure !== undefined ? {error: this.failure} : {}),
    };
  }
}

/**
 * Runs one command to completion and returns its bounded output. Never rejects:
 * spawn failures, timeouts, and signals are reported in the result so the tool
 * can map them onto its own error vocabulary.
 */
export function runChildProcess(request: ChildProcessRequest): Promise<ChildProcessResult> {
  return new Promise((resolve) => {
    const collector = new OutputCollector(request.output);
    const startedAt = Date.now();
    let settled = false;
    let timedOut = false;
    let aborted = false;
    let termination: "sigterm" | "sigkill" | undefined;
    let leftRunning = false;
    let exitCode: number | null = null;
    let exitSignal: NodeJS.Signals | null = null;
    let spawnError: {code?: string; message: string} | undefined;
    let sawExit = false;
    const timers: NodeJS.Timeout[] = [];
    const spill: Record<ChildStream, SpillWriter | undefined> = {
      stdout: request.spill?.stdout ? new SpillWriter(request.spill.stdout) : undefined,
      stderr: request.spill?.stderr ? new SpillWriter(request.spill.stderr) : undefined,
    };

    const child = spawn(request.file, request.args, {
      cwd: request.cwd,
      env: request.env,
      stdio: [request.stdin === undefined ? "ignore" : "pipe", "pipe", "pipe"],
      detached: true,
      windowsHide: true,
    });

    if (request.stdin !== undefined && child.stdin) {
      // A program that exits or never reads its input closes the pipe under us
      // (EPIPE); that is the program's choice, not a failure of the call.
      child.stdin.on("error", () => undefined);
      child.stdin.end(request.stdin);
    }

    const signalGroup = (signal: NodeJS.Signals): void => {
      if (child.pid === undefined) {
        return;
      }
      try {
        // Negative pid targets the child's process group, so compilers and
        // servers started by the command die with it.
        process.kill(-child.pid, signal);
      } catch {
        // The group is already gone (ESRCH) or cannot be signalled (EPERM).
      }
    };

    const later = (
      callback: () => void,
      delay: number,
      cancelOnFinish = true,
      keepEventLoopAlive = false,
    ): void => {
      const timer = setTimeout(callback, delay);
      if (!keepEventLoopAlive) {
        timer.unref();
      }
      if (cancelOnFinish) {
        timers.push(timer);
      }
    };

    const finish = (): void => {
      if (settled) {
        return;
      }
      settled = true;
      for (const timer of timers) {
        clearTimeout(timer);
      }
      request.signal?.removeEventListener("abort", abort);
      collector.end();
      const rendered = collector.render();
      const durationMs = Date.now() - startedAt;
      const outcomes = {
        stdout: spill.stdout,
        stderr: spill.stderr,
      };
      // A process left running must not keep reading our pipes, and the streams
      // must not keep the event loop busy after the call has been answered.
      void Promise.all([
        spill.stdout?.close() ?? Promise.resolve(undefined),
        spill.stderr?.close() ?? Promise.resolve(undefined),
      ]).then(([stdout, stderr]) => {
        child.stdout?.destroy();
        child.stderr?.destroy();
        resolve({
          ...rendered,
          exitCode,
          signal: exitSignal,
          timedOut,
          ...(aborted ? {aborted} : {}),
          durationMs,
          ...(termination ? {termination} : {}),
          ...(leftRunning ? {leftRunning} : {}),
          ...(outcomes.stdout || outcomes.stderr
            ? {
                spill: {
                  ...(stdout ? {stdout} : {}),
                  ...(stderr ? {stderr} : {}),
                },
              }
            : {}),
          ...(spawnError ? {spawnError} : {}),
        });
      });
    };

    const terminate = (reason: "timeout" | "abort"): void => {
      if (timedOut || aborted) {
        return;
      }
      timedOut = reason === "timeout";
      aborted = reason === "abort";
      termination = "sigterm";
      signalGroup("SIGTERM");
      later(() => {
        termination = "sigkill";
        signalGroup("SIGKILL");
        // The ladder must end: a process that cannot die must not hold the call
        // open, so the result is reported and the process is left to the system.
        later(() => {
          leftRunning = true;
          finish();
        }, KILL_DEADLINE_MS);
      }, request.killGraceMs);
      if (sawExit) {
        later(finish, DRAIN_AFTER_EXIT_MS);
      }
    };

    const timeout = (): void => terminate("timeout");
    const abort = (): void => terminate("abort");
    request.signal?.addEventListener("abort", abort, {once: true});
    if (request.signal?.aborted) {
      abort();
    }
    if (request.timeoutMs > 0) {
      timers.push(setTimeout(timeout, request.timeoutMs));
    }

    child.on("error", (error: NodeJS.ErrnoException) => {
      spawnError = {
        ...(typeof error.code === "string" ? {code: error.code} : {}),
        message: error.message,
      };
      finish();
    });
    child.on("exit", (code: number | null, signal: NodeJS.Signals | null) => {
      exitCode = code;
      exitSignal = signal;
      sawExit = true;
      if (!timedOut && request.killProcessGroupWhenDone) {
        // Leftover descendants inherited our pipes; signalling the group here
        // (before the child is reaped) keeps the pid from being recycled.
        signalGroup("SIGTERM");
        // Keep escalating after close: descendants can close inherited pipes,
        // make this call finish, and still ignore SIGTERM.
        later(() => signalGroup("SIGKILL"), request.killGraceMs, false, true);
      }
      later(finish, DRAIN_AFTER_EXIT_MS);
    });
    child.on("close", () => {
      finish();
    });
    const capture = (stream: ChildStream, chunk: Buffer): void => {
      collector.push(stream, chunk);
      const source = stream === "stdout" ? child.stdout : child.stderr;
      const drained = spill[stream]?.write(chunk);
      if (!drained || !source) {
        return;
      }
      source.pause();
      void drained.finally(() => {
        if (!settled) {
          source.resume();
        }
      });
    };
    child.stdout?.on("data", (chunk: Buffer) => capture("stdout", chunk));
    child.stderr?.on("data", (chunk: Buffer) => capture("stderr", chunk));
  });
}
