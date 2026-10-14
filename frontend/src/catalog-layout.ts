/**
 * @file catalog-layout.ts
 * @input A height in pixels and the share of the viewport it may take
 * @output The CSS height the catalog editor's list and editor use
 * @position Pure sizing rule for the catalog dialog; no React and no DOM
 */
export function cappedHeight(maxPx: number, viewportPercent: number): string {
  return `min(${maxPx}px, ${viewportPercent}dvh)`;
}
