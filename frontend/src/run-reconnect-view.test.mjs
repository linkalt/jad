import assert from 'node:assert/strict';
import test from 'node:test';

import {runReconnectNotice} from './run-reconnect-view.ts';

test('words an attempt count only after the first attempt', () => {
  assert.equal(
    runReconnectNotice({attempt: 0, offline: false}),
    'Reconnecting to the run…',
  );
  assert.equal(
    runReconnectNotice({attempt: 3, offline: false}),
    'Reconnecting to the run (attempt 3)…',
  );
});

test('says the run continues when the browser is offline', () => {
  const text = runReconnectNotice({attempt: 5, offline: true});
  assert.match(text, /offline/);
  assert.match(text, /continues on the server/);
  assert.doesNotMatch(text, /attempt/);
});
