/**
 * @file catalog-list-view.ts
 * @input The files a catalog kind lists, the search text and the filter choice
 * @output The rows the catalog list shows, its columns and the options of its filter
 * @position Pure rules behind the catalog list; no React and no network
 */
import type {CatalogFileInfo, CatalogKind, CatalogSessionRef} from './api.ts';
import {workspaceOfSessionRef} from './workspaces.ts';

/** Workspace labels by workspace id; an id with no label is shown as itself. */
export type WorkspaceLabels = ReadonlyMap<string, string>;

/** Filter values for "everything" and "nothing to group by"; neither can start a category name, a workspace id or a session ID. */
export const FILTER_ALL = '*';
export const FILTER_NONE = '-';

/** Profiles are filtered by the sessions that use them; every other kind by category, and sessions by workspace. */
export function filtersBySession(kind: CatalogKind): boolean {
  return kind === 'profiles';
}

/**
 * The group a file is listed and filtered under. A session is in the workspace its reference
 * names (`<root>:<name>`; a bare name names none, which is the only case with one root); every
 * other kind is in the folder part of its ID. A session's own `category` text is not read.
 */
export function groupOf(kind: CatalogKind, file: CatalogFileInfo): string {
  if (kind === 'sessions') return workspaceOfSessionRef(file.id) ?? '';
  const slash = file.id.indexOf('/');
  return slash > 0 ? file.id.slice(0, slash) : '';
}

/** What the group column and the filter show for a group: a workspace's label, else the group itself. */
export function groupLabel(kind: CatalogKind, group: string, labels?: WorkspaceLabels): string {
  return kind === 'sessions' ? labels?.get(group) ?? group : group;
}

/** The sessions' filter and column show workspaces; they are useful only with more than one. */
export function groupsByWorkspace(kind: CatalogKind, files: readonly CatalogFileInfo[]): boolean {
  return kind === 'sessions' && new Set(files.map(file => groupOf(kind, file))).size > 1;
}

export function sessionLabel(session: CatalogSessionRef): string {
  return session.name ?? session.id;
}

/** The Session column of a profile: the names of the sessions that can use it. */
export function sessionsOf(file: CatalogFileInfo): string {
  return (file.sessions ?? []).map(sessionLabel).join(', ');
}

export interface FilterOption {
  value: string;
  label: string;
}

export interface FilterSection {
  type: 'section';
  title: string;
  options: FilterOption[];
}

export type FilterEntry = FilterOption | FilterSection;

const byLabel = (a: FilterOption, b: FilterOption) => a.label.localeCompare(b.label);

export function filterOptions(kind: CatalogKind, files: readonly CatalogFileInfo[], labels?: WorkspaceLabels): FilterEntry[] {
  if (filtersBySession(kind)) return sessionFilterOptions(files, labels);
  const named = new Set<string>();
  let hasNone = false;
  for (const file of files) {
    const group = groupOf(kind, file);
    if (group === '') hasNone = true;
    else named.add(group);
  }
  const bySession = kind === 'sessions';
  const options: FilterEntry[] = [{value: FILTER_ALL, label: bySession ? 'All workspaces' : 'All categories'}];
  const namedOptions = [...named].map(group => ({value: group, label: groupLabel(kind, group, labels)}));
  for (const option of namedOptions.sort((a, b) => a.label.localeCompare(b.label) || a.value.localeCompare(b.value))) options.push(option);
  if (hasNone) options.push({value: FILTER_NONE, label: bySession ? 'No workspace' : 'No category'});
  return options;
}

/**
 * The sessions a profile list can be filtered by. With sessions in more than one workspace,
 * one section per workspace holds its sessions; with one (or none named), a plain list.
 */
function sessionFilterOptions(files: readonly CatalogFileInfo[], labels?: WorkspaceLabels): FilterEntry[] {
  const sessions = new Map<string, CatalogSessionRef>();
  let hasNone = false;
  for (const file of files) {
    if ((file.sessions ?? []).length === 0) hasNone = true;
    for (const session of file.sessions ?? []) sessions.set(session.id, session);
  }
  const groups = new Map<string, FilterOption[]>();
  for (const session of sessions.values()) {
    const key = workspaceOfSessionRef(session.id) ?? '';
    groups.set(key, [...(groups.get(key) ?? []), {value: session.id, label: sessionLabel(session)}]);
  }
  const entries: FilterEntry[] = [{value: FILTER_ALL, label: 'All sessions'}];
  if (groups.size <= 1) {
    for (const options of groups.values()) entries.push(...options.sort(byLabel));
  } else {
    const titles = [...groups.keys()].sort((a, b) => (a === '' ? 1 : b === '' ? -1 : groupLabel('sessions', a, labels).localeCompare(groupLabel('sessions', b, labels))));
    for (const title of titles) {
      entries.push({type: 'section', title: title === '' ? 'No workspace' : groupLabel('sessions', title, labels), options: (groups.get(title) ?? []).sort(byLabel)});
    }
  }
  if (hasNone) entries.push({value: FILTER_NONE, label: 'Not in any session'});
  return entries;
}

