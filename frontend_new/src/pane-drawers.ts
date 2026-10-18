/**
 * Which panes are showing a composer drawer.
 *
 * A drawer belongs to the pane that opened it, so this is a set of view keys
 * rather than one open/closed flag: with a single flag, opening the model picker
 * or the usage details in pane 1 closed pane 2's, because the one value moved to
 * whichever pane was clicked last.
 */
export type PaneDrawerSet = ReadonlySet<string>;

/** No pane is showing this drawer. */
export const NO_PANE_DRAWERS: PaneDrawerSet = new Set<string>();

/** Whether this pane shows the drawer. */
export function hasPaneDrawer(keys: PaneDrawerSet, viewKey: string): boolean {
  return keys.has(viewKey);
}

/** Adds one pane, keeping every other pane's drawer open. */
export function openPaneDrawer(keys: PaneDrawerSet, viewKey: string): PaneDrawerSet {
  return keys.has(viewKey) ? keys : new Set([...keys, viewKey]);
}

/** Removes one pane's drawer and leaves the rest alone. */
export function closePaneDrawer(keys: PaneDrawerSet, viewKey: string): PaneDrawerSet {
  if (!keys.has(viewKey)) {
    return keys;
  }
  const next = new Set(keys);
  next.delete(viewKey);
  return next;
}

/** A pane's own control: opens its drawer, or closes it when already open. */
export function togglePaneDrawer(keys: PaneDrawerSet, viewKey: string): PaneDrawerSet {
  return keys.has(viewKey) ? closePaneDrawer(keys, viewKey) : openPaneDrawer(keys, viewKey);
}

/**
 * Whether pressing a pane's exclusive trigger opens its drawer.
 *
 * The state-context and conversation-actions triggers clear their pane's other
 * drawers before showing their own, so the decision has to be taken from the set
 * as it stood *before* that clear. Reading `togglePaneDrawer` afterwards would
 * always answer "open" — the clear had already removed the key — and a second
 * press could never collapse the drawer.
 */
export function paneDrawerPressOpens(keys: PaneDrawerSet, viewKey: string): boolean {
  return !keys.has(viewKey);
}
