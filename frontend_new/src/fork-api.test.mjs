import assert from 'node:assert/strict';
import {after, test} from 'node:test';

const originalFetch = globalThis.fetch;
const requests = [];
let reply = () => new Response('{}');
globalThis.fetch = async (url, init = {}) => {
  requests.push({url: String(url), init});
  return reply();
};
const {forkSession} = await import('./api.ts');

after(() => {
  globalThis.fetch = originalFetch;
});

test('forkSession posts the new session, the message and keep_turn, and reads the result', async () => {
  reply = () => new Response(JSON.stringify({session: 'local:demo-fork-1', profile: 'dev/coder', turns: 2, bytes: 10, images: 0}), {status: 201});
  const result = await forkSession('local:demo', {session: 'local:demo-fork-1', messageId: 'session-user-o3', keepTurn: false}, 'dev/coder');
  assert.deepEqual(result, {session: 'local:demo-fork-1', profile: 'dev/coder', turns: 2});
  assert.match(requests[0].url, /\/sessions\/local%3Ademo\/fork\?profile=dev%2Fcoder$/);
  assert.equal(requests[0].init.method, 'POST');
  assert.deepEqual(JSON.parse(requests[0].init.body), {session: 'local:demo-fork-1', message_id: 'session-user-o3', keep_turn: false});
});

test('forkSession reports the server error and a malformed answer', async () => {
  reply = () => new Response(JSON.stringify({error: 'session already exists'}), {status: 412});
  await assert.rejects(forkSession('local:demo', {session: 'x'}), /already exists/);
  reply = () => new Response(JSON.stringify({session: 7}), {status: 201});
  await assert.rejects(forkSession('local:demo', {session: 'x'}), /invalid fork data/);
});
