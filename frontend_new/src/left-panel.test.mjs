import assert from 'node:assert/strict';
import test from 'node:test';

import {LEFT_PANEL_IDS, leftPanelAfterToggle} from './left-panel.ts';

test('the same item closes the window it already holds', () => {
  for (const id of LEFT_PANEL_IDS) {
    assert.equal(leftPanelAfterToggle(id, id), null);
  }
});

test('another item hands the one window over instead of stacking', () => {
  assert.equal(leftPanelAfterToggle('search', 'notifications'), 'notifications');
  assert.equal(leftPanelAfterToggle('notifications', 'checkpoints'), 'checkpoints');
  assert.equal(leftPanelAfterToggle('checkpoints', 'settings'), 'settings');
  assert.equal(leftPanelAfterToggle('settings', 'search'), 'search');
});

test('an empty edge opens whatever is asked for', () => {
  for (const id of LEFT_PANEL_IDS) {
    assert.equal(leftPanelAfterToggle(null, id), id);
  }
});

test('notifications and checkpoints are views of the left window, not the right edge', () => {
  // The two activity panels the left footer opens are part of the same window,
  // so the app has a single floating window on the left and none on the right.
  assert.deepEqual(
    [...LEFT_PANEL_IDS].sort(),
    ['checkpoints', 'notifications', 'panes', 'search', 'settings'],
  );
});

test('the panes view is handed the same one window', () => {
  for (const current of LEFT_PANEL_IDS) {
    assert.equal(leftPanelAfterToggle(current, 'panes'), current === 'panes' ? null : 'panes');
  }
});
