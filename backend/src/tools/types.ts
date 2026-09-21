import type { ValidateFunction } from "ajv";

import type { JsonRecord } from "../persistence.js";

export type ToolSource = "native" | "mcp";

export interface ToolDefinition {
  id: string;
  source: ToolSource;
  server?: string;
  name: string;
  description: string;
  inputSchema: JsonRecord;
}

export interface ToolExecutionContext {
  callId: string;
  workspaceRoot: string;
  sessionDir: string;
}

export interface LoadedTool {
  definition: ToolDefinition;
  validate: ValidateFunction<unknown>;
  execute(
    args: JsonRecord,
    context: ToolExecutionContext,
  ): Promise<string>;
}
