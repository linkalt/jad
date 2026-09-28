import assert from 'node:assert/strict';
import {after, test} from 'node:test';

const originalFetch = globalThis.fetch;
const requests = [];
globalThis.fetch = (input, init = {}) => {
  requests.push({url: String(input), init});
  return new Promise((resolve, reject) => {
    const signal = init.signal;
    if (signal?.aborted) {
      reject(new DOMException('aborted', 'AbortError'));
      return;
    }
    signal?.addEventListener(
      'abort',
      () => reject(new DOMException('aborted', 'AbortError')),
      {once: true},
    );
  });
};

const {fetchSession, fetchSessionHistory, fetchSessions} = await import('./api.ts');

after(() => {
  globalThis.fetch = originalFetch;
});

async function assertAbortSignal(request) {
  const controller = new AbortController();
  const pending = request(controller.signal);
  assert.equal(requests.at(-1)?.init.signal, controller.signal);
  controller.abort();
  await assert.rejects(pending, error => error.name === 'AbortError');
}

test('conditional session and history fetches propagate cancellation signals', async () => {
  requests.length = 0;
  await assertAbortSignal(signal => fetchSessions(undefined, signal));
  await assertAbortSignal(signal => fetchSession('session-1', {signal}));
  await assertAbortSignal(signal => fetchSessionHistory('session-1', undefined, undefined, signal));
  assert.deepEqual(requests.map(request => new URL(request.url, 'http://localhost').pathname), [
    '/api/sessions',
    '/api/sessions/session-1',
    '/api/sessions/session-1/history',
  ]);
});
