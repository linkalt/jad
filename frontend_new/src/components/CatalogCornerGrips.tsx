/**
 * @file CatalogCornerGrips.tsx
 * @input The width and height regions the catalog dialog is sized by
 * @output Four pointer-only grips, one per corner, each moving both regions in one gesture
 * @position The two-axis half of the catalog dialog's resize frame; the four single-axis
 *   edges stay with `ResizeHandle`
 *
 * `ResizeHandle` is one axis by construction — a direction, one coordinate and one
 * region — so a corner is ours to build. What is copied from its own handler is the
 * part that is easy to get wrong: capture the pointer on the way down, measure the
 * cumulative distance from where the gesture started, and treat `pointercancel` and a
 * lost capture as the end of the gesture without reporting a resize end. Without
 * capture a corner would only respond while the pointer stayed inside its own square,
 * which is not a resize.
 *
 * The grips are `aria-hidden` and stay out of the tab order on purpose. They are a
 * convenience for a pointer: every edge already gives a keyboard user a focusable
 * separator with arrows, Home and End, and those are the accessible path. Announcing
 * a control that no keyboard can drive would be worse than not announcing it.
 */
import {useRef, type PointerEvent as ReactPointerEvent} from 'react';
import * as stylex from '@stylexjs/stylex';

import {VStack} from '@astryxdesign/core';
import type {ResizableProps} from '@astryxdesign/core/Resizable';

import {DIALOG_CORNERS, centredEdgeDelta, cornerDelta, type DialogCorner} from '../catalog-layout';

/**
 * The band the library's own overlay handles occupy, taken from the library rather
 * than restated: its custom property when a theme sets one, otherwise the spacing step
 * that its stylesheet already uses as the 16px fallback. A local `16px` here would be
 * a second constant for the same edge, and the two could drift apart.
 */
const BAND = 'var(--resize-handle-hit-area, var(--spacing-4))';

const styles = stylex.create({
  grip: {
    position: 'absolute',
    zIndex: 3,
    width: BAND,
    height: BAND,
    // A touch that starts on a corner is a resize, never a scroll of the page behind.
    touchAction: 'none',
  },
  top: {top: 0},
  bottom: {bottom: 0},
  start: {insetInlineStart: 0},
  end: {insetInlineEnd: 0},
  /* The corners on one diagonal share a cursor: nwse runs from the top-left down. */
  nwse: {cursor: 'nwse-resize'},
  nesw: {cursor: 'nesw-resize'},
});

const PLACEMENT: Record<DialogCorner, stylex.StyleXStyles> = {
  'top-start': [styles.top, styles.start, styles.nwse],
  'top-end': [styles.top, styles.end, styles.nesw],
  'bottom-start': [styles.bottom, styles.start, styles.nesw],
  'bottom-end': [styles.bottom, styles.end, styles.nwse],
};

export interface CatalogCornerGripsProps {
  /** The width region, as `useResizable` returned it. */
  width: ResizableProps;
  /** The height region, likewise. */
  height: ResizableProps;
}

interface Drag {
  pointerId: number;
  startX: number;
  startY: number;
  /** Right-to-left splits the inline sign, the way the library's own handle does. */
  inline: 1 | -1;
  corner: DialogCorner;
}

export function CatalogCornerGrips({width, height}: CatalogCornerGripsProps) {
  // One gesture at a time, and a ref so a move does not re-render the dialog.
  const drag = useRef<Drag | null>(null);

  /**
   * The end of a gesture. `didComplete` separates the pointer being released from a
   * gesture being taken away: the regions are told differently, exactly as they are
   * for an edge, and the body cursor is released either way.
   */
  const end = (event: ReactPointerEvent<HTMLElement>, didComplete: boolean): void => {
    const current = drag.current;
    if (current === null || current.pointerId !== event.pointerId) return;
    drag.current = null;
    document.body.style.cursor = '';
    document.body.style.userSelect = '';
    if (didComplete) {
      width._onResizeEnd();
      height._onResizeEnd();
      return;
    }
    width._onResizeCancel?.();
    height._onResizeCancel?.();
  };

  const grips = (corner: DialogCorner) => ({
    onPointerDown: (event: ReactPointerEvent<HTMLElement>): void => {
      if (drag.current !== null) return;
      event.preventDefault();
      event.stopPropagation();
      // The pointer leaves the square on the first move, so the rest of the gesture
      // has to be delivered to this element rather than hit-tested under the cursor.
      event.currentTarget.setPointerCapture?.(event.pointerId);
      const computed = getComputedStyle(event.currentTarget);
      drag.current = {
        pointerId: event.pointerId,
        startX: event.clientX,
        startY: event.clientY,
        inline: computed.direction === 'rtl' ? -1 : 1,
        corner,
      };
      width._onResizeStart();
      height._onResizeStart();
      // The cursor travels off the grip at once, so it is held on the body for the
      // duration — read from the grip, so the stylesheet stays the only place that
      // names one.
      document.body.style.cursor = computed.cursor;
      document.body.style.userSelect = 'none';
    },
    onPointerMove: (event: ReactPointerEvent<HTMLElement>): void => {
      const current = drag.current;
      if (current === null || current.pointerId !== event.pointerId) return;
      const delta = cornerDelta(current.corner, (event.clientX - current.startX) * current.inline, event.clientY - current.startY);
      width._onResizeMove(centredEdgeDelta(delta.inline));
      height._onResizeMove(centredEdgeDelta(delta.block));
    },
    onPointerUp: (event: ReactPointerEvent<HTMLElement>): void => end(event, true),
    onPointerCancel: (event: ReactPointerEvent<HTMLElement>): void => end(event, false),
    // Fires after every release too, by which point there is no gesture left to end.
    onLostPointerCapture: (event: ReactPointerEvent<HTMLElement>): void => end(event, false),
  });

  return (
    <>
      {DIALOG_CORNERS.map(corner => (
        <VStack key={corner} xstyle={[styles.grip, PLACEMENT[corner]]} aria-hidden data-corner={corner} {...grips(corner)} />
      ))}
    </>
  );
}
