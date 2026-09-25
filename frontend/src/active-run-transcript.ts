export interface SenderMessage {
  sender: string;
}

/**
 * An attached run's stream is the canonical live representation of its current
 * turn. The transcript can already contain completed provider steps from that
 * turn, so suppress assistant rows after the latest user prompt while the live
 * snapshot is available.
 */
export function suppressAttachedRunTranscriptTail<Message extends SenderMessage>(
  messages: readonly Message[],
  hasLiveAttachedRun: boolean,
): readonly Message[] {
  if (!hasLiveAttachedRun) {
    return messages;
  }

  let latestUserIndex = -1;
  for (let index = messages.length - 1; index >= 0; index -= 1) {
    if (messages[index]?.sender === 'user') {
      latestUserIndex = index;
      break;
    }
  }
  if (latestUserIndex < 0) {
    return messages;
  }

  const hasAssistantTail = messages
    .slice(latestUserIndex + 1)
    .some(message => message.sender === 'assistant');
  return hasAssistantTail ? messages.slice(0, latestUserIndex + 1) : messages;
}
