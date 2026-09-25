import assert from 'node:assert/strict';
import test from 'node:test';

import {selectionAfterPanelToggle} from './right-panel.ts';

test('clicking the panel that already holds the right edge closes it', () => {
  assert.equal(selectionAfterPanelToggle('notifications', 'notifications'), null);
  assert.equal(selectionAfterPanelToggle('inspector', 'inspector'), null);
});

test('clicking the other panel hands the edge over instead of closing it', () => {
  assert.equal(selectionAfterPanelToggle('inspector', 'notifications'), 'notifications');
  assert.equal(selectionAfterPanelToggle('notifications', 'inspector'), 'inspector');
});

test('clicking a panel while the right edge is empty opens it', () => {
  assert.equal(selectionAfterPanelToggle(null, 'notifications'), 'notifications');
  assert.equal(selectionAfterPanelToggle(null, 'inspector'), 'inspector');
});
