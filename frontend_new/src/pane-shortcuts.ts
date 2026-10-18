/** What a pane key combination asks the pane group to do. */
export type PaneShortcut = 'next' | 'previous' | 'close';

/** The parts of a keydown this module reads. */
export type PaneShortcutEvent = {
  key: string;
  altKey: boolean;
  ctrlKey: boolean;
  metaKey: boolean;
  shiftKey: boolean;
};

/**
 * The keyboard route through the panes.
 *
 * Ctrl/Cmd+Alt+ArrowLeft and ArrowRight move focus along the arrangement, and
 * Ctrl/Cmd+Alt+W closes the focused pane. The modifier pair is deliberate: a
 * bare Alt+Arrow moves the caret by word inside the composer, so claiming it
 * would break typing in the very field these shortcuts have to work from. Shift
 * is left unbound — these three are the only keys the pane group takes.
 */
export function paneShortcutFor(event: PaneShortcutEvent): PaneShortcut | null {
  if (!event.altKey || !(event.ctrlKey || event.metaKey) || event.shiftKey) {
    return null;
  }
  if (event.key === 'ArrowLeft') {
    return 'previous';
  }
  if (event.key === 'ArrowRight') {
    return 'next';
  }
  if (event.key.toLowerCase() === 'w') {
    return 'close';
  }
  return null;
}

/**
 * The pane one step away from the focused one, wrapping at both ends.
 *
 * Wrapping keeps a hand on two keys: from the leftmost pane, "previous" reaches
 * the rightmost one instead of doing nothing.
 */
export function paneAfterStep(
  keys: readonly string[],
  focusedKey: string,
  step: number,
): string {
  if (keys.length === 0) {
    return focusedKey;
  }
  const index = keys.indexOf(focusedKey);
  if (index < 0) {
    return keys[0];
  }
  const count = keys.length;
  return keys[(((index + step) % count) + count) % count];
}
