export type ScrollPositionAnchor = Pick<
  HTMLElement,
  'isConnected' | 'getBoundingClientRect'
>;

/**
 * The parts of a scroll container the reveal code drives. Kept structural so a
 * test can stand in for the transcript scroller without a DOM.
 */
export interface ScrollScroller {
  isConnected: boolean;
  scrollTop: number;
  scrollHeight: number;
  clientHeight: number;
  clientWidth: number;
  offsetWidth: number;
  hasAttribute: (name: string) => boolean;
  addEventListener: (
    type: string,
    listener: () => void,
    options?: AddEventListenerOptions,
  ) => void;
  removeEventListener: (type: string, listener: () => void) => void;
  dispatchEvent: (event: Event) => boolean;
}

export interface ScrollPositionSnapshot {
  scroller: ScrollScroller;
  scrollTop: number;
  anchor: ScrollPositionAnchor | null;
  anchorTop: number | null;
}

/** A scroller position captured before a reveal, held by total height. */
export interface GrowthSnapshot {
  scroller: ScrollScroller;
  scrollTop: number;
  scrollHeight: number;
}

/** Nearest ancestor that can scroll vertically. */
export function findScrollParent(node: HTMLElement): HTMLElement | null {
  let current = node.parentElement;
  while (current !== null) {
    const {overflowY} = getComputedStyle(current);
    if (
      (overflowY === 'auto' || overflowY === 'scroll') &&
      current.scrollHeight > current.clientHeight
    ) {
      return current;
    }
    current = current.parentElement;
  }
  return null;
}

/** Capture a scroller position and an optional element's viewport position. */
export function captureScrollPosition(
  scroller: ScrollPositionSnapshot['scroller'] | null,
  anchor: ScrollPositionAnchor | null = null,
): ScrollPositionSnapshot | null {
  return scroller === null
    ? null
    : {
        scroller,
        scrollTop: scroller.scrollTop,
        anchor,
        anchorTop: anchor?.getBoundingClientRect().top ?? null,
      };
}

/** Capture a scroller position to hold by total height instead of an anchor. */
export function captureGrowthPosition(scroller: ScrollScroller | null): GrowthSnapshot | null {
  return scroller === null
    ? null
    : {scroller, scrollTop: scroller.scrollTop, scrollHeight: scroller.scrollHeight};
}

/** Preserve the anchor's viewport position despite browser scroll anchoring. */
export function restoreScrollPosition(snapshot: ScrollPositionSnapshot | null): void {
  if (snapshot === null || !snapshot.scroller.isConnected) {
    return;
  }

  const {scroller, anchor, anchorTop} = snapshot;
  if (anchor !== null && anchor.isConnected && anchorTop !== null) {
    const correction = anchor.getBoundingClientRect().top - anchorTop;
    if (correction !== 0) {
      scroller.scrollTop += correction;
    }
    return;
  }

  scroller.scrollTop = snapshot.scrollTop;
}

/**
 * Revealing history is explicit reader intent. ChatLayout's follow hook unlocks
 * on an upward scroll event, so nudge the scroller and notify it before its
 * ResizeObserver follows the expanded content. At scrollTop 0, create a
 * temporary one-pixel range and return to the top so the hook sees an upward
 * move. The captured anchor restores any nudge after React commits the reveal.
 */
export function unlockScrollFollower(
  scroller: Pick<
    HTMLElement,
    'scrollTop' | 'scrollHeight' | 'clientHeight' | 'dispatchEvent'
  > | null,
): void {
  if (scroller === null) {
    return;
  }

  const maxScrollTop = Math.max(0, scroller.scrollHeight - scroller.clientHeight);
  if (maxScrollTop < 1) {
    return;
  }

  if (scroller.scrollTop > 0) {
    scroller.scrollTop = Math.max(0, scroller.scrollTop - 1);
    scroller.dispatchEvent(new Event('scroll'));
    return;
  }

  scroller.scrollTop = 1;
  scroller.dispatchEvent(new Event('scroll'));
  scroller.scrollTop = 0;
  scroller.dispatchEvent(new Event('scroll'));
}

// =============================================================================
// Holding a reveal steady
// =============================================================================

