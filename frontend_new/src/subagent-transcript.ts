/**
 * @file subagent-transcript.ts
 * @input A subagent transcript (the steps fetched on demand by the subagent route; the
 *   tool result itself carries only a summary)
 * @output The completed steps worth showing below the result, and the read-only message
 *   they make
 * @position Used by components/SubagentTranscript.tsx
 *
 * The snapshot is one-shot and non-streaming. Only steps whose status is
 * `completed` render; running, pending or unknown steps are dropped, as are
 * steps with nothing to show. A snapshot that fails validation shows nothing.
 *
 * The child's last text is the content of a result that came back with it whole, so the
 * read-only message leaves it out there (see `subagentTranscriptMessages`); a result that
 * does not hold it whole (`omitsLastText` on its summary: handed off, cut, failed, lost),
 * or whose snapshot says its last step may not be the child's newest response
 * (`mayHaveNewer`), gets nothing left out. The snapshot and the shared renderer stay whole.
 */
import {
  validSubagentTranscript,
  type WebUiMessageStep,
  type WebUiPart,
  type WebUiSubagentStep,
  type WebUiSubagentSummary,
  type WebUiSubagentTranscript,
} from './api.ts';
import type {ReadOnlyMessage} from './read-only-transcript.ts';

/** The completed steps of a subagent snapshot, in order; empty when absent. */
export function completedSubagentSteps(
  subagent: WebUiSubagentTranscript | undefined,
): WebUiSubagentStep[] {
  // A malformed snapshot is ignored whole, the way the stream drops it from the result.
  const valid = validSubagentTranscript(subagent);
  if (!valid) return [];
  return valid.steps.filter(step => step.status === 'completed' && step.parts.length > 0);
}

const isToolCall = (part: WebUiPart) => part.type === 'tool_call';

/** What the tool result says of the child's last text; see `WebUiSubagentSummary`. */
export interface SubagentResultShape {
  omitsLastText?: boolean;
  /** The parent's call has no result yet, so no copy of the child's last text exists above to leave out. */
  unsettled?: boolean;
}

/** Whether the snapshot says its last step may not be the newest response; read as `validSubagentTranscript` does. */
function mayHaveNewer(subagent: WebUiSubagentTranscript | undefined): boolean {
  return subagent?.mayHaveNewer !== undefined && subagent.mayHaveNewer !== false;
}

/**
 * The steps to show: the completed steps without the child's last text, which the tool
 * result above already shows. When the result holds that text whole and the snapshot has a
 * tool call, the trailing step's text parts are dropped (its reasoning and other parts stay)
 * and a trailing step left empty goes with them. A result that omits it (`omitsLastText`)
 * gets its snapshot shown whole: it holds a run id (the child is still going, or ended after
 * the result was served), only the head of the text (cut), no text (failed), or text that
 * can be a step the snapshot leaves out (lost). A snapshot that says its last step may not
 * be the child's newest response (`mayHaveNewer`: it stopped at a limit, refused or withheld
 * a later response) is shown whole as well, since the result's text may belong to a step the
 * snapshot does not show; that text can then appear twice, which is the cheaper mistake. A
 * snapshot without any tool call is shown whole too, since there the text is all the child
 * did. Nothing is left out while the parent's call is unsettled (no result holds the text
 * yet) or the child is still running (`running`: its last step is not its end): the trim
 * applies only to a settled result, an uncut text and a finished child.
 */
function shownSubagentSteps(
  subagent: WebUiSubagentTranscript | undefined,
  result: SubagentResultShape,
): WebUiSubagentStep[] {
  const steps = completedSubagentSteps(subagent);
  if (result.unsettled === true || subagent?.running === true || result.omitsLastText === true || mayHaveNewer(subagent) || !steps.some(step => step.parts.some(isToolCall))) return steps;
  const last = steps[steps.length - 1];
  const parts = last.parts.filter(part => part.type !== 'text');
  return parts.length === 0 ? steps.slice(0, -1) : [...steps.slice(0, -1), {...last, parts}];
}

/**
 * The shown steps as read-only messages: one assistant message whose parts are the
 * steps' parts in order, with a step range for each, so the transcript renders as the
 * conversation does (tool runs end at a step boundary, rows of different steps stack
 * tight). Empty when nothing survives. `result` is the tool result's summary (only its
 * `omitsLastText` is read; the transcript carries its own `mayHaveNewer`); without it the
 * result is taken to hold the child's last text.
 */
export function subagentTranscriptMessages(
  subagent: WebUiSubagentTranscript | undefined,
  result: SubagentResultShape = {},
): ReadOnlyMessage[] {
  const parts: WebUiPart[] = [];
  const steps: WebUiMessageStep[] = [];
  for (const step of shownSubagentSteps(subagent, result)) {
    const startPart = parts.length;
    parts.push(...step.parts);
    steps.push({
      id: step.id,
      startPart,
      endPart: parts.length,
      hasTools: step.parts.some(part => part.type === 'tool_call'),
    });
  }
  return parts.length === 0 ? [] : [{key: 'subagent', role: 'assistant', parts, steps}];
}

/** The native subagent tool's call names, as the server's `IsSubagentTool` takes them. */
export function isSubagentToolName(name: string): boolean {
  return name === 'subagent' || name === 'native.subagent';
}

/** What the right panel needs to show the subagent section of a tool call. */
export interface SubagentSection {
  /** The settled result's summary; absent while the call has no result. */
  summary?: WebUiSubagentSummary;
  /** The call has its result. */
  settled: boolean;
}

/**
 * The subagent section of a tool block, `undefined` for none. A settled result shows it when
 * it carries a summary (as before). A subagent call that has no result yet shows it too: its
 * child is running (or about to), and waiting for the result is what kept the transcript
 * empty until the child had ended. A settled result without a summary (a child that failed
 * before its first step) has none, so a section shown while the call ran goes away then.
 */
export function subagentSectionOf(
  call: {name: string},
  result: {subagent?: WebUiSubagentSummary} | undefined,
): SubagentSection | undefined {
  if (result === undefined) {
    return isSubagentToolName(call.name) ? {settled: false} : undefined;
  }
  return result.subagent === undefined ? undefined : {summary: result.subagent, settled: true};
}

/**
 * The transcript button's label. `transcript` is what the open panel last read (undefined
 * while closed, loading or starting: nothing is known then). A running child says so with
 * the steps it has completed; with no summary yet (the call is unsettled), or a summary of
 * zero steps, an unread child is taken to be running, since a result without a step comes
 * only from a handed-off child.
 */
export function subagentTranscriptLabel({
  isOpen,
  summary,
  transcript,
}: {
  isOpen: boolean;
  summary?: WebUiSubagentSummary;
  transcript?: WebUiSubagentTranscript;
}): string {
  const steps = transcript === undefined ? (summary?.steps ?? 0) : transcript.steps.length;
  const running = transcript === undefined ? summary === undefined || summary.steps === 0 : transcript.running === true;
  const count = `${steps} ${steps === 1 ? 'step' : 'steps'}`;
  const tail = running ? (steps === 0 ? 'running, no step completed yet' : `running, ${count}`) : count;
  return `${isOpen ? 'Hide' : 'Show'} Transcript · ${tail}`;
}
