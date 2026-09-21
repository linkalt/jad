import type { JsonRecord } from "../../persistence.js";
import { textFromParts } from "./shared-web-ui.js";
import type { WebUiTextPart } from "./types.js";

export function reasoningPartFromOutput(
  value: JsonRecord,
): WebUiTextPart | undefined {
  if (value.type !== "reasoning") {
    return undefined;
  }
  const text = [
    textFromParts(value.content, ["reasoning_text", "output_text", "text"]),
    textFromParts(value.summary, ["summary_text", "reasoning_text", "output_text", "text"]),
  ]
    .filter(Boolean)
    .join("\n\n");
  return text.trim().length > 0 ? { type: "reasoning", text } : undefined;
}

export function reasoningDeltaFromEvent(value: JsonRecord): string | undefined {
  if (
    (value.type === "response.reasoning_text.delta" ||
      value.type === "response.reasoning_summary_text.delta") &&
    typeof value.delta === "string"
  ) {
    return value.delta.trim().length > 0 ? value.delta : undefined;
  }
  return undefined;
}
