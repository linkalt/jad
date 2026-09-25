import assert from 'node:assert/strict';
import test from 'node:test';

import {
  captureGrowthPosition,
  captureScrollPosition,
  describeReveal,
  holdAnchorPosition,
  holdGrowthPosition,
  restoreScrollPosition,
  revealScrollOptions,
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

// =============================================================================
// Holding a reveal steady
// =============================================================================

const FOLLOWING_ATTRIBUTE = 'data-astryx-chat-following';

/**
 * Stands in for ChatLayout's follower: it unlocks when a scroll event shows an
 * upward move, and records the position of every event it processes — the same
 * bookkeeping the real hook uses to decide that.
 */
function createScroller({
  scrollTop = 0,
  scrollHeight = 1000,
  clientHeight = 500,
  locked = false,
} = {}) {
  const scroller = {
    isConnected: true,
    scrollTop,
    scrollHeight,
    clientHeight,
    clientWidth: 800,
    offsetWidth: 815,
    locked,
    hookLastScrollTop: scrollTop,
    nudges: 0,
    respondsToScroll: true,
    listeners: new Map(),
    hasAttribute(name) {
      return name === FOLLOWING_ATTRIBUTE ? scroller.locked : false;
    },
    addEventListener(type, listener) {
      scroller.listeners.set(type, listener);
    },
    removeEventListener(type) {
      scroller.listeners.delete(type);
    },
    dispatchEvent(event) {
      assert.equal(event.type, 'scroll');
      scroller.nudges += 1;
      if (!scroller.respondsToScroll) {
        return true;
      }
      const landedAtBottom =
        scroller.scrollHeight - scroller.scrollTop - scroller.clientHeight < 1;
      const isScrollingUp = scroller.scrollTop < scroller.hookLastScrollTop;
      if (isScrollingUp && !landedAtBottom && scroller.locked) {
        scroller.locked = false;
      }
      scroller.hookLastScrollTop = scroller.scrollTop;
      return true;
    },
  };
  return scroller;
}

/** An element whose document position holds still unless layout moves it. */
function createPlacedAnchor(scroller, documentTop) {
  const anchor = {
    isConnected: true,
    shiftAbove: 0,
    getBoundingClientRect: () => ({top: documentTop + anchor.shiftAbove - scroller.scrollTop}),
  };
  return anchor;
}

/** Frames run when the test says so, so a late layout pass can be injected. */
function createScheduler() {
  const pending = [];
  return {
    schedule: callback => pending.push(callback),
    get pendingCount() {
      return pending.length;
    },
    run(frames, before) {
      for (let index = 0; index < frames; index += 1) {
        const next = pending.shift();
        if (next === undefined) {
          return;
        }
        before?.(index);
        next();
      }
    },
  };
}

function createReveal(overrides = {}) {
  const scheduler = createScheduler();
  const runs = [];
  let elapsed = 0;
  const options = {
    settleFrames: 16,
    driftSlackPx: 300,
    unlockAttempts: 3,
    schedule: scheduler.schedule,
    now: () => (elapsed += 16),
    trace: {
      onSettled: (samples, stopReason) => runs.push({samples: [...samples], stopReason}),
    },
    ...overrides,
  };
  return {options, scheduler, runs};
}

function correctionsOf(run) {
  return run.samples.filter(sample => sample.frame >= 0).map(sample => sample.correction);
}

test('holds the clicked control when the engine moves the scroller after the reveal', () => {
  const scroller = createScroller({scrollTop: 400, scrollHeight: 1000, clientHeight: 500});
  const anchor = createPlacedAnchor(scroller, 1200);
  const snapshot = captureScrollPosition(scroller, anchor);
  assert.equal(snapshot.anchorTop, 800);

  // The reveal grows the content and the engine moves the reader down 80px.
  scroller.scrollHeight = 1300;
  scroller.scrollTop = 480;
  const {options, scheduler, runs} = createReveal();

  holdAnchorPosition(snapshot, options);

  assert.equal(scroller.scrollTop, 400, 'the first correction puts the control back');

  // A later layout pass moves it again, after our one-shot correction is done.
  scheduler.run(1, () => {
    scroller.scrollTop = 430;
  });
  assert.equal(scroller.scrollTop, 400, 'the late pass is corrected too');

  scheduler.run(20);

  assert.equal(scroller.scrollTop, 400);
  assert.equal(anchor.getBoundingClientRect().top, 800);
  assert.equal(runs.length, 1);
  assert.equal(runs[0].stopReason, 'settled');
  assert.deepEqual(correctionsOf(runs[0]).slice(0, 2), [-80, -30]);
  assert.ok(correctionsOf(runs[0]).slice(2).every(correction => correction === 0));
  const last = runs[0].samples.at(-1);
  assert.equal(last.anchorTop, 800);
  // The reader kept their place; only the range below them grew.
  assert.equal(last.fromBottom, 400);
  assert.equal(scheduler.pendingCount, 0);
});

test('retries the nudge when the follower is still locked by a stale position', () => {
  const scroller = createScroller({
    scrollTop: 400,
    scrollHeight: 1000,
    clientHeight: 500,
    locked: true,
  });
  const anchor = createPlacedAnchor(scroller, 1200);
  const snapshot = captureScrollPosition(scroller, anchor);

  // The reveal clamps the container down, and that scroll event has not
  // reached the hook yet: its bookkeeping still says 400.
  scroller.scrollHeight = 1300;
  scroller.scrollTop = 480;
  const {options, scheduler, runs} = createReveal();

  holdAnchorPosition(snapshot, options);

  assert.equal(scroller.nudges, 2, 'the first nudge was not an upward move');
  assert.equal(scroller.locked, false, 'the follower was released');
  assert.equal(scroller.scrollTop, 400, 'the nudge residue is corrected');

  scheduler.run(20);

  assert.equal(runs[0].stopReason, 'settled');
  assert.equal(runs[0].samples.at(-1).following, false);
});

test('gives up instead of fighting a container that is still following', () => {
  const scroller = createScroller({
    scrollTop: 400,
    scrollHeight: 1000,
    clientHeight: 500,
    locked: true,
  });
  scroller.respondsToScroll = false;
  const anchor = createPlacedAnchor(scroller, 1200);
  const snapshot = captureScrollPosition(scroller, anchor);
  scroller.scrollHeight = 1300;
  scroller.scrollTop = 480;
  const {options, scheduler, runs} = createReveal();

  holdAnchorPosition(snapshot, options);

  assert.equal(scroller.nudges, 3);
  assert.equal(scroller.locked, true);
  assert.equal(runs[0].stopReason, 'still-following');
  assert.equal(runs[0].samples.length, 2, 'the captured frame and one correction');
  assert.equal(scroller.scrollTop, 400);
  assert.equal(scheduler.pendingCount, 0);
});

test('holds the reader by total growth when older messages are inserted above', () => {
  const scroller = createScroller({scrollTop: 400, scrollHeight: 1000, clientHeight: 500});
  const snapshot = captureGrowthPosition(scroller);
  assert.equal(scroller.scrollHeight - scroller.scrollTop - scroller.clientHeight, 100);

  scroller.scrollHeight = 1300;
  const {options, scheduler, runs} = createReveal();

  holdGrowthPosition(snapshot, options);
  assert.equal(scroller.scrollTop, 700);

  // A row inserted above measures taller than the estimate a frame later.
  scheduler.run(1, () => {
    scroller.scrollHeight = 1500;
  });
  assert.equal(scroller.scrollTop, 900);

  scheduler.run(20);

  assert.equal(scroller.scrollTop, 900);
  assert.equal(scroller.scrollHeight - scroller.scrollTop - scroller.clientHeight, 100);
  assert.equal(runs[0].stopReason, 'settled');
  assert.equal(scheduler.pendingCount, 0);
});

test('stops correcting as soon as the reader takes over', () => {
  const scroller = createScroller({scrollTop: 400, scrollHeight: 1000, clientHeight: 500});
  const anchor = createPlacedAnchor(scroller, 1200);
  const snapshot = captureScrollPosition(scroller, anchor);
  const {options, scheduler, runs} = createReveal();

  holdAnchorPosition(snapshot, options);

  const wheel = scroller.listeners.get('wheel');
  assert.equal(typeof wheel, 'function');

  wheel();
  scroller.scrollTop = 250;
  scheduler.run(20);

  assert.equal(scroller.scrollTop, 250, 'the reader keeps the position they chose');
  assert.equal(runs[0].stopReason, 'reader');
  assert.equal(scroller.listeners.size, 0);
});

test("refuses to chase movement the container's own size cannot explain", () => {
  const scroller = createScroller({scrollTop: 400, scrollHeight: 1000, clientHeight: 500});
  const anchor = createPlacedAnchor(scroller, 1200);
  const snapshot = captureScrollPosition(scroller, anchor);
  const {options, scheduler, runs} = createReveal();

  holdAnchorPosition(snapshot, options);
  scheduler.run(1, () => {
    scroller.scrollTop = 4000;
  });

  assert.equal(runs[0].stopReason, 'correction-too-large');
  assert.equal(scroller.scrollTop, 4000, 'the move is left alone, not chased');
  assert.equal(scheduler.pendingCount, 0);
});

test('follows a late pass that the container measuring a row explains', () => {
  const scroller = createScroller({scrollTop: 2000, scrollHeight: 3000, clientHeight: 500});
  const anchor = createPlacedAnchor(scroller, 3200);
  const snapshot = captureScrollPosition(scroller, anchor);
  assert.equal(snapshot.anchorTop, 1200);

  const {options, scheduler, runs} = createReveal();

  holdAnchorPosition(snapshot, options);
  // A row above the reader is measured 800px shorter than its estimate.
  scheduler.run(1, () => {
    scroller.scrollHeight = 2200;
    anchor.shiftAbove = -800;
  });

  assert.equal(scroller.scrollTop, 1200, 'the reader travels with the content above');
  assert.equal(anchor.getBoundingClientRect().top, 1200, 'and keeps looking at the same pixels');

  scheduler.run(20);

  assert.equal(runs[0].samples.at(-1).heightChange, 0, 'the geometry went quiet');
  const measured = runs[0].samples.find(sample => sample.heightChange !== 0);
  assert.equal(measured.heightChange, -800);
  assert.equal(measured.correction, -800);
  assert.deepEqual(correctionsOf(runs[0]).slice(0, 2), [0, -800]);
  assert.equal(runs[0].stopReason, 'settled');
});

test('falls back to the captured position when the control unmounts', () => {
  const scroller = createScroller({scrollTop: 400, scrollHeight: 1000, clientHeight: 500});
  const anchor = createPlacedAnchor(scroller, 1200);
  const snapshot = captureScrollPosition(scroller, anchor);
  scroller.scrollHeight = 1300;
  scroller.scrollTop = 700;
  // The last reveal removes the control that was being held.
  anchor.isConnected = false;
  const {options, scheduler, runs} = createReveal();

  holdAnchorPosition(snapshot, options);

  assert.equal(scroller.scrollTop, 400);
  assert.equal(runs[0].stopReason, 'anchor-gone');
  assert.equal(scheduler.pendingCount, 0);
});

test('ignores a reveal with nothing left to hold', () => {
  const scroller = createScroller({scrollTop: 400});
  const {options} = createReveal();

  holdAnchorPosition(null, options);
  holdGrowthPosition(null, options);

  const detached = createScroller({scrollTop: 400});
  detached.isConnected = false;
  holdAnchorPosition(captureScrollPosition(detached, createPlacedAnchor(detached, 100)), options);
  holdGrowthPosition(captureGrowthPosition(detached), options);

  // No anchor captured: the recorded position is the only thing to restore.
  holdAnchorPosition(captureScrollPosition(scroller), options);

  assert.equal(scroller.scrollTop, 400);
  assert.equal(scroller.listeners.size, 0);
});

test('reads the window from config and reports one line when the probe is on', () => {
  const config = {
    scroll_debug: true,
    scroll_settle_frames: 12,
    scroll_drift_slack_px: 300,
    scroll_unlock_attempts: 3,
  };
  const lines = [];
  const fromConfig = revealScrollOptions('steps', config, line => lines.push(line));
  const scheduler = createScheduler();
  let elapsed = 0;

  const scroller = createScroller({scrollTop: 400, scrollHeight: 1000, clientHeight: 500});
  const anchor = createPlacedAnchor(scroller, 1200);
  holdAnchorPosition(captureScrollPosition(scroller, anchor), {
    ...fromConfig,
    schedule: scheduler.schedule,
    now: () => (elapsed += 16),
  });
  scheduler.run(20);

  assert.equal(lines.length, 1);
  assert.match(lines[0], /^\[scroll-debug\] reveal steps: /);
  assert.match(lines[0], /stop=settled/);
  assert.match(lines[0], /scrollTop 400→400/);
  assert.match(lines[0], /fromBottom 100→100/);
  assert.match(lines[0], /following false→false/);
  assert.match(lines[0], /anchor 800→800/);

  const quiet = revealScrollOptions('steps', {...config, scroll_debug: false}, () =>
    assert.fail('the probe is off'),
  );
  assert.equal(quiet.trace, undefined);
  assert.deepEqual(
    {
      settleFrames: quiet.settleFrames,
      driftSlackPx: quiet.driftSlackPx,
      unlockAttempts: quiet.unlockAttempts,
    },
    {settleFrames: 12, driftSlackPx: 300, unlockAttempts: 3},
  );
  assert.equal(describeReveal([], 'budget'), 'no frames | stop=budget');
});
