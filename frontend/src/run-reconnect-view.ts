/**
 * @file run-reconnect-view.ts
 * @input The reconnect state `attachSessionRun` and `streamSessionRun` report while the run stream is down
 * @output The notice line shown in the running area
 * @position Pure model behind the "reconnecting to the run" notice
 */

import type {RunStreamReconnectState} from './api';

/** The notice line: whether the network is gone or the run is being reached again. */
export function runReconnectNotice(state: RunStreamReconnectState): string {
  if (state.offline) {
    return 'You are offline. The run continues on the server; this view reconnects when the network returns.';
  }
  return state.attempt === 0
    ? 'Reconnecting to the run…'
    : `Reconnecting to the run (attempt ${state.attempt})…`;
}
