import OpenAI from "openai";
import type { ResponseCreateParamsStreaming } from "openai/resources/responses/responses.js";

import {
  SessionConfigLoader,
  resolveApiKey,
  type LoadedSessionConfig,
} from "./config.js";
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

export type SessionUserInput = string | ResponseInputItem | readonly ResponseInputItem[];

export interface AgentSessionOptions {
  client?: OpenAI;
  environment?: NodeJS.ProcessEnv;
  globalJadRoot?: string;
  store?: SessionResponseStoreOptions;
}

export interface AgentRunOptions {
  turnId?: string;
  onEvent?: (event: JsonRecord) => void | Promise<void>;
  onToolResult?: (item: ResponseInputItem) => void | Promise<void>;
}

export interface AgentRunResult {
  config: LoadedSessionConfig;
  response: PersistedResponse;
}

const MAX_TOOL_ROUNDS = 8;

class AsyncMutex {
  private tail: Promise<void> = Promise.resolve();

  public async runExclusive<T>(operation: () => Promise<T>): Promise<T> {
    const previous = this.tail;
    let release!: () => void;
    this.tail = new Promise<void>((resolve) => {
      release = resolve;
    });

    await previous;
    try {
      return await operation();
    } finally {
      release();
    }
  }
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
  ) {}

  public static async open(
    sessionDir: string,
    options: AgentSessionOptions = {},
  ): Promise<AgentSession> {
    const store = await SessionResponseStore.open(sessionDir, options.store);
    return new AgentSession(
      sessionDir,
      store,
      new SessionConfigLoader(sessionDir, {
        globalJadRoot: options.globalJadRoot,
      }),
      options,
    );
  }

  public async run(
    input: SessionUserInput,
    options: AgentRunOptions = {},
  ): Promise<AgentRunResult> {
    return this.runMutex.runExclusive(async () => {
      this.assertOpen();
      const inputItems = normalizeSessionInput(input);
      await this.store.appendInputItems(inputItems);
      const config = await this.loader.load();
      const catalog = await loadToolCatalog(config);
      const client = this.options.client ?? createClient(config, this.options.environment ?? process.env);

      for (let round = 0; round < MAX_TOOL_ROUNDS; round += 1) {
        const history: ResponseInputItem[] = [];
        for await (const item of this.store.readInputItems()) {
          history.push(item);
        }

        const request = buildResponseRequest(config, history, catalog.responseTools());
        const response = await persistOpenAIResponse(client, this.store, request, {
          turnId: options.turnId ? `${options.turnId}-${round}` : undefined,
          onEvent: options.onEvent,
        });
        const calls = responseFunctionCalls(response.event.response);
        if (calls.length === 0) {
          return {config, response};
        }

        const toolOutputs: ResponseInputItem[] = [];
        for (const call of calls) {
          toolOutputs.push(
            await catalog.executeFunctionCall(call, {
              workspaceRoot: config.settings.cwd,
              sessionDir: config.paths.session_dir,
            }),
          );
        }
        await this.store.appendInputItems(toolOutputs);
        for (const toolOutput of toolOutputs) {
          await options.onToolResult?.(toolOutput);
        }
      }

      throw new Error(`tool execution exceeded ${MAX_TOOL_ROUNDS} rounds`);
    });
  }

  public async listTools(): Promise<ToolDefinition[]> {
    this.assertOpen();
    const config = await this.loader.load();
    return (await loadToolCatalog(config)).definitions();
  }

  public async close(): Promise<void> {
    if (this.closed) {
      return;
    }
    await this.store.close();
    this.closed = true;
  }

  private assertOpen(): void {
    if (this.closed) {
      throw new Error("agent session is closed");
    }
  }
}
