/**
 * @file ripgrep.ts
 * @input Search flags, pattern, paths, and the configured budgets
 * @output Exports the ripgrep process runner and its structured match groups
 * @position Process and output layer behind the native `grep` tool
 *
 * The runner owns everything that must not be re-implemented per tool:
 * deterministic invocation (`--json --no-config --sort path`), argument safety
 * (no shell, `--` before pattern and paths), streaming JSON Lines parsing,
 * result/byte budgets, and the wall-clock kill. Rendering and policy filtering
 * stay in the tool, because heading paths must be resolved first.
 */

import {spawn} from "node:child_process";
import {environmentWithoutApiToken} from "../../server-secrets.js";

import {MAX_DIAGNOSTIC_CHARS} from "./text.js";

export type RipgrepEntryKind = "match" | "context";

export interface RipgrepEntry {
  kind: RipgrepEntryKind;
  line: number;
  text: string;
}

export interface RipgrepGroup {
  /** Path exactly as ripgrep printed it, with a leading "./" removed. */
  path: string;
  entries: RipgrepEntry[];
  matches: number;
}

export type RipgrepTruncationReason = "max_results" | "max_content_bytes" | "timeout";

export type RipgrepFailureKind = "unavailable" | "invalid_pattern" | "failed";

export interface RipgrepFailure {
  kind: RipgrepFailureKind;
  /** Bounded, single-line diagnostic suitable for a tool result. */
  message: string;
}

export interface RipgrepResult {
  groups: RipgrepGroup[];
  matches: number;
  /** Bytes of match text collected before the budget stopped the search. */
  textBytes: number;
  truncated: boolean;
  truncationReason?: RipgrepTruncationReason;
  failure?: RipgrepFailure;
  /** Set when ripgrep reported a match inside a binary file we cannot decode. */
  binarySkipped: number;
  /** Set when any file returned as many matches as the per-file cap allows. */
  perFileLimitReached: boolean;
}

export interface RipgrepSearch {
  binary: string;
  /** Tool flags, for example --glob, --context, --fixed-strings. */
  flags: string[];
  pattern: string;
  paths: string[];
  cwd: string;
  timeoutMs: number;
  /** Cancels this foreground search and its process group. */
  signal?: AbortSignal;
  /** Matches returned overall; enforced while parsing. */
  maxMatches: number;
  /** Matches per file (ripgrep --max-count). */
  maxMatchesPerFile: number;
  /** Largest file to search (ripgrep --max-filesize). */
  maxFileBytes: number;
  maxBytes: number;
  maxLineChars: number;
}

const MAX_STDERR_BYTES = 8 * 1024;
/** Internal cleanup bounds; the configured search timeout remains the user-facing limit. */
const KILL_GRACE_MS = 250;
const KILL_DEADLINE_MS = 1000;
const DRAIN_AFTER_EXIT_MS = 500;

interface JsonLine {
  type?: unknown;
  data?: Record<string, unknown>;
}

function textOf(value: unknown): string | undefined {
  if (typeof value !== "object" || value === null) {
    return undefined;
  }
  const record = value as Record<string, unknown>;
  return typeof record.text === "string" ? record.text : undefined;
}

function numberField(value: unknown): number | undefined {
  return typeof value === "number" && Number.isFinite(value) ? value : undefined;
}

/** Splits a JSON lines payload into complete lines, ignoring a torn tail. */
function takeLines(buffer: string): {lines: string[]; rest: string} {
  const parts = buffer.split("\n");
  const rest = parts.pop() ?? "";
  return {lines: parts, rest};
}

function firstLine(value: string): string {
  const line = value.split("\n")[0]?.trim() ?? "";
  return line.length > MAX_DIAGNOSTIC_CHARS
    ? `${line.slice(0, MAX_DIAGNOSTIC_CHARS)}...`
    : line;
}

/**
 * ripgrep prints a multi-line regex diagnostic; the first line alone only says
 * "regex parse error", so keep a few non-empty lines joined into one bounded
 * line that still names the offending pattern.
 */
