import type {JsonRecord, SessionUsageSummary} from "../../persistence.js";
import {
  assistantTextDeltaFromEvent,
  assistantTextPartFromOutput,
} from "./assistant-output-web-ui.js";
import {
  appendTextPart,
  appendToolPart,
  assistantMessage,
  fieldsFromParts,
  isRecord,
  responseFromRecord,
  textFromParts,
  timestampFromRecord,
} from "./shared-web-ui.js";
import {
  reasoningDeltaFromEvent,
  reasoningPartFromOutput,
} from "./reasoning-web-ui.js";
import { sessionSettingsFromConfig } from "./setting-web-ui.js";
import {sessionUsageFromSummary} from "./usage-web-ui.js";
import {
  toolCallArgumentsDeltaFromEvent,
  toolCallArgumentsFromEvent,
  toolCallPartFromAddedItem,
  toolCallPartFromAnyOutput,
  toolCallStreamTarget,
  toolResultPartFromOutput,
} from "./tool-read-web-ui.js";
import type {
  WebUiMessage,
  WebUiPart,
  WebUiSessionConfig,
  WebUiSessionSnapshot,
  WebUiToolCallPart,
} from "./types.js";

export function responseParts(value: JsonRecord): WebUiPart[] {
  const response = responseFromRecord(value);
  if (!Array.isArray(response.output)) {
    return [];
  }

  let parts: WebUiPart[] = [];
  for (const output of response.output) {
    if (!isRecord(output)) {
      continue;
    }

    const reasoning = reasoningPartFromOutput(output);
    if (reasoning) {
      parts = appendTextPart(parts, "reasoning", reasoning.text);
      continue;
    }

    const text = assistantTextPartFromOutput(output);
    if (text) {
      parts = appendTextPart(parts, "text", text.text);
      continue;
    }

    const call = toolCallPartFromAnyOutput(output, parts.length);
    if (call) {
      parts = appendToolPart(parts, call);
    }
  }
  return parts;
}

function mergeCompletedParts(
  streamedParts: WebUiPart[],
  completedParts: WebUiPart[],
): WebUiPart[] {
  if (streamedParts.length === 0) {
    return completedParts;
  }
  let parts = streamedParts;
  for (const completedPart of completedParts) {
    if (completedPart.type === "tool_call" || completedPart.type === "tool_result") {
      const hasPart = parts.some(
        (part) =>
          part.type === completedPart.type && part.callId === completedPart.callId,
      );
      if (!hasPart) {
        parts = appendToolPart(parts, completedPart);
        continue;
      }
      if (completedPart.type === "tool_call") {
        // The provider's finished item is authoritative for the arguments, and
        // it also drops the in-progress marker of a streamed call.
        parts = parts.map((part): WebUiPart =>
          part.type === "tool_call" && part.callId === completedPart.callId
            ? {...completedPart}
            : part,
        );
      }
      continue;
    }
    if (!parts.some((part) => part.type === completedPart.type)) {
      parts = appendTextPart(parts, completedPart.type, completedPart.text);
    }
  }
  return parts;
}

function userMessage(value: JsonRecord, index: number): WebUiMessage | undefined {
  if (value.type !== "message" || value.role !== "user") {
    return undefined;
  }
  const text = textFromParts(value.content, ["input_text"]);
  if (text.length === 0) {
    return undefined;
  }
  return {
    id: `session-user-${index}`,
    sender: "user",
    text,
    timestamp: timestampFromRecord(value),
    parts: [{ type: "text", text }],
  };
}

