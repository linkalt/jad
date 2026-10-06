/**
 * @file approval-answer.ts
 * @input The preview a gated tool call published and the reason the user typed
 * @output Read-only rows for the preview and the body an Allow or Deny posts
 * @position Pure model behind the composer's approval drawer
 *
 * An approval is a question about one call that is already fixed: the user can
 * allow exactly what is shown or deny it with a reason. There is deliberately no
 * edit path — the endpoint refuses `update` for an approval, so a command the
 * user did not see can never run. The rows therefore only describe the call.
 */

export type ApprovalDecision = 'accept' | 'decline';

export interface ApprovalRow {
  /** The preview key, which is also the row's identity. */
  key: string;
  /** What the row is called, e.g. `command`, `oldText`. */
  label: string;
  /** The value as text; objects are pretty-printed JSON. */
  text: string;
  /** When the backend cut the value, how long the full text was in bytes. */
  truncatedBytes?: number;
}

const TRUNCATED_SUFFIX = '_truncated_bytes';
/** The tool name is shown in the title line, not as a row. */
const HIDDEN_KEYS: ReadonlySet<string> = new Set(['tool']);

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function rowText(value: unknown): string {
  if (typeof value === 'string') {
    return value;
  }
  try {
    return JSON.stringify(value, null, 2) ?? '';
  } catch {
    return '';
  }
}

/**
 * One row per field the backend put in the preview, in the order it sent them.
 * A `<name>_truncated_bytes` field is not a row: it marks `<name>` as cut.
 */
export function approvalRows(draft: unknown): ApprovalRow[] {
  if (!isRecord(draft)) {
    return [];
  }
  const rows: ApprovalRow[] = [];
  for (const [key, value] of Object.entries(draft)) {
    if (HIDDEN_KEYS.has(key) || key.endsWith(TRUNCATED_SUFFIX)) {
      continue;
    }
    const cut = draft[`${key}${TRUNCATED_SUFFIX}`];
    rows.push({
      key,
      label: key,
      text: rowText(value),
      ...(typeof cut === 'number' && Number.isSafeInteger(cut) && cut >= 0 ? {truncatedBytes: cut} : {}),
    });
  }
  return rows;
}

/** The sentence under a cut row; says the user is deciding on a shortened view. */
export function truncationNote(truncatedBytes: number): string {
  return `Shortened for display; the full text is ${truncatedBytes} bytes.`;
}

/**
 * The body an answer posts, or why the drawer cannot post it yet.
 *
 * `accept` carries nothing; `decline` requires the reason, the same rule the
 * endpoint enforces, so the drawer refuses before a request is wasted.
 */
export function reviewApprovalAnswer(options: {
  decision: ApprovalDecision;
  reason: string;
  reasonMaxLength: number;
}): {body: {decision: ApprovalDecision; reason?: string}} | {error: string} {
  if (options.decision === 'accept') {
    return {body: {decision: 'accept'}};
  }
  const reason = options.reason.trim();
  if (reason.length === 0) {
    return {error: 'Say why you are denying it, so the agent can try something else.'};
  }
  if (reason.length > options.reasonMaxLength) {
    return {error: `Keep the reason under ${options.reasonMaxLength} characters.`};
  }
  return {body: {decision: 'decline', reason}};
}

/**
 * The title of the notification for an answer or an extension the server refused.
 *
 * An approval is a question about running a call, not a draft the user edits, so
 * it must not be called one. An absent kind is a draft, as everywhere else.
 */
export function interactionFailureTitle(
  kind: 'draft' | 'approval' | undefined,
  action: 'answer' | 'extend',
): string {
  const noun = kind === 'approval' ? 'the approval' : 'the draft';
  return action === 'answer' ? `Unable to answer ${noun}` : `Unable to extend ${noun}`;
}
