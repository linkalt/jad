import assert from 'node:assert/strict';
import test from 'node:test';

import {nextColorScheme, parseColorScheme, resolveColorScheme} from './color-scheme.ts';

test('parseColorScheme accepts only light and dark', () => {
  assert.equal(parseColorScheme('light'), 'light');
  assert.equal(parseColorScheme('dark'), 'dark');
  assert.equal(parseColorScheme('system'), undefined);
  assert.equal(parseColorScheme(undefined), undefined);
  assert.equal(parseColorScheme(null), undefined);
});

test('nextColorScheme flips between the two schemes', () => {
  assert.equal(nextColorScheme('light'), 'dark');
  assert.equal(nextColorScheme('dark'), 'light');
});

test('resolveColorScheme prefers the saved choice over the system preference', () => {
  assert.equal(resolveColorScheme('light', true), 'light');
  assert.equal(resolveColorScheme('dark', false), 'dark');
  assert.equal(resolveColorScheme(undefined, true), 'dark');
  assert.equal(resolveColorScheme('garbage', false), 'light');
});
