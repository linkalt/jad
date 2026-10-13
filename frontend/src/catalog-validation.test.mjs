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
