import { openaiCredentials } from "@openai-oauth/local";
import { createOpenAIOptions } from "@openai-oauth/openai-client";
import OpenAI from "openai";
import {stat} from "node:fs/promises";
import {join} from "node:path";
import type { ResponseCreateParamsStreaming } from "openai/resources/responses/responses.js";

import {
  DEFAULT_NATIVE_MEMORY_STORAGE_DIR,
  DEFAULT_NATIVE_PROCESS_STORAGE_DIR,
  DEFAULT_NATIVE_TASK_STORAGE_DIR,
  DEFAULT_OPENAI_OAUTH_AUTH_FILE,
  MAX_CONFIGURED_TOOL_ROUNDS,
  SessionConfigLoader,
  resolveApiKey,
  type LoadedSessionConfig,
  type PromptRole,
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
import {MemoryStore} from "./tools/native/memory-store.js";
import {McpServerManager} from "./tools/mcp/manager.js";
import {collectStateContextSnapshot} from "./statecontext.js";

export type SessionUserInput = string | ResponseInputItem | readonly ResponseInputItem[];

export interface AgentSessionOptions {
  client?: OpenAI;
  environment?: NodeJS.ProcessEnv;
  globalJadRoot?: string;
  store?: SessionResponseStoreOptions;
  /**
   * Open the session on this path profile instead of the stored default.
   *
   * Two sessions opened on two profiles of one session directory keep separate
   * transcripts, staging and caches, so they can run concurrently.
   */
  profile?: string;
  /**
   * Session-scoped stores shared with another session opened on the same
   * directory (a second profile). The owner creates and, at refcount zero,
   * closes them — a shared process manager must not be recovered twice, and
   * closing one profile's session must not stop the other's processes.
   */
  shared?: AgentSessionSharedStores;
  /** Called when this session closes, so the owner can release shared stores. */
  onClosed?: () => void | Promise<void>;
}

/**
 * The stores derived from `cwd` rather than from a profile: every profile of one
 * session directory shares them, because storage directories configured for the
 * process, task and memory tools are session-scoped.
 */
export interface AgentSessionSharedStores {
  processManager: ProcessManager;
  taskStore: TaskStore;
  memoryStore: MemoryStore;
}

export interface AgentRunOptions {
  turnId?: string;
  /** Cancels provider and foreground tool work for this run. */
  signal?: AbortSignal;
  /** Override setting.json max_tool_rounds for this run. */
  maxToolRounds?: number;
  /** Select a model config path allowed by the active path profile. */
  modelConfigPath?: string;
  onEvent?: (event: JsonRecord) => void | Promise<void>;
  /** Called after each completed or incomplete provider response is committed. */
  onResponseCommitted?: (response: PersistedResponse) => void | Promise<void>;
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
  stateContext?: {role: PromptRole; text: string},
): ResponseCreateParamsStreaming {
  const request: JsonRecord = {
    ...config.model.request,
    model: config.model.model,
    input: [
      ...(config.settings.prompt_text.length > 0
        ? [{role: config.settings.prompt_role, content: [{type: "input_text", text: config.settings.prompt_text}]}]
        : []),
      ...history,
      ...(stateContext ? [{role: stateContext.role, content: [{type: "input_text", text: stateContext.text}]}] : []),
    ],
    stream: true,
  };
  if (tools.length > 0) {
    request.tools = [...tools];
  }
  return request as unknown as ResponseCreateParamsStreaming;
}

export function createClient(
  config: LoadedSessionConfig,
  environment: NodeJS.ProcessEnv,
  nativeFetch?: typeof fetch,
): OpenAI {
  if (config.model.auth_mode === "openai_oauth") {
    const options = createOpenAIOptions(
      openaiCredentials({
        authFilePath: config.model.oauth_auth_file ?? DEFAULT_OPENAI_OAUTH_AUTH_FILE,
        ...(nativeFetch ? {fetch: nativeFetch} : {}),
      }),
    );
    // Wrap the adapter, not its upstream fetch: the latter also refreshes OAuth
    // tokens, whose request bodies must never be written to HTTP debug logs.
    const debugFetch = createHttpDebugFetch(
      config.paths.session_dir,
      config.settings.http_debug,
      options.fetch,
      config.paths.http_debug_dir,
    );
    return new OpenAI({
      ...options,
      ...(debugFetch ? {fetch: debugFetch} : {}),
    });
  }

  const apiKey = resolveApiKey(config.model, environment);
  const debugFetch = createHttpDebugFetch(
    config.paths.session_dir,
    config.settings.http_debug,
    nativeFetch,
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
    /** Persistence for this session; readers that project the transcript use it. */
    public readonly store: SessionResponseStore,
    private readonly loader: SessionConfigLoader,
    private readonly options: AgentSessionOptions,
    private readonly processManager: ProcessManager,
    private readonly taskStore: TaskStore,
    private readonly memoryStore: MemoryStore,
    private readonly mcpManager: McpServerManager,
  ) {}

  private async readActiveStateContext(config: LoadedSessionConfig): Promise<{role: PromptRole; text: string} | undefined> {
    if (!config.settings.statecontext.enabled) return undefined;
    const snapshot = await this.store.readStateContextSnapshot();
    if (!snapshot || !config.settings.statecontext.trigger.includes(snapshot.trigger as "compaction")) return undefined;
    return {role: config.settings.statecontext.role, text: snapshot.text};
  }

  private async refreshStateContext(config: LoadedSessionConfig, trigger: "compaction"): Promise<void> {
    const snapshot = await collectStateContextSnapshot(config, {
      taskStore: this.taskStore,
      memoryStore: this.memoryStore,
      processManager: this.processManager,
    }, trigger);
    if (!snapshot) return;
    await this.store.writeStateContextSnapshot({schema_version: 1, ...snapshot});
  }

  public static async open(
    sessionDir: string,
    options: AgentSessionOptions = {},
  ): Promise<AgentSession> {
    const loader = new SessionConfigLoader(sessionDir, {
      globalJadRoot: options.globalJadRoot,
      ...(options.profile === undefined ? {} : {profile: options.profile}),
    });
    const settings = await loader.loadSettings();
    const store = await SessionResponseStore.open(loader.paths.session_dir, {
      ...options.store,
      messagesPath: options.store?.messagesPath ?? loader.paths.messages_path,
      stagingDir: options.store?.stagingDir ?? loader.paths.staging_dir,
      compactionCachePath: settings.paths.compaction_cache,
      historyCachePath: options.store?.historyCachePath ?? settings.paths.history_cache,
      usageCachePath: options.store?.usageCachePath ?? settings.paths.usage_cache,
      cursorCachePath: options.store?.cursorCachePath ?? settings.paths.cursor_cache,
      messagesBackupPath: settings.paths.messages_backup,
      stateContextPath: options.store?.stateContextPath ?? settings.paths.statecontext,
      durability: options.store?.durability ?? settings.persistence.durability,
      maxItemBytes: options.store?.maxItemBytes ?? settings.persistence.max_item_bytes,
      responseMetadata: options.store?.responseMetadata ?? settings.persistence.response_metadata,
    });
    const processManager = options.shared?.processManager ?? new ProcessManager(
      settings.tools.native.process?.storage_dir ?? join(settings.cwd, DEFAULT_NATIVE_PROCESS_STORAGE_DIR),
    );
    if (options.shared === undefined) {
      // Recovery is a session-startup action: a shared manager was already
      // recovered by the owner, and recovering again would mark the other
      // profile's live processes lost.
      await processManager.recover();
    }
    const shared = options.shared;
    const taskStore = shared?.taskStore ?? new TaskStore(
      settings.tools.native.task?.storage_dir ?? join(settings.cwd, DEFAULT_NATIVE_TASK_STORAGE_DIR),
    );
    const memoryStore = shared?.memoryStore ?? new MemoryStore(
      settings.tools.native.memory?.storage_dir ?? join(settings.cwd, DEFAULT_NATIVE_MEMORY_STORAGE_DIR),
    );
    const mcpManager = new McpServerManager(options.environment ?? process.env);
    const session = new AgentSession(
      loader.paths.session_dir,
      store,
      loader,
      options,
      processManager,
      taskStore,
      memoryStore,
      mcpManager,
    );
    await session.repairPendingToolCalls();
    return session;
  }

  public async readUsageSummary() {
    return this.store.readUsageSummary();
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
      const priorUsage = await this.store.readUsageSummary();
      if (priorUsage.turn_count >= Number.MAX_SAFE_INTEGER) {
        throw new Error("session turn counter is exhausted");
      }
      const sessionTurn = priorUsage.turn_count + 1;
      await this.store.appendInputItems(inputItems);
      throwIfAborted(options.signal);
      const config = await this.loader.load(options.modelConfigPath);
      const catalog = await loadToolCatalog(config, {processManager: this.processManager, taskStore: this.taskStore, memoryStore: this.memoryStore, mcpManager: this.mcpManager});
      const client = this.options.client ?? createClient(config, this.options.environment ?? process.env);
      const maxToolRounds = validateMaxToolRounds(
        options.maxToolRounds ?? config.settings.max_tool_rounds,
      );

      for (let round = 0; ; round += 1) {
        const history: ResponseInputItem[] = [];
        for await (const item of this.store.readInputItems()) {
          history.push(item);
        }
        const stateContext = await this.readActiveStateContext(config);
        const request = buildResponseRequest(
          config,
          history,
          catalog.responseTools(),
          stateContext,
        );
        const response = await persistOpenAIResponse(client, this.store, request, {
          turnId: options.turnId ? `${options.turnId}-${round}` : undefined,
          sessionTurn,
          sessionStep: round + 1,
          onEvent: options.onEvent,
          signal: options.signal,
        });
        await options.onResponseCommitted?.(response);
        const calls = responseFunctionCalls(response.event.response);
        const usage = response.event.response.usage;
        let refreshStateContext = false;
        if (
          config.settings.compaction.auto_enabled &&
          response.event.response.status === "completed" &&
          isRecord(usage) &&
          typeof usage.input_tokens === "number" &&
          usage.input_tokens >= config.settings.compaction.trigger_tokens
        ) {
          const responseId = response.event.response.id;
          const compaction = await this.compactWithSettings(
            config.settings.compaction,
            calls.length > 0 && typeof responseId === "string" ? responseId : undefined,
          );
          refreshStateContext = compaction.compacted
            && config.settings.statecontext.enabled
            && config.settings.statecontext.trigger.includes("compaction");
        }
        if (calls.length === 0) {
          if (refreshStateContext) await this.refreshStateContext(config, "compaction");
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
          if (refreshStateContext) await this.refreshStateContext(config, "compaction");
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
        if (refreshStateContext) await this.refreshStateContext(config, "compaction");
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

  public async compact(): Promise<{compacted: boolean; compacted_items: number; retained_items: number; backup_path: string}> {
    return this.runMutex.runExclusive(async () => {
      this.assertOpen();
      const config = await this.loader.load();
      const result = await this.compactWithSettings(config.settings.compaction);
      if (result.compacted) await this.refreshStateContext(config, "compaction");
      return result;
    });
  }

  private async compactWithSettings(
    settings: LoadedSessionConfig["settings"]["compaction"],
    preserveResponseId?: string,
  ): Promise<{compacted: boolean; compacted_items: number; retained_items: number; backup_path: string}> {
    return this.store.compactHistory({
      turnType: settings.keepbatch.turn.type,
      keepTurns: settings.keepbatch.turn.keep,
      keepToolPairs: settings.keepbatch.tools,
      backupMode: settings.backup_mode,
      ...(preserveResponseId ? {preserveResponseId} : {}),
    });
  }

  public async listTools(): Promise<ToolDefinition[]> {
    return this.runMutex.runExclusive(async () => {
      this.assertOpen();
      const config = await this.loader.load();
      return (await loadToolCatalog(config, {processManager: this.processManager, taskStore: this.taskStore, memoryStore: this.memoryStore, mcpManager: this.mcpManager})).definitions();
    });
  }

  public async close(): Promise<void> {
    await this.runMutex.runExclusive(async () => {
      if (this.closed) return;
      // A shared manager belongs to the session directory, not to this profile:
      // its owner closes it when the last profile's session is gone.
      if (this.options.shared === undefined) {
        await this.processManager.close();
      }
      await this.mcpManager.close();
      await this.store.close();
      this.closed = true;
      await this.options.onClosed?.();
    });
  }

  private assertOpen(): void {
    if (this.closed) {
      throw new Error("agent session is closed");
    }
  }
}
