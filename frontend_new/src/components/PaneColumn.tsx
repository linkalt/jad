import {HStack, VisuallyHidden} from '@astryxdesign/core';
import {ResizeHandle, useResizable} from '@astryxdesign/core/Resizable';
import {pixel} from '@astryxdesign/core/Resizable/utils';
import {type ReactNode, useEffect, useRef, useState} from 'react';

import {isPaneFolded} from '../pane-fold';
import {useElementWidth} from '../use-element-width';

export type PaneColumnProps = {
  /** Where this pane's chosen width persists, one key per pane slot. */
  storageId: string;
  /** The width the pane starts at, before anyone drags it. */
  defaultSizePx: number;
  minSizePx: number;
  maxSizePx: number;
  /**
   * The last pane takes whatever the others leave instead of a width of its own,
   * so the columns always fill the area no matter how the others moved.
   */
  isFlexible: boolean;
  /**
   * The floor every pane keeps: widening one pane squeezes its neighbours, and
   * this is what stops a neighbour from shrinking out of existence.
   */
  foldWidthPx: number;
  /**
   * The width the fit rule allocated to this pane, when the row has to share what
   * it has. Absent while the panes fit, and the pane then keeps its own size.
   */
  widthPx?: number;
  /** Reports the width this pane holds, so the row can decide what fits. */
  onSizeChange?: (widthPx: number) => void;
  /** Hands the pane's own resize setter up, so the row can commit a drag. */
  onRegisterResize?: (resize: (widthPx: number) => void) => void;
  /** A drag on this pane's divider: the row moves the boundary, not one pane. */
  onResizeStart?: () => void;
  onResizeDelta?: (delta: number) => void;
  onResizeEnd?: () => void;
  /** What this pane shows instead of its conversation when it is too narrow. */
  folded: ReactNode;
  /** Screen-reader name for this pane's divider. */
  handleLabel: string;
  children: ReactNode;
};

/**
 * One pane of a multi-pane arrangement: a width of its own and a divider on its
 * right edge that moves it.
 *
 * The divider belongs to the pane on its left, so dragging it moves that pane
 * and the flexible last pane absorbs the difference. `useResizable` is a hook,
 * so a pane needs its own component to hold its own size — this is that
 * component, and the pane itself stays the child it always was.
 *
 * Sized panes shrink proportionally once the row runs out of room (they keep
 * `flex-shrink: 1`, floored at `foldWidthPx`) rather than holding their width
 * and pushing the flexible pane to nothing: nothing leaves the screen, and
 * whatever falls below `minSizePx` renders `folded` instead of a conversation
 * nobody could read or type into.
 */
