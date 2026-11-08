/**
 * @file subagent-transcript.ts
 * @input A tool result's optional `subagent` snapshot
 * @output The completed steps worth showing below the result
 * @position Used by ToolCallDetail in components/TranscriptParts.tsx
 *
 * The snapshot is one-shot and non-streaming. Only steps whose status is
 * `completed` render; running, pending or unknown steps are dropped, as are
 * steps with nothing to show.
 */
import type {WebUiSubagentStep, WebUiSubagentTranscript} from './api';

/** The completed steps of a subagent snapshot, in order; empty when absent. */
export function completedSubagentSteps(
  subagent: WebUiSubagentTranscript | undefined,
): WebUiSubagentStep[] {
  if (!subagent || !Array.isArray(subagent.steps)) return [];
  return subagent.steps.filter(
    step => step.status === 'completed' && Array.isArray(step.parts) && step.parts.length > 0,
  );
}
