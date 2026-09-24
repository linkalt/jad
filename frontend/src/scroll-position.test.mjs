import assert from 'node:assert/strict';
import test from 'node:test';

import {
  captureScrollPosition,
  restoreScrollPosition,
} from './components/scroll-position.ts';

test('keeps the transcript at the top when expanding hidden steps changes scroll height', () => {
  const scroller = {isConnected: true, scrollTop: 0};
  const anchor = captureScrollPosition(scroller);

  // Simulate the browser's native scroll anchoring after the inserted content grows.
  scroller.scrollTop = 100;
  restoreScrollPosition(anchor);

  assert.equal(scroller.scrollTop, 0);
});

test('restores a nonzero scroll position after content expansion', () => {
  const scroller = {isConnected: true, scrollTop: 240};
  const anchor = captureScrollPosition(scroller);

  scroller.scrollTop = 340;
  restoreScrollPosition(anchor);

  assert.equal(scroller.scrollTop, 240);
});

test('does not write a captured position to a detached scroller', () => {
  const scroller = {isConnected: false, scrollTop: 100};
  restoreScrollPosition({scroller, scrollTop: 0});

  assert.equal(scroller.scrollTop, 100);
});
