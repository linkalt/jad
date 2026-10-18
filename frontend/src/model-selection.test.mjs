import assert from 'node:assert/strict';
import test from 'node:test';

import {
  defaultEffortFor,
  modelChoiceRows,
  filterModelOptions,
  filterModels,
  formatRunModel,
  modelOptionsForSession,
  resolveRunModelChoice,
  resolveRunModelSelection,
  selectedModelForSession,
} from './model-selection.ts';

test('prefers the active profile default when it is available', () => {
  assert.equal(
    selectedModelForSession(['model-a', 'model-b'], 'model-b', 'fallback-model'),
    'model-b',
  );
});

test('falls back to the first configured model when the default is unavailable', () => {
  assert.equal(
    selectedModelForSession(['model-a', 'model-b'], 'missing-model', 'fallback-model'),
    'model-a',
  );
});

test('uses the configured fallback when no profile models are listed', () => {
  assert.equal(selectedModelForSession(undefined, undefined, 'fallback-model'), 'fallback-model');
});

const modelNames = ['claude-sonnet-4-5', 'claude-opus-4-1', 'gpt-5-mini'];

test('an empty or whitespace-only query keeps every model, in order', () => {
  assert.deepEqual(filterModels(modelNames, ''), modelNames);
  assert.deepEqual(filterModels(modelNames, '   '), modelNames);
});

test('model search ignores case', () => {
  assert.deepEqual(filterModels(modelNames, 'CLAUDE'), ['claude-sonnet-4-5', 'claude-opus-4-1']);
  assert.deepEqual(filterModels(modelNames, 'Mini'), ['gpt-5-mini']);
});

test('every term has to match, in any order', () => {
  assert.deepEqual(filterModels(modelNames, 'claude 4-5'), ['claude-sonnet-4-5']);
  assert.deepEqual(filterModels(modelNames, '4-5 claude'), ['claude-sonnet-4-5']);
  assert.deepEqual(filterModels(modelNames, 'claude mini'), []);
});

test('a query that matches nothing returns nothing', () => {
  assert.deepEqual(filterModels(modelNames, 'zzz'), []);
});

test('filtering never touches the input list', () => {
  const before = [...modelNames];
  filterModels(modelNames, 'claude');
  assert.deepEqual(modelNames, before);
});

const optionList = [
  {id: 'opus', provider: 'anthropic', efforts: ['low', 'high'], defaultEffort: 'high'},
  {id: 'gpt-5', provider: 'openai', efforts: ['low', 'medium'], defaultEffort: 'bogus'},
  {id: 'plain', provider: 'local'},
];

test('modelOptionsForSession prefers modelOptions, else maps models with the provider', () => {
  assert.deepEqual(modelOptionsForSession({id: 's', modelOptions: optionList, models: ['x']}), optionList);
  assert.deepEqual(modelOptionsForSession({id: 's', modelOptions: [], models: ['a', 'b'], model: {provider: 'p'}}), [
    {id: 'a', provider: 'p'},
    {id: 'b', provider: 'p'},
  ]);
  assert.deepEqual(modelOptionsForSession({id: 's'}), []);
});

test('filterModelOptions matches id and provider ignoring case; blank keeps all', () => {
  assert.deepEqual(filterModelOptions(optionList, '  '), optionList);
  assert.deepEqual(filterModelOptions(optionList, 'OPEN').map(o => o.id), ['gpt-5']);
  assert.deepEqual(filterModelOptions(optionList, 'opus').map(o => o.id), ['opus']);
  assert.deepEqual(filterModelOptions(optionList, 'zzz'), []);
});

test('defaultEffortFor uses a valid default, else the first effort, else undefined', () => {
  assert.equal(defaultEffortFor(optionList[0]), 'high');
  assert.equal(defaultEffortFor(optionList[1]), 'low');
  assert.equal(defaultEffortFor(optionList[2]), undefined);
});

test('formatRunModel appends the effort, strips an existing suffix, omits an empty effort', () => {
  assert.equal(formatRunModel('opus', 'high'), 'opus@high');
  assert.equal(formatRunModel('opus@low', 'high'), 'opus@high');
  assert.equal(formatRunModel('opus'), 'opus');
  assert.equal(formatRunModel('opus', ''), 'opus');
  assert.equal(formatRunModel('opus@low'), 'opus');
});

test('resolveRunModelSelection uses the stored pick of the same session', () => {
  const record = {id: 's1', modelOptions: optionList, modelDefault: 'gpt-5'};
  assert.equal(resolveRunModelSelection(record, {sessionKey: 's1', modelId: 'opus', effort: 'low'}), 'opus@low');
  assert.equal(resolveRunModelSelection(record, {sessionKey: 's1', modelId: 'opus', effort: 'nope'}), 'opus@high');
});

test('resolveRunModelSelection ignores another session or unknown model and falls back to the default', () => {
  const record = {id: 's1', modelOptions: optionList, modelDefault: 'gpt-5'};
  assert.equal(resolveRunModelSelection(record, {sessionKey: 's2', modelId: 'opus', effort: 'low'}), 'gpt-5@low');
  assert.equal(resolveRunModelSelection(record, {sessionKey: 's1', modelId: 'gone'}), 'gpt-5@low');
  assert.equal(resolveRunModelSelection(record, null), 'gpt-5@low');
  assert.equal(resolveRunModelSelection({id: 's1', modelOptions: optionList}, null), 'opus@high');
  assert.equal(resolveRunModelSelection({id: 's1', modelOptions: optionList, modelDefault: 'plain'}, null), 'plain');
});

test('resolveRunModelSelection is undefined without options', () => {
  assert.equal(resolveRunModelSelection({id: 's1'}, null), undefined);
  assert.deepEqual(resolveRunModelChoice({id: 's1'}, null), {});
});

test('modelChoiceRows collapsed keeps the running model alone and marks its effort', () => {
  assert.deepEqual(modelChoiceRows(optionList, 'gpt-5', 'medium', '', false), [
    {option: optionList[1], isSelected: true, markedEffort: 'medium'},
  ]);
  // A stored effort that the option does not offer falls back to the option default.
  assert.deepEqual(modelChoiceRows(optionList, 'gpt-5', 'nope', '', false), [
    {option: optionList[1], isSelected: true, markedEffort: 'low'},
  ]);
});

test('modelChoiceRows collapsed shows the first option when nothing is selected', () => {
  assert.deepEqual(modelChoiceRows(optionList, undefined, undefined, '', false), [
    {option: optionList[0], isSelected: false, markedEffort: 'high'},
  ]);
  assert.deepEqual(modelChoiceRows([], undefined, undefined, '', false), []);
});

test('modelChoiceRows expanded lists every model, filtered by the query', () => {
  assert.deepEqual(modelChoiceRows(optionList, 'opus', 'high', '', true), [
    {option: optionList[0], isSelected: true, markedEffort: 'high'},
    {option: optionList[1], isSelected: false, markedEffort: 'low'},
    {option: optionList[2], isSelected: false},
  ]);
  assert.deepEqual(modelChoiceRows(optionList, 'opus', 'high', 'OPENAI', true).map(row => row.option.id), ['gpt-5']);
  assert.deepEqual(modelChoiceRows(optionList, 'opus', 'high', 'zzz', true), []);
});

test('modelChoiceRows marks the default effort of a row that is not selected', () => {
  assert.equal(modelChoiceRows(optionList, 'opus', undefined, 'gpt', true)[0].markedEffort, 'low');
});
