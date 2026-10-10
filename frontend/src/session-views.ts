/**
 * The left list's model: a session row, and any number of pinned profile rows.
 *
 * The session list is where a view is chosen, so a view is a row: the session
 * itself (following its stored default profile, exactly as before) or one of its
 * path profiles pinned to a row of its own — `playground: chatgpt`. A pinned row
 * is a conversation of its own: its own transcript, model and run slot, which is
 * what the server addresses with `?profile=`.
 *
 * Everything here is pure. Ids are derived from the target, so a row keeps its
 * identity across reloads and two rows can never name the same view.
 */

export type ViewTarget = {
  sessionRef: string;
  /** A path profile of that session; absent means "follow the stored default". */
  profile?: string;
};

/** A row this client added for one profile of a session. */
export type PinnedView = ViewTarget & {profile: string};

/**
 * The separator inside a derived id.
 *
 * Session references are `[A-Za-z0-9._-]` (with a `:` between a root and a
 * session) and profile names add `_` and `-`, so a NUL can never appear in
 * either part — the id stays injective and parseable.
 */
const KEY_SEPARATOR = '\u0000';

/** The id of one view: the session, plus the profile when it is pinned. */
export function viewKey(sessionRef: string, profile?: string): string {
  return profile === undefined ? sessionRef : `${sessionRef}${KEY_SEPARATOR}${profile}`;
}

export function viewKeyOf(target: ViewTarget): string {
  return viewKey(target.sessionRef, target.profile);
}

/** The session a view key names, whatever profile it was pinned to. */
export function sessionRefOfViewKey(key: string): string {
  const separator = key.indexOf(KEY_SEPARATOR);
  return separator < 0 ? key : key.slice(0, separator);
}

/** The profile a view key is pinned to, or undefined for the session's own row. */
export function profileOfViewKey(key: string): string | undefined {
  const separator = key.indexOf(KEY_SEPARATOR);
  return separator < 0 ? undefined : key.slice(separator + 1);
}

export function isPinnedViewKey(key: string): boolean {
  return key.includes(KEY_SEPARATOR);
}

/** What a row is called: the session, and the profile when it is pinned to one. */
export function viewLabel(sessionLabel: string, profile?: string): string {
  const base = sessionLabel.trim();
  return profile === undefined ? base : `${base}: ${profile}`;
}

export function findPinnedView(views: readonly PinnedView[], target: ViewTarget): PinnedView | undefined {
  if (target.profile === undefined) {
    return undefined;
  }
  return views.find(view => view.sessionRef === target.sessionRef && view.profile === target.profile);
}

export function hasPinnedView(views: readonly PinnedView[], target: ViewTarget): boolean {
  return findPinnedView(views, target) !== undefined;
}

/**
 * Adds a pinned row.
 *
 * A view that is already open is left alone — the caller selects it by setting
 * the active key to `viewKeyOf(target)` — and the list stops growing at `limit`.
 */
export function addPinnedView(
  views: readonly PinnedView[],
  target: PinnedView,
  limit: number,
): PinnedView[] {
  if (views.length >= limit || hasPinnedView(views, target)) {
    return [...views];
  }
  return [...views, {sessionRef: target.sessionRef, profile: target.profile}];
}

/** Removes the row a view key names; a session's own row cannot be removed. */
export function removePinnedView(views: readonly PinnedView[], key: string): PinnedView[] {
  const profile = profileOfViewKey(key);
  if (profile === undefined) {
    return [...views];
  }
  const sessionRef = sessionRefOfViewKey(key);
  return views.filter(view => !(view.sessionRef === sessionRef && view.profile === profile));
}

/** Drops rows whose session the backend no longer lists. */
export function prunePinnedViews(
  views: readonly PinnedView[],
  knownSessionRefs: readonly string[],
): PinnedView[] {
  const known = new Set(knownSessionRefs);
  return views.filter(view => known.has(view.sessionRef));
}

/**
 * What picking a profile in the composer means for the row on screen.
 *
 * Every control of the composer acts on the view, not on the session behind it,
 * so the choice lands where the pointer is:
 *
 * - the session's own row follows whichever profile it is given — that is the
 *   stored default every other client sees (`'session'`);
 * - a pinned row shows the picked profile in that row (`'repin'`), except when
 *   the pick is the stored default: that conversation *is* the session's own row,
 *   so the row is dropped and `'session'` says where to go.
 */
export function profileSelection(
  row: {profile?: string},
  picked: string,
  session: {activeProfile?: string},
): "session" | "repin" {
  if (row.profile === undefined || picked === session.activeProfile) {
    return "session";
  }
  return "repin";
}

/** One row of the left list. */
export type SessionListRow = {
  /** The view this row opens: `session` or `session\u0000profile`. */
  key: string;
  sessionRef: string;
  profile?: string;
  label: string;
  isPinned: boolean;
};

