import appConfig from './config.json' with {type: 'json'};
import {createAuthenticatedFetch} from './api-auth.ts';
import {requestApiToken} from './token-request.ts';
import {
  parseCheckpointList,
  parseRestorePlan,
  type CheckpointList,
  type RestoreMode,
  type RestorePlan,
} from './checkpoint-view.ts';
import {parseInteractionEnd, type InteractionEnd} from './interaction-end.ts';
import {parseHookReport, type HookReport} from './hook-report-view.ts';
import {parseProviderRetryWait, type ProviderRetryWait} from './provider-retry-view.ts';
import type {RunInputPart} from './image-attachments.ts';

const authenticatedFetch = createAuthenticatedFetch(
  appConfig.api_token_storage_key,
  globalThis.fetch,
  appConfig.api_token_fragment_param,
  requestApiToken,
);

function apiUrl(path: string): string {
  return `${appConfig.api_base_path.replace(/\/$/, '')}${path}`;
}

export interface WebUiPartTruncation {
  /** Full UTF-8 size of the text before it was cut for transport. */
  bytes: number;
}

/** A web source the provider cited in answer text. */
export interface WebUiCitation {
  url: string;
  title?: string;
}

export type WebUiTextPart =
  | {type: 'reasoning'; text: string; truncated?: WebUiPartTruncation}
  | {
      type: 'text';
      text: string;
      citations?: WebUiCitation[];
      /** On a user message: file names of the images it carries that the server stored. */
      images?: string[];
      /** On a user message: names of the files (PDF) attached to it, in order. */
      files?: string[];
      truncated?: WebUiPartTruncation;
    };

/**
 * A tool the provider ran itself (hosted web search, web fetch, or any tool a
 * provider adds later). No tool is named in the client: `name` is whatever the
 * server derived from the item type, and `raw` is the provider's item as JSON.
 */
export interface WebUiHostedToolPart {
  type: 'hosted_tool';
  callId: string;
  name: string;
  /** The provider's state, e.g. `in_progress`, `searching`, `completed`, `failed`. */
  status?: string;
  /** A short text such as the query or URL; absent until the provider reports it. */
  target?: string;
  /** The provider's item as JSON, shortened to the inline limit when `truncated` is set. */
  raw?: string;
  /** File names of images the provider generated and the server stored; fetch them with `fetchSessionImage`. */
  images?: string[];
  truncated?: WebUiPartTruncation;
}

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
  /** File names of images the server stored for this result; fetch them with `fetchSessionImage`. */
  images?: string[];
  /** How many images of this result are still inline in the transcript; fetch them with `fetchMessageImage`. */
  inlineImages?: number;
  truncated?: WebUiPartTruncation;
}

export type WebUiPart =
  | WebUiTextPart
  | WebUiToolCallPart
  | WebUiToolResultPart
  | WebUiHostedToolPart;

export type WebUiInteractionDecision = 'accept' | 'update' | 'decline';

export type WebUiInteractionKind = 'draft' | 'approval';

/** A tool call waiting on the user's decision, published as first-class run state. */
export interface WebUiInteraction {
  id: string;
  /** Name of the native tool that asked, e.g. `request_draft`. */
  tool: string;
  tool_call_id: string;
  /** The tool's proposal; any JSON object the tool defines. */
  draft: Record<string, unknown>;
  /**
   * What kind of question this is. Absent means a draft, so a backend that never
   * sends it keeps working. An `approval` is a fixed call to allow or deny, with
   * `draft` holding its preview; it cannot be edited.
   */
  kind?: WebUiInteractionKind;
  title?: string;
  created_at: string;
  /** When the run stops waiting and reports the question as unanswered. */
  expires_at: string;
  /** The latest deadline an extension may push to. */
  max_expires_at: string;
  /** How much one extension adds to the deadline; zero when extending is off. */
  extend_ms: number;
}

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
  /** Compatibility mirror of the run's pending interaction while it holds the turn open. */
  pending?: WebUiInteraction;
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
    : `?profile=${encodeURIComponent(profile)}`;
}

function withProfile(query: URLSearchParams, profile: string | undefined): string {
  if (profile !== undefined && profile.length > 0) {
    query.set('profile', profile);
  }
  return query.size === 0 ? '' : `?${query.toString()}`;
}

/**
 * A run that failed after its user message was saved. The server keeps these
 * beside the transcript, never in it; `message_id` names the user message the
 * failure belongs to and is absent when the server could not name one.
 */
export interface SessionRunFailure {
  turn: number;
  message_id?: string;
  turn_id: string;
  at: string;
  message: string;
}

export interface SessionSnapshot {
  id: string;
  session_id?: string;
  root_id?: string;
  root_label?: string;
  /** The session's category (session.json); absent when it has none. */
  category?: string;
  active_profile?: string;
  profiles?: string[];
  /** Display name each profile file sets, by profile ID; a profile without one is absent. */
  profile_names?: Record<string, string>;
  model_default?: string;
  models?: string[];
  front_compaction?: {enabled: boolean; delete_mode: 'permanent' | 'recoverable'};
  session_name: string;
  turns: number;
  /** Turns in each profile's transcript, by profile ID; absent when the session has no profiles. */
  profile_turns?: Record<string, number>;
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
  /** Set when the transcript was capped; `next_before` fetches older turns. */
  page?: SessionPage;
  /**
   * Reference to the history block this snapshot left out: turns a compaction
   * removed, served from the backup by `fetchSessionHistory`.
   */
  history?: SessionHistory;
  /** Failed runs, oldest first. Absent when the session has none. */
  failures?: SessionRunFailure[];
}

