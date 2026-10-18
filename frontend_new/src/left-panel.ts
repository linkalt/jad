/**
 * The left floating window's views.
 *
 * One window, several datasets: session search, the running configuration, and
 * the two activity panels that used to claim the app's right edge. Every control
 * in the left footer that shows a panel hands this same window its view, so two
 * panels never stack and nothing the left footer opens floats on the far edge of
 * the app.
 */
export type LeftPanelId = 'search' | 'settings' | 'notifications' | 'checkpoints' | 'panes';

/** Every view the window can show, in the order the footer offers them. */
export const LEFT_PANEL_IDS: readonly LeftPanelId[] = [
  'search',
  'settings',
  'notifications',
  'checkpoints',
  'panes',
];

/**
 * The rule every control that owns the window follows: clicking the item that
 * already holds it closes the window, any other item hands the window over.
 */
export function leftPanelAfterToggle(
  current: LeftPanelId | null,
  target: LeftPanelId,
): LeftPanelId | null {
  return current === target ? null : target;
}