/**
 * The rows the list shows: every session in list order, and directly under it
 * the profile rows this client pinned to it.
 */
export function sessionListRows(
  sessions: readonly {id: string; title: string}[],
  views: readonly PinnedView[],
): SessionListRow[] {
  const rows: SessionListRow[] = [];
  for (const session of sessions) {
    rows.push({key: session.id, sessionRef: session.id, label: session.title, isPinned: false});
    for (const view of views) {
      if (view.sessionRef !== session.id) {
        continue;
      }
      rows.push({
        key: viewKey(view.sessionRef, view.profile),
        sessionRef: view.sessionRef,
        profile: view.profile,
        label: viewLabel(session.title, view.profile),
        isPinned: true,
      });
    }
  }
  return rows;
}

/**
 * The run a row shows.
 *
 * A pinned row looks its own profile up in the session's per-profile runs; the
 * session's own row follows the stored default, which is what `run` holds.
 */
export function rowRunStatus<T>(
  row: SessionListRow | undefined,
  session: {run?: T; runsByProfile?: Record<string, T>} | undefined,
): T | undefined {
  if (row === undefined || session === undefined) {
    return undefined;
  }
  return row.profile === undefined ? session.run : session.runsByProfile?.[row.profile];
}

/**
 * Whether a row should show an active-run marker.
 *
 * A pinned row only reflects its own profile's run. The session's stored-default
 * row also acts as an aggregate indicator for detached or remotely running
 * pinned profiles, without making sibling pinned rows look active.
 */
export function rowHasRunningRun(
  row: SessionListRow | undefined,
  session: {
    activeProfile?: string;
    run?: {status?: string};
    runsByProfile?: Record<string, {status?: string}>;
  } | undefined,
  locallyRunningViewKeys: ReadonlySet<string>,
): boolean {
  if (row === undefined || session === undefined) {
    return false;
  }
  if (locallyRunningViewKeys.has(row.key) || rowRunStatus(row, session)?.status === "running") {
    return true;
  }
  if (row.profile !== undefined) {
    return false;
  }
  if (Object.entries(session.runsByProfile ?? {}).some(
    ([profile, run]) => profile !== session.activeProfile && run.status === "running",
  )) {
    return true;
  }
  return [...locallyRunningViewKeys].some(
    (key) => sessionRefOfViewKey(key) === row.sessionRef && profileOfViewKey(key) !== undefined,
  );
}

export type StoredViews = {
  views: PinnedView[];
  activeViewKey: string;
};

/**
 * The rows and selection to open a page with.
 *
 * The stored set wins when it holds anything usable; otherwise the configured
 * seed; otherwise the live session. `restored` says whether the user's own
 * arrangement was used — a preference like "open the configured session" must
 * not move it.
 */
export function restoreViews(input: {
  stored?: unknown;
  seed?: readonly PinnedView[];
  liveSessionId: string;
  limit: number;
}): StoredViews & {restored: boolean} {
  const stored = parseStoredViews(input.stored, input.limit);
  if (stored.views.length > 0 || stored.activeViewKey.length > 0) {
    return {...stored, restored: true};
  }
  const seed = (input.seed ?? [])
    .filter(view => typeof view?.sessionRef === 'string' && view.sessionRef.length > 0 && typeof view?.profile === 'string' && view.profile.length > 0)
    .slice(0, input.limit)
    .map(view => ({sessionRef: view.sessionRef, profile: view.profile}));
  return {views: seed, activeViewKey: input.liveSessionId, restored: false};
}

/** The persisted shape, defended against anything a storage key may hold. */
export function parseStoredViews(value: unknown, limit: number): StoredViews {
  if (typeof value !== 'object' || value === null) {
    return {views: [], activeViewKey: ''};
  }
  const record = value as {views?: unknown; activeViewKey?: unknown};
  const views: PinnedView[] = [];
  for (const entry of Array.isArray(record.views) ? record.views : []) {
    if (typeof entry !== 'object' || entry === null) {
      continue;
    }
    const candidate = entry as {sessionRef?: unknown; profile?: unknown};
    if (
      typeof candidate.sessionRef !== 'string' ||
      candidate.sessionRef.length === 0 ||
      typeof candidate.profile !== 'string' ||
      candidate.profile.length === 0
    ) {
      continue;
    }
    const target = {sessionRef: candidate.sessionRef, profile: candidate.profile};
    if (hasPinnedView(views, target) || views.length >= limit) {
      continue;
    }
    views.push(target);
  }
  return {
    views,
    activeViewKey: typeof record.activeViewKey === 'string' ? record.activeViewKey : '',
  };
}

