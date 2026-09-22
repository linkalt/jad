import {Ajv, type ValidateFunction} from "ajv";
import {readFile} from "node:fs/promises";
import {isAbsolute, resolve} from "node:path";
import {isDeepStrictEqual} from "node:util";

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
import {nativeToolSpecs} from "./native/registry.js";
import type {NativeToolSpec} from "./native/registry.js";
import type {ProcessManager} from "./native/process-manager.js";
import type {TaskStore} from "./native/task-store.js";
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

const nativeSpecsById = new Map(
  nativeToolSpecs().map((spec) => [spec.id, spec]),
);

function validateNativeDefinition(
  definition: ToolDefinition,
  config: LoadedSessionConfig,
  spec: NativeToolSpec,
): void {
  // Native implementations rely on the full schema, not only its required
  // fields. Definition documents are an enablement manifest; the in-code
  // definition remains the executable contract and provider source of truth.
  if (!isDeepStrictEqual(definition.inputSchema, spec.definition.inputSchema)) {
    throw configError(
      config.paths.setting_path,
      `${definition.id} inputSchema must exactly match the native tool schema`,
    );
  }
}

function validationExample(definition: ToolDefinition): JsonRecord | undefined {
  return nativeSpecsById.get(definition.id)?.exampleOperation;
}

function buildNativeTool(
  spec: NativeToolSpec,
  definition: ToolDefinition,
  config: LoadedSessionConfig,
  validate: ValidateFunction<unknown>,
  resources: {processManager?: ProcessManager; taskStore?: TaskStore},
): LoadedTool {
  const built = spec.build(definition, config, validate, resources);
  if ("error" in built) {
    throw configError(config.paths.setting_path, built.error);
  }
  return {...built.tool, normalizeArguments: spec.normalizeArguments};
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

    const exampleOperation = validationExample(tool.definition);
    const rawArguments = call.arguments;
    if (typeof rawArguments !== "string") {
      return {
        type: "function_call_output",
        call_id: callId,
        output: schemaError(
          callId,
          `Invalid arguments for ${name}`,
          [{keyword: "type", message: "function call arguments must be a JSON string"}],
          exampleOperation,
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
        output: invalidArgumentsJsonError(callId, name, exampleOperation),
      };
    }
    // Normalize harmless variants ("utf-8", "Lines") before validation, so a
    // near-miss spelling does not become an invalid_input retry.
    const parsedObject = asJsonRecord(parsed);
    const normalized = parsedObject && tool.normalizeArguments
      ? tool.normalizeArguments(parsedObject)
      : parsed;
    if (!tool.validate(normalized)) {
      return {
        type: "function_call_output",
        call_id: callId,
        output: schemaError(
          callId,
          `Invalid arguments for ${name}`,
          tool.validate.errors ?? [],
          exampleOperation,
        ),
      };
    }
    const args = asJsonRecord(normalized);
    if (!args) {
      return {
        type: "function_call_output",
        call_id: callId,
        output: schemaError(
          callId,
          `Invalid arguments for ${name}`,
          [{keyword: "type", message: "arguments must be a JSON object"}],
          exampleOperation,
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
  resources: {processManager?: ProcessManager; taskStore?: TaskStore} = {},
): Promise<ToolCatalog> {
  const configuredSpecs = nativeToolSpecs().filter((spec) => spec.configured(config));
  if (configuredSpecs.length === 0) {
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

  const loadedTools: LoadedTool[] = [];
  for (const spec of configuredSpecs) {
    const definition = definitions.get(spec.id);
    if (!definition) {
      throw configError(
        config.paths.setting_path,
        `tools.native.${spec.settingName} is configured but ${spec.id} is missing from tool_definition_paths`,
      );
    }
    if (definition.name !== spec.definition.name || definition.source !== "native") {
      throw configError(
        config.paths.setting_path,
        `${spec.id} must define the native ${spec.definition.name} tool`,
      );
    }
    validateNativeDefinition(definition, config, spec);
    loadedTools.push(
      buildNativeTool(spec, spec.definition, config, ajv.compile(spec.definition.inputSchema), resources),
    );
  }
  return new ToolCatalog(loadedTools);
}

export function responseFunctionCalls(response: JsonRecord): JsonRecord[] {
  if (!Array.isArray(response.output)) {
    return [];
  }
  return response.output.filter(
    (item): item is JsonRecord => isRecord(item) && item.type === "function_call",
  );
}
