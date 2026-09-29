/**
 * @file json-bounds.ts
 * @input A dynamic JSON value and the bounds it must fit in
 * @output Exports depth and byte measurement for JSON values
 * @position Shared by the tool that proposes a draft and the endpoint that accepts the edit
 *
 * A dynamic payload crosses three boundaries — the provider request, the run's
 * message state and the answer endpoint — so the tool that proposes it and the
 * endpoint that takes the user's version measure it the same way. Depth counts
 * containers exactly as memory's records count them, so one configured limit
 * means one thing everywhere.
 */

/** The bounds a dynamic JSON payload must fit in; both are configured per tool. */
export interface DraftBounds {
  /** Largest accepted payload, in bytes of its JSON encoding. */
  max_draft_bytes: number;
  /** Deepest accepted nesting, counting containers as memory's records do. */
  max_draft_depth: number;
}

/** Nesting depth: `{a: 1}` is 1, `{a: {b: 1}}` is 2, a scalar is 0. */
export function jsonDepth(value: unknown): number {
  if (typeof value !== "object" || value === null) {
    return 0;
  }
  return 1 + Math.max(0, ...Object.values(value).map(jsonDepth));
}

/** The payload's size as it travels: UTF-8 bytes of its JSON encoding. */
export function jsonBytes(value: unknown): number {
  return Buffer.byteLength(JSON.stringify(value) ?? "", "utf8");
}

/**
 * Why the payload does not fit, as a sentence a tool result or an HTTP refusal
 * can carry, or `undefined` when it fits.
 */
export function draftBoundsError(value: unknown, bounds: DraftBounds): string | undefined {
  const bytes = jsonBytes(value);
  if (bytes > bounds.max_draft_bytes) {
    return `the payload is ${bytes} bytes; the limit is ${bounds.max_draft_bytes}`;
  }
  const depth = jsonDepth(value);
  if (depth > bounds.max_draft_depth) {
    return `the payload nests ${depth} levels; the limit is ${bounds.max_draft_depth}`;
  }
  return undefined;
}
