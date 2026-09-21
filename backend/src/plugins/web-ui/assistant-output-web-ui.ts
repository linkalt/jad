import type { JsonRecord } from "../../persistence.js";
import { textFromParts } from "./shared-web-ui.js";
import type { WebUiTextPart } from "./types.js";

export function assistantTextPartFromOutput(
  value: JsonRecord,
): WebUiTextPart | undefined {
  if (value.type !== "message" || value.role !== "assistant") {
    return undefined;
  }
  const text = textFromParts(value.content, ["output_text"]);
  return text.length > 0 ? { type: "text", text } : undefined;
}

export function assistantTextDeltaFromEvent(value: JsonRecord): string | undefined {
  if (
    value.type === "response.output_text.delta" &&
    typeof value.delta === "string"
  ) {
    return value.delta;
  }
  return undefined;
}
