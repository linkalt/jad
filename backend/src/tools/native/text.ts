/**
 * @file text.ts
 * @input Decoded file text and search text
 * @output Exports UTF-8 decoding, line-ending, occurrence and diagnostic helpers
 * @position Pure helpers shared by the native text tools (used by `read`, `write`, and `edit`)
 *
 * Everything here is pure and allocation-light: it runs on file content that is
 * already bounded by the configured native tool byte limits, and it never uses
 * regular expressions for the caller's text so replacement strings are always
 * inserted literally (`$&` and friends keep their literal meaning).
 */

export type LineEnding = "lf" | "crlf" | "cr" | "mixed";

export type ContentEncoding = "utf8" | "base64";
export type RangeType = "lines" | "bytes";

/** Diagnostic excerpts stay single-line and short enough to be cheap context. */
export const MAX_DIAGNOSTIC_CHARS = 200;

export function decodeUtf8(data: Buffer): string | undefined {
  try {
    return new TextDecoder("utf-8", {fatal: true}).decode(data);
  } catch {
    return undefined;
  }
}

/**
 * Normalizes the near-miss spellings a model produces for the encoding enum
 * ("utf-8", "UTF8") to the canonical schema value. Unknown values stay
 * undefined: the schema rejects them before execution.
 */
export function normalizeEncoding(value: unknown): ContentEncoding | undefined {
  if (typeof value !== "string") {
    return undefined;
  }
  const normalized = value.trim().toLowerCase().replace(/[^a-z0-9]/g, "");
  if (normalized === "utf8") {
    return "utf8";
  }
  return normalized === "base64" ? "base64" : undefined;
}

/** Normalizes the near-miss spellings of a range unit ("Lines", "BYTE"). */
export function normalizeRangeType(value: unknown): RangeType | undefined {
  if (typeof value !== "string") {
    return undefined;
  }
  const normalized = value.trim().toLowerCase().replace(/[^a-z]/g, "");
  if (normalized === "line" || normalized === "lines") {
    return "lines";
  }
  if (normalized === "byte" || normalized === "bytes") {
    return "bytes";
  }
  return undefined;
}

/** Detects the line ending style of decoded text; undefined when it has none. */
export function detectLineEnding(text: string): LineEnding | undefined {
  let lf = false;
  let crlf = false;
  let cr = false;
  for (let index = 0; index < text.length; index += 1) {
    const value = text[index];
    if (value === "\n") {
      if (index > 0 && text[index - 1] === "\r") {
        crlf = true;
      } else {
        lf = true;
      }
    } else if (value === "\r" && text[index + 1] !== "\n") {
      cr = true;
    }
  }
  const styles = [lf, crlf, cr].filter(Boolean).length;
  if (styles === 0) {
    return undefined;
  }
  if (styles > 1) {
    return "mixed";
  }
  if (crlf) {
    return "crlf";
  }
  return cr ? "cr" : "lf";
}

/**
 * Rewrites text to the file's line ending style so an agent can send text with
 * plain LF endings. Text for an unknown or mixed file is returned unchanged:
 * only an exact byte match is safe when the file has no single style.
 */
export function withLineEnding(text: string, ending: LineEnding | undefined): string {
  if (ending === undefined || ending === "mixed") {
    return text;
  }
  const normalized = text.replace(/\r\n/g, "\n").replace(/\r/g, "\n");
  if (ending === "lf") {
    return normalized;
  }
  return ending === "crlf"
    ? normalized.replace(/\n/g, "\r\n")
    : normalized.replace(/\n/g, "\r");
}

export function countNewlines(text: string): number {
  return countNewlinesBetween(text, 0, text.length);
}

function countNewlinesBetween(text: string, from: number, to: number): number {
  let count = 0;
  const end = Math.min(to, text.length);
  for (let index = Math.max(0, from); index < end; index += 1) {
    if (text.charCodeAt(index) === 10) {
      count += 1;
    }
  }
  return count;
}