function firstDiagnostic(value: string, maxLines = 3): string {
  const lines = value
    .split("\n")
    .map((line) => line.trim())
    .filter((line) => line.length > 0)
    .slice(0, maxLines);
  const joined = lines.join(" / ");
  return joined.length > MAX_DIAGNOSTIC_CHARS
    ? `${joined.slice(0, MAX_DIAGNOSTIC_CHARS)}...`
    : joined;
}

function failureKind(exitCode: number | null, stderr: string): RipgrepFailure | undefined {
  if (exitCode === null || exitCode === 0 || exitCode === 1) {
    return undefined;
  }
  if (/regex parse error|error parsing regex/i.test(stderr)) {
    return {kind: "invalid_pattern", message: firstDiagnostic(stderr)};
  }
  return {
    kind: "failed",
    message: firstDiagnostic(stderr) || `ripgrep exited with ${exitCode}`,
  };
}

/**
 * Runs one search and returns structured groups plus how it stopped. A kill by
 * budget or timeout is reported as truncation, never as a failure: the partial
 * result stays usable.
 */
export function runRipgrep(search: RipgrepSearch): Promise<RipgrepResult> {
  const argv = [
    "--json",
    "--no-config",
    "--color",
    "never",
    "--sort",
    "path",
    "--max-count",
    String(search.maxMatchesPerFile),
    "--max-filesize",
    String(search.maxFileBytes),
    ...search.flags,
    "--",
    search.pattern,
    ...search.paths,
  ];

  return new Promise<RipgrepResult>((resolve) => {
    const groups: RipgrepGroup[] = [];
    const byPath = new Map<string, RipgrepGroup>();
    let matches = 0;
    let textBytes = 0;
    let binarySkipped = 0;
    let perFileLimitReached = false;
    let truncated = false;
    let truncationReason: RipgrepTruncationReason | undefined;
    let buffer = "";
    let stderr = "";
    let settled = false;
    let timedOut = false;
    let stopped = false;
    let exitCode: number | null = null;
    const timers: NodeJS.Timeout[] = [];

    const child = spawn(search.binary, argv, {
      cwd: search.cwd,
      env: environmentWithoutApiToken(process.env),
      stdio: ["ignore", "pipe", "pipe"],
      // A separate group lets cleanup cover a wrapped or misbehaving binary.
      detached: true,
      windowsHide: true,
    });

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

    const signalProcess = (signal: NodeJS.Signals): void => {
      if (child.pid === undefined) {
        return;
      }
      try {
        process.kill(-child.pid, signal);
      } catch {
        // Windows has no negative process groups; fall back to the leader.
        try {
          child.kill(signal);
        } catch {
          // The child is already gone or cannot be signalled.
        }
      }
    };

    const finish = (exitCode: number | null, spawnError?: Error, force = false): void => {
      if (settled) {
        return;
      }
      settled = true;
      for (const timer of timers) {
        clearTimeout(timer);
      }
      search.signal?.removeEventListener("abort", abort);
      if (force) {
        child.stdout?.destroy();
        child.stderr?.destroy();
      }
      const failure = timedOut
        ? undefined
        : spawnError
          ? {kind: "unavailable" as const, message: firstLine(spawnError.message)}
          : failureKind(exitCode, stderr);
      resolve({
        groups,
        matches,
        textBytes,
        truncated,
        ...(truncationReason ? {truncationReason} : {}),
        ...(failure ? {failure} : {}),
        binarySkipped,
        perFileLimitReached,
      });
    };

    const stop = (reason: RipgrepTruncationReason): void => {
      if (!truncated) {
        truncated = true;
        truncationReason = reason;
      }
      if (stopped) {
        return;
      }
      stopped = true;
      signalProcess("SIGTERM");
      later(() => {
        signalProcess("SIGKILL");
        // A descendant retaining stdout/stderr must not make the tool exceed
        // its timeout after the leader was killed.
        later(() => finish(null, undefined, true), KILL_DEADLINE_MS);
      }, KILL_GRACE_MS);
    };

    function abort(): void {
      stop("timeout");
    }

    const timer = setTimeout(() => {
      timedOut = true;
      stop("timeout");
    }, search.timeoutMs);
    timers.push(timer);
    search.signal?.addEventListener("abort", abort, {once: true});
    if (search.signal?.aborted) {
      abort();
    }

    child.on("error", (error: Error) => finish(null, error));
    child.on("exit", (code: number | null) => {
      exitCode = code;
      if (!stopped) {
        // ripgrep is foreground work too. A wrapper can leave a descendant
        // holding no pipes, so TERM/SIGKILL must outlive this leader's close.
        signalProcess("SIGTERM");
        later(() => signalProcess("SIGKILL"), KILL_GRACE_MS, false, true);
      }
      later(() => finish(code, undefined, true), DRAIN_AFTER_EXIT_MS);
    });
    child.on("close", (code: number | null) => finish(code ?? exitCode));
    child.stderr?.on("data", (chunk: Buffer | string) => {
      if (stderr.length < MAX_STDERR_BYTES) {
        stderr += chunk.toString();
      }
    });

    const pushEntry = (path: string, entry: RipgrepEntry): void => {
      let group = byPath.get(path);
      if (!group) {
        group = {path, entries: [], matches: 0};
        byPath.set(path, group);
        groups.push(group);
      }
      group.entries.push(entry);
      if (entry.kind === "match") {
        group.matches += 1;
        if (group.matches >= search.maxMatchesPerFile) {
          perFileLimitReached = true;
        }
      }
    };

    const handleEvent = (event: JsonLine): void => {
      const data = event.data;
      if (!data) {
        return;
      }
      const path = textOf(data.path);
      if (!path) {
        // A path given as bytes is not usable text: skip the file entirely.
        if (event.type === "match") {
          binarySkipped += 1;
        }
        return;
      }
      const display = path.startsWith("./") ? path.slice(2) : path;
      if (event.type !== "match" && event.type !== "context") {
        return;
      }
      const linesText = textOf(data.lines);
      if (linesText === undefined) {
        if (event.type === "match") {
          binarySkipped += 1;
        }
        return;
      }
      const startLine = numberField(data.line_number) ?? 1;
      const kind: RipgrepEntryKind = event.type === "match" ? "match" : "context";
      const rawLines = linesText.split("\n");
      if (rawLines.length > 0 && rawLines[rawLines.length - 1] === "") {
        rawLines.pop();
      }
      for (const [index, raw] of rawLines.entries()) {
        if (kind === "match") {
          if (matches >= search.maxMatches) {
            stop("max_results");
            return;
          }
          matches += 1;
        }
        const text = raw.length > search.maxLineChars
          ? `${raw.slice(0, search.maxLineChars)} [... truncated line]`
          : raw;
        const entryBytes = Buffer.byteLength(text, "utf8") + 16;
        if (textBytes + entryBytes > search.maxBytes) {
          stop("max_content_bytes");
          return;
        }
        textBytes += entryBytes;
        pushEntry(display, {kind, line: startLine + index, text});
      }
    };

    child.stdout?.setEncoding("utf8");
    child.stdout?.on("data", (chunk: string) => {
      if (stopped) {
        return;
      }
      buffer += chunk;
      const {lines, rest} = takeLines(buffer);
      buffer = rest;
      for (const line of lines) {
        if (stopped) {
          return;
        }
        let parsed: JsonLine;
        try {
          parsed = JSON.parse(line) as JsonLine;
        } catch {
          continue;
        }
        handleEvent(parsed);
      }
    });
  });
}

/** Renders groups in ripgrep's heading format: path, then `line:text` rows. */
export function renderRipgrepGroups(groups: RipgrepGroup[]): string {
  return groups
    .map((group) => {
      const lines = group.entries.map(
        (entry) => `${entry.line}${entry.kind === "match" ? ":" : "-"}${entry.text}`,
      );
      return [group.path, ...lines].join("\n");
    })
    .join("\n\n");
}
