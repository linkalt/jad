import type {WebUiMessage, WebUiMessageStep, WebUiPart} from "./types.js";

/** One event of a run stream: `ui.delta` appends to a part, `ui.part` sets one. */
export interface WebUiStreamEvent {
  event: string;
  data: unknown;
}

/**
 * The events that take a client from `previous` to `next`, or `undefined` when
 * the change can only be expressed as a full `ui.message` snapshot.
 *
 * A run publishes a message for every provider delta, and a complete snapshot
 * carries every part accumulated so far — so streaming the whole message per
 * delta grew quadratically and a busy turn measured gigabytes on the wire. The
 * normalizer keeps the parts before the first change by identity, so only the
 * changed part and anything after it need to be sent: an append (streamed text,
 * reasoning or tool arguments) becomes a suffix, and a replaced or added part is
 * sent once, as a part.
 */
function sameSteps(
  previous: readonly WebUiMessageStep[] | undefined,
  next: readonly WebUiMessageStep[] | undefined,
): boolean {
  if (previous === undefined || next === undefined) {
    return previous === next;
  }
  return previous.length === next.length && previous.every((step, index) => {
    const other = next[index];
    return other !== undefined &&
      step.id === other.id &&
      step.startPart === other.startPart &&
      step.endPart === other.endPart &&
      step.hasTools === other.hasTools;
  });
}

export function messageIncrements(
  previous: WebUiMessage,
  next: WebUiMessage,
): WebUiStreamEvent[] | undefined {
  if (
    previous.id !== next.id ||
    previous.status !== next.status ||
    !sameSteps(previous.steps, next.steps)
  ) {
    return undefined;
  }
  const before = previous.parts;
  const after = next.parts;
  // A part that disappeared has no increment form; a snapshot replaces the state.
  if (after.length < before.length) {
    return undefined;
  }
  let changed = 0;
  while (changed < before.length && before[changed] === after[changed]) {
    changed += 1;
  }
  if (changed === before.length && before.length === after.length) {
    return [];
  }

  const events: WebUiStreamEvent[] = [];
  let index = changed;
  if (index < before.length) {
    const append = appendedText(before[index]!, after[index]);
    events.push(
      append === undefined
        ? {event: "ui.part", data: {id: next.id, index, part: after[index]}}
        : {event: "ui.delta", data: {id: next.id, index, append}},
    );
    index += 1;
  }
  for (; index < after.length; index += 1) {
    events.push({event: "ui.part", data: {id: next.id, index, part: after[index]}});
  }
  return events;
}

/**
 * The suffix the part gained, or `undefined` when it changed in any other way —
 * a finished tool call (its name or `partial` flag changes) is sent whole.
 */
function appendedText(before: WebUiPart, after: WebUiPart | undefined): string | undefined {
  if (after === undefined || after.type !== before.type) {
    return undefined;
  }
  if (before.truncated !== undefined || after.truncated !== undefined) {
    return undefined;
  }
  if (before.type === "tool_call" && after.type === "tool_call") {
    if (before.callId !== after.callId || before.name !== after.name || before.partial !== after.partial) {
      return undefined;
    }
    return after.arguments.startsWith(before.arguments)
      ? after.arguments.slice(before.arguments.length)
      : undefined;
  }
  if ((before.type === "text" || before.type === "reasoning") && after.type === before.type) {
    return after.text.startsWith(before.text) ? after.text.slice(before.text.length) : undefined;
  }
  return undefined;
}
