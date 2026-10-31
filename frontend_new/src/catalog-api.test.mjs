import assert from 'node:assert/strict';
import {after, test} from 'node:test';

const originalFetch = globalThis.fetch;
const requests = [];
let reply = () => new Response('{}', {status: 200});
globalThis.fetch = async (input, init = {}) => {
  requests.push({url: String(input), init});
  return reply();
};

const {fetchCatalogFiles, fetchCatalogFile, saveCatalogFile, deleteCatalogFile, CatalogRequestError} = await import('./api.ts');

after(() => {
  globalThis.fetch = originalFetch;
});

const sentHeaders = request => Object.fromEntries(new Headers(request.init.headers).entries());
const json = (value, status = 200, headers = {}) => new Response(JSON.stringify(value), {status, headers});

test('fetchCatalogFiles reads the list and rejects a malformed one', async () => {
  requests.length = 0;
  reply = () => json({files: [{id: 'jad/default', etag: '"a"'}]});
  assert.deepEqual(await fetchCatalogFiles('profiles'), [{id: 'jad/default', etag: '"a"'}]);
  assert.match(requests[0].url, /\/catalog\/profiles$/);
  reply = () => json({files: [{id: 'x', etag: 7}]});
  await assert.rejects(fetchCatalogFiles('profiles'), /invalid catalog list/);
});

test('fetchCatalogFiles accepts a session row that carries an error and no etag', async () => {
  reply = () => json({files: [{id: 'a', etag: '"1"'}, {id: 'broken', error: 'could not be read'}]});
  assert.deepEqual(await fetchCatalogFiles('sessions'), [{id: 'a', etag: '"1"'}, {id: 'broken'}]);
});

test('fetchCatalogFiles keeps a session name, trims it, and drops a blank or non-string one', async () => {
  reply = () => json({files: [{id: 'a', etag: '"1"', name: '  Main work '}, {id: 'b', etag: '"2"', name: '   '}, {id: 'c', etag: '"3"'}]});
  assert.deepEqual(await fetchCatalogFiles('sessions'), [
    {id: 'a', etag: '"1"', name: 'Main work'},
    {id: 'b', etag: '"2"'},
    {id: 'c', etag: '"3"'},
  ]);
  reply = () => json({files: [{id: 'a', etag: '"1"', name: 7}]});
  await assert.rejects(fetchCatalogFiles('sessions'), /invalid catalog list/);
});

test('fetchCatalogFiles keeps a trimmed session category and drops a blank or non-string one', async () => {
  reply = () => json({files: [{id: 'a', etag: '"1"', category: ' work '}, {id: 'b', etag: '"2"', category: '  '}]});
  assert.deepEqual(await fetchCatalogFiles('sessions'), [
    {id: 'a', etag: '"1"', category: 'work'},
    {id: 'b', etag: '"2"'},
  ]);
  reply = () => json({files: [{id: 'a', etag: '"1"', category: 7}]});
  await assert.rejects(fetchCatalogFiles('sessions'), /invalid catalog list/);
});

test('fetchCatalogFiles keeps the sessions of a profile, trimmed, and rejects a malformed list of them', async () => {
  reply = () => json({files: [{id: 'a/b', etag: '"1"', sessions: [{id: 's', name: ' S ', category: ' c '}, {id: 't', name: '  '}]}, {id: 'c/d', etag: '"2"', sessions: []}]});
  assert.deepEqual(await fetchCatalogFiles('profiles'), [
    {id: 'a/b', etag: '"1"', sessions: [{id: 's', name: 'S', category: 'c'}, {id: 't'}]},
    {id: 'c/d', etag: '"2"'},
  ]);
  reply = () => json({files: [{id: 'a/b', etag: '"1"', sessions: [{name: 'x'}]}]});
  await assert.rejects(fetchCatalogFiles('profiles'), /invalid catalog list/);
  reply = () => json({files: [{id: 'a/b', etag: '"1"', sessions: 'x'}]});
  await assert.rejects(fetchCatalogFiles('profiles'), /invalid catalog list/);
});

test('fetchCatalogFile returns the raw text with the version header and encodes the id', async () => {
  requests.length = 0;
  reply = () => new Response('{\n  "a": 1\n}\n', {status: 200, headers: {etag: '"v1"'}});
  const file = await fetchCatalogFile('profiles', 'my cat/p.1');
  assert.deepEqual(file, {id: 'my cat/p.1', etag: '"v1"', text: '{\n  "a": 1\n}\n'});
  assert.match(requests[0].url, /\/catalog\/profiles\/my%20cat\/p\.1$/);
});

test('fetchCatalogFile refuses a response with no version and an id that is not category/name', async () => {
  reply = () => new Response('{}', {status: 200});
  await assert.rejects(fetchCatalogFile('profiles', 'a/b'), /without a version/);
  await assert.rejects(fetchCatalogFile('profiles', 'only'), /<category>\/<name>/);
  await assert.rejects(fetchCatalogFile('profiles', 'a/b/c'), /<category>\/<name>/);
});

