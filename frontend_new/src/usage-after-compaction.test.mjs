import assert from 'node:assert/strict';
import {mkdirSync, readFileSync, rmSync, writeFileSync} from 'node:fs';
import {after, test} from 'node:test';

// The usage the server reports around a manual compaction. COMPACT_BODY and
// RUN_USAGE are the bytes the backend wrote in a captured session (three
// answered messages, a manual compaction, then a fourth message): the compact
// response carries the cumulative totals and a latest response with every token
// count zeroed, and the next message's usage event adds to those totals.
const COMPACT_BODY = '{"compaction":{"compacted":true,"compacted_items":6,"retained_items":2},"usage":{"input_tokens":15,"output_tokens":3,"total_tokens":18,"cached_input_tokens":6,"completed_responses":3,"responses_with_usage":3,"turn_count":3,"history_partial":false,"current":{"input_tokens":0,"output_tokens":0,"total_tokens":0,"cached_input_tokens":0,"turn":3,"step":1,"response_id":"r3"}}}';
const RUN_USAGE = '{"usage":{"input_tokens":20,"output_tokens":4,"total_tokens":24,"cached_input_tokens":8,"completed_responses":4,"responses_with_usage":4,"turn_count":4,"history_partial":false,"current":{"input_tokens":5,"output_tokens":1,"total_tokens":6,"cached_input_tokens":2,"turn":4,"step":1,"response_id":"r4"}}}';

const originalFetch = globalThis.fetch;
const responses = [];
globalThis.fetch = async () => {
  const response = responses.shift();
  assert.ok(response, 'unexpected API request');
  return response;
};

const {compactSession, streamSessionRun} = await import('./api.ts');

after(() => {
  globalThis.fetch = originalFetch;
});

function jsonResponse(body) {
  return new Response(body, {headers: {'Content-Type': 'application/json'}});
}

function runResponse(usageBody) {
  const message = {
    id: 'turn-4',
    sender: 'assistant',
    status: 'complete',
    text: 'done',
    timestamp: '2026-01-01T00:00:00.000Z',
    parts: [{type: 'text', text: 'done'}],
  };
  const frames = [
    ['run', {turn_id: 'turn-4', started_at: '2026-01-01T00:00:00.000Z', status: 'running'}],
    ['usage', JSON.parse(usageBody)],
    ['ui.message', {message}],
    ['completed', {message, committed: true}],
    ['done', {}],
  ];
  const text = frames.map(([event, data]) => `event: ${event}\ndata: ${JSON.stringify(data)}\n\n`).join('');
  return new Response(text, {headers: {'Content-Type': 'text/event-stream'}});
}

test('the compact response keeps the cumulative totals and the zeroed latest response', async () => {
  responses.push(jsonResponse(COMPACT_BODY));

  const result = await compactSession('session-1', 'dev/coder');

  assert.equal(result.compacted, true);
  assert.deepEqual(result.usage, JSON.parse(COMPACT_BODY).usage);
  assert.equal(result.usage.input_tokens, 15);
  assert.equal(result.usage.completed_responses, 3);
  assert.equal(result.usage.turn_count, 3);
  assert.deepEqual(result.usage.current, {
    input_tokens: 0,
    output_tokens: 0,
    total_tokens: 0,
    cached_input_tokens: 0,
    turn: 3,
    step: 1,
    response_id: 'r3',
  });
});

test('the next message after a compaction reports totals that add to the kept ones', async () => {
  responses.push(runResponse(RUN_USAGE));
  const seen = [];

  const result = await streamSessionRun('session-1', 'turn 4', {onUsage: usage => seen.push(usage)});

  assert.equal(seen.length, 1);
  assert.deepEqual(seen[0], JSON.parse(RUN_USAGE).usage);
  assert.deepEqual(result.usage, JSON.parse(RUN_USAGE).usage);
  const compacted = JSON.parse(COMPACT_BODY).usage;
  assert.equal(result.usage.input_tokens, compacted.input_tokens + result.usage.current.input_tokens);
  assert.equal(result.usage.completed_responses, compacted.completed_responses + 1);
});

test('a summary with the latest response zeroed is not rejected as a usage event', async () => {
  responses.push(runResponse(JSON.stringify({usage: JSON.parse(COMPACT_BODY).usage})));
  const seen = [];

  await streamSessionRun('session-1', 'turn 4', {onUsage: usage => seen.push(usage)});

  assert.deepEqual(seen, [JSON.parse(COMPACT_BODY).usage]);
});

// The drawer is a React component, so it is transpiled (with the same transformer
// the app's build uses) into a scratch folder under node_modules, where its imports
// resolve, and rendered to markup.
const scratch = new URL('../node_modules/.cache/usage-after-compaction-test/', import.meta.url);

async function renderDrawer(usage) {
  const {transformWithOxc} = await import('vite');
  const React = await import('react');
  const {renderToStaticMarkup} = await import('react-dom/server');
  const source = readFileSync(new URL('./components/UsageDetailsDrawer.tsx', import.meta.url), 'utf8');
  const output = await transformWithOxc(source, 'UsageDetailsDrawer.tsx', {jsx: {runtime: 'automatic'}});
  mkdirSync(scratch, {recursive: true});
  writeFileSync(new URL('drawer.mjs', scratch), output.code);
  const {UsageDetailsDrawer} = await import(new URL('drawer.mjs', scratch).href);
  const html = renderToStaticMarkup(React.createElement(UsageDetailsDrawer, {sessionName: 's', usage}));
  return html.replace(/<[^>]+>/g, ' ').replace(/\s+/g, ' ').trim();
}

after(() => {
  rmSync(scratch, {recursive: true, force: true});
});

test('the usage drawer shows the kept totals beside a zeroed latest response', async () => {
  const text = await renderDrawer(JSON.parse(COMPACT_BODY).usage);

  assert.match(text, /3 completed model responses across 3 user turns/);
  assert.match(text, /Latest response Turn 3 · Step 1 Input tokens 0 Output tokens 0 Total tokens 0 Cached input tokens 0/);
  assert.match(text, /Session totals Input tokens 15 Output tokens 3 Total tokens 18 Cached input tokens 6/);
  assert.doesNotMatch(text, /Usage details will appear after the first completed response/);
});

test('the usage drawer after the next message shows the grown totals', async () => {
  const text = await renderDrawer(JSON.parse(RUN_USAGE).usage);

  assert.match(text, /4 completed model responses across 4 user turns/);
  assert.match(text, /Latest response Turn 4 · Step 1 Input tokens 5 Output tokens 1/);
  assert.match(text, /Session totals Input tokens 20 Output tokens 4 Total tokens 24 Cached input tokens 8/);
});
