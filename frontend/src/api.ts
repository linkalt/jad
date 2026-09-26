import appConfig from './config.json';

function apiUrl(path: string): string {
  return `${appConfig.api_base_path.replace(/\/$/, '')}${path}`;
}

export interface WebUiPartTruncation {
  /** Full UTF-8 size of the text before it was cut for transport. */
  bytes: number;
}

export type WebUiTextPart =
  | {type: 'reasoning'; text: string; truncated?: WebUiPartTruncation}
  | {type: 'text'; text: string; truncated?: WebUiPartTruncation};

export interface WebUiToolCallPart {
  type: 'tool_call';
  callId: string;
  name: string;
  arguments: string;
  /** True while the provider is still streaming this call's arguments. */
  partial?: boolean;
  truncated?: WebUiPartTruncation;
}

export interface WebUiToolResultPart {
  type: 'tool_result';
  callId: string;
  output: string;
  status: 'success' | 'error' | 'unknown';
  truncated?: WebUiPartTruncation;
}

export type WebUiPart = WebUiTextPart | WebUiToolCallPart | WebUiToolResultPart;

export interface WebUiMessageStep {
  id: string;
  startPart: number;
  endPart: number;
  hasTools: boolean;
}

export interface WebUiMessage {
  id: string;
  sender: 'system' | 'user' | 'assistant';
  text: string;
  timestamp: string;
  parts: WebUiPart[];
  steps?: WebUiMessageStep[];
  status?: 'streaming' | 'complete';
  reasoningContent?: string;
  /**
   * Turn that produced this message, set when a part was capped. Passing it to
   * `fetchSessionMessage` returns the message with its parts whole.
   */
  cursor?: number;
}

export interface ResponseTokenUsage {
  input_tokens?: number;
  output_tokens?: number;
  total_tokens?: number;
  cached_input_tokens?: number;
}

export interface CurrentResponseUsage extends ResponseTokenUsage {
  turn: number;
  step: number;
  response_id?: string;
}

export interface SessionUsageSummary extends ResponseTokenUsage {
  completed_responses: number;
  responses_with_usage: number;
  turn_count: number;
  history_partial: boolean;
  current?: CurrentResponseUsage;
}

export type SessionRunStatus = 'running' | 'completed' | 'failed' | 'aborted';

/**
 * The run a client can attach to for a session. It stays present while the turn
 * is in flight and briefly after it ends, so a reload can still catch up.
 */
export interface SessionRunState {
  turn_id: string;
  started_at: string;
  status: SessionRunStatus;
}

/**
 * The `profile` query parameter.
 *
 * A session's path profiles are addressed per request: a tab pinned to one
 * profile sends it on every session-scoped request, so the server resolves that
 * profile's transcript, model and run slot instead of the stored default. An
 * absent profile means "the session's stored default", which is what an
 * unqualified view shows.
 */
export function profileQuery(profile: string | undefined): string {
  return profile === undefined || profile.length === 0
    ? ''
    : `profile=${encodeURIComponent(profile)}`;
}

function withProfile(query: URLSearchParams, profile: string | undefined): string {
  if (profile !== undefined && profile.length > 0) {
    query.set('profile', profile);
  }
  return query.size === 0 ? '' : `?${query.toString()}`;
}

export interface SessionSnapshot {
  id: string;
  session_id?: string;
  root_id?: string;
  root_label?: string;
  active_profile?: string;
  path_profiles?: string[];
  model_default?: string;
  models?: string[];
  session_name: string;
  turns: number;
  /** Absent in the session list: transcripts are fetched per session. */
  messages?: WebUiMessage[];
  model?: {
    provider?: string;
    model?: string;
  };
  usage?: SessionUsageSummary;
  run?: SessionRunState;
  /**
   * The run of every profile that has one, keyed by profile name; `run` is the
   * stored default's. A tab pinned to a profile reads its status here.
   */
  runs_by_profile?: Record<string, SessionRunState>;
  /** The profile the session stores as its default; `active_profile` is the one this response resolved. */
  default_profile?: string;
  /** Set when the transcript was capped; `next_before` fetches older turns. */
  page?: SessionPage;
  /**
   * Reference to the history block this snapshot left out: turns a compaction
   * removed, served from the backup by `fetchSessionHistory`.
   */
  history?: SessionHistory;
}

export interface SessionPathProfileUpdate {
  active_profile: string;
  path_profiles: string[];
  model_default?: string;
  models?: string[];
  changed: boolean;
}

