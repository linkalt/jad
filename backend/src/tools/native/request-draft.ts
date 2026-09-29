/**
 * @file request-draft.ts
 * @input The model's structured reading of an ambiguous request, via its arguments
 * @output Exports the native `request_draft` tool and its result rendering
 * @position Native tool that parks the turn until the user decides
 *
 * The tool asks the question; the run publishes it as message state and settles
 * the promise when the user answers, when the deadline passes, or when the run
 * is stopped. The tool result carries that outcome back to the model, so a
 * declined or unanswered draft is an ordinary turn step rather than a failed
 * call — which is also why the outcome is persisted as a plain
 * `function_call_output` and the transcript format is untouched.
 *
 * The draft is dynamic JSON, like a memory record: any shape is allowed and the
 * bounds are the configured ones. The same bounds are re-checked on the user's
 * edited version by the answer endpoint, so an edit cannot be larger than a
 * proposal could have been.
 */
import {isDeepStrictEqual} from "node:util";

import type {NativeRequestDraftToolSettings} from "../../config.js";
import {jsonBytes, jsonDepth} from "../../json-bounds.js";
import type {JsonRecord} from "../../persistence.js";
import type {
  ToolDefinition,
  ToolExecutionContext,
  ToolInteractionAnswer,
  ToolOutput,
} from "../types.js";

export const NATIVE_REQUEST_DRAFT_ID = "native.request_draft";

export const nativeRequestDraftDefinition: ToolDefinition = {
  id: NATIVE_REQUEST_DRAFT_ID,
  source: "native",
  name: "request_draft",
  description:
    "Present how you read the user's request as a JSON draft and wait for them to " +
    "accept, edit or decline it. Call it before starting work when the request could " +
    "reasonably be read in more than one way, or when a wrong reading would waste the " +
    "turn. Do not start the work before it returns. The result carries `decision` " +
    "(accept = proceed as drafted, update = proceed with `draft`, the user's edited " +
    "version, decline = do not proceed), `status` (answered, timed_out or cancelled) " +
    "and, when the user changed something, `changed` — the paths they added, removed " +
    "or corrected. Treat anything but `answered` as a decline.",
  inputSchema: {
    type: "object",
    properties: {
      title: {
        type: "string",
        description: "One line naming what the draft decides, shown above it.",
      },
      draft: {
        type: "object",
        description:
          "Your structured understanding: the fields the user should confirm or " +
          "correct (goal, scope, constraints, assumptions, open questions, …). Any " +
          "shape is allowed; keep it small enough to read at a glance.",
        additionalProperties: true,
      },
    },
    required: ["draft"],
    additionalProperties: false,
  },
};

/**
 * The tool result the model reads: the decision, the accepted draft, the paths
 * the user changed and why. `changed` is reported against the tool's own draft,
 * so a large edit reads as a handful of paths instead of every leaf inside it.
 */
export function renderDraftAnswer(answer: ToolInteractionAnswer, proposed?: JsonRecord): string {
  const changed = proposed === undefined || answer.draft === undefined
    ? undefined
    : changedPaths(proposed, answer.draft);
  return JSON.stringify(
    {
      decision: answer.decision,
      status: answer.outcome,
      ...(answer.draft === undefined ? {} : {draft: answer.draft}),
      ...(changed === undefined || changed.length === 0 ? {} : {changed}),
      ...(answer.reason === undefined ? {} : {reason: answer.reason}),
    },
    null,
    2,
  );
}

/**
 * The paths where `after` differs from `before`, as the model reads a JSON path:
 * `goal`, `scope[1]`, `reviewers[0].name`. A replaced container reports itself as
 * one path rather than as every leaf inside it.
 */
export function changedPaths(before: unknown, after: unknown, prefix = ""): string[] {
  if (isDeepStrictEqual(before, after)) {
    return [];
  }
  if (isJsonRecord(before) && isJsonRecord(after)) {
    const paths: string[] = [];
    for (const key of new Set([...Object.keys(before), ...Object.keys(after)])) {
      paths.push(...changedPaths(before[key], after[key], prefix.length === 0 ? key : `${prefix}.${key}`));
    }
    return paths;
  }
  if (Array.isArray(before) && Array.isArray(after)) {
    const paths: string[] = [];
    for (let index = 0; index < Math.max(before.length, after.length); index += 1) {
      paths.push(...changedPaths(before[index], after[index], `${prefix}[${index}]`));
    }
    return paths;
  }
  return [prefix];
}

export async function executeNativeRequestDraft(
  args: JsonRecord,
  context: ToolExecutionContext,
  policy: NativeRequestDraftToolSettings,
): Promise<ToolOutput> {
  const draft = asJsonRecord(args.draft);
  if (draft === undefined) {
    return renderRefusal("invalid_draft", "draft must be a JSON object.");
  }
  const bytes = jsonBytes(draft);
  if (bytes > policy.max_draft_bytes) {
    return renderRefusal(
      "draft_too_large",
      `draft is ${bytes} bytes; this session accepts at most ${policy.max_draft_bytes}. ` +
        "Send a smaller draft — the user reads it at a glance.",
      {max_draft_bytes: policy.max_draft_bytes},
    );
  }
  const depth = jsonDepth(draft);
  if (depth > policy.max_draft_depth) {
    return renderRefusal(
      "draft_too_deep",
      `draft nests ${depth} levels; this session accepts at most ${policy.max_draft_depth}. ` +
        "Flatten it, or move the deep part into a JSON string the user can edit as text.",
      {max_draft_depth: policy.max_draft_depth},
    );
  }
  const title = typeof args.title === "string" ? args.title.trim() : "";
  if (title.length > policy.max_title_chars) {
    return renderRefusal(
      "title_too_long",
      `title is ${title.length} characters; this session accepts at most ${policy.max_title_chars}.`,
      {max_title_chars: policy.max_title_chars},
    );
  }
  const requestInteraction = context.requestInteraction;
  if (requestInteraction === undefined) {
    return renderRefusal(
      "no_client",
      "This run cannot reach a client that could answer a draft; continue without confirming it.",
    );
  }
  const answer = await requestInteraction({
    draft,
    ...(title.length === 0 ? {} : {title}),
    timeout_ms: policy.timeout_ms,
    extend_ms: policy.extend_ms,
    max_timeout_ms: policy.max_timeout_ms,
    limits: {max_draft_bytes: policy.max_draft_bytes, max_draft_depth: policy.max_draft_depth},
  });
  return renderDraftAnswer(answer, draft);
}

function renderRefusal(code: string, message: string, details: JsonRecord = {}): string {
  return JSON.stringify({error: code, message, ...details}, null, 2);
}

function asJsonRecord(value: unknown): JsonRecord | undefined {
  return isJsonRecord(value) ? value : undefined;
}

function isJsonRecord(value: unknown): value is JsonRecord {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}
