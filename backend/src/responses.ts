import OpenAI from "openai";
import type {
  ResponseCreateParamsStreaming,
  ResponseStreamEvent,
} from "openai/resources/responses/responses.js";

import type {ProviderRetrySettings} from "./config.js";
import {withProviderRetry} from "./provider-retry.js";
import {
  PersistenceError,
  type CommitResult,
  type JsonRecord,
  type ResponseCompletedEvent,
  type ResponseOutputItemDoneEvent,
  type ResponseTurn,
  type SessionResponseStore,
} from "./persistence.js";

export interface PersistResponseStreamOptions {
  turnId?: string;
  /** Session-level user submission that produced this model response. */
  sessionTurn?: number;
  /** Model response round within the current session turn. */
  sessionStep?: number;
  onEvent?: (event: JsonRecord) => void | Promise<void>;
  /** Called when the response-turn staging directory is ready. */
  onTurnStarted?: (turn: ResponseTurn) => void | Promise<void>;
  /** Called only after the output item is durably staged. */
  onOutputItemStaged?: (
    turn: ResponseTurn,
    outputIndex: number,
    item: JsonRecord,
  ) => void | Promise<void>;
  /** Cancels the provider request and streaming response. */
  signal?: AbortSignal;
  /**
   * Retry of the request that opens the stream. When set, the SDK's own retries are
   * switched off for that request so this loop is the only one; when absent the SDK's
   * defaults apply, as before the setting existed.
   */
  retry?: ProviderRetrySettings;
}

export interface PersistedResponse extends CommitResult {
  event: ResponseCompletedEvent;
}

function isRecord(value: unknown): value is JsonRecord {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function asRecord(value: unknown, description: string): JsonRecord {
  if (!isRecord(value)) {
    throw new PersistenceError(`${description} must be a JSON object`);
  }
  return value;
}

function isTerminalFailureType(type: unknown): boolean {
  return type === "response.failed" || type === "error";
}

function completedEventFromTerminal(event: JsonRecord): ResponseCompletedEvent | undefined {
  if (event.type !== "response.completed" && event.type !== "response.incomplete") {
    return undefined;
  }
  return {
    // The transcript currently replays response output items from the durable
    // response.completed envelope. Preserve an incomplete response's status and
    // incomplete_details inside response while using that replay-compatible
    // envelope, so completed function calls are not discarded.
    type: "response.completed",
    response: asRecord(event.response, `${String(event.type)}.response`),
  };
}

/**
 * Persists any Responses-compatible async event stream.
 *
 * This function deliberately accepts `unknown` events so the persistence layer
 * remains usable with the official SDK, a raw SSE parser, or a test stream.
 */
export async function persistResponseStream(
  store: SessionResponseStore,
  stream: AsyncIterable<unknown>,
  options: PersistResponseStreamOptions = {},
): Promise<PersistedResponse> {
  const turn = await store.beginTurn(options.turnId);
  const stagedOutputItems = new Map<number, JsonRecord>();
  let nextOutputIndex = 0;

  try {
    await options.onTurnStarted?.(turn);
    for await (const rawEvent of stream) {
      const event = asRecord(rawEvent, "Responses stream event");
      if (options.onEvent) {
        await options.onEvent(event);
      }
      const type = event.type;

      if (type === "response.output_item.done") {
        const item = asRecord(event.item, "response.output_item.done.item");
        const outputIndex = event.output_index;
        const stagedIndex = typeof outputIndex === "number" ? outputIndex : nextOutputIndex;
        const normalizedEvent: ResponseOutputItemDoneEvent = {
          type: "response.output_item.done",
          item,
          ...(typeof outputIndex === "number" ? { output_index: outputIndex } : {}),
        };
        await turn.stageOutputItem(normalizedEvent);
        stagedOutputItems.set(stagedIndex, item);
        nextOutputIndex = Math.max(nextOutputIndex, stagedIndex + 1);
        await options.onOutputItemStaged?.(turn, stagedIndex, item);
        continue;
      }

      const completedEvent = completedEventFromTerminal(event);
      if (completedEvent) {
        const responseOutput = completedEvent.response.output;
        if (
          stagedOutputItems.size > 0 &&
          (responseOutput === undefined || (Array.isArray(responseOutput) && responseOutput.length === 0))
        ) {
          // Some Responses-compatible streaming providers emit output items as
          // events but leave response.completed.response.output empty. Rebuild
          // the envelope from the staged events so commits, tool execution, and
          // transcript consumers all see the same completed response.
          completedEvent.response.output = [...stagedOutputItems.entries()]
            .sort(([left], [right]) => left - right)
            .map(([, item]) => item);
        }
        if (options.sessionTurn !== undefined) {
          completedEvent.session_turn = options.sessionTurn;
        }
        if (options.sessionStep !== undefined) {
          completedEvent.session_step = options.sessionStep;
        }
        if (type === "response.incomplete") {
          const incompleteDetails = completedEvent.response.incomplete_details;
          const reason = isRecord(incompleteDetails) && typeof incompleteDetails.reason === "string"
            ? incompleteDetails.reason
            : "unspecified";
          console.warn(
            `[responses] provider response incomplete id=${String(completedEvent.response.id ?? "unknown")} reason=${reason}; preserving staged output`,
          );
        }
        const commit = await turn.commit(completedEvent);
        return { ...commit, event: completedEvent };
      }

      if (isTerminalFailureType(type)) {
        const errorDetails = event.response ?? event.error ?? type;
        throw new PersistenceError(
          `Responses stream terminated without response.completed: ${JSON.stringify(errorDetails)}`,
        );
      }
    }

    throw new PersistenceError("Responses stream ended before response.completed");
  } catch (error) {
    // Closing preserves the staging directory for recovery. A remote stream
    // cannot generally resume at an arbitrary output item, so no partial
    // response is committed here.
    await turn.close();
    throw error;
  }
}

/**
 * Official OpenAI Node SDK adapter. The SDK owns HTTP, authentication, and SSE
 * decoding; persistence only receives the normalized event stream.
 */
export async function persistOpenAIResponse(
  client: OpenAI,
  store: SessionResponseStore,
  params: ResponseCreateParamsStreaming,
  options: PersistResponseStreamOptions = {},
): Promise<PersistedResponse> {
  const open = () =>
    client.responses.create(params, {
      ...(options.signal ? {signal: options.signal} : {}),
      ...(options.retry ? {maxRetries: 0} : {}),
    });
  // Only opening the stream is retried; a failure after events arrived is not.
  const stream = options.retry
    ? await withProviderRetry(options.retry, open, options.signal, {
        onRetry: ({attempt, delayMs, status}) => {
          console.warn(
            `[provider-retry] attempt=${attempt}/${options.retry?.max_retries} status=${status ?? "connection"} wait_ms=${delayMs}`,
          );
        },
      })
    : await open();
  return persistResponseStream(store, stream as AsyncIterable<ResponseStreamEvent>, options);
}