export interface SessionPathModelDefaultUpdate {
  active_profile: string;
  path_profiles: string[];
  model_default: string;
  models: string[];
  model: {provider: string; model: string};
  changed: boolean;
}

export interface SessionPage {
  has_more: boolean;
  next_before: number | null;
  generation: string;
  stale_cursor: boolean;
}

export interface SessionHistory {
  etag: string | null;
  count: number;
}

/**
 * A response that may be answered with `304 Not Modified`: `value` is only
 * present when the server had something new to send.
 */
export type ConditionalResult<T> =
  | {kind: 'ok'; value: T; etag: string | undefined}
  | {kind: 'not-modified'; etag: string | undefined};

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

/**
 * Conditional request: the server is asked to revalidate a fingerprint the
 * client already holds, and answers `304` when the state is unchanged.
 */
async function fetchConditional<T>(
  path: string,
  etag: string | undefined,
  parse: (value: unknown) => T,
): Promise<ConditionalResult<T>> {
  const response = await fetch(apiUrl(path), {
    headers: etag === undefined ? {} : {'If-None-Match': etag},
  });
  const responseEtag = response.headers.get('etag') ?? undefined;
  if (response.status === 304) {
    return {kind: 'not-modified', etag: responseEtag ?? etag};
  }
  if (!response.ok) {
    throw new Error(await readError(response));
  }
  return {kind: 'ok', value: parse(await response.json()), etag: responseEtag};
}

export async function fetchSessions(
  etag?: string,
): Promise<ConditionalResult<SessionSnapshot[]>> {
  return fetchConditional('/sessions', etag, value => {
    if (!isRecord(value) || !Array.isArray(value.sessions)) {
      throw new Error('the server returned invalid sessions');
    }
    return value.sessions as SessionSnapshot[];
  });
}

export async function fetchSession(
  sessionId: string,
  options: {
    etag?: string;
    before?: number;
    generation?: string;
    turns?: number;
    /** One path profile of the session; absent follows its stored default. */
    profile?: string;
  } = {},
): Promise<ConditionalResult<SessionSnapshot>> {
  const query = new URLSearchParams();
  if (options.before !== undefined) {
    query.set('before', String(options.before));
  }
  if (options.generation !== undefined) {
    query.set('cursor_generation', options.generation);
  }
  if (options.turns !== undefined) {
    query.set('turns', String(options.turns));
  }
  const suffix = withProfile(query, options.profile);
  return fetchConditional(
    `/sessions/${encodeURIComponent(sessionId)}${suffix}`,
    options.etag,
    value => {
      if (!isRecord(value)) {
        throw new Error('the server returned an invalid session');
      }
      return value as unknown as SessionSnapshot;
    },
  );
}

/**
 * The history block a snapshot referred to.
 *
 * Turns a compaction removed, folded from the backup, are static until the next
 * compaction — so they are fetched once per identity instead of riding on every
 * snapshot a committed step produces. A `304` means the held copy is still the
 * one the server would send.
 */
export async function updateSessionPathProfile(
  sessionRef: string,
  activeProfile: string,
): Promise<SessionPathProfileUpdate> {
  const response = await fetch(
    apiUrl(`/sessions/${encodeURIComponent(sessionRef)}/profile`),
    {
      method: 'POST',
      headers: {'content-type': 'application/json'},
      body: JSON.stringify({active_profile: activeProfile}),
    },
  );
  if (!response.ok) {
    throw new Error(await readError(response));
  }
  const value: unknown = await response.json();
  if (
    !isRecord(value) ||
    typeof value.active_profile !== 'string' ||
    !Array.isArray(value.path_profiles) ||
    !value.path_profiles.every(profile => typeof profile === 'string') ||
    typeof value.changed !== 'boolean' ||
    (value.model_default !== undefined && typeof value.model_default !== 'string') ||
    (value.models !== undefined &&
      (!Array.isArray(value.models) || !value.models.every(model => typeof model === 'string')))
  ) {
    throw new Error('the server returned invalid session path profile settings');
  }
  return value as unknown as SessionPathProfileUpdate;
}

