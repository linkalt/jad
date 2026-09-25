import assert from 'node:assert/strict';
import test from 'node:test';

import {
  isSessionRunning,
  reconcileDetachedRunSessionIds,
  sessionUiStateForRun,
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
