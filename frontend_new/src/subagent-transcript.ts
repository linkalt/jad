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
 * does not hold it whole (`omitsLastText` on its summary: handed off, cut, failed, lost)
 * gets nothing left out. The snapshot and the shared renderer stay whole.
 */
import {validSubagentTranscript, type WebUiMessageStep, type WebUiPart, type WebUiSubagentStep, type WebUiSubagentTranscript} from './api.ts';
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
}

/**
 * The steps to show: the completed steps without the child's last text, which the tool
 * result above already shows. When the result holds that text whole and the snapshot has a
 * tool call, the trailing step's text parts are dropped (its reasoning and other parts stay)
 * and a trailing step left empty goes with them. A result that omits it (`omitsLastText`)
 * gets its snapshot shown whole: it holds a run id (the child is still going, or ended after
 * the result was served), only the head of the text (cut), no text (failed), or text that
 * can be a step the snapshot leaves out (lost). A snapshot without any tool call is shown
 * whole too, since there the text is all the child did.
 */
function shownSubagentSteps(
  subagent: WebUiSubagentTranscript | undefined,
  result: SubagentResultShape,
): WebUiSubagentStep[] {
  const steps = completedSubagentSteps(subagent);
  if (result.omitsLastText === true || !steps.some(step => step.parts.some(isToolCall))) return steps;
  const last = steps[steps.length - 1];
  const parts = last.parts.filter(part => part.type !== 'text');
  return parts.length === 0 ? steps.slice(0, -1) : [...steps.slice(0, -1), {...last, parts}];
}

/**
 * The shown steps as read-only messages: one assistant message whose parts are the
 * steps' parts in order, with a step range for each, so the transcript renders as the
 * conversation does (tool runs end at a step boundary, rows of different steps stack
 * tight). Empty when nothing survives. `result` is the tool result's summary (only its
 * `omitsLastText` is read); without it the result is taken to hold the child's last text.
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
