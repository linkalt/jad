/**
 * How many conversations share the screen, said in one place.
 *
 * The left footer shows the short form beside its icon and the panel header
 * spells the same numbers out, so both read from here instead of formatting them
 * twice. "Active" means the panes on screen — `conversation_panes_max` is the
 * ceiling — and a pane whose run is in flight is counted separately rather than
 * being called active.
 */

/** The same count as a sentence, for the tooltip and the accessible name. */
export function paneCountLabel(open: number, limit: number): string {
  return `Panes: ${open} of ${limit} open`;
}

/** The panel's subtitle: the count, then how many of those panes are running. */
export function paneCountSummary(open: number, limit: number, running: number): string {
  const panes = `${open} of ${limit} ${limit === 1 ? 'pane' : 'panes'}`;
  return running > 0 ? `${panes} · ${running} running` : panes;
}
