import assert from 'node:assert/strict';
import {afterEach, test} from 'node:test';

import {createSubagentPoller} from './subagent-transcript-poll.ts';
import {createSubagentTranscriptStore} from './subagent-transcript-store.ts';

const POLL = 100;
const MAX = 350;

const step = id => ({id, status: 'completed', parts: [{type: 'text', text: `step ${id}`}]});
const answer = (count, running) => ({steps: Array.from({length: count}, (_, index) => step(`1.${index + 1}`)), ...(running ? {running: true} : {})});
const flush = () => new Promise(resolve => setImmediate(resolve));

// A clock the poller's setTimeout/clearTimeout run on, so the tests can count what is pending.
const real = {setTimeout: globalThis.setTimeout, clearTimeout: globalThis.clearTimeout};
function installClock() {
  let now = 0;
  let nextId = 1;
  const timers = new Map();
  globalThis.setTimeout = (callback, ms = 0) => {
    const id = nextId++;
    timers.set(id, {at: now + ms, callback});
    return id;
  };
  globalThis.clearTimeout = id => {
    timers.delete(id);
  };
  return {
    pending: () => timers.size,
    delays: () => [...timers.values()].map(timer => timer.at - now),
    async tick(ms) {
      const end = now + ms;
      for (;;) {
        const due = [...timers.entries()].filter(([, timer]) => timer.at <= end).sort((a, b) => a[1].at - b[1].at)[0];
        if (due === undefined) break;
        timers.delete(due[0]);
        now = due[1].at;
        due[1].callback();
        await flush();
      }
      now = end;
    },
  };
}
afterEach(() => {
  globalThis.setTimeout = real.setTimeout;
  globalThis.clearTimeout = real.clearTimeout;
});

function visibility() {
  const listeners = new Set();
  const view = {
    visible: true,
    isVisible: () => view.visible,
    subscribe: listener => {
      listeners.add(listener);
      return () => listeners.delete(listener);
    },
    set(visible) {
      view.visible = visible;
      for (const listener of [...listeners]) listener();
    },
    listeners: () => listeners.size,
  };
  return view;
}

// A poller over a scripted server: each read takes the next reply (a transcript or an Error);
// the last reply repeats.
function harness({replies, input = {settled: false, summarySteps: 0}, store = createSubagentTranscriptStore(4), key = 'k', view = visibility()}) {
  const calls = {fetches: 0};
  const states = [];
  const fetch = async () => {
    const reply = replies[Math.min(calls.fetches, replies.length - 1)];
    calls.fetches += 1;
    if (reply instanceof Error) throw reply;
    return reply;
  };
  const poller = createSubagentPoller({store, key, fetch, pollMs: POLL, maxDelayMs: MAX, visibility: view, input, onState: state => states.push(state)});
  return {poller, calls, states, view, store, last: () => states[states.length - 1]};
}
const notFound = () => Object.assign(new Error('subagent transcript not found'), {status: 404});

test('opening fetches once at once, then every interval appends the newly completed steps until running is absent', async () => {
  const clock = installClock();
  // A handed-off child: the call settled with a summary of no step, the child goes on.
  const h = harness({replies: [answer(0, true), answer(1, true), answer(2, true), answer(3, false)], input: {settled: true, summarySteps: 0}});
  h.poller.start();
  assert.equal(h.calls.fetches, 1, 'one fetch on open, before any interval');
  await flush();
  assert.deepEqual(h.last().transcript.steps.length, 0);
  assert.equal(h.last().transcript.running, true);
  assert.deepEqual(clock.delays(), [POLL]);
  await clock.tick(POLL - 1);
  assert.equal(h.calls.fetches, 1, 'nothing before the interval');
  await clock.tick(1);
  assert.equal(h.calls.fetches, 2);
  assert.deepEqual(h.last().transcript.steps.map(s => s.id), ['1.1']);
  await clock.tick(POLL);
  assert.deepEqual(h.last().transcript.steps.map(s => s.id), ['1.1', '1.2']);
  await clock.tick(POLL);
  assert.deepEqual(h.last().transcript.steps.map(s => s.id), ['1.1', '1.2', '1.3']);
  assert.equal(h.last().transcript.running, undefined);
  assert.equal(clock.pending(), 0, 'an answer without running is final');
  await clock.tick(10 * POLL);
  assert.equal(h.calls.fetches, 4);
});

