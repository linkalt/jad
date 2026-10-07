/**
 * @file approval.ts
 * @input A validated tool call, the session's approval settings, the run's way to ask the user
 * @output Exports approvalPreview(), createApprovalRequester() and the verdict types
 * @position Between a tool call that needs a decision and the run's interaction broker
 *
 * A tool whose `approval` is `ask` does not run until the user says yes. The
 * question is the ordinary run interaction with `kind: "approval"`, so the
 * answer endpoint, the deadline, the extension and the stop signal all work as
 * they do for a draft. What differs is the outcome: a refusal, a missed
 * deadline and a stopped run all become a normal `blocked` tool result the model
 * can read and re-plan from, never a failed run.
 *
 * Questions are asked one at a time: the broker holds a single pending question,
 * and two calls of one response may both need a decision. Calls that need none
 * are not held up by it.
 */
import type {ApprovalSettings} from "../config.js";
import {AsyncMutex} from "../concurrency.js";
import type {JsonRecord} from "../persistence.js";
import type {ApprovalCall, ApprovalVerdict, ToolInteractionHandler, ToolInteractionQuestion} from "./types.js";

/** What the user is shown: a short title and the call's own fields, cut to a bound. */
export interface ApprovalPreview {
  title: string;
  draft: JsonRecord;
}

/** Cuts `text` to at most `maxBytes` of UTF-8 without leaving half a character. */
export function cutText(text: string, maxBytes: number): {text: string; truncated: boolean; bytes: number} {
  const buffer = Buffer.from(text, "utf8");
  if (buffer.length <= maxBytes) {
    return {text, truncated: false, bytes: buffer.length};
  }
  let end = maxBytes;
  // A UTF-8 continuation byte starts with 0b10; stepping back to a lead byte drops a split character.
  while (end > 0 && (buffer[end]! & 0xc0) === 0x80) {
    end -= 1;
  }
  return {text: buffer.subarray(0, end).toString("utf8"), truncated: true, bytes: buffer.length};
}

function field(draft: JsonRecord, name: string, value: unknown, maxBytes: number): void {
  if (typeof value !== "string") {
    return;
  }
  const cut = cutText(value, maxBytes);
  draft[name] = cut.text;
  if (cut.truncated) {
    draft[`${name}_truncated_bytes`] = cut.bytes;
  }
}

function optionalField(draft: JsonRecord, name: string, value: unknown): void {
  if (value !== undefined) {
    draft[name] = value;
  }
}

/**
 * The call as the user sees it. Only fields that say what will happen are shown;
 * long text is cut to `maxBytes` and says how long it really was.
 */
export function approvalPreview(
  toolName: string,
  server: string | undefined,
  args: JsonRecord,
  maxBytes: number,
): ApprovalPreview {
  const draft: JsonRecord = {tool: toolName};
  if (server !== undefined) {
    draft.server = server;
    field(draft, "arguments", JSON.stringify(args), maxBytes);
    return {title: `Run ${server} tool ${toolName}`, draft};
  }
  switch (toolName) {
    case "bash":
      field(draft, "command", args.command, maxBytes);
      // The text piped to the command is part of what runs (`python3 -` alone says nothing).
      field(draft, "stdin", args.stdin, maxBytes);
      optionalField(draft, "cwd", args.cwd);
      optionalField(draft, "timeout_ms", args.timeout_ms);
      return {title: "Run shell command", draft};
    case "exec":
      // The code is what runs; the language alone says nothing.
      field(draft, "lang", args.lang, maxBytes);
      field(draft, "code", args.code, maxBytes);
      optionalField(draft, "args", args.args);
      optionalField(draft, "cwd", args.cwd);
      optionalField(draft, "timeout_ms", args.timeout_ms);
      return {title: "Run code", draft};
    case "process":
      draft.action = "start";
      field(draft, "command", args.command, maxBytes);
      // The text written to the process is part of what runs (`python3 -` alone says nothing).
      field(draft, "stdin", args.stdin, maxBytes);
      optionalField(draft, "cwd", args.cwd);
      return {title: "Start background process", draft};
    case "write":
      field(draft, "path", args.path, maxBytes);
      optionalField(draft, "contentEncoding", args.contentEncoding);
      if (typeof args.content === "string") {
        draft.content_bytes = Buffer.byteLength(args.content, "utf8");
        if (args.contentEncoding !== "base64") {
          field(draft, "content", args.content, maxBytes);
        }
      }
      return {title: "Write file", draft};
    case "edit":
      field(draft, "path", args.path, maxBytes);
      field(draft, "oldText", args.oldText, maxBytes);
      field(draft, "newText", args.newText, maxBytes);
      optionalField(draft, "replaceAll", args.replaceAll);
      return {title: "Edit file", draft};
    default:
      field(draft, "arguments", JSON.stringify(args), maxBytes);
      return {title: `Run ${toolName}`, draft};
  }
}

/** Whether a call to `toolName` with `args` is one that approval applies to. */
export function callNeedsApproval(toolName: string, args: JsonRecord): boolean {
  if (toolName !== "process") {
    return true;
  }
  // Only a start launches anything; status, wait, list and stop act on what the
  // user already approved. `action` is inferred from `command` like the tool does.
  const action = typeof args.action === "string" ? args.action.trim() : "";
  if (action.length > 0) {
    return action === "start";
  }
  return typeof args.command === "string" && args.command.trim().length > 0;
}

export interface ApprovalRequesterOptions {
  settings: ApprovalSettings;
  /** One per run step: serializes questions, because the broker holds only one. */
  gate: AsyncMutex;
  /** Absent when no client can answer; `settings.headless` then decides. */
  ask: ((question: ToolInteractionQuestion, identity: {tool: string; tool_call_id: string}) => ReturnType<ToolInteractionHandler>) | undefined;
}

/** Turns one call into a question for the user and the answer into a verdict. */
export function createApprovalRequester(
  options: ApprovalRequesterOptions,
): (call: ApprovalCall) => Promise<ApprovalVerdict> {
  const {settings, gate, ask} = options;
  return async (call) => {
    if (!callNeedsApproval(call.tool, call.args)) {
      return {allowed: true};
    }
    if (ask === undefined) {
      return settings.headless === "allow"
        ? {allowed: true}
        : {
            allowed: false,
            code: "approval_unavailable",
            reason: "This call needs the user's approval and no client is connected that can give it.",
          };
    }
    const preview = approvalPreview(call.tool, call.server, call.args, settings.preview_bytes);
    return gate.runExclusive(async (): Promise<ApprovalVerdict> => {
      const answer = await ask(
        {
          kind: "approval",
          draft: preview.draft,
          title: preview.title,
          timeout_ms: settings.timeout_ms,
          extend_ms: settings.extend_ms,
          max_timeout_ms: settings.max_timeout_ms,
        },
        {tool: call.modelToolName, tool_call_id: call.callId},
      );
      if (answer.outcome === "cancelled") {
        return {
          allowed: false,
          code: "approval_cancelled",
          reason: "The run was stopped before the call was approved.",
        };
      }
      if (answer.outcome === "timed_out") {
        return {
          allowed: false,
          code: "approval_timeout",
          reason: "The user did not decide in time; the call was not run.",
        };
      }
      if (answer.decision === "accept") {
        return {allowed: true};
      }
      return {
        allowed: false,
        code: "approval_denied",
        reason: answer.reason !== undefined && answer.reason.length > 0
          ? answer.reason
          : "The user declined this call.",
      };
    });
  };
}
