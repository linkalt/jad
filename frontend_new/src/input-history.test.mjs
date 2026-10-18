import assert from 'node:assert/strict';
import test from 'node:test';

import {IDLE_HISTORY, recentUserTexts, stepHistory} from './input-history.ts';

const user = (text, extra = {}) => ({sender: 'user', text, ...extra});
const bot = text => ({sender: 'assistant', text});
const edges = (over = {}) => ({
  isCollapsed: true,
  atStart: true,
  onFirstLine: true,
  onLastLine: true,
  hasTokens: false,
  ...over,
});

test('recentUserTexts keeps the newest user messages, oldest first', () => {
  const messages = [user('a'), bot('x'), user('b'), user('c'), bot('y'), user('d')];
  assert.deepEqual(recentUserTexts(messages, 3), ['b', 'c', 'd']);
  assert.deepEqual(recentUserTexts(messages, 99), ['a', 'b', 'c', 'd']);
});

test('recentUserTexts honours a zero or invalid limit', () => {
  const messages = [user('a')];
  assert.deepEqual(recentUserTexts(messages, 0), []);
  assert.deepEqual(recentUserTexts(messages, -1), []);
  assert.deepEqual(recentUserTexts(messages, Number.NaN), []);
});

test('recentUserTexts skips attachment-only messages and consecutive repeats', () => {
  const messages = [
    user('hello'),
    user('[image]', {sentImages: ['data:x']}),
    user('hello'),
    user('  '),
    user('bye'),
  ];
  assert.deepEqual(recentUserTexts(messages, 10), ['hello', 'bye']);
});

test('ArrowUp recalls the newest message from an empty draft and remembers the draft', () => {
  const step = stepHistory(['a', 'b'], IDLE_HISTORY, '', 'ArrowUp', edges());
  assert.deepEqual(step, {cursor: {index: 1, draft: ''}, text: 'b'});
});

test('ArrowUp walks back to the oldest entry and stops there', () => {
  let cursor = {index: 1, draft: 'wip'};
  let step = stepHistory(['a', 'b'], cursor, 'b', 'ArrowUp', edges({atStart: false}));
  assert.deepEqual(step, {cursor: {index: 0, draft: 'wip'}, text: 'a'});
  cursor = step.cursor;
  step = stepHistory(['a', 'b'], cursor, 'a', 'ArrowUp', edges({atStart: false}));
  assert.deepEqual(step, {cursor: {index: 0, draft: 'wip'}, text: 'a'});
});

test('ArrowDown walks forward and then returns to the saved draft', () => {
  let step = stepHistory(['a', 'b'], {index: 0, draft: 'wip'}, 'a', 'ArrowDown', edges());
  assert.deepEqual(step, {cursor: {index: 1, draft: 'wip'}, text: 'b'});
  step = stepHistory(['a', 'b'], step.cursor, 'b', 'ArrowDown', edges());
  assert.deepEqual(step, {cursor: IDLE_HISTORY, text: 'wip'});
});

test('a non-empty draft is recalled over only with the caret at its start', () => {
  assert.equal(stepHistory(['a'], IDLE_HISTORY, 'typed', 'ArrowUp', edges({atStart: false})), null);
  assert.notEqual(stepHistory(['a'], IDLE_HISTORY, 'typed', 'ArrowUp', edges()), null);
});

test('a draft with a badge is never recalled over', () => {
  assert.equal(stepHistory(['a'], IDLE_HISTORY, '', 'ArrowUp', edges({hasTokens: true})), null);
});

test('a selection or an empty history leaves the key to the editor', () => {
  assert.equal(stepHistory(['a'], IDLE_HISTORY, '', 'ArrowUp', edges({isCollapsed: false})), null);
  assert.equal(stepHistory([], IDLE_HISTORY, '', 'ArrowUp', edges()), null);
});

test('ArrowDown with no entry on show is left to the editor', () => {
  assert.equal(stepHistory(['a'], IDLE_HISTORY, 'typed', 'ArrowDown', edges()), null);
});

test('editing the entry on show ends the walk', () => {
  const cursor = {index: 1, draft: ''};
  assert.equal(stepHistory(['a', 'b'], cursor, 'b edited', 'ArrowDown', edges()), null);
  const up = stepHistory(['a', 'b'], cursor, 'b edited', 'ArrowUp', edges({atStart: false}));
  assert.equal(up, null);
});

test('inside a multi-line entry the keys move the caret until its first or last line', () => {
  const cursor = {index: 1, draft: ''};
  const entry = 'one\ntwo';
  const history = ['a', entry];
  assert.equal(stepHistory(history, cursor, entry, 'ArrowUp', edges({onFirstLine: false})), null);
  assert.notEqual(stepHistory(history, cursor, entry, 'ArrowUp', edges({onFirstLine: true})), null);
  assert.equal(stepHistory(history, cursor, entry, 'ArrowDown', edges({onLastLine: false})), null);
});