test('fetchCatalogFiles keeps a profile name too', async () => {
  reply = () => json({files: [{id: 'dev/coder', etag: '"a"', name: ' Coder '}, {id: 'dev/x', etag: '"b"'}]});
  assert.deepEqual(await fetchCatalogFiles('profiles'), [
    {id: 'dev/coder', etag: '"a"', name: 'Coder'},
    {id: 'dev/x', etag: '"b"'},
  ]);
});

test('saveCatalogFile replaces with If-Match and creates with If-None-Match', async () => {
  requests.length = 0;
  reply = () => json({id: 'a/b', etag: '"v2"'}, 200);
  const replaced = await saveCatalogFile('profiles', 'a/b', '{}', '"v1"');
  assert.deepEqual(replaced, {info: {id: 'a/b', etag: '"v2"'}, created: false});
  assert.equal(requests[0].init.method, 'PUT');
  assert.deepEqual(sentHeaders(requests[0]), {'if-match': '"v1"'});
  assert.equal(requests[0].init.body, '{}');
  reply = () => json({id: 'a/b', etag: '"v1"'}, 201);
  const created = await saveCatalogFile('profiles', 'a/b', '{}', null);
  assert.equal(created.created, true);
  assert.deepEqual(sentHeaders(requests[1]), {'if-none-match': '*'});
});

test('a refused write keeps its status and the server message', async () => {
  reply = () => json({error: 'profile "a/b" changed since it was read; read it again'}, 412);
  await assert.rejects(saveCatalogFile('profiles', 'a/b', '{}', '"old"'), error => {
    assert.ok(error instanceof CatalogRequestError);
    assert.equal(error.status, 412);
    assert.match(error.message, /changed since it was read/);
    return true;
  });
  reply = () => json({error: 'toolset: not available'}, 422);
  await assert.rejects(saveCatalogFile('profiles', 'a/b', '{}', null), error => error.status === 422);
});

test('deleteCatalogFile sends If-Match and reports a profile that is in use', async () => {
  requests.length = 0;
  reply = () => new Response(null, {status: 204});
  await deleteCatalogFile('profiles', 'a/b', '"v1"');
  assert.equal(requests[0].init.method, 'DELETE');
  assert.deepEqual(sentHeaders(requests[0]), {'if-match': '"v1"'});
  reply = () => json({error: 'profile "a/b" is needed by session test'}, 409);
  await assert.rejects(deleteCatalogFile('profiles', 'a/b', '"v1"'), error => error.status === 409 && /needed by session/.test(error.message));
});

test('the models catalog uses its own path and the same preconditions', async () => {
  requests.length = 0;
  reply = () => json({files: []});
  await fetchCatalogFiles('models');
  assert.match(requests[0].url, /\/catalog\/models$/);
  reply = () => new Response(null, {status: 204});
  await deleteCatalogFile('models', 'a/b', '"v1"');
  assert.match(requests[1].url, /\/catalog\/models\/a\/b$/);
  assert.equal(requests[1].init.method, 'DELETE');
  assert.equal(sentHeaders(requests[1])['if-match'], '"v1"');
});

test('sessions are addressed by one path segment and delete can ask for the state to go', async () => {
  requests.length = 0;
  reply = () => json({files: []});
  await fetchCatalogFiles('sessions');
  assert.match(requests[0].url, /\/catalog\/sessions$/);
  reply = () => new Response('{}', {status: 200, headers: {etag: '"v1"'}});
  await fetchCatalogFile('sessions', 'local:demo');
  assert.match(requests[1].url, /\/catalog\/sessions\/local%3Ademo$/);
  await assert.rejects(fetchCatalogFile('sessions', 'a/b'), /<name> or <root>:<name>/);
  await assert.rejects(fetchCatalogFile('sessions', ''), /<name> or <root>:<name>/);

  reply = () => new Response(null, {status: 204});
  await deleteCatalogFile('sessions', 'demo', '"v1"');
  await deleteCatalogFile('sessions', 'demo', '"v1"', {deleteState: false});
  await deleteCatalogFile('sessions', 'demo', '"v1"', {deleteState: true});
  assert.equal(requests.length, 5);
  assert.match(requests[2].url, /\/catalog\/sessions\/demo$/);
  assert.match(requests[3].url, /\/catalog\/sessions\/demo$/);
  assert.match(requests[4].url, /\/catalog\/sessions\/demo\?state=delete$/);
  await deleteCatalogFile('profiles', 'a/b', '"v1"', {deleteState: true});
  assert.doesNotMatch(requests[5].url, /state=/);
});

test('toolsets and MCP servers are addressed as category/name under their own paths', async () => {
  requests.length = 0;
  reply = () => json({files: []});
  await fetchCatalogFiles('toolsets');
  await fetchCatalogFiles('mcp');
  assert.match(requests[0].url, /\/catalog\/toolsets$/);
  assert.match(requests[1].url, /\/catalog\/mcp$/);
  reply = () => new Response(null, {status: 204});
  await deleteCatalogFile('mcp', 'jad/zvec-grep', '"v1"');
  assert.match(requests[2].url, /\/catalog\/mcp\/jad\/zvec-grep$/);
  assert.equal(requests[2].init.method, 'DELETE');
  await assert.rejects(fetchCatalogFile('toolsets', 'one'), /<category>\/<name>/);
});
