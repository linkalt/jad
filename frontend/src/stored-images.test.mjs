import assert from 'node:assert/strict';
import {after, test} from 'node:test';

const originalFetch = globalThis.fetch;
const requests = [];
let reply = () => new Response(new Uint8Array([1, 2, 3]), {status: 200, headers: {'Content-Type': 'image/png'}});
globalThis.fetch = async (input, init = {}) => {
  requests.push({url: String(input), init});
  return reply();
};

const {fetchSessionImage, STORED_IMAGE_NAME} = await import('./api.ts');

after(() => {
  globalThis.fetch = originalFetch;
});

const name = `${'a'.repeat(64)}.png`;

test('fetchSessionImage reads one stored image through the API client', async () => {
  requests.length = 0;
  const blob = await fetchSessionImage('my session', name, 'p 1');
  assert.equal(blob.size, 3);
  assert.equal(requests.length, 1);
  const url = new URL(requests[0].url, 'http://localhost');
  assert.equal(url.pathname, `/api/sessions/my%20session/images/${name}`);
  assert.equal(url.searchParams.get('profile'), 'p 1');
});

test('fetchSessionImage sends no profile query when none is given', async () => {
  requests.length = 0;
  await fetchSessionImage('s', name);
  assert.equal(new URL(requests[0].url, 'http://localhost').search, '');
});

test('fetchSessionImage refuses a name that is not a stored file name, without a request', async () => {
  requests.length = 0;
  for (const bad of ['', '../x.png', `${'A'.repeat(64)}.png`, `${'a'.repeat(63)}.png`, `${'a'.repeat(64)}.svg`, `${name}/..`, `${name}?x=1`]) {
    await assert.rejects(() => fetchSessionImage('s', bad), /invalid stored image name/, bad);
  }
  assert.equal(requests.length, 0);
});

test('fetchSessionImage reports a server error', async () => {
  reply = () => new Response(JSON.stringify({error: 'image not found'}), {status: 404});
  await assert.rejects(() => fetchSessionImage('s', name), /image not found/);
});

test('STORED_IMAGE_NAME accepts the four known extensions only', () => {
  for (const extension of ['png', 'jpg', 'webp', 'gif']) {
    assert.ok(STORED_IMAGE_NAME.test(`${'0f'.repeat(32)}.${extension}`), extension);
  }
  assert.ok(!STORED_IMAGE_NAME.test(`${'0f'.repeat(32)}.jpeg`));
});
