import type {JsonRecord, SessionUsageSummary} from "../../persistence.js";

export type WebUiPartType = "reasoning" | "text" | "tool_call" | "tool_result";
export type WebUiMessageStatus = "streaming" | "complete";
export type WebUiToolResultStatus = "success" | "error" | "unknown";

/**
 * Present when a part's text was cut for transport: `bytes` is the full UTF-8
 * size of the original, and the message's `cursor` re-fetches it whole.
 */
export interface WebUiPartTruncation {
  bytes: number;
}

export interface WebUiTextPart {
  type: "reasoning" | "text";
  text: string;
  truncated?: WebUiPartTruncation;
}

export interface WebUiToolCallPart {
  type: "tool_call";
  callId: string;
  name: string;
  arguments: string;
  /**
   * True while the provider is still streaming the arguments for this call, so
   * a long `write`/`edit` call is visible before its item completes.
   */
  partial?: boolean;
  truncated?: WebUiPartTruncation;
}

export interface WebUiToolResultPart {
  type: "tool_result";
  callId: string;
  output: string;
  status: WebUiToolResultStatus;
  truncated?: WebUiPartTruncation;
}

export type WebUiPart = WebUiTextPart | WebUiToolCallPart | WebUiToolResultPart;

export interface WebUiMessageStep {
  id: string;
  /** Inclusive index into WebUiMessage.parts. */
  startPart: number;
  /** Exclusive index into WebUiMessage.parts. */
  endPart: number;
  /** Tool-producing steps are eligible for the earlier-step window. */
  hasTools: boolean;
}

/**
 * A tool call waiting on the user, published as run-level interaction state and
 * mirrored on `WebUiMessage.pending` for message-based clients.
 */
export interface WebUiInteraction {
  id: string;
  /** Name of the native tool that asked, e.g. `request_draft`. */
  tool: string;
  tool_call_id: string;
  /** The tool's proposal, shown to the user; any JSON object the tool defines. */
  draft: JsonRecord;
  title?: string;
  created_at: string;
  /** When the run stops waiting and reports the question as unanswered. */
  expires_at: string;
  /** The latest deadline an extension may push to; equals `expires_at` when extending is off. */
  max_expires_at: string;
  /** How much one extension adds to the deadline; zero when the session offers no more time. */
  extend_ms: number;
}

export interface WebUiMessage {
  id: string;
  sender: "system" | "user" | "assistant";
  text: string;
  timestamp: string;
  parts: WebUiPart[];
  /** Present when this message combines multiple provider response steps. */
  steps?: WebUiMessageStep[];
  status?: WebUiMessageStatus;
  reasoningContent?: string;
  /** Compatibility mirror of the run's pending interaction for message-based clients. */
  pending?: WebUiInteraction;
  /**
   * Turn that produced this message, set only when a part was capped: passing it
   * to the message endpoint returns the message with its parts whole.
   */
  cursor?: number;
}

export interface WebUiFrontCompactionSettings {
  enabled: boolean;
  delete_mode: "permanent" | "recoverable";
}

export interface WebUiSessionConfig {
  model: {
    provider: string;
    model: string;
  };
  pathProfiles?: string[];
  settings: {
    session_name: string;
    front_compaction?: WebUiFrontCompactionSettings;
    paths?: {
      active_profile?: string;
      default_profile?: string;
      model_default?: string;
      models?: string[];
    };
  };
}

export type WebUiSessionUsage = SessionUsageSummary;

export type WebUiRunStatus = "running" | "completed" | "failed" | "aborted";

/**
 * The run a client can attach to for a session: `running` while a turn is in
 * flight, then the terminal status for as long as the server retains it.
 */
export interface WebUiSessionRun {
  turn_id: string;
  started_at: string;
  status: WebUiRunStatus;
}

/**
 * Where the transcript of a snapshot stops: `has_more` reports that older turns
 * exist, and `next_before` is the cursor that fetches them.
 */
export interface WebUiSessionPage {
  has_more: boolean;
  next_before: number | null;
  generation: string;
  stale_cursor: boolean;
}

/**
 * Reference to the history block a snapshot left out.
 *
 * The block is the backup's tail — turns a compaction removed, which the
 * transcript no longer carries. It is static between compactions, so it is
 * served as its own resource: `etag` is what the block's copy was built from,
 * and `count` how many messages it holds. `etag` is `null` when there is no
 * history to show.
 */
export interface WebUiSessionHistory {
  etag: string | null;
  count: number;
}

export interface WebUiSessionSnapshot {
  id: string;
  session_id?: string;
  root_id?: string;
  root_label?: string;
  active_profile?: string;
  /**
   * The profile the session's `setting.json` stores as its default. It is what an
   * unqualified request resolves to, and what a client names when switching.
   */
  default_profile?: string;
  path_profiles?: string[];
  model_default?: string;
  models?: string[];
  front_compaction?: WebUiFrontCompactionSettings;
  session_name: string;
  turns: number;
  messages: WebUiMessage[];
  model: {
    provider: string;
    model: string;
  };
  usage?: WebUiSessionUsage;
  run?: WebUiSessionRun;
  /**
   * The run of every profile that has one, keyed by profile name — omitted for a
   * session that configures no profiles. `run` is the stored default's; the rest
   * let a client show a status per profile without opening the session.
   */
  runs_by_profile?: Record<string, WebUiSessionRun>;
  page?: WebUiSessionPage;
  history?: WebUiSessionHistory;
}
