import assert from 'node:assert/strict';
import {test} from 'node:test';

import {createCoalescedRefresh} from './coalesced-refresh.ts';

function deferred() {
  let resolve;
  const promise = new Promise(res => { resolve = res; });
  return {promise, resolve};
}

test('runs one refresh at a time and collapses busy-period triggers into one follow-up', async () => {
  let active = 0;
  let peakActive = 0;
  let runCount = 0;
  const releases = [];
  const refresh = createCoalescedRefresh(() => {
    runCount += 1;
    active += 1;
    peakActive = Math.max(peakActive, active);
    const gate = deferred();
    releases.push(gate);
    return gate.promise.finally(() => { active -= 1; });
  }, error => { throw error; });

  refresh.request();
  assert.equal(runCount, 1);
  for (let index = 0; index < 20; index += 1) refresh.request();
  assert.equal(runCount, 1);

  releases[0].resolve();
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(runCount, 2);
  for (let index = 0; index < 20; index += 1) refresh.request();
  releases[1].resolve();
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(runCount, 3);
  assert.equal(peakActive, 1);

  refresh.dispose();
  releases[2].resolve();
  await new Promise(resolve => setImmediate(resolve));
  refresh.request();
  assert.equal(runCount, 3);
});

test('reports a failed refresh and remains usable for a later request', async () => {
  const failure = new Error('poll failed');
  const errors = [];
  let runCount = 0;
  const refresh = createCoalescedRefresh(async () => {
    runCount += 1;
    if (runCount === 1) throw failure;
  }, error => errors.push(error));

  refresh.request();
  await new Promise(resolve => setImmediate(resolve));
  assert.deepEqual(errors, [failure]);
  refresh.request();
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(runCount, 2);
  refresh.dispose();
});
