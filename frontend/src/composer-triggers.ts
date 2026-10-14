/**
 * The composer's `@` file menu and `/` command menu.
 *
 * Astryx renders the menu itself: `ChatComposerInput` takes trigger definitions,
 * detects the character at a word boundary, searches the trigger's
 * `SearchSource` and inserts whatever `onSelect` returns — plain text, or an
 * inline badge chip. What it does not own is the file list, so this module does:
 * the source the menu searches (the active session's file listing, with the
 * checked-in list as the fallback when that listing fails), the ranking that
 * answers a typed query, and the trigger definitions themselves.
 *
 * A selected chip serializes to `@<path>` — the path reference the agent's own
 * `read`/`grep`/`glob` tools act on — so nothing downstream has to understand
 * mentions beyond the path text.
 *
 * Two behaviours of 0.6.2 worth knowing before extending this:
 * - `deserialize` is declared on a trigger but never called by the component
 *   (grep: zero occurrences in the compiled JS), so a chip that outlives the
 *   element — a reload, or an external rewrite of the composer value — stays
 *   plain `@path` text. It still reads correctly; it just loses its badge.
 * - The menu only opens when the trigger character starts a word, so `@` typed
 *   mid-word (an email address, say) leaves the menu closed.
 */

import type {
  ChatComposerToken,
  ChatComposerTrigger,
  SearchableItem,
  SearchSource,
} from '@astryxdesign/core';
import type {SlashSelection} from './slash-commands';

/** Character that opens the file menu. */
export const FILE_TRIGGER_CHARACTER = '@';
/** Character that opens the command menu. */
export const COMMAND_TRIGGER_CHARACTER = '/';

/**
 * Paths offered by the `@` menu when the app ships without a listing service.
 *
 * They are repository-relative — the same form the agent's tools take — and are
 * ordered roughly by how often a prompt refers to them, because that order is
 * what a bare `@` shows. `resolveMentionFiles` lets `mention_files` in
 * config.json replace the whole list without a code change, and the eventual
 * backend listing swaps in at the same seam.
 */
export const BUILTIN_MENTION_FILES: readonly string[] = [
  'CLAUDE.md',
  'MEMORY_DESIGN.md',
  'design_cursor.md',
  'design_bash.md',
  'design_edit.md',
  'design_glob.md',
  'design_grep.md',
  'design_process.md',
  'design_task.md',
  'frontend/package.json',
  'frontend/src/App.tsx',
  'frontend/src/api.ts',
  'frontend/src/config.json',
  'frontend/src/composer-triggers.ts',
  'frontend/src/draftStore.ts',
  'frontend/src/promptLibrary.ts',
  'frontend/src/row-heights.ts',
  'frontend/src/components/ComposerPane.tsx',
  'frontend/src/components/TranscriptWindow.tsx',
  'frontend/src/components/scroll-position.ts',
  'frontend/src/components/MarkdownText.tsx',
  'frontend/src/components/PromptLibraryPanel.tsx',
  'frontend/src/themes/neutral/neutralTheme.ts',
  'frontend/src/themes/neutral/icons.tsx',
  'sample/schemes/setting.schema.json',
  'sample/tools/native/read.json',
];

/** A configured list wins once it has entries; otherwise the built-in one stands. */
export function resolveMentionFiles(
  configured: readonly string[] | undefined,
  builtin: readonly string[] = BUILTIN_MENTION_FILES,
): readonly string[] {
  return configured && configured.length > 0 ? configured : builtin;
}

/**
 * How well a path answers a query — lower is better, `null` means no match.
 *
 * A file's own name beats the directories it sits in: typing `composer` should
 * reach `ComposerPane.tsx` before something that merely lives under a
 * `composer/` folder.
 */
function matchScore(path: string, needle: string): number | null {
  const lowerPath = path.toLowerCase();
  if (lowerPath === needle) {
    return 0;
  }
  // A folder is listed as `src/`; its own name is the part before that slash.
  const trimmed = lowerPath.endsWith('/') ? lowerPath.slice(0, -1) : lowerPath;
  const base = trimmed.slice(trimmed.lastIndexOf('/') + 1);
  if (base === needle) {
    return 1;
  }
  if (base.startsWith(needle)) {
    return 2;
  }
  if (base.includes(needle)) {
    return 3;
  }
  return lowerPath.includes(needle) ? 4 : null;
}

/**
 * The paths a query matches, best first and capped.
 *
 * Ties keep the list's own order, which is its curated order, so the ranking
 * only ever reorders for a reason. An empty query is "show me the list" and
 * returns that order unchanged.
 */
export function rankMentionFiles(
  files: readonly string[],
  query: string,
  maxResults: number,
): string[] {
  const limit = Math.max(0, Math.floor(maxResults));
  const needle = query.trim().toLowerCase();
  if (needle === '') {
    return files.slice(0, limit);
  }
  const ranked: {path: string; score: number; index: number}[] = [];
  files.forEach((path, index) => {
    const score = matchScore(path, needle);
    if (score !== null) {
      ranked.push({path, score, index});
    }
  });
  ranked.sort((a, b) => a.score - b.score || a.index - b.index);
  return ranked.slice(0, limit).map(entry => entry.path);
}

