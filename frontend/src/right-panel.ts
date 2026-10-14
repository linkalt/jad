/** The right edge shows one panel at a time. */
export type RightPanelId = 'inspector' | 'notifications' | 'checkpoints' | 'readme';

/**
 * The rule shared by every control that owns the right edge: clicking the item
 * that already holds it closes the panel, any other click hands it over. The
 * side-nav items, the composer toggle and the notification item all route their
 * decision through here so a second click never re-opens what it just closed.
 */
export function selectionAfterPanelToggle(
  current: RightPanelId | null,
  target: RightPanelId,
): RightPanelId | null {
  return current === target ? null : target;
}
