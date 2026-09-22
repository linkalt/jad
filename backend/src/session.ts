import OpenAI from "openai";
import {stat} from "node:fs/promises";
import {join} from "node:path";
import type { ResponseCreateParamsStreaming } from "openai/resources/responses/responses.js";

import {
  DEFAULT_NATIVE_PROCESS_STORAGE_DIR,
  DEFAULT_NATIVE_TASK_STORAGE_PATH,
  MAX_CONFIGURED_TOOL_ROUNDS,
  SessionConfigLoader,
  resolveApiKey,
  type LoadedSessionConfig,
} from "./config.js";
import { AsyncMutex } from "./concurrency.js";
import {
  SessionResponseStore,
  type JsonRecord,
  type ResponseInputItem,
  type SessionResponseStoreOptions,
} from "./persistence.js";
import {
  persistOpenAIResponse,
  type PersistedResponse,
} from "./responses.js";
import { createHttpDebugFetch } from "./http-debug.js";
import {
  loadToolCatalog,
  responseFunctionCalls,
  type ToolDefinition,
} from "./tools/index.js";
import {renderToolResult} from "./tools/output.js";
import {ProcessManager} from "./tools/native/process-manager.js";
import {TaskStore} from "./tools/native/task-store.js";

export type SessionUserInput = string | ResponseInputItem | readonly ResponseInputItem[];

export interface AgentSessionOptions {
  client?: OpenAI;
  environment?: NodeJS.ProcessEnv;
  globalJadRoot?: string;
  store?: SessionResponseStoreOptions;
}

export interface AgentRunOptions {
  turnId?: string;
  /** Cancels provider and foreground tool work for this run. */
  signal?: AbortSignal;
  /** Override setting.json max_tool_rounds for this run. */
  maxToolRounds?: number;
  onEvent?: (event: JsonRecord) => void | Promise<void>;
  onToolResult?: (item: ResponseInputItem) => void | Promise<void>;
}

export interface AgentRunResult {
  config: LoadedSessionConfig;
  response: PersistedResponse;
}

function validateMaxToolRounds(value: number): number {
  if (
    !Number.isSafeInteger(value) ||
    value < 0 ||
    value > MAX_CONFIGURED_TOOL_ROUNDS
  ) {
    throw new Error(
      `maxToolRounds must be a safe integer between 0 and ${MAX_CONFIGURED_TOOL_ROUNDS}`,
    );
  }
  return value;
}

export class AgentRunAbortedError extends Error {
  public override name = "AgentRunAbortedError";

  public constructor() {
    super("agent run was cancelled");
  }
}

function throwIfAborted(signal: AbortSignal | undefined): void {
  if (signal?.aborted) {
    throw new AgentRunAbortedError();
  }
}

function functionCallId(item: JsonRecord): string | undefined {
  return typeof item.call_id === "string" && item.call_id.length > 0
    ? item.call_id
    : undefined;
}

function makeMissingToolOutput(
  callId: string,
  errorCode: string,
  summary: string,
): ResponseInputItem {
  return {
    type: "function_call_output",
    call_id: callId,
    output: renderToolResult({
      id: callId,
      status: "error",
      summary,
      information: [
        ["error_type", "execution"],
        ["error_code", errorCode],
      ],
    }),
  };
}

function findPendingFunctionCalls(
  history: readonly ResponseInputItem[],
): JsonRecord[] {
  const pending = new Map<string, JsonRecord>();
  for (const item of history) {
    if (item.type === "function_call") {
      const callId = functionCallId(item);
      if (callId !== undefined) {
        pending.set(callId, item);
      }
      continue;
    }
    if (item.type === "function_call_output" && typeof item.call_id === "string") {
      pending.delete(item.call_id);
    }
  }
  return [...pending.values()];
}

