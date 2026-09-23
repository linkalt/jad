import { fieldsFromParts } from "./shared-web-ui.js";
import type { WebUiMessage, WebUiPart } from "./types.js";

/**
 * Transport cap for the web UI projection.
 *
 * A transcript part can be hundreds of kilobytes — a `read` output, a `write`
 * body, a reasoning summary — and a snapshot ships every part of every message
 * it covers. The cap keeps a bounded prefix of each part inline and reports the
 * full size, so a client can decide to fetch the whole message back: a tail
 * message through its turn cursor, a compacted message through the cache item it
 * came from.
 */
export interface WebUiPartCapOptions {
  /** Largest inline text per part, in UTF-8 bytes; `0` disables the cap. */
  maxInlineBytes: number;
  /** Turn cursor per transcript message; the compacted head has no entries. */
  cursors: ReadonlyMap<string, number>;
}

interface ByteCut {
  text: string;
  /** Full UTF-8 size of the text before it was cut. */
  bytes: number;
}

/**
 * Cuts `text` to at most `limit` bytes without splitting a code point.
 *
 * The buffer is sliced at the limit and then walked back over the continuation
 * bytes of a multi-byte character, so the kept prefix is always valid UTF-8.
 */
function cutToBytes(text: string, limit: number): ByteCut | undefined {
  const full = Buffer.from(text, "utf8");
  if (full.length <= limit) {
    return undefined;
  }
  let end = limit;
  while (end > 0 && ((full[end] ?? 0) & 0xc0) === 0x80) {
    end -= 1;
  }
  return {text: full.toString("utf8", 0, end), bytes: full.length};
}

/** The part with its text field capped, or `undefined` when it already fits. */
function capPart(part: WebUiPart, limit: number): WebUiPart | undefined {
  if (part.type === "tool_call") {
    const cut = cutToBytes(part.arguments, limit);
    return cut === undefined
      ? undefined
      : {...part, arguments: cut.text, truncated: {bytes: cut.bytes}};
  }
  if (part.type === "tool_result") {
    const cut = cutToBytes(part.output, limit);
    return cut === undefined
      ? undefined
      : {...part, output: cut.text, truncated: {bytes: cut.bytes}};
  }
  const cut = cutToBytes(part.text, limit);
  return cut === undefined
    ? undefined
    : {...part, text: cut.text, truncated: {bytes: cut.bytes}};
}

/**
 * Caps the inline text of every part, so one huge tool output cannot dominate
 * the payload.
 *
 * A capped message carries the cursor of its turn when it has one — the
 * compacted head does not, and is re-fetched from the cache instead — and its
 * derived fields (`text`, `reasoningContent`) are recomputed from the kept text:
 * shipping the full value beside a cut part would defeat the cap. Messages that
 * fit are returned unchanged, identity included.
 */
export function capWebUiMessages(
  messages: readonly WebUiMessage[],
  options: WebUiPartCapOptions,
): WebUiMessage[] {
  const {maxInlineBytes, cursors} = options;
  if (maxInlineBytes <= 0) {
    return [...messages];
  }
  return messages.map((message) => {
    let capped = false;
    const parts = message.parts.map((part) => {
      const cappedPart = capPart(part, maxInlineBytes);
      if (cappedPart === undefined) {
        return part;
      }
      capped = true;
      return cappedPart;
    });
    if (!capped) {
      return message;
    }
    const cursor = cursors.get(message.id);
    return {
      ...message,
      parts,
      ...(cursor === undefined ? {} : {cursor}),
      ...fieldsFromParts(parts),
    };
  });
}
