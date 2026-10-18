import assert from 'node:assert/strict';
import test from 'node:test';
import {stopRunAfterConfirmation} from './stop-run.ts';

test('detaches only after the server confirms the run stopped', async () => {
  const events = [];
  const stopped = await stopRunAfterConfirmation(
    async () => { events.push('request'); return {stopped: true}; },
    () => events.push('detach'),
  );
  assert.equal(stopped, true);
  assert.deepEqual(events, ['request', 'detach']);
});

test('keeps the local stream attached when the server reports no active run', async () => {
  let detached = false;
  const stopped = await stopRunAfterConfirmation(
    async () => ({stopped: false}),
    () => { detached = true; },
  );
  assert.equal(stopped, false);
  assert.equal(detached, false);
});

test('keeps the local stream attached when the stop request fails', async () => {
  let detached = false;
  await assert.rejects(
    stopRunAfterConfirmation(
      async () => { throw new Error('request failed'); },
      () => { detached = true; },
    ),
    /request failed/,
  );
  assert.equal(detached, false);
});
