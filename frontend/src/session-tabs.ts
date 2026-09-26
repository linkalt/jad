/**
 * The tab strip's model: one tab is one view of one session.
 *
 * A tab addresses a session, optionally pinned to one of its path profiles. The
 * pair is what the backend calls a run slot and what the client keys every
 * per-view cache by — two tabs on one session with different profiles are two
 * conversations, each with its own transcript, model and run.
 *
 * Everything here is pure: ids are derived from the target, so a tab keeps its
 * identity across reloads and two tabs can never name the same view twice.
 */

export type SessionTabTarget = {
  sessionRef: string;
  /** A path profile of that session; absent means "follow the stored default". */
  profile?: string;
};

/** What a tab holds: its target, addressed by an id derived from it. */
export type SessionTab = SessionTabTarget & {
  id: string;
};

export type TabMenuTarget = SessionTabTarget & {
  /** What the entry says, e.g. `session1` or `session1: chatgpt`. */
  label: string;
  /** The same name without the profile, for a menu's grouping or a tooltip. */
  sessionLabel: string;
};

/**
 * The separator inside a derived id.
 *
 * Session references are `[A-Za-z0-9._-]` (with a `:` between a root and a
 * session), and profile names add `_` and `-`, so a NUL can never appear in
 * either part — the id stays injective and parseable.
 */
const KEY_SEPARATOR = '\u0000';

/** The id of one view: the session, plus the profile when the tab is pinned. */
export function tabId(target: SessionTabTarget): string {
  return target.profile === undefined
    ? target.sessionRef
    : `${target.sessionRef}${KEY_SEPARATOR}${target.profile}`;
}

/**
 * The key per-view client state is held under.
 *
 * It is the view id itself: a transcript, a scroll position, an attached run and
 * a hydrated message all belong to one `(session, profile)` and must not be
 * shared with the session's other profile.
 */
export function viewKey(sessionRef: string, profile: string | undefined): string {
  return tabId(profile === undefined ? {sessionRef} : {sessionRef, profile});
}

/** The session a view key names, whatever profile it was pinned to. */
export function sessionRefOfViewKey(key: string): string {
  const separator = key.indexOf(KEY_SEPARATOR);
  return separator < 0 ? key : key.slice(0, separator);
}

/** True when a view key is pinned to a profile. */
export function isPinnedViewKey(key: string): boolean {
  return key.includes(KEY_SEPARATOR);
}

export function tabFromTarget(target: SessionTabTarget): SessionTab {
  // Only the target is kept: a menu entry carries presentation fields of its
  // own, and a tab must not accumulate them (they would be persisted with it).
  return target.profile === undefined
    ? {sessionRef: target.sessionRef, id: target.sessionRef}
    : {sessionRef: target.sessionRef, profile: target.profile, id: tabId(target)};
}

function sameTarget(left: SessionTabTarget, right: SessionTabTarget): boolean {
  return left.sessionRef === right.sessionRef && left.profile === right.profile;
}

/** The tab already showing this view, if any. */
export function findTab(tabs: readonly SessionTab[], target: SessionTabTarget): SessionTab | undefined {
  return tabs.find(tab => sameTarget(tab, target));
}

/**
 * Opens a target in a new tab.
 *
 * A target that is already open is left alone — the caller focuses it by setting
 * the active id to `tabId(target)` — and the strip stops growing at `limit`.
 */
export function addTab(
  tabs: readonly SessionTab[],
  target: SessionTabTarget,
  limit: number,
): SessionTab[] {
  if (tabs.length >= limit || findTab(tabs, target) !== undefined) {
    return [...tabs];
  }
  return [...tabs, tabFromTarget(target)];
}

/**
 * Points a tab at another view.
 *
 * This is what a sidebar click does when the strip is already showing tabs: the
 * current view becomes the session that was clicked, at its stored default. If
 * another tab already shows that view, this tab is dropped instead of
 * duplicating it.
 */
export function retargetTab(
  tabs: readonly SessionTab[],
  tabIdToRetarget: string,
  target: SessionTabTarget,
): SessionTab[] {
  const existing = findTab(tabs, target);
  if (existing !== undefined) {
    return existing.id === tabIdToRetarget
      ? [...tabs]
      : tabs.filter(tab => tab.id !== tabIdToRetarget);
  }
  return tabs.map(tab => (tab.id === tabIdToRetarget ? tabFromTarget(target) : tab));
}

/** The id that takes over when a tab closes: its right neighbour, else its left. */
export function neighbourTabId(tabs: readonly SessionTab[], tabIdToClose: string): string {
  const index = tabs.findIndex(tab => tab.id === tabIdToClose);
  if (index < 0) {
    return tabs[0]?.id ?? '';
  }
  return (tabs[index + 1] ?? tabs[index - 1] ?? tabs[index]!).id;
}

/**
 * Closes one tab. The strip always keeps at least one view: closing the last tab
 * would leave the app with nothing to show, and the conversation it held is on
 * the server either way.
 */
export function closeTab(tabs: readonly SessionTab[], tabIdToClose: string): SessionTab[] {
  if (tabs.length <= 1) {
    return [...tabs];
  }
  return tabs.filter(tab => tab.id !== tabIdToClose);
}

