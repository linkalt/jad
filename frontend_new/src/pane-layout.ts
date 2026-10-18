/**
 * The conversation panes: which views share the screen, in order, and which one
 * has focus.
 *
 * A pane is a view (`session` or `session\u0000profile`), so the pane set is a
 * list of view keys. Everything here is pure: the caller owns the state, and a
 * configured maximum decides how many panes may be open. Focus is part of the
 * model because the destructive conversation actions, the composer drawers and
 * the model drawer belong to exactly one pane at a time.
 */

export type PaneState = {
  /** The views on screen, left to right. Never empty. */
  keys: readonly string[];
  /**
   * The pane the user is working in. Always one of `keys`; falls back to the
   * first pane when a restored focus names a view that is no longer open.
   */
  focusedKey: string;
};

/** How many panes may be open: a configured positive integer, else a single pane. */
export function paneCapacity(configured: number | undefined): number {
  if (configured === undefined || !Number.isFinite(configured)) {
    return 1;
  }
  const whole = Math.floor(configured);
  return whole > 0 ? whole : 1;
}

/** The single-pane arrangement: today's behaviour, used before any pane is open. */
export function singlePane(viewKey: string): PaneState {
  return {keys: [viewKey], focusedKey: viewKey};
}

/**
 * Drop duplicates and cap the list from the left, so the panes nearest the
 * focused one survive a smaller configured maximum.
 */
export function normalizePaneKeys(keys: readonly unknown[], max: number): string[] {
  const capacity = paneCapacity(max);
  const seen = new Set<string>();
  const kept: string[] = [];
  for (const key of keys) {
    if (typeof key !== 'string' || key.length === 0 || seen.has(key)) {
      continue;
    }
    seen.add(key);
    kept.push(key);
  }
  return kept.slice(0, capacity);
}

/**
 * The arrangement restored from storage, or the fallback single pane.
 *
 * Storage is user-writable and survives code changes, so every field is
 * validated: an arrangement that cannot be trusted becomes the fallback.
 */
export function parsePaneState(raw: unknown, fallbackKey: string, max: number): PaneState {
  const fallback = singlePane(fallbackKey);
  if (typeof raw !== 'object' || raw === null) {
    return fallback;
  }
  const record = raw as {keys?: unknown; focusedKey?: unknown};
  if (!Array.isArray(record.keys)) {
    return fallback;
  }
  const keys = normalizePaneKeys(record.keys, max);
  if (keys.length === 0) {
    return fallback;
  }
  const focused = typeof record.focusedKey === 'string' ? record.focusedKey : '';
  return {
    keys,
    focusedKey: keys.includes(focused) ? focused : keys[0]!,
  };
}

/** Whether another pane may be opened at this maximum. */
export function canOpenPane(state: PaneState, max: number): boolean {
  return state.keys.length < paneCapacity(max);
}

/**
 * Show `viewKey` and focus it.
 *
 * An already open pane is only focused. A new one is appended; at the maximum,
 * the pane that has been on screen longest (the leftmost that is not the focused
 * one) makes room, so opening a fifth view never leaves the arrangement in a
 * state the configured maximum forbids.
 */
export function ensurePane(state: PaneState, viewKey: string, max: number): PaneState {
  if (state.keys.includes(viewKey)) {
    return state.focusedKey === viewKey ? state : {keys: state.keys, focusedKey: viewKey};
  }
  const capacity = paneCapacity(max);
  const kept = state.keys.slice();
  while (kept.length >= capacity) {
    const victim = kept.findIndex(key => key !== state.focusedKey);
    kept.splice(victim < 0 ? 0 : victim, 1);
  }
  kept.push(viewKey);
  return {keys: kept, focusedKey: viewKey};
}

/** Focus a pane that is already open; an unknown view leaves the state alone. */
export function focusPane(state: PaneState, viewKey: string): PaneState {
  if (!state.keys.includes(viewKey) || state.focusedKey === viewKey) {
    return state;
  }
  return {keys: state.keys, focusedKey: viewKey};
}

/**
 * Show `viewKey` in the focused pane, keeping the others.
 *
 * This is what choosing a row in the session list does: the row replaces the
 * conversation in the pane the user is working in, instead of opening a fifth
 * pane. Opening an extra pane is `ensurePane`.
 */
export function replaceFocusedPane(state: PaneState, viewKey: string): PaneState {
  const index = state.keys.indexOf(state.focusedKey);
  if (index < 0) {
    return singlePane(viewKey);
  }
  if (state.keys[index] === viewKey) {
    return state;
  }
  // The focused pane takes the new view: its own view leaves the arrangement
  // when no other pane shows it, and so does a second pane already showing the
  // new view — a set of panes never holds the same view twice.
  const keys = state.keys.filter(key => key !== viewKey && key !== state.focusedKey);
  const at = Math.min(index, keys.length);
  keys.splice(at, 0, viewKey);
  return {keys, focusedKey: viewKey};
}

/**
 * Show another view in one pane, in place.
 *
 * This is what picking a profile in a pane's own drawer does: the pane keeps its
 * slot and the width the user gave it, and only its conversation changes — so a
 * profile is reachable without going back to the session list. A view is on
 * screen once, so the key keeps the place of the pane that showed it first.
 */
export function replacePaneView(state: PaneState, fromKey: string, toKey: string): PaneState {
  const index = state.keys.indexOf(fromKey);
  if (index < 0 || fromKey === toKey) {
    return state;
  }
  const mapped = state.keys.map(key => (key === fromKey ? toKey : key));
  const keys = mapped.filter((key, at) => mapped.indexOf(key) === at);
  return {
    keys,
    focusedKey: state.focusedKey === fromKey ? toKey : state.focusedKey,
  };
}

/**
 * Close one pane. The last pane stays: the shell always shows a conversation,
 * and the caller decides what that view is by activating another one.
 */
export function closePane(state: PaneState, viewKey: string): PaneState {
  const index = state.keys.indexOf(viewKey);
  if (index < 0 || state.keys.length === 1) {
    return state;
  }
  const keys = state.keys.filter(key => key !== viewKey);
  if (state.focusedKey !== viewKey) {
    return {keys, focusedKey: state.focusedKey};
  }
  const neighbour = keys[Math.min(index, keys.length - 1)];
  return {keys, focusedKey: neighbour ?? keys[0]!};
}

/** Where a pane sits, or -1 when it is not open. */
export function paneIndex(state: PaneState, viewKey: string): number {
  return state.keys.indexOf(viewKey);
}

/**
 * The arrangement after the session list changed: panes whose view disappeared
 * are closed, and focus moves to the neighbour of the pane that held it, exactly
 * as closing that pane would.
 */
export function prunePanes(state: PaneState, knownViewKeys: ReadonlySet<string>): PaneState {  const keys = state.keys.filter(key => knownViewKeys.has(key));
  if (keys.length === state.keys.length) {
    return state;
  }
  if (keys.length === 0) {
    return state;
  }
  if (keys.includes(state.focusedKey)) {
    return {keys, focusedKey: state.focusedKey};
  }
  const index = state.keys.indexOf(state.focusedKey);
  return {keys, focusedKey: keys[Math.min(index, keys.length - 1)] ?? keys[0]!};
}
