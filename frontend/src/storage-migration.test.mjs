import assert from 'node:assert/strict';
import test from 'node:test';

import {migrateStorageKeys} from './storage-migration.ts';

function createStorage(initial = {}) {
  const data = new Map(Object.entries(initial));
  return {
    data,
    getItem: key => (data.has(key) ? data.get(key) : null),
    setItem: (key, value) => data.set(key, value),
    removeItem: key => data.delete(key),
  };
}

const migrations = [
  {from: 'old.a', to: 'new.a', area: 'local'},
  {from: 'old.t', to: 'new.t', area: 'session'},
];

test('copies the old value to the new key and deletes the old key', () => {
  const local = createStorage({'old.a': 'A'});
  const session = createStorage({'old.t': 'secret'});
  assert.equal(migrateStorageKeys(migrations, {local, session}), 2);
  assert.equal(local.getItem('new.a'), 'A');
  assert.equal(local.getItem('old.a'), null);
  assert.equal(session.getItem('new.t'), 'secret');
  assert.equal(session.getItem('old.t'), null);
});

test('a value already under the new key wins and the old key is still removed', () => {
  const local = createStorage({'old.a': 'OLD', 'new.a': 'NEW'});
  assert.equal(migrateStorageKeys(migrations, {local}), 0);
  assert.equal(local.getItem('new.a'), 'NEW');
  assert.equal(local.getItem('old.a'), null);
});

test('a second run changes nothing', () => {
  const local = createStorage({'old.a': 'A'});
  migrateStorageKeys(migrations, {local});
  assert.equal(migrateStorageKeys(migrations, {local}), 0);
  assert.equal(local.getItem('new.a'), 'A');
});

test('a missing or throwing storage does not stop the others', () => {
  const broken = {getItem() { throw new Error('blocked'); }, setItem() {}, removeItem() {}};
  const session = createStorage({'old.t': 'x'});
  assert.equal(migrateStorageKeys(migrations, {local: broken, session}), 1);
  assert.equal(session.getItem('new.t'), 'x');
  assert.equal(migrateStorageKeys(migrations, {local: null, session: null}), 0);
});
