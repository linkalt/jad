import assert from 'node:assert/strict';
import {after, test} from 'node:test';

const originalFetch = globalThis.fetch;
const requests = [];
let reply = () => new Response('{}', {status: 200});
globalThis.fetch = async (input, init = {}) => {
  requests.push({url: String(input), init});
  return reply();
};

const {fetchSessionMCPPrompt, fetchSessionMCPPrompts} = await import('./api.ts');

after(() => {
  globalThis.fetch = originalFetch;
});

function json(value, status = 200) {
  return new Response(JSON.stringify(value), {status});
}

test('fetchSessionMCPPrompts reads the profile route and keeps the failures', async () => {
  requests.length = 0;
  reply = () =>
    json({
      prompts: [{server: 'gh', name: 'review', arguments: [{name: 'pr', required: true}]}],
      failures: [{server: 'down', error: 'failed to connect'}],
    });
  const listed = await fetchSessionMCPPrompts('sess 1', 'dev/coder');
  assert.match(requests[0].url, /\/sessions\/sess%201\/mcp-prompts\?profile=dev%2Fcoder$/);
  assert.equal(listed.prompts[0].arguments[0].name, 'pr');
  assert.deepEqual(listed.failures, [{server: 'down', error: 'failed to connect'}]);

  reply = () => json({prompts: []});
  assert.deepEqual(await fetchSessionMCPPrompts('s'), {prompts: [], failures: []});
});

test('fetchSessionMCPPrompts rejects a malformed list', async () => {
  for (const body of [{}, {prompts: [{server: 'gh'}]}, {prompts: [{server: 'gh', name: 'x', arguments: [{}]}]}, {prompts: [], failures: [{}]}]) {
    reply = () => json(body);
    await assert.rejects(fetchSessionMCPPrompts('s'), /invalid MCP prompt data/);
  }
});

test('fetchSessionMCPPrompt posts the arguments and returns the text', async () => {
  requests.length = 0;
  reply = () => json({prompt: {server: 'gh', name: 'review', text: 'Review PR 42'}});
  const text = await fetchSessionMCPPrompt('s', {server: 'gh', name: 'review', arguments: {pr: '42'}}, 'alt');
  assert.equal(text, 'Review PR 42');
  assert.match(requests[0].url, /\/sessions\/s\/mcp-prompts\/get\?profile=alt$/);
  assert.equal(requests[0].init.method, 'POST');
  assert.deepEqual(JSON.parse(requests[0].init.body), {server: 'gh', name: 'review', arguments: {pr: '42'}});
});

test('fetchSessionMCPPrompt surfaces the server refusal', async () => {
  reply = () => json({error: 'MCP server gh: prompt review: the server refused: no such PR'}, 502);
  await assert.rejects(fetchSessionMCPPrompt('s', {server: 'gh', name: 'review', arguments: {}}), /no such PR/);
});
