import {ChatSystemMessage, Icon} from '@astryxdesign/core';
import {type SessionRunFailure} from '../api';
import {runFailureText} from '../run-failures';

/** A failed run, drawn where the user message it belongs to ends. */
export function RunFailureRow({failure}: {failure: SessionRunFailure}) {
  return (
    <ChatSystemMessage
      variant="default"
      icon={<Icon icon="close" color="error" size="sm" />}
    >
      {runFailureText(failure)}
    </ChatSystemMessage>
  );
}
