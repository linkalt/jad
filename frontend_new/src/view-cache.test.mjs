import assert from 'node:assert/strict';
import {test} from 'node:test';

import {removeViewKeys, removedViewKeys} from './view-cache.ts';

test('finds only view keys omitted from the new ownership set', () => {
  const previous = new Set(['session-a', 'session-b\u0000profile-a', 'session-c']);
  const current = new Set(['session-a', 'session-c', 'session-d']);

  assert.deepEqual(removedViewKeys(previous, current), new Set(['session-b\u0000profile-a']));
  assert.deepEqual(removedViewKeys(current, previous), new Set(['session-d']));
});

test('removes stale record entries and preserves the original when unchanged', () => {
  const record = {
    'session-a': ['kept'],
    'session-b\u0000profile-a': ['removed'],
  };

  const pruned = removeViewKeys(record, new Set(['session-b\u0000profile-a']));
  assert.deepEqual(pruned, {'session-a': ['kept']});
  assert.notEqual(pruned, record);
  assert.equal(removeViewKeys(record, new Set(['not-present'])), record);
});
