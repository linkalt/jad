import type { JsonRecord } from "../../persistence.js";
import {
  isRecord,
  serializedValue,
  stringValue,
} from "./shared-web-ui.js";
import type {
  WebUiToolCallPart,
  WebUiToolResultPart,
  WebUiToolResultStatus,
} from "./types.js";

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

function toolCallPartFromOutput(
  value: JsonRecord,
  index: number,
): WebUiToolCallPart | undefined {
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

/** Native read output adapter. Additional tool-specific adapters can be added beside this file. */
export function readToolCallPart(
  value: JsonRecord,
  index: number,
): WebUiToolCallPart | undefined {
  if (value.type !== "function_call" || value.name !== "read") {
    return undefined;
  }
  return toolCallPartFromOutput(value, index);
}

/** Current fallback for function calls without a dedicated tool adapter. */
export function genericToolCallPart(
  value: JsonRecord,
  index: number,
): WebUiToolCallPart | undefined {
  return toolCallPartFromOutput(value, index);
}

export function toolCallPartFromAnyOutput(
  value: JsonRecord,
  index: number,
): WebUiToolCallPart | undefined {
  return readToolCallPart(value, index) ?? genericToolCallPart(value, index);
}

export function toolResultPartFromOutput(
  value: JsonRecord,
  index: number,
): WebUiToolResultPart | undefined {
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
