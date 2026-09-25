export type ScrollPositionAnchor = Pick<
  HTMLElement,
  'isConnected' | 'getBoundingClientRect'
>;

export interface ScrollPositionSnapshot {
  scroller: Pick<HTMLElement, 'isConnected' | 'scrollTop'>;
  scrollTop: number;
  anchor: ScrollPositionAnchor | null;
  anchorTop: number | null;
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
