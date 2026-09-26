import type {SessionRunState, SessionRunStatus} from './api';

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

/**
 * Refresh the run a session object carries while leaving every field the local
 * stream owns (`state`, `turns`, `usage`) alone.
 *
 * The session poll keeps the previous object for the session this client is
 * streaming, so its fresh `run` used to be dropped — leaving the client unable
 * to re-attach (it needs `turn_id`) after the run was detached by a session
 * switch, with a placeholder no stream could ever finish.
 */
export function withSnapshotRun<Session extends {run?: SessionRunState}>(
  session: Session,
  snapshot: {run?: SessionRunState},
): Session {
  const run = snapshot.run;
  if (run === undefined) {
    if (session.run === undefined) {
      return session;
    }
    const next = {...session} as Session & {run?: SessionRunState};
    delete next.run;
    return next;
  }

  const current = session.run;
  if (
    current !== undefined &&
    current.turn_id === run.turn_id &&
    current.status === run.status &&
    current.started_at === run.started_at
  ) {
    return session;
  }
  return {...session, run};
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
