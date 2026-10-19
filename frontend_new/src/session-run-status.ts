import type {SessionRunState, SessionRunStatus} from './api';

export type SessionUiState = 'success' | 'warning' | 'accent';

/** Use server run state immediately after reload, before stream attachment settles. */
export function isSessionRunning(
  locallyRunning: boolean,
  serverRunStatus: SessionRunStatus | undefined,
): boolean {
  return locallyRunning || serverRunStatus === 'running';
}

/**
 * Whether a fresh session snapshot proves the run this client streams is over.
 *
 * A run's identity is its turn: only a snapshot whose run carries the same
 * `turn_id` can speak for this client's own run, and only a terminal status ends
 * it. This is the way out the local running state has no other form of: when the
 * stream stops delivering frames without ever erroring, nothing else would clear
 * it and the composer would stay blocked until a reload.
 */
export function snapshotEndsLocalRun(
  localTurnId: string | null | undefined,
  snapshotRun: SessionRunState | undefined,
): boolean {
  if (localTurnId === null || localTurnId === undefined || snapshotRun === undefined) {
    return false;
  }
  return snapshotRun.turn_id === localTurnId && snapshotRun.status !== 'running';
}

function sameRun(
  left: SessionRunState | undefined,
  right: SessionRunState | undefined,
): boolean {
  return left === right || (
    left !== undefined &&
    right !== undefined &&
    left.turn_id === right.turn_id &&
    left.status === right.status &&
    left.started_at === right.started_at &&
    left.source === right.source &&
    left.caller === right.caller
  );
}

function sameProfileRuns(
  left: Readonly<Record<string, SessionRunState>> | undefined,
  right: Readonly<Record<string, SessionRunState>> | undefined,
): boolean {
  if (left === right) {
    return true;
  }
  if (left === undefined || right === undefined) {
    return false;
  }
  const leftProfiles = Object.keys(left);
  const rightProfiles = Object.keys(right);
  return leftProfiles.length === rightProfiles.length && leftProfiles.every(
    profile => Object.hasOwn(right, profile) && sameRun(left[profile], right[profile]),
  );
}

/**
 * Refresh the run descriptors a session object carries while leaving every field
 * the local stream owns (`state`, `turns`, `usage`) alone.
 *
 * A poll preserves the previous session object while this client streams. Its
 * default and per-profile run descriptors still need refreshing, or a detached
 * row can show an old status or lose the `turn_id` needed to re-attach.
 */
export function withSnapshotRun<Session extends {
  run?: SessionRunState;
  runsByProfile?: Record<string, SessionRunState>;
}>(
  session: Session,
  snapshot: {
    run?: SessionRunState;
    runs_by_profile?: Record<string, SessionRunState>;
  },
): Session {
  const run = snapshot.run;
  const runsByProfile = snapshot.runs_by_profile;
  if (sameRun(session.run, run) && sameProfileRuns(session.runsByProfile, runsByProfile)) {
    return session;
  }

  const next = {...session} as Session & {
    run?: SessionRunState;
    runsByProfile?: Record<string, SessionRunState>;
  };
  if (run === undefined) {
    delete next.run;
  } else {
    next.run = run;
  }
  if (runsByProfile === undefined) {
    delete next.runsByProfile;
  } else {
    next.runsByProfile = runsByProfile;
  }
  return next;
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
