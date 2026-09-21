export interface WebUiTextPart {
  type: 'reasoning' | 'text';
  text: string;
}

export interface WebUiToolCallPart {
  type: 'tool_call';
  callId: string;
  name: string;
  arguments: string;
}

export interface WebUiToolResultPart {
  type: 'tool_result';
  callId: string;
  output: string;
  status: 'success' | 'error' | 'unknown';
}

export type WebUiPart = WebUiTextPart | WebUiToolCallPart | WebUiToolResultPart;

export interface WebUiMessage {
  id: string;
  sender: 'system' | 'user' | 'assistant';
  text: string;
  timestamp: string;
  parts: WebUiPart[];
  status?: 'streaming' | 'complete';
  reasoningContent?: string;
}

export interface SessionSnapshot {
  id: string;
  session_id?: string;
  root_id?: string;
  root_label?: string;
  session_name: string;
  turns: number;
  messages: WebUiMessage[];
  model?: {
    provider?: string;
    model?: string;
  };
}

export interface ToolDefinition {
  id: string;
  source: 'native' | 'mcp';
  server?: string;
  name: string;
  description: string;
  inputSchema: Record<string, unknown>;
}

interface SseMessage {
  event: string;
  data: unknown;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function errorText(value: unknown): string {
  if (isRecord(value) && typeof value.error === 'string') {
    return value.error;
  }
  if (isRecord(value) && typeof value.message === 'string') {
    return value.message;
  }
  return typeof value === 'string' ? value : 'request failed';
}

async function readError(response: Response): Promise<string> {
  try {
    return errorText(JSON.parse(await response.text()));
  } catch {
    return `${response.status} ${response.statusText}`;
  }
}

export async function fetchSessions(): Promise<SessionSnapshot[]> {
  const response = await fetch('/api/sessions');
  if (!response.ok) {
    throw new Error(await readError(response));
  }
  const value = (await response.json()) as {sessions?: unknown};
  if (!Array.isArray(value.sessions)) {
    throw new Error('the server returned invalid sessions');
  }
  return value.sessions as SessionSnapshot[];
}

export async function fetchSession(sessionId: string): Promise<SessionSnapshot> {
  const response = await fetch(`/api/sessions/${encodeURIComponent(sessionId)}`);
  if (!response.ok) {
    throw new Error(await readError(response));
  }
  return (await response.json()) as SessionSnapshot;
}

export async function fetchSessionTools(sessionId: string): Promise<ToolDefinition[]> {
  const response = await fetch(`/api/sessions/${encodeURIComponent(sessionId)}/tools`);
  if (!response.ok) {
    throw new Error(await readError(response));
  }
  const value = (await response.json()) as {tools?: unknown};
  if (!Array.isArray(value.tools)) {
    throw new Error('the server returned invalid tool definitions');
  }
  return value.tools as ToolDefinition[];
}

async function* readSse(response: Response): AsyncGenerator<SseMessage> {
  if (!response.body) {
    throw new Error('the server did not return a stream');
  }

  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  let buffer = '';

  const parseBlock = (block: string): SseMessage | undefined => {
    let event = 'message';
    const data: string[] = [];
    for (const line of block.split('\n')) {
      if (line.startsWith('event:')) {
        event = line.slice('event:'.length).trim();
      } else if (line.startsWith('data:')) {
        data.push(line.slice('data:'.length).trimStart());
      }
    }
    if (data.length === 0) {
      return undefined;
    }
    try {
      return {event, data: JSON.parse(data.join('\n'))};
    } catch {
      throw new Error('the server returned invalid stream data');
    }
  };

  while (true) {
    const chunk = await reader.read();
    buffer += decoder.decode(chunk.value, {stream: !chunk.done}).replace(/\r\n/g, '\n');
    let separator = buffer.indexOf('\n\n');
    while (separator >= 0) {
      const block = buffer.slice(0, separator);
      buffer = buffer.slice(separator + 2);
      const message = parseBlock(block);
      if (message) {
        yield message;
      }
      separator = buffer.indexOf('\n\n');
    }
    if (chunk.done) {
      const message = parseBlock(buffer.trim());
      if (message) {
        yield message;
      }
      return;
    }
  }
}

function isWebUiPart(value: unknown): value is WebUiPart {
  if (!isRecord(value) || typeof value.type !== 'string') {
    return false;
  }
  if (value.type === 'reasoning' || value.type === 'text') {
    return typeof value.text === 'string';
  }
  if (value.type === 'tool_call') {
    return (
      typeof value.callId === 'string' &&
      typeof value.name === 'string' &&
      typeof value.arguments === 'string'
    );
  }
  if (value.type === 'tool_result') {
    return (
      typeof value.callId === 'string' &&
      typeof value.output === 'string' &&
      (value.status === 'success' || value.status === 'error' || value.status === 'unknown')
    );
  }
  return false;
}

function isWebUiMessage(value: unknown): value is WebUiMessage {
  if (!isRecord(value)) {
    return false;
  }
  return (
    typeof value.id === 'string' &&
    (value.sender === 'system' || value.sender === 'user' || value.sender === 'assistant') &&
    typeof value.text === 'string' &&
    typeof value.timestamp === 'string' &&
    Array.isArray(value.parts) &&
    value.parts.every(isWebUiPart)
  );
}

export interface StreamSessionRunOptions {
  signal?: AbortSignal;
  onMessage?: (message: WebUiMessage) => void;
  onCompleted?: (message: WebUiMessage) => void;
}

export interface StreamSessionRunResult {
  message?: WebUiMessage;
}

export async function streamSessionRun(
  sessionId: string,
  input: string,
  options: StreamSessionRunOptions = {},
): Promise<StreamSessionRunResult> {
  const response = await fetch(`/api/sessions/${encodeURIComponent(sessionId)}/run`, {
    method: 'POST',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({input}),
    signal: options.signal,
  });
  if (!response.ok) {
    throw new Error(await readError(response));
  }

  let completedMessage: WebUiMessage | undefined;
  for await (const message of readSse(response)) {
    if (message.event === 'ui.message') {
      if (isRecord(message.data) && isWebUiMessage(message.data.message)) {
        options.onMessage?.(message.data.message);
        if (message.data.message.status === 'complete') {
          completedMessage = message.data.message;
        }
      }
      continue;
    }
    if (message.event === 'completed') {
      if (isRecord(message.data) && isWebUiMessage(message.data.message)) {
        completedMessage = message.data.message;
        options.onCompleted?.(completedMessage);
      }
      continue;
    }
    if (message.event === 'error') {
      throw new Error(errorText(message.data));
    }
    if (message.event === 'done') {
      break;
    }
  }

  return {message: completedMessage};
}