test('an unsettled call keeps polling after an answer without running, and settling ends it', async () => {
  const clock = installClock();
  const h = harness({replies: [answer(2, false)]});
  h.poller.start();
  await flush();
  assert.equal(clock.pending(), 1, 'the call has no result yet, so the child may not have started or just ended');
  await clock.tick(POLL);
  assert.equal(h.calls.fetches, 2);
  // The result arrives with a summary that matches what is held: nothing more to read.
  h.poller.update({settled: true, summarySteps: 2});
  assert.equal(clock.pending(), 0);
  await clock.tick(10 * POLL);
  assert.equal(h.calls.fetches, 2);
});

test('a body without running (an older server) on a settled call is one fetch and no timer', async () => {
  const clock = installClock();
  const h = harness({replies: [answer(3, false)], input: {settled: true, summarySteps: 3}});
  h.poller.start();
  await flush();
  assert.equal(h.calls.fetches, 1);
  assert.equal(h.last().phase, 'ready');
  assert.equal(clock.pending(), 0);
  await clock.tick(100 * POLL);
  assert.equal(h.calls.fetches, 1);
});

test('stopping leaves no timer, and an answer that lands after it neither shows nor schedules', async () => {
  const clock = installClock();
  const h = harness({replies: [answer(1, true)]});
  h.poller.start();
  await flush();
  assert.equal(clock.pending(), 1);
  h.poller.stop();
  assert.equal(clock.pending(), 0, 'a closed panel has no timer');
  assert.equal(h.view.listeners(), 0, 'and no visibility listener');
  await clock.tick(100 * POLL);
  assert.equal(h.calls.fetches, 1);

  // Closed while a read is in flight.
  const late = harness({replies: [answer(1, true)], key: 'other'});
  late.poller.start();
  late.poller.stop();
  const shown = late.states.length;
  await flush();
  assert.equal(late.states.length, shown);
  assert.equal(clock.pending(), 0);
});

test('a hidden tab pauses the polling and becoming visible fetches at once', async () => {
  const clock = installClock();
  const h = harness({replies: [answer(1, true), answer(2, true), answer(3, true)]});
  h.poller.start();
  await flush();
  assert.equal(clock.pending(), 1);
  h.view.set(false);
  assert.equal(clock.pending(), 0, 'hidden: no timer waits');
  await clock.tick(10 * POLL);
  assert.equal(h.calls.fetches, 1, 'hidden: no request');
  h.view.set(true);
  assert.equal(h.calls.fetches, 2, 'visible again: asks at once');
  await flush();
  assert.equal(h.last().transcript.steps.length, 2);
  assert.equal(clock.pending(), 1, 'and the cadence resumes');

  // Opened in a hidden tab: nothing is asked until it shows.
  const hidden = harness({replies: [answer(1, true)], key: 'hidden', view: Object.assign(visibility(), {visible: false})});
  hidden.poller.start();
  assert.equal(hidden.calls.fetches, 0);
  assert.equal(hidden.last().phase, 'loading');
  hidden.view.set(true);
  assert.equal(hidden.calls.fetches, 1);
});

test('a failed read doubles the delay up to the cap, keeps the last steps on screen, and the next answer recovers', async () => {
  const clock = installClock();
  const h = harness({replies: [answer(1, true), new Error('boom'), new Error('boom'), new Error('boom'), answer(2, true), answer(3, false)], input: {settled: true, summarySteps: 0}});
  h.poller.start();
  await flush();
  assert.deepEqual(clock.delays(), [POLL]);
  await clock.tick(POLL);
  assert.deepEqual(clock.delays(), [2 * POLL], 'first failure: doubled');
  assert.equal(h.last().phase, 'ready');
  assert.equal(h.last().retryError, 'boom');
  assert.equal(h.last().transcript.steps.length, 1, 'what was read stays');
  await clock.tick(2 * POLL);
  assert.deepEqual(clock.delays(), [MAX], 'second failure: 400 is capped at the maximum');
  await clock.tick(MAX);
  assert.deepEqual(clock.delays(), [MAX], 'and stays there');
  await clock.tick(MAX);
  assert.equal(h.last().transcript.steps.length, 2);
  assert.equal(h.last().retryError, undefined);
  assert.deepEqual(clock.delays(), [POLL], 'recovered: the normal cadence');
  await clock.tick(POLL);
  assert.equal(h.last().transcript.steps.length, 3);
  assert.equal(clock.pending(), 0);
});

test('an early 404 on an unsettled call is "starting" and keeps the normal cadence until the child answers', async () => {
  const clock = installClock();
  const h = harness({replies: [notFound(), notFound(), answer(0, true), answer(1, true)]});
  h.poller.start();
  await flush();
  assert.equal(h.last().phase, 'starting');
  assert.deepEqual(clock.delays(), [POLL]);
  await clock.tick(POLL);
  assert.equal(h.last().phase, 'starting');
  assert.deepEqual(clock.delays(), [POLL], 'a 404 is not a failure: no backoff');
  await clock.tick(POLL);
  assert.equal(h.last().phase, 'ready');
  assert.equal(h.last().transcript.running, true);
  assert.equal(h.last().transcript.steps.length, 0);
});

