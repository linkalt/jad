import assert from 'node:assert/strict';
import test from 'node:test';

import {filterModels, selectedModelForSession} from './model-selection.ts';

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