/** Every selectable value of the filter, so a stale choice can fall back to "all". */
export function filterValues(entries: readonly FilterEntry[]): string[] {
  return entries.flatMap(entry => ('type' in entry ? entry.options.map(option => option.value) : [entry.value]));
}

function matchesFilter(kind: CatalogKind, file: CatalogFileInfo, filter: string): boolean {
  if (filter === FILTER_ALL) return true;
  if (filtersBySession(kind)) {
    const sessions = file.sessions ?? [];
    return filter === FILTER_NONE ? sessions.length === 0 : sessions.some(session => session.id === filter);
  }
  const own = groupOf(kind, file);
  return filter === FILTER_NONE ? own === '' : own === filter;
}

/**
 * Keeps the files that pass the filter and whose name, ID, category (workspace for a session)
 * or sessions hold every whitespace-separated term of the query, ignoring case. Rows sort by
 * that group, then ID.
 */
export function filterCatalogFiles(
  kind: CatalogKind,
  files: readonly CatalogFileInfo[],
  query: string,
  filter: string,
  labels?: WorkspaceLabels,
): CatalogFileInfo[] {
  const terms = query.toLowerCase().split(/\s+/).filter(term => term.length > 0);
  const kept = files.filter(file => {
    if (!matchesFilter(kind, file, filter)) return false;
    const haystack = `${file.name ?? ''} ${file.id} ${groupLabel(kind, groupOf(kind, file), labels)} ${sessionsOf(file)}`.toLowerCase();
    return terms.every(term => haystack.includes(term));
  });
  return kept.sort((a, b) => groupOf(kind, a).localeCompare(groupOf(kind, b)) || a.id.localeCompare(b.id));
}

export type SortDirection = 'ascending' | 'descending';
export interface SortEntry {
  sortKey: string;
  direction: SortDirection;
}

/** One table row; the keys are the column keys, so a column sorts by its own key. */
export interface CatalogRow extends Record<string, unknown> {
  id: string;
  sessions: string;
  /** The category column: a session's workspace label, else the folder part of the ID. */
  category: string;
  name: string;
  /** The reasoning efforts a model file lists, comma separated; empty for other kinds. */
  efforts: string;
  /**
   * The version the file had when it was listed. A row delete carries it, so the
   * server refuses the delete when the file changed after the list was read.
   * Absent for a session file the listing could not read; such a row cannot be deleted.
   */
  etag?: string;
}

/** The two values a row delete needs: which file, and the version it was listed at. */
export type CatalogRowTarget = {id: string; etag: string};

export function catalogRows(kind: CatalogKind, files: readonly CatalogFileInfo[], labels?: WorkspaceLabels): CatalogRow[] {
  return files.map(file => ({
    id: file.id,
    sessions: sessionsOf(file),
    category: groupLabel(kind, groupOf(kind, file), labels),
    name: file.name ?? '',
    efforts: (file.efforts ?? []).join(', '),
    etag: file.etag,
  }));
}

const COLUMN_VALUE: Record<string, (row: CatalogRow) => string> = {
  sessions: row => row.sessions,
  category: row => row.category,
  id: row => row.id,
  name: row => row.name,
  efforts: row => row.efforts,
};

/** Sorts a copy by each entry in turn, ignoring case and reading digits as numbers; the full ID breaks ties. */
export function sortCatalogRows(rows: readonly CatalogRow[], sort: readonly SortEntry[]): CatalogRow[] {
  const compare = (a: string, b: string) => a.localeCompare(b, undefined, {numeric: true, sensitivity: 'base'});
  return [...rows].sort((a, b) => {
    for (const entry of sort) {
      const read = COLUMN_VALUE[entry.sortKey];
      if (!read) continue;
      const result = compare(read(a), read(b));
      if (result !== 0) return entry.direction === 'ascending' ? result : -result;
    }
    return compare(a.id, b.id);
  });
}
