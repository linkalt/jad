import { randomBytes } from "node:crypto";

import type { JsonRecord } from "../persistence.js";

export type ToolStatus =
  | "ok"
  | "not_found"
  | "forbidden"
  | "invalid_input"
  | "is_directory"
  | "unsupported_encoding"
  | "blocked"
  | "error";

export interface ToolContent {
  text: string;
  encoding: "utf8" | "base64";
}

function valueText(value: unknown): string {
  if (typeof value === "string") {
    return value.includes("\n") ? JSON.stringify(value) : value;
  }
  if (value === null) {
    return "null";
  }
  if (typeof value === "number" || typeof value === "boolean") {
    return String(value);
  }
  return JSON.stringify(value);
}

function contentToken(text: string): string {
  for (;;) {
    const token = `r_${randomBytes(16).toString("base64url")}`;
    if (!text.includes(`--- END CONTENT token=${token} ---`)) {
      return token;
    }
  }
}

export function renderToolResult(options: {
  id: string;
  status: ToolStatus;
  summary: string;
  information?: Array<[string, unknown]>;
  content?: ToolContent;
}): string {
  const lines = [
    JSON.stringify({
      id: options.id,
      status: options.status,
      summary: options.summary,
    }),
    "",
    "--- INFORMATION ---",
  ];

  for (const [key, value] of options.information ?? []) {
    lines.push(`${key}: ${valueText(value)}`);
  }

  if (options.content) {
    const token = contentToken(options.content.text);
    const encoding = options.content.encoding === "base64" ? "base64" : "utf8";
    lines.push(
      "",
      `--- CONTENT token=${token} encoding=${encoding} ---`,
      options.content.text,
      `--- END CONTENT token=${token} ---`,
    );
  }

  return `${lines.join("\n")}\n`;
}

export function executionError(
  id: string,
  status: Exclude<ToolStatus, "ok" | "invalid_input" | "is_directory">,
  errorCode: string,
  summary: string,
  information: Array<[string, unknown]> = [],
): string {
  return renderToolResult({
    id,
    status,
    summary,
    information: [
      ["error_type", "execution"],
      ["error_code", errorCode],
      ...information,
    ],
  });
}

export function schemaError(
  id: string,
  summary: string,
  errors: unknown,
  exampleOperation?: JsonRecord,
): string {
  return renderToolResult({
    id,
    status: "invalid_input",
    summary,
    information: [
      ["error_type", "schema"],
      ["error_code", "validation_failed"],
      ...(exampleOperation ? [["example_operation", exampleOperation] as [string, unknown]] : []),
      ["ajv_errors", errors],
    ],
  });
}

export function unknownToolError(id: string, name: string): string {
  return executionError(
    id,
    "error",
    "unknown_tool",
    `Tool ${name} is not available`,
    [["tool", name]],
  );
}

export function invalidArgumentsJsonError(
  id: string,
  name: string,
  exampleOperation?: JsonRecord,
): string {
  return schemaError(
    id,
    `Invalid JSON arguments for ${name}`,
    [{keyword: "parse", message: "tool arguments must be valid JSON"}],
    exampleOperation,
  );
}

export function asJsonRecord(value: unknown): JsonRecord | undefined {
  return typeof value === "object" && value !== null && !Array.isArray(value)
    ? value as JsonRecord
    : undefined;
}
