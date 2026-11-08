/**
 * @file subagent-transcript.ts
 * @input A tool result's optional `subagent` snapshot
 * @output The completed steps worth showing below the result, and the read-only message
 *   they make
 * @position Used by components/SubagentTranscript.tsx
 *
 * The snapshot is one-shot and non-streaming. Only steps whose status is
 * `completed` render; running, pending or unknown steps are dropped, as are
 * steps with nothing to show. A snapshot that fails validation shows nothing.
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

/**
 * The completed steps as read-only messages: one assistant message whose parts are the
 * steps' parts in order, with a step range for each, so the transcript renders as the
 * conversation does (tool runs end at a step boundary, rows of different steps stack
 * tight). Empty when no step is completed.
 */
export function subagentTranscriptMessages(
  subagent: WebUiSubagentTranscript | undefined,
): ReadOnlyMessage[] {
  const parts: WebUiPart[] = [];
  const steps: WebUiMessageStep[] = [];
  for (const step of completedSubagentSteps(subagent)) {
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