export async function updateSessionProfileModelDefault(
  sessionRef: string,
  modelDefault: string,
): Promise<SessionPathModelDefaultUpdate> {
  const response = await fetch(
    apiUrl(`/sessions/${encodeURIComponent(sessionRef)}/model-default`),
    {
      method: 'POST',
      headers: {'content-type': 'application/json'},
      body: JSON.stringify({model_default: modelDefault}),
    },
  );
  if (!response.ok) {
    throw new Error(await readError(response));
  }
  const value: unknown = await response.json();
  if (
    !isRecord(value) ||
    typeof value.active_profile !== 'string' ||
    !Array.isArray(value.path_profiles) ||
    !value.path_profiles.every(profile => typeof profile === 'string') ||
    typeof value.model_default !== 'string' ||
    !Array.isArray(value.models) ||
    !value.models.every(model => typeof model === 'string') ||
    !isRecord(value.model) ||
    typeof value.model.provider !== 'string' ||
    typeof value.model.model !== 'string' ||
    typeof value.changed !== 'boolean'
  ) {
    throw new Error('the server returned invalid session model settings');
  }
  return value as unknown as SessionPathModelDefaultUpdate;
}

export async function fetchSessionHistory(
  sessionId: string,
  etag?: string,
  profile?: string,
): Promise<ConditionalResult<{messages: WebUiMessage[]}>> {
  return fetchConditional(
    `/sessions/${encodeURIComponent(sessionId)}/history${profileQuery(profile)}`,
    etag,
    value => {
      if (!isRecord(value) || !Array.isArray(value.messages)) {
        throw new Error('the server returned invalid session history');
      }
      return {messages: value.messages as WebUiMessage[]};
    },
  );
}

/**
 * One transcript message, whole.
 *
 * A snapshot caps the inline text of large parts, so a message it sent may carry
 * a cut reasoning summary, call arguments or tool output. `cursor` is the turn
 * the message came from, which a capped transcript message publishes; a message
 * from the history block has none and is served from the block itself.
 */
export async function fetchSessionMessage(
  sessionId: string,
  messageId: string,
  cursor?: number,
  profile?: string,
): Promise<WebUiMessage> {
  const query = new URLSearchParams();
  if (cursor !== undefined) {
    query.set('cursor', String(cursor));
  }
  if (profile !== undefined && profile.length > 0) {
    query.set('profile', profile);
  }
  const suffix = query.size === 0 ? '' : `?${query.toString()}`;
  const response = await fetch(
    apiUrl(
      `/sessions/${encodeURIComponent(sessionId)}/messages/${encodeURIComponent(messageId)}${suffix}`,
    ),
  );
  if (!response.ok) {
    throw new Error(await readError(response));
  }
  const value: unknown = await response.json();
  if (!isRecord(value) || !isRecord(value.message)) {
    throw new Error('the server returned an invalid message');
  }
  return value.message as unknown as WebUiMessage;
}

export interface SessionFileListing {
  /** cwd-relative paths, shallow files first. */
  files: string[];
  /** True when a cap, inaccessible branch, or unsafe path left the list incomplete. */
  truncated: boolean;
}

/**
 * The session's cwd listing for the `@` menu: the paths the agent's own tools
 * accept, relative to the session's working directory. Names only — the
 * endpoint never returns file contents.
 */
export async function fetchSessionFiles(
  sessionRef: string,
  signal?: AbortSignal,
  profile?: string,
): Promise<SessionFileListing> {
  const response = await fetch(
    apiUrl(`/sessions/${encodeURIComponent(sessionRef)}/files${profileQuery(profile)}`),
    {signal},
  );
  if (!response.ok) {
    throw new Error(await readError(response));
  }
  const body = (await response.json()) as {files?: unknown; truncated?: unknown};
  return {
    files: Array.isArray(body.files)
      ? body.files.filter((file): file is string => typeof file === 'string')
      : [],
    truncated: body.truncated === true,
  };
}

export async function fetchSessionTools(sessionId: string, profile?: string): Promise<ToolDefinition[]> {  const response = await fetch(apiUrl(`/sessions/${encodeURIComponent(sessionId)}/tools${profileQuery(profile)}`));
  if (!response.ok) {
    throw new Error(await readError(response));
  }
  const value = (await response.json()) as {tools?: unknown};
  if (!Array.isArray(value.tools)) {
    throw new Error('the server returned invalid tool definitions');
  }
  return value.tools as ToolDefinition[];
}

export interface SessionCompactionResult {
  compacted: boolean;
  compacted_items: number;
  retained_items: number;
}

