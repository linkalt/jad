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

test('a fork after an answer names the user message that started its turn', async () => {
  const {forkPointMessage} = await import('./session-fork.ts');
  const messages = [
    {id: 'session-user-o0', sender: 'user'},
    {id: 'resp-1', sender: 'assistant'},
    {id: 'session-user-o90', sender: 'user'},
    {id: 'resp-1', sender: 'assistant'},
  ];
  assert.equal(forkPointMessage(messages, messages[3], true), messages[2]);
  assert.equal(forkPointMessage(messages, messages[1], true), messages[0]);
  // A fork before a message, or a user message, names the message itself.
  assert.equal(forkPointMessage(messages, messages[3], false), messages[3]);
  assert.equal(forkPointMessage(messages, messages[2], true), messages[2]);
  // A turn of the compacted history, or no user message before: the answer itself.
  const compacted = [{id: 'history-user-1', sender: 'user'}, {id: 'resp-2', sender: 'assistant'}];
  assert.equal(forkPointMessage(compacted, compacted[1], true), compacted[1]);
  assert.equal(forkPointMessage([messages[1]], messages[1], true), messages[1]);
});
