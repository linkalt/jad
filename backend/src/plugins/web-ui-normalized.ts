import type { LoadedSessionConfig } from "../config.js";
import type { JsonRecord } from "../persistence.js";

export type WebUiPartType = "reasoning" | "text" | "tool_call" | "tool_result";
export type WebUiMessageStatus = "streaming" | "complete";
export type WebUiToolResultStatus = "success" | "error" | "unknown";

export interface WebUiTextPart {
  type: "reasoning" | "text";
  text: string;
}

export interface WebUiToolCallPart {
  type: "tool_call";
  callId: string;
  name: string;
  arguments: string;
}

export interface WebUiToolResultPart {
  type: "tool_result";
  callId: string;
  output: string;
  status: WebUiToolResultStatus;
}

export type WebUiPart = WebUiTextPart | WebUiToolCallPart | WebUiToolResultPart;

export interface WebUiMessage {
  id: string;
  sender: "system" | "user" | "assistant";
  text: string;
  timestamp: string;
  parts: WebUiPart[];
  status?: WebUiMessageStatus;
  reasoningContent?: string;
}

export interface WebUiSessionSnapshot {
  id: string;
  session_name: string;
  turns: number;
  messages: WebUiMessage[];
  model: {
    provider: string;
    model: string;
  };
}