/**
 * Revealing history changes the scroller's height while the reader is looking
 * at it, and each engine applies that change on its own schedule: transcript
 * rows carry `content-visibility: auto`, so the container's height is an
 * estimate until a row is measured, and one reveal can be followed by more than
 * one layout pass. A single correction inside one layout effect therefore only
 * settles the engines that finish their layout in the same frame.
 *
 * Most of this window is *not* the correction. Scroll anchoring applies during
 * layout — before the frame is painted — while anything written from a frame
 * callback lands after that frame is on screen. So the engine holds late passes
 * better than this module can, and the window deliberately leaves the
 * container's anchoring alone: it releases ChatLayout's follower (which is what
 * turns anchoring back on), takes one correction before paint, and then watches
 * for movement the engine did not absorb.
 *
 * The window is deliberately narrow: its first correction is exactly the one
 * the call sites made before it existed, it gives up on a container ChatLayout
 * is still animating instead of fighting that animation, and any reader input
 * closes it at once.
 */

/** ChatLayout toggles this while its streaming follower owns the position. */
const FOLLOWING_ATTRIBUTE = 'data-astryx-chat-following';

/** Reader intent; any of these ends the settle window immediately. */
const READER_EVENT_TYPES = ['wheel', 'touchstart', 'keydown', 'pointerdown'];

/** Consecutive quiet frames that close the window before its budget runs out. */
const STABLE_FRAMES_TO_STOP = 8;

/** Drift below this is sub-pixel noise, not movement worth chasing. */
const NO_MOVEMENT_PX = 0.5;

/** Frame index of the state captured before the window's own correction. */
const CAPTURED_FRAME = -1;

export type RevealStopReason =
  | 'settled'
  | 'budget'
  | 'reader'
  | 'still-following'
  | 'anchor-gone'
  | 'disconnected'
  | 'correction-too-large';

/** One frame of the reveal window, for the `scroll_debug` readout. */
export interface RevealDebugSample {
  /** `CAPTURED_FRAME` before the window's correction, then 0, 1, 2, … */
  frame: number;
  timeMs: number;
  scrollTop: number;
  scrollHeight: number;
  clientHeight: number;
  /** Distance from the bottom — what the scrollbar's thumb reflects. */
  fromBottom: number;
  anchorTop: number | null;
  correction: number;
  following: boolean;
  /** How much the container's own height changed this frame. */
  heightChange: number;
  /** Space the scrollbar takes out of the content box. */
  gutter: number;
}

export interface RevealTrace {
  onSettled(samples: readonly RevealDebugSample[], stopReason: RevealStopReason): void;
}

export interface RevealOptions {
  /** Frames the window may run for, including its synchronous first frame. */
  settleFrames: number;
  /**
   * Movement beyond the container's own height change that a later frame may
   * still correct. A geometry change explains a shift of the same size, so the
   * allowance is that change plus this slack; anything bigger is somebody
   * else's animation and this window stays out of it.
   */
  driftSlackPx: number;
  /** Nudges allowed to make ChatLayout stop following. */
  unlockAttempts: number;
  schedule?: (callback: () => void) => void;
  now?: () => number;
  trace?: RevealTrace;
}

/** The `scroll_*` settings a reveal reads. */
export interface RevealScrollConfig {
  scroll_debug: boolean;
  scroll_settle_frames: number;
  scroll_drift_slack_px: number;
  scroll_unlock_attempts: number;
}

interface SettleStep {
  /** Pixels to add to `scrollTop`; 0 when the anchor is already in place. */
  correction: number;
  /** Whether the geometry stopped changing since the previous frame. */
  settled: boolean;
  /** The anchor's viewport position this frame, for the readout. */
  anchorTop: number | null;
}

/** Build a window from the app config, with the debug readout when it is on. */
export function revealScrollOptions(
  label: string,
  config: RevealScrollConfig,
  log: (line: string) => void = line => console.info(line),
): RevealOptions {
  return {
    settleFrames: config.scroll_settle_frames,
    driftSlackPx: config.scroll_drift_slack_px,
    unlockAttempts: config.scroll_unlock_attempts,
    ...(config.scroll_debug
      ? {
          trace: {
            onSettled: (samples, stopReason) =>
              log(`[scroll-debug] reveal ${label}: ${describeReveal(samples, stopReason)}`),
          },
        }
      : {}),
  };
}

