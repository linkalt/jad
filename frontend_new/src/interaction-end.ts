/**
 * @file interaction-end.ts
 * @input The `ended` member the run stream attaches to the event that clears a question
 * @output A validated end and, for a tab that did not cause it, the notice to show
 * @position Pure model behind the "answered in another window" notice
 *
 * A question leaves the stream as `pending: null`, which alone cannot say whether it
 * was answered here, answered in another window, timed out or cut off by the run
 * ending. The server now says so once, on that event. The tab that answered already
 * knows and stays quiet; any other tab would otherwise watch its question vanish
 * with no explanation, so it gets one line. A tab that attaches later is told only
 * that nothing is pending: the reason is not replayed.
 */

export type InteractionEndKind = 'draft' | 'approval';
export type InteractionEndOutcome = 'answered' | 'timed_out' | 'cancelled';
export type InteractionEndDecision = 'accept' | 'update' | 'decline';

export interface InteractionEnd {
  interactionId: string;
  kind: InteractionEndKind;
  tool: string;
  outcome: InteractionEndOutcome;
  /** Present only for an answer; a timeout or a stop is not a decision anybody made. */
  decision?: InteractionEndDecision;
}

export interface InteractionEndNotice {
  title: string;
  message: string;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

/**
 * Reads the `ended` member of an interaction event. A missing or malformed value is
 * `undefined` (an older server, or a bad event) and shows nothing: this is an
 * explanation, so a bad one must never become a wrong one.
 */
export function parseInteractionEnd(value: unknown): InteractionEnd | undefined {
  if (!isRecord(value)) return undefined;
  const {interaction_id: interactionId, kind, tool, outcome, decision} = value;
  if (typeof interactionId !== 'string' || interactionId === '') return undefined;
  if (kind !== 'draft' && kind !== 'approval') return undefined;
  if (typeof tool !== 'string') return undefined;
  if (outcome !== 'answered' && outcome !== 'timed_out' && outcome !== 'cancelled') return undefined;
  if (outcome === 'answered') {
    if (decision !== 'accept' && decision !== 'update' && decision !== 'decline') return undefined;
    return {interactionId, kind, tool, outcome, decision};
  }
  if (decision !== undefined) return undefined;
  return {interactionId, kind, tool, outcome};
}

/**
 * What to tell this tab, or `undefined` when it already knows. `answeredHere` is true
 * when this tab sent the answer, and `stoppedHere` when it asked for the stop.
 */
export function interactionEndNotice(
  end: InteractionEnd,
  context: {answeredHere: boolean; stoppedHere: boolean},
): InteractionEndNotice | undefined {
  const approval = end.kind === 'approval';
  if (end.outcome === 'answered') {
    if (context.answeredHere) return undefined;
    const title = 'Answered in another window';
    if (approval) {
      return {
        title,
        message: end.decision === 'decline'
          ? `Another window denied ${end.tool}, so it did not run.`
          : `Another window allowed ${end.tool}.`,
      };
    }
    if (end.decision === 'decline') {
      return {title, message: `Another window declined the draft for ${end.tool}.`};
    }
    return {
      title,
      message: end.decision === 'update'
        ? `Another window accepted an edited draft for ${end.tool}.`
        : `Another window accepted the draft for ${end.tool}.`,
    };
  }
  if (end.outcome === 'timed_out') {
    return approval
      ? {title: 'The approval timed out', message: `Nobody answered in time, so ${end.tool} did not run.`}
      : {title: 'The draft timed out', message: `Nobody answered in time; the draft for ${end.tool} was not confirmed.`};
  }
  if (context.stoppedHere) return undefined;
  return {
    title: 'The run stopped',
    message: `The run stopped before ${approval ? 'the approval for' : 'the draft for'} ${end.tool} was answered.`,
  };
}
