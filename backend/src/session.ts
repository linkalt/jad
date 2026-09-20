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

export type SessionUserInput = string | ResponseInputItem | readonly ResponseInputItem[];

export interface AgentSessionOptions {
  client?: OpenAI;
  environment?: NodeJS.ProcessEnv;
  store?: SessionResponseStoreOptions;
}

export interface AgentRunOptions {
  turnId?: string;
  onEvent?: (event: JsonRecord) => void | Promise<void>;
}

export interface AgentRunResult {
  config: LoadedSessionConfig;
  response: PersistedResponse;
}

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
      new SessionConfigLoader(sessionDir),
      options,
    );
  }

  public async run(
    input: SessionUserInput,
    options: AgentRunOptions = {},
  ): Promise<AgentRunResult> {
    return this.runMutex.runExclusive(async () => {
      this.assertOpen();
      const config = await this.loader.load();
      const inputItems = normalizeSessionInput(input);
      await this.store.appendInputItems(inputItems);

      const history: ResponseInputItem[] = [];
      for await (const item of this.store.readInputItems()) {
        history.push(item);
      }

      const request = buildResponseRequest(config, history);
      const client = this.options.client ?? createClient(config, this.options.environment ?? process.env);
      const response = await persistOpenAIResponse(client, this.store, request, {
        turnId: options.turnId,
        onEvent: options.onEvent,
      });
      return { config, response };
    });
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
