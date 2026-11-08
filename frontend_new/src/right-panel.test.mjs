import assert from 'node:assert/strict';
import test from 'node:test';

import {activityClickOutcome, activityRefsChanged, conversationPanelAfterToggle, toolGroupActivityKey} from './right-panel.ts';

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

test('the open activity panel refreshes when a group gains a call or a result', () => {
  const call = {};
  const result = {};
  assert.equal(activityRefsChanged(undefined, [call]), true);
  assert.equal(activityRefsChanged([call, undefined], [call, undefined]), false);
  assert.equal(activityRefsChanged([call, undefined], [call, result]), true);
  assert.equal(activityRefsChanged([call, undefined], [call, undefined, {}, undefined]), true);
});

test('a tool group key is stable while it grows and differs from a call key', () => {
  assert.equal(toolGroupActivityKey('m1', 4), toolGroupActivityKey('m1', 4));
  assert.notEqual(toolGroupActivityKey('m1', 4), toolGroupActivityKey('m1', 5));
  assert.notEqual(toolGroupActivityKey('m1', 4), 'tool-c1');
});
