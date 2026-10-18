import test from 'node:test';
import assert from 'node:assert/strict';

import {
  canOpenPane,
  closePane,
  ensurePane,
  focusPane,
  normalizePaneKeys,
  paneCapacity,
  paneIndex,
  parsePaneState,
  prunePanes,
  replaceFocusedPane,
  singlePane,
} from './pane-layout.ts';

const a = 'session1';
const b = 'session1\u0000chatgpt';
const c = 'session1\u0000reviewer';
const d = 'session2';
const e = 'session2\u0000x';
const f = 'session2\u0000y';

test('the pane capacity is a configured positive integer, else one pane', () => {
  assert.equal(paneCapacity(4), 4);
  assert.equal(paneCapacity(0), 1);
  assert.equal(paneCapacity(-3), 1);
  assert.equal(paneCapacity(2.9), 2);
  assert.equal(paneCapacity(Number.NaN), 1);
  assert.equal(paneCapacity(Number.POSITIVE_INFINITY), 1);
  assert.equal(paneCapacity(undefined), 1);
});

test('one pane is the arrangement today', () => {
  assert.deepEqual(singlePane(a), {keys: [a], focusedKey: a});
});

test('normalizing keeps the order, drops duplicates and caps from the left', () => {
  assert.deepEqual(normalizePaneKeys([a, b, a, c], 4), [a, b, c]);
  assert.deepEqual(normalizePaneKeys([a, b, c], 2), [a, b]);
  assert.deepEqual(normalizePaneKeys([a, '', 7, null, b], 4), [a, b]);
  assert.deepEqual(normalizePaneKeys([], 4), []);
});

test('a restored arrangement is validated field by field', () => {
  assert.deepEqual(parsePaneState(undefined, a, 4), singlePane(a));
  assert.deepEqual(parsePaneState(null, a, 4), singlePane(a));
  assert.deepEqual(parsePaneState('junk', a, 4), singlePane(a));
  assert.deepEqual(parsePaneState({keys: 'junk'}, a, 4), singlePane(a));
  assert.deepEqual(parsePaneState({keys: []}, a, 4), singlePane(a));
  // A focus outside the open set falls back to the first pane.
  assert.deepEqual(parsePaneState({keys: [b, c], focusedKey: 'gone'}, a, 4), {
    keys: [b, c],
    focusedKey: b,
  });
  // A stored arrangement wider than the configured maximum is capped.
  assert.deepEqual(parsePaneState({keys: [a, b, c, d, e], focusedKey: e}, a, 2), {
    keys: [a, b],
    focusedKey: a,
  });
  assert.deepEqual(parsePaneState({keys: [a, b], focusedKey: b}, a, 4), {
    keys: [a, b],
    focusedKey: b,
  });
});

test('opening a view adds a pane and focuses it', () => {
  const one = singlePane(a);
  assert.equal(canOpenPane(one, 4), true);
  const two = ensurePane(one, b, 4);
  assert.deepEqual(two, {keys: [a, b], focusedKey: b});
  // Opening one that is already open only focuses it, without reordering.
  assert.deepEqual(ensurePane(two, a, 4), {keys: [a, b], focusedKey: a});
  // Already focused: the same record comes back, so React can skip the render.
  assert.equal(ensurePane(two, b, 4), two);
});

test('at the maximum the pane on screen longest makes room', () => {
  const full = {keys: [a, b, c, d], focusedKey: d};
  assert.equal(canOpenPane(full, 4), false);
  const next = ensurePane(full, e, 4);
  assert.deepEqual(next, {keys: [b, c, d, e], focusedKey: e});
  assert.equal(next.keys.length, 4);
  // The focused pane is never the one dropped.
  const focusedFirst = {keys: [a, b], focusedKey: a};
  assert.deepEqual(ensurePane(focusedFirst, c, 2), {keys: [a, c], focusedKey: c});
});

test('a maximum of one replaces the single pane', () => {
  assert.deepEqual(ensurePane(singlePane(a), b, 1), singlePane(b));
});

test('focusing a pane that is not open changes nothing', () => {
  const state = {keys: [a, b], focusedKey: a};
  assert.deepEqual(focusPane(state, b), {keys: [a, b], focusedKey: b});
  assert.equal(focusPane(state, 'gone'), state);
  // Already focused: the same record, so the caller can skip the write.
  const focused = {keys: [a, b], focusedKey: b};
  assert.equal(focusPane(focused, b), focused);
});

test('closing a pane moves focus to its neighbour and keeps the last pane', () => {
  const three = {keys: [a, b, c], focusedKey: b};
  assert.deepEqual(closePane(three, b), {keys: [a, c], focusedKey: c});
  // Closing the focused tail pane focuses the pane at its left.
  assert.deepEqual(closePane({keys: [a, b, c], focusedKey: c}, c), {keys: [a, b], focusedKey: b});
  // Closing a pane that is not focused keeps the focus.
  assert.deepEqual(closePane(three, c), {keys: [a, b], focusedKey: b});
  // A view that is not open, and the last pane, are left alone.
  const one = singlePane(a);
  assert.equal(closePane(one, a), one);
  assert.equal(closePane(one, b), one);
});

test('choosing a row replaces the focused pane and never duplicates a view', () => {
  const state = {keys: [a, b], focusedKey: b};
  assert.deepEqual(replaceFocusedPane(state, c), {keys: [a, c], focusedKey: c});
  // Replacing with a view another pane already shows leaves that pane's view
  // open once: the duplicate collapses into the focused slot.
  assert.deepEqual(replaceFocusedPane(state, a), {keys: [a], focusedKey: a});
  assert.deepEqual(replaceFocusedPane({keys: [a, b, c], focusedKey: b}, c), {
    keys: [a, c],
    focusedKey: c,
  });
  // The focused view again: the same record, so nothing re-renders.
  const single = singlePane(a);
  assert.equal(replaceFocusedPane(single, a), single);
});

test('a pane index locates the view on screen', () => {
  assert.equal(paneIndex({keys: [a, b, c], focusedKey: a}, c), 2);
  assert.equal(paneIndex(singlePane(a), b), -1);
});

test('panes whose view disappeared are closed and focus falls back', () => {
  const state = {keys: [a, b, c], focusedKey: b};
  assert.equal(prunePanes(state, new Set([a, b, c])), state);
  assert.deepEqual(prunePanes(state, new Set([a, c])), {keys: [a, c], focusedKey: c});
  assert.deepEqual(prunePanes(state, new Set([a])), {keys: [a], focusedKey: a});
  // Nothing left would blank the screen, so the arrangement stands until a view is activated.
  assert.equal(prunePanes(state, new Set()), state);
});
