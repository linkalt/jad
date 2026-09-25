import assert from 'node:assert/strict';
import test from 'node:test';

import {
  loadModelSelectionBySession,
  saveModelSelectionBySession,
  selectedModelForSession,
} from './model-selection.ts';

class MemoryStorage {
  value = null;

  getItem() {
    return this.value;
  }

  setItem(_key, value) {
    this.value = value;
  }
}

test('persists selected model IDs per session across reloads', () => {
  const storage = new MemoryStorage();
  const selections = {sessionA: 'model-b', 'root:sessionB': 'model-c'};

  saveModelSelectionBySession(selections, storage);

  assert.deepEqual(loadModelSelectionBySession(storage), selections);
});

test('prefers an available saved model and falls back to the profile default', () => {
  assert.equal(
    selectedModelForSession(
      'sessionA',
      ['model-a', 'model-b'],
      'model-a',
      'configured-model',
      {sessionA: 'model-b'},
    ),
    'model-b',
  );
  assert.equal(
    selectedModelForSession(
      'sessionA',
      ['model-a', 'model-b'],
      'model-a',
      'configured-model',
      {sessionA: 'removed-model'},
    ),
    'model-a',
  );
});

test('uses the first configured model or model.json value when no profile default exists', () => {
  assert.equal(
    selectedModelForSession(undefined, ['model-a', 'model-b'], undefined, 'configured-model', {}),
    'model-a',
  );
  assert.equal(
    selectedModelForSession('sessionA', undefined, undefined, 'configured-model', {sessionA: 'model-b'}),
    'configured-model',
  );
});

test('ignores malformed stored preferences and tolerates unavailable storage', () => {
  const storage = new MemoryStorage();
  storage.value = JSON.stringify({sessionA: ' model-a ', invalid: 42, empty: '  '});
  assert.deepEqual(loadModelSelectionBySession(storage), {sessionA: 'model-a'});
  assert.deepEqual(loadModelSelectionBySession(undefined), {});
  assert.doesNotThrow(() => saveModelSelectionBySession({sessionA: 'model-a'}, undefined));
});
