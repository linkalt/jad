import assert from 'node:assert/strict';
import test from 'node:test';
import {createAuthenticatedFetch, takeTokenFromFragment} from './api-auth.ts';

function fakeHistory() {
  const calls = [];
  return {calls, state: {keep: true}, replaceState: (state, _title, url) => calls.push({state, url})};
}

test('takeTokenFromFragment returns the token and strips only that parameter from the URL', () => {
  const history = fakeHistory();
  const token = takeTokenFromFragment(
    'api-token',
    {hash: '#view=live&api-token=%20secret-value%20', pathname: '/app', search: '?a=1'},
    history,
  );
  assert.equal(token, 'secret-value');
  assert.deepEqual(history.calls, [{state: {keep: true}, url: '/app?a=1#view=live'}]);
});

test('takeTokenFromFragment leaves the URL alone when the parameter is absent or empty', () => {
  for (const hash of ['', '#', '#view=live', '#api-token=', '#api-token=%20']) {
    const history = fakeHistory();
    assert.equal(takeTokenFromFragment('api-token', {hash, pathname: '/', search: ''}, history), null, hash);
    assert.equal(history.calls.length, 0, hash);
  }
  assert.equal(takeTokenFromFragment('api-token', undefined, undefined), null);
});

test('takeTokenFromFragment drops the whole fragment when the token was its only content', () => {
  const history = fakeHistory();
  assert.equal(takeTokenFromFragment('api-token', {hash: '#api-token=abc', pathname: '/', search: ''}, history), 'abc');
  assert.equal(history.calls[0].url, '/');
});

test('a fragment token authenticates without any prompt, even when storage is blocked', async () => {
  const previousWindow = globalThis.window;
  const previousStorage = globalThis.sessionStorage;
  const previousLocation = globalThis.location;
  const previousHistory = globalThis.history;
  let prompts = 0;
  const history = fakeHistory();
  Object.defineProperty(globalThis, 'sessionStorage', {
    configurable: true,
    get() { throw new Error('storage blocked'); },
  });
  globalThis.window = {prompt: () => { prompts += 1; return 'never'; }};
  globalThis.location = {hash: '#api-token=frag-secret', pathname: '/', search: ''};
  globalThis.history = history;
  try {
    const sent = [];
    const request = createAuthenticatedFetch('api-secret', async (_url, init) => {
      const header = new Headers(init.headers).get('Authorization');
      sent.push(header);
      return new Response(null, {status: header === 'Bearer frag-secret' ? 200 : 401});
    }, 'api-token');
    assert.equal((await request('/api/sessions')).status, 200);
    assert.equal((await request('/api/sessions')).status, 200);
    assert.deepEqual(sent, ['Bearer frag-secret', 'Bearer frag-secret']);
    assert.equal(prompts, 0);
    assert.equal(history.calls.length, 1);
  } finally {
    delete globalThis.sessionStorage;
    if (previousStorage !== undefined) globalThis.sessionStorage = previousStorage;
    if (previousWindow === undefined) delete globalThis.window;
    else globalThis.window = previousWindow;
    if (previousLocation === undefined) delete globalThis.location;
    else globalThis.location = previousLocation;
    if (previousHistory === undefined) delete globalThis.history;
    else globalThis.history = previousHistory;
  }
});

test('prompts once on 401, stores a tab token, and retries with Authorization', async () => {
  const previousWindow = globalThis.window;
  const previousStorage = globalThis.sessionStorage;
  const entries = new Map();
  const sent = [];
  let prompts = 0;
  globalThis.sessionStorage = {
    getItem: key => entries.get(key) ?? null,
    setItem: (key, value) => entries.set(key, value),
    removeItem: key => entries.delete(key),
  };
  globalThis.window = {prompt: () => { prompts += 1; return 'example-secret'; }};
  try {
    const request = createAuthenticatedFetch('api-secret', async (_url, init) => {
      const header = new Headers(init.headers).get('Authorization');
      sent.push(header);
      return new Response(null, {status: header === 'Bearer example-secret' ? 200 : 401});
    });
    assert.equal((await request('/api/sessions')).status, 200);
    assert.deepEqual(sent, [null, 'Bearer example-secret']);
    assert.equal(entries.get('api-secret'), 'example-secret');
    assert.equal((await request('/api/sessions')).status, 200);
    assert.equal(prompts, 1);
    assert.equal(sent.at(-1), 'Bearer example-secret');
  } finally {
    if (previousWindow === undefined) delete globalThis.window;
    else globalThis.window = previousWindow;
    if (previousStorage === undefined) delete globalThis.sessionStorage;
    else globalThis.sessionStorage = previousStorage;
  }
});

test('a rejected replacement token is removed instead of retried indefinitely', async () => {
  const previousWindow = globalThis.window;
  const previousStorage = globalThis.sessionStorage;
  const entries = new Map();
  globalThis.sessionStorage = {
    getItem: key => entries.get(key) ?? null,
    setItem: (key, value) => entries.set(key, value),
    removeItem: key => entries.delete(key),
  };
  globalThis.window = {prompt: () => 'wrong'};
  try {
    let requests = 0;
    const request = createAuthenticatedFetch('api-secret', async () => {
      requests += 1;
      return new Response(null, {status: 401});
    });
    assert.equal((await request('/api/sessions')).status, 401);
    assert.equal(requests, 2);
    assert.equal(entries.has('api-secret'), false);
  } finally {
    if (previousWindow === undefined) delete globalThis.window;
    else globalThis.window = previousWindow;
    if (previousStorage === undefined) delete globalThis.sessionStorage;
    else globalThis.sessionStorage = previousStorage;
  }
});
