/**
 * When a pane is too narrow to use, it folds instead of vanishing.
 *
 * Panes are flex columns, so widening one squeezes its neighbours. Without a
 * floor the last pane shrinks to a sliver — measured at 1px wide and off-screen
 * once three panes held the row — and focusing it left an 8px composer nobody
 * could see. A pane now keeps at least `conversation_pane_fold_width` and, below
 * `conversation_pane_min_size`, renders a stub that says which pane it is and
 * whether it is running, with no composer to type into by accident.
 */
import appConfig from './config.json' with {type: 'json'};

/** Whether this measured width is too narrow to show a conversation. */
export function isPaneFolded(widthPx: number, minSizePx: number = appConfig.conversation_pane_min_size): boolean {
  // A width of 0 is "not measured yet", not "folded": the first paint must not
  // flash a stub before the pane has said how wide it is.
  return widthPx > 0 && widthPx < minSizePx;
}

/** What a folded pane says instead of a conversation. */
export function foldedPaneTitle(number: number, label: string): string {
  return `Pane ${number}: ${label}`;
}

/**
 * The widths the panes actually render at.
 *
 * Panes keep the width the user dragged them to only while all of them fit. When
 * they do not — four panes at their minimum need more room than the row has —
 * simply shrinking them proportionally folds most of them, which reads as "the
 * app broke" rather than "these do not fit". So the room is spent in a fixed
 * order: every pane's fold width first, then enough on each pane (largest stored
 * width first) to reach the usable minimum, then what is left grows them again
 * in proportion to what they asked for.
 *
 * The result never gives a pane less than the fold width, never exceeds the
 * available width, and fits as many usable panes as the row can hold.
 */
export function fitPaneWidths(
  stored: readonly number[],
  available: number,
  minSizePx: number,
  foldWidthPx: number,
  priorityIndex?: number,
): number[] {
  if (stored.length === 0) {
    return [];
  }
  if (available <= 0) {
    // Not measured yet: the stored widths are the best answer there is.
    return [...stored];
  }
  const floorTotal = stored.length * foldWidthPx;
  /**
   * The last pane takes whatever the others leave.
   *
   * Its own width is a starting point, not a cap: the row is a flex row whose
   * last column grows to fill the area, so a plan that stopped at the stored
   * widths would disagree with what is on screen — and a divider drag, which is
   * computed from the plan, would stop at a maximum nowhere near the edge. In a
   * wide window that showed up as "the first pane cannot be resized any
   * further", while a narrow window (where the plan and the screen agree) worked.
   */
  const fillRow = (widths: number[]): number[] => {
    if (widths.length === 0) {
      return widths;
    }
    const last = widths.length - 1;
    const remainder = available - widths.reduce((sum, width) => sum + width, 0);
    if (remainder === 0) {
      return widths;
    }
    const filled = [...widths];
    filled[last] = Math.max(foldWidthPx, filled[last] + remainder);
    return filled;
  };
  if (floorTotal >= available) {
    // Even folded panes do not fit; each keeps its floor and the row scrolls.
    return fillRow(stored.map(() => foldWidthPx));
  }
  if (stored.reduce((total, width) => total + width, 0) <= available) {
    /**
     * There is room for every pane, so none of them should stay folded.
     *
     * A stored width below the usable minimum is a leftover from a tighter row —
     * or from a drag that cascaded — not a request to keep a stub. It used to be
     * honoured: after closing two of four panes the row came back as a 56px stub
     * beside a 1284px conversation, with nothing to drag the stub back to a
     * readable width except a divider that was already at its limit.
     */
    const floorPerPane = Math.max(minSizePx, foldWidthPx);
    if (stored.length * floorPerPane <= available && stored.some(width => width < minSizePx)) {
      const spare = available - stored.length * floorPerPane;
      const claim = stored.reduce((total, width) => total + Math.max(0, width - floorPerPane), 0);
      const lifted = stored.map(width => floorPerPane + (claim > 0
        ? Math.floor((spare * Math.max(0, width - floorPerPane)) / claim)
        : 0));
      return fillRow(lifted);
    }
    return fillRow([...stored]);
  }  const widths = stored.map(() => foldWidthPx);
  let budget = available - floorTotal;
  /** The lift a pane needs to be usable at all. */
  const liftNeeded = (index: number): number =>
    Math.min(Math.max(0, minSizePx - foldWidthPx), Math.max(0, stored[index] - foldWidthPx));
  /** Spend on one pane, up to the room the budget allows. */
  const liftPane = (index: number, upTo: number): void => {
    const granted = Math.min(upTo, budget);
    widths[index] += granted;
    budget -= granted;
  };
  // The focused pane first: a pane that cannot show its conversation must not be
  // the one holding the composer, or there is nowhere to type.
  if (priorityIndex !== undefined && priorityIndex >= 0 && priorityIndex < stored.length) {
    liftPane(priorityIndex, liftNeeded(priorityIndex));
  }
  // Then largest first — but only a pane that can reach the usable minimum gets
  // anything: a pane stuck between the floor and the minimum would be a stub
  // taking up as much room as a conversation.
  const order = stored
    .map((width, index) => ({width, index}))
    .sort((left, right) => right.width - left.width)
    .map(entry => entry.index);
  for (const index of order) {
    if (budget <= 0) {
      break;
    }
    if (index === priorityIndex) {
      continue;
    }
    const needed = liftNeeded(index);
    if (needed <= budget) {
      liftPane(index, needed);
    }
  }
  // What is left over goes back in proportion to what each pane asked for,
  // among the panes that reached the usable minimum.
  const usable = widths
    .map((width, index) => ({width, index}))
    .filter(entry => entry.width >= minSizePx);
  const claim = usable.reduce((total, entry) => total + Math.max(0, stored[entry.index] - minSizePx), 0);
  if (budget > 0 && claim > 0) {
    for (const entry of usable) {
      const share = Math.max(0, stored[entry.index] - minSizePx) / claim;
      widths[entry.index] += Math.floor(budget * share);
    }
  }
  return fillRow(widths);
}
