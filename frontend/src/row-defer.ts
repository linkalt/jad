import {type CSSProperties, useCallback, useRef} from 'react';
import appConfig from './config.json';
import {type RowHeightStore, createRowHeightStore} from './row-heights';

// Off-screen transcript rows are skipped by the renderer. Without this, every
// keystroke in the composer costs a full layout of the scroll container's
// contents (the composer dock is sticky inside the same scroller), which made
// typing slow in long sessions — measured ~42ms of synchronous work per
// keystroke against a 36k-pixel scroll height, versus ~10ms with this enabled.
//
// The intrinsic size is the part that has to be right. A deferred row renders
// at that size until the engine lays it out, and the scroll *range* changes by
// the difference when it does — measured on a long session, 695–927px per row,
// one row every 200–1500ms of scrolling, in both directions as rows are
// re-created by a re-render. Firefox does not compensate that change (the
// content travels with the range), so it reads as the transcript jumping while
// you scroll. The engine's own `auto` keyword only remembers a size while the
// DOM element lives, which a re-render ends, so the height every row was last
// measured at is kept in `row-heights.ts` and fed back here.
let rowHeightStore: RowHeightStore | null = null;

let rowHeightObserver: ResizeObserver | null = null;

/** The width rows are rendering at, learned from the rows themselves. */
let latestRowWidthPx = 0;

const rowHeightIds = new WeakMap<Element, string>();

function rowHeights(): RowHeightStore {
  rowHeightStore ??= createRowHeightStore({
    storage: window.localStorage,
    storageKey: appConfig.row_height_storage_key,
    maxEntries: appConfig.row_height_max_entries,
    saveDelayMs: appConfig.row_height_save_delay_ms,
    widthBucketPx: appConfig.row_height_width_bucket_px,
  });
  return rowHeightStore;
}

/** Skip-this-row styling, at the height this row had the last time we saw it. */
export function deferredRowStyle(messageId: string | null): CSSProperties | undefined {
  if (!appConfig.defer_offscreen_transcript_rows) {
    return undefined;
  }
  const remembered =
    messageId === null ? null : rowHeights().heightFor(messageId, latestRowWidthPx);
  return {
    contentVisibility: 'auto',
    containIntrinsicSize: `auto ${remembered ?? appConfig.transcript_row_estimate_px}px`,
  };
}

/**
 * Measures a row as the engine lays it out, so the next render can skip it at
 * that height. One observer serves every row.
 */
export function useRowHeightRef(messageId: string | null): (node: HTMLElement | null) => void {
  const observedRef = useRef<Element | null>(null);
  return useCallback(
    (node: HTMLElement | null) => {
      if (observedRef.current !== null) {
        rowHeightObserver?.unobserve(observedRef.current);
        observedRef.current = null;
      }
      if (node === null || messageId === null || !appConfig.defer_offscreen_transcript_rows) {
        return;
      }
      rowHeightObserver ??= new ResizeObserver(entries => {
        for (const entry of entries) {
          const id = rowHeightIds.get(entry.target);
          if (id === undefined) {
            continue;
          }
          const width = entry.contentRect.width;
          if (width > 0) {
            latestRowWidthPx = width;
          }
          const height = entry.borderBoxSize?.[0]?.blockSize ?? entry.contentRect.height;
          // A row the engine has not laid out yet reports its own intrinsic
          // size, which is what we already assume for it; storing that would
          // turn the estimate into a "measurement".
          const assumed = rowHeights().heightFor(id, width) ?? appConfig.transcript_row_estimate_px;
          if (Math.abs(height - assumed) < 1) {
            continue;
          }
          rowHeights().remember(id, height, width);
        }
      });
      rowHeightIds.set(node, messageId);
      rowHeightObserver.observe(node);
      observedRef.current = node;
    },
    [messageId],
  );
}
