import assert from 'node:assert/strict';
import test from 'node:test';

import {conversationPanelAfterToggle} from './right-panel.ts';

test('a pane toggles its own right-edge panel', () => {
  assert.equal(conversationPanelAfterToggle(null, 'inspector'), 'inspector');
  assert.equal(conversationPanelAfterToggle(null, 'prompts'), 'prompts');
  // The pane's own toggle closes what it shows, and the other one takes over.
  assert.equal(conversationPanelAfterToggle('inspector', 'inspector'), null);
  assert.equal(conversationPanelAfterToggle('prompts', 'prompts'), null);
  assert.equal(conversationPanelAfterToggle('inspector', 'prompts'), 'prompts');
  assert.equal(conversationPanelAfterToggle('prompts', 'inspector'), 'inspector');
});
