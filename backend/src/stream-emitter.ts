import type {WebUiMessage, WebUiPart} from "./plugins/web-ui/types.js";
import {
  messageAppendIncrement,
  type WebUiMessageAppend,
} from "./plugins/web-ui/incremental-web-ui.js";

/**
 * A message is published for every provider delta, and a complete snapshot of it
 * is orders of magnitude larger than the delta that changed it — one 5.9 KB
 * `write` body produced 1244 snapshots and 4.8 MB on the wire, and a message
 * that carries the turn's tool results re-sent all of them per delta.
 *
 * Only growth waits for a coalescing window. A state whose only change is an
 * appended text, reasoning or tool-arguments part is held for at most
 * `intervalMs`; anything else — a new part (a tool result, a finished call), a
 * status change — goes out immediately, after flushing the held state so a
 * subscriber never sees an older state after a newer one.
 */
export class MessageStreamEmitter {
  private pending: WebUiMessage | undefined;
  private pendingAppend: {index: number; startLength: number} | undefined;
  private timer: NodeJS.Timeout | undefined;
  private lastEmitAt = Number.NEGATIVE_INFINITY;
  private lastEmitted: WebUiMessage | undefined;

  public constructor(
    private readonly intervalMs: number,
    private readonly emit: (message: WebUiMessage, append?: WebUiMessageAppend) => void,
  ) {}

  public push(message: WebUiMessage, append?: WebUiMessageAppend): void {
    if (this.intervalMs <= 0) {
      this.flush();
      this.emitNow(message, append);
      return;
    }
    if (append === undefined || append.append.length === 0) {
      this.flush();
      this.emitNow(message);
      return;
    }

    const previous = this.pending ?? this.lastEmitted;
    if (previous === undefined || messageAppendIncrement(previous, message, append) === undefined) {
      this.flush();
      this.emitNow(message);
      return;
    }

    if (this.pending !== undefined) {
      if (this.pendingAppend?.index !== append.index) {
        this.flush();
        this.emitNow(message, append);
        return;
      }
      // Keep only the newest snapshot and the starting offset. The appended
      // suffix is sliced once when the coalescing window flushes; no delta queue
      // or repeated concatenation is needed.
      this.pending = message;
      this.schedule();
      return;
    }

    // Nothing is waiting and the window is open: the caller sees this state at
    // once instead of one tick later, so a part appears the moment it starts.
    if (this.timer === undefined && Date.now() - this.lastEmitAt >= this.intervalMs) {
      this.emitNow(message, append);
      return;
    }

    const startLength = appendableLength(previous.parts[append.index]);
    if (startLength === undefined) {
      this.emitNow(message);
      return;
    }
    this.pending = message;
    this.pendingAppend = {index: append.index, startLength};
    this.schedule();
  }

  /** Sends the newest coalesced snapshot, if one is waiting. */
  public flush(): void {
    this.clearTimer();
    const pending = this.pending;
    const pendingAppend = this.pendingAppend;
    this.pending = undefined;
    this.pendingAppend = undefined;
    if (pending !== undefined) {
      let append: WebUiMessageAppend | undefined;
      if (pendingAppend !== undefined) {
        const text = appendableText(pending.parts[pendingAppend.index]);
        if (text !== undefined && text.length > pendingAppend.startLength) {
          append = {
            index: pendingAppend.index,
            append: text.slice(pendingAppend.startLength),
          };
        }
      }
      this.emitNow(pending, append);
    }
  }

  /** Drops a coalesced snapshot that a newer state has superseded. */
  public discard(): void {
    this.clearTimer();
    this.pending = undefined;
    this.pendingAppend = undefined;
  }

  private schedule(): void {
    if (this.timer !== undefined) {
      return;
    }
    const wait = Math.max(0, this.intervalMs - (Date.now() - this.lastEmitAt));
    this.timer = setTimeout(() => {
      this.timer = undefined;
      this.flush();
    }, wait);
    this.timer.unref?.();
  }

  private emitNow(message: WebUiMessage, append?: WebUiMessageAppend): void {
    this.lastEmitAt = Date.now();
    this.lastEmitted = message;
    this.emit(message, append);
  }

  private clearTimer(): void {
    if (this.timer !== undefined) {
      clearTimeout(this.timer);
      this.timer = undefined;
    }
  }
}

function appendableText(part: WebUiPart | undefined): string | undefined {
  if (part?.type === "tool_call") {
    return part.arguments;
  }
  if (part?.type === "text" || part?.type === "reasoning") {
    return part.text;
  }
  return undefined;
}

function appendableLength(part: WebUiPart | undefined): number | undefined {
  return appendableText(part)?.length;
}

/** True while a tool call is still receiving its arguments. */
export function isStreamingToolCallMessage(message: WebUiMessage): boolean {
  return message.parts.some(
    (part) => part.type === "tool_call" && part.partial === true,
  );
}
