import assert from 'node:assert/strict';
import {after, test} from 'node:test';

const originalFetch = globalThis.fetch;
const requests = [];
const responses = [];
globalThis.fetch = async (input, init = {}) => {
  requests.push({url: String(input), method: init.method ?? 'GET', body: init.body});
  const response = responses.shift();
  assert.ok(response, 'unexpected API request');
  return typeof response === 'function' ? response(init) : response;
};

const {attachSessionRun, streamSessionRun} = await import('./api.ts');
const {default: appConfig} = await import('./config.json', {with: {type: 'json'}});

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

/** A connection that delivers its frames and then stays open, silent, forever. */
function stalledStream(events) {
  const encoder = new TextEncoder();
  const body = events.map(({event, data}) => sse(event, data)).join('');
  return new Response(new ReadableStream({
    start(controller) {
      controller.enqueue(encoder.encode(body));
      // Never closed: this is the sleeping-laptop socket the watchdog exists for.
    },
  }), {headers: {'Content-Type': 'text/event-stream'}});
}

/** A connection whose frames arrive `gapMs` apart, each inside the stall window. */
function slowStream(events, gapMs) {
  const encoder = new TextEncoder();
  return new Response(new ReadableStream({
    start(controller) {
      events.forEach(({event, data}, index) => {
        setTimeout(() => {
          controller.enqueue(encoder.encode(sse(event, data)));
          if (index === events.length - 1) {
            controller.close();
          }
        }, gapMs * (index + 1));
      });
    },
  }), {headers: {'Content-Type': 'text/event-stream'}});
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
  assert.equal(JSON.parse(requests[0].body).supports_interaction, true);
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

test('keeps reconnecting after the configured batch when the run is known active', async () => {
  requests.length = 0;
  responses.length = 0;
  const originalConfig = {
    attempts: appConfig.run_stream_reconnect_max_attempts,
    baseDelay: appConfig.run_stream_reconnect_base_delay_ms,
    maxDelay: appConfig.run_stream_reconnect_max_delay_ms,
  };
  appConfig.run_stream_reconnect_max_attempts = 1;
  appConfig.run_stream_reconnect_base_delay_ms = 0;
  appConfig.run_stream_reconnect_max_delay_ms = 0;
  const runningRun = {
    turn_id: 'turn-1',
    started_at: '2026-01-01T00:00:00.000Z',
    status: 'running',
  };
  const observedRuns = [];
  responses.push(
    stalledStream([{event: 'run', data: runningRun}]),
    stalledStream([{event: 'run', data: runningRun}]),
    completedStream(),
  );

  try {
    const result = await streamSessionRun('session-1', 'hello', {
      stallTimeoutMs: 5,
      onRun: run => observedRuns.push(run.turn_id),
    });

    assert.deepEqual(requests.map(request => request.method), ['POST', 'GET', 'GET']);
    assert.deepEqual(observedRuns, ['turn-1', 'turn-1', 'turn-1']);
    assert.equal(result.message?.text, 'the complete response');
  } finally {
    appConfig.run_stream_reconnect_max_attempts = originalConfig.attempts;
    appConfig.run_stream_reconnect_base_delay_ms = originalConfig.baseDelay;
    appConfig.run_stream_reconnect_max_delay_ms = originalConfig.maxDelay;
  }
});

test('retries a temporary server error when attaching to a run', async () => {
  requests.length = 0;
  responses.length = 0;
  responses.push(new Response('temporarily unavailable', {status: 503}), completedStream());

  const result = await attachSessionRun('session-1');

  assert.deepEqual(requests.map(request => request.method), ['GET', 'GET']);
  assert.equal(result.message?.text, 'the complete response');
});

test('keeps an attached run connected after the retry batch limit', async () => {
  requests.length = 0;
  responses.length = 0;
  const originalConfig = {
    attempts: appConfig.run_stream_reconnect_max_attempts,
    baseDelay: appConfig.run_stream_reconnect_base_delay_ms,
    maxDelay: appConfig.run_stream_reconnect_max_delay_ms,
  };
  appConfig.run_stream_reconnect_max_attempts = 1;
  appConfig.run_stream_reconnect_base_delay_ms = 0;
  appConfig.run_stream_reconnect_max_delay_ms = 0;
  const runningRun = {
    turn_id: 'turn-1',
    started_at: '2026-01-01T00:00:00.000Z',
    status: 'running',
  };
  responses.push(
    stalledStream([{event: 'run', data: runningRun}]),
    stalledStream([{event: 'run', data: runningRun}]),
    completedStream(),
  );

  try {
    const result = await attachSessionRun('session-1', {stallTimeoutMs: 5});

    assert.deepEqual(requests.map(request => request.method), ['GET', 'GET', 'GET']);
    assert.equal(result.attached, true);
    assert.equal(result.message?.text, 'the complete response');
  } finally {
    appConfig.run_stream_reconnect_max_attempts = originalConfig.attempts;
    appConfig.run_stream_reconnect_base_delay_ms = originalConfig.baseDelay;
    appConfig.run_stream_reconnect_max_delay_ms = originalConfig.maxDelay;
  }
});

test('reconnects when the initial run response never sends headers', async () => {
  requests.length = 0;
  responses.length = 0;
  let aborted = false;
  responses.push(
    init => new Promise((_resolve, reject) => {
      const signal = init.signal;
      assert.ok(signal, 'the request must have a timeout signal');
      const onAbort = () => {
        aborted = true;
        reject(signal.reason ?? new Error('aborted'));
      };
      if (signal.aborted) onAbort();
      else signal.addEventListener('abort', onAbort, {once: true});
    }),
    completedStream(),
  );

  const result = await streamSessionRun('session-1', 'hello', {stallTimeoutMs: 30});

  assert.equal(aborted, true);
  assert.deepEqual(requests.map(request => request.method), ['POST', 'GET']);
  assert.equal(result.message?.text, 'the complete response');
});

test('replaces a stream that goes silent instead of waiting forever', async () => {
  requests.length = 0;
  responses.push(
    stalledStream([{event: 'ui.message', data: {message: message('streaming', 'partial')}}]),
    completedStream(),
  );

  const result = await streamSessionRun('session-1', 'hello', {stallTimeoutMs: 30});

  assert.deepEqual(requests.map(request => request.method), ['POST', 'GET']);
  assert.equal(result.message?.text, 'the complete response');
});

test('a stream that keeps sending frames is never treated as stalled', async () => {
  requests.length = 0;
  const finalMessage = message('complete', 'kept alive');
  responses.push(
    slowStream([
      {event: 'ui.message', data: {message: message('streaming', 'partial')}},
      {event: 'ui.message', data: {message: finalMessage}},
      {event: 'completed', data: {message: finalMessage, committed: true}},
      {event: 'done', data: {}},
    ], 15),
  );

  const result = await streamSessionRun('session-1', 'hello', {stallTimeoutMs: 60});

  assert.deepEqual(requests.map(request => request.method), ['POST']);
  assert.equal(result.message?.text, 'kept alive');
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
