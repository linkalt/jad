/**
 * @file RunReconnectNotice.tsx
 * @input The reconnect state of a run whose stream dropped
 * @output One system line in the running area saying the connection is being restored
 * @position Sibling of ProviderRetryNotice; without it a dropped stream looks like a stalled run
 */
import {ChatSystemMessage} from '@astryxdesign/core/Chat';
import {Icon} from '@astryxdesign/core';

import type {RunStreamReconnectState} from '../api';
import {runReconnectNotice} from '../run-reconnect-view';

export interface RunReconnectNoticeProps {
  state: RunStreamReconnectState;
}

export function RunReconnectNotice({state}: RunReconnectNoticeProps) {
  return (
    <ChatSystemMessage
      variant="default"
      icon={<Icon icon="clock" color="warning" size="sm" />}
    >
      {runReconnectNotice(state)}
    </ChatSystemMessage>
  );
}
