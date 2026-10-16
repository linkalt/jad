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

export function writeCollapsedGroups(key: string, value: readonly string[]): void {
  try {
    window.localStorage.setItem(key, JSON.stringify(value));
  } catch {
    // Ignored: the groups keep their state for this page.
  }
}
