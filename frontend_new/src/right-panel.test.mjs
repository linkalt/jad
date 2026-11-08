import assert from 'node:assert/strict';
import test from 'node:test';

import {activityClickOutcome, conversationPanelAfterToggle} from './right-panel.ts';

test('a pane toggles its own right-edge panel', () => {
  assert.equal(conversationPanelAfterToggle(null, 'inspector'), 'inspector');
  assert.equal(conversationPanelAfterToggle(null, 'prompts'), 'prompts');
  // The pane's own toggle closes what it shows, and the other one takes over.
  assert.equal(conversationPanelAfterToggle('inspector', 'inspector'), null);
  assert.equal(conversationPanelAfterToggle('prompts', 'prompts'), null);
  assert.equal(conversationPanelAfterToggle('inspector', 'prompts'), 'prompts');
  assert.equal(conversationPanelAfterToggle('prompts', 'inspector'), 'inspector');
});

test('a transcript block opens the activity panel and a second click on it closes it', () => {
  assert.equal(activityClickOutcome(null, undefined, 'a'), 'open');
  assert.equal(activityClickOutcome('activity', 'a', 'a'), 'close');
  assert.equal(activityClickOutcome('activity', 'a', 'b'), 'open');
  // Another panel on the edge: the block takes it over even with a stale key.
  assert.equal(activityClickOutcome('prompts', 'a', 'a'), 'open');
});
