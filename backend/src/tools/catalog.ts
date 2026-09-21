import {Ajv, type ValidateFunction} from "ajv";
import {readFile} from "node:fs/promises";
import {isAbsolute, resolve} from "node:path";

import type {LoadedSessionConfig} from "../config.js";
import {ConfigError} from "../config.js";
import type {JsonRecord, ResponseInputItem} from "../persistence.js";
import {
  asJsonRecord,
  executionError,
  invalidArgumentsJsonError,
  schemaError,
  unknownToolError,
} from "./output.js";
import {executeNativeRead, NATIVE_READ_ID, nativeReadDefinition} from "./native/read.js";
import type {LoadedTool, ToolDefinition, ToolExecutionContext} from "./types.js";

const definitionDocumentSchema: JsonRecord = {
  type: "object",
  required: ["tools"],
  properties: {
    schema_version: {type: "integer", const: 1},
    tools: {
      type: "array",
      minItems: 1,
      items: {
        type: "object",
        required: ["id", "source", "name", "description", "inputSchema"],
        properties: {
          id: {type: "string", minLength: 1},
          source: {type: "string", enum: ["native", "mcp"]},
          server: {type: "string", minLength: 1},
          name: {type: "string", minLength: 1},
          description: {type: "string"},
          inputSchema: {type: "object"},
        },
        additionalProperties: true,
      },
    },
  },
  additionalProperties: true,
};

const ajv = new Ajv({
  allErrors: true,
  strict: true,
  coerceTypes: false,
  removeAdditional: false,
});
const validateDefinitionDocument = ajv.compile(definitionDocumentSchema);

