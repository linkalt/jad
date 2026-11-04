import assert from 'node:assert/strict';
import {after, test} from 'node:test';

const originalFetch = globalThis.fetch;
const requests = [];
let reply = () => new Response('{}', {status: 200});
globalThis.fetch = async (input, init = {}) => {
  requests.push({url: String(input), init});
  return reply();
};

const {fetchSessionMCPResource, fetchSessionMCPResources} = await import('./api.ts');

after(() => {
  globalThis.fetch = originalFetch;
});

const json = (value, status = 200) => new Response(JSON.stringify(value), {status});

test('the resource list reads resources and templates, and keeps the failures', async () => {
  requests.length = 0;
  reply = () =>
    json({
      resources: [
        {server: 'docs', uri: 'docs://guide', name: 'guide', title: 'Guide', mime_type: 'text/markdown'},
        {server: 'docs', uri_template: 'docs://pages/{name}', variables: ['name'], name: 'page'},
      ],
      failures: [{server: 'down', error: 'failed to connect'}],
    });
  const listed = await fetchSessionMCPResources('sess 1', 'dev/coder');
  assert.match(requests[0].url, /\/sessions\/sess%201\/mcp-resources\?profile=dev%2Fcoder$/);
  assert.deepEqual(listed.resources, [
    {server: 'docs', uri: 'docs://guide', name: 'guide', title: 'Guide', mimeType: 'text/markdown', variables: []},
    {server: 'docs', uriTemplate: 'docs://pages/{name}', variables: ['name'], name: 'page'},
  ]);
  assert.deepEqual(listed.failures, [{server: 'down', error: 'failed to connect'}]);
});

test('a malformed resource list is refused', async () => {
  for (const value of [
    {resources: 'x'},
    {resources: [{server: 'docs', name: 'n'}]},
    {resources: [{server: 'docs', name: 'n', uri: 'a', uri_template: 'b'}]},
    {resources: [{server: 'docs', name: 'n', uri_template: 'b', variables: [1]}]},
    {resources: [], failures: [{server: 'x'}]},
  ]) {
    reply = () => json(value);
    await assert.rejects(fetchSessionMCPResources('s'), /invalid MCP resource data/);
  }
});

test('reading posts the URI, or the template with its variables', async () => {
  requests.length = 0;
  reply = () => json({resource: {server: 'docs', uri: 'docs://guide', text: 'Use tabs.'}});
  const guide = {server: 'docs', uri: 'docs://guide', name: 'guide', variables: []};
  assert.equal(await fetchSessionMCPResource('s', guide, {}, 'dev/coder'), 'Use tabs.');
  assert.match(requests[0].url, /\/sessions\/s\/mcp-resources\/read\?profile=dev%2Fcoder$/);
  assert.deepEqual(JSON.parse(requests[0].init.body), {server: 'docs', uri: 'docs://guide'});
  const page = {server: 'docs', uriTemplate: 'docs://pages/{name}', name: 'page', variables: ['name']};
  await fetchSessionMCPResource('s', page, {name: 'intro'});
  assert.deepEqual(JSON.parse(requests[1].init.body), {server: 'docs', uri_template: 'docs://pages/{name}', variables: {name: 'intro'}});
  reply = () => json({error: 'MCP server docs: resource docs://logo has no text to insert'}, 502);
  await assert.rejects(fetchSessionMCPResource('s', guide, {}), /no text to insert/);
});
