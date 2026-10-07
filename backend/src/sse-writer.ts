export interface SseWritableResponse {
  readonly writableEnded: boolean;
  readonly destroyed: boolean;
  readonly writableLength: number;
  write(chunk: string): boolean;
  destroy(): void;
}

/**
 * Events that close a run's stream: the answer, the failure, and the end marker.
 * They are finite and the connection ends right after them, so they are always
 * written: refusing one would cut a client off from the result it just waited
 * for, however large the message before it was.
 */
const TERMINAL_EVENTS: ReadonlySet<string> = new Set(["completed", "error", "done"]);

/**
 * For a response whose last full `ui.message` snapshot went over the budget: the
 * bytes written after that snapshot. The snapshot itself, and what was queued in
 * front of it, is exempt from the budget (it is the one frame the protocol
 * cannot make smaller), so a client that reads it keeps its stream. Only the
 * frames behind it count, which still bounds a stalled client.
 */
const bytesBehindSnapshot = new WeakMap<object, number>();

/** What the budget counts for this response right now. */
function queuedBytes(response: SseWritableResponse): number {
  const behind = bytesBehindSnapshot.get(response);
  if (behind === undefined) {
    return response.writableLength;
  }
  if (response.writableLength === 0) {
    bytesBehindSnapshot.delete(response);
    return 0;
  }
  // The queue drains from the front, so what is behind the snapshot can never be
  // more than what is still queued.
  return Math.min(response.writableLength, behind);
}

/**
 * Writes one SSE frame without allowing queued frames to grow with a stalled
 * client. A single frame may exceed the configured limit; it is the smallest
 * unit the current protocol can send. A full `ui.message` snapshot may also
 * exceed the remaining budget so a reconnect can resynchronize; that snapshot is
 * not counted against the frames that follow it, but those frames are bounded by
 * the budget. The terminal events of a run (`completed`, `error`, `done`) are
 * always written.
 */
export function writeSse(
  response: SseWritableResponse,
  event: string,
  value: unknown,
  maxBufferedBytes: number,
): boolean {
  if (response.writableEnded || response.destroyed) {
    return false;
  }
  try {
    const frame = `event: ${event}\ndata: ${JSON.stringify(value)}\n\n`;
    if (TERMINAL_EVENTS.has(event)) {
      return response.write(frame);
    }
    const bufferedBytes = queuedBytes(response);
    if (bufferedBytes > maxBufferedBytes) {
      response.destroy();
      return false;
    }
    const frameBytes = Buffer.byteLength(frame);
    const wouldExceedLimit = bufferedBytes + frameBytes > maxBufferedBytes;
    const isSnapshot = event === "ui.message";
    // An over-budget snapshot is let through only when the socket's own queue is
    // within the budget, so a stalled client cannot pile up one big snapshot after
    // another. Any other over-budget frame is let through only when nothing is
    // counted yet (it is then the smallest unit the protocol can send).
    const refuse = isSnapshot
      ? wouldExceedLimit && response.writableLength > maxBufferedBytes
      : wouldExceedLimit && bufferedBytes > 0;
    if (refuse) {
      response.destroy();
      return false;
    }
    const written = response.write(frame);
    if (isSnapshot && wouldExceedLimit) {
      bytesBehindSnapshot.set(response, 0);
    } else if (bytesBehindSnapshot.has(response)) {
      bytesBehindSnapshot.set(response, (bytesBehindSnapshot.get(response) ?? 0) + frameBytes);
    }
    return written;
  } catch {
    // A disconnected browser must not abort the provider request or persistence.
    return false;
  }
}

/**
 * Keeps an SSE connection from looking dead while a run is silent, for example
 * during a long tool call. A client that treats a quiet socket as broken
 * reconnects and drops what it was showing; a comment frame carries no event, so
 * it only proves the connection is alive. A frame is skipped while earlier
 * output is still queued: a backed-up client needs no extra bytes, and the
 * queue limit is enforced by `writeSse`. Returns a function that stops it.
 */
export function startSseHeartbeat(
  response: SseWritableResponse,
  intervalMs: number,
): () => void {
  if (intervalMs <= 0) {
    return () => undefined;
  }
  const timer = setInterval(() => {
    if (response.writableEnded || response.destroyed) {
      clearInterval(timer);
      return;
    }
    if (response.writableLength > 0) {
      return;
    }
    try {
      response.write(": keep-alive\n\n");
    } catch {
      // A disconnected browser must not affect the run.
    }
  }, intervalMs);
  timer.unref?.();
  return () => clearInterval(timer);
}
