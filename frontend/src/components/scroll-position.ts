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
