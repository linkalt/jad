import assert from 'node:assert/strict';
import test from 'node:test';

import {placeRunFailures, runFailureText} from './run-failures.ts';

const failure = (turn, messageId) => ({
  turn,
  ...(messageId === undefined ? {} : {message_id: messageId}),
  turn_id: `t${turn}`,
  at: '2026-10-05T00:00:00.000Z',
  message: `boom ${turn}`,
});

test('places a failure after the user message it names', () => {
  const placed = placeRunFailures(
    [failure(2, 'session-user-o10')],
    ['session-user-o0', 'session-user-o10'],
  );
  assert.deepEqual([...placed.byMessageId.keys()], ['session-user-o10']);
  assert.equal(placed.byMessageId.get('session-user-o10')[0].turn, 2);
  assert.deepEqual(placed.trailing, []);
});

test('keeps several failures of one message in the order given', () => {
  const placed = placeRunFailures(
    [failure(2, 'm'), failure(3, 'm')],
    ['m'],
  );
  assert.deepEqual(placed.byMessageId.get('m').map(item => item.turn), [2, 3]);
});

test('a failure whose message is not visible is hidden, not moved to the end', () => {
  const placed = placeRunFailures([failure(1, 'older')], ['newer']);
  assert.equal(placed.byMessageId.size, 0);
  assert.deepEqual(placed.trailing, []);
});

test('a failure with no message id has no anchor and goes last', () => {
  const placed = placeRunFailures([failure(4)], ['m']);
  assert.equal(placed.byMessageId.size, 0);
  assert.deepEqual(placed.trailing.map(item => item.turn), [4]);
});

test('no failures gives an empty placement', () => {
  for (const input of [undefined, []]) {
    const placed = placeRunFailures(input, ['m']);
    assert.equal(placed.byMessageId.size, 0);
    assert.deepEqual(placed.trailing, []);
  }
});

test('the row text carries the server message', () => {
  assert.equal(runFailureText(failure(1, 'm')), 'This run failed: boom 1');
});