/** One console line per reveal: the numbers that tell the engines apart. */
export function describeReveal(
  samples: readonly RevealDebugSample[],
  stopReason: RevealStopReason,
): string {
  const before = samples.find(sample => sample.frame === CAPTURED_FRAME) ?? samples[0];
  const after = samples[samples.length - 1];
  if (before === undefined || after === undefined) {
    return `no frames | stop=${stopReason}`;
  }
  const corrections = samples
    .filter(sample => sample.frame !== CAPTURED_FRAME)
    .map(sample => round(sample.correction))
    .join(',');
  const anchor =
    before.anchorTop === null
      ? ''
      : ` | anchor ${round(before.anchorTop)}→${round(after.anchorTop ?? before.anchorTop)}`;
  return [
    `frames=${samples.length - 1} stop=${stopReason}`,
    `scrollTop ${round(before.scrollTop)}→${round(after.scrollTop)}`,
    `scrollHeight ${round(before.scrollHeight)}→${round(after.scrollHeight)}`,
    `fromBottom ${round(before.fromBottom)}→${round(after.fromBottom)}`,
    `clientHeight ${after.clientHeight} gutter ${after.gutter}`,
    `following ${before.following}→${after.following}`,
    `corrections [${corrections}]`,
    `${round(after.timeMs)}ms`,
  ]
    .join(' | ')
    .concat(anchor);
}

/**
 * Hold the clicked control where it was: the reader keeps looking at the same
 * pixels while content is revealed around it. This is what the history controls
 * need, because they sit beside the content being revealed.
 */
export function holdAnchorPosition(
  snapshot: ScrollPositionSnapshot | null,
  options: RevealOptions,
): void {
  if (snapshot === null || !snapshot.scroller.isConnected) {
    return;
  }
  const {scroller, anchor, anchorTop} = snapshot;
  if (anchor === null || anchorTop === null) {
    restoreScrollPosition(snapshot);
    return;
  }
  const anchorTopNow = () => (anchor.isConnected ? anchor.getBoundingClientRect().top : null);
  runSettleWindow({
    scroller,
    options,
    capturedAnchorTop: anchorTop,
    apply: () => {
      const currentTop = anchorTopNow();
      if (currentTop === null) {
        return null;
      }
      const drift = currentTop - anchorTop;
      const still = Math.abs(drift) < NO_MOVEMENT_PX;
      return {correction: still ? 0 : drift, settled: still, anchorTop: currentTop};
    },
    // The control unmounted, so its former position cannot be measured. Fall
    // back to the captured position, as the reveal controls did before.
    onUnmeasurable: () => restoreScrollPosition(snapshot),
  });
}

/**
 * Hold the reader's place by total height: used where history is inserted
 * before the content being read, so the viewport has to travel down by exactly
 * what the scroller grew.
 */
export function holdGrowthPosition(snapshot: GrowthSnapshot | null, options: RevealOptions): void {
  if (snapshot === null || !snapshot.scroller.isConnected) {
    return;
  }
  const {scroller, scrollTop, scrollHeight} = snapshot;
  let previousGrowth = scroller.scrollHeight - scrollHeight;
  runSettleWindow({
    scroller,
    options,
    apply: () => {
      const growth = scroller.scrollHeight - scrollHeight;
      const settled = Math.abs(growth - previousGrowth) < NO_MOVEMENT_PX;
      previousGrowth = growth;
      return {correction: scrollTop + growth - scroller.scrollTop, settled, anchorTop: null};
    },
  });
}

/**
 * Correct once before paint, then keep correcting while the window is open.
 * `apply` returns the pixels to add to `scrollTop`, or null when it has nothing
 * left to measure.
 */