/** Closes every tab except this one. */
export function closeOtherTabs(tabs: readonly SessionTab[], keepTabId: string): SessionTab[] {
  const kept = tabs.filter(tab => tab.id === keepTabId);
  return kept.length === 0 ? [...tabs] : kept;
}

/**
 * Drops tabs whose session the backend no longer lists.
 *
 * All of them can be gone (a session deleted while it was the only tab open), so
 * the caller gets an empty list and decides what to fall back to.
 */
export function pruneTabs(tabs: readonly SessionTab[], liveSessionRefs: readonly string[]): SessionTab[] {
  const live = new Set(liveSessionRefs);
  return tabs.filter(tab => live.has(tab.sessionRef));
}

/**
 * What a tab is called: the session, and the profile when it is pinned to one.
 *
 * `session1` follows whatever the session's stored default is; `session1:
 * chatgpt` is a view of that profile specifically.
 */
export function tabLabel(target: SessionTabTarget, sessionLabel?: string): string {
  const base = sessionLabel?.trim() || target.sessionRef;
  return target.profile === undefined ? base : `${base}: ${target.profile}`;
}

/**
 * The entries the `+` menu offers: every live session, and every profile of a
 * session that configures them.
 *
 * A session without profiles contributes one entry; a session with two profiles
 * contributes the plain view plus one per profile, so a user can open "the
 * session" and "this session on chatgpt" side by side.
 */
export function tabMenuTargets(
  sessions: readonly {sessionRef: string; label: string; profiles?: readonly string[]}[],
): TabMenuTarget[] {
  const targets: TabMenuTarget[] = [];
  for (const session of sessions) {
    targets.push({
      sessionRef: session.sessionRef,
      label: tabLabel({sessionRef: session.sessionRef}, session.label),
      sessionLabel: session.label,
    });
    for (const profile of session.profiles ?? []) {
      targets.push({
        sessionRef: session.sessionRef,
        profile,
        label: tabLabel({sessionRef: session.sessionRef, profile}, session.label),
        sessionLabel: session.label,
      });
    }
  }
  return targets;
}

/**
 * The run a tab shows.
 *
 * A pinned tab looks its own profile up in the session's per-profile runs; an
 * unpinned one follows the session's stored default, which is exactly what the
 * snapshot's `run` holds.
 */
export function tabRunStatus<T>(
  tab: SessionTabTarget | undefined,
  session: {run?: T; runsByProfile?: Record<string, T>} | undefined,
): T | undefined {
  if (tab === undefined || session === undefined) {
    return undefined;
  }
  if (tab.profile === undefined) {
    return session.run;
  }
  return session.runsByProfile?.[tab.profile];
}

export type ParsedTabs = {
  tabs: SessionTab[];
  activeTabId: string;
};

export type StoredTabs = ParsedTabs & {
  /**
   * True when the stored set was used. A restored set is a choice the user
   * already made, so a preference like "open the configured session" must not
   * move it.
   */
  restored: boolean;
};

/**
 * The tabs to open on a fresh page.
 *
 * The stored set wins when it holds anything usable; otherwise the configured
 * seed; otherwise the live session, which is what the app opened before tabs
 * existed.
 */
export function restoreTabs(input: {
  stored?: unknown;
  seed?: readonly SessionTabTarget[];
  liveSessionId: string;
  limit: number;
}): StoredTabs {
  const stored = parseStoredTabs(input.stored, input.limit);
  if (stored.tabs.length > 0) {
    return {...stored, restored: true};
  }
  const seed = (input.seed ?? [])
    .filter(target => typeof target?.sessionRef === 'string' && target.sessionRef.length > 0)
    .slice(0, input.limit);
  const tabs = (seed.length > 0 ? seed : [{sessionRef: input.liveSessionId}]).map(tabFromTarget);
  return {tabs, activeTabId: tabs[0]!.id, restored: false};
}

/** The persisted shape, defended against anything a storage key may hold. */
export function parseStoredTabs(value: unknown, limit: number): ParsedTabs {
  if (typeof value !== 'object' || value === null) {
    return {tabs: [], activeTabId: ''};
  }
  const record = value as {tabs?: unknown; activeTabId?: unknown};
  const targets = Array.isArray(record.tabs) ? record.tabs : [];
  const tabs: SessionTab[] = [];
  for (const entry of targets) {
    if (typeof entry !== 'object' || entry === null) {
      continue;
    }
    const candidate = entry as {sessionRef?: unknown; profile?: unknown};
    if (typeof candidate.sessionRef !== 'string' || candidate.sessionRef.length === 0) {
      continue;
    }
    const target: SessionTabTarget = typeof candidate.profile === 'string' && candidate.profile.length > 0
      ? {sessionRef: candidate.sessionRef, profile: candidate.profile}
      : {sessionRef: candidate.sessionRef};
    if (findTab(tabs, target) !== undefined || tabs.length >= limit) {
      continue;
    }
    tabs.push(tabFromTarget(target));
  }
  if (tabs.length === 0) {
    return {tabs: [], activeTabId: ''};
  }
  const requested = typeof record.activeTabId === 'string' ? record.activeTabId : '';
  const activeTabId = tabs.some(tab => tab.id === requested) ? requested : tabs[0]!.id;
  return {tabs, activeTabId};
}
