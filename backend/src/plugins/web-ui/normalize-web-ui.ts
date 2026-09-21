import type { JsonRecord } from "../../persistence.js";
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
import {
  toolCallPartFromAnyOutput,
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

  return {
    id,
    ...sessionSettingsFromConfig(config),
    turns: records.filter((record) => record.type === "response.completed").length,
    messages,
  };
}

export class WebUiNormalizedRun {
  private parts: WebUiPart[] = [];
  private readonly timestamp: string;

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

    if (event.type === "response.output_item.done" && isRecord(event.item)) {
      const call = toolCallPartFromAnyOutput(event.item, this.parts.length);
      if (call && !this.hasToolCall(call.callId)) {
        this.parts = appendToolPart(this.parts, call);
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
    this.parts = mergeCompletedParts(this.parts, completedParts);
    return assistantMessage(this.messageId, this.parts, this.timestamp, "complete");
  }

  private hasToolCall(callId: string): boolean {
    return this.parts.some(
      (part) => part.type === "tool_call" && part.callId === callId,
    );
  }

  private streamingMessage(): WebUiMessage {
    return assistantMessage(this.messageId, this.parts, this.timestamp, "streaming");
  }
}
