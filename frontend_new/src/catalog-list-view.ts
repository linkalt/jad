/**
 * @file catalog-list-view.ts
 * @input The files a catalog kind lists, the search text and the filter choice
 * @output The rows the catalog list shows, its columns and the options of its filter
 * @position Pure rules behind the catalog list; no React and no network
 */
import type {CatalogFileInfo, CatalogKind, CatalogSessionRef} from './api.ts';

/** Filter values for "everything" and "nothing to group by"; neither can start a category name or a session ID. */
export const FILTER_ALL = '*';
export const FILTER_NONE = '-';

/** Profiles are grouped by the sessions that use them; every other kind by category. */
export function filtersBySession(kind: CatalogKind): boolean {
  return kind === 'profiles';
}

/** A session sets its own category; every other kind is grouped by the folder part of its ID. */
export function categoryOf(kind: CatalogKind, file: CatalogFileInfo): string {
  if (kind === 'sessions') return file.category ?? '';
  const slash = file.id.indexOf('/');
  return slash > 0 ? file.id.slice(0, slash) : '';
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

export function filterOptions(kind: CatalogKind, files: readonly CatalogFileInfo[]): FilterEntry[] {
  if (filtersBySession(kind)) return sessionFilterOptions(files);
  const named = new Set<string>();
  let hasNone = false;
  for (const file of files) {
    const category = categoryOf(kind, file);
    if (category === '') hasNone = true;
    else named.add(category);
  }
  const options: FilterEntry[] = [{value: FILTER_ALL, label: 'All categories'}];
  for (const category of [...named].sort((a, b) => a.localeCompare(b))) options.push({value: category, label: category});
  if (hasNone) options.push({value: FILTER_NONE, label: 'No category'});
  return options;
}

/** One section per session category, holding that category's sessions; sessions with none come last. */
function sessionFilterOptions(files: readonly CatalogFileInfo[]): FilterEntry[] {
  const sessions = new Map<string, CatalogSessionRef>();
  let hasNone = false;
  for (const file of files) {
    if ((file.sessions ?? []).length === 0) hasNone = true;
    for (const session of file.sessions ?? []) sessions.set(session.id, session);
  }
  const groups = new Map<string, FilterOption[]>();
  for (const session of sessions.values()) {
    const key = session.category ?? '';
    groups.set(key, [...(groups.get(key) ?? []), {value: session.id, label: sessionLabel(session)}]);
  }
  const entries: FilterEntry[] = [{value: FILTER_ALL, label: 'All sessions'}];
  const titles = [...groups.keys()].sort((a, b) => (a === '' ? 1 : b === '' ? -1 : a.localeCompare(b)));
  for (const title of titles) {
    entries.push({type: 'section', title: title === '' ? 'No category' : title, options: (groups.get(title) ?? []).sort(byLabel)});
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
  const own = categoryOf(kind, file);
  return filter === FILTER_NONE ? own === '' : own === filter;
}

/**
 * Keeps the files that pass the filter and whose name, ID, category or sessions hold every
 * whitespace-separated term of the query, ignoring case. Rows sort by category, then ID.
 */
export function filterCatalogFiles(
  kind: CatalogKind,
  files: readonly CatalogFileInfo[],
  query: string,
  filter: string,
): CatalogFileInfo[] {
  const terms = query.toLowerCase().split(/\s+/).filter(term => term.length > 0);
  const kept = files.filter(file => {
    if (!matchesFilter(kind, file, filter)) return false;
    const haystack = `${file.name ?? ''} ${file.id} ${categoryOf(kind, file)} ${sessionsOf(file)}`.toLowerCase();
    return terms.every(term => haystack.includes(term));
  });
  return kept.sort((a, b) => categoryOf(kind, a).localeCompare(categoryOf(kind, b)) || a.id.localeCompare(b.id));
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
  category: string;
  name: string;
}

export function catalogRows(kind: CatalogKind, files: readonly CatalogFileInfo[]): CatalogRow[] {
  return files.map(file => ({
    id: file.id,
    sessions: sessionsOf(file),
    category: categoryOf(kind, file),
    name: file.name ?? '',
  }));
}

const COLUMN_VALUE: Record<string, (row: CatalogRow) => string> = {
  sessions: row => row.sessions,
  category: row => row.category,
  id: row => row.id,
  name: row => row.name,
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
