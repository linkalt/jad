import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import {test} from 'node:test';

const {parseModelOptions} = await import('./api.ts');

test('maps a valid model_options array to camelCase options', () => {
  assert.deepEqual(
    parseModelOptions([
      {id: 'gpt-5', provider: 'openai', efforts: ['low', 'high'], default_effort: 'high'},
      {id: 'plain'},
    ]),
    [
      {id: 'gpt-5', provider: 'openai', efforts: ['low', 'high'], defaultEffort: 'high'},
      {id: 'plain'},
    ],
  );
});

test('a profile scope marks the running profile’s own model; any other scope is dropped', () => {
  assert.deepEqual(parseModelOptions([{id: 'openai/fast', scope: 'profile'}, {id: 'openai/default', scope: 'global'}, {id: 'x', scope: 7}]), [
    {id: 'openai/fast', scope: 'profile'},
    {id: 'openai/default'},
    {id: 'x'},
  ]);
});

test('an absent or malformed model_options is undefined and never throws', () => {
  for (const raw of [undefined, null, 'x', 42, {id: 'a'}, true]) {
    assert.equal(parseModelOptions(raw), undefined);
  }
});

test('entries without a usable id are dropped', () => {
  assert.deepEqual(
    parseModelOptions([null, 'a', {}, {id: ''}, {id: '  '}, {id: 7}, {id: 'ok'}]),
    [{id: 'ok'}],
  );
});

test('an empty array stays empty and non-string efforts are filtered', () => {
  assert.deepEqual(parseModelOptions([]), []);
  assert.deepEqual(parseModelOptions([{id: 'm', efforts: ['low', 3, null], default_effort: 5}]), [
    {id: 'm', efforts: ['low']},
  ]);
});

test('the snapshot mapping parses model_options into the session record', () => {
  // sessionFromSnapshot pulls in StyleX-compiled modules, so the wiring is
  // pinned at the source level: the parser runs on the snapshot field and the
  // result reaches the record as `modelOptions` (the drawer's input).
  const mapping = readFileSync(new URL('./session-mapping.ts', import.meta.url), 'utf8');
  assert.match(mapping, /const modelOptions = parseModelOptions\(snapshot\.model_options\);/);
  assert.match(mapping, /\.\.\.\(modelOptions === undefined \? \{\} : \{modelOptions\}\)/);
});
