import assert from 'node:assert/strict';
import {after, test} from 'node:test';

const originalFetch = globalThis.fetch;
const requests = [];
let reply = () => new Response('{}', {status: 200});
globalThis.fetch = async (input, init = {}) => {
  requests.push({url: String(input), init});
  return reply();
};

const {writeSessionStateContext} = await import('./api.ts');

after(() => {
  globalThis.fetch = originalFetch;
});

function json(value, status = 200) {
  return new Response(JSON.stringify(value), {status});
}

test('writeSessionStateContext posts the prompt and preview flag to the profile route', async () => {
  requests.length = 0;
  reply = () => json({statecontext: {written: false, preview: true, role: 'developer', text: 'block', bytes: 5}});
  const result = await writeSessionStateContext('sess 1', {prompt: 'keep x', preview: true}, 'alt');
  assert.equal(requests.length, 1);
  assert.match(requests[0].url, /\/sessions\/sess%201\/statecontext\?profile=alt$/);
  assert.equal(requests[0].init.method, 'POST');
  assert.deepEqual(JSON.parse(requests[0].init.body), {prompt: 'keep x', preview: true});
  assert.deepEqual(result, {written: false, preview: true, role: 'developer', text: 'block', bytes: 5});
});

test('writeSessionStateContext reports an empty write without text', async () => {
  reply = () => json({statecontext: {written: false}});
  assert.deepEqual(await writeSessionStateContext('s', {prompt: '', preview: false}), {
    written: false,
    preview: false,
  });
});

test('writeSessionStateContext surfaces the server error text', async () => {
  reply = () => json({error: 'the user state context trigger is not configured for this session'}, 403);
  await assert.rejects(
    writeSessionStateContext('s', {prompt: 'x', preview: false}),
    /not configured for this session/,
  );
});

test('writeSessionStateContext surfaces the conflict while a run is active', async () => {
  reply = () => json({error: 'cannot write state context while a run is active'}, 409);
  await assert.rejects(
    writeSessionStateContext('s', {prompt: 'x', preview: false}),
    /while a run is active/,
  );
});

test('writeSessionStateContext rejects a malformed response', async () => {
  for (const body of [{}, {statecontext: {}}, {statecontext: {written: 'yes'}}, {statecontext: {written: true, bytes: -1}}]) {
    reply = () => json(body);
    await assert.rejects(
      writeSessionStateContext('s', {prompt: '', preview: true}),
      /invalid state context data/,
      JSON.stringify(body),
    );
  }
});
