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
 * means one thing everywhere. Traversal is iterative: an over-deep but small
 * JSON value must receive a normal bounds error, not exhaust the JS call stack.
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
  if (typeof value !== "object" || value === null) return 0;

  let deepest = 1;
  const stack: Array<{values: Iterator<unknown>; depth: number}> = [
    {values: Object.values(value).values(), depth: 1},
  ];
  while (stack.length > 0) {
    const frame = stack[stack.length - 1]!;
    const child = frame.values.next();
    if (child.done) {
      stack.pop();
      continue;
    }
    if (typeof child.value !== "object" || child.value === null) continue;
    const depth = frame.depth + 1;
    deepest = Math.max(deepest, depth);
    stack.push({values: Object.values(child.value).values(), depth});
  }
  return deepest;
}

/**
 * The payload's size as it travels: UTF-8 bytes of its JSON encoding.
 * Inputs at the tool/HTTP boundaries are parsed JSON values. This iterative
 * counter preserves JSON.stringify's byte count without recursively stringifying
 * a deeply nested value.
 */
export function jsonBytes(value: unknown): number {
  if (value === undefined || typeof value === "function" || typeof value === "symbol") return 0;

  type Frame = {value: unknown; inArray?: boolean} | {leave: object};
  const frames: Frame[] = [{value}];
  const ancestors = new Set<object>();
  let bytes = 0;
  const addText = (text: string): void => {
    bytes += Buffer.byteLength(text, "utf8");
  };

  while (frames.length > 0) {
    const frame = frames.pop()!;
    if ("leave" in frame) {
      ancestors.delete(frame.leave);
      continue;
    }

    const current = frame.value;
    if (current === undefined || typeof current === "function" || typeof current === "symbol") {
      if (frame.inArray) addText("null");
      continue;
    }
    if (current === null) {
      addText("null");
      continue;
    }
    if (typeof current === "string" || typeof current === "number" || typeof current === "boolean") {
      addText(JSON.stringify(current) ?? "null");
      continue;
    }
    if (typeof current === "bigint") {
      throw new TypeError("Do not know how to serialize a BigInt");
    }
    if (typeof current !== "object") continue;
    if (ancestors.has(current)) {
      throw new TypeError("Converting circular structure to JSON");
    }
    ancestors.add(current);
    frames.push({leave: current});

    if (Array.isArray(current)) {
      addText("[");
      if (current.length > 0) addText(",".repeat(current.length - 1));
      for (let index = current.length - 1; index >= 0; index -= 1) {
        frames.push({value: current[index], inArray: true});
      }
      addText("]");
      continue;
    }

    const record = current as Record<string, unknown>;
    const keys = Object.keys(record).filter((key) => {
      const child = record[key];
      return child !== undefined && typeof child !== "function" && typeof child !== "symbol";
    });
    addText("{");
    if (keys.length > 0) addText(",".repeat(keys.length - 1));
    for (let index = keys.length - 1; index >= 0; index -= 1) {
      const key = keys[index]!;
      frames.push({value: record[key]});
      addText(JSON.stringify(key));
      addText(":");
    }
    addText("}");
  }
  return bytes;
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
