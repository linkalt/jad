import assert from 'node:assert/strict';
import {after, test} from 'node:test';

const originalFetch = globalThis.fetch;
const requests = [];
const responses = [];
globalThis.fetch = async (input, init = {}) => {
  requests.push({url: String(input), method: init.method ?? 'GET'});
  const response = responses.shift();
  assert.ok(response, 'unexpected API request');
  return response;
};

const {attachSessionRun, streamSessionRun} = await import('./api.ts');

after(() => {
  globalThis.fetch = originalFetch;
});

function sse(event, data) {
  return `event: ${event}\ndata: ${JSON.stringify(data)}\n\n`;
}

function streamResponse(events) {
  const body = events.map(({event, data}) => sse(event, data)).join('');
  return new Response(body, {headers: {'Content-Type': 'text/event-stream'}});
}

function message(status, text) {
  return {
    id: 'turn-1',
    sender: 'assistant',
    status,
    text,
    timestamp: '2026-01-01T00:00:00.000Z',
    parts: [{type: 'text', text}],
  };
}

function completedStream() {
  const finalMessage = message('complete', 'the complete response');
  return streamResponse([
    {event: 'run', data: {turn_id: 'turn-1', started_at: '2026-01-01T00:00:00.000Z', status: 'running'}},
    {event: 'ui.message', data: {message: finalMessage}},
    {event: 'completed', data: {message: finalMessage, committed: true}},
    {event: 'done', data: {}},
  ]);
}

test('resumes an interrupted new run by attaching instead of posting again', async () => {
  requests.length = 0;
  responses.push(
    streamResponse([{event: 'ui.message', data: {message: message('streaming', 'partial')}}]),
    completedStream(),
  );
  const messages = [];
  const completions = [];

  const result = await streamSessionRun('session-1', 'hello', {
    onMessage: value => messages.push(value.text),
    onCompleted: value => completions.push(value.text),
  });

  assert.deepEqual(requests.map(request => request.method), ['POST', 'GET']);
  assert.match(requests[1].url, /\/sessions\/session-1\/run\/stream/);
  assert.deepEqual(messages, ['partial', 'the complete response']);
  assert.deepEqual(completions, ['the complete response']);
  assert.equal(result.message?.text, 'the complete response');
  assert.equal(result.attached, true);
});

test('reconnects an attached run after an incomplete stream and preserves its snapshot', async () => {
  requests.length = 0;
  responses.push(
    streamResponse([
      {event: 'run', data: {turn_id: 'turn-1', started_at: '2026-01-01T00:00:00.000Z', status: 'running'}},
      {event: 'ui.message', data: {message: message('streaming', 'partial')}},
    ]),
    completedStream(),
  );
  const result = await attachSessionRun('session-1');

  assert.deepEqual(requests.map(request => request.method), ['GET', 'GET']);
  assert.equal(result.attached, true);
  assert.equal(result.message?.text, 'the complete response');
});

test('does not reconnect after caller cancellation', async () => {
  requests.length = 0;
  responses.push(streamResponse([{event: 'ui.message', data: {message: message('streaming', 'partial')}}]));
  const controller = new AbortController();

  await assert.rejects(
    streamSessionRun('session-1', 'hello', {
      signal: controller.signal,
      onMessage: () => controller.abort(),
    }),
    /aborted/,
  );
  assert.deepEqual(requests.map(request => request.method), ['POST']);
});

test('does not reconnect after a provider error event', async () => {
  requests.length = 0;
  responses.push(streamResponse([{event: 'error', data: {message: 'provider failed'}}]));

  await assert.rejects(streamSessionRun('session-1', 'hello'), /provider failed/);
  assert.deepEqual(requests.map(request => request.method), ['POST']);
});
