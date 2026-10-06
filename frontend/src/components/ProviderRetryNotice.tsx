/**
 * @file ProviderRetryNotice.tsx
 * @input A provider retry wait the run stream reported
 * @output One centered status line with a live countdown
 * @position The "the run is not hung" notice at the end of the running area
 *
 * While the server waits to retry a failed provider request nothing else on screen
 * changes, so a long wait looks like a hang. This line says why and for how long.
 * It re-renders on a timer from the wait's own end time, so the countdown cannot drift
 * from the server's wait, and it stops ticking when the wait is cleared and the
 * component unmounts.
 */
import {useEffect, useState} from 'react';

import {ChatSystemMessage} from '@astryxdesign/core/Chat';
import {Icon} from '@astryxdesign/core';

import {providerRetryNotice, type ProviderRetryWait} from '../provider-retry-view';

export interface ProviderRetryNoticeProps {
  wait: ProviderRetryWait;
  /** How often the countdown refreshes, in milliseconds. */
  tickMs: number;
}

export function ProviderRetryNotice({wait, tickMs}: ProviderRetryNoticeProps) {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    setNow(Date.now());
    const timer = setInterval(() => setNow(Date.now()), tickMs);
    return () => clearInterval(timer);
  }, [wait, tickMs]);
  return (
    <ChatSystemMessage
      variant="default"
      icon={<Icon icon="clock" color="warning" size="sm" />}
    >
      {providerRetryNotice(wait, now)}
    </ChatSystemMessage>
  );
}
