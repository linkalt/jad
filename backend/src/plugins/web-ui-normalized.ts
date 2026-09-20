import type { LoadedSessionConfig } from "../config.js";
import type { JsonRecord } from "../persistence.js";

export type WebUiPartType = "reasoning" | "text";
export type WebUiMessageStatus = "streaming" | "complete";

export interface WebUiPart {
  type: WebUiPartType;
  text: string;
}

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

function appendPart(
  parts: WebUiPart[],
  type: WebUiPartType,
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

function fieldsFromParts(parts: WebUiPart[]): Pick<WebUiMessage, "text" | "reasoningContent"> {
  const text = parts
    .filter((part) => part.type === "text")
    .map((part) => part.text)
    .join("");
  const reasoningContent = parts
    .filter((part) => part.type === "reasoning")
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
      parts = appendPart(parts, "reasoning", reasoning);
      continue;
    }
    if (output.type === "message" && output.role === "assistant") {
      parts = appendPart(parts, "text", textFromParts(output.content, ["output_text"]));
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
    if (!parts.some((part) => part.type === completedPart.type)) {
      parts = appendPart(parts, completedPart.type, completedPart.text);
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
  records.forEach((record, index) => {
    const user = userMessage(record, index);
    if (user) {
      messages.push(user);
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
      this.parts = appendPart(this.parts, "text", event.delta);
      return this.streamingMessage();
    }
    if (
      (event.type === "response.reasoning_text.delta" ||
        event.type === "response.reasoning_summary_text.delta") &&
      typeof event.delta === "string"
    ) {
      this.parts = appendPart(this.parts, "reasoning", event.delta);
      return this.streamingMessage();
    }
    return undefined;
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
