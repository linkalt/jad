/**
 * The profile search's model: the rows the rail renders, flattened, and the
 * filter over them.
 *
 * The panel used to offer sessions. But the rows a user scrolls, reads turns for
 * and clicks are profile rows: a session's title appears nowhere in them, so
 * typing `Backend1` found nothing. The searchable set is therefore the rows
 * themselves — one entry per profile row the rail shows, and a session's own
 * row only when it has no profile rows to offer — and choosing one opens that
 * view, exactly as clicking the row does.
 *
 * Everything here is pure. The turns and the state come from the same helpers
 * the rail uses, so a result and its row can never disagree.
 */

import type {SessionRunStatus} from './api';
import type {SessionUiState} from './session-run-status';
import {
  rowHasRunningRun,
  rowTurnCount,
  rowUiState,
  viewLabel,
  type SessionListGroup,
  type SessionListRow,
} from './session-views.ts';

/** The parts of a session the search reads: what a row shows, and nothing else. */
export type ProfileSearchSession = {
  id: string;
  title: string;
  turns: number;
  profileTurns?: Readonly<Record<string, number>>;
  state: SessionUiState;
  run?: {status?: SessionRunStatus};
  runsByProfile?: Record<string, {status?: SessionRunStatus}>;
};

/** One result: a row of the rail, with the numbers that row shows. */
export type ProfileSearchRow = {
  /** The view this result opens: `session` or `session\u0000profile`. */
  key: string;
  /** The row's own name: the profile, or the session title for a session with no profiles. */
  label: string;
  /** What the row is called with its session: the rail's accessible name. */
  fullLabel: string;
  /** The session the row belongs to, printed under the label. */
  sessionTitle: string;
  /** The count the rail's row shows, or undefined when it shows none. */
  turns: number | undefined;
  /** The state the rail's dot shows. */
  state: SessionUiState;
};

/**
 * The rows the search offers, in the rail's order.
 *
 * A group's children are its profile rows, so those are what a user can find by
 * name. A group without children offers no profile row at all, so its own row
 * stands in for it: otherwise that session would be unreachable from the panel.
 */
export function profileSearchRows(
  groups: readonly SessionListGroup[],
  sessions: readonly ProfileSearchSession[],
  rowTurns: Readonly<Record<string, number>>,
  runningViewKeys: ReadonlySet<string>,
): ProfileSearchRow[] {
  const entry = (row: SessionListRow, session: ProfileSearchSession): ProfileSearchRow => ({
    key: row.key,
    label: row.label,
    fullLabel: row.profile === undefined ? row.label : viewLabel(session.title, row.label),
    sessionTitle: session.title,
    turns: rowTurnCount(row, session, rowTurns),
    state: rowUiState(row, session, rowHasRunningRun(row, session, runningViewKeys)),
  });
  const rows: ProfileSearchRow[] = [];
  for (const group of groups) {
    const session = sessions.find(candidate => candidate.id === group.sessionRef);
    if (session === undefined) {
      continue;
    }
    if (group.children.length === 0) {
      rows.push(entry(group.ownRow, session));
      continue;
    }
    for (const child of group.children) {
      rows.push(entry(child, session));
    }
  }
  return rows;
}

/** The line under a result: which session, how many turns, and the state. */
export function profileSearchDescription(row: ProfileSearchRow): string {
  const parts: string[] = [];
  // On a session's own row the label already is the session title.
  if (row.label !== row.sessionTitle) {
    parts.push(row.sessionTitle);
  }
  if (row.turns !== undefined) {
    parts.push(`${row.turns} ${row.turns === 1 ? 'turn' : 'turns'}`);
  }
  parts.push(row.state);
  return parts.join(' · ');
}

/**
 * The results for `query`, capped at `limit`.
 *
 * A row matches on its own name, the session it belongs to, its view key — so a
 * session id is searchable — and its state. The query is trimmed and matched
 * case-insensitively, and an empty query offers every row.
 */
export function matchProfileSearchRows(
  rows: readonly ProfileSearchRow[],
  query: string,
  limit: number,
): ProfileSearchRow[] {
  const trimmed = query.trim().toLowerCase();
  const matches =
    trimmed.length === 0
      ? rows
      : rows.filter(row =>
          `${row.label} ${row.sessionTitle} ${row.key} ${row.state}`.toLowerCase().includes(trimmed),
        );
  return matches.slice(0, limit);
}
