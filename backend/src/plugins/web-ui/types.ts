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
}
