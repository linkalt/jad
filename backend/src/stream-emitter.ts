import type {WebUiMessage} from "./plugins/web-ui/types.js";

/**
 * A message snapshot is cumulative: every one supersedes the previous. While a
 * long tool argument streams, the provider emits a small delta every few
 * characters, so publishing a full snapshot per delta grows quadratically — one
 * 5.9 KB `write` body produced 1244 snapshots and 4.8 MB on the wire.
 *
 * This coalesces the in-progress snapshots to at most one per interval. Nothing
 * that matters is delayed: a snapshot without an in-progress tool call goes out
 * immediately, and `flush()` (used before tool results and terminal events) sends
 * the newest coalesced state first.
 */
export class MessageStreamEmitter {
  private pending: WebUiMessage | undefined;
  private timer: NodeJS.Timeout | undefined;
  private lastEmitAt = Number.NEGATIVE_INFINITY;

  public constructor(
    private readonly intervalMs: number,
    private readonly emit: (message: WebUiMessage) => void,
  ) {}

  public push(message: WebUiMessage): void {
    if (this.intervalMs <= 0 || !isStreamingToolCallMessage(message)) {
      this.flush();
      this.emitNow(message);
      return;
    }
    // Nothing is waiting and the window is open: the caller sees this state at
    // once instead of one tick later, so a tool call appears the moment it starts.
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
