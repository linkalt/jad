import assert from 'node:assert/strict';
import test from 'node:test';

import {selectedModelForSession} from './model-selection.ts';

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
