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

import type {SessionRunStatus} from './api';
import {sessionUiStateForRun, type SessionUiState} from './session-run-status.ts';

type ViewTarget = {
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

/** What a profile is called: the name its file sets, or its ID. */
export function profileLabel(profile: string, names?: Readonly<Record<string, string>>): string {
  return names?.[profile] ?? profile;
}

/** What a row is called: the session, and the profile when it is pinned to one. */
export function viewLabel(sessionLabel: string, profile?: string): string {
  const base = sessionLabel.trim();
  return profile === undefined ? base : `${base}: ${profile}`;
}

function findPinnedView(views: readonly PinnedView[], target: ViewTarget): PinnedView | undefined {
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
 * the active key to `viewKey(target.sessionRef, target.profile)`.
 */
export function addPinnedView(views: readonly PinnedView[], target: PinnedView): PinnedView[] {
  if (hasPinnedView(views, target)) {
    return [...views];
  }
  return [...views, {sessionRef: target.sessionRef, profile: target.profile}];
}

/**
 * Drops rows that no longer have a place in the list: the session is gone, the
 * session no longer lists the profile, or the profile is now the stored default
 * (that conversation is the session's own row). A session whose profiles are not
 * known yet keeps its rows.
 */export function prunePinnedViews(
  views: readonly PinnedView[],
  sessions: readonly {id: string; profiles?: readonly string[]; activeProfile?: string}[],
): PinnedView[] {
  const byId = new Map(sessions.map(session => [session.id, session]));
  return views.filter(view => {
    const session = byId.get(view.sessionRef);
    if (session === undefined) {
      return false;
    }
    if (session.profiles === undefined || session.profiles.length === 0) {
      return true;
    }
    return session.profiles.includes(view.profile) && view.profile !== session.activeProfile;
  });
}

/**
 * The active view after pruning: an active profile row whose pin was dropped falls
 * back to its session's own row, as long as that session still exists.
 */
export function activeKeyAfterPrune(
  activeViewKey: string,
  retained: readonly PinnedView[],
  knownSessionIds: ReadonlySet<string>,
): string {
  const profile = profileOfViewKey(activeViewKey);
  const sessionRef = sessionRefOfViewKey(activeViewKey);
  if (
    profile !== undefined &&
    knownSessionIds.has(sessionRef) &&
    !hasPinnedView(retained, {sessionRef, profile})
  ) {
    return viewKey(sessionRef);
  }
  return activeViewKey;
}

/** Visit order of pinned views, newest first; a dropped pin leaves the list. */
export function nextRecentViewKeys(
  recent: readonly string[],
  activeViewKey: string,
  pinnedKeys: ReadonlySet<string>,
): string[] {
  const rest = recent.filter(key => key !== activeViewKey && pinnedKeys.has(key));
  return pinnedKeys.has(activeViewKey) ? [activeViewKey, ...rest] : rest;
}

/**
 * The pinned views whose caches stay: the active one, any with a run in
 * progress, and the `max` most recently visited others. A `max` that is not a
 * positive integer keeps none of the others.
 */
export function retainedPinnedViewKeys(
  pinnedKeys: ReadonlySet<string>,
  recent: readonly string[],
  activeViewKey: string,
  max: number,
  runningKeys: ReadonlySet<string>,
): Set<string> {
  const limit = Number.isInteger(max) && max > 0 ? max : 0;
  const recentOthers = new Set(recent.filter(key => key !== activeViewKey).slice(0, limit));
  const retained = new Set<string>();
  for (const key of pinnedKeys) {
    if (key === activeViewKey || runningKeys.has(key) || recentOthers.has(key)) {
      retained.add(key);
    }
  }
  return retained;
}

/**
 * The retained cache keys plus every pane on screen.
 *
 * The warm-view cap is about *revisit* cost: it may evict a pin the user is not
 * looking at. A pane is on screen, so its key stays whatever the cap says —
 * dropping it skips its transcript snapshot, and the pane renders blank until
 * it is focused.
 */
export function withPaneViewKeys(
  retained: ReadonlySet<string>,
  paneKeys: Iterable<string>,
): Set<string> {
  const next = new Set(retained);
  for (const key of paneKeys) {
    next.add(key);
  }
  return next;
}

/** One row of the left list. */
export type SessionListRow = {
  /** The view this row opens: `session` or `session\u0000profile`. */
  key: string;
  sessionRef: string;
  profile?: string;
  label: string;
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
    rows.push({key: session.id, sessionRef: session.id, label: session.title});
    for (const view of views) {
      if (view.sessionRef !== session.id) {
        continue;
      }
      rows.push({
        key: viewKey(view.sessionRef, view.profile),
        sessionRef: view.sessionRef,
        profile: view.profile,
        label: viewLabel(session.title, view.profile),
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
 * The turn count a row shows.
 *
 * A row opened in this tab reports its own live count. Before that, the
 * session's own row shows the session's count and a profile row shows the
 * server's count for that profile, or nothing when the server sent none.
 */
export function rowTurnCount(
  row: SessionListRow,
  session: {turns: number; profileTurns?: Readonly<Record<string, number>>},
  liveTurns: Readonly<Record<string, number>>,
): number | undefined {
  if (row.profile === undefined) {
    return liveTurns[row.key] ?? session.turns;
  }
  return liveTurns[row.key] ?? session.profileTurns?.[row.profile];
}

/**
 * Whether a row should show an active-run marker.
 *
 * A row reflects only its own run: the session's own row follows the stored
 * default profile, a pinned row its own profile. Other profiles of the session
 * are `groupHasRunningRun`'s concern.
 */
export function rowHasRunningRun(
  row: SessionListRow | undefined,
  session: {
    run?: {status?: string};
    runsByProfile?: Record<string, {status?: string}>;
  } | undefined,
  locallyRunningViewKeys: ReadonlySet<string>,
): boolean {
  if (row === undefined || session === undefined) {
    return false;
  }
  return locallyRunningViewKeys.has(row.key) || rowRunStatus(row, session)?.status === "running";
}

/**
 * The state a row's dot shows.
 *
 * A row running now is `accent` whatever its stored run says. The session's own
 * row follows the session's state; a profile row follows its own profile's run.
 * The rail and the profile search both read this, so the two can never disagree.
 */
export function rowUiState(
  row: SessionListRow,
  session: {
    state: SessionUiState;
    run?: {status?: SessionRunStatus};
    runsByProfile?: Record<string, {status?: SessionRunStatus}>;
  },
  isRunning: boolean,
): SessionUiState {
  if (isRunning) {
    return 'accent';
  }
  if (row.profile === undefined) {
    return session.state;
  }
  return sessionUiStateForRun(rowRunStatus(row, session)?.status);
}

/**
 * The colour a rail dot can wear: the three run states, plus the base `neutral`
 * variant, which a session's own label keeps while `fill` repaints it.
 */
export type DotVariant = SessionUiState | 'neutral';

/**
 * Which paint a dot wears: its variant’s own colour, or the session level’s
 * fill. Explicit, so no call site has to read the tone off the variant name —
 * the session level’s base variant is `neutral` (grey) while what it paints is
 * not grey.
 */
export type DotFill = 'variant' | 'session';

/** A dot’s colour, the word its label uses, and which paint it wears. */
export type DotMark = {variant: DotVariant; word: SessionUiState | 'idle'; fill: DotFill};

/** The mark a profile row shows: its own run state, in that state’s colour. */
export function rowDot(state: SessionUiState): DotMark {
  return {variant: state, word: state, fill: 'variant'};
}

/**
 * The mark a session’s own label shows.
 *
 * The rail draws two levels: the rows are profiles, whose dot answers "what did
 * this profile’s last run do", and the label above them answers only "this is
 * the session". Painting both from the same state made a finished group and every
 * row beneath it one green, so a session label wears the level fill instead —
 * blue in the components, which paint `fill: 'session'` over the base variant.
 * A failure keeps its warning there: a collapsed group must not lose the
 * signal that something went wrong.
 *
 * Use this only where the label really is a session — a profile row, including
 * the stored default profile whose row is the session’s own view, keeps
 * `rowUiState` as it is.
 */
export function sessionLevelDot(state: SessionUiState): DotMark {
  // `success` is the one state a session label does not report: nothing failed
  // and nothing is running, which the rows say better than a second green. The
  // word follows the colour, so the label never announces a state the dot is not
  // showing. The base variant stays `neutral`; `fill: 'session'` is what paints
  // it blue, so the tone is stated rather than inferred from the variant.
  return state === 'success'
    ? {variant: 'neutral', word: 'idle', fill: 'session'}
    : rowDot(state);
}

/**
 * The profile whose `profile_message` call holds a row's profile, while that run
 * is running. The server's run state decides, so the marker stays while this
 * client streams the run too.
 */
export function rowProfileMessageCaller(
  row: SessionListRow | undefined,
  session: {
    run?: {status?: string; source?: string; caller?: string};
    runsByProfile?: Record<string, {status?: string; source?: string; caller?: string}>;
  } | undefined,
): string | undefined {
  if (row === undefined || session === undefined) {
    return undefined;
  }
  const run = rowRunStatus(row, session);
  if (run?.status !== "running" || run.source !== "profile_message") {
    return undefined;
  }
  return run.caller;
}

export function profileMessageRunLabel(caller: string): string {
  return `Running for ${caller} via profile_message`;
}

export function profileMessageBusyNote(caller: string): string {
  return `Busy: ${caller} is using this profile`;
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
}): StoredViews & {restored: boolean} {
  const stored = parseStoredViews(input.stored);
  if (stored.views.length > 0 || stored.activeViewKey.length > 0) {
    return {...stored, restored: true};
  }
  const seed = (input.seed ?? [])
    .filter(view => typeof view?.sessionRef === 'string' && view.sessionRef.length > 0 && typeof view?.profile === 'string' && view.profile.length > 0)
    .map(view => ({sessionRef: view.sessionRef, profile: view.profile}));
  return {views: seed, activeViewKey: input.liveSessionId, restored: false};
}

/** The persisted shape, defended against anything a storage key may hold. */
export function parseStoredViews(value: unknown): StoredViews {
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
    if (hasPinnedView(views, target)) {
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
  /** The session's workspace (its root id); absent when the server names none. */
  workspace?: string;
  /**
   * The profile the session stores as its default, by id. The own row follows
   * it, so this is the id that row actually shows: `Default · jad/default`.
   */
  defaultProfile?: string;
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
 * default exactly as before. Any other profile is the view of that profile
 * (`session\u0000profile`). Every child row looks the same.
 */
export function sessionListGroups(
  sessions: readonly {
    id: string;
    title: string;
    rootId?: string;
    profiles?: readonly string[];
    profileNames?: Readonly<Record<string, string>>;
    activeProfile?: string;
    defaultProfile?: string;
  }[],
): SessionListGroup[] {
  return sessions.map(session => {
    const workspace = session.rootId === undefined || session.rootId === '' ? {} : {workspace: session.rootId};
    const defaultProfile =
      session.defaultProfile === undefined ? {} : {defaultProfile: session.defaultProfile};
    const ownRow: SessionListRow = {key: session.id, sessionRef: session.id, label: session.title};
    const profiles = [...new Set(session.profiles ?? [])];
    if (profiles.length === 0) {
      return {sessionRef: session.id, title: session.title, ...workspace, ...defaultProfile, children: [], ownRow};
    }
    const labelOf = (profile: string) => profileLabel(profile, session.profileNames);
    const children: SessionListRow[] = [];
    if (session.activeProfile === undefined || !profiles.includes(session.activeProfile)) {
      children.push({
        key: session.id,
        sessionRef: session.id,
        label: session.activeProfile === undefined ? session.title : labelOf(session.activeProfile),
      });
    }
    for (const name of profiles) {
      children.push(
        name === session.activeProfile
          ? {key: session.id, sessionRef: session.id, label: labelOf(name)}
          : {key: viewKey(session.id, name), sessionRef: session.id, profile: name, label: labelOf(name)},
      );
    }
    return {sessionRef: session.id, title: session.title, ...workspace, ...defaultProfile, children, ownRow};
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

/**
 * True when any profile of the session has a running run, listed as a row or not
 * (a profile running remotely or in another tab has no row here).
 */
export function groupHasRunningRun(
  group: SessionListGroup,
  session: (Parameters<typeof rowHasRunningRun>[1] & {activeProfile?: string}) | undefined,
  locallyRunningViewKeys: ReadonlySet<string>,
): boolean {
  if ([...group.children, group.ownRow].some(row => rowHasRunningRun(row, session, locallyRunningViewKeys))) {
    return true;
  }
  if (Object.entries(session?.runsByProfile ?? {}).some(
    ([profile, run]) => profile !== session?.activeProfile && run.status === "running",
  )) {
    return true;
  }
  return [...locallyRunningViewKeys].some(
    key => sessionRefOfViewKey(key) === group.sessionRef && profileOfViewKey(key) !== undefined,
  );
}
