import type {SessionRunStatus} from './api';

export type SessionUiState = 'success' | 'warning' | 'accent';

/** Use server run state immediately after reload, before stream attachment settles. */
export function isSessionRunning(
  locallyRunning: boolean,
  serverRunStatus: SessionRunStatus | undefined,
  detachedRunKnownActive = false,
): boolean {
  return locallyRunning || detachedRunKnownActive || serverRunStatus === 'running';
}

export interface DetachedRunSessionSnapshot {
  id: string;
  status?: SessionRunStatus;
}

/** Keep detached runs active until a fresh session-list snapshot reports a terminal state. */
export function reconcileDetachedRunSessionIds(
  detachedSessionIds: ReadonlySet<string>,
  snapshots: readonly DetachedRunSessionSnapshot[],
): ReadonlySet<string> {
  if (detachedSessionIds.size === 0) {
    return detachedSessionIds;
  }

  const statusById = new Map(snapshots.map(({id, status}) => [id, status]));
  const hasTerminalRun = [...detachedSessionIds].some(
    sessionId => statusById.get(sessionId) !== 'running',
  );
  if (!hasTerminalRun) {
    return detachedSessionIds;
  }

  return new Set(
    [...detachedSessionIds].filter(sessionId => statusById.get(sessionId) === 'running'),
  );
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
