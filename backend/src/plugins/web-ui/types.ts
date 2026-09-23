import type {SessionUsageSummary} from "../../persistence.js";

export type WebUiPartType = "reasoning" | "text" | "tool_call" | "tool_result";
export type WebUiMessageStatus = "streaming" | "complete";
export type WebUiToolResultStatus = "success" | "error" | "unknown";

export interface WebUiTextPart {
  type: "reasoning" | "text";
  text: string;
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
}

export interface WebUiToolResultPart {
  type: "tool_result";
  callId: string;
  output: string;
  status: WebUiToolResultStatus;
}

export type WebUiPart = WebUiTextPart | WebUiToolCallPart | WebUiToolResultPart;

export interface WebUiMessage {
  id: string;
  sender: "system" | "user" | "assistant";
  text: string;
  timestamp: string;
  parts: WebUiPart[];
  status?: WebUiMessageStatus;
  reasoningContent?: string;
}

export interface WebUiSessionConfig {
  model: {
    provider: string;
    model: string;
  };
  settings: {
    session_name: string;
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

export interface WebUiSessionSnapshot {
  id: string;
  session_id?: string;
  root_id?: string;
  root_label?: string;
  session_name: string;
  turns: number;
  messages: WebUiMessage[];
  model: {
    provider: string;
    model: string;
  };
  usage?: WebUiSessionUsage;
  run?: WebUiSessionRun;
}
