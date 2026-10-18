import assert from 'node:assert/strict';
import test from 'node:test';

import {
  NO_PANE_DRAWERS,
  closePaneDrawer,
  hasPaneDrawer,
  openPaneDrawer,
  togglePaneDrawer,
} from './pane-drawers.ts';

test('opening a drawer in one pane leaves the other panes open', () => {
  const one = openPaneDrawer(NO_PANE_DRAWERS, 'a');
  assert.equal(hasPaneDrawer(one, 'a'), true);
  const two = openPaneDrawer(one, 'b');
  assert.equal(hasPaneDrawer(two, 'a'), true, 'pane a keeps its drawer');
  assert.equal(hasPaneDrawer(two, 'b'), true, 'pane b gains its own');
  assert.equal(hasPaneDrawer(NO_PANE_DRAWERS, 'a'), false, 'the input is not mutated');
});

test('a pane\'s own control toggles only its pane', () => {
  const open = openPaneDrawer(openPaneDrawer(NO_PANE_DRAWERS, 'a'), 'b');
  const closedA = togglePaneDrawer(open, 'a');
  assert.equal(hasPaneDrawer(closedA, 'a'), false);
  assert.equal(hasPaneDrawer(closedA, 'b'), true, 'pane b is untouched by pane a\'s toggle');
  const reopenedA = togglePaneDrawer(closedA, 'a');
  assert.equal(hasPaneDrawer(reopenedA, 'a'), true);
  assert.equal(hasPaneDrawer(reopenedA, 'b'), true);
});

test('closing one pane\'s drawer never closes another\'s', () => {
  const both = openPaneDrawer(openPaneDrawer(NO_PANE_DRAWERS, 'a'), 'b');
  const onlyB = closePaneDrawer(both, 'a');
  assert.deepEqual([...onlyB], ['b']);
  assert.equal(closePaneDrawer(onlyB, 'a'), onlyB, 'closing a closed drawer changes nothing');
});

test('opening twice and closing twice are idempotent', () => {
  const once = openPaneDrawer(NO_PANE_DRAWERS, 'a');
  assert.equal(openPaneDrawer(once, 'a'), once, 'no new set when already open');
  const empty = closePaneDrawer(once, 'a');
  assert.deepEqual([...empty], []);
  assert.equal(closePaneDrawer(empty, 'a'), empty);
});

test('four panes can show the same drawer at once', () => {
  let keys = NO_PANE_DRAWERS;
  for (const key of ['a', 'b', 'c', 'd']) {
    keys = openPaneDrawer(keys, key);
  }
  assert.equal(keys.size, 4);
  for (const key of ['a', 'b', 'c', 'd']) {
    assert.equal(hasPaneDrawer(keys, key), true);
  }
});
