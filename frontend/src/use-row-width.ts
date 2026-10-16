import {useCallback, useEffect, useRef, useState} from 'react';

/**
 * Measured width available to message rows. The conversation lane is clamped by
 * its container (the run inspector takes part of it, and narrow windows squeeze
 * it), so a configured lane size is not the width rows actually render at.
 */
export function useRowWidth(): [(node: HTMLElement | null) => void, number] {
  const [rowWidthPx, setRowWidthPx] = useState(0);
  const observerRef = useRef<ResizeObserver | null>(null);

  const measure = useCallback((node: HTMLElement) => {
    const computed = getComputedStyle(node);
    const inlinePadding =
      (Number.parseFloat(computed.paddingInlineStart) || 0) +
      (Number.parseFloat(computed.paddingInlineEnd) || 0);
    setRowWidthPx(Math.max(0, node.clientWidth - inlinePadding));
  }, []);

  const attach = useCallback(
    (node: HTMLElement | null) => {
      observerRef.current?.disconnect();
      observerRef.current = null;
      if (!node) {
        return;
      }
      measure(node);
      const observer = new ResizeObserver(() => measure(node));
      observer.observe(node);
      observerRef.current = observer;
    },
    [measure],
  );

  useEffect(() => () => observerRef.current?.disconnect(), []);

  return [attach, rowWidthPx];
}
