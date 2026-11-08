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
 * The child's closing summary is the tool result's own output, so the read-only message
 * leaves it out (see `subagentTranscriptMessages`); the snapshot and the shared renderer
 * stay whole.
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

/**
 * The steps to show: the completed steps without the child's terminal summary. When the
 * snapshot has a tool call, the trailing step's text parts are dropped (its reasoning and
 * other parts stay) and a trailing step left empty goes with them. A snapshot without any
 * tool call is shown whole, since there the text is all the child did.
 */
function shownSubagentSteps(subagent: WebUiSubagentTranscript | undefined): WebUiSubagentStep[] {
  const steps = completedSubagentSteps(subagent);
  if (!steps.some(step => step.parts.some(isToolCall))) return steps;
  const last = steps[steps.length - 1];
  const parts = last.parts.filter(part => part.type !== 'text');
  return parts.length === 0 ? steps.slice(0, -1) : [...steps.slice(0, -1), {...last, parts}];
}

/**
 * The shown steps as read-only messages: one assistant message whose parts are the
 * steps' parts in order, with a step range for each, so the transcript renders as the
 * conversation does (tool runs end at a step boundary, rows of different steps stack
 * tight). Empty when nothing survives.
 */
export function subagentTranscriptMessages(
  subagent: WebUiSubagentTranscript | undefined,
): ReadOnlyMessage[] {
  const parts: WebUiPart[] = [];
  const steps: WebUiMessageStep[] = [];
  for (const step of shownSubagentSteps(subagent)) {
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
