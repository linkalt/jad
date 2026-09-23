import type {WebUiMessage, WebUiPart} from './api';

/**
 * Applies one run increment to the message a client already holds.
 *
 * The run stream sends increments (`ui.delta`, `ui.part`) instead of a complete
 * snapshot per provider delta, so the client owns the merge: the part at `index`
 * grows by `append` or is replaced by `part`, and the message-level fields the
 * renderer reads are recomputed the way the server derives them.
 */
export function applyPartDelta(message: WebUiMessage, index: number, append: string): WebUiMessage {
  if (append.length === 0) {
    return message;
  }
  const part = message.parts[index];
  if (part === undefined) {
    return message;
  }
  const grown: WebUiPart =
    part.type === 'tool_call'
      ? {...part, arguments: part.arguments + append}
      : part.type === 'tool_result'
        ? {...part, output: part.output + append}
        : {...part, text: part.text + append};
  return withPart(message, index, grown);
}

/** Replaces the part at `index`; the server appends a new part by sending its index. */
export function applyPart(message: WebUiMessage, index: number, part: WebUiPart): WebUiMessage {
  return withPart(message, index, part);
}

function withPart(message: WebUiMessage, index: number, part: WebUiPart): WebUiMessage {
  if (index > message.parts.length) {
    return message;
  }
  const parts = message.parts.slice();
  parts[index] = part;
  const text = parts
    .filter(item => item.type === 'text')
    .map(item => item.text)
    .join('');
  const reasoningContent = parts
    .filter(item => item.type === 'reasoning')
    .map(item => item.text)
    .join('\n\n')
    .trim();
  const next: WebUiMessage = {...message, parts, text};
  if (reasoningContent.length > 0) {
    next.reasoningContent = reasoningContent;
  } else {
    delete next.reasoningContent;
  }
  return next;
}
