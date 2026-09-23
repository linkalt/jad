import type {WebUiMessage} from "./plugins/web-ui/types.js";
import {messageIncrements} from "./plugins/web-ui/incremental-web-ui.js";

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
  private timer: NodeJS.Timeout | undefined;
  private lastEmitAt = Number.NEGATIVE_INFINITY;
  private lastEmitted: WebUiMessage | undefined;

  public constructor(
    private readonly intervalMs: number,
    private readonly emit: (message: WebUiMessage) => void,
  ) {}

  public push(message: WebUiMessage): void {
    if (this.intervalMs <= 0 || !this.canCoalesce(message)) {
      this.flush();
      this.emitNow(message);
      return;
    }
    // Nothing is waiting and the window is open: the caller sees this state at
    // once instead of one tick later, so a part appears the moment it starts.
    if (
      this.pending === undefined &&
      this.timer === undefined &&
      Date.now() - this.lastEmitAt >= this.intervalMs
    ) {
      this.emitNow(message);
      return;
    }
    this.pending = message;
    this.schedule();
  }

  /**
   * Only a state that grew its last part can wait: a change a client cannot
   * express as an append is visible at once.
   */
  private canCoalesce(message: WebUiMessage): boolean {
    const previous = this.lastEmitted;
    if (previous === undefined) {
      return false;
    }
    const increments = messageIncrements(previous, message);
    return increments !== undefined && increments.every((item) => item.event === "ui.delta");
  }

  /** Sends the newest coalesced snapshot, if one is waiting. */
  public flush(): void {
    this.clearTimer();
    const pending = this.pending;
    this.pending = undefined;
    if (pending !== undefined) {
      this.emitNow(pending);
    }
  }

  /** Drops a coalesced snapshot that a newer state has superseded. */
  public discard(): void {
    this.clearTimer();
    this.pending = undefined;
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

  private emitNow(message: WebUiMessage): void {
    this.lastEmitAt = Date.now();
    this.lastEmitted = message;
    this.emit(message);
  }

  private clearTimer(): void {
    if (this.timer !== undefined) {
      clearTimeout(this.timer);
      this.timer = undefined;
    }
  }
}

/** True while a tool call is still receiving its arguments. */
export function isStreamingToolCallMessage(message: WebUiMessage): boolean {
  return message.parts.some(
    (part) => part.type === "tool_call" && part.partial === true,
  );
}