function isRecord(value: unknown): value is JsonRecord {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function configError(path: string, message: string): ConfigError {
  return new ConfigError(`${path}: ${message}`);
}

async function readDefinitionDocument(path: string): Promise<JsonRecord> {
  let value: unknown;
  try {
    value = JSON.parse(await readFile(path, "utf8"));
  } catch (error) {
    throw configError(
      path,
      `could not read or parse tool definitions: ${error instanceof Error ? error.message : String(error)}`,
    );
  }
  if (!validateDefinitionDocument(value)) {
    throw configError(
      path,
      `invalid tool definition document: ${JSON.stringify(validateDefinitionDocument.errors ?? [])}`,
    );
  }
  return value as JsonRecord;
}

function configuredNativeRead(config: LoadedSessionConfig): boolean {
  return config.settings.tools.native.read !== undefined;
}

function validateNativeReadDefinition(
  definition: ToolDefinition,
  config: LoadedSessionConfig,
): void {
  const required = definition.inputSchema.required;
  if (
    !Array.isArray(required) ||
    required.length !== 1 ||
    required[0] !== "path"
  ) {
    throw configError(
      config.paths.setting_path,
      `${NATIVE_READ_ID} must require exactly path`,
    );
  }
}

function buildLoadedTool(
  definition: ToolDefinition,
  config: LoadedSessionConfig,
  validate: ValidateFunction<unknown>,
): LoadedTool {
  if (definition.id !== NATIVE_READ_ID) {
    throw configError(
      config.paths.setting_path,
      `native tool ${definition.id} is not implemented`,
    );
  }
  const policy = config.settings.tools.native.read;
  if (!policy) {
    throw configError(
      config.paths.setting_path,
      `tool ${definition.id} is not exposed in tools.native`,
    );
  }
  return {
    definition,
    validate,
    execute: (args, context) =>
      executeNativeRead(args, {
        ...context,
        policy,
      }),
  };
}

export class ToolCatalog {
  private readonly byName: Map<string, LoadedTool>;

  public constructor(
    private readonly tools: readonly LoadedTool[],
  ) {
    this.byName = new Map();
    for (const tool of tools) {
      if (this.byName.has(tool.definition.name)) {
        throw new ConfigError(`duplicate tool name ${tool.definition.name}`);
      }
      this.byName.set(tool.definition.name, tool);
    }
  }

  public definitions(): ToolDefinition[] {
    return this.tools.map((tool) => tool.definition);
  }

  public responseTools(): JsonRecord[] {
    return this.tools.map((tool) => ({
      type: "function",
      name: tool.definition.name,
      description: tool.definition.description,
      parameters: tool.definition.inputSchema,
      strict: false,
    }));
  }

  public async executeFunctionCall(
    call: JsonRecord,
    context: Omit<ToolExecutionContext, "callId">,
  ): Promise<ResponseInputItem> {
    const callId = typeof call.call_id === "string" && call.call_id.length > 0
      ? call.call_id
      : undefined;
    if (!callId) {
      throw new Error("function call is missing call_id");
    }
    const name = typeof call.name === "string" ? call.name : "";
    const tool = this.byName.get(name);
    if (!tool) {
      return {
        type: "function_call_output",
        call_id: callId,
        output: unknownToolError(callId, name),
      };
    }

    const rawArguments = call.arguments;
    if (typeof rawArguments !== "string") {
      return {
        type: "function_call_output",
        call_id: callId,
        output: schemaError(
          callId,
          `Invalid arguments for ${name}`,
          [{keyword: "type", message: "function call arguments must be a JSON string"}],
        ),
      };
    }

    let parsed: unknown;
    try {
      parsed = JSON.parse(rawArguments);
    } catch {
      return {
        type: "function_call_output",
        call_id: callId,
        output: invalidArgumentsJsonError(callId, name),
      };
    }
    if (!tool.validate(parsed)) {
      return {
        type: "function_call_output",
        call_id: callId,
        output: schemaError(
          callId,
          `Invalid arguments for ${name}`,
          tool.validate.errors ?? [],
        ),
      };
    }
    const args = asJsonRecord(parsed);
    if (!args) {
      return {
        type: "function_call_output",
        call_id: callId,
        output: schemaError(
          callId,
          `Invalid arguments for ${name}`,
          [{keyword: "type", message: "arguments must be a JSON object"}],
        ),
      };
    }

    let output: string;
    try {
      output = await tool.execute(args, { ...context, callId });
    } catch (error) {
      output = executionError(
        callId,
        "error",
        "tool_execution_failed",
        `Tool ${name} failed during execution`,
      );
    }
    return {
      type: "function_call_output",
      call_id: callId,
      output,
    };
  }
}

export async function loadToolCatalog(
  config: LoadedSessionConfig,
): Promise<ToolCatalog> {
  if (!configuredNativeRead(config)) {
    return new ToolCatalog([]);
  }

  const definitions = new Map<string, ToolDefinition>();
  for (const configuredPath of config.settings.tool_definition_paths) {
    const path = isAbsolute(configuredPath)
      ? resolve(configuredPath)
      : resolve(config.paths.global_jad_root, configuredPath);
    const document = await readDefinitionDocument(path);
    const rawTools = document.tools;
    if (!Array.isArray(rawTools)) {
      throw configError(path, "tools must be an array");
    }
    for (const rawTool of rawTools) {
      if (!isRecord(rawTool)) {
        throw configError(path, "each tool definition must be an object");
      }
      const definition = rawTool as unknown as ToolDefinition;
      if (definitions.has(definition.id)) {
        throw configError(path, `duplicate tool definition ${definition.id}`);
      }
      definitions.set(definition.id, definition);
    }
  }

  const definition = definitions.get(NATIVE_READ_ID);
  if (!definition) {
    throw configError(
      config.paths.setting_path,
      `tools.native.read is configured but ${NATIVE_READ_ID} is missing from tool_definition_paths`,
    );
  }
  if (definition.name !== nativeReadDefinition.name || definition.source !== "native") {
    throw configError(
      config.paths.setting_path,
      `${NATIVE_READ_ID} must define the native read tool`,
    );
  }
  validateNativeReadDefinition(definition, config);
  const validate = ajv.compile(definition.inputSchema);
  return new ToolCatalog([buildLoadedTool(definition, config, validate)]);
}

export function responseFunctionCalls(response: JsonRecord): JsonRecord[] {
  if (!Array.isArray(response.output)) {
    return [];
  }
  return response.output.filter(
    (item): item is JsonRecord => isRecord(item) && item.type === "function_call",
  );
}
