import assert from 'node:assert/strict';
import {after, test} from 'node:test';

const originalFetch = globalThis.fetch;
const requests = [];
let reply = () => new Response('{}', {status: 200});
globalThis.fetch = async (input, init = {}) => {
  requests.push({url: String(input), init});
  return reply();
};

const {fetchSessionReadme} = await import('./api.ts');

after(() => {
  globalThis.fetch = originalFetch;
});

function json(value, status = 200) {
  return new Response(JSON.stringify(value), {status});
}

test('fetchSessionReadme asks the profile route and returns the README', async () => {
  requests.length = 0;
  reply = () => json({name: 'README.md', content: '# Hi', truncated: false});
  const readme = await fetchSessionReadme('sess 1', 'dev/coder');
  assert.match(requests[0].url, /\/sessions\/sess%201\/readme\?profile=dev%2Fcoder$/);
  assert.deepEqual(readme, {name: 'README.md', content: '# Hi', truncated: false});
});

test('a working directory with no README is null, not an error', async () => {
  reply = () => json({error: 'no README file'}, 404);
  assert.equal(await fetchSessionReadme('s'), null);
});

test('any other failure is an error with the server message, a missing session included', async () => {
  reply = () => json({error: 'session not found'}, 404);
  await assert.rejects(fetchSessionReadme('s'), /session not found/);
  reply = () => json({error: 'file listing is disabled'}, 403);
  await assert.rejects(fetchSessionReadme('s'), /file listing is disabled/);
  reply = () => json({name: 'README.md'}, 200);
  await assert.rejects(fetchSessionReadme('s'), /invalid README data/);
});
