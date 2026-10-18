import assert from 'node:assert/strict';
import test from 'node:test';

import {paneCountLabel, paneCountSummary} from './pane-count.ts';

// The desktop footer answers "how many conversations are open right now" without
// spending room on it: the count lives in the icon's tooltip and accessible name,
// and the panel the icon opens spells it out with how many panes are running.
test('the tooltip and accessible name spell the count out', () => {
  assert.equal(paneCountLabel(3, 4), 'Panes: 3 of 4 open');
  assert.equal(paneCountLabel(1, 4), 'Panes: 1 of 4 open');
  assert.equal(paneCountLabel(4, 4), 'Panes: 4 of 4 open');
});

test('the panel header adds how many panes are running, and only when one is', () => {
  assert.equal(paneCountSummary(2, 4, 0), '2 of 4 panes');
  assert.equal(paneCountSummary(2, 4, 1), '2 of 4 panes · 1 running');
  assert.equal(paneCountSummary(4, 4, 3), '4 of 4 panes · 3 running');
});

test('a single allowed pane is not called "panes"', () => {
  assert.equal(paneCountSummary(1, 1, 0), '1 of 1 pane');
  assert.equal(paneCountLabel(1, 1), 'Panes: 1 of 1 open');
});
