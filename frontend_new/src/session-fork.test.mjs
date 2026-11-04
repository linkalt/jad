import assert from 'node:assert/strict';
import test from 'node:test';

import {forkSessionRef} from './session-fork.ts';

test('forkSessionRef keeps the root, adds the suffix and a stamp, and stays a valid id', () => {
  assert.equal(forkSessionRef('local:demo', '-fork-', 36 ** 2), 'local:demo-fork-100');
  assert.equal(forkSessionRef('demo', '-fork-', 35), 'demo-fork-z');
  const long = forkSessionRef('local:' + 'a'.repeat(200), '-fork-', 1);
  const id = long.slice('local:'.length);
  assert.equal(id.length, 128);
  assert.match(id, /^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$/);
  assert.ok(id.endsWith('-fork-1'));
});