/** One session of the grouped list: a header, and a row for each of its profiles. */
export type SessionListGroup = {
  sessionRef: string;
  title: string;
  /** The session's category; absent when it has none. */
  category?: string;
  /**
   * One row per profile. Empty when the session has no path profiles: the group
   * is then a single selectable row, `ownRow`.
   */
  children: SessionListRow[];
  /** The session's own row (its stored default profile), always present. */
  ownRow: SessionListRow;
};

/**
 * The grouped list: every session once, with a child row for each of its path
 * profiles.
 *
 * The profile that is the session's stored default is the session's own row
 * (`key` = the session id, no `profile`), so it keeps following the stored
 * default exactly as before. Any other profile is the pinned view of that
 * profile (`session\u0000profile`), whether or not this client has pinned it yet.
 * A pinned view whose profile the session no longer lists stays visible, after
 * the listed ones, so the row on screen always has a place in the list.
 */
export function sessionListGroups(
  sessions: readonly {
    id: string;
    title: string;
    category?: string;
    profiles?: readonly string[];
    activeProfile?: string;
  }[],
  views: readonly PinnedView[],
): SessionListGroup[] {
  return sessions.map(session => {
    const category = session.category === undefined || session.category === '' ? {} : {category: session.category};
    const ownRow: SessionListRow = {
      key: session.id,
      sessionRef: session.id,
      label: session.title,
      isPinned: false,
    };
    const profiles = session.profiles ?? [];
    if (profiles.length === 0) {
      return {sessionRef: session.id, title: session.title, ...category, children: [], ownRow};
    }
    const pinnedProfiles = new Set(
      views.filter(view => view.sessionRef === session.id).map(view => view.profile),
    );
    const listed = new Set(profiles);
    const names = [
      ...new Set([...profiles, ...[...pinnedProfiles].filter(profile => !listed.has(profile))]),
    ];
    const children: SessionListRow[] = [];
    if (session.activeProfile === undefined || !listed.has(session.activeProfile)) {
      children.push({
        key: session.id,
        sessionRef: session.id,
        label: session.activeProfile ?? session.title,
        isPinned: false,
      });
    }
    for (const name of names) {
      if (name === session.activeProfile) {
        // Listed: this is the own row. Unlisted: the own row was added above.
        if (listed.has(name)) {
          children.push({key: session.id, sessionRef: session.id, label: name, isPinned: false});
        }
        // A view pinned before this profile became the stored default keeps its
        // row, so it can still be removed and still counts where the user sees it.
        if (pinnedProfiles.has(name)) {
          children.push({key: viewKey(session.id, name), sessionRef: session.id, profile: name, label: name, isPinned: true});
        }
        continue;
      }
      children.push({
        key: viewKey(session.id, name),
        sessionRef: session.id,
        profile: name,
        label: name,
        isPinned: pinnedProfiles.has(name),
      });
    }
    return {sessionRef: session.id, title: session.title, ...category, children, ownRow};
  });
}

/** Whether a group is open: expanded unless the user collapsed it. */
export function isGroupExpanded(group: SessionListGroup, collapsedRefs: readonly string[]): boolean {
  return !collapsedRefs.includes(group.sessionRef);
}

/** The new collapsed set after the user opened or closed one group. */
export function setGroupCollapsed(
  collapsedRefs: readonly string[],
  sessionRef: string,
  isCollapsed: boolean,
): string[] {
  const rest = collapsedRefs.filter(ref => ref !== sessionRef);
  return isCollapsed ? [...rest, sessionRef] : rest;
}

/** The persisted collapsed set, defended against anything a store may hold. */
export function parseCollapsedGroups(value: unknown): string[] {
  if (!Array.isArray(value)) {
    return [];
  }
  return [...new Set(value.filter((ref): ref is string => typeof ref === 'string' && ref.length > 0))];
}

/** The sessions of one category, in list order. */
export type CategorySection = {
  /** The category name; absent for the sessions that have none. */
  category?: string;
  groups: SessionListGroup[];
};

/**
 * Splits the grouped list by category.
 *
 * Categories keep the order in which the list first meets them, so the list's own
 * order decides, and sessions inside a category keep their list order. Sessions
 * without a category form one section, after the named ones.
 */
export function groupByCategory(groups: readonly SessionListGroup[]): CategorySection[] {
  const named = new Map<string, SessionListGroup[]>();
  const unnamed: SessionListGroup[] = [];
  for (const group of groups) {
    if (group.category === undefined) {
      unnamed.push(group);
      continue;
    }
    const section = named.get(group.category);
    if (section === undefined) {
      named.set(group.category, [group]);
    } else {
      section.push(group);
    }
  }
  const sections: CategorySection[] = [...named].map(([category, items]) => ({category, groups: items}));
  if (unnamed.length > 0) {
    sections.push({groups: unnamed});
  }
  return sections;
}
