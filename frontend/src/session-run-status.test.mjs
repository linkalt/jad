import assert from 'node:assert/strict';
import test from 'node:test';

import {
  isSessionRunning,
  sessionUiStateForRun,
} from './session-run-status.ts';

test('server run state keeps the UI active before stream attachment completes', () => {
  assert.equal(isSessionRunning(false, 'running'), true);
  assert.equal(sessionUiStateForRun('running'), 'accent');
});

test('local run state remains active until its stream settles', () => {
  assert.equal(isSessionRunning(true, 'completed'), true);
});

test('terminal run states restore the expected UI state', () => {
  assert.equal(isSessionRunning(false, 'completed'), false);
  assert.equal(sessionUiStateForRun('completed'), 'success');
  assert.equal(sessionUiStateForRun('failed'), 'warning');
  assert.equal(sessionUiStateForRun('aborted'), 'warning');
  assert.equal(sessionUiStateForRun(undefined), 'success');
});