export function normalizeWebUiSession(
  id: string,
  config: WebUiSessionConfig,
  records: readonly JsonRecord[],
  usageSummary?: SessionUsageSummary,
): WebUiSessionSnapshot {
  const messages: WebUiMessage[] = [];
  const toolMessages = new Map<string, number>();
  let activeToolMessageIndex: number | undefined;

  records.forEach((record, index) => {
    const user = userMessage(record, index);
    if (user) {
      toolMessages.clear();
      activeToolMessageIndex = undefined;
      messages.push(user);
      return;
    }

    const result = toolResultPartFromOutput(record, index);
    if (result) {
      const messageIndex = toolMessages.get(result.callId);
      if (messageIndex !== undefined) {
        const message = messages[messageIndex];
        if (message) {
          const parts = [...message.parts, result];
          messages[messageIndex] = {
            ...message,
            parts,
            ...fieldsFromParts(parts),
          };
        }
      } else {
        messages.push(
          assistantMessage(
            `session-tool-${index}`,
            [result],
            timestampFromRecord(record),
            "complete",
          ),
        );
      }
      return;
    }

    if (record.type !== "response.completed") {
      return;
    }
    const response = responseFromRecord(record);
    const parts = responseParts(record);
    if (parts.length === 0) {
      return;
    }

    const callParts = parts.filter(
      (part): part is WebUiToolCallPart => part.type === "tool_call",
    );
    if (callParts.length > 0) {
      let messageIndex = activeToolMessageIndex;
      const activeMessage =
        messageIndex === undefined ? undefined : messages[messageIndex];
      if (messageIndex === undefined || !activeMessage) {
        messageIndex = messages.length;
        messages.push(
          assistantMessage(
            typeof response.id === "string"
              ? response.id
              : `session-assistant-${index}`,
            parts,
            timestampFromRecord(response),
            "complete",
          ),
        );
      } else {
        const mergedParts = [...activeMessage.parts, ...parts];
        messages[messageIndex] = {
          ...activeMessage,
          parts: mergedParts,
          ...fieldsFromParts(mergedParts),
        };
      }
      activeToolMessageIndex = messageIndex;
      for (const part of callParts) {
        toolMessages.set(part.callId, messageIndex);
      }
      return;
    }

    if (activeToolMessageIndex !== undefined) {
      const activeMessage = messages[activeToolMessageIndex];
      if (activeMessage?.parts.some((part) => part.type === "tool_call")) {
        const mergedParts = [...activeMessage.parts, ...parts];
        messages[activeToolMessageIndex] = {
          ...activeMessage,
          parts: mergedParts,
          ...fieldsFromParts(mergedParts),
        };
        toolMessages.clear();
        activeToolMessageIndex = undefined;
        return;
      }
    }

    messages.push(
      assistantMessage(
        typeof response.id === "string"
          ? response.id
          : `session-assistant-${index}`,
        parts,
        timestampFromRecord(response),
        "complete",
      ),
    );
  });

  const usage = sessionUsageFromSummary(usageSummary);
  return {
    id,
    ...sessionSettingsFromConfig(config),
    turns: records.filter((record) => record.type === "response.completed").length,
    messages,
    ...(usage === undefined ? {} : { usage }),
  };
}

export class WebUiNormalizedRun {
  private parts: WebUiPart[] = [];
  private readonly timestamp: string;
  // Stream events identify an item by `item_id` (and `output_index`), while a
  // tool part is keyed by its call id, so the two have to be correlated.
  private readonly callIdsByItemId = new Map<string, string>();
  private readonly callIdsByOutputIndex = new Map<number, string>();

  public constructor(private readonly messageId: string) {
    this.timestamp = new Date().toLocaleTimeString([], {
      hour: "numeric",
      minute: "2-digit",
    });
  }

