import type { ValidateFunction } from "ajv";

import type { ResponseFunctionCallOutputItemList } from "openai/resources/responses/responses.js";

import type { DraftBounds } from "../json-bounds.js";
import type { JsonRecord } from "../persistence.js";

export type ToolOutput = string | ResponseFunctionCallOutputItemList;

export type ToolSource = "native" | "mcp";

/** What the user decided about a tool's question. */
export type ToolDecision = "accept" | "update" | "decline";
export type ToolInteractionSource = "user" | "timeout";

/** Why the wait ended: the user answered, the deadline passed, or the run stopped. */
export type ToolInteractionOutcome = "answered" | "timed_out" | "cancelled";

/** What a tool asks the user; the runner adds the identity of the call. */
export interface ToolInteractionQuestion {
  /** Absent means `draft`. An `approval` is a yes or no and is never edited. */
  kind?: "draft" | "approval";
  draft: JsonRecord;
  title?: string;
  /** How long the run waits for the user; the session's configured wait. */
  timeout_ms: number;
  /** How much one extension adds to the deadline when the user asks for more time. */
  extend_ms: number;
  /** The ceiling for the whole wait, extensions included. */
  max_timeout_ms: number;
  /**
   * Bounds the user's edited draft is re-checked against, so an edit cannot be
   * larger or deeper than a proposal could have been.
   */
  limits?: DraftBounds;
}

export interface ToolInteractionAnswer {
  decision: ToolDecision;
  /** The accepted draft: the user's edited version when they changed it. */
  draft?: JsonRecord;
  reason?: string;
  outcome: ToolInteractionOutcome;
  /** Who resolved the wait; omitted for cancellation. */
  source?: ToolInteractionSource;
}

/**
 * Asks the user to decide on data the tool produced. Absent when the run cannot
 * reach a client that could answer, in which case a tool continues without the
 * answer instead of failing.
 */
export type ToolInteractionHandler = (
  question: ToolInteractionQuestion,
) => Promise<ToolInteractionAnswer>;

/** Why an approval did not let a call run. `approval_unavailable` means nobody could be asked. */
export type ApprovalDenialCode =
  | "approval_denied"
  | "approval_timeout"
  | "approval_cancelled"
  | "approval_unavailable";

export type ApprovalVerdict =
  | {allowed: true}
  | {allowed: false; code: ApprovalDenialCode; reason: string};

/** A call the user is asked to approve; the run turns it into a question. */
export interface ApprovalCall {
  /** The call's id in the transcript; the question publishes it to the client. */
  callId: string;
  /** The name the model used, which is what the client shows and the transcript holds. */
  modelToolName: string;
  /** The tool as the user knows it: the remote name for an MCP tool. */
  tool: string;
  /** Set for MCP tools: the server the call goes to. */
  server?: string;
  args: JsonRecord;
}

export interface ToolDefinition {
  id: string;
  source: ToolSource;
  server?: string;
  name: string;
  description: string;
  inputSchema: JsonRecord;
}

export interface ToolExecutionContext {
  callId: string;
  workspaceRoot: string;
  sessionDir: string;
  /** Cancels foreground work when its owning run is shutting down. */
  signal?: AbortSignal;
  /** Present when a client can answer; see `ToolInteractionHandler`. */
  requestInteraction?: ToolInteractionHandler;
  /**
   * Decides whether a call to a tool whose `approval` is `ask` may run. When the
   * host supplies none the call is blocked, so a missing wire cannot run it.
   */
  requestApproval?: (call: ApprovalCall) => Promise<ApprovalVerdict>;
  /** Host persistence hook called after a user/headless decision is settled. */
  onInteractionResolved?: (
    answer: ToolInteractionAnswer,
    proposed: JsonRecord,
  ) => void | Promise<void>;
}

export interface LoadedTool {
  definition: ToolDefinition;
  validate: ValidateFunction<unknown>;
  /** Scheduler policy: resolve this call before running co-batched calls. */
  requiresUserDecision?: boolean;
  /** Never changes state and depends on none, so it may run beside other calls. */
  readOnly?: boolean;
  /** `ask` holds each call until the user approves it; absent or `off` runs it. */
  approval?: "off" | "ask";
  /** Tolerant pre-validation normalization (enum aliases, harmless variants). */
  normalizeArguments?(args: JsonRecord): JsonRecord;
  execute(
    args: JsonRecord,
    context: ToolExecutionContext,
  ): Promise<ToolOutput>;
}
