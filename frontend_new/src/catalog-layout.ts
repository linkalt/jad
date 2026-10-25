/**
 * @file catalog-layout.ts
 * @input A viewport size, the heights and margins the catalog dialog may take from
 *   it, and a pointer distance
 * @output The CSS cap of the dialog surface, the body height it opens at, the sizes
 *   each region may be dragged between, the width change a centred drag asks for, and
 *   the distances a corner's two-axis drag asks of each region
 * @position Pure sizing rules for the catalog dialog; no React and no DOM
 */

/** The share of the window the dialog surface may never exceed, as Dialog wants it. */
export function dialogMaxHeight(viewportPercent: number): string {
  return `${viewportPercent}dvh`;
}

/**
 * The height the body opens at: the configured number, or the share of the window
 * when that is smaller. The same cap `min(Npx, M dVH)` used to express as CSS, in
 * pixels, because a draggable region has to be given a size.
 */
export function bodyDefaultHeight(viewportPx: number, defaultHeightPx: number, viewportPercent: number): number {
  return Math.min(defaultHeightPx, Math.round((viewportPx * viewportPercent) / 100));
}

/**
 * The tallest the body may be dragged: the surface's own share of the window, less
 * the header and footer that sit around the body. Past that the surface's CSS would
 * clamp it and the region would keep a height nothing can paint — the dead zone the
 * width had.
 */
export function bodyHeightBound(viewportPx: number, maxHeightPercent: number, chromePx: number): number {
  return Math.max(1, Math.floor((viewportPx * maxHeightPercent) / 100 - chromePx));
}

/**
 * The widest the dialog surface can paint: the window less the margin the surface
 * keeps on both sides, and never more than the configured maximum.
 *
 * The surface is centred and clamped by its own CSS, so a region allowed past
 * this keeps a size it cannot show: the painted box stops at the clamp while the
 * stored size grows, and the next drag is spent on the invisible excess instead
 * of moving the edge.
 */
export function dialogWidthBound(viewportPx: number, marginPx: number, maxPx: number): number {
  return Math.max(1, Math.min(maxPx, viewportPx - marginPx));
}

/**
 * The size change a centred dialog needs for the edge under the pointer to keep
 * up: it grows from its middle, so a pointer that travels one pixel moves its
 * edge by half of the size it gains. The handle reports the pointer distance, and
 * the region is asked for twice it, on either axis.
 */
export function centredEdgeDelta(delta: number): number {
  return delta * 2;
}

/**
 * The smallest a region may be, given what the window can paint it at. A configured
 * floor larger than the bound is a floor nothing can show: the hook then reports a
 * minimum above its own maximum — measured, 720 against 608 at a 640 px window — and
 * the box only holds because the library's clamp lets the maximum win. A region wider
 * than its paint box is the one way this dialog could ever need to scroll, so the floor
 * follows the bound down rather than trusting that ordering to stay.
 */
export function regionFloor(configuredMinPx: number, boundPx: number): number {
  return Math.min(configuredMinPx, boundPx);
}

/** The four grips at the dialog's corners, named for the edges they sit between. */
export type DialogCorner = 'top-start' | 'top-end' | 'bottom-start' | 'bottom-end';

export const DIALOG_CORNERS: readonly DialogCorner[] = ['top-start', 'top-end', 'bottom-start', 'bottom-end'];

/**
 * What a corner's gesture asks of each region, per axis and before the centred
 * doubling: dragging a corner outwards grows the dialog, so the two start-side
 * corners grow on a negative inline distance and the two end-side corners on a
 * positive one, and the block axis follows the same rule top and bottom.
 *
 * Measured in the app: +40/+30 on the bottom-right corner gave +80 width and +60
 * height with both edges keeping pace, and the top-left corner did the same with
 * both signs flipped. The doubling is left to `centredEdgeDelta` so a corner and an
 * edge cannot disagree about it.
 */
export function cornerDelta(corner: DialogCorner, dx: number, dy: number): {inline: number; block: number} {
  return {
    // Written as a subtraction rather than a negation so a still pointer gives +0
    // and not −0: the same distance, but one that compares unequal to itself.
    inline: corner.endsWith('start') ? 0 - dx : dx,
    block: corner.startsWith('top') ? 0 - dy : dy,
  };
}
