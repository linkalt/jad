import {type PinnedView, parseCollapsedGroups} from './session-views';

/** Storage is best effort: a blocked or full store must not break the list. */
export function readStoredViews(key: string): unknown {
  try {
    const raw = window.localStorage.getItem(key);
    return raw === null ? undefined : JSON.parse(raw);
  } catch {
    return undefined;
  }
}

export function writeStoredViews(key: string, value: {views: PinnedView[]; activeViewKey: string}): void {
  try {
    window.localStorage.setItem(key, JSON.stringify(value));
  } catch {
    // Ignored: the rows live for this page even when they cannot be persisted.
  }
}

/** The groups the user collapsed; best effort, like the pinned rows. */
export function readCollapsedGroups(key: string): string[] {
  return parseCollapsedGroups(readStoredViews(key));
}

/**
 * The panes that shared the screen, and the one that had focus. Read through the
 * same best-effort helper: an unreadable store opens the single-pane default.
 */
export function readStoredPanes(key: string): unknown {
  return readStoredViews(key);
}

export function writeStoredPanes(
  key: string,
  value: {keys: readonly string[]; focusedKey: string},
): void {
  try {
    window.localStorage.setItem(key, JSON.stringify(value));
  } catch {
    // Ignored: the arrangement lives for this page even when it cannot be persisted.
  }
}

export function writeCollapsedGroups(key: string, value: readonly string[]): void {
  try {
    window.localStorage.setItem(key, JSON.stringify(value));
  } catch {
    // Ignored: the groups keep their state for this page.
  }
}
