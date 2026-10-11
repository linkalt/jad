import assert from 'node:assert/strict';
import {after, test} from 'node:test';

const originalFetch = globalThis.fetch;
const requests = [];
let reply = () => new Response(new Uint8Array([1, 2, 3]), {status: 200, headers: {'Content-Type': 'image/png'}});
globalThis.fetch = async (input, init = {}) => {
  requests.push({url: String(input), init});
  return reply();
};

const {attachSessionRun, fetchMessageImage, fetchSessionImage, STORED_IMAGE_NAME} = await import('./api.ts');

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

test('fetchMessageImage asks for one inline image of a message by call id and position', async () => {
  reply = () => new Response(new Uint8Array([1, 2, 3]), {status: 200});
  requests.length = 0;
  const blob = await fetchMessageImage('my session', 'o12', 'call/1', 1, 340, 'p 1');
  assert.equal(blob.size, 3);
  const url = new URL(requests[0].url, 'http://localhost');
  assert.match(url.pathname, /\/sessions\/my%20session\/messages\/o12\/images\/call%2F1\/1$/);
  assert.equal(url.searchParams.get('cursor'), '340');
  assert.equal(url.searchParams.get('profile'), 'p 1');
});

test('fetchMessageImage sends no query when there is no cursor and no profile', async () => {
  requests.length = 0;
  await fetchMessageImage('s', 'm', 'c', 0);
  assert.equal(new URL(requests[0].url, 'http://localhost').search, '');
});

test('fetchMessageImage refuses a position that is not a non-negative integer, without a request', async () => {
  requests.length = 0;
  for (const bad of [-1, 1.5, Number.NaN, Number.POSITIVE_INFINITY]) {
    await assert.rejects(() => fetchMessageImage('s', 'm', 'c', bad), /invalid image position/, String(bad));
  }
  assert.equal(requests.length, 0);
});

test('fetchMessageImage reports a server error', async () => {
  reply = () => new Response(JSON.stringify({error: 'image not found'}), {status: 404});
  await assert.rejects(() => fetchMessageImage('s', 'm', 'c', 0), /image not found/);
});

test('a streamed tool result keeps inlineImages, and a bad count is dropped as an invalid message', async () => {
  const streamed = inlineImages => ({
    id: 'turn-1',
    sender: 'assistant',
    status: 'streaming',
    text: '',
    timestamp: '2026-01-01T00:00:00.000Z',
    parts: [{type: 'tool_result', callId: 'c1', output: '[MCP image content]', status: 'success', ...(inlineImages === undefined ? {} : {inlineImages})}],
  });
  const received = async inlineImages => {
    const body = `event: ui.message\ndata: ${JSON.stringify({message: streamed(inlineImages)})}\n\nevent: done\ndata: {}\n\n`;
    reply = () => new Response(body, {status: 200, headers: {'Content-Type': 'text/event-stream'}});
    const messages = [];
    await attachSessionRun('s', {onMessage: value => messages.push(value)}).catch(() => undefined);
    return messages;
  };
  assert.equal((await received(2))[0]?.parts[0].inlineImages, 2);
  assert.equal((await received(undefined))[0]?.parts[0].inlineImages, undefined);
  for (const bad of ['2', -1, 1.5, null]) {
    assert.deepEqual(await received(bad), [], JSON.stringify(bad));
  }
});
