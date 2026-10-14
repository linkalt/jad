interface FactMessage {
  sender: string;
  parts?: readonly {type: string}[];
  tools?: unknown;
}

/** The assistant message the live tool list falls back to once nothing is streaming. */
export function latestToolCallMessage<Message extends FactMessage>(
  messages: readonly Message[],
): Message | undefined {
  for (let index = messages.length - 1; index >= 0; index -= 1) {
    const message = messages[index];
    if (message?.sender === 'assistant' && message.parts?.some(part => part.type === 'tool_call')) {
      return message;
    }
  }
  return undefined;
}

/** The newest assistant message that carries a tool list. */
export function latestToolsMessage<Message extends FactMessage>(
  messages: readonly Message[],
): Message | undefined {
  for (let index = messages.length - 1; index >= 0; index -= 1) {
    const message = messages[index];
    if (message?.sender === 'assistant' && message.tools) {
      return message;
    }
  }
  return undefined;
}
