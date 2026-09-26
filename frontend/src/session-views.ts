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
 * The profiles a session's context menu offers.
 *
 * The session's stored default is left out: its row is the session's own row, so
 * pinning it would put the same conversation in the list twice.
 */
export function profileMenuTargets(session: {
  id: string;
  title: string;
  profiles?: readonly string[];
  activeProfile?: string;
}): {profile: string; label: string}[] {
  return (session.profiles ?? [])
    .filter(profile => profile !== session.activeProfile)
    .map(profile => ({profile, label: viewLabel(session.title, profile)}));
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