test('a read that fails on a settled call with no answer is a failure to retry by hand, not a timer', async () => {
  const clock = installClock();
  const h = harness({replies: [notFound(), answer(2, false)], input: {settled: true, summarySteps: 2}});
  h.poller.start();
  await flush();
  assert.equal(h.last().phase, 'failed');
  assert.match(h.last().message, /not found/);
  assert.equal(clock.pending(), 0);
  h.poller.retry();
  await flush();
  assert.equal(h.last().phase, 'ready');
  assert.equal(h.calls.fetches, 2);
});

test('an unsettled call keeps trying after a hard error without an answer', async () => {
  const clock = installClock();
  const h = harness({replies: [new Error('boom'), answer(1, true)]});
  h.poller.start();
  await flush();
  assert.equal(h.last().phase, 'failed');
  assert.deepEqual(clock.delays(), [2 * POLL]);
  await clock.tick(2 * POLL);
  assert.equal(h.last().phase, 'ready');
});

test('a stored answer: a finished child the summary does not outgrow is shown without a request, anything else asks at once', async () => {
  const clock = installClock();
  const store = createSubagentTranscriptStore(4);
  const finished = answer(3, false);
  await store.refresh('done', async () => finished);
  const same = harness({replies: [answer(5, false)], store, key: 'done', input: {settled: true, summarySteps: 3}});
  same.poller.start();
  assert.equal(same.last().transcript, finished);
  assert.equal(same.calls.fetches, 0);
  assert.equal(clock.pending(), 0);

  // The settled summary shows more steps than the entry holds: one more read, once.
  const behind = harness({replies: [answer(5, false)], store, key: 'done', input: {settled: true, summarySteps: 5}});
  behind.poller.start();
  await flush();
  assert.equal(behind.calls.fetches, 1);
  assert.equal(behind.last().transcript.steps.length, 5);
  assert.equal(clock.pending(), 0);

  // A stored running answer is not final.
  await store.refresh('going', async () => answer(1, true));
  const going = harness({replies: [answer(2, true)], store, key: 'going', input: {settled: true, summarySteps: 0}});
  going.poller.start();
  assert.equal(going.calls.fetches, 1);
  assert.equal(going.states[0].transcript.steps.length, 1, 'the stored steps show while it asks');
});

test('the call settling after the child ended reads once more when the summary counts more steps, and never loops on a summary it cannot reach', async () => {
  const clock = installClock();
  const h = harness({replies: [answer(1, true), answer(2, false)]});
  h.poller.start();
  await flush();
  await clock.tick(POLL);
  assert.equal(h.last().transcript.steps.length, 2);
  assert.equal(clock.pending(), 1, 'unsettled: still polling');
  const before = h.calls.fetches;
  // The summary counts a step the snapshot never reaches (a limit cut it).
  h.poller.update({settled: true, summarySteps: 9});
  await flush();
  assert.equal(h.calls.fetches, before + 1);
  assert.equal(clock.pending(), 0);
  await clock.tick(100 * POLL);
  assert.equal(h.calls.fetches, before + 1, 'asked once for that count');
  h.poller.update({settled: true, summarySteps: 9});
  await flush();
  assert.equal(h.calls.fetches, before + 1, 'an unchanged summary is not news');
});

test('pollers of one key share the request in flight, and each answer replaces the stored one', async () => {
  const store = createSubagentTranscriptStore(4);
  let release;
  let fetches = 0;
  const slow = () => {
    fetches += 1;
    return new Promise(resolve => {
      release = resolve;
    });
  };
  const first = createSubagentPoller({store, key: 'k', fetch: slow, pollMs: POLL, maxDelayMs: MAX, visibility: visibility(), input: {settled: true, summarySteps: 0}, onState() {}});
  const second = createSubagentPoller({store, key: 'k', fetch: slow, pollMs: POLL, maxDelayMs: MAX, visibility: visibility(), input: {settled: true, summarySteps: 0}, onState() {}});
  first.start();
  second.start();
  assert.equal(fetches, 1);
  release(answer(1, false));
  await flush();
  assert.equal(store.peek('k').steps.length, 1);
  await store.refresh('k', async () => answer(2, false));
  assert.equal(store.peek('k').steps.length, 2, 'the newer answer replaces the entry under the same key');
  first.stop();
  second.stop();
});