function isRecord(value: unknown): value is JsonRecord {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

export function normalizeSessionInput(input: SessionUserInput): ResponseInputItem[] {
  if (typeof input === "string") {
    if (input.trim().length === 0) {
      throw new Error("session input must not be empty");
    }
    return [
      {
        type: "message",
        role: "user",
        content: [{ type: "input_text", text: input }],
      },
    ];
  }

  if (Array.isArray(input)) {
    if (input.length === 0) {
      throw new Error("session input item list must not be empty");
    }
    return [...input];
  }

  if (!isRecord(input)) {
    throw new Error("session input must be text or a Responses API input item");
  }
  return [input];
}

export function buildResponseRequest(
  config: LoadedSessionConfig,
  history: readonly ResponseInputItem[],
  tools: readonly JsonRecord[] = [],
): ResponseCreateParamsStreaming {
  const request: JsonRecord = {
    ...config.model.request,
    model: config.model.model,
    input: [...history],
    stream: true,
  };
  if (config.settings.system_prompt.length > 0) {
    request.instructions = config.settings.system_prompt;
  }
  if (tools.length > 0) {
    request.tools = [...tools];
  }
  return request as unknown as ResponseCreateParamsStreaming;
}

function createClient(
  config: LoadedSessionConfig,
  environment: NodeJS.ProcessEnv,
): OpenAI {
  const apiKey = resolveApiKey(config.model, environment);
  const debugFetch = createHttpDebugFetch(
    config.paths.session_dir,
    config.settings.http_debug,
    undefined,
    config.paths.http_debug_dir,
  );
  return new OpenAI({
    apiKey,
    baseURL: config.model.base_url,
    ...(debugFetch ? { fetch: debugFetch } : {}),
  });
}

export class AgentSession {
  private readonly runMutex = new AsyncMutex();
  private closed = false;

  private constructor(
    public readonly sessionDir: string,
    private readonly store: SessionResponseStore,
    private readonly loader: SessionConfigLoader,
    private readonly options: AgentSessionOptions,
    private readonly processManager: ProcessManager,
    private readonly taskStore: TaskStore,
  ) {}

  public static async open(
    sessionDir: string,
    options: AgentSessionOptions = {},
  ): Promise<AgentSession> {
    const loader = new SessionConfigLoader(sessionDir, {
      globalJadRoot: options.globalJadRoot,
    });
    const settings = await loader.loadSettings();
    const store = await SessionResponseStore.open(loader.paths.session_dir, {
      ...options.store,
      messagesPath: options.store?.messagesPath ?? loader.paths.messages_path,
      stagingDir: options.store?.stagingDir ?? loader.paths.staging_dir,
      durability: options.store?.durability ?? settings.persistence.durability,
      maxItemBytes: options.store?.maxItemBytes ?? settings.persistence.max_item_bytes,
    });
    const processManager = new ProcessManager(
      settings.tools.native.process?.storage_dir ?? join(settings.cwd, DEFAULT_NATIVE_PROCESS_STORAGE_DIR),
    );
    await processManager.recover();
    const taskStore = new TaskStore(
      settings.tools.native.task?.storage_path ?? join(settings.cwd, DEFAULT_NATIVE_TASK_STORAGE_PATH),
    );
    const session = new AgentSession(
      loader.paths.session_dir,
      store,
      loader,
      options,
      processManager,
      taskStore,
    );
    await session.repairPendingToolCalls();
    return session;
  }

  public async run(
    input: SessionUserInput,
    options: AgentRunOptions = {},
  ): Promise<AgentRunResult> {
    return this.runMutex.runExclusive(async () => {
      this.assertOpen();
      throwIfAborted(options.signal);
      const inputItems = normalizeSessionInput(input);
      await this.repairPendingToolCalls();
      await this.store.appendInputItems(inputItems);
      throwIfAborted(options.signal);
      const config = await this.loader.load();
      const catalog = await loadToolCatalog(config, {processManager: this.processManager, taskStore: this.taskStore});
      const client = this.options.client ?? createClient(config, this.options.environment ?? process.env);
      const maxToolRounds = validateMaxToolRounds(
        options.maxToolRounds ?? config.settings.max_tool_rounds,
      );

      for (let round = 0; ; round += 1) {
        const history: ResponseInputItem[] = [];
        for await (const item of this.store.readInputItems()) {
          history.push(item);
        }
        const request = buildResponseRequest(config, history, catalog.responseTools());
        const response = await persistOpenAIResponse(client, this.store, request, {
          turnId: options.turnId ? `${options.turnId}-${round}` : undefined,
          onEvent: options.onEvent,
          signal: options.signal,
        });
        const calls = responseFunctionCalls(response.event.response);
        if (calls.length === 0) {
          throwIfAborted(options.signal);
          return {config, response};
        }
        if (round >= maxToolRounds) {
          await this.appendToolOutputs(
            calls,
            [],
            "tool_round_limit",
            `Tool execution was not started because the ${maxToolRounds}-round limit was reached.`,
          );
          throw new Error(`tool execution exceeded ${maxToolRounds} tool rounds`);
        }

        const toolOutputsByCallId = new Map<string, ResponseInputItem>();
        let executionError: unknown;
        try {
          for (const call of calls) {
            throwIfAborted(options.signal);
            const toolOutput = await catalog.executeFunctionCall(call, {
              workspaceRoot: config.settings.cwd,
              sessionDir: config.paths.session_dir,
              signal: options.signal,
            });
            const callId = functionCallId(toolOutput);
            if (callId !== undefined) {
              toolOutputsByCallId.set(callId, toolOutput);
            }
          }
        } catch (error) {
          executionError = error;
        }

        const toolOutputs = await this.appendToolOutputs(
          calls,
          [...toolOutputsByCallId.values()],
          options.signal?.aborted
            ? "tool_execution_cancelled"
            : "tool_execution_failed",
          options.signal?.aborted
            ? "Tool execution was cancelled before every call produced a persisted result."
            : "Tool execution failed before every call produced a persisted result.",
        );
        for (const toolOutput of toolOutputs) {
          await options.onToolResult?.(toolOutput);
        }
        if (executionError !== undefined) {
          throw executionError;
        }
        throwIfAborted(options.signal);
      }
    });
  }

  private async repairPendingToolCalls(): Promise<void> {
    try {
      if ((await stat(this.store.messagesPath)).size === 0) {
        return;
      }
    } catch {
      return;
    }

    const history: ResponseInputItem[] = [];
    for await (const item of this.store.readInputItems()) {
      history.push(item);
    }
    const pendingCalls = findPendingFunctionCalls(history);
    if (pendingCalls.length === 0) {
      return;
    }

    const repairedOutputs = pendingCalls.flatMap((call) => {
      const callId = functionCallId(call);
      return callId === undefined
        ? []
        : [
            makeMissingToolOutput(
              callId,
              "tool_result_missing",
              "A previous tool call has no persisted result and was not rerun automatically.",
            ),
          ];
    });
    await this.store.appendInputItems(repairedOutputs);
  }

  private async appendToolOutputs(
    calls: readonly JsonRecord[],
    completedOutputs: readonly ResponseInputItem[],
    errorCode: string,
    summary: string,
  ): Promise<ResponseInputItem[]> {
    const completedByCallId = new Map<string, ResponseInputItem>();
    for (const output of completedOutputs) {
      const callId = functionCallId(output);
      if (callId !== undefined) {
        completedByCallId.set(callId, output);
      }
    }

    const orderedOutputs = calls.flatMap((call) => {
      const callId = functionCallId(call);
      if (callId === undefined) {
        return [];
      }
      return [
        completedByCallId.get(callId) ?? makeMissingToolOutput(callId, errorCode, summary),
      ];
    });
    await this.store.appendInputItems(orderedOutputs);
    return orderedOutputs;
  }

  public async listTools(): Promise<ToolDefinition[]> {
    this.assertOpen();
    const config = await this.loader.load();
    return (await loadToolCatalog(config, {processManager: this.processManager, taskStore: this.taskStore})).definitions();
  }

  public async close(): Promise<void> {
    if (this.closed) {
      return;
    }
    await this.processManager.close();
    await this.store.close();
    this.closed = true;
  }

  private assertOpen(): void {
    if (this.closed) {
      throw new Error("agent session is closed");
    }
  }
}
