import assert from 'node:assert/strict';
import {existsSync, readFileSync, readdirSync} from 'node:fs';
import {test} from 'node:test';

const {createCatalogValidator} = await import('./catalog-validation.ts');
const {clampProblems, locatePointer, syntaxProblem} = await import('./json-document.ts');

const schemesDir = new URL('../../sample/schemes/', import.meta.url);
const schemes = Object.fromEntries(
  readdirSync(schemesDir)
    .filter(name => name.endsWith('.json'))
    .map(name => [name, JSON.parse(readFileSync(new URL(name, schemesDir), 'utf8'))]),
);
const validator = createCatalogValidator(schemes);

// In the workspace layout the workspace's .jad holds profiles and sessions; models, tools and
// MCP servers are global (G, ~/.jad by default). `jad check` judges those files, not this test,
// and a unit test must not read $HOME.
const globalKind = dir => `no .jad/${dir} here: in the workspace layout this kind is global (G); \`jad check\` checks it, not this test`;

const goodProfile = {
  schema_version: 1,
  description: 'x',
  models: ['openrouter'],
  toolset: 'jad/default',
  tools: {allow: ['read']},
};
const pretty = value => JSON.stringify(value, null, 2);

function sliceOf(text, problem) {
  return text.slice(problem.from, problem.to);
}

test('a valid profile has no problems', () => {
  assert.deepEqual(validator.validate('profiles', pretty(goodProfile)), []);
});

test('a profile toolset may be a list of ids, never an empty one', () => {
  const list = {...goodProfile, toolset: ['jad/default', 'jad/extra']};
  assert.deepEqual(validator.validate('profiles', pretty(list)), []);
  const empty = {...goodProfile, toolset: []};
  assert.notDeepEqual(validator.validate('profiles', pretty(empty)), []);
});

test('every profile in the local layout passes the scheme', t => {
  const root = new URL('../../.jad/profiles/', import.meta.url);
  if (!existsSync(root)) {
    t.skip('.jad is git-ignored and absent in this checkout');
    return;
  }
  let checked = 0;
  for (const category of readdirSync(root, {withFileTypes: true}).filter(entry => entry.isDirectory())) {
    for (const file of readdirSync(new URL(`${category.name}/`, root)).filter(name => name.endsWith('.json'))) {
      const text = readFileSync(new URL(`${category.name}/${file}`, root), 'utf8');
      assert.deepEqual(validator.validate('profiles', text), [], `${category.name}/${file}`);
      checked += 1;
    }
  }
  assert.ok(checked >= 1);
});

test('broken JSON reports the engine message at the error position', () => {
  const text = '{\n  "schema_version": 1,\n  "models": [\n}';
  const problems = validator.validate('profiles', text);
  assert.equal(problems.length, 1);
  assert.ok(problems[0].message.length > 0);
  assert.ok(problems[0].from >= text.indexOf('}'));
  assert.ok(problems[0].to > problems[0].from);
});

test('an empty document is a syntax problem inside the document bounds', () => {
  const problem = syntaxProblem('');
  assert.ok(problem);
  assert.equal(problem.from, 0);
  assert.equal(problem.to, 0);
});

test('an unknown key underlines the key', () => {
  const text = pretty({...goodProfile, bogus: 1});
  const [problem] = validator.validate('profiles', text);
  assert.equal(sliceOf(text, problem), '"bogus"');
  assert.match(problem.message, /unknown key "bogus"/);
});

test('a wrong type underlines the key of the wrongly typed member', () => {
  const text = pretty({...goodProfile, models: 'openrouter'});
  const problems = validator.validate('profiles', text);
  assert.equal(problems.length, 1);
  assert.equal(sliceOf(text, problems[0]), '"models"');
  assert.match(problems[0].message, /^\/models: must be array/);
});

test('a missing required key is reported at the document', () => {
  const {schema_version: _dropped, ...rest} = goodProfile;
  const text = pretty(rest);
  const [problem] = validator.validate('profiles', text);
  assert.match(problem.message, /missing required key "schema_version"/);
  assert.equal(sliceOf(text, problem), '{');
});

