import test from 'node:test';
import assert from 'node:assert/strict';

import {paneAfterStep, paneShortcutFor} from './pane-shortcuts.ts';

const key = (overrides) => ({key: 'ArrowRight', altKey: false, ctrlKey: false, metaKey: false, shiftKey: false, ...overrides});

test('Ctrl/Cmd+Alt+Arrow walks the panes, and Alt+W closes one', () => {
  assert.equal(paneShortcutFor(key({key: 'ArrowRight', ctrlKey: true, altKey: true})), 'next');
  assert.equal(paneShortcutFor(key({key: 'ArrowLeft', ctrlKey: true, altKey: true})), 'previous');
  assert.equal(paneShortcutFor(key({key: 'w', ctrlKey: true, altKey: true})), 'close');
  // The same three on the Apple modifier.
  assert.equal(paneShortcutFor(key({key: 'ArrowLeft', metaKey: true, altKey: true})), 'previous');
  assert.equal(paneShortcutFor(key({key: 'W', metaKey: true, altKey: true})), 'close');
});

test('the modifiers a text field needs are left alone', () => {
  // Alt+Arrow jumps the caret by word inside the composer.
  assert.equal(paneShortcutFor(key({key: 'ArrowRight', altKey: true})), null);
  // Plain arrows move the caret and the window.
  assert.equal(paneShortcutFor(key({key: 'ArrowRight'})), null);
  assert.equal(paneShortcutFor(key({key: 'w'})), null);
  // Ctrl+Arrow and Cmd+Arrow are the browser's and the platform's.
  assert.equal(paneShortcutFor(key({key: 'ArrowRight', ctrlKey: true})), null);
  assert.equal(paneShortcutFor(key({key: 'ArrowRight', metaKey: true})), null);
  // Shift is not claimed at all, and Ctrl+Shift+W closes the tab.
  assert.equal(paneShortcutFor(key({key: 'w', ctrlKey: true, altKey: true, shiftKey: true})), null);
  assert.equal(paneShortcutFor(key({key: 'w', ctrlKey: true, shiftKey: true})), null);
  // An unrelated key with the right modifiers is not a pane shortcut.
  assert.equal(paneShortcutFor(key({key: 'k', ctrlKey: true, altKey: true})), null);
});

test('a step wraps around the arrangement at both ends', () => {
  const keys = ['a', 'b', 'c'];
  assert.equal(paneAfterStep(keys, 'a', 1), 'b');
  assert.equal(paneAfterStep(keys, 'b', 1), 'c');
  assert.equal(paneAfterStep(keys, 'c', 1), 'a');
  assert.equal(paneAfterStep(keys, 'a', -1), 'c');
  assert.equal(paneAfterStep(keys, 'b', -1), 'a');
  // A single pane steps to itself, and an unknown focus falls back to the first.
  assert.equal(paneAfterStep(['only'], 'only', 1), 'only');
  assert.equal(paneAfterStep(keys, 'gone', 1), 'a');
  assert.equal(paneAfterStep([], 'gone', 1), 'gone');
  // A step larger than the arrangement still lands inside it.
  assert.equal(paneAfterStep(keys, 'a', 5), 'c');
  assert.equal(paneAfterStep(keys, 'a', -5), 'b');
});
