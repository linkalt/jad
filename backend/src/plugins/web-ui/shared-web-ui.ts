import type { JsonRecord } from "../../persistence.js";
import type {
  WebUiMessage,
  WebUiMessageStatus,
  WebUiPart,
  WebUiTextPart,
  WebUiToolCallPart,
  WebUiToolResultPart,
} from "./types.js";

export function isRecord(value: unknown): value is JsonRecord {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

export function textFromParts(value: unknown, types: readonly string[]): string {
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

export function responseFromRecord(value: JsonRecord): JsonRecord {
  return isRecord(value.response) ? value.response : value;
}

export function appendTextPart(
  parts: WebUiPart[],
  type: "reasoning" | "text",
  text: string,
): WebUiPart[] {
  if (text.length === 0 || (type === "reasoning" && text.trim().length === 0)) {
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

export function appendToolPart(
  parts: WebUiPart[],
  part: WebUiToolCallPart | WebUiToolResultPart,
): WebUiPart[] {
  return [...parts, part];
}

export function stringValue(value: unknown, fallback: string): string {
  return typeof value === "string" && value.length > 0 ? value : fallback;
}

export function serializedValue(value: unknown): string {
  if (typeof value === "string") {
    return value;
  }
  try {
    return JSON.stringify(value) ?? "";
  } catch {
    return "";
  }
}

export function fieldsFromParts(
  parts: WebUiPart[],
): Pick<WebUiMessage, "text" | "reasoningContent"> {
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

export function timestampFromRecord(value: JsonRecord): string {
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

export function assistantMessage(
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