/** The menu's items. The path is both the id and what the menu shows. */
export function mentionItems(
  files: readonly string[],
  query: string,
  maxResults: number,
): SearchableItem[] {
  return rankMentionFiles(files, query, maxResults).map(path => ({
    id: path,
    label: path,
  }));
}

/** A `SearchSource` over a fixed list — the whole list is local, so search is sync. */
export function createFileMentionSource(
  files: readonly string[],
  maxResults: number,
): SearchSource {
  return {
    search: query => mentionItems(files, query, maxResults),
    bootstrap: () => mentionItems(files, '', maxResults),
  };
}

/**
 * What a selected file becomes: a chip that shows the path and serializes to
 * `@path`. The chip is blue so it is not mistaken for a pasted-text token,
 * which is neutral.
 */
function mentionToken(path: string): ChatComposerToken {
  return {
    value: `${FILE_TRIGGER_CHARACTER}${path}`,
    label: path,
    variant: 'blue',
  };
}

/** Loads the `@` menu's paths for a session; rejects when the listing is unavailable. */
export type SessionFilesLoader = (signal?: AbortSignal) => Promise<readonly string[]>;

export type SessionFileSourceOptions = {
  loadFiles: SessionFilesLoader;
  /** Answered when the listing fails, so `@` keeps working with the backend down. */
  fallbackFiles: readonly string[];
  maxResults: number;
};

/**
 * The `@` menu's source over a session's file listing.
 *
 * `bootstrap` runs once per menu open and always asks again, so files the agent
 * created since the last open appear. `search` then filters the fetched list in
 * memory, which keeps typing off the network. A failed or aborted load answers
 * the configured static list rather than an empty menu the user cannot explain.
 */
export function createSessionFileSource({
  loadFiles,
  fallbackFiles,
  maxResults,
}: SessionFileSourceOptions): SearchSource {
  let controller: AbortController | null = null;
  let inFlight: Promise<readonly string[]> | null = null;
  let loaded: readonly string[] | null = null;

  const load = (): Promise<readonly string[]> => {
    if (inFlight) {
      return inFlight;
    }
    controller = new AbortController();
    const signal = controller.signal;
    inFlight = (async () => {
      try {
        loaded = await loadFiles(signal);
      } catch {
        // Aborts and transport failures land here together: keep whatever the
        // menu already has instead of replacing it with nothing.
        loaded = loaded ?? fallbackFiles;
      } finally {
        inFlight = null;
      }
      return loaded ?? fallbackFiles;
    })();
    return inFlight;
  };

  return {
    bootstrap: () => {
      loaded = null;
      return load().then(files => mentionItems(files, '', maxResults));
    },
    search: async query => mentionItems(loaded ?? (await load()), query, maxResults),
    cancel: () => {
      controller?.abort();
      controller = null;
    },
  };
}

/** The `@` trigger over a path source. */
export function fileMentionTrigger(searchSource: SearchSource): ChatComposerTrigger {
  return {
    character: FILE_TRIGGER_CHARACTER,
    searchSource,
    menuLabel: 'Files',
    emptySearchText: 'No matching files',
    loadingText: 'Loading files…',
    onSelect: item => mentionToken(item.id),
  };
}

/**
 * The `/` trigger over a slash source. Picking an entry does not put text in the
 * draft: the selection goes to `onPick`, which runs the command or fetches the
 * library item, and the `/query` the user typed is removed.
 */
export function slashTrigger(
  searchSource: SearchSource,
  onPick: (selection: SlashSelection) => void,
): ChatComposerTrigger {
  return {
    character: COMMAND_TRIGGER_CHARACTER,
    searchSource,
    menuLabel: 'Commands and library',
    emptySearchText: 'No matching command or library item',
    loadingText: 'Loading library…',
    onSelect: item => {
      const selection = item.auxiliaryData as SlashSelection | undefined;
      if (selection !== undefined) onPick(selection);
      return '';
    },
  };
}

export type ComposerTriggerOptions = {
  files: readonly string[];
  /** Cap on menu entries, from `mention_max_results`. */
  maxResults: number;
  /**
   * When set, the `@` menu lists this session's files instead of the static
   * list, which stays behind as the fallback for a listing that fails.
   */
  sessionFiles?: SessionFilesLoader;
  /** The `/` menu. Left out, the composer has no slash trigger. */
  slash?: {source: SearchSource; onPick: (selection: SlashSelection) => void};
};

/** Every trigger the composer should carry, in menu-character order. */
export function createComposerTriggers({
  files,
  maxResults,
  sessionFiles,
  slash,
}: ComposerTriggerOptions): ChatComposerTrigger[] {
  const mentionSource = sessionFiles
    ? createSessionFileSource({loadFiles: sessionFiles, fallbackFiles: files, maxResults})
    : createFileMentionSource(files, maxResults);
  const triggers = [fileMentionTrigger(mentionSource)];
  if (slash !== undefined) {
    triggers.push(slashTrigger(slash.source, slash.onPick));
  }
  return triggers;
}