/** 1-based line number of a character position. */
export function lineNumberAt(text: string, position: number): number {
  let line = 1;
  const end = Math.min(position, text.length);
  for (let index = 0; index < end; index += 1) {
    if (text.charCodeAt(index) === 10) {
      line += 1;
    }
  }
  return line;
}

/** Counts non-overlapping occurrences without allocating one entry per match. */
export function countOccurrences(text: string, search: string): number {
  if (search.length === 0) {
    return 0;
  }
  let count = 0;
  let index = text.indexOf(search);
  while (index !== -1) {
    count += 1;
    index = text.indexOf(search, index + search.length);
  }
  return count;
}

/** 1-based line numbers of the first `limit` occurrences. */
export function occurrenceLineNumbers(
  text: string,
  search: string,
  limit: number,
): number[] {
  const lines: number[] = [];
  if (search.length === 0 || limit < 1) {
    return lines;
  }
  let line = 1;
  let scanned = 0;
  let index = text.indexOf(search);
  while (index !== -1 && lines.length < limit) {
    line += countNewlinesBetween(text, scanned, index);
    lines.push(line);
    scanned = index;
    index = text.indexOf(search, index + search.length);
  }
  return lines;
}

/**
 * Replaces at most `limit` non-overlapping occurrences, inserting `replacement`
 * literally. Returns the first match position so callers can report lines.
 */
export function replaceOccurrences(
  text: string,
  search: string,
  replacement: string,
  limit: number,
): {text: string; count: number; firstIndex?: number} {
  if (search.length === 0 || limit < 1) {
    return {text, count: 0};
  }
  const parts: string[] = [];
  let count = 0;
  let firstIndex: number | undefined;
  let cursor = 0;
  for (;;) {
    const index = text.indexOf(search, cursor);
    if (index === -1 || count >= limit) {
      parts.push(text.slice(cursor));
      break;
    }
    firstIndex ??= index;
    parts.push(text.slice(cursor, index), replacement);
    cursor = index + search.length;
    count += 1;
  }
  return {...(firstIndex !== undefined ? {firstIndex} : {}), text: parts.join(""), count};
}

/** Trims a value to one short line for an INFORMATION diagnostic field. */
export function truncateForDiagnostic(value: string): string {
  const single = value.replace(/[\r\n]+/g, " ");
  return single.length > MAX_DIAGNOSTIC_CHARS
    ? `${single.slice(0, MAX_DIAGNOSTIC_CHARS)}…`
    : single;
}

export interface MatchDiagnostic {
  /** 1-based line of the closest line whose trimmed text matches the anchor. */
  nearestLine: number;
  /** How many leading oldText lines match the file exactly at `nearestLine`. */
  matchedLines: number;
  expected: string;
  actual: string;
  whitespaceOnly: boolean;
}

/**
 * Locates the closest candidate for a failed match: the first file line whose
 * trimmed text equals the trimmed first line of the search text, plus the first
 * differing line. This is what turns a failed edit into one corrected retry
 * instead of a full re-read.
 */
export function diagnoseMatch(text: string, oldText: string): MatchDiagnostic | undefined {
  const expectedLines = oldText.split("\n");
  const anchor = (expectedLines[0] ?? "").trim();
  if (anchor.length === 0) {
    return undefined;
  }
  const lines = text.split("\n");
  let start = -1;
  for (let index = 0; index < lines.length; index += 1) {
    if (lines[index]!.trim() === anchor) {
      start = index;
      break;
    }
  }
  if (start === -1) {
    return undefined;
  }

  let matchedLines = 0;
  while (
    matchedLines < expectedLines.length &&
    lines[start + matchedLines] === expectedLines[matchedLines]
  ) {
    matchedLines += 1;
  }
  const lastIndex = expectedLines.length - 1;
  const expectedLine = matchedLines <= lastIndex
    ? expectedLines[matchedLines]!
    : expectedLines[lastIndex]!;
  const actualLine = lines[start + matchedLines] ?? "";
  return {
    nearestLine: start + 1,
    matchedLines,
    expected: truncateForDiagnostic(expectedLine),
    actual: truncateForDiagnostic(actualLine),
    whitespaceOnly: expectedLine.trim() === actualLine.trim(),
  };
}
