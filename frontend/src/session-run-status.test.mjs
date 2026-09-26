import assert from 'node:assert/strict';
import test from 'node:test';

import {
  isSessionRunning,
  reconcileDetachedRunSessionIds,
  sessionUiStateForRun,
  withSnapshotRun,
} from './session-run-status.ts';

test('server run state keeps the UI active before stream attachment completes', () => {
  assert.equal(isSessionRunning(false, 'running'), true);
  assert.equal(sessionUiStateForRun('running'), 'accent');
});

test('local run state remains active until its stream settles', () => {
  assert.equal(isSessionRunning(true, 'completed'), true);
});

test('a detached run stays active until a fresh snapshot reports it ended', () => {
  const detached = new Set(['session-a', 'session-b']);

  assert.equal(isSessionRunning(false, undefined, detached.has('session-a')), true);
  assert.deepEqual(
    reconcileDetachedRunSessionIds(detached, [
      {id: 'session-a', status: 'running'},
      {id: 'session-b', status: 'completed'},
    ]),
    new Set(['session-a']),
  );
  assert.equal(
    isSessionRunning(false, 'running', detached.has('session-a')),
    true,
  );
  assert.deepEqual(
    reconcileDetachedRunSessionIds(new Set(['session-a']), [
      {id: 'session-a', status: 'completed'},
    ]),
    new Set(),
  );
  assert.equal(isSessionRunning(false, 'completed', false), false);
});

test('detached runs disappear when their session is no longer listed', () => {
  assert.deepEqual(
    reconcileDetachedRunSessionIds(new Set(['deleted-session']), []),
    new Set(),
  );
});

test('terminal run states restore the expected UI state', () => {
  assert.equal(isSessionRunning(false, 'completed'), false);
  assert.equal(sessionUiStateForRun('completed'), 'success');
  assert.equal(sessionUiStateForRun('failed'), 'warning');
  assert.equal(sessionUiStateForRun('aborted'), 'warning');
  assert.equal(sessionUiStateForRun(undefined), 'success');
});

test('a polled snapshot refreshes the run a streamed session carries', () => {
  const session = {
    id: 'session-a',
    state: 'accent',
    turns: 7,
    run: {turn_id: 'turn-old', started_at: '2026-01-01T00:00:00.000Z', status: 'completed'},
  };
  const merged = withSnapshotRun(session, {
    run: {turn_id: 'turn-new', started_at: '2026-01-02T00:00:00.000Z', status: 'running'},
  });

  // The client can attach again: it holds the turn the server is running.
  assert.deepEqual(merged.run, {
    turn_id: 'turn-new',
    started_at: '2026-01-02T00:00:00.000Z',
    status: 'running',
  });
  assert.equal(isSessionRunning(false, merged.run.status, false), true);
  // Fields the local stream owns are untouched.
  assert.equal(merged.state, 'accent');
  assert.equal(merged.turns, 7);
  assert.equal(merged.id, 'session-a');
});

test('an unchanged run keeps the session identity', () => {
  const run = {turn_id: 'turn-1', started_at: '2026-01-01T00:00:00.000Z', status: 'running'};
  const session = {id: 'session-a', run};

  assert.equal(withSnapshotRun(session, {run}), session);
  assert.equal(withSnapshotRun(session, {run: {...run}}), session);
  assert.equal(withSnapshotRun({id: 'session-a'}, {}).run, undefined);
});

test('a snapshot without a run clears a stale one', () => {
  const session = {
    id: 'session-a',
    run: {turn_id: 'turn-1', started_at: '2026-01-01T00:00:00.000Z', status: 'running'},
  };
  const cleared = withSnapshotRun(session, {});

  assert.equal('run' in cleared, false);
  assert.equal(isSessionRunning(false, cleared.run?.status, false), false);
});
