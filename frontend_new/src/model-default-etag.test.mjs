import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import {after, test} from 'node:test';

const originalFetch = globalThis.fetch;
const requests = [];
let reply = () => new Response('{}');
globalThis.fetch = async (url, init = {}) => {
  requests.push({url: String(url), init});
  return reply();
};
const {updateSessionProfileModelDefault, SettingsRequestError} = await import('./api.ts');
const {modelDefaultIfMatch} = await import('./model-selection.ts');

after(() => {
  globalThis.fetch = originalFetch;
});

const okBody = {active_profile: 'dev/coder', profiles: ['dev/coder'], model_default: 'm', models: ['m'], model: {provider: 'p', model: 'm'}, changed: true, profile_etag: '"2"'};

test('the model-default write sends If-Match only when given one', async () => {
  requests.length = 0;
  reply = () => new Response(JSON.stringify(okBody));
  await updateSessionProfileModelDefault('local:demo', 'm', 'dev/coder', '"1"');
  await updateSessionProfileModelDefault('local:demo', 'm', 'dev/coder');
  assert.equal(new Headers(requests[0].init.headers).get('if-match'), '"1"');
  assert.equal(new Headers(requests[1].init.headers).get('if-match'), null);
});

test('a refused write carries its status, and 409 and 412 read as conflicts', async () => {
  for (const [status, conflict] of [[412, true], [409, true], [400, false]]) {
    reply = () => new Response(JSON.stringify({error: 'changed since it was read'}), {status});
    const error = await updateSessionProfileModelDefault('local:demo', 'm').catch(caught => caught);
    assert.ok(error instanceof SettingsRequestError);
    assert.equal(error.status, status);
    assert.equal(error.isConflict, conflict);
  }
});

test("the snapshot keeps profile_etag, and If-Match is sent only for the record's own profile", () => {
  const mapping = readFileSync(new URL('./session-mapping.ts', import.meta.url), 'utf8');
  assert.match(mapping, /profileEtag: snapshot\.profile_etag/);
  const session = {activeProfile: 'dev/coder', profileEtag: '"7"'};
  assert.equal(modelDefaultIfMatch(session, undefined), '"7"');
  assert.equal(modelDefaultIfMatch(session, 'dev/coder'), '"7"');
  assert.equal(modelDefaultIfMatch(session, 'review/reviewer'), undefined);
  assert.equal(modelDefaultIfMatch({activeProfile: 'dev/coder'}, undefined), undefined);
  assert.equal(modelDefaultIfMatch(undefined, undefined), undefined);
});
