/**
 * Runs the tool calls of one provider response with bounded concurrency.
 *
 * A model that returns several `function_call` items in one response expects
 * them to run in parallel. Each finished call is reported as soon as it settles,
 * so a client sees every result when it exists instead of waiting for the
 * slowest call of the batch.
 *
 * Failure policy matches sequential execution: after the first thrown error (or
 * once `shouldStop` reports true) no further call is started, calls already in
 * flight are allowed to settle, and the error is returned for the caller to
 * rethrow after it has persisted every result that did complete.
 */
export interface ToolBatchOutcome {
  /** True when a call threw; `error` holds the first thrown value. */
  failed: boolean;
  error?: unknown;
}

/**
 * How many calls of one response may run at once. With `ordered` scheduling a
 * response holding a call that can change state runs one call at a time in the
 * model's order, so a later call sees what an earlier one did; a response of
 * read-only calls still runs together, up to `limit`.
 */
export function toolBatchConcurrency<Call>(
  calls: readonly Call[],
  limit: number,
  scheduling: "ordered" | "concurrent",
  changesState: (call: Call) => boolean,
): number {
  return scheduling === "ordered" && calls.some(changesState) ? 1 : limit;
}

export async function runToolBatch<Call, Output>(
  calls: readonly Call[],
  limit: number,
  execute: (call: Call) => Promise<Output>,
  onFinished: (call: Call, output: Output) => void | Promise<void>,
  shouldStop: () => boolean = () => false,
): Promise<ToolBatchOutcome> {
  if (!Number.isSafeInteger(limit) || limit < 1) {
    throw new RangeError("tool call concurrency must be a positive safe integer");
  }
  let next = 0;
  const outcome: ToolBatchOutcome = {failed: false};

  const worker = async (): Promise<void> => {
    while (!outcome.failed && !shouldStop() && next < calls.length) {
      const call = calls[next]!;
      next += 1;
      try {
        await onFinished(call, await execute(call));
      } catch (error) {
        if (!outcome.failed) {
          outcome.failed = true;
          outcome.error = error;
        }
      }
    }
  };

  await Promise.all(Array.from({length: Math.min(limit, calls.length)}, worker));
  return outcome;
}