  public update(event: JsonRecord): WebUiMessage | undefined {
    const textDelta = assistantTextDeltaFromEvent(event);
    if (textDelta !== undefined) {
      this.parts = appendTextPart(this.parts, "text", textDelta);
      return this.streamingMessage();
    }

    const reasoningDelta = reasoningDeltaFromEvent(event);
    if (reasoningDelta !== undefined) {
      this.parts = appendTextPart(this.parts, "reasoning", reasoningDelta);
      return this.streamingMessage();
    }

    if (event.type === "response.output_item.added" && isRecord(event.item)) {
      const call = toolCallPartFromAddedItem(event.item, this.parts.length);
      if (call !== undefined) {
        this.rememberToolCall(call.callId, event, event.item);
        if (!this.hasToolCall(call.callId)) {
          this.parts = appendToolPart(this.parts, call);
          return this.streamingMessage();
        }
      }
      return undefined;
    }

    const argumentsDelta = toolCallArgumentsDeltaFromEvent(event);
    if (argumentsDelta !== undefined) {
      const callId = this.toolCallIdForEvent(event);
      if (callId !== undefined && this.hasToolCall(callId)) {
        this.updateToolArguments(callId, (current) => `${current}${argumentsDelta}`, true);
        return this.streamingMessage();
      }
      return undefined;
    }

    const argumentsDone = toolCallArgumentsFromEvent(event);
    if (argumentsDone !== undefined) {
      const callId = this.toolCallIdForEvent(event);
      if (callId !== undefined && this.hasToolCall(callId)) {
        this.updateToolArguments(callId, () => argumentsDone, false);
        return this.streamingMessage();
      }
      return undefined;
    }

    if (event.type === "response.output_item.done" && isRecord(event.item)) {
      const call = toolCallPartFromAnyOutput(event.item, this.parts.length);
      if (call !== undefined) {
        this.rememberToolCall(call.callId, event, event.item);
        if (this.hasToolCall(call.callId)) {
          // The finished item is authoritative: it replaces whatever arrived as
          // deltas and clears the partial marker.
          this.updateToolArguments(call.callId, () => call.arguments, false, call.name);
        } else {
          this.parts = appendToolPart(this.parts, call);
        }
        return this.streamingMessage();
      }
    }

    if (event.type === "response.completed") {
      const completedParts = responseParts(event);
      const toolParts = completedParts.filter(
        (part): part is WebUiToolCallPart => part.type === "tool_call",
      );
      let appended = false;
      for (const part of toolParts) {
        if (this.hasToolCall(part.callId)) {
          continue;
        }
        this.parts = appendToolPart(this.parts, part);
        appended = true;
      }
      if (appended) {
        return this.streamingMessage();
      }
    }
    return undefined;
  }

  public toolResult(value: JsonRecord): WebUiMessage | undefined {
    const result = toolResultPartFromOutput(value, this.parts.length);
    if (!result) {
      return undefined;
    }
    this.parts = appendToolPart(this.parts, result);
    return this.streamingMessage();
  }

  public complete(response: JsonRecord): WebUiMessage {
    const completedParts = responseParts(response);
    this.parts = clearPartialToolCalls(mergeCompletedParts(this.parts, completedParts));
    return assistantMessage(this.messageId, this.parts, this.timestamp, "complete");
  }

  private hasToolCall(callId: string): boolean {
    return this.parts.some(
      (part) => part.type === "tool_call" && part.callId === callId,
    );
  }

  private rememberToolCall(callId: string, event: JsonRecord, item: JsonRecord): void {
    const itemId = typeof item.id === "string" && item.id.length > 0 ? item.id : undefined;
    if (itemId !== undefined) {
      this.callIdsByItemId.set(itemId, callId);
    }
    const {outputIndex} = toolCallStreamTarget(event);
    if (outputIndex !== undefined) {
      this.callIdsByOutputIndex.set(outputIndex, callId);
    }
  }

  private toolCallIdForEvent(event: JsonRecord): string | undefined {
    const {itemId, outputIndex} = toolCallStreamTarget(event);
    if (itemId !== undefined) {
      const known = this.callIdsByItemId.get(itemId);
      if (known !== undefined) {
        return known;
      }
    }
    if (outputIndex !== undefined) {
      return this.callIdsByOutputIndex.get(outputIndex);
    }
    return undefined;
  }

  private updateToolArguments(
    callId: string,
    argumentsText: (current: string) => string,
    partial: boolean,
    name?: string,
  ): void {
    this.parts = this.parts.map((part): WebUiPart => {
      if (part.type !== "tool_call" || part.callId !== callId) {
        return part;
      }
      const {partial: _streamed, ...rest} = part;
      const next: WebUiToolCallPart = {
        ...rest,
        arguments: argumentsText(part.arguments),
        ...(name === undefined ? {} : {name}),
      };
      return partial ? {...next, partial: true} : next;
    });
  }

  private streamingMessage(): WebUiMessage {
    return assistantMessage(this.messageId, this.parts, this.timestamp, "streaming");
  }
}

/** A finished message never carries the in-progress marker. */
function clearPartialToolCalls(parts: WebUiPart[]): WebUiPart[] {
  return parts.map((part): WebUiPart => {
    if (part.type !== "tool_call" || part.partial !== true) {
      return part;
    }
    const {partial: _streamed, ...rest} = part;
    return rest;
  });
}
