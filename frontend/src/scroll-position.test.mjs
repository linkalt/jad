import assert from 'node:assert/strict';
import test from 'node:test';

import {
  captureScrollPosition,
  restoreScrollPosition,
  unlockScrollFollower,
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

test('unlocks chat scroll-follow and restores the one-pixel nudge', () => {
  let lastScrollTop = 240;
  let isLocked = true;
  const scroller = {
    isConnected: true,
    scrollTop: 240,
    scrollHeight: 1000,
    clientHeight: 500,
    dispatchEvent(event) {
      assert.equal(event.type, 'scroll');
      if (this.scrollTop < lastScrollTop) {
        isLocked = false;
      }
      lastScrollTop = this.scrollTop;
      return true;
    },
  };
  const anchor = createAnchor(() => 320 - scroller.scrollTop);
  const snapshot = captureScrollPosition(scroller, anchor);

  unlockScrollFollower(scroller);

  assert.equal(isLocked, false);
  assert.equal(scroller.scrollTop, 239);
  restoreScrollPosition(snapshot);
  assert.equal(scroller.scrollTop, 240);
  assert.equal(anchor.getBoundingClientRect().top, 80);
});

test('unlocks scroll-follow from the top once the reveal creates scroll range', () => {
  let lastScrollTop = 0;
  let isLocked = true;
  const positions = [];
  const scroller = {
    scrollTop: 0,
    scrollHeight: 300,
    clientHeight: 200,
    dispatchEvent(event) {
      assert.equal(event.type, 'scroll');
      positions.push(this.scrollTop);
      const isScrollingUp = this.scrollTop < lastScrollTop;
      const landedAtBottom =
        this.scrollHeight - this.scrollTop - this.clientHeight < 1;
      if (isScrollingUp && !landedAtBottom && isLocked) {
        isLocked = false;
      }
      lastScrollTop = this.scrollTop;
      return true;
    },
  };

  unlockScrollFollower(scroller);

  assert.deepEqual(positions, [1, 0]);
  assert.equal(isLocked, false);
  assert.equal(scroller.scrollTop, 0);
});

test('does not synthesize scrolling when the container has no scroll range', () => {
  let scrollEvents = 0;
  const scroller = {
    scrollTop: 0,
    scrollHeight: 100,
    clientHeight: 100,
    dispatchEvent() {
      scrollEvents += 1;
      return true;
    },
  };

  unlockScrollFollower(scroller);

  assert.equal(scrollEvents, 0);
  assert.equal(scroller.scrollTop, 0);
});