export interface SessionPathModelDefaultUpdate {
  active_profile: string;
  profiles: string[];
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
  signal?: AbortSignal,
): Promise<ConditionalResult<T>> {
  const response = await authenticatedFetch(apiUrl(path), {
    headers: etag === undefined ? {} : {'If-None-Match': etag},
    ...(signal === undefined ? {} : {signal}),
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
  signal?: AbortSignal,
): Promise<ConditionalResult<SessionSnapshot[]>> {
  return fetchConditional('/sessions', etag, value => {
    if (!isRecord(value) || !Array.isArray(value.sessions)) {
      throw new Error('the server returned invalid sessions');
    }
    return value.sessions as SessionSnapshot[];
  }, signal);
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
    signal?: AbortSignal;
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
    options.signal,
  );
}

export async function updateSessionProfileModelDefault(
  sessionRef: string,
  modelDefault: string,
  profile?: string,
): Promise<SessionPathModelDefaultUpdate> {
  const response = await authenticatedFetch(
    apiUrl(`/sessions/${encodeURIComponent(sessionRef)}/model-default${profileQuery(profile)}`),
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
    !Array.isArray(value.profiles) ||
    !value.profiles.every(profile => typeof profile === 'string') ||
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

/**
 * The history block a snapshot referred to.
 *
 * Turns a compaction removed, folded from the backup, are static until the next
 * compaction — so they are fetched once per identity instead of riding on every
 * snapshot a committed step produces. A `304` means the held copy is still the
 * one the server would send.
 */
export async function fetchSessionHistory(
  sessionId: string,
  etag?: string,
  profile?: string,
  signal?: AbortSignal,
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
    signal,
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
  const response = await authenticatedFetch(
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

export type RootLibraryKind = 'prompt' | 'template' | 'skill';

export interface RootLibraryAssetSummary {
  id: string;
  kind: RootLibraryKind;
  name: string;
  description: string;
  source: string;
}

export interface RootLibraryAsset extends RootLibraryAssetSummary {
  content: string;
}

export interface RootLibraryCatalog {
  prompts: RootLibraryAssetSummary[];
  templates: RootLibraryAssetSummary[];
  skills: RootLibraryAssetSummary[];
}

function isRootLibraryAssetSummary(
  value: unknown,
  kind: RootLibraryKind,
): value is RootLibraryAssetSummary {
  return isRecord(value) &&
    typeof value.id === 'string' &&
    value.kind === kind &&
    typeof value.name === 'string' &&
    typeof value.description === 'string' &&
    typeof value.source === 'string';
}

export async function fetchRootLibrary(signal?: AbortSignal): Promise<RootLibraryCatalog> {
  const response = await authenticatedFetch(apiUrl('/library'), {signal, cache: 'no-store'});
  if (!response.ok) throw new Error(await readError(response));
  const value: unknown = await response.json();
  if (!isRecord(value)) throw new Error('the server returned invalid library data');
  const prompts = value.prompts;
  const templates = value.templates;
  const skills = value.skills;
  if (
    !Array.isArray(prompts) || !prompts.every(item => isRootLibraryAssetSummary(item, 'prompt')) ||
    !Array.isArray(templates) || !templates.every(item => isRootLibraryAssetSummary(item, 'template')) ||
    !Array.isArray(skills) || !skills.every(item => isRootLibraryAssetSummary(item, 'skill'))
  ) {
    throw new Error('the server returned invalid library data');
  }
  return {prompts, templates, skills};
}

export async function fetchRootLibraryAsset(
  kind: RootLibraryKind,
  id: string,
  signal?: AbortSignal,
): Promise<RootLibraryAsset> {
  const response = await authenticatedFetch(
    apiUrl(`/library/${encodeURIComponent(kind)}/${encodeURIComponent(id)}`),
    {signal, cache: 'no-store'},
  );
  if (!response.ok) throw new Error(await readError(response));
  const value: unknown = await response.json();
  if (!isRecord(value) || !isRecord(value.asset)) {
    throw new Error('the server returned invalid library item data');
  }
  const asset = value.asset;
  if (
    !isRootLibraryAssetSummary(asset, kind) ||
    typeof asset.content !== 'string'
  ) {
    throw new Error('the server returned invalid library item data');
  }
  return {
    id: asset.id,
    kind: asset.kind,
    name: asset.name,
    description: asset.description,
    source: asset.source,
    content: asset.content,
  };
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
  const response = await authenticatedFetch(
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

/** The shape of a stored image file name: 64 lowercase hex digits and a known extension. */
export const STORED_IMAGE_NAME = /^[0-9a-f]{64}\.(png|jpg|webp|gif)$/;

/**
 * One image the server stored for a tool result. The route needs the API token,
 * so the bytes are fetched here and the caller shows them from an object URL.
 */
export async function fetchSessionImage(
  sessionId: string,
  name: string,
  profile?: string,
  signal?: AbortSignal,
): Promise<Blob> {
  if (!STORED_IMAGE_NAME.test(name)) {
    throw new Error('invalid stored image name');
  }
  const response = await authenticatedFetch(
    apiUrl(`/sessions/${encodeURIComponent(sessionId)}/images/${name}${profileQuery(profile)}`),
    signal === undefined ? {} : {signal},
  );
  if (!response.ok) throw new Error(await readError(response));
  return response.blob();
}

/**
 * One image a tool result still holds inline in the transcript. `cursor` is the turn of
 * the message that carries the result (a message of the history block has none). The
 * server decodes the image on demand, so it is never part of the message itself.
 */
export async function fetchMessageImage(
  sessionId: string,
  messageId: string,
  callId: string,
  index: number,
  cursor?: number,
  profile?: string,
  signal?: AbortSignal,
): Promise<Blob> {
  if (!Number.isInteger(index) || index < 0) {
    throw new Error('invalid image position');
  }
  const query = new URLSearchParams();
  if (cursor !== undefined) {
    query.set('cursor', String(cursor));
  }
  if (profile !== undefined && profile.length > 0) {
    query.set('profile', profile);
  }
  const suffix = query.size === 0 ? '' : `?${query.toString()}`;
  const response = await authenticatedFetch(
    apiUrl(
      `/sessions/${encodeURIComponent(sessionId)}/messages/${encodeURIComponent(messageId)}/images/${encodeURIComponent(callId)}/${index}${suffix}`,
    ),
    signal === undefined ? {} : {signal},
  );
  if (!response.ok) throw new Error(await readError(response));
  return response.blob();
}

/**
 * One image of a tool result of the run that is still going. The result is not in the
 * transcript yet, so it is addressed by call id and position, not by message.
 */
export async function fetchLiveRunImage(
  sessionId: string,
  callId: string,
  index: number,
  profile?: string,
  signal?: AbortSignal,
): Promise<Blob> {
  if (!Number.isInteger(index) || index < 0) {
    throw new Error('invalid image position');
  }
  const response = await authenticatedFetch(
    apiUrl(
      `/sessions/${encodeURIComponent(sessionId)}/run/images/${encodeURIComponent(callId)}/${index}${profileQuery(profile)}`,
    ),
    signal === undefined ? {} : {signal},
  );
  if (!response.ok) throw new Error(await readError(response));
  return response.blob();
}

export async function fetchSessionTools(sessionId: string, profile?: string): Promise<ToolDefinition[]> {  const response = await authenticatedFetch(apiUrl(`/sessions/${encodeURIComponent(sessionId)}/tools${profileQuery(profile)}`));
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
  usage?: SessionUsageSummary;
}

export async function compactSession(
  sessionId: string,
  profile?: string,
): Promise<SessionCompactionResult> {
  const response = await authenticatedFetch(
    apiUrl(`/sessions/${encodeURIComponent(sessionId)}/compact${profileQuery(profile)}`),
    {method: 'POST'},
  );
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
    compaction.retained_items < 0 ||
    (value.usage !== undefined && !isSessionUsageSummary(value.usage))
  ) {
    throw new Error('the server returned invalid compaction data');
  }
  return {
    compacted: compaction.compacted,
    compacted_items: compaction.compacted_items,
    retained_items: compaction.retained_items,
    ...(value.usage === undefined ? {} : {usage: value.usage}),
  };
}

export interface SessionStateContextResult {
  written: boolean;
  preview: boolean;
  role?: string;
  text?: string;
  bytes?: number;
}

/**
 * Fires the session's `user` state-context trigger. With `preview` the server
 * renders the block and returns it without storing anything.
 */
export async function writeSessionStateContext(
  sessionId: string,
  body: {prompt: string; preview: boolean},
  profile?: string,
): Promise<SessionStateContextResult> {
  const response = await authenticatedFetch(
    apiUrl(`/sessions/${encodeURIComponent(sessionId)}/statecontext${profileQuery(profile)}`),
    {
      method: 'POST',
      headers: {'content-type': 'application/json'},
      body: JSON.stringify(body),
    },
  );
  if (!response.ok) {
    throw new Error(await readError(response));
  }
  const value: unknown = await response.json();
  const result = isRecord(value) ? value.statecontext : undefined;
  if (
    !isRecord(result) ||
    typeof result.written !== 'boolean' ||
    (result.preview !== undefined && typeof result.preview !== 'boolean') ||
    (result.role !== undefined && typeof result.role !== 'string') ||
    (result.text !== undefined && typeof result.text !== 'string') ||
    (result.bytes !== undefined &&
      (typeof result.bytes !== 'number' || !Number.isSafeInteger(result.bytes) || result.bytes < 0))
  ) {
    throw new Error('the server returned invalid state context data');
  }
  return {
    written: result.written,
    preview: result.preview === true,
    ...(result.role === undefined ? {} : {role: result.role}),
    ...(result.text === undefined ? {} : {text: result.text}),
    ...(result.bytes === undefined ? {} : {bytes: result.bytes}),
  };
}

export interface FrontCompactionResult {
  delete_mode: 'permanent' | 'recoverable';
  archive_name?: string;
}

export async function frontCompactSessionFromMessage(
  sessionId: string,
  messageId: string,
  profile?: string,
): Promise<FrontCompactionResult> {
  const response = await authenticatedFetch(
    apiUrl(`/sessions/${encodeURIComponent(sessionId)}/front-compact${profileQuery(profile)}`),
    {
      method: 'POST',
      headers: {'content-type': 'application/json'},
      body: JSON.stringify({message_id: messageId}),
    },
  );
  if (!response.ok) {
    throw new Error(await readError(response));
  }
  const value: unknown = await response.json();
  if (!isRecord(value) || !isRecord(value.front_compaction)) {
    throw new Error('the server returned invalid front compaction data');
  }
  const frontCompaction = value.front_compaction;
  if (
    (frontCompaction.delete_mode !== 'permanent' && frontCompaction.delete_mode !== 'recoverable') ||
    (frontCompaction.archive_name !== undefined && typeof frontCompaction.archive_name !== 'string')
  ) {
    throw new Error('the server returned invalid front compaction data');
  }
  return {
    delete_mode: frontCompaction.delete_mode,
    ...(typeof frontCompaction.archive_name === 'string'
      ? {archive_name: frontCompaction.archive_name}
      : {}),
  };
}

export async function resetSession(sessionId: string, profile?: string): Promise<void> {
  const response = await authenticatedFetch(
    apiUrl(`/sessions/${encodeURIComponent(sessionId)}/reset${profileQuery(profile)}`),
    {method: 'POST'},
  );
  if (!response.ok) {
    throw new Error(await readError(response));
  }
  const value: unknown = await response.json();
  if (!isRecord(value) || value.reset !== true) {
    throw new Error('the server returned invalid reset data');
  }
}

/** The session's checkpoints, newest first, and whether the session keeps any. */
export async function fetchSessionCheckpoints(
  sessionId: string,
  profile?: string,
  signal?: AbortSignal,
): Promise<CheckpointList> {
  const response = await authenticatedFetch(
    apiUrl(`/sessions/${encodeURIComponent(sessionId)}/checkpoints${profileQuery(profile)}`),
    signal === undefined ? undefined : {signal},
  );
  if (!response.ok) {
    throw new Error(await readError(response));
  }
  return parseCheckpointList(await response.json());
}

/**
 * Asks what a restore would do (`dryRun`) or does it. The server refuses while a
 * run is active (409) and answers a checkpoint with no saved state the same way.
 */
export async function restoreSessionCheckpoint(
  sessionId: string,
  checkpointId: string,
  options: {dryRun: boolean; mode: RestoreMode},
  profile?: string,
): Promise<RestorePlan> {
  const response = await authenticatedFetch(
    apiUrl(
      `/sessions/${encodeURIComponent(sessionId)}/checkpoints/${encodeURIComponent(checkpointId)}/restore${profileQuery(profile)}`,
    ),
    {
      method: 'POST',
      headers: {'content-type': 'application/json'},
      body: JSON.stringify({dry_run: options.dryRun, mode: options.mode}),
    },
  );
  if (!response.ok) {
    throw new Error(await readError(response));
  }
  return parseRestorePlan(await response.json());
}

class RunStreamConnectionError extends Error {
  public constructor(message: string) {
    super(message);
    this.name = 'RunStreamConnectionError';
  }
}

async function* readSse(
  response: Response,
  signal?: AbortSignal,
  stallTimeoutMs?: number,
): AsyncGenerator<SseMessage> {
  if (!response.body) {
    throw new Error('the server did not return a stream');
  }

  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  let pieces: string[] = [];
  let lastChar = '';
  let pendingReturn = '';

  /**
   * A socket that stops delivering frames — a sleeping laptop, a dropped
   * connection — neither errors nor ends, so reading would wait forever.
   * Only *silence* counts: a slow provider that keeps sending deltas resets it
   * with every chunk.
   */
  const readChunk = async (): Promise<ReadableStreamReadResult<Uint8Array>> => {
    if (stallTimeoutMs === undefined || stallTimeoutMs <= 0) {
      return reader.read();
    }
    let timer: ReturnType<typeof setTimeout> | undefined;
    try {
      return await Promise.race([
        reader.read(),
        new Promise<never>((_resolve, reject) => {
          timer = setTimeout(
            () => reject(new RunStreamConnectionError('the run stream went silent')),
            stallTimeoutMs,
          );
        }),
      ]);
    } finally {
      if (timer !== undefined) {
        clearTimeout(timer);
      }
    }
  };

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
      throw new RunStreamConnectionError('the server returned invalid stream data');
    }
  };

  try {
    while (true) {
      let chunk: ReadableStreamReadResult<Uint8Array>;
      try {
        chunk = await readChunk();
      } catch (error) {
        if (signal?.aborted) {
          throw error;
        }
        if (error instanceof RunStreamConnectionError) {
          throw error;
        }
        throw new RunStreamConnectionError('the run stream connection was interrupted');
      }
      // The frame is kept as pieces and only the new text is searched, so one
      // large frame split into many chunks stays linear: concatenating onto one
      // string and searching it would copy the whole buffer for every chunk.
      // A CRLF pair can be split across chunks, so a trailing `\r` is held back
      // until the next chunk shows whether `\n` follows; a `\n\n` can straddle
      // the previous end too, which `lastChar` covers.
      let incoming = pendingReturn + decoder.decode(chunk.value, {stream: !chunk.done});
      pendingReturn = '';
      if (!chunk.done && incoming.endsWith('\r')) {
        pendingReturn = '\r';
        incoming = incoming.slice(0, -1);
      }
      const text = incoming.replace(/\r\n/g, '\n');
      let offset = 0;
      while (true) {
        let block: string;
        if (offset === 0 && lastChar === '\n' && text.startsWith('\n')) {
          block = pieces.join('').slice(0, -1);
          offset = 1;
        } else {
          const separator = text.indexOf('\n\n', offset);
          if (separator < 0) {
            break;
          }
          block = pieces.join('') + text.slice(offset, separator);
          offset = separator + 2;
        }
        pieces = [];
        lastChar = '';
        const message = parseBlock(block);
        if (message) {
          yield message;
        }
      }
      if (offset < text.length) {
        pieces.push(offset === 0 ? text : text.slice(offset));
        lastChar = text[text.length - 1];
      }
      if (chunk.done) {
        const message = parseBlock(pieces.join('').trim());
        if (message) {
          yield message;
        }
        return;
      }
    }
  } finally {
    try {
      await reader.cancel();
    } catch {
      // The connection may already be closed or aborted.
    }
    reader.releaseLock();
  }
}

function isWebUiCitation(value: unknown): value is WebUiCitation {
  return (
    isRecord(value) &&
    typeof value.url === 'string' &&
    (value.title === undefined || typeof value.title === 'string')
  );
}

function isWebUiPart(value: unknown): value is WebUiPart {
  if (!isRecord(value) || typeof value.type !== 'string') {
    return false;
  }
  if (value.type === 'reasoning') {
    return typeof value.text === 'string';
  }
  if (value.type === 'text') {
    return (
      typeof value.text === 'string' &&
      (value.citations === undefined ||
        (Array.isArray(value.citations) && value.citations.every(isWebUiCitation))) &&
      (value.images === undefined ||
        (Array.isArray(value.images) && value.images.every(name => typeof name === 'string'))) &&
      (value.files === undefined ||
        (Array.isArray(value.files) && value.files.every(name => typeof name === 'string')))
    );
  }
  if (value.type === 'hosted_tool') {
    return (
      typeof value.callId === 'string' &&
      typeof value.name === 'string' &&
      (value.status === undefined || typeof value.status === 'string') &&
      (value.target === undefined || typeof value.target === 'string') &&
      (value.raw === undefined || typeof value.raw === 'string') &&
      (value.images === undefined ||
        (Array.isArray(value.images) && value.images.every(name => typeof name === 'string')))
    );
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
      (value.status === 'success' || value.status === 'error' || value.status === 'unknown') &&
      (value.images === undefined ||
        (Array.isArray(value.images) && value.images.every(name => typeof name === 'string'))) &&
      (value.inlineImages === undefined ||
        (typeof value.inlineImages === 'number' &&
          Number.isInteger(value.inlineImages) &&
          value.inlineImages >= 0))
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

function isWebUiInteraction(value: unknown): value is WebUiInteraction {
  return (
    isRecord(value) &&
    typeof value.id === 'string' &&
    typeof value.tool === 'string' &&
    typeof value.tool_call_id === 'string' &&
    isRecord(value.draft) &&
    (value.kind === undefined || value.kind === 'draft' || value.kind === 'approval') &&
    (value.title === undefined || typeof value.title === 'string') &&
    typeof value.created_at === 'string' &&
    typeof value.expires_at === 'string' &&
    typeof value.max_expires_at === 'string' &&
    isNonnegativeSafeInteger(value.extend_ms)
  );
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
  /** A pending user decision for this run; `undefined` clears the interaction. */
  onInteraction?: (turnId: string, interaction: WebUiInteraction | undefined) => void;
  /**
   * Why a question left the run: sent once, with the event that clears it, and only
   * when the server says (an older server never does). It is not replayed to a client
   * that attaches later.
   */
  onInteractionEnd?: (turnId: string, end: InteractionEnd) => void;
  /**
   * The run is waiting to retry a failed provider request; `undefined` clears the
   * notice (the wait ended, or the event was malformed, so a bad one never sticks).
   */
  onProviderRetry?: (turnId: string, wait: ProviderRetryWait | undefined) => void;
  onHookReport?: (turnId: string, report: HookReport) => void;
  onIdle?: () => void;
  /**
   * How long the run response or event stream may stay silent before it counts
   * as broken and is reconnected. Tests set this small; the app takes
   * `run_stream_stall_timeout_ms`.
   */
  stallTimeoutMs?: number;
  /**
   * The run stream dropped and the client is trying to restore it. Called with the
   * state before each attempt (`offline` while it waits for the network to return) and
   * with `undefined` once the stream is back or the reconnect has ended.
   */
  onReconnecting?: (state: RunStreamReconnectState | undefined) => void;
}

export interface RunStreamReconnectState {
  /** Reconnect attempts made so far; 0 before the first. */
  attempt: number;
  /** The browser reports no network; the next attempt waits for it to return. */
  offline: boolean;
}

export interface StreamSessionRunResult {
  message?: WebUiMessage;
  usage?: SessionUsageSummary;
  /** Whether the server reported a run for this session (attach path only). */
  attached?: boolean;
  /**
   * The run was known active but the server no longer holds it, so its outcome
   * is unknown to this stream. It is not a failure: the committed transcript
   * holds the result, and the caller should reload that.
   */
  idle?: boolean;
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
 * `ui.message` completion is message-level only; a run can still be waiting on
 * a user decision. Only run terminal events finish the stream.
 */
interface ConsumedRunStream {
  result: StreamSessionRunResult;
  finished: boolean;
  idle: boolean;
}

async function consumeRunStream(
  response: Response,
  options: StreamSessionRunOptions,
): Promise<ConsumedRunStream> {
  let completedMessage: WebUiMessage | undefined;
  let lastCompleteMessage: WebUiMessage | undefined;
  let sessionUsage: SessionUsageSummary | undefined;
  let attached = false;
  let finished = false;
  let runCompleted = false;
  let idle = false;
  let completionNotified = false;
  const stallTimeoutMs = options.stallTimeoutMs ?? appConfig.run_stream_stall_timeout_ms;

  const rememberCompletion = (message: WebUiMessage): void => {
    completedMessage = message;
    if (!completionNotified) {
      completionNotified = true;
      options.onCompleted?.(message);
    }
  };

  try {
    for await (const message of readSse(response, options.signal, stallTimeoutMs)) {
      if (message.event === 'run') {
        if (isSessionRunState(message.data)) {
          attached = true;
          options.onRun?.(message.data);
        }
        continue;
      }
      if (message.event === 'interaction') {
        if (
          isRecord(message.data) &&
          typeof message.data.turn_id === 'string' &&
          (message.data.pending === null || isWebUiInteraction(message.data.pending))
        ) {
          options.onInteraction?.(
            message.data.turn_id,
            message.data.pending === null ? undefined : message.data.pending,
          );
          if (message.data.pending === null) {
            const end = parseInteractionEnd(message.data.ended);
            if (end !== undefined) {
              options.onInteractionEnd?.(message.data.turn_id, end);
            }
          }
        }
        continue;
      }
      if (message.event === 'hook_report') {
        const report = parseHookReport(message.data);
        if (report !== undefined) {
          options.onHookReport?.(report.turnId, report.report);
        }
        continue;
      }
      if (message.event === 'provider_retry') {
        if (isRecord(message.data) && typeof message.data.turn_id === 'string') {
          options.onProviderRetry?.(message.data.turn_id, parseProviderRetryWait(message.data.waiting));
        }
        continue;
      }
      if (message.event === 'ui.message') {
        if (isRecord(message.data) && isWebUiMessage(message.data.message)) {
          options.onMessage?.(message.data.message);
          if (message.data.message.status === 'complete') {
            lastCompleteMessage = message.data.message;
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
          runCompleted = true;
          rememberCompletion(message.data.message);
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
        idle = true;
        options.onIdle?.();
        continue;
      }
      if (message.event === 'error') {
        throw new Error(errorText(message.data));
      }
      if (message.event === 'done') {
        if (completedMessage === undefined && lastCompleteMessage !== undefined) {
          rememberCompletion(lastCompleteMessage);
        }
        finished = true;
        break;
      }
    }
  } catch (error) {
    if (!(error instanceof RunStreamConnectionError) || !runCompleted) {
      throw error;
    }
    finished = true;
  }

  if (runCompleted) {
    finished = true;
  }
  return {
    result: {
      ...(completedMessage === undefined ? {} : {message: completedMessage}),
      ...(sessionUsage === undefined ? {} : {usage: sessionUsage}),
      ...(attached ? {attached: true} : {}),
    },
    finished,
    idle,
  };
}

function mergeRunStreamResults(
  previous: StreamSessionRunResult,
  next: StreamSessionRunResult,
): StreamSessionRunResult {
  const message = next.message ?? previous.message;
  const usage = next.usage ?? previous.usage;
  const attached = next.attached === true || previous.attached === true;
  return {
    ...(message === undefined ? {} : {message}),
    ...(usage === undefined ? {} : {usage}),
    ...(attached ? {attached: true} : {}),
  };
}

function reconnectDelayMs(attempt: number): number {
  return Math.min(
    appConfig.run_stream_reconnect_base_delay_ms * 2 ** attempt,
    appConfig.run_stream_reconnect_max_delay_ms,
  );
}

function waitForRunStreamReconnect(delayMs: number, signal?: AbortSignal): Promise<void> {
  return new Promise<void>((resolve, reject) => {
    let timer: ReturnType<typeof setTimeout> | undefined;
    const cleanup = (): void => {
      if (timer !== undefined) {
        clearTimeout(timer);
      }
      signal?.removeEventListener('abort', onAbort);
    };
    const onAbort = (): void => {
      cleanup();
      reject(new Error('the run stream was aborted'));
    };
    if (signal?.aborted) {
      onAbort();
      return;
    }
    signal?.addEventListener('abort', onAbort, {once: true});
    timer = setTimeout(() => {
      cleanup();
      resolve();
    }, delayMs);
  });
}

function browserIsOffline(): boolean {
  return typeof navigator !== 'undefined' && navigator.onLine === false;
}

function tabIsHidden(): boolean {
  return typeof document !== 'undefined' && document.visibilityState === 'hidden';
}

/**
 * Resolves when `blocked()` is false, when `eventName` fires on `target` (the page says the
 * condition changed), or after `recheckMs` so a wrong report (a "work offline" switch, some
 * VPN adapters) cannot park the run forever; rejects when the caller aborts.
 */
function waitWhileBlocked(
  target: EventTarget | undefined,
  eventName: string,
  blocked: () => boolean,
  recheckMs: number,
  signal?: AbortSignal,
): Promise<void> {
  return new Promise<void>((resolve, reject) => {
    let timer: ReturnType<typeof setTimeout> | undefined;
    const cleanup = (): void => {
      if (timer !== undefined) {
        clearTimeout(timer);
      }
      target?.removeEventListener?.(eventName, onChange);
      signal?.removeEventListener('abort', onAbort);
    };
    const onChange = (): void => {
      cleanup();
      resolve();
    };
    const onAbort = (): void => {
      cleanup();
      reject(new Error('the run stream was aborted'));
    };
    if (signal?.aborted) {
      onAbort();
      return;
    }
    if (!blocked()) {
      resolve();
      return;
    }
    target?.addEventListener?.(eventName, onChange);
    signal?.addEventListener('abort', onAbort, {once: true});
    if (Number.isFinite(recheckMs) && recheckMs > 0) {
      timer = setTimeout(onChange, recheckMs);
    }
  });
}

function waitForBrowserOnline(recheckMs: number, signal?: AbortSignal): Promise<void> {
  return waitWhileBlocked(globalThis as EventTarget, 'online', browserIsOffline, recheckMs, signal);
}

// A hidden tab keeps retrying, only less often: the run's completion still reaches the
// notifications, and the tab catches up at once when it is shown again.
function waitForTabVisible(recheckMs: number, signal?: AbortSignal): Promise<void> {
  return waitWhileBlocked(
    typeof document === 'undefined' ? undefined : document,
    'visibilitychange',
    tabIsHidden,
    recheckMs,
    signal,
  );
}

async function fetchRunStreamResponse(
  path: string,
  init: RequestInit,
  options: StreamSessionRunOptions,
): Promise<Response> {
  const timeoutMs = options.stallTimeoutMs ?? appConfig.run_stream_stall_timeout_ms;
  if (!Number.isFinite(timeoutMs) || timeoutMs <= 0) {
    return authenticatedFetch(apiUrl(path), {
      ...init,
      ...(options.signal === undefined ? {} : {signal: options.signal}),
    });
  }

  const controller = new AbortController();
  let timedOut = false;
  const relayAbort = (): void => controller.abort(options.signal?.reason);
  if (options.signal?.aborted) {
    relayAbort();
  } else {
    options.signal?.addEventListener('abort', relayAbort, {once: true});
  }
  const timer = setTimeout(() => {
    timedOut = true;
    controller.abort();
  }, timeoutMs);

  try {
    const response = await authenticatedFetch(apiUrl(path), {
      ...init,
      signal: controller.signal,
    });
    if (timedOut) {
      void response.body?.cancel().catch(() => undefined);
      throw new RunStreamConnectionError('the run stream response did not arrive in time');
    }
    return response;
  } catch (error) {
    if (options.signal?.aborted) {
      throw error;
    }
    if (timedOut) {
      throw new RunStreamConnectionError('the run stream response did not arrive in time');
    }
    throw error;
  } finally {
    clearTimeout(timer);
    options.signal?.removeEventListener('abort', relayAbort);
  }
}

function isRetryableRunStreamStatus(status: number): boolean {
  return status === 408 || status >= 500;
}

async function fetchAttachedRunStream(
  sessionRef: string,
  options: StreamSessionRunOptions,
): Promise<Response> {
  let response: Response;
  try {
    response = await fetchRunStreamResponse(
      `/sessions/${encodeURIComponent(sessionRef)}/run/stream${profileQuery(options.profile)}`,
      {},
      options,
    );
  } catch (error) {
    if (options.signal?.aborted) {
      throw error;
    }
    throw new RunStreamConnectionError('the run stream connection was interrupted');
  }
  if (!response.ok) {
    const error = await readError(response);
    if (isRetryableRunStreamStatus(response.status)) {
      throw new RunStreamConnectionError(error);
    }
    throw new Error(error);
  }
  return response;
}

async function reconnectRunStream(
  sessionRef: string,
  options: StreamSessionRunOptions,
  previous: StreamSessionRunResult,
  initialError?: Error,
): Promise<StreamSessionRunResult> {
  try {
    return await runStreamReconnectLoop(sessionRef, options, previous, initialError);
  } finally {
    options.onReconnecting?.(undefined);
  }
}

async function runStreamReconnectLoop(
  sessionRef: string,
  options: StreamSessionRunOptions,
  previous: StreamSessionRunResult,
  initialError?: Error,
): Promise<StreamSessionRunResult> {
  let result = previous;
  let lastConnectionError = initialError;
  let attached = previous.attached === true;
  let attempt = 0;
  const trackedOptions: StreamSessionRunOptions = {
    ...options,
    onRun: run => {
      attached = true;
      options.onRun?.(run);
    },
  };

  // Once the server has identified this run, retry until it finishes or the
  // caller detaches. A bounded batch alone leaves an active run looking stuck
  // after every connection in that batch is interrupted.
  while (attempt < appConfig.run_stream_reconnect_max_attempts || attached) {
    options.onReconnecting?.({attempt, offline: browserIsOffline()});
    await waitForRunStreamReconnect(reconnectDelayMs(attempt), options.signal);
    if (browserIsOffline()) {
      // A dead network only burns attempts; wait for it instead of retrying into it.
      options.onReconnecting?.({attempt, offline: true});
      await waitForBrowserOnline(appConfig.run_stream_offline_recheck_ms, options.signal);
    }
    if (tabIsHidden()) {
      await waitForTabVisible(appConfig.run_stream_hidden_recheck_ms, options.signal);
    }
    attempt += 1;
    options.onReconnecting?.({attempt, offline: false});
    let response: Response;
    try {
      response = await fetchAttachedRunStream(sessionRef, trackedOptions);
    } catch (error) {
      if (options.signal?.aborted) {
        throw error;
      }
      if (!(error instanceof RunStreamConnectionError)) {
        throw error;
      }
      lastConnectionError = error;
      continue;
    }

    options.onReconnecting?.(undefined);
    let consumed: ConsumedRunStream;
    try {
      consumed = await consumeRunStream(response, trackedOptions);
    } catch (error) {
      if (options.signal?.aborted) {
        throw error;
      }
      if (!(error instanceof RunStreamConnectionError)) {
        throw error;
      }
      lastConnectionError = error;
      continue;
    }

    result = mergeRunStreamResults(result, consumed.result);
    attached ||= result.attached === true;
    if (consumed.finished) {
      return result;
    }
    if (consumed.idle) {
      // The run ended while the connection was down and the server has already
      // dropped its retained state. Reporting a failure here would be wrong: the
      // run may have succeeded, and the transcript holds the answer.
      return {...result, idle: true};
    }
    lastConnectionError = undefined;
  }

  if (lastConnectionError instanceof Error) {
    throw new Error(`the run stream could not be restored: ${lastConnectionError.message}`);
  }
  throw new Error('the run stream ended before completion and could not be restored');
}

export async function streamSessionRun(
  sessionId: string,
  input: string | RunInputPart[],
  options: StreamSessionRunOptions = {},
): Promise<StreamSessionRunResult> {
  let attached = false;
  const runOptions: StreamSessionRunOptions = {
    ...options,
    onRun: run => {
      attached = true;
      options.onRun?.(run);
    },
  };
  let response: Response;
  try {
    response = await fetchRunStreamResponse(
      `/sessions/${encodeURIComponent(sessionId)}/run${profileQuery(options.profile)}`,
      {
        method: 'POST',
        headers: {'Content-Type': 'application/json'},
        body: JSON.stringify({
          input,
          supports_interaction: true,
          ...(options.model === undefined ? {} : {model: options.model}),
        }),
      },
      runOptions,
    );
  } catch (error) {
    if (options.signal?.aborted) {
      throw error;
    }
    const connectionError = new RunStreamConnectionError('the run stream connection was interrupted');
    return reconnectRunStream(
      sessionId,
      runOptions,
      attached ? {attached: true} : {},
      connectionError,
    );
  }
  if (!response.ok) {
    const error = await readError(response);
    if (!isRetryableRunStreamStatus(response.status)) {
      throw new Error(error);
    }
    return reconnectRunStream(
      sessionId,
      runOptions,
      attached ? {attached: true} : {},
      new RunStreamConnectionError(error),
    );
  }

  let consumed: ConsumedRunStream;
  try {
    consumed = await consumeRunStream(response, runOptions);
  } catch (error) {
    if (!(error instanceof RunStreamConnectionError) || options.signal?.aborted) {
      throw error;
    }
    return reconnectRunStream(
      sessionId,
      runOptions,
      attached ? {attached: true} : {},
      error,
    );
  }
  if (consumed.finished) {
    return consumed.result;
  }
  if (consumed.idle) {
    throw new Error('the run stream ended before the run started');
  }
  return reconnectRunStream(sessionId, runOptions, consumed.result);
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
  let attached = false;
  const runOptions: StreamSessionRunOptions = {
    ...options,
    onRun: run => {
      attached = true;
      options.onRun?.(run);
    },
  };
  let response: Response;
  try {
    response = await fetchAttachedRunStream(sessionRef, runOptions);
  } catch (error) {
    if (!(error instanceof RunStreamConnectionError) || options.signal?.aborted) {
      throw error;
    }
    return reconnectRunStream(
      sessionRef,
      runOptions,
      attached ? {attached: true} : {},
      error,
    );
  }

  let consumed: ConsumedRunStream;
  try {
    consumed = await consumeRunStream(response, runOptions);
  } catch (error) {
    if (!(error instanceof RunStreamConnectionError) || options.signal?.aborted) {
      throw error;
    }
    return reconnectRunStream(
      sessionRef,
      runOptions,
      attached ? {attached: true} : {},
      error,
    );
  }
  if (consumed.finished || consumed.idle) {
    return consumed.result;
  }
  return reconnectRunStream(sessionRef, runOptions, consumed.result);
}

export interface AnswerSessionRunInteractionResult {
  interaction_id: string;
  decision: WebUiInteractionDecision;
  status: 'answered';
}

/**
 * Answers the question a run is waiting on.
 *
 * The reply names the question it answers: a stale tab, a repeated click, or an
 * answer sent after the deadline all come back as a refusal instead of answering
 * whatever question happens to be pending.
 */
export async function answerSessionRunInteraction(
  sessionRef: string,
  body: {
    interaction_id: string;
    decision: WebUiInteractionDecision;
    draft?: unknown;
    reason?: string;
  },
  profile?: string,
  signal?: AbortSignal,
): Promise<AnswerSessionRunInteractionResult> {
  const response = await authenticatedFetch(
    apiUrl(`/sessions/${encodeURIComponent(sessionRef)}/run/interaction${profileQuery(profile)}`),
    {
      method: 'POST',
      headers: {'content-type': 'application/json'},
      body: JSON.stringify(body),
      ...(signal === undefined ? {} : {signal}),
    },
  );
  if (!response.ok) {
    throw new Error(await readError(response));
  }
  const value: unknown = await response.json();
  const interaction = isRecord(value) ? value.interaction : undefined;
  if (
    !isRecord(interaction) ||
    typeof interaction.interaction_id !== 'string' ||
    (interaction.decision !== 'accept' && interaction.decision !== 'update' && interaction.decision !== 'decline')
  ) {
    throw new Error('the server returned invalid interaction data');
  }
  return {
    interaction_id: interaction.interaction_id,
    decision: interaction.decision,
    status: 'answered',
  };
}

export interface ExtendSessionRunInteractionResult {
  interaction_id: string;
  status: 'pending';
  expires_at: string;
  max_expires_at: string;
}

/**
 * Pushes the question's deadline out by the session's own step.
 *
 * The amount is not in the body: the step and the ceiling belong to the session,
 * not to the caller. The run republishes the question with its new deadline, so
 * this response and every open tab show the same time left.
 */
export async function extendSessionRunInteraction(
  sessionRef: string,
  body: {interaction_id: string},
  profile?: string,
  signal?: AbortSignal,
): Promise<ExtendSessionRunInteractionResult> {
  const response = await authenticatedFetch(
    apiUrl(`/sessions/${encodeURIComponent(sessionRef)}/run/interaction/extend${profileQuery(profile)}`),
    {
      method: 'POST',
      headers: {'content-type': 'application/json'},
      body: JSON.stringify(body),
      ...(signal === undefined ? {} : {signal}),
    },
  );
  if (!response.ok) {
    throw new Error(await readError(response));
  }
  const value: unknown = await response.json();
  const interaction = isRecord(value) ? value.interaction : undefined;
  if (
    !isRecord(interaction) ||
    typeof interaction.interaction_id !== 'string' ||
    interaction.status !== 'pending' ||
    typeof interaction.expires_at !== 'string' ||
    typeof interaction.max_expires_at !== 'string'
  ) {
    throw new Error('the server returned invalid interaction data');
  }
  return {
    interaction_id: interaction.interaction_id,
    status: 'pending',
    expires_at: interaction.expires_at,
    max_expires_at: interaction.max_expires_at,
  };
}

export interface StopSessionRunResult {
  stopped: boolean;
  turn_id?: string;
}

/** Stops the run in flight for a session; the run no longer commits a response. */
export async function stopSessionRun(
  sessionRef: string,
  profile?: string,
  signal?: AbortSignal,
): Promise<StopSessionRunResult> {
  const response = await authenticatedFetch(
    apiUrl(`/sessions/${encodeURIComponent(sessionRef)}/run/stop${profileQuery(profile)}`),
    {method: 'POST', ...(signal === undefined ? {} : {signal})},
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

export type CatalogKind = 'profiles' | 'models' | 'sessions' | 'toolsets' | 'mcp';

export interface CatalogFileInfo {
  id: string;
  etag: string;
  /** Display name the file sets; absent when unset. */
  name?: string;
  /** Folder a session file sets to group its listing; other kinds are grouped by the ID. */
  category?: string;
  /** The sessions whose scope selects this profile; set for profiles only. */
  sessions?: CatalogSessionRef[];
}

export interface CatalogSessionRef {
  id: string;
  name?: string;
  category?: string;
}

export interface CatalogFileContent extends CatalogFileInfo {
  text: string;
}

/** A refused catalog request. `status` tells a stale file (412) from a file in use (409) or a bad body (422). */
export class CatalogRequestError extends Error {
  readonly status: number;

  constructor(status: number, message: string) {
    super(message);
    this.name = 'CatalogRequestError';
    this.status = status;
  }
}

function catalogFilePath(kind: CatalogKind, id: string): string {
  if (kind === 'sessions') {
    if (id === '' || id.includes('/')) {
      throw new Error(`"${id}" must be written <name> or <root>:<name>`);
    }
    return `/catalog/sessions/${encodeURIComponent(id)}`;
  }
  const [category, name, ...rest] = id.split('/');
  if (!category || !name || rest.length > 0) {
    throw new Error(`"${id}" must be written <category>/<name>`);
  }
  return `/catalog/${kind}/${encodeURIComponent(category)}/${encodeURIComponent(name)}`;
}

async function catalogFailure(response: Response): Promise<CatalogRequestError> {
  return new CatalogRequestError(response.status, await readError(response));
}

function isSessionRef(value: unknown): boolean {
  return (
    isRecord(value) &&
    typeof value.id === 'string' &&
    (value.name === undefined || typeof value.name === 'string') &&
    (value.category === undefined || typeof value.category === 'string')
  );
}

function toSessionRef(value: unknown): CatalogSessionRef {
  const ref = value as {id: string; name?: string; category?: string};
  return {
    id: ref.id,
    ...(typeof ref.name === 'string' && ref.name.trim() !== '' ? {name: ref.name.trim()} : {}),
    ...(typeof ref.category === 'string' && ref.category.trim() !== '' ? {category: ref.category.trim()} : {}),
  };
}

export async function fetchCatalogFiles(kind: CatalogKind, signal?: AbortSignal): Promise<CatalogFileInfo[]> {
  const response = await authenticatedFetch(apiUrl(`/catalog/${kind}`), signal === undefined ? {} : {signal});
  if (!response.ok) {
    throw await catalogFailure(response);
  }
  const value: unknown = await response.json();
  if (
    !isRecord(value) ||
    !Array.isArray(value.files) ||
    !value.files.every(
      file =>
        isRecord(file) &&
        typeof file.id === 'string' &&
        typeof file.etag === 'string' &&
        (file.name === undefined || typeof file.name === 'string') &&
        (file.category === undefined || typeof file.category === 'string') &&
        (file.sessions === undefined || (Array.isArray(file.sessions) && file.sessions.every(isSessionRef))),
    )
  ) {
    throw new Error('the server returned an invalid catalog list');
  }
  return value.files.map(file => ({
    id: file.id as string,
    etag: file.etag as string,
    ...(typeof file.name === 'string' && file.name.trim() !== '' ? {name: file.name.trim()} : {}),
    ...(typeof file.category === 'string' && file.category.trim() !== '' ? {category: file.category.trim()} : {}),
    ...(Array.isArray(file.sessions) && file.sessions.length > 0 ? {sessions: (file.sessions as unknown[]).map(toSessionRef)} : {}),
  }));
}

export async function fetchCatalogFile(
  kind: CatalogKind,
  id: string,
  signal?: AbortSignal,
): Promise<CatalogFileContent> {
  const response = await authenticatedFetch(apiUrl(catalogFilePath(kind, id)), signal === undefined ? {} : {signal});
  if (!response.ok) {
    throw await catalogFailure(response);
  }
  const etag = response.headers.get('etag');
  if (!etag) {
    throw new Error('the server returned a catalog file without a version');
  }
  return {id, etag, text: await response.text()};
}

/** `etag` replaces the file only if it still has that version; `null` creates it and fails if it exists. */
export async function saveCatalogFile(
  kind: CatalogKind,
  id: string,
  text: string,
  etag: string | null,
): Promise<{info: CatalogFileInfo; created: boolean}> {
  const response = await authenticatedFetch(apiUrl(catalogFilePath(kind, id)), {
    method: 'PUT',
    headers: etag === null ? {'If-None-Match': '*'} : {'If-Match': etag},
    body: text,
  });
  if (!response.ok) {
    throw await catalogFailure(response);
  }
  const value: unknown = await response.json();
  if (!isRecord(value) || typeof value.id !== 'string' || typeof value.etag !== 'string') {
    throw new Error('the server returned an invalid catalog write result');
  }
  return {info: {id: value.id, etag: value.etag}, created: response.status === 201};
}

/** `deleteState` also removes a session's transcripts and tool state; it only applies to sessions. */
export async function deleteCatalogFile(
  kind: CatalogKind,
  id: string,
  etag: string,
  options: {deleteState?: boolean} = {},
): Promise<void> {
  const query = kind === 'sessions' && options.deleteState === true ? '?state=delete' : '';
  const response = await authenticatedFetch(apiUrl(`${catalogFilePath(kind, id)}${query}`), {
    method: 'DELETE',
    headers: {'If-Match': etag},
  });
  if (!response.ok) {
    throw await catalogFailure(response);
  }
}