export async function compactSession(sessionId: string): Promise<SessionCompactionResult> {
  const response = await fetch(apiUrl(`/sessions/${encodeURIComponent(sessionId)}/compact`), {
    method: 'POST',
  });
  if (!response.ok) {
    throw new Error(await readError(response));
  }
  const value: unknown = await response.json();
  if (!isRecord(value) || !isRecord(value.compaction)) {
    throw new Error('the server returned invalid compaction data');
  }
  const compaction = value.compaction;
  if (
    typeof compaction.compacted !== 'boolean' ||
    typeof compaction.compacted_items !== 'number' ||
    !Number.isSafeInteger(compaction.compacted_items) ||
    compaction.compacted_items < 0 ||
    typeof compaction.retained_items !== 'number' ||
    !Number.isSafeInteger(compaction.retained_items) ||
    compaction.retained_items < 0
  ) {
    throw new Error('the server returned invalid compaction data');
  }
  return {
    compacted: compaction.compacted,
    compacted_items: compaction.compacted_items,
    retained_items: compaction.retained_items,
  };
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

function isWebUiMessageStep(value: unknown): value is WebUiMessageStep {
  return (
    isRecord(value) &&
    typeof value.id === 'string' &&
    isNonnegativeSafeInteger(value.startPart) &&
    isNonnegativeSafeInteger(value.endPart) &&
    value.endPart > value.startPart &&
    typeof value.hasTools === 'boolean'
  );
}

function isWebUiMessageSteps(value: unknown, partCount: number): value is WebUiMessageStep[] {
  if (value === undefined) {
    return true;
  }
  if (!Array.isArray(value) || value.length < 2 || !value.every(isWebUiMessageStep)) {
    return false;
  }
  let nextPart = 0;
  for (const step of value) {
    if (step.startPart !== nextPart || step.endPart > partCount) {
      return false;
    }
    nextPart = step.endPart;
  }
  return nextPart === partCount;
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
    value.parts.every(isWebUiPart) &&
    isWebUiMessageSteps(value.steps, value.parts.length)
  );
}

function isNonnegativeSafeInteger(value: unknown): value is number {
  return typeof value === 'number' && Number.isSafeInteger(value) && value >= 0;
}

function hasValidTokenFields(value: Record<string, unknown>): boolean {
  return ['input_tokens', 'output_tokens', 'total_tokens', 'cached_input_tokens']
    .every(field => value[field] === undefined || isNonnegativeSafeInteger(value[field]));
}

function isSessionUsageSummary(value: unknown): value is SessionUsageSummary {
  if (
    !isRecord(value) ||
    !isNonnegativeSafeInteger(value.completed_responses) ||
    !isNonnegativeSafeInteger(value.responses_with_usage) ||
    value.responses_with_usage > value.completed_responses ||
    !isNonnegativeSafeInteger(value.turn_count) ||
    typeof value.history_partial !== 'boolean' ||
    !hasValidTokenFields(value)
  ) {
    return false;
  }
  if (value.current === undefined) return true;
  return (
    isRecord(value.current) &&
    Number.isSafeInteger(value.current.turn) &&
    (value.current.turn as number) > 0 &&
    Number.isSafeInteger(value.current.step) &&
    (value.current.step as number) > 0 &&
    (value.current.response_id === undefined || typeof value.current.response_id === 'string') &&
    hasValidTokenFields(value.current)
  );
}

export interface StreamSessionRunOptions {
  /** Model identifier selected for the new run. */
  model?: string;
  /**
   * The path profile to run in. A tab pinned to one profile must run there, so
   * its transcript, model default and run slot are that profile's.
   */
  profile?: string;
  signal?: AbortSignal;
  onMessage?: (message: WebUiMessage) => void;
  /**
   * An increment to the message a client already holds: `ui.delta` appends to a
   * part, `ui.part` replaces one. The full snapshot still arrives on structural
   * changes and at completion, so a client can ignore increments and stay correct
   * from the next snapshot on.
   */
  onDelta?: (messageId: string, index: number, append: string) => void;
  onPart?: (messageId: string, index: number, part: WebUiPart) => void;
  onCompleted?: (message: WebUiMessage) => void;
  onUsage?: (usage: SessionUsageSummary) => void;
  onRun?: (run: SessionRunState) => void;
  onIdle?: () => void;
}

export interface StreamSessionRunResult {
  message?: WebUiMessage;
  usage?: SessionUsageSummary;
  /** Whether the server reported a run for this session (attach path only). */
  attached?: boolean;
}

function isSessionRunState(value: unknown): value is SessionRunState {
  return (
    isRecord(value) &&
    typeof value.turn_id === 'string' &&
    typeof value.started_at === 'string' &&
    (value.status === 'running' ||
      value.status === 'completed' ||
      value.status === 'failed' ||
      value.status === 'aborted')
  );
}