test('an error inside an array element points at that element', () => {
  const text = pretty({...goodProfile, tools: {allow: ['read', 5]}});
  const [problem] = validator.validate('profiles', text);
  assert.equal(sliceOf(text, problem), '5');
  assert.match(problem.message, /^\/tools\/allow\/1: must be string/);
});

test('settings are checked against setting.schema.json through the cross-file reference', () => {
  const text = pretty({...goodProfile, settings: {not_a_setting: 1}});
  const problems = validator.validate('profiles', text);
  assert.ok(problems.length >= 1);
  assert.ok(problems.some(problem => sliceOf(text, problem).includes('not_a_setting')));
});

test('a wrong const reports the expected value', () => {
  const text = pretty({...goodProfile, schema_version: 2});
  const [problem] = validator.validate('profiles', text);
  assert.equal(sliceOf(text, problem), '"schema_version"');
  assert.match(problem.message, /must be 1/);
});

test('problems come back in document order and are not repeated', () => {
  const text = pretty({...goodProfile, zeta: 1, alpha: 2});
  const problems = validator.validate('profiles', text);
  const starts = problems.map(problem => problem.from);
  assert.deepEqual(starts, [...starts].sort((a, b) => a - b));
  assert.equal(new Set(problems.map(problem => `${problem.from}:${problem.message}`)).size, problems.length);
});

test('locatePointer finds nested keys, array items and escaped keys', () => {
  const text = '{"a/b": {"c~d": [10, {"e": true}]}}';
  assert.equal(text.slice(...Object.values(locatePointer(text, '/a~1b/c~0d/0').value)), '10');
  assert.equal(text.slice(...Object.values(locatePointer(text, '/a~1b/c~0d/1/e').value)), 'true');
  assert.equal(locatePointer(text, '/a~1b/c~0d/2'), null);
  assert.equal(locatePointer(text, '/missing'), null);
  assert.equal(locatePointer(text, '/a~1b/c~0d/x'), null);
  assert.equal(locatePointer(text, '').isContainer, true);
});

test('locatePointer returns the key range of an object member', () => {
  const text = '{"a": 1, "b": 2}';
  const target = locatePointer(text, '/b');
  assert.equal(text.slice(target.key.from, target.key.to), '"b"');
  assert.equal(text.slice(target.value.from, target.value.to), '2');
});

test('problems are clamped into a document that has since shrunk', () => {
  const clamped = clampProblems(
    [
      {from: 10, to: 20, message: 'a'},
      {from: -3, to: 2, message: 'b'},
      {from: 4, to: 1, message: 'c'},
    ],
    8,
  );
  assert.deepEqual(clamped, [
    {from: 8, to: 8, message: 'a'},
    {from: 0, to: 2, message: 'b'},
    {from: 4, to: 4, message: 'c'},
  ]);
});

test('a valid model has no problems, extra keys are allowed and a wrong type is underlined', () => {
  const model = {schema_version: 1, provider: 'openai', model: 'gpt-x', api_key_env: 'OPENAI_API_KEY', request: {reasoning: {effort: 'high'}}};
  assert.deepEqual(validator.validate('models', pretty(model)), []);
  assert.deepEqual(validator.validate('models', pretty({...model, vendor_note: 1})), []);
  const text = pretty({...model, model: 5});
  const [problem] = validator.validate('models', text);
  assert.equal(sliceOf(text, problem), '"model"');
  assert.match(problem.message, /string/);
});

test('the model template is refused until the model name is filled in', () => {
  const problems = validator.validate('models', '{\n  "schema_version": 1,\n  "provider": "openai",\n  "model": ""\n}\n');
  assert.equal(problems.length, 1);
  assert.match(problems[0].message, /model/);
  assert.deepEqual(validator.validate('models', '{"model": "x"}'), []);
});

