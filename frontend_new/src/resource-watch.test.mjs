import assert from 'node:assert/strict';
import {after, test} from 'node:test';

import {isResourceWatched, mergeResourceEvents, parseResourceEvents} from './resource-watch.ts';

const originalFetch = globalThis.fetch;
const requests = [];
let reply = () => new Response('{}', {status: 200});
globalThis.fetch = async (input, init = {}) => {
  requests.push({url: String(input), init});
  return reply();
};
const {fetchSessionMCPResourceEvents, setSessionMCPResourceWatch} = await import('./api.ts');
after(() => {
  globalThis.fetch = originalFetch;
});
const json = (value, status = 200) => new Response(JSON.stringify(value), {status});

const event = (seq, uri = 'docs://a') => ({seq, server: 'docs', kind: 'updated', uri, at: '2026-11-04T10:00:00Z'});

test('parseResourceEvents reads the events route and refuses another shape', () => {
  const parsed = parseResourceEvents({
    events: [event(1), {seq: 2, server: 'docs', kind: 'list_changed', at: 'x'}],
    latest: 2,
    subscriptions: [{server: 'docs', uri: 'docs://a'}],
  });
  assert.deepEqual(parsed?.events.map(item => item.seq), [1, 2]);
  assert.equal(parsed?.events[1].uri, undefined);
  assert.equal(parsed?.latest, 2);
  assert.equal(parseResourceEvents({events: [], latest: '2', subscriptions: []}), null);
  assert.equal(parseResourceEvents({events: [{seq: 1}], latest: 1, subscriptions: []}), null);
  assert.equal(parseResourceEvents({events: [], latest: 0}), null);
});

test('mergeResourceEvents puts new events on top, once each, and caps the list', () => {
  const held = [event(2), event(1)];
  const merged = mergeResourceEvents(held, {events: [event(2), event(3), event(4)], latest: 4, subscriptions: []}, 3);
  assert.deepEqual(merged.map(item => item.seq), [4, 3, 2]);
  // A server whose log started again (latest below what is held) replaces the list.
  const restarted = mergeResourceEvents(held, {events: [event(1, 'docs://b')], latest: 1, subscriptions: []}, 10);
  assert.deepEqual(restarted.map(item => item.uri), ['docs://b']);
});

test('isResourceWatched matches the server and the URI', () => {
  const subscriptions = [{server: 'docs', uri: 'docs://a'}];
  assert.equal(isResourceWatched(subscriptions, 'docs', 'docs://a'), true);
  assert.equal(isResourceWatched(subscriptions, 'other', 'docs://a'), false);
  assert.equal(isResourceWatched(subscriptions, 'docs', 'docs://b'), false);
});

test('the watch calls post to subscribe and unsubscribe and read the events after a seq', async () => {
  requests.length = 0;
  reply = () => json({server: 'docs', uri: 'docs://a', subscribed: true});
  await setSessionMCPResourceWatch('sess 1', 'docs', 'docs://a', true, 'dev/coder');
  await setSessionMCPResourceWatch('sess 1', 'docs', 'docs://a', false);
  assert.match(requests[0].url, /\/sessions\/sess%201\/mcp-resources\/subscribe\?profile=dev%2Fcoder$/);
  assert.equal(requests[0].init.method, 'POST');
  assert.deepEqual(JSON.parse(requests[0].init.body), {server: 'docs', uri: 'docs://a'});
  assert.match(requests[1].url, /\/mcp-resources\/unsubscribe$/);

  reply = () => json({events: [event(5)], latest: 5, subscriptions: [{server: 'docs', uri: 'docs://a'}]});
  const events = await fetchSessionMCPResourceEvents('sess 1', 4, 'dev/coder');
  assert.match(requests[2].url, /\/mcp-resources\/events\?after=4&profile=dev%2Fcoder$/);
  assert.equal(events.latest, 5);

  reply = () => json({error: 'the server does not offer resource subscriptions'}, 400);
  await assert.rejects(setSessionMCPResourceWatch('sess 1', 'web', 'web://x', true), /does not offer/);
  reply = () => json({events: 'no'});
  await assert.rejects(fetchSessionMCPResourceEvents('sess 1', 0), /invalid MCP resource event data/);
});