function isRecord(value: unknown): value is JsonRecord {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function textFromParts(value: unknown, types: readonly string[]): string {
  if (!Array.isArray(value)) {
    return "";
  }
  return value
    .filter(
      (part): part is JsonRecord =>
        isRecord(part) && typeof part.type === "string" && types.includes(part.type),
    )
    .map((part) => (typeof part.text === "string" ? part.text : ""))
    .join("");
}

function responseFromRecord(value: JsonRecord): JsonRecord {
  return isRecord(value.response) ? value.response : value;
}

function appendTextPart(
  parts: WebUiPart[],
  type: "reasoning" | "text",
  text: string,
): WebUiPart[] {
  if (text.length === 0) {
    return parts;
  }
  const previous = parts[parts.length - 1];
  if (previous?.type === type) {
    return [
      ...parts.slice(0, -1),
      { type: previous.type, text: previous.text + text },
    ];
  }
  return [...parts, { type, text }];
}

function appendToolPart(parts: WebUiPart[], part: WebUiToolCallPart | WebUiToolResultPart): WebUiPart[] {
  return [...parts, part];
}

function stringValue(value: unknown, fallback: string): string {
  return typeof value === "string" && value.length > 0 ? value : fallback;
}

function serializedValue(value: unknown): string {
  if (typeof value === "string") {
    return value;
  }
  try {
    return JSON.stringify(value) ?? "";
  } catch {
    return "";
  }
}

function toolResultStatus(output: string): WebUiToolResultStatus {
  const firstLine = output.split("\n", 1)[0] ?? "";
  try {
    const metadata = JSON.parse(firstLine) as unknown;
    if (!isRecord(metadata) || typeof metadata.status !== "string") {
      return "unknown";
    }
    return metadata.status === "ok" ? "success" : "error";
  } catch {
    return "unknown";
  }
}

function toolCallPart(value: JsonRecord, index: number): WebUiToolCallPart | undefined {
  if (value.type !== "function_call") {
    return undefined;
  }
  return {
    type: "tool_call",
    callId: stringValue(value.call_id, stringValue(value.id, `tool-call-${index}`)),
    name: stringValue(value.name, "unknown_tool"),
    arguments: serializedValue(value.arguments),
  };
}

function toolResultPart(value: JsonRecord, index: number): WebUiToolResultPart | undefined {
  if (value.type !== "function_call_output") {
    return undefined;
  }
  const output = serializedValue(value.output);
  return {
    type: "tool_result",
    callId: stringValue(value.call_id, `tool-call-${index}`),
    output,
    status: toolResultStatus(output),
  };
}

function fieldsFromParts(parts: WebUiPart[]): Pick<WebUiMessage, "text" | "reasoningContent"> {
  const text = parts
    .filter((part): part is WebUiTextPart => part.type === "text")
    .map((part) => part.text)
    .join("");
  const reasoningContent = parts
    .filter((part): part is WebUiTextPart => part.type === "reasoning")
    .map((part) => part.text)
    .join("\n\n")
    .trim();
  return {
    text,
    ...(reasoningContent ? { reasoningContent } : {}),
  };
}

function timestampFromRecord(value: JsonRecord): string {
  const createdAt = value.created_at;
  if (typeof createdAt === "number" || typeof createdAt === "string") {
    const date = new Date(
      typeof createdAt === "number" ? createdAt * 1000 : createdAt,
    );
    if (!Number.isNaN(date.valueOf())) {
      return date.toLocaleTimeString([], { hour: "numeric", minute: "2-digit" });
    }
  }
  return "";
}

function assistantMessage(
  id: string,
  parts: WebUiPart[],
  timestamp: string,
  status: WebUiMessageStatus,
): WebUiMessage {
  return {
    id,
    sender: "assistant",
    timestamp,
    parts,
    status,
    ...fieldsFromParts(parts),
  };
}

function responseParts(value: JsonRecord): WebUiPart[] {
  const response = responseFromRecord(value);
  if (!Array.isArray(response.output)) {
    return [];
  }

  let parts: WebUiPart[] = [];
  for (const output of response.output) {
    if (!isRecord(output)) {
      continue;
    }
    if (output.type === "reasoning") {
      const reasoning = [
        textFromParts(output.content, ["reasoning_text", "output_text", "text"]),
        textFromParts(output.summary, ["summary_text", "reasoning_text", "output_text", "text"]),
      ]
        .filter(Boolean)
        .join("\n\n");
      parts = appendTextPart(parts, "reasoning", reasoning);
      continue;
    }
    if (output.type === "message" && output.role === "assistant") {
      parts = appendTextPart(parts, "text", textFromParts(output.content, ["output_text"]));
      continue;
    }
    const call = toolCallPart(output, parts.length);
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
  config: LoadedSessionConfig,
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

    const result = toolResultPart(record, index);
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
          assistantMessage(`session-tool-${index}`, [result], timestampFromRecord(record), "complete"),
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
      const activeMessage = messageIndex === undefined ? undefined : messages[messageIndex];
      if (messageIndex === undefined || !activeMessage) {
        messageIndex = messages.length;
        messages.push(
          assistantMessage(
            typeof response.id === "string" ? response.id : `session-assistant-${index}`,
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
        typeof response.id === "string" ? response.id : `session-assistant-${index}`,
        parts,
        timestampFromRecord(response),
        "complete",
      ),
    );
  });

  return {
    id,
    session_name: config.settings.session_name,
    model: {
      provider: config.model.provider,
      model: config.model.model,
    },
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
    if (
      event.type === "response.output_text.delta" &&
      typeof event.delta === "string"
    ) {
      this.parts = appendTextPart(this.parts, "text", event.delta);
      return this.streamingMessage();
    }
    if (
      (event.type === "response.reasoning_text.delta" ||
        event.type === "response.reasoning_summary_text.delta") &&
      typeof event.delta === "string"
    ) {
      this.parts = appendTextPart(this.parts, "reasoning", event.delta);
      return this.streamingMessage();
    }
    if (event.type === "response.completed") {
      const responsePartsValue = responseParts(event);
      const toolParts = responsePartsValue.filter(
        (part): part is WebUiToolCallPart => part.type === "tool_call",
      );
      if (toolParts.length > 0) {
        for (const part of toolParts) {
          this.parts = appendToolPart(this.parts, part);
        }
        return this.streamingMessage();
      }
    }
    return undefined;
  }

  public toolResult(value: JsonRecord): WebUiMessage | undefined {
    const result = toolResultPart(value, this.parts.length);
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

  private streamingMessage(): WebUiMessage {
    return assistantMessage(this.messageId, this.parts, this.timestamp, "streaming");
  }
}