test('every model in the local layout passes the scheme', t => {
  const root = new URL('../../.jad/models/', import.meta.url);
  if (!existsSync(root)) {
    t.skip(globalKind('models'));
    return;
  }
  let checked = 0;
  for (const category of readdirSync(root, {withFileTypes: true}).filter(entry => entry.isDirectory())) {
    for (const file of readdirSync(new URL(`${category.name}/`, root)).filter(name => name.endsWith('.json'))) {
      const text = readFileSync(new URL(`${category.name}/${file}`, root), 'utf8');
      assert.deepEqual(validator.validate('models', text), [], `${category.name}/${file}`);
      checked += 1;
    }
  }
  assert.ok(checked >= 1);
});

test('a valid session has no problems and an unknown key is underlined', () => {
  const session = {
    schema_version: 1,
    cwd: '/tmp/work',
    scope: {profiles: ['dev']},
  };
  assert.deepEqual(validator.validate('sessions', pretty(session)), []);
  const text = pretty({...session, bogus: 1});
  const [problem] = validator.validate('sessions', text);
  assert.equal(sliceOf(text, problem), '"bogus"');
});

// The session category was removed; a file that still sets it keeps loading, so the editor does not
// underline it (the schema marks it deprecated).
test('a session that still sets the removed category is not underlined', () => {
  const text = pretty({schema_version: 1, cwd: '/tmp/work', category: 'work'});
  assert.deepEqual(validator.validate('sessions', text), []);
});

test('a session that still names an active_profile is underlined', () => {
  const text = pretty({schema_version: 1, cwd: '/tmp/work', active_profile: 'dev/coder'});
  const [problem] = validator.validate('sessions', text);
  assert.equal(sliceOf(text, problem), '"active_profile"');
});

test('the session template is refused until cwd is filled in', () => {
  const template = '{\n  "schema_version": 1,\n  "cwd": ""\n}\n';
  const problems = validator.validate('sessions', template);
  assert.ok(problems.some(problem => /cwd/.test(problem.message)), JSON.stringify(problems));
  assert.ok(!problems.some(problem => /active_profile/.test(problem.message)));
  assert.deepEqual(validator.validate('sessions', template.replace('"cwd": ""', '"cwd": "/tmp/work"')), []);
});

test('every session in the local layout passes the scheme', t => {
  const root = new URL('../../.jad/sessions/', import.meta.url);
  if (!existsSync(root)) {
    t.skip('.jad is git-ignored and absent in this checkout');
    return;
  }
  let checked = 0;
  const sessions = readdirSync(root, {withFileTypes: true}).filter(entry => entry.isFile() && /^[A-Za-z0-9][^.]*\.json$/.test(entry.name));
  for (const session of sessions) {
    assert.deepEqual(validator.validate('sessions', readFileSync(new URL(session.name, root), 'utf8')), [], session.name);
    checked += 1;
  }
  assert.ok(checked >= 1);
});

function localJson(dir, t) {
  const root = new URL(`../../.jad/${dir}/`, import.meta.url);
  if (!existsSync(root)) {
    t.skip(globalKind(dir));
    return [];
  }
  const files = [];
  for (const category of readdirSync(root, {withFileTypes: true}).filter(entry => entry.isDirectory())) {
    for (const file of readdirSync(new URL(`${category.name}/`, root)).filter(name => name.endsWith('.json'))) {
      files.push([`${category.name}/${file}`, readFileSync(new URL(`${category.name}/${file}`, root), 'utf8')]);
    }
  }
  return files;
}

test('every toolset in the local layout passes the scheme', t => {
  const files = localJson('tools', t);
  for (const [name, text] of files) assert.deepEqual(validator.validate('toolsets', text), [], name);
});

test('every MCP server file in the local layout passes the scheme', t => {
  const files = localJson('mcp', t);
  for (const [name, text] of files) assert.deepEqual(validator.validate('mcp', text), [], name);
});

