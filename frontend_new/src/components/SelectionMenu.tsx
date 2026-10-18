import {useCallback, useEffect, useRef} from 'react';
import {Button, Card, useLayer} from '@astryxdesign/core';

/** Where the selection was, and which side of it the menu opened on. */
export type QuoteAnchor = {
  x: number;
  y: number;
  /** True when the menu sits above the selection, false when it had to open below. */
  isAbove: boolean;
};

type SelectionMenuProps = {
  anchor: QuoteAnchor | null;
  onQuote: () => void;
  /** Called when the menu closed itself — an outside click, Escape, a scroll. */
  onDismiss: () => void;
};

/**
 * The one action offered for a text selection in the conversation.
 *
 * A fixed-mode layer rather than a Popover: there is no trigger element to
 * anchor to — the anchor is a text range — and the layer lives in the top
 * layer, where the transcript's own scroll containers cannot clip it.
 *
 * The button cancels mousedown and stops it: a press that moved focus or
 * reached the app's own listeners would collapse the selection the menu is
 * quoting, and the quote would arrive empty.
 */
export function SelectionMenu({anchor, onQuote, onDismiss}: SelectionMenuProps) {
  const hide = useCallback(() => onDismiss(), [onDismiss]);
  const layer = useLayer({mode: 'fixed', lightDismiss: true, onHide: hide});
  // The layer has to stay mounted to be shown again, so the last anchor keeps
  // the menu in place while it is hidden.
  const lastAnchor = useRef<QuoteAnchor>({x: 0, y: 0, isAbove: true});
  if (anchor !== null) {
    lastAnchor.current = anchor;
  }
  const {x, y, isAbove} = lastAnchor.current;

  useEffect(() => {
    if (anchor === null) {
      layer.hide();
      return;
    }
    layer.show();
  }, [anchor, layer]);

  return layer.render(
    anchor === null ? null : (
      <Card elevation="med" padding={1}>
        <Button
          label="Quote in composer"
          size="sm"
          variant="ghost"
          onMouseDown={event => {
            event.preventDefault();
            event.stopPropagation();
          }}
          onClick={onQuote}
        />
      </Card>
    ),
    // The menu opens above the selection's first line, where a selection menu
    // is expected; `translateY(-100%)` is what makes the anchor its bottom edge.
    {x, y, style: isAbove ? {transform: 'translateY(-100%)'} : undefined},
  );
}
