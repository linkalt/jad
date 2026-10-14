import assert from 'node:assert/strict';
import test from 'node:test';

import {forgetSnapshotEtags, rememberSnapshotEtag} from './snapshot-etags.ts';

test('etags are kept per view', () => {
  const etags = new Map();
  rememberSnapshotEtag(etags, 'a', '"1"');
  rememberSnapshotEtag(etags, 'b', '"2"');
  assert.equal(etags.get('a'), '"1"');
  assert.equal(etags.get('b'), '"2"');
});

test('a newer etag replaces the older one', () => {
  const etags = new Map();
  rememberSnapshotEtag(etags, 'a', '"1"');
  rememberSnapshotEtag(etags, 'a', '"2"');
  assert.equal(etags.get('a'), '"2"');
});

test('a response without an etag forgets that view only', () => {
  const etags = new Map([['a', '"1"'], ['b', '"2"']]);
  rememberSnapshotEtag(etags, 'a', undefined);
  assert.equal(etags.has('a'), false);
  assert.equal(etags.get('b'), '"2"');
});

test('forgetting views drops their etags and keeps the others', () => {
  const etags = new Map([['a', '"1"'], ['b', '"2"'], ['c', '"3"']]);
  forgetSnapshotEtags(etags, new Set(['a', 'c', 'missing']));
  assert.deepEqual([...etags.keys()], ['b']);
});
