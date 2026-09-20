import OpenAI from "openai";
import type {
  ResponseCreateParamsStreaming,
  ResponseStreamEvent,
} from "openai/resources/responses/responses.js";

import {
  PersistenceError,
  type CommitResult,
  type JsonRecord,
  type ResponseCompletedEvent,
  type ResponseOutputItemDoneEvent,
  type SessionResponseStore,
} from "./persistence.js";

export interface PersistResponseStreamOptions {
  turnId?: string;
  onEvent?: (event: JsonRecord) => void | Promise<void>;
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
  return type === "response.failed" || type === "response.incomplete" || type === "error";
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

  try {
    for await (const rawEvent of stream) {
      const event = asRecord(rawEvent, "Responses stream event");
      if (options.onEvent) {
        await options.onEvent(event);
      }
      const type = event.type;

      if (type === "response.output_item.done") {
        const item = asRecord(event.item, "response.output_item.done.item");
        const outputIndex = event.output_index;
        const normalizedEvent: ResponseOutputItemDoneEvent = {
          type: "response.output_item.done",
          item,
          ...(typeof outputIndex === "number" ? { output_index: outputIndex } : {}),
        };
        await turn.stageOutputItem(normalizedEvent);
        continue;
      }

      if (type === "response.completed") {
        const completedEvent: ResponseCompletedEvent = {
          type: "response.completed",
          response: asRecord(event.response, "response.completed.response"),
        };
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
  const stream = await client.responses.create(params);
  return persistResponseStream(store, stream as AsyncIterable<ResponseStreamEvent>, options);
}
