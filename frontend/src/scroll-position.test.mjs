import assert from 'node:assert/strict';
import test from 'node:test';

import {
  captureScrollPosition,
  restoreScrollPosition,
} from './components/scroll-position.ts';

function createAnchor(getTop) {
  return {
    isConnected: true,
    getBoundingClientRect: () => ({top: getTop()}),
  };
}

test('preserves the clicked anchor when browser scroll anchoring moves the scroller', () => {
  const scroller = {isConnected: true, scrollTop: 0};
  const anchor = createAnchor(() => 80 - scroller.scrollTop);
  const snapshot = captureScrollPosition(scroller, anchor);

  // Simulate Firefox applying native scroll anchoring after the content grows.
  scroller.scrollTop = 100;
  restoreScrollPosition(snapshot);

  assert.equal(scroller.scrollTop, 0);
  assert.equal(anchor.getBoundingClientRect().top, 80);
});

test('preserves a nonzero anchor position when content expands above it', () => {
  const scroller = {isConnected: true, scrollTop: 240};
  let layoutShift = 0;
  const anchor = createAnchor(() => 150 + layoutShift - scroller.scrollTop);
  const snapshot = captureScrollPosition(scroller, anchor);

  layoutShift = 100;
  restoreScrollPosition(snapshot);

  assert.equal(scroller.scrollTop, 340);
  assert.equal(anchor.getBoundingClientRect().top, -90);
});

test('falls back to the captured scroll position if the anchor is gone', () => {
  const scroller = {isConnected: true, scrollTop: 240};
  const anchor = createAnchor(() => 50 - scroller.scrollTop);
  const snapshot = captureScrollPosition(scroller, anchor);

  anchor.isConnected = false;
  scroller.scrollTop = 340;
  restoreScrollPosition(snapshot);

  assert.equal(scroller.scrollTop, 240);
});

test('does not write a captured position to a detached scroller', () => {
  const scroller = {isConnected: false, scrollTop: 100};
  restoreScrollPosition({scroller, scrollTop: 0, anchor: null, anchorTop: null});

  assert.equal(scroller.scrollTop, 100);
});
