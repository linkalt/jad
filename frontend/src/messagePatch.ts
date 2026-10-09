import type {WebUiMessage, WebUiPart} from './api';

/**
 * Applies one run increment to the message a client already holds.
 *
 * The run stream sends increments (`ui.delta`, `ui.part`) instead of a complete
 * snapshot per provider delta, so the client owns the merge. Derived message
 * fields are projections of `parts`; append-only updates to a final part can
 * extend those projections without filtering and joining every part again.
 */
export function applyPartDelta(message: WebUiMessage, index: number, append: string): WebUiMessage {
  if (append.length === 0) {
    return message;
  }
  const part = message.parts[index];
  if (part === undefined || part.type === 'hosted_tool') {
    // A hosted tool row has no text to grow; the server resends it as a whole part.
    return message;
  }
  const grown: WebUiPart =
    part.type === 'tool_call'
      ? {...part, arguments: part.arguments + append}
      : part.type === 'tool_result'
        ? {...part, output: part.output + append}
        : {...part, text: part.text + append};
  const parts = message.parts.slice();
  parts[index] = grown;

  // Tool payloads do not contribute to either derived text field.
  if (part.type === 'tool_call' || part.type === 'tool_result') {
    return {...message, parts};
  }

  if (part.type === 'text') {
    // Appending to the final text part preserves the projection by concatenating
    // the delta. If later text parts exist, rebuild only the text projection.
    const text = hasLaterPart(message.parts, index, 'text')
      ? textFromParts(parts)
      : message.text + append;
    return {...message, parts, text};
  }

  const next = {...message, parts};
  if (
    message.reasoningContent !== undefined &&
    message.reasoningContent.length > 0 &&
    !hasTrailingWhitespace(part.text) &&
    !hasLaterPart(message.parts, index, 'reasoning')
  ) {
    // The existing projection is already trimmed and this final part has no
    // trailing whitespace to lose, so its append can be trimmed independently.
    return withReasoningContent(next, `${message.reasoningContent}${append}`.trim());
  }
  return withReasoningContent(next, reasoningContentFromParts(parts));
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
  return withReasoningContent(
    {...message, parts, text: textFromParts(parts)},
    reasoningContentFromParts(parts),
  );
}

function textFromParts(parts: readonly WebUiPart[]): string {
  return parts
    .filter((item): item is Extract<WebUiPart, {type: 'text'}> => item.type === 'text')
    .map(item => item.text)
    .join('');
}

function reasoningContentFromParts(parts: readonly WebUiPart[]): string {
  return parts
    .filter((item): item is Extract<WebUiPart, {type: 'reasoning'}> => item.type === 'reasoning')
    .map(item => item.text)
    .join('\n\n')
    .trim();
}

function withReasoningContent(message: WebUiMessage, reasoningContent: string): WebUiMessage {
  if (reasoningContent.length > 0) {
    message.reasoningContent = reasoningContent;
  } else {
    delete message.reasoningContent;
  }
  return message;
}

function hasLaterPart(
  parts: readonly WebUiPart[],
  index: number,
  type: 'text' | 'reasoning',
): boolean {
  for (let next = index + 1; next < parts.length; next += 1) {
    if (parts[next]?.type === type) {
      return true;
    }
  }
  return false;
}

function hasTrailingWhitespace(value: string): boolean {
  const lastCharacter = value.at(-1);
  return lastCharacter !== undefined && lastCharacter.trim().length === 0;
}
