export interface ScrollPositionSnapshot {
  scroller: Pick<HTMLElement, 'isConnected' | 'scrollTop'>;
  scrollTop: number;
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

export function captureScrollPosition(
  scroller: ScrollPositionSnapshot['scroller'] | null,
): ScrollPositionSnapshot | null {
  return scroller === null ? null : {scroller, scrollTop: scroller.scrollTop};
}

/** Restore the viewport after content expands, overriding browser scroll anchoring. */
export function restoreScrollPosition(snapshot: ScrollPositionSnapshot | null): void {
  if (snapshot !== null && snapshot.scroller.isConnected) {
    snapshot.scroller.scrollTop = snapshot.scrollTop;
  }
}
