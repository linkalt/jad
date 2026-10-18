/**
 * Moving a pane divider.
 *
 * A divider belongs to the boundary on the right of one pane: dragging it
 * changes that pane, and the panes that give it room. Two rules keep it honest:
 *
 * - **Never across the dragged pane.** A drag only spends the room on the side it
 *   moves toward. Growing takes from the panes on the right, nearest first, down
 *   to the fold floor, and stops when they are all there — it does not reach past
 *   itself to the left, which read as "drag right and pane 1 shrinks".
 * - **Left only when the dragged pane cannot shrink.** A pane already folded has
 *   nothing to give, so its divider takes the room from the panes on its left
 *   (nearest first, down to the floor) and hands it to the right. Otherwise that
 *   divider is a dead control, which was reported for two clamped panes.
 *
 * Dragging the other way hands the freed width back the same way, filling a
 * folded neighbour up to the usable minimum before spilling into the next, so a
 * drag never parks a pane at a stub width it could have avoided. The sum of the
 * row never changes: a drag can neither invent room nor lose it.
 */
export function movePaneBoundary(
  widths: readonly number[],
  index: number,
  delta: number,
  minSizePx: number,
  foldWidthPx: number,
): number[] {
  // The last pane has no divider, and an unknown index moves nothing.
  if (index < 0 || index >= widths.length - 1) {
    return [...widths];
  }
  const next = [...widths];
  // Nearest first on the right: both directions hand room to these panes.

  if (delta > 0) {
    let remaining = delta;
    /**
     * Only the panes to the right give, and the drag stops at their floor.
     *
     * Taking from the left once the right side was folded moved the *other*
     * boundary of the dragged pane: with three panes, dragging pane 2's divider
     * far to the right shrank pane 1, so "resize to the right" read as "resize to
     * the left". A divider now only spends the room it actually has on its own
     * side; if every pane there is at the floor, it has none and the drag stops.
     */
    for (let j = index + 1; j < next.length && remaining > 0; j++) {
      // A pane gives down to the floor and no further.
      const take = Math.min(remaining, Math.max(0, next[j] - foldWidthPx));
      next[j] -= take;
      remaining -= take;
    }
    next[index] += delta - remaining;
    return next;
  }

  if (delta < 0) {
    /**
     * Shrinking goes all the way down to the fold floor.
     *
     * It used to stop at the usable minimum, which made the same pane behave two
     * different ways: growing its neighbour could fold it to the floor, but
     * dragging its own divider could never take it below the minimum — the
     * divider simply stopped under the pointer ("resize to the left sticks, but
     * resizing all the way right does clamp it"). A drag now folds the pane it
     * moves, so both directions reach the same widths, and dragging back out
     * restores it.
     */
    const floor = foldWidthPx;
    let wanted = -delta;
    const freed = Math.min(wanted, Math.max(0, next[index] - floor));
    next[index] -= freed;
    wanted -= freed;
    /**
     * What this pane cannot give, the panes on its left give.
     *
     * A pane already at the floor used to make its divider a dead control: with
     * the row clamped to `[wide, 56, 56]`, dragging the divider between the two
     * stubs to the left did nothing at all, while the divider before it worked.
     * The boundary can still move — the stub keeps its floor and the pane on its
     * left gives the room — so the drag follows the pointer instead of jamming.
     * Nearest first, down to the floor, and only as far as the pointer asks.
     */
    let borrowed = 0;
    if (wanted > 0) {
      for (let j = index - 1; j >= 0 && wanted > 0; j--) {
        const take = Math.min(wanted, Math.max(0, next[j] - floor));
        next[j] -= take;
        wanted -= take;
        borrowed += take;
      }
    }
    let remaining = freed + borrowed;
    // Right-hand side, nearest first: a folded pane is filled to the usable
    // minimum before it takes the rest; one that is already usable takes it all.
    for (let j = index + 1; j < next.length && remaining > 0; j++) {
      const target = next[j] < minSizePx ? Math.min(minSizePx, next[j] + remaining) : next[j] + remaining;
      const give = Math.min(remaining, target - next[j]);
      next[j] += give;
      remaining -= give;
    }
    if (remaining > 0) {
      // Nowhere to put it (the whole row is at the floor): leave it as it was.
      next[index] += remaining;
    }
    return next;
  }

  return next;
}
