import {useCallback, useEffect, useRef, useState} from 'react';

/**
 * The measured inline size of one element, in CSS pixels.
 *
 * A pane's width is decided by flex, not by the width it asked for: widening one
 * pane squeezes the others. Folding such a pane needs the width it actually
 * renders at, so this watches the element rather than reading configuration.
 */
export function useElementWidth(): [(node: HTMLElement | null) => void, number] {
  const [widthPx, setWidthPx] = useState(0);
  const observerRef = useRef<ResizeObserver | null>(null);

  const attach = useCallback((node: HTMLElement | null) => {
    observerRef.current?.disconnect();
    observerRef.current = null;
    if (node === null) {
      return;
    }
    const report = () => {
      const next = Math.round(node.getBoundingClientRect().width);
      // Only whole-pixel changes matter, and ignoring the rest keeps the
      // observer from re-rendering on sub-pixel layout noise.
      setWidthPx(current => (current === next ? current : next));
    };
    report();
    const observer = new ResizeObserver(report);
    observer.observe(node);
    observerRef.current = observer;
  }, []);

  useEffect(() => () => observerRef.current?.disconnect(), []);

  return [attach, widthPx];
}
