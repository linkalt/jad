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

/** Polls until `check()` is true, failing after `limitMs` instead of racing a fixed sleep. */
async function waitUntil(check, limitMs = 2000) {
  const deadline = Date.now() + limitMs;
  while (!check()) {
    assert.ok(Date.now() < deadline, 'condition not reached in time');
    await new Promise(resolve => setTimeout(resolve, 2));
  }
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

test('the run request carries the per-run model only when one is given', async () => {
  requests.length = 0;
  responses.length = 0;
  responses.push(completedStream(), completedStream());

  await streamSessionRun('session-1', 'hello', {model: 'x@high'});
  await streamSessionRun('session-1', 'hello', {});

  assert.equal(JSON.parse(requests[0].body).model, 'x@high');
  assert.equal('model' in JSON.parse(requests[1].body), false);
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

test('keeps a pending interaction open until the run explicitly completes', async () => {
  requests.length = 0;
  responses.length = 0;
  const runningRun = {
    turn_id: 'turn-1',
    started_at: '2026-01-01T00:00:00.000Z',
    status: 'running',
  };
  const pending = {
    id: 'interaction-1',
    tool: 'request_draft',
    tool_call_id: 'call-draft',
    draft: {goal: 'write a file'},
    created_at: '2026-01-01T00:00:00.000Z',
    expires_at: '2026-01-01T00:01:00.000Z',
    max_expires_at: '2026-01-01T00:02:00.000Z',
    extend_ms: 30000,
  };
  const pendingMessage = {...message('complete', 'Draft proposed'), pending};
  const finalMessage = message('complete', 'The approved plan is ready');
  responses.push(
    stalledStream([
      {event: 'run', data: runningRun},
      {event: 'interaction', data: {turn_id: 'turn-1', pending}},
      {event: 'ui.message', data: {message: pendingMessage}},
    ]),
    streamResponse([
      {event: 'run', data: runningRun},
      {event: 'interaction', data: {turn_id: 'turn-1', pending}},
      {event: 'ui.message', data: {message: pendingMessage}},
      {event: 'completed', data: {message: finalMessage, committed: true}},
      {event: 'done', data: {}},
    ]),
  );
  const interactions = [];
  const completions = [];

  const result = await streamSessionRun('session-1', 'hello', {
    stallTimeoutMs: 5,
    onInteraction: (turnId, value) => interactions.push({turnId, id: value?.id}),
    onCompleted: value => completions.push(value.text),
  });

  assert.deepEqual(requests.map(request => request.method), ['POST', 'GET']);
  assert.deepEqual(interactions, [
    {turnId: 'turn-1', id: 'interaction-1'},
    {turnId: 'turn-1', id: 'interaction-1'},
  ]);
  assert.deepEqual(completions, ['The approved plan is ready']);
  assert.equal(result.message?.text, 'The approved plan is ready');
});

test('delivers a pending interaction before any message snapshot', async () => {
  requests.length = 0;
  responses.length = 0;
  const pending = {
    id: 'interaction-2',
    tool: 'request_draft',
    tool_call_id: 'call-draft-2',
    draft: {goal: 'review a plan'},
    created_at: '2026-01-01T00:00:00.000Z',
    expires_at: '2026-01-01T00:01:00.000Z',
    max_expires_at: '2026-01-01T00:02:00.000Z',
    extend_ms: 30000,
  };
  const finalMessage = message('complete', 'The run continued after the answer');
  responses.push(streamResponse([
    {event: 'run', data: {turn_id: 'turn-1', started_at: '2026-01-01T00:00:00.000Z', status: 'running'}},
    {event: 'interaction', data: {turn_id: 'turn-1', pending}},
    {event: 'completed', data: {message: finalMessage, committed: true}},
    {event: 'done', data: {}},
  ]));
  const interactions = [];
  const messages = [];

  const result = await streamSessionRun('session-1', 'hello', {
    onInteraction: (turnId, value) => interactions.push({turnId, id: value?.id}),
    onMessage: value => messages.push(value.text),
  });

  assert.deepEqual(interactions, [{turnId: 'turn-1', id: 'interaction-2'}]);
  assert.deepEqual(messages, []);
  assert.equal(result.message?.text, 'The run continued after the answer');
});

test('an interaction event that clears a question reports why it ended, and only when it says', async () => {
  requests.length = 0;
  responses.length = 0;
  const finalMessage = message('complete', 'all done');
  const ended = {interaction_id: 'i1', tool: 'request_draft', outcome: 'answered', decision: 'decline'};
  responses.push(streamResponse([
    {event: 'run', data: {turn_id: 'turn-1', started_at: '2026-01-01T00:00:00.000Z', status: 'running'}},
    {event: 'interaction', data: {turn_id: 'turn-1', pending: null, ended}},
    {event: 'interaction', data: {turn_id: 'turn-1', pending: null}},
    {event: 'interaction', data: {turn_id: 'turn-1', pending: null, ended: {...ended, outcome: 'nonsense'}}},
    {event: 'interaction', data: {turn_id: 'turn-1', pending: null, ended: {interaction_id: 'i2', tool: 'request_draft', outcome: 'timed_out'}}},
    {event: 'completed', data: {message: finalMessage, committed: true}},
    {event: 'done', data: {}},
  ]));
  const cleared = [];
  const ends = [];

  await streamSessionRun('session-1', 'hello', {
    onInteraction: (turnId, value) => cleared.push({turnId, id: value?.id}),
    onInteractionEnd: (turnId, end) => ends.push({turnId, id: end.interactionId, outcome: end.outcome, decision: end.decision}),
  });

  assert.equal(cleared.length, 4, 'every event still clears the question, with or without a reason');
  assert.deepEqual(ends, [
    {turnId: 'turn-1', id: 'i1', outcome: 'answered', decision: 'decline'},
    {turnId: 'turn-1', id: 'i2', outcome: 'timed_out', decision: undefined},
  ], 'an absent or malformed reason is not reported');
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

test('reports each reconnect attempt and clears the state once the stream is back', async () => {
  requests.length = 0;
  responses.length = 0;
  const originalConfig = {
    baseDelay: appConfig.run_stream_reconnect_base_delay_ms,
    maxDelay: appConfig.run_stream_reconnect_max_delay_ms,
  };
  appConfig.run_stream_reconnect_base_delay_ms = 0;
  appConfig.run_stream_reconnect_max_delay_ms = 0;
  const runningRun = {turn_id: 'turn-1', started_at: '2026-01-01T00:00:00.000Z', status: 'running'};
  responses.push(
    stalledStream([{event: 'run', data: runningRun}]),
    new Response('upstream down', {status: 503}),
    completedStream(),
  );
  const states = [];

  try {
    const result = await streamSessionRun('session-1', 'hello', {
      stallTimeoutMs: 5,
      onReconnecting: state => states.push(state),
    });

    assert.equal(result.message?.text, 'the complete response');
    const attempts = states.filter(state => state !== undefined).map(state => state.attempt);
    assert.ok(attempts.includes(2), 'the second attempt is reported');
    assert.deepEqual(attempts, [...attempts].sort((left, right) => left - right), 'attempts never go back');
    assert.ok(states.every(state => state === undefined || state.offline === false));
    assert.equal(states.at(-1), undefined, 'the notice never outlives the reconnect');
  } finally {
    appConfig.run_stream_reconnect_base_delay_ms = originalConfig.baseDelay;
    appConfig.run_stream_reconnect_max_delay_ms = originalConfig.maxDelay;
  }
});

test('waits for the browser to come back online instead of burning attempts', async () => {
  requests.length = 0;
  responses.length = 0;
  const originalConfig = {
    baseDelay: appConfig.run_stream_reconnect_base_delay_ms,
    maxDelay: appConfig.run_stream_reconnect_max_delay_ms,
  };
  appConfig.run_stream_reconnect_base_delay_ms = 0;
  appConfig.run_stream_reconnect_max_delay_ms = 0;
  const runningRun = {turn_id: 'turn-1', started_at: '2026-01-01T00:00:00.000Z', status: 'running'};
  responses.push(
    stalledStream([{event: 'run', data: runningRun}]),
    completedStream(),
  );
  const states = [];
  const hadNavigator = Object.getOwnPropertyDescriptor(globalThis, 'navigator');
  let online = false;
  Object.defineProperty(globalThis, 'navigator', {configurable: true, value: {get onLine() { return online; }}});
  const listeners = new Set();
  const originalAdd = globalThis.addEventListener;
  const originalRemove = globalThis.removeEventListener;
  globalThis.addEventListener = (type, listener) => { if (type === 'online') listeners.add(listener); };
  globalThis.removeEventListener = (type, listener) => { if (type === 'online') listeners.delete(listener); };

  try {
    const run = streamSessionRun('session-1', 'hello', {
      stallTimeoutMs: 5,
      onReconnecting: state => states.push(state),
    });
    await waitUntil(() => listeners.size === 1);
    assert.equal(requests.length, 1, 'no reconnect request is made while offline');
    assert.deepEqual(states.at(-1), {attempt: 0, offline: true});
    assert.equal(listeners.size, 1, 'the wait listens for the online event');

    online = true;
    for (const listener of [...listeners]) listener();
    const result = await run;

    assert.equal(result.message?.text, 'the complete response');
    assert.equal(requests.length, 2);
    assert.equal(listeners.size, 0, 'the online listener is removed');
  } finally {
    if (hadNavigator) Object.defineProperty(globalThis, 'navigator', hadNavigator);
    else delete globalThis.navigator;
    globalThis.addEventListener = originalAdd;
    globalThis.removeEventListener = originalRemove;
    appConfig.run_stream_reconnect_base_delay_ms = originalConfig.baseDelay;
    appConfig.run_stream_reconnect_max_delay_ms = originalConfig.maxDelay;
  }
});

test('a browser that keeps reporting offline is retried after the recheck interval', async () => {
  requests.length = 0;
  responses.length = 0;
  const originalConfig = {
    baseDelay: appConfig.run_stream_reconnect_base_delay_ms,
    maxDelay: appConfig.run_stream_reconnect_max_delay_ms,
    recheck: appConfig.run_stream_offline_recheck_ms,
  };
  appConfig.run_stream_reconnect_base_delay_ms = 0;
  appConfig.run_stream_reconnect_max_delay_ms = 0;
  appConfig.run_stream_offline_recheck_ms = 20;
  const runningRun = {turn_id: 'turn-1', started_at: '2026-01-01T00:00:00.000Z', status: 'running'};
  responses.push(stalledStream([{event: 'run', data: runningRun}]), completedStream());
  const hadNavigator = Object.getOwnPropertyDescriptor(globalThis, 'navigator');
  Object.defineProperty(globalThis, 'navigator', {configurable: true, value: {onLine: false}});

  try {
    const result = await streamSessionRun('session-1', 'hello', {stallTimeoutMs: 5});
    assert.equal(result.message?.text, 'the complete response');
    assert.equal(requests.length, 2, 'the run is retried although the browser never says online');
  } finally {
    if (hadNavigator) Object.defineProperty(globalThis, 'navigator', hadNavigator);
    else delete globalThis.navigator;
    appConfig.run_stream_reconnect_base_delay_ms = originalConfig.baseDelay;
    appConfig.run_stream_reconnect_max_delay_ms = originalConfig.maxDelay;
    appConfig.run_stream_offline_recheck_ms = originalConfig.recheck;
  }
});

test('aborting while offline ends the wait and clears the reconnect state', async () => {
  requests.length = 0;
  responses.length = 0;
  const originalConfig = {
    baseDelay: appConfig.run_stream_reconnect_base_delay_ms,
    maxDelay: appConfig.run_stream_reconnect_max_delay_ms,
  };
  appConfig.run_stream_reconnect_base_delay_ms = 0;
  appConfig.run_stream_reconnect_max_delay_ms = 0;
  const runningRun = {turn_id: 'turn-1', started_at: '2026-01-01T00:00:00.000Z', status: 'running'};
  responses.push(stalledStream([{event: 'run', data: runningRun}]));
  const states = [];
  const hadNavigator = Object.getOwnPropertyDescriptor(globalThis, 'navigator');
  Object.defineProperty(globalThis, 'navigator', {configurable: true, value: {onLine: false}});
  const listeners = new Set();
  const originalAdd = globalThis.addEventListener;
  const originalRemove = globalThis.removeEventListener;
  globalThis.addEventListener = (type, listener) => { if (type === 'online') listeners.add(listener); };
  globalThis.removeEventListener = (type, listener) => { if (type === 'online') listeners.delete(listener); };
  const controller = new AbortController();

  try {
    const run = streamSessionRun('session-1', 'hello', {
      stallTimeoutMs: 5,
      signal: controller.signal,
      onReconnecting: state => states.push(state),
    });
    const settled = run.then(() => 'resolved', () => 'rejected');
    await waitUntil(() => listeners.size === 1);
    controller.abort();

    assert.equal(await settled, 'rejected');
    assert.equal(listeners.size, 0, 'abort removes the online listener');
    assert.equal(states.at(-1), undefined);
  } finally {
    if (hadNavigator) Object.defineProperty(globalThis, 'navigator', hadNavigator);
    else delete globalThis.navigator;
    globalThis.addEventListener = originalAdd;
    globalThis.removeEventListener = originalRemove;
    appConfig.run_stream_reconnect_base_delay_ms = originalConfig.baseDelay;
    appConfig.run_stream_reconnect_max_delay_ms = originalConfig.maxDelay;
  }
});

function hiddenTabHarness() {
  const listeners = new Set();
  const original = Object.getOwnPropertyDescriptor(globalThis, 'document');
  const doc = {
    visibilityState: 'hidden',
    addEventListener: (type, listener) => { if (type === 'visibilitychange') listeners.add(listener); },
    removeEventListener: (type, listener) => { if (type === 'visibilitychange') listeners.delete(listener); },
  };
  Object.defineProperty(globalThis, 'document', {configurable: true, value: doc});
  const config = {
    baseDelay: appConfig.run_stream_reconnect_base_delay_ms,
    maxDelay: appConfig.run_stream_reconnect_max_delay_ms,
    recheck: appConfig.run_stream_hidden_recheck_ms,
  };
  appConfig.run_stream_reconnect_base_delay_ms = 0;
  appConfig.run_stream_reconnect_max_delay_ms = 0;
  return {
    doc,
    listeners,
    restore() {
      if (original) Object.defineProperty(globalThis, 'document', original);
      else delete globalThis.document;
      appConfig.run_stream_reconnect_base_delay_ms = config.baseDelay;
      appConfig.run_stream_reconnect_max_delay_ms = config.maxDelay;
      appConfig.run_stream_hidden_recheck_ms = config.recheck;
    },
  };
}

test('a hidden tab waits for the page to be shown before reconnecting', async () => {
  requests.length = 0;
  responses.length = 0;
  const runningRun = {turn_id: 'turn-1', started_at: '2026-01-01T00:00:00.000Z', status: 'running'};
  responses.push(
    stalledStream([{event: 'run', data: runningRun}]),
    completedStream(),
  );
  const harness = hiddenTabHarness();
  appConfig.run_stream_hidden_recheck_ms = 60000;
  try {
    const run = streamSessionRun('session-1', 'hello', {stallTimeoutMs: 5});
    await waitUntil(() => harness.listeners.size === 1);
    assert.equal(requests.length, 1, 'no reconnect request is made while the tab is hidden');

    harness.doc.visibilityState = 'visible';
    for (const listener of [...harness.listeners]) listener();
    const result = await run;

    assert.equal(result.message?.text, 'the complete response');
    assert.equal(requests.length, 2);
    assert.equal(harness.listeners.size, 0, 'the listener is removed once the wait ends');
  } finally {
    harness.restore();
  }
});

test('a tab that stays hidden still reconnects after the recheck interval', async () => {
  requests.length = 0;
  responses.length = 0;
  const runningRun = {turn_id: 'turn-1', started_at: '2026-01-01T00:00:00.000Z', status: 'running'};
  responses.push(
    stalledStream([{event: 'run', data: runningRun}]),
    completedStream(),
  );
  const harness = hiddenTabHarness();
  appConfig.run_stream_hidden_recheck_ms = 20;
  try {
    const result = await streamSessionRun('session-1', 'hello', {stallTimeoutMs: 5});
    assert.equal(result.message?.text, 'the complete response');
    assert.equal(requests.length, 2, 'the hidden tab retried without a visibility event');
    assert.equal(harness.listeners.size, 0);
  } finally {
    harness.restore();
  }
});

test('aborting while the tab is hidden ends the wait and removes the listener', async () => {
  requests.length = 0;
  responses.length = 0;
  const runningRun = {turn_id: 'turn-1', started_at: '2026-01-01T00:00:00.000Z', status: 'running'};
  responses.push(stalledStream([{event: 'run', data: runningRun}]));
  const harness = hiddenTabHarness();
  appConfig.run_stream_hidden_recheck_ms = 60000;
  const controller = new AbortController();
  try {
    const run = streamSessionRun('session-1', 'hello', {stallTimeoutMs: 5, signal: controller.signal});
    const settled = run.then(() => 'resolved', () => 'rejected');
    await waitUntil(() => harness.listeners.size === 1);
    controller.abort();
    assert.equal(await settled, 'rejected');
    assert.equal(harness.listeners.size, 0);
  } finally {
    harness.restore();
  }
});

test('a reconnect that finds no run reports an unknown outcome instead of failing', async () => {
  requests.length = 0;
  responses.length = 0;
  const originalConfig = {
    baseDelay: appConfig.run_stream_reconnect_base_delay_ms,
    maxDelay: appConfig.run_stream_reconnect_max_delay_ms,
  };
  appConfig.run_stream_reconnect_base_delay_ms = 0;
  appConfig.run_stream_reconnect_max_delay_ms = 0;
  const runningRun = {
    turn_id: 'turn-1',
    started_at: '2026-01-01T00:00:00.000Z',
    status: 'running',
  };
  // The run finished while the connection was down and its retained state has
  // expired, so the server answers `idle`. The run did not fail: the committed
  // transcript holds the outcome.
  responses.push(
    stalledStream([{event: 'run', data: runningRun}]),
    streamResponse([{event: 'idle', data: {}}]),
  );

  try {
    const result = await streamSessionRun('session-1', 'hello', {stallTimeoutMs: 5});

    assert.deepEqual(requests.map(request => request.method), ['POST', 'GET']);
    assert.equal(result.idle, true);
    assert.equal(result.message, undefined);
    assert.equal(result.attached, true);
  } finally {
    appConfig.run_stream_reconnect_base_delay_ms = originalConfig.baseDelay;
    appConfig.run_stream_reconnect_max_delay_ms = originalConfig.maxDelay;
  }
});

test('parses CRLF frames wherever the network splits the stream', async () => {
  const finalMessage = message('complete', 'the complete response');
  const events = [
    {event: 'run', data: {turn_id: 'turn-1', started_at: '2026-01-01T00:00:00.000Z', status: 'running'}},
    {event: 'completed', data: {message: finalMessage, committed: true}},
    {event: 'done', data: {}},
  ];
  const wire = new TextEncoder().encode(
    events.map(({event, data}) => sse(event, data)).join('').replace(/\n/g, '\r\n'),
  );
  const failures = [];
  const originalConfig = {
    attempts: appConfig.run_stream_reconnect_max_attempts,
    baseDelay: appConfig.run_stream_reconnect_base_delay_ms,
    maxDelay: appConfig.run_stream_reconnect_max_delay_ms,
  };
  appConfig.run_stream_reconnect_max_attempts = 1;
  appConfig.run_stream_reconnect_base_delay_ms = 0;
  appConfig.run_stream_reconnect_max_delay_ms = 0;

  try {
    for (let offset = 1; offset < wire.length; offset += 1) {
      requests.length = 0;
      responses.length = 0;
      responses.push(new Response(new ReadableStream({
        start(controller) {
          controller.enqueue(wire.slice(0, offset));
          controller.enqueue(wire.slice(offset));
          controller.close();
        },
      }), {headers: {'Content-Type': 'text/event-stream'}}));

      // A lost frame leaves a known-active run, which the client retries without
      // limit. Aborting on the first retry turns that into a recorded failure.
      const controller = new AbortController();
      responses.push(() => {
        controller.abort();
        throw new Error('the first connection did not deliver every frame');
      });

      try {
        const result = await attachSessionRun('session-1', {signal: controller.signal});
        if (requests.length !== 1 || result.message?.text !== 'the complete response') {
          failures.push(offset);
        }
      } catch {
        failures.push(offset);
      }
    }
  } finally {
    appConfig.run_stream_reconnect_max_attempts = originalConfig.attempts;
    appConfig.run_stream_reconnect_base_delay_ms = originalConfig.baseDelay;
    appConfig.run_stream_reconnect_max_delay_ms = originalConfig.maxDelay;
  }

  assert.deepEqual(failures, []);
});

test('a large frame split into many small chunks is parsed in linear time', async () => {
  const text = 'x'.repeat(8 * 1024 * 1024);
  const finalMessage = message('complete', text);
  const wire = new TextEncoder().encode(
    [
      {event: 'run', data: {turn_id: 'turn-1', started_at: '2026-01-01T00:00:00.000Z', status: 'running'}},
      {event: 'completed', data: {message: finalMessage, committed: true}},
      {event: 'done', data: {}},
    ].map(({event, data}) => sse(event, data)).join('').replace(/\n/g, '\r\n'),
  );
  const chunkSize = 1024;
  requests.length = 0;
  responses.length = 0;
  responses.push(new Response(new ReadableStream({
    start(controller) {
      for (let offset = 0; offset < wire.length; offset += chunkSize) {
        controller.enqueue(wire.slice(offset, offset + chunkSize));
      }
      controller.close();
    },
  }), {headers: {'Content-Type': 'text/event-stream'}}));

  const started = performance.now();
  const result = await attachSessionRun('session-1');
  const elapsedMs = performance.now() - started;

  assert.equal(result.message?.text.length, text.length);
  assert.equal(requests.length, 1);
  assert.ok(elapsedMs < 3000, `parsing took ${Math.round(elapsedMs)} ms`);
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

test('a keep-alive comment frame resets the stall watchdog and is otherwise ignored', async () => {
  requests.length = 0;
  responses.length = 0;
  const encoder = new TextEncoder();
  const runningRun = {turn_id: 'turn-1', started_at: '2026-01-01T00:00:00.000Z', status: 'running'};
  const finalMessage = message('complete', 'finished after a long tool call');
  // The run is silent for three times the stall window, but the server sends
  // comment frames inside every window, so the watchdog must not reconnect.
  responses.push(new Response(new ReadableStream({
    start(controller) {
      const stallMs = 60;
      controller.enqueue(encoder.encode(sse('run', runningRun)));
      let beats = 0;
      const timer = setInterval(() => {
        controller.enqueue(encoder.encode(': keep-alive\n\n'));
        beats += 1;
        if (beats === 8) {
          clearInterval(timer);
          controller.enqueue(encoder.encode(sse('completed', {message: finalMessage, committed: true})));
          controller.enqueue(encoder.encode(sse('done', {})));
          controller.close();
        }
      }, stallMs / 2);
    },
  }), {headers: {'Content-Type': 'text/event-stream'}}));

  const events = [];
  const result = await streamSessionRun('session-1', 'run a slow tool', {
    stallTimeoutMs: 60,
    onRun: () => events.push('run'),
    onMessage: () => events.push('message'),
    onCompleted: value => events.push(`completed:${value.text}`),
  });

  // One POST, no reconnect GET, and no event was invented for the comments.
  assert.deepEqual(requests.map(request => request.method), ['POST']);
  assert.deepEqual(events, ['run', 'completed:finished after a long tool call']);
  assert.equal(result.message?.text, 'finished after a long tool call');
});

test('a hook_report event reaches the caller, and a malformed one is ignored', async () => {
  requests.length = 0;
  responses.length = 0;
  const runningRun = {turn_id: 'turn-1', started_at: '2026-01-01T00:00:00.000Z', status: 'running'};
  const finalMessage = message('complete', 'done');
  responses.push(streamResponse([
    {event: 'run', data: runningRun},
    {event: 'hook_report', data: {turn_id: 'turn-1', event: 'run_start', trigger: '', report: '[a] passed (1 ms, in .)'}},
    {event: 'hook_report', data: {turn_id: 'turn-1', event: 'pre_tool', trigger: '', report: 'nope'}},
    {event: 'hook_report', data: {event: 'run_end', report: 'no turn'}},
    {event: 'hook_report', data: {turn_id: 'turn-1', event: 'pre_compact', trigger: 'automatic', report: '[b] failed, exit 1 (2 ms, in .)'}},
    {event: 'completed', data: {message: finalMessage, committed: true}},
    {event: 'done', data: {}},
  ]));
  const seen = [];
  await streamSessionRun('session-1', 'hello', {
    onHookReport: (turnId, report) => seen.push([turnId, report.event, report.trigger, report.report]),
  });
  assert.deepEqual(seen, [
    ['turn-1', 'run_start', '', '[a] passed (1 ms, in .)'],
    ['turn-1', 'pre_compact', 'automatic', '[b] failed, exit 1 (2 ms, in .)'],
  ]);
});

test('a provider_retry event reaches the caller as a wait, and null or a malformed one clears it', async () => {
  requests.length = 0;
  responses.length = 0;
  const runningRun = {turn_id: 'turn-1', started_at: '2026-01-01T00:00:00.000Z', status: 'running'};
  const finalMessage = message('complete', 'recovered');
  const wait = {attempt: 1, max_retries: 3, status: 429, wait_ms: 2000, resume_at: '2026-01-01T00:00:02.000Z'};
  responses.push(streamResponse([
    {event: 'run', data: runningRun},
    {event: 'provider_retry', data: {turn_id: 'turn-1', waiting: wait}},
    {event: 'provider_retry', data: {turn_id: 'turn-1', waiting: {...wait, attempt: 2, status: null}}},
    {event: 'provider_retry', data: {turn_id: 'turn-1', waiting: {attempt: 'two'}}},
    {event: 'provider_retry', data: {turn_id: 'turn-1', waiting: wait}},
    {event: 'provider_retry', data: {turn_id: 'turn-1', waiting: null}},
    {event: 'provider_retry', data: {waiting: wait}},
    {event: 'completed', data: {message: finalMessage, committed: true}},
    {event: 'done', data: {}},
  ]));
  const seen = [];
  const result = await streamSessionRun('session-1', 'hello', {
    onProviderRetry: (turnId, value) => seen.push({turnId, attempt: value?.attempt, status: value?.status, resumeAtMs: value?.resumeAtMs}),
  });
  assert.deepEqual(seen, [
    {turnId: 'turn-1', attempt: 1, status: 429, resumeAtMs: Date.parse('2026-01-01T00:00:02.000Z')},
    {turnId: 'turn-1', attempt: 2, status: null, resumeAtMs: Date.parse('2026-01-01T00:00:02.000Z')},
    {turnId: 'turn-1', attempt: undefined, status: undefined, resumeAtMs: undefined},
    {turnId: 'turn-1', attempt: 1, status: 429, resumeAtMs: Date.parse('2026-01-01T00:00:02.000Z')},
    {turnId: 'turn-1', attempt: undefined, status: undefined, resumeAtMs: undefined},
  ], 'the event without a turn_id is ignored');
  assert.equal(result.message?.text, 'recovered');
});

test('a run that never retries, and a caller that does not listen, are unaffected by provider_retry', async () => {
  requests.length = 0;
  responses.length = 0;
  responses.push(streamResponse([
    {event: 'run', data: {turn_id: 'turn-1', started_at: '2026-01-01T00:00:00.000Z', status: 'running'}},
    {event: 'provider_retry', data: {turn_id: 'turn-1', waiting: {attempt: 1, max_retries: 2, status: 503, wait_ms: 1, resume_at: '2026-01-01T00:00:00.001Z'}}},
    {event: 'completed', data: {message: message('complete', 'fine'), committed: true}},
    {event: 'done', data: {}},
  ]));
  const result = await streamSessionRun('session-1', 'hello', {});
  assert.equal(result.message?.text, 'fine');
});

test('accepts a hosted_tool part and cited text, and drops a message whose hosted tool or citation part is malformed', async () => {
  requests.length = 0;
  responses.length = 0;
  const run = {turn_id: 'turn-1', started_at: '2026-01-01T00:00:00.000Z', status: 'running'};
  const withParts = (id, parts) => ({id, sender: 'assistant', status: 'streaming', text: 'x', timestamp: 't', parts});
  const good = withParts('good', [
    {type: 'hosted_tool', callId: 'ws_1', name: 'web_search', target: 'go release', status: 'completed', raw: '{"id":"ws_1"}'},
    {type: 'text', text: 'x', citations: [{url: 'https://go.dev', title: 'Go'}, {url: 'https://pkg.go.dev'}]},
  ]);
  const noCallId = withParts('no-call-id', [{type: 'hosted_tool', name: 'web_search', target: 'q'}]);
  const badQuery = withParts('bad-query', [{type: 'hosted_tool', callId: 'a', name: 'web_search', target: 5}]);
  const noName = withParts('no-name', [{type: 'hosted_tool', callId: 'a'}]);
  const badRaw = withParts('bad-raw', [{type: 'hosted_tool', callId: 'a', name: 'x', raw: {id: 1}}]);
  const badCitationUrl = withParts('bad-url', [{type: 'text', text: 'x', citations: [{url: 1}]}]);
  const badCitationList = withParts('bad-list', [{type: 'text', text: 'x', citations: 'https://go.dev'}]);
  const badTitle = withParts('bad-title', [{type: 'text', text: 'x', citations: [{url: 'https://go.dev', title: 3}]}]);
  const finalMessage = message('complete', 'done');
  responses.push(streamResponse([
    {event: 'run', data: run},
    {event: 'ui.message', data: {message: good}},
    {event: 'ui.message', data: {message: noCallId}},
    {event: 'ui.message', data: {message: badQuery}},
    {event: 'ui.message', data: {message: noName}},
    {event: 'ui.message', data: {message: badRaw}},
    {event: 'ui.message', data: {message: badCitationUrl}},
    {event: 'ui.message', data: {message: badCitationList}},
    {event: 'ui.message', data: {message: badTitle}},
    {event: 'ui.part', data: {id: 'good', index: 0, part: {type: 'hosted_tool', callId: 'ws_2', name: 'openrouter:web_fetch'}}},
    {event: 'ui.part', data: {id: 'good', index: 1, part: {type: 'hosted_tool', name: 'x'}}},
    {event: 'completed', data: {message: finalMessage, committed: true}},
    {event: 'done', data: {}},
  ]));
  const seen = [];
  const parts = [];
  await streamSessionRun('session-1', 'hello', {
    onMessage: value => seen.push(value.id),
    onPart: (id, index, part) => parts.push(`${id}:${index}:${part.callId}`),
  });
  assert.deepEqual(seen, ['good']);
  assert.deepEqual(parts, ['good:0:ws_2']);
});

test('an async_state event reaches the caller as counts, and a malformed one or an unknown event is ignored', async () => {
  requests.length = 0;
  responses.length = 0;
  responses.push(streamResponse([
    {event: 'run', data: {turn_id: 'turn-1', started_at: '2026-01-01T00:00:00.000Z', status: 'running'}},
    {event: 'async_state', data: {turn_id: 'turn-1', active: 2, unread: 1, ids: ['a', 'b']}},
    {event: 'async_state', data: {turn_id: 'turn-1', active: -1, unread: 0, ids: []}},
    {event: 'async_state', data: {turn_id: 'turn-1', active: 'two', unread: 0}},
    {event: 'async_state', data: {active: 1, unread: 0}},
    {event: 'async_state', data: null},
    {event: 'a_future_event', data: {anything: true}},
    {event: 'async_state', data: {turn_id: 'turn-1', active: 0, unread: 3, ids: []}},
    {event: 'completed', data: {message: message('complete', 'fine'), committed: true}},
    {event: 'done', data: {}},
  ]));
  const seen = [];
  const result = await streamSessionRun('session-1', 'hello', {
    onAsyncState: (turnId, state) => seen.push({turnId, ...state}),
  });
  assert.deepEqual(seen, [
    {turnId: 'turn-1', active: 2, unread: 1},
    {turnId: 'turn-1', active: 0, unread: 3},
  ]);
  assert.equal(result.message?.text, 'fine');
});

test('a caller that does not listen is unaffected by async_state', async () => {
  requests.length = 0;
  responses.length = 0;
  responses.push(streamResponse([
    {event: 'run', data: {turn_id: 'turn-1', started_at: '2026-01-01T00:00:00.000Z', status: 'running'}},
    {event: 'async_state', data: {turn_id: 'turn-1', active: 1, unread: 0, ids: ['a']}},
    {event: 'completed', data: {message: message('complete', 'fine'), committed: true}},
    {event: 'done', data: {}},
  ]));
  const result = await streamSessionRun('session-1', 'hello', {});
  assert.equal(result.message?.text, 'fine');
});

test('a plan-mode run sends plan: true, and any other run sends no plan key', async () => {
  requests.length = 0;
  responses.push(completedStream(), completedStream());
  await streamSessionRun('session-1', 'design a cache', {plan: true});
  await streamSessionRun('session-1', 'build it');
  assert.equal(JSON.parse(requests[0].body).plan, true);
  assert.equal('plan' in JSON.parse(requests[1].body), false);
});