function runSettleWindow({
  scroller,
  options,
  apply,
  capturedAnchorTop,
  onUnmeasurable,
}: {
  scroller: ScrollScroller;
  options: RevealOptions;
  apply: () => SettleStep | null;
  /** The anchor position the reveal promised to hold, for the readout. */
  capturedAnchorTop?: number | null;
  onUnmeasurable?: () => void;
}): void {
  const schedule =
    options.schedule ??
    ((callback: () => void) => {
      requestAnimationFrame(callback);
    });
  const now = options.now ?? (() => performance.now());
  const samples: RevealDebugSample[] | null = options.trace ? [] : null;
  // ChatLayout stops following on its own only when it sees an upward move, so
  // release it before anything is corrected; see releaseFollower. Releasing it
  // also hands late layout passes back to the browser's own scroll anchoring,
  // which holds them in the frame they happen.
  const released = releaseFollower(scroller, options.unlockAttempts);

  let cancelled = false;
  const cancel = () => {
    cancelled = true;
  };
  let frame = 0;
  let stableFrames = 0;
  let previousHeight = scroller.scrollHeight;
  let stopReason: RevealStopReason = 'budget';

  const finish = () => {
    for (const type of READER_EVENT_TYPES) {
      scroller.removeEventListener(type, cancel);
    }
    options.trace?.onSettled(samples ?? [], stopReason);
  };

  const record = (
    frameIndex: number,
    anchorTop: number | null,
    correction: number,
    heightChange: number,
  ) => {
    if (samples === null) {
      return;
    }
    samples.push({
      frame: frameIndex,
      timeMs: round(now()),
      scrollTop: round(scroller.scrollTop),
      scrollHeight: round(scroller.scrollHeight),
      clientHeight: scroller.clientHeight,
      fromBottom: round(scroller.scrollHeight - scroller.scrollTop - scroller.clientHeight),
      anchorTop,
      correction: round(correction),
      following: scroller.hasAttribute(FOLLOWING_ATTRIBUTE),
      heightChange: round(heightChange),
      gutter: scroller.offsetWidth - scroller.clientWidth,
    });
  };

  const tick = () => {
    if (cancelled || !scroller.isConnected) {
      stopReason = cancelled ? 'reader' : 'disconnected';
      finish();
      return;
    }

    const step = apply();
    if (step === null) {
      onUnmeasurable?.();
      stopReason = 'anchor-gone';
      finish();
      return;
    }

    const height = scroller.scrollHeight;
    const heightChange = height - previousHeight;
    previousHeight = height;

    // The first frame is the correction these call sites made before this
    // window existed. Later frames only follow movement the container's own
    // size change explains; a shift with no geometry behind it is somebody
    // else's animation, and correcting through it would fight that.
    const allowance = Math.abs(heightChange) + options.driftSlackPx;
    if (frame > 0 && Math.abs(step.correction) > allowance) {
      stopReason = 'correction-too-large';
      record(frame, step.anchorTop, step.correction, heightChange);
      finish();
      return;
    }

    if (step.correction !== 0) {
      scroller.scrollTop += step.correction;
    }
    record(frame, step.anchorTop, step.correction, heightChange);
    frame += 1;
    stableFrames = step.settled ? stableFrames + 1 : 0;

    if (!released) {
      // A container ChatLayout still follows animates to its bottom on the next
      // resize, which would undo this correction anyway.
      stopReason = 'still-following';
      finish();
      return;
    }
    if (stableFrames >= STABLE_FRAMES_TO_STOP) {
      stopReason = 'settled';
      finish();
      return;
    }
    if (frame >= options.settleFrames) {
      stopReason = 'budget';
      finish();
      return;
    }
    schedule(tick);
  };

  for (const type of READER_EVENT_TYPES) {
    scroller.addEventListener(type, cancel, {passive: true});
  }
  record(CAPTURED_FRAME, capturedAnchorTop ?? null, 0, 0);
  tick();
}

/**
 * Ask ChatLayout to stop following, and say whether it did.
 *
 * Its follower unlocks when a scroll event shows an upward move, but that test
 * compares against the position of the last scroll event it has processed. A
 * reveal can leave a stale position behind — the container's own clamp onto the
 * bottom fires an event whose bookkeeping has not been delivered yet — so the
 * first nudge can be read as no movement at all. Each nudge makes the hook
 * record the position it just saw, which makes the next one a move it cannot
 * miss. The attribute it toggles is written synchronously, so this verifies
 * instead of assuming.
 *
 * Releasing it matters beyond the spring: while the attribute is set,
 * ChatLayout's own rule turns the container's scroll anchoring off, so a
 * follower left locked also loses the engine's own correction of late layout
 * passes.
 */
function releaseFollower(scroller: ScrollScroller, attempts: number): boolean {
  for (let attempt = 0; attempt < attempts; attempt += 1) {
    if (!scroller.hasAttribute(FOLLOWING_ATTRIBUTE)) {
      return true;
    }
    unlockScrollFollower(scroller);
  }
  return !scroller.hasAttribute(FOLLOWING_ATTRIBUTE);
}

function round(value: number): number {
  return Math.round(value * 10) / 10;
}