export function PaneColumn({
  storageId,
  defaultSizePx,
  minSizePx,
  maxSizePx,
  isFlexible,
  foldWidthPx,
  widthPx,
  onSizeChange,
  onRegisterResize,
  onResizeStart,
  onResizeDelta,
  onResizeEnd,
  folded,
  handleLabel,
  children,
}: PaneColumnProps) {
  const region = useResizable({
    defaultSize: pixel(defaultSizePx),
    // The floor, not the usable minimum: a folded pane has to be able to store
    // its stub width, and a drag may fold the pane it moves — the same way
    // growing its neighbour can (pane-resize.ts).
    minSize: pixel(foldWidthPx),
    maxSize: pixel(maxSizePx),
    autoSaveId: storageId,
  });
  const [attach, widthPxMeasured] = useElementWidth();
  const isFolded = isPaneFolded(widthPxMeasured, minSizePx);
  /**
   * A fresh handle after every gesture.
   *
   * `ResizeHandle` keeps its own drag in a ref and clears it only from the events
   * it sees. A gesture it missed the release for leaves that ref set, and every
   * later press on the divider then bails out silently: the divider looks dead
   * until the page is reloaded. Remounting the handle once a gesture is over
   * gives it a clean ref without touching the pane's own size.
   */
  const [handleGeneration, setHandleGeneration] = useState(0);
  // The row decides what fits from the sizes the panes hold, not from the widths
  // they render at: a squeezed pane reports the width it would have again as
  // soon as it gets its room back.
  useEffect(() => {
    onSizeChange?.(region.size);
  }, [onSizeChange, region.size]);
  useEffect(() => {
    onRegisterResize?.(region.resize);
  }, [onRegisterResize, region.resize]);
  // The gesture outlives the render that started it: a drag re-renders the row on
  // every frame, so its listeners call through the latest props instead of the
  // ones they were installed with — a stale `onResizeEnd` would commit the widths
  // the drag started from.
  const callbacksRef = useRef({onResizeStart, onResizeDelta, onResizeEnd});
  callbacksRef.current = {onResizeStart, onResizeDelta, onResizeEnd};
  const regionRef = useRef(region);
  regionRef.current = region;
  /** Where the pointer went down: the gesture's own basis. */
  const pointerDownXRef = useRef<number | null>(null);
  /** Whether a button is held right now: a keyboard resize holds none. */
  const isPointerDownRef = useRef(false);
  useEffect(() => {
    const trackDown = (event: PointerEvent) => {
      pointerDownXRef.current = event.clientX;
      isPointerDownRef.current = true;
    };
    const trackUp = () => {
      isPointerDownRef.current = false;
    };
    window.addEventListener('pointerdown', trackDown, true);
    window.addEventListener('pointerup', trackUp, true);
    window.addEventListener('pointercancel', trackUp, true);
    return () => {
      window.removeEventListener('pointerdown', trackDown, true);
      window.removeEventListener('pointerup', trackUp, true);
      window.removeEventListener('pointercancel', trackUp, true);
    };
  }, []);
  /**
   * The gesture, installed on `window` in the **capture** phase.
   *
   * Capture is not a preference. `ResizeHandle` calls `stopPropagation()` on the
   * pointer, so a bubbling listener on `window` never sees the release: the drag
   * stayed armed after the button was let go and the panes went on following the
   * mouse — "the mouse does not want to release". The capture phase runs before
   * anything can stop the event.
   *
   * `follow` also heals itself: a move that arrives with no button held means the
   * release was lost altogether (a dropped pointer, a release outside the
   * window), and ends the gesture instead of dragging on.
   */
  const endGestureRef = useRef<(() => void) | null>(null);
  // A pane unmounted mid-drag must not leave the row dragging forever.
  useEffect(() => () => {
    endGestureRef.current?.();
  }, []);
  const beginGesture = (startX: number): void => {
    // One gesture at a time: a second press cannot inherit the first one's state.
    endGestureRef.current?.();
    const follow = (event: PointerEvent) => {
      if (event.buttons === 0) {
        endGestureRef.current?.();
        return;
      }
      callbacksRef.current.onResizeDelta?.(event.clientX - startX);
    };
    const finish = () => {
      if (endGestureRef.current !== finish) {
        return;
      }
      endGestureRef.current = null;
      window.removeEventListener('pointermove', follow, true);
      window.removeEventListener('pointerup', finish, true);
      window.removeEventListener('pointercancel', finish, true);
      window.removeEventListener('blur', finish, true);
      window.removeEventListener('lostpointercapture', finish, true);
      // Release whatever Astryx's hook froze for the gesture, in case the handle
      // itself never saw the end.
      regionRef.current.props._onResizeEnd();
      // The handle may have missed the release; the next press gets a clean one.
      setHandleGeneration(generation => generation + 1);
      callbacksRef.current.onResizeEnd?.();
    };
    endGestureRef.current = finish;
    window.addEventListener('pointermove', follow, true);
    // Capture phase, all of them: the handle stops the pointer's propagation.
    window.addEventListener('pointerup', finish, true);
    window.addEventListener('pointercancel', finish, true);
    window.addEventListener('blur', finish, true);
    window.addEventListener('lostpointercapture', finish, true);
  };
  const resizeProps = {
    ...region.props,
    _onResizeStart: () => {
      region.props._onResizeStart();
      // A pointer press starts the gesture from the position this window saw, so
      // a press with no move before it cannot drag from a stale coordinate. A
      // keyboard resize holds no pointer: it starts and ends inside one keydown.
      if (isPointerDownRef.current && pointerDownXRef.current !== null) {
        beginGesture(pointerDownXRef.current);
      }
      callbacksRef.current.onResizeStart?.();
    },
    _onResizeMove: (delta: number) => {
      // The pane's own size follows the gesture so it persists; the row is moved
      // by the window listeners above.
      region.props._onResizeMove(delta);
      // Keyboard: no pointer gesture to follow, so the row is told here. Without
      // this a keypress is masked by the width the last drag committed and the
      // divider looks dead to the keyboard.
      if (endGestureRef.current === null) {
        callbacksRef.current.onResizeDelta?.(delta);
      }
    },
    _onResizeEnd: () => {
      region.props._onResizeEnd();
      // A keyboard resize has no pointer gesture: it starts and ends in the same
      // tick, so the row commits here instead.
      if (endGestureRef.current === null) {
        callbacksRef.current.onResizeEnd?.();
      }
    },
  };

  return (
    <HStack
      ref={attach}
      align="stretch"
      gap={0}
      height="100%"
      width={widthPx ?? (isFlexible ? undefined : region.size)}
      maxWidth={isFlexible ? undefined : '100%'}
      style={isFlexible
        ? {flex: '1 1 auto', minWidth: foldWidthPx}
        : {flex: '0 0 auto', minWidth: foldWidthPx}}
      data-pane-column={isFlexible ? 'flexible' : 'sized'}
      data-pane-folded={isFolded ? 'true' : 'false'}
    >
      {isFolded ? folded : children}
      {isFlexible ? null : (
        <ResizeHandle
          key={`pane-divider-${handleGeneration}`}
          direction="horizontal"
          hasDivider
          isAlwaysVisible={false}
          // The grab zone is wider than the line and sits on the pill's side.
          // It has to sit on the *next* pane's side: one pane's transcript
          // scrollbar is flush against its right edge, and a grab zone over it
          // would take the scrollbar's clicks and hide it under the pill.
          pillPlacement="end"
          label={handleLabel}
          resizable={resizeProps}
        >
          <VisuallyHidden>{handleLabel}</VisuallyHidden>
        </ResizeHandle>
      )}
    </HStack>
  );
}
