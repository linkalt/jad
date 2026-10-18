import type {SessionRunFailure} from './api';

export interface PlacedRunFailures {
  /** Failures to show right after the message with that id. */
  byMessageId: ReadonlyMap<string, readonly SessionRunFailure[]>;
  /** Failures the server could not tie to a message; shown after the last row. */
  trailing: readonly SessionRunFailure[];
}

/**
 * Decides where each recorded failure is drawn.
 *
 * A failure that names its user message is shown only while that message is in
 * `visibleMessageIds`: when the message is in an older page or outside the
 * transcript window, the failure appears together with it instead of being
 * moved to the end, where it would read as the latest turn's outcome. A failure
 * without a `message_id` has no anchor, so it goes last.
 */
export function placeRunFailures(
  failures: readonly SessionRunFailure[] | undefined,
  visibleMessageIds: Iterable<string>,
): PlacedRunFailures {
  const byMessageId = new Map<string, SessionRunFailure[]>();
  const trailing: SessionRunFailure[] = [];
  if (failures === undefined || failures.length === 0) {
    return {byMessageId, trailing};
  }
  const visible = new Set(visibleMessageIds);
  for (const failure of failures) {
    if (failure.message_id === undefined) {
      trailing.push(failure);
      continue;
    }
    if (!visible.has(failure.message_id)) {
      continue;
    }
    const list = byMessageId.get(failure.message_id);
    if (list === undefined) {
      byMessageId.set(failure.message_id, [failure]);
    } else {
      list.push(failure);
    }
  }
  return {byMessageId, trailing};
}

/** The text of one failure row. */
export function runFailureText(failure: SessionRunFailure): string {
  return `This run failed: ${failure.message}`;
}
