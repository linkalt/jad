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

const MCP_TOOL_ERROR_TEXT = "MCP server reported a tool error.";

/**
 * True only when that sentence *is* an output part, which is how the MCP client
 * reports a tool-level error. Scanning the whole payload instead would flag any
 * result that merely quotes the sentence — reading this file, for instance.
 */
function isMcpToolError(value: unknown): boolean {
  const parts = Array.isArray(value) ? value : [value];
  return parts.some((part) => {
    if (typeof part === "string") {
      return part.trim() === MCP_TOOL_ERROR_TEXT;
    }
    return isRecord(part) && part.text === MCP_TOOL_ERROR_TEXT;
  });
}

function toolResultStatus(output: string, mcpToolError: boolean): WebUiToolResultStatus {
  if (mcpToolError) return "error";
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

/**
 * `response.output_item.added` announces a function call before its arguments
 * exist. The part is created right away and marked partial so the UI can show
 * the call while a long argument (a `write` body, for instance) streams.
 */
export function toolCallPartFromAddedItem(
  value: JsonRecord,
  index: number,
): WebUiToolCallPart | undefined {
  const call = toolCallPartFromAnyOutput(value, index);
  return call === undefined ? undefined : {...call, partial: true};
}

/**
 * Argument deltas. Providers stream `response.function_call_arguments.delta`
 * for every function call, including MCP tools; custom tools use a separate
 * event type the harness does not configure.
 */
export function toolCallArgumentsDeltaFromEvent(value: JsonRecord): string | undefined {
  if (
    value.type === "response.function_call_arguments.delta" &&
    typeof value.delta === "string"
  ) {
    return value.delta;
  }
  return undefined;
}

/** The final argument text of a streamed call. */
export function toolCallArgumentsFromEvent(value: JsonRecord): string | undefined {
  if (
    value.type === "response.function_call_arguments.done" &&
    typeof value.arguments === "string"
  ) {
    return value.arguments;
  }
  return undefined;
}

/** Stream metadata that ties an event to the item it belongs to. */
export function toolCallStreamTarget(value: JsonRecord): {
  itemId?: string;
  outputIndex?: number;
} {
  return {
    ...(typeof value.item_id === "string" && value.item_id.length > 0
      ? {itemId: value.item_id}
      : {}),
    ...(typeof value.output_index === "number" ? {outputIndex: value.output_index} : {}),
  };
}

function toolOutputForUi(value: unknown): string {
  if (!Array.isArray(value)) return serializedValue(value);
  return value.map((part) => {
    if (!isRecord(part)) return "";
    if (part.type === "input_text" && typeof part.text === "string") return part.text;
    if (part.type === "input_image") return "[MCP image content]";
    if (part.type === "input_file") return `[MCP file${typeof part.filename === "string" ? `: ${part.filename}` : ""}]`;
    return `[MCP content: ${stringValue(part.type, "unknown")}]`;
  }).filter(Boolean).join("\n");
}

export function toolResultPartFromOutput(
  value: JsonRecord,
  index: number,
): WebUiToolResultPart | undefined {
  if (value.type !== "function_call_output") {
    return undefined;
  }
  const output = toolOutputForUi(value.output);
  const status = toolResultStatus(output, isMcpToolError(value.output));
  return {
    type: "tool_result",
    callId: stringValue(value.call_id, `tool-call-${index}`),
    output,
    status: Array.isArray(value.output)
      ? status === "error" ? "error" : "success"
      : status,
  };
}