/**
 * Consumes the shared run event stream. `ui.message` payloads are complete
 * snapshots of the message, so a stream that starts in the middle of a run only
 * needs the newest snapshot to be correct.
 */
async function consumeRunStream(
  response: Response,
  options: StreamSessionRunOptions,
): Promise<StreamSessionRunResult> {
  let completedMessage: WebUiMessage | undefined;
  let sessionUsage: SessionUsageSummary | undefined;
  let attached = false;

  for await (const message of readSse(response)) {
    if (message.event === 'run') {
      if (isSessionRunState(message.data)) {
        attached = true;
        options.onRun?.(message.data);
      }
      continue;
    }
    if (message.event === 'ui.message') {
      if (isRecord(message.data) && isWebUiMessage(message.data.message)) {
        options.onMessage?.(message.data.message);
        if (message.data.message.status === 'complete') {
          completedMessage = message.data.message;
        }
      }
      continue;
    }
    if (message.event === 'ui.delta') {
      if (
        isRecord(message.data) &&
        typeof message.data.id === 'string' &&
        isNonnegativeSafeInteger(message.data.index) &&
        typeof message.data.append === 'string'
      ) {
        options.onDelta?.(message.data.id, message.data.index, message.data.append);
      }
      continue;
    }
    if (message.event === 'ui.part') {
      if (
        isRecord(message.data) &&
        typeof message.data.id === 'string' &&
        isNonnegativeSafeInteger(message.data.index) &&
        isWebUiPart(message.data.part)
      ) {
        options.onPart?.(message.data.id, message.data.index, message.data.part);
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
    if (message.event === 'usage') {
      if (isRecord(message.data) && isSessionUsageSummary(message.data.usage)) {
        sessionUsage = message.data.usage;
        options.onUsage?.(sessionUsage);
      }
      continue;
    }
    if (message.event === 'idle') {
      options.onIdle?.();
      continue;
    }
    if (message.event === 'error') {
      throw new Error(errorText(message.data));
    }
    if (message.event === 'done') {
      break;
    }
  }

  return {
    ...(completedMessage === undefined ? {} : {message: completedMessage}),
    ...(sessionUsage === undefined ? {} : {usage: sessionUsage}),
    ...(attached ? {attached: true} : {}),
  };
}

export async function streamSessionRun(
  sessionId: string,
  input: string,
  options: StreamSessionRunOptions = {},
): Promise<StreamSessionRunResult> {
  const response = await fetch(apiUrl(`/sessions/${encodeURIComponent(sessionId)}/run${profileQuery(options.profile)}`), {
    method: 'POST',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({
      input,
      ...(options.model === undefined ? {} : {model: options.model}),
    }),
    signal: options.signal,
  });
  if (!response.ok) {
    throw new Error(await readError(response));
  }

  return consumeRunStream(response, options);
}

/**
 * Attaches to a run this client did not start — the case after a page reload or
 * from a second tab. The server replays the current snapshot and continues with
 * live events, and answers `idle` when nothing is running.
 */
export async function attachSessionRun(
  sessionRef: string,
  options: StreamSessionRunOptions = {},
): Promise<StreamSessionRunResult> {
  const response = await fetch(
    apiUrl(`/sessions/${encodeURIComponent(sessionRef)}/run/stream${profileQuery(options.profile)}`),
    {
      ...(options.signal ? {signal: options.signal} : {}),
    },
  );
  if (!response.ok) {
    throw new Error(await readError(response));
  }

  return consumeRunStream(response, options);
}

export interface StopSessionRunResult {
  stopped: boolean;
  turn_id?: string;
}

/** Stops the run in flight for a session; the run no longer commits a response. */
export async function stopSessionRun(
  sessionRef: string,
  profile?: string,
): Promise<StopSessionRunResult> {
  const response = await fetch(
    apiUrl(`/sessions/${encodeURIComponent(sessionRef)}/run/stop${profileQuery(profile)}`),
    {method: 'POST'},
  );
  if (!response.ok) {
    throw new Error(await readError(response));
  }
  const value: unknown = await response.json();
  if (!isRecord(value) || !isRecord(value.stop) || typeof value.stop.stopped !== 'boolean') {
    throw new Error('the server returned invalid stop data');
  }
  const turnId = value.stop.turn_id;
  return {
    stopped: value.stop.stopped,
    ...(typeof turnId === 'string' ? {turn_id: turnId} : {}),
  };
}
