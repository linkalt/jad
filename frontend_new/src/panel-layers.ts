/**
 * Stacking order for the floating surfaces and the chrome they cover.
 *
 * Two different panels share one shell, and they need opposite orders:
 *
 * - A pane's own panel (run inspector, prompt library) floats *inside* that
 *   pane, so it sits **under** the pane's title strip. The strip's label, its
 *   running spinner and its close button stay visible and clickable while the
 *   panel is open, instead of the panel painting over them.
 * - The app's left window (session search, run settings, notifications,
 *   checkpoints, panes) is one window for the whole app, so it sits **above**
 *   every pane — including their title strips. A pane's header must never paint
 *   over it.
 *
 * Keeping the three numbers here, in one ascending list, is what makes that
 * order checkable: `panel-layers.test.mjs` asserts it.
 */

/** A panel anchored to a pane's own edge, under that pane's title strip. */
export const PANE_PANEL_Z_INDEX = 3;

/** The title strip of a pane: above its own panel, below the app's left window. */
export const PANE_HEADER_Z_INDEX = 4;

/** The app's left floating window: above every pane and every pane header. */
export const APP_WINDOW_Z_INDEX = 5;

/** The custom property the pane sets for a panel pinned to the window edge. */
export const PANE_PANEL_TOP_VAR = '--pane-panel-top';

/**
 * Where a window-edge panel starts, measured from the viewport's top.
 *
 * A panel pinned to the window edge is `position: fixed`, so it does not sit
 * inside the pane, and the pane's full-width title strip (above it in the stack,
 * see PANE_HEADER_Z_INDEX) would paint over its top: heading, close button and
 * the first content. It therefore starts at the strip's bottom edge, the same
 * relationship an in-pane panel has. Without a strip, or without a measurement
 * yet, it starts at the top.
 */
export function edgePanelTop(isEdgeFixed: boolean, headerBottom: number | null): string {
  if (!isEdgeFixed || headerBottom === null || !Number.isFinite(headerBottom) || headerBottom <= 0) {
    return '0px';
  }
  return `${Math.ceil(headerBottom)}px`;
}
