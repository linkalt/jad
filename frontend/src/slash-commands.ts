/**
 * The composer's `/` menu: three built-in commands, then every library item.
 *
 * Astryx ends a trigger's query at the first space, so a library item cannot be
 * typed by its name ("Software engineering"). Each one gets a slug instead, and
 * the library kind is the prefix: `/prompt:software-engineering`,
 * `/template:chatgpt-template`, `/skill:review`. Typing `/prompt:` lists one
 * kind; a new kind needs no change here beyond the catalog it arrives in.
 *
 * The module is pure: it builds and ranks the entries. What a selection does is
 * decided by the app, which owns the drawers and the composer inserts.
 */

import type {SearchableItem, SearchSource} from '@astryxdesign/core';
import type {RootLibraryAssetSummary, RootLibraryCatalog, RootLibraryKind} from './api';

export type SlashCommandName = 'compact' | 'state' | 'reset';

export type SlashCommand = {
  name: SlashCommandName;
  description: string;
  /** Extra match terms, so a command is still found by an alias. */
  keywords: readonly string[];
};

/** What the user picked: a built-in action, or a library item to insert. */
export type SlashSelection =
  | {type: 'command'; name: SlashCommandName}
  | {type: 'library'; kind: RootLibraryKind; id: string; name: string};

export const SLASH_COMMANDS: readonly SlashCommand[] = [
  {
    name: 'compact',
    description: 'Compact the conversation history',
    keywords: ['shrink', 'summarise', 'summarize'],
  },
  {
    name: 'state',
    description: 'Write a state-context block',
    keywords: ['context'],
  },
  {
    name: 'reset',
    description: 'Reset this conversation',
    keywords: ['clear', 'new'],
  },
];

export type SlashEntry = {
  item: SearchableItem<SlashSelection>;
  /** Lower-case text a query may match besides the label. */
  terms: readonly string[];
};

/** A name a person can type after `/`: letters and digits of any script, joined by dashes. */
export function librarySlug(name: string): string {
  return (
    name
      .toLowerCase()
      .replace(/[^\p{L}\p{N}]+/gu, '-')
      .replace(/^-+|-+$/g, '') || 'untitled'
  );
}

function libraryEntries(items: readonly RootLibraryAssetSummary[], taken: Set<string>): SlashEntry[] {
  return items.map(summary => {
    const base = `${summary.kind}:${librarySlug(summary.name)}`;
    let key = base;
    for (let suffix = 2; taken.has(key); suffix += 1) {
      key = `${base}-${suffix}`;
    }
    taken.add(key);
    return {
      item: {
        id: key,
        label: `/${key}`,
        auxiliaryData: {type: 'library', kind: summary.kind, id: summary.id, name: summary.name},
      },
      terms: [summary.name.toLowerCase(), summary.description.toLowerCase()],
    };
  });
}

/** Commands first, then the library in the order the server lists it. */
export function slashEntries(
  commands: readonly SlashCommand[],
  catalog: RootLibraryCatalog | null,
): SlashEntry[] {
  const taken = new Set<string>(commands.map(command => command.name));
  const entries: SlashEntry[] = commands.map(command => ({
    item: {
      id: command.name,
      label: `/${command.name}`,
      auxiliaryData: {type: 'command', name: command.name},
    },
    terms: [command.description.toLowerCase(), ...command.keywords.map(word => word.toLowerCase())],
  }));
  if (catalog !== null) {
    for (const items of [catalog.prompts, catalog.templates, catalog.skills]) {
      entries.push(...libraryEntries(items, taken));
    }
  }
  return entries;
}

/** Lower is better, `null` is no match. A name's start beats its middle; descriptions rank last. */
function slashScore(entry: SlashEntry, needle: string): number | null {
  const key = entry.item.id.toLowerCase();
  if (key === needle) return 0;
  if (key.startsWith(needle)) return 1;
  const slug = key.slice(key.indexOf(':') + 1);
  if (slug.startsWith(needle)) return 2;
  if (key.includes(needle)) return 3;
  return entry.terms.some(term => term.includes(needle)) ? 4 : null;
}

/** The entries a query matches, best first and capped; an empty query keeps the list order. */
export function rankSlashEntries(
  entries: readonly SlashEntry[],
  query: string,
  maxResults: number,
): SearchableItem<SlashSelection>[] {
  const limit = Math.max(0, Math.floor(maxResults));
  const needle = query.trim().toLowerCase().replace(/^\//, '');
  if (needle === '') {
    return entries.slice(0, limit).map(entry => entry.item);
  }
  const ranked: {item: SearchableItem<SlashSelection>; score: number; index: number}[] = [];
  entries.forEach((entry, index) => {
    const score = slashScore(entry, needle);
    if (score !== null) ranked.push({item: entry.item, score, index});
  });
  ranked.sort((a, b) => a.score - b.score || a.index - b.index);
  return ranked.slice(0, limit).map(entry => entry.item);
}

export type SlashSourceOptions = {
  commands: readonly SlashCommand[];
  /** Loads the library catalog; a failure leaves the built-in commands on their own. */
  loadCatalog: () => Promise<RootLibraryCatalog>;
  maxResults: number;
  /** How long a loaded catalog answers before the next search asks the server again. */
  refreshMs: number;
  now?: () => number;
};

/**
 * The `/` menu's source.
 *
 * Astryx only ever calls `search` (never `bootstrap`) and calls `cancel` before
 * every search, so the library is fetched lazily from `search` and a load is
 * never aborted. A catalog stays usable for `refreshMs`; after that the next
 * keystroke refetches it, so a prompt saved since the menu last opened appears.
 */
export function createSlashSource({
  commands,
  loadCatalog,
  maxResults,
  refreshMs,
  now = Date.now,
}: SlashSourceOptions): SearchSource {
  let entries: SlashEntry[] = slashEntries(commands, null);
  let loadedAt: number | null = null;
  let inFlight: Promise<void> | null = null;

  const ensureFresh = (): Promise<void> => {
    if (loadedAt !== null && now() - loadedAt < refreshMs) return Promise.resolve();
    if (inFlight) return inFlight;
    inFlight = (async () => {
      try {
        entries = slashEntries(commands, await loadCatalog());
        loadedAt = now();
      } catch {
        // A failed load keeps the entries the menu already has, and is retried next search.
      } finally {
        inFlight = null;
      }
    })();
    return inFlight;
  };

  const search = async (query: string) => {
    await ensureFresh();
    return rankSlashEntries(entries, query, maxResults);
  };
  return {search, bootstrap: () => search('')};
}
