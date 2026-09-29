import assert from 'node:assert/strict';
import test from 'node:test';

import {
  EMPTY_HISTORY_BLOCKS,
  forgetHistoryBlock,
  forgetHistoryBlocks,
  historyBlockFor,
  historyMessagesFor,
  putHistoryBlock,
} from './history-blocks.ts';

const message = id => ({id});

test('keeps a view block while another view is stored', () => {
  // The reported bug: switching to the profile row dropped the session row's
  // block, and a skipped transcript refresh never fetched it again.
  const session = putHistoryBlock(EMPTY_HISTORY_BLOCKS, 'default', {
    etag: 'session-etag',
    messages: [message('s1'), message('s2')],
  });
  const withProfile = putHistoryBlock(session, 'default\u0000chatgpt-go', {
    etag: 'profile-etag',
    messages: [message('p1')],
  });

  assert.deepEqual(historyMessagesFor(withProfile, 'default')?.map(m => m.id), ['s1', 's2']);
  assert.deepEqual(
    historyMessagesFor(withProfile, 'default\u0000chatgpt-go')?.map(m => m.id),
    ['p1'],
  );
  assert.equal(historyBlockFor(withProfile, 'default')?.etag, 'session-etag');
});

test('renders only the block of the view being shown', () => {
  const blocks = putHistoryBlock(EMPTY_HISTORY_BLOCKS, 'a', {
    etag: 'a',
    messages: [message('a1')],
  });

  assert.equal(historyMessagesFor(blocks, 'b'), undefined);
  assert.equal(historyBlockFor(blocks, 'b'), undefined);
});

test('an empty block adds nothing to the transcript', () => {
  const blocks = putHistoryBlock(EMPTY_HISTORY_BLOCKS, 'a', {etag: 'a', messages: []});
  assert.equal(historyMessagesFor(blocks, 'a'), undefined);
});

test('storing an unchanged block keeps the record identity', () => {
  const messages = [message('a1')];
  const first = putHistoryBlock(EMPTY_HISTORY_BLOCKS, 'a', {etag: 'a', messages});
  assert.equal(putHistoryBlock(first, 'a', {etag: 'a', messages}), first);
  assert.notEqual(putHistoryBlock(first, 'a', {etag: 'b', messages}), first);
  assert.notEqual(putHistoryBlock(first, 'a', {etag: 'a', messages: [message('a1')]}), first);
});

test('forgetting drops the named views and leaves the rest', () => {
  const blocks = putHistoryBlock(
    putHistoryBlock(EMPTY_HISTORY_BLOCKS, 'a', {etag: 'a', messages: [message('a1')]}),
    'b',
    {etag: 'b', messages: [message('b1')]},
  );

  assert.deepEqual(Object.keys(forgetHistoryBlock(blocks, 'a')), ['b']);
  assert.equal(forgetHistoryBlock(blocks, 'missing'), blocks);
  assert.equal(forgetHistoryBlocks(blocks, new Set(['missing'])), blocks);
  assert.deepEqual(Object.keys(forgetHistoryBlocks(blocks, new Set(['a', 'b']))), []);
});
