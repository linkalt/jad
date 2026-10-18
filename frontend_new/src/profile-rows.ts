/**
 * The rows of a pane's profile drawer.
 *
 * Every profile of every session is offered, so one pane can be pointed at any
 * conversation in the app without going back to the rail. The rows are the
 * rail's own: the session's stored default profile is the session's own row and
 * every other profile is a child row of that session. The drawer therefore works
 * from the rail's rows instead of rebuilding keys of its own — a key it invented
 * would name a view the rest of the app cannot open, and the default profile
 * would get a second, dead key (`session\u0000default`) beside the session's own.
 */

/** One profile as the rail already offers it. */
export interface ProfileSourceRow {
  /** The view key the rail opens for this profile. */
  key: string;
  /** The label the rail shows for it. */
  label: string;
  /** The profile path this row names; absent for the session's own row. */
  profile?: string;
  /**
   * The profile a session's own row follows, by id — what the server reports as
   * the session's stored default. Only the own row carries it: its `profile` is
   * absent on purpose, so the server keeps resolving the default for it.
   */
  defaultProfile?: string;
  /** The session this row belongs to, so a list of every profile stays readable. */
  session?: string;
}

/** One row of the drawer, with its own selection state. */
export interface ProfileChoiceRow {
  row: ProfileSourceRow;
  isSelected: boolean;
  /**
   * The view is already on screen in another pane, so picking it here is not
   * offered: a view is shown once, and the row that has it says so instead.
   */
  isOpenElsewhere: boolean;
}

/** No pane is showing any of the rows. */
const NO_OPEN_VIEWS: ReadonlySet<string> = new Set<string>();

/** A session as the rail groups it: its own row's title and its profile rows. */
export interface ProfileGroupSource {
  sessionRef: string;
  title: string;
  /** The profile the session stores as its default, by id. */
  defaultProfile?: string;
  children: readonly ProfileSourceRow[];
}

/**
 * Every profile of every session, in the rail's order: per session, its own row
 * (which follows the stored default) first, then its profile rows.
 *
 * The session's own row is kept even for a session whose list already names a
 * default profile, because a pane can be sitting on that row (`key` =
 * `sessionRef`) and it has to find itself in the list. Its label is the rail's
 * own label for it (`Default`), and its id is the profile the server reports as
 * the session's stored default (`jad/default`) — the row carries no `profile`, so
 * the server keeps resolving that default for it. A row the rail already offers
 * under that key is kept once.
 */
export function allProfileRows(groups: readonly ProfileGroupSource[]): ProfileSourceRow[] {
  const rows: ProfileSourceRow[] = [];
  const seen = new Set<string>();
  const add = (row: ProfileSourceRow): void => {
    if (seen.has(row.key)) return;
    seen.add(row.key);
    rows.push(row);
  };
  for (const group of groups) {
    const ownRow = group.children.find(child => child.key === group.sessionRef);
    const storedDefault =
      group.defaultProfile === undefined ? {} : {defaultProfile: group.defaultProfile};
    add(
      ownRow === undefined
        ? {key: group.sessionRef, label: group.title, session: group.title, ...storedDefault}
        : {...ownRow, session: group.title, ...storedDefault},
    );
    for (const child of group.children) {
      add({...child, session: group.title});
    }
  }
  return rows;
}

export function filterProfileRows(
  rows: readonly ProfileSourceRow[],
  query: string,
): ProfileSourceRow[] {
  const needle = query.trim().toLowerCase();
  if (needle === '') return [...rows];
  return rows.filter(
    row =>
      row.label.toLowerCase().includes(needle) ||
      (row.session ?? '').toLowerCase().includes(needle) ||
      (row.profile ?? row.defaultProfile ?? '').toLowerCase().includes(needle),
  );
}

/**
 * The rows the profile drawer shows. Collapsed, it is the conversation's current
 * profile alone — clicking that row opens the list. Expanded, it is every
 * profile of every session, filtered by the search text.
 *
 * A row whose view is open in another pane (`openViewKeys`) is marked instead of
 * selectable. Only the open list refuses rows: collapsed, the single row is the
 * way into the list, so it stays pressable even when another pane holds that
 * view — otherwise the drawer could not be opened at all.
 *
 * A pane whose key is not one of those rows (restored against a profile no
 * session lists any more) keeps the first row visible instead of opening an
 * empty drawer; that row is left unmarked, because it is not where the pane is.
 */
export function profileChoiceRows(
  rows: readonly ProfileSourceRow[],
  selectedKey: string | undefined,
  query: string,
  isExpanded: boolean,
  openViewKeys: ReadonlySet<string> = NO_OPEN_VIEWS,
): ProfileChoiceRow[] {
  const current =
    selectedKey !== undefined && rows.some(row => row.key === selectedKey)
      ? selectedKey
      : rows[0]?.key;
  const visible = isExpanded ? filterProfileRows(rows, query) : rows.filter(row => row.key === current);
  return visible.map(row => ({
    row,
    isSelected: row.key === selectedKey,
    // Only the open list refuses rows: collapsed, the one row on screen is the
    // way into it, so it must stay pressable even when another pane holds it.
    isOpenElsewhere: isExpanded && row.key !== selectedKey && openViewKeys.has(row.key),
  }));
}

/**
 * Whether the drawer has anywhere to go: a list holding only the conversation on
 * screen has nothing to switch to, so its control is not offered at all.
 */
export function hasProfileToSwitchTo(
  rows: readonly ProfileSourceRow[],
  selectedKey: string | undefined,
): boolean {
  return rows.some(row => row.key !== selectedKey);
}
