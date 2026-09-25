import assert from 'node:assert/strict';
import test from 'node:test';

import {
  bucketWidth,
  createRowHeightStore,
  parseRowHeights,
  usableHeight,
} from './row-heights.ts';

function createStorage() {
  const data = new Map();
  const storage = {
    writes: 0,
    getItem: key => (data.has(key) ? data.get(key) : null),
    setItem: (key, value) => {
      storage.writes += 1;
      data.set(key, value);
    },
  };
  return storage;
}

function createTimers() {
  const pending = [];
  return {
    schedule: (callback, delayMs) => {
      pending.push({callback, delayMs});
    },
    get count() {
      return pending.length;
    },
    runAll() {
      for (const item of pending.splice(0)) {
        item.callback();
      }
    },
  };
}

function createStore(storage, overrides = {}) {
  return createRowHeightStore({
    storage,
    storageKey: 'row-heights',
    maxEntries: 3,
    saveDelayMs: 500,
    widthBucketPx: 50,
    schedule: createTimers().schedule,
    ...overrides,
  });
}

test('remembers a row height for the width it was measured at', () => {
  const store = createStore(createStorage());

  assert.equal(store.heightFor('m1', 800), null, 'unknown rows have no height');

  store.remember('m1', 123.456, 800);
  assert.equal(store.heightFor('m1', 800), 123.5);
  assert.equal(store.heightFor('m2', 800), null);
});

test('accepts a neighbouring width and refuses a different one', () => {
  const store = createStore(createStorage());
  store.remember('m1', 200, 800);

  assert.equal(store.heightFor('m1', 830), 200, 'same bucket');
  assert.equal(store.heightFor('m1', 850), 200, 'one bucket of slack');
  assert.equal(store.heightFor('m1', 700), null, 'two buckets is a different wrap');
  assert.equal(store.heightFor('m1', 400), null);
  assert.equal(store.heightFor('m1', 0), 200, 'a session that has not measured a row yet');
});

test('survives a reload through storage', () => {
  const storage = createStorage();
  const store = createStore(storage);
  store.remember('m1', 150, 800);
  store.remember('m2', 40, 800);
  store.flush();

  const reader = createStore(storage);
  assert.equal(reader.heightFor('m1', 800), 150);
  assert.equal(reader.heightFor('m2', 800), 40);
  assert.equal(reader.size(), 2);
});

test('ignores stored data it cannot trust', () => {
  assert.deepEqual(parseRowHeights(null), []);
  assert.deepEqual(parseRowHeights('not json'), []);
  assert.deepEqual(parseRowHeights('[]'), []);
  assert.deepEqual(parseRowHeights(JSON.stringify({v: 2, rows: [['a', 10, 800]]})), []);
  assert.deepEqual(parseRowHeights(JSON.stringify({v: 1, rows: 'nope'})), []);
  assert.deepEqual(
    parseRowHeights(
      JSON.stringify({
        v: 1,
        rows: [['a', 0, 800], ['b', -5, 800], ['c', null, 800], ['d', 1e9, 800], [5, 10, 800], ['e', 10, 800]],
      }),
    ),
    [['e', 10, 800]],
  );
  assert.deepEqual(parseRowHeights(JSON.stringify({v: 1, rows: [['f', 10, 'wide']]})), [['f', 10, 0]]);
});

test('a stored file larger than the cap keeps the newest rows', () => {
  const storage = createStorage();
  const store = createStore(storage);
  for (const id of ['a', 'b', 'c', 'd']) {
    store.remember(id, 10, 800);
  }
  store.flush();

  assert.deepEqual(
    parseRowHeights(storage.getItem('row-heights')).map(entry => entry[0]),
    ['b', 'c', 'd'],
  );
  assert.equal(store.size(), 3);
});

test('re-measuring the same height does not queue another write', () => {
  const storage = createStorage();
  const timers = createTimers();
  const store = createStore(storage, {schedule: timers.schedule});

  store.remember('m1', 100, 800);
  assert.equal(timers.count, 1);

  store.remember('m1', 100, 800);
  assert.equal(timers.count, 1, 'unchanged measurements are dropped');

  store.remember('m2', 200, 800);
  assert.equal(timers.count, 1, 'one pending write covers a batch');

  timers.runAll();
  assert.equal(storage.writes, 1);
  assert.equal(store.heightFor('m2', 800), 200);

  store.remember('m3', 300, 800);
  assert.equal(timers.count, 1, 'the next change schedules again');
});

test('refuses measurements that cannot be a row', () => {
  const store = createStore(createStorage());

  store.remember('m1', 0, 800);
  store.remember('m2', -5, 800);
  store.remember('m3', Number.NaN, 800);
  store.remember('m4', 1e9, 800);
  store.remember('', 10, 800);

  assert.equal(store.size(), 0);
  assert.equal(store.heightFor('m1', 800), null);
});

test('buckets widths and treats an unmeasured one as unknown', () => {
  assert.equal(bucketWidth(824, 50), 800);
  assert.equal(bucketWidth(826, 50), 850);
  assert.equal(bucketWidth(0, 50), 0);
  assert.equal(bucketWidth(-10, 50), 0);
  assert.equal(bucketWidth(Number.NaN, 50), 0);
  assert.equal(usableHeight(0.5), true);
  assert.equal(usableHeight(0), false);
  assert.equal(usableHeight(1e9), false);
});
