/**
 * @file subagent-transcript.ts
 * @input A tool result's optional `subagent` snapshot
 * @output The completed steps worth showing below the result
 * @position Used by ToolCallDetail in components/TranscriptParts.tsx
 *
 * The snapshot is one-shot and non-streaming. Only steps whose status is
 * `completed` render; running, pending or unknown steps are dropped, as are
 * steps with nothing to show. A snapshot that fails validation shows nothing.
 */
import {validSubagentTranscript, type WebUiSubagentStep, type WebUiSubagentTranscript} from './api.ts';

/** The completed steps of a subagent snapshot, in order; empty when absent. */
export function completedSubagentSteps(
  subagent: WebUiSubagentTranscript | undefined,
): WebUiSubagentStep[] {
  // A malformed snapshot is ignored whole, the way the stream drops it from the result.
  const valid = validSubagentTranscript(subagent);
  if (!valid) return [];
  return valid.steps.filter(step => step.status === 'completed' && step.parts.length > 0);
}