test('a toolset with an unknown key or an unknown top-level section is flagged at the key', () => {
  const text = pretty({schema_version: 1, bogus: 1});
  const [problem] = validator.validate('toolsets', text);
  assert.equal(sliceOf(text, problem), '"bogus"');
  assert.match(problem.message, /unknown key "bogus"/);
});

test('the toolset template is valid', () => {
  assert.deepEqual(validator.validate('toolsets', '{\n  "schema_version": 1\n}\n'), []);
});

test('the MCP template is refused until the stdio command is filled in', () => {
  const template = '{\n  "schema_version": 1,\n  "transport": {\n    "type": "stdio",\n    "command": ""\n  }\n}\n';
  const problems = validator.validate('mcp', template);
  assert.ok(problems.some(problem => /command/.test(problem.message)), JSON.stringify(problems));
  const filled = template.replace('"command": ""', '"command": "bunx"');
  assert.deepEqual(validator.validate('mcp', filled), []);
});

test('the MCP scheme flags an unknown top-level key, which the server accepts', () => {
  const text = pretty({schema_version: 1, transport: {type: 'stdio', command: 'x'}, bogus: 1});
  const [problem] = validator.validate('mcp', text);
  assert.equal(sliceOf(text, problem), '"bogus"');
});

test('the MCP scheme refuses a missing transport and a wrong transport type', () => {
  assert.ok(validator.validate('mcp', pretty({schema_version: 1})).some(p => /transport/.test(p.message)));
  assert.ok(validator.validate('mcp', pretty({schema_version: 1, transport: {type: 'pipe'}})).length > 0);
  assert.deepEqual(
    validator.validate('mcp', pretty({schema_version: 1, transport: {type: 'streamable_http', url: 'http://h/mcp'}})),
    [],
  );
});

test('an MCP transport error names only the branch the type selects', () => {
  const template = pretty({schema_version: 1, transport: {type: 'stdio', command: ''}});
  const messages = validator.validate('mcp', template).map(problem => problem.message);
  assert.equal(messages.length, 1, messages.join(' | '));
  assert.match(messages[0], /command/);
  const http = pretty({schema_version: 1, transport: {type: 'streamable_http'}});
  const httpMessages = validator.validate('mcp', http).map(problem => problem.message);
  assert.ok(httpMessages.some(message => /missing required key "url"/.test(message)), httpMessages.join(' | '));
  assert.ok(httpMessages.every(message => !/stdio|command/.test(message)), httpMessages.join(' | '));
});

test('an unknown MCP transport type still lists what was allowed', () => {
  const messages = validator.validate('mcp', pretty({schema_version: 1, transport: {type: 'pipe'}})).map(problem => problem.message);
  assert.ok(messages.includes('/transport/type: must be "stdio"'), messages.join(' | '));
  assert.ok(messages.includes('/transport/type: must be "streamable_http"'), messages.join(' | '));
});

test('without a usable transport type no branch is hidden, so the allowed shapes stay visible', () => {
  const none = validator.validate('mcp', pretty({schema_version: 1, transport: {}})).map(problem => problem.message);
  assert.ok(none.includes('/transport: missing required key "type"'), none.join(' | '));
  assert.ok(none.includes('/transport: missing required key "command"'), none.join(' | '));
  assert.ok(none.includes('/transport: missing required key "url"'), none.join(' | '));
  const text = validator.validate('mcp', pretty({schema_version: 1, transport: 'x'})).map(problem => problem.message);
  assert.ok(text.includes('/transport: must be object'), text.join(' | '));
  const five = validator.validate('mcp', pretty({schema_version: 1, transport: {type: 5}})).map(problem => problem.message);
  assert.ok(five.includes('/transport/type: must be "stdio"'), five.join(' | '));
});

test('documents without a discriminated oneOf keep every error', () => {
  const messages = validator
    .validate('profiles', pretty({...goodProfile, tools: {allow: 5}}))
    .map(problem => problem.message);
  assert.ok(messages.length >= 1, messages.join(' | '));
  assert.ok(messages.every(message => /tools/.test(message)), messages.join(' | '));
});
