export interface SseWritableResponse {
  readonly writableEnded: boolean;
  readonly destroyed: boolean;
  readonly writableLength: number;
  write(chunk: string): boolean;
  destroy(): void;
}

/**
 * Writes one SSE frame without allowing queued frames to grow with a stalled
 * client. A single frame may exceed the configured limit; it is the smallest
 * unit the current protocol can send. A full `ui.message` snapshot may also
 * exceed the remaining budget once so a reconnect can resynchronize. Further
 * writes are rejected until the client attaches again and receives the latest
 * run snapshot.
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
    const bufferedBytes = response.writableLength;
    if (bufferedBytes > maxBufferedBytes) {
      response.destroy();
      return false;
    }
    const frame = `event: ${event}\ndata: ${JSON.stringify(value)}\n\n`;
    const frameBytes = Buffer.byteLength(frame);
    const wouldExceedLimit = bufferedBytes > 0 && bufferedBytes + frameBytes > maxBufferedBytes;
    const mayResynchronize = event === "ui.message" && bufferedBytes <= maxBufferedBytes;
    if (wouldExceedLimit && !mayResynchronize) {
      response.destroy();
      return false;
    }
    return response.write(frame);
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
