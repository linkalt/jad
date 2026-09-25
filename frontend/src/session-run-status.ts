import type {SessionRunStatus} from './api';

export type SessionUiState = 'success' | 'warning' | 'accent';

/** Use server run state immediately after reload, before stream attachment settles. */
export function isSessionRunning(
  locallyRunning: boolean,
  serverRunStatus: SessionRunStatus | undefined,
): boolean {
  return locallyRunning || serverRunStatus === 'running';
}

export function sessionUiStateForRun(
  status: SessionRunStatus | undefined,
): SessionUiState {
  switch (status) {
    case 'running':
      return 'accent';
    case 'failed':
    case 'aborted':
      return 'warning';
    default:
      return 'success';
  }
}
