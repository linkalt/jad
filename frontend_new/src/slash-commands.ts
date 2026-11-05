/**
 * The composer's `/` menu: the built-in commands, then the custom commands
 * (`/<command>`, from libraries.command_dirs), then every library item, then
 * the session profile's MCP prompts and MCP resources. `/plan` switches the composer's plan mode;
 * the other built-ins run a session action.
 *
 * Astryx ends a trigger's query at the first space, so a library item cannot be
 * typed by its name ("Software engineering"). Each one gets a slug instead, and
 * the library kind is the prefix: `/prompt:software-engineering`,
 * `/template:chatgpt-template`, `/skill:review`. Typing `/prompt:` lists one
 * kind; a new kind needs no change here beyond the catalog it arrives in. An MCP
 * prompt is `/mcp:<server>:<slug>`, from the servers whose toolset entry has
 * `prompts: true`; `jad run` accepts the same words.
 *
 * The module is pure: it builds and ranks the entries. What a selection does is
 * decided by the app, which owns the drawers and the composer inserts.
 */

import type {SearchableItem, SearchSource} from '@astryxdesign/core';
import type {
  MCPPrompt,
  MCPResource,
  RootLibraryAssetSummary,
  RootLibraryCatalog,
  RootLibraryCommand,
  RootLibraryKind,
} from './api';

export type SlashCommandName = 'compact' | 'state' | 'reset' | 'plan';

export type SlashCommand = {
  name: SlashCommandName;
  description: string;
  /** Extra match terms, so a command is still found by an alias. */
  keywords: readonly string[];
};

/** What the user picked: a built-in action, or a library item to insert. */
export type SlashSelection =
  | {type: 'command'; name: SlashCommandName}
  | {type: 'library'; kind: RootLibraryKind; id: string; name: string}
  | {type: 'mcp'; prompt: MCPPrompt}
  | {type: 'custom'; command: RootLibraryCommand}
  | {type: 'resource'; resource: MCPResource};

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
  {
    name: 'plan',
    description: 'Switch plan mode: read-only tools, the answer is a plan',
    keywords: ['read-only', 'design', 'think'],
  },
];

/** The built-in commands, without `/plan` when the composer has no plan mode. */
export function builtInSlashCommands(planModeEnabled: boolean): readonly SlashCommand[] {
  return planModeEnabled ? SLASH_COMMANDS : SLASH_COMMANDS.filter(command => command.name !== 'plan');
}

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

function customEntries(commands: readonly RootLibraryCommand[], taken: Set<string>): SlashEntry[] {
  // The server gives each command its own word and keeps the built-in words and the
  // kind prefixes for themselves; a repeat is still dropped rather than shadowing one.
  return commands.flatMap(command => {
    if (taken.has(command.command)) return [];
    taken.add(command.command);
    return [{
      item: {id: command.command, label: `/${command.command}`, auxiliaryData: {type: 'custom', command}},
      terms: [command.name, command.description, command.argumentHint ?? ''].map(term => term.toLowerCase()),
    }];
  });
}

function mcpEntries(prompts: readonly MCPPrompt[], taken: Set<string>): SlashEntry[] {
  return prompts.map(prompt => {
    const base = `mcp:${prompt.server}:${librarySlug(prompt.name)}`;
    let key = base;
    for (let suffix = 2; taken.has(key); suffix += 1) {
      key = `${base}-${suffix}`;
    }
    taken.add(key);
    return {
      item: {id: key, label: `/${key}`, auxiliaryData: {type: 'mcp', prompt}},
      terms: [prompt.name, prompt.title ?? '', prompt.description ?? ''].map(term => term.toLowerCase()),
    };
  });
}

function resourceEntries(resources: readonly MCPResource[], taken: Set<string>): SlashEntry[] {
  return resources.map(resource => {
    const base = `resource:${resource.server}:${librarySlug(resource.name)}`;
    let key = base;
    for (let suffix = 2; taken.has(key); suffix += 1) {
      key = `${base}-${suffix}`;
    }
    taken.add(key);
    return {
      item: {id: key, label: `/${key}`, auxiliaryData: {type: 'resource', resource}},
      terms: [resource.name, resource.title ?? '', resource.description ?? '', resource.uri ?? resource.uriTemplate ?? '']
        .map(term => term.toLowerCase()),
    };
  });
}

/**
 * Built-ins first, then custom commands, the library in the order the server lists it,
 * the MCP prompts and the MCP resources.
 */
export function slashEntries(
  commands: readonly SlashCommand[],
  catalog: RootLibraryCatalog | null,
  mcpPrompts: readonly MCPPrompt[] = [],
  mcpResources: readonly MCPResource[] = [],
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
    entries.push(...customEntries(catalog.commands ?? [], taken));
    for (const items of [catalog.prompts, catalog.templates, catalog.skills]) {
      entries.push(...libraryEntries(items, taken));
    }
  }
  entries.push(...mcpEntries(mcpPrompts, taken));
  entries.push(...resourceEntries(mcpResources, taken));
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
  /** Loads the session profile's MCP prompts; left out, the menu has none. */
  loadMCPPrompts?: () => Promise<readonly MCPPrompt[]>;
  /** Loads the session profile's MCP resources; left out, the menu has none. */
  loadMCPResources?: () => Promise<readonly MCPResource[]>;
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
  loadMCPPrompts,
  loadMCPResources,
  maxResults,
  refreshMs,
  now = Date.now,
}: SlashSourceOptions): SearchSource {
  let catalog: RootLibraryCatalog | null = null;
  let mcpPrompts: readonly MCPPrompt[] = [];
  let mcpResources: readonly MCPResource[] = [];
  let entries: SlashEntry[] = slashEntries(commands, null);
  let loadedAt: number | null = null;
  let inFlight: Promise<void> | null = null;

  // The library, the MCP prompts and the MCP resources load side by side. One failing
  // keeps what the menu had of it, shows the others, and leaves the whole load to be
  // retried next search.
  const ensureFresh = (): Promise<void> => {
    if (loadedAt !== null && now() - loadedAt < refreshMs) return Promise.resolve();
    if (inFlight) return inFlight;
    inFlight = (async () => {
      try {
        const [library, mcp, resources] = await Promise.allSettled([
          loadCatalog(),
          loadMCPPrompts === undefined ? Promise.resolve(mcpPrompts) : loadMCPPrompts(),
          loadMCPResources === undefined ? Promise.resolve(mcpResources) : loadMCPResources(),
        ]);
        if (library.status === 'fulfilled') catalog = library.value;
        if (mcp.status === 'fulfilled') mcpPrompts = mcp.value;
        if (resources.status === 'fulfilled') mcpResources = resources.value;
        entries = slashEntries(commands, catalog, mcpPrompts, mcpResources);
        if (library.status === 'fulfilled' && mcp.status === 'fulfilled' && resources.status === 'fulfilled') {
          loadedAt = now();
        }
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

/** The variables to send for a resource template: the template's own, without empty ones. */
export function mcpResourceVariableValues(
  resource: MCPResource,
  values: Readonly<Record<string, string>>,
): Record<string, string> {
  const chosen: Record<string, string> = {};
  for (const name of resource.variables) {
    const value = values[name] ?? '';
    if (value.trim() !== '') chosen[name] = value;
  }
  return chosen;
}

/** Whether every required argument of an MCP prompt has a value. */
export function mcpPromptArgumentsReady(prompt: MCPPrompt, values: Readonly<Record<string, string>>): boolean {
  return prompt.arguments.every(argument => argument.required !== true || (values[argument.name] ?? '').trim() !== '');
}

/** The arguments to send: the prompt's own, without the optional ones left empty. */
export function mcpPromptArgumentValues(
  prompt: MCPPrompt,
  values: Readonly<Record<string, string>>,
): Record<string, string> {
  const chosen: Record<string, string> = {};
  for (const argument of prompt.arguments) {
    const value = values[argument.name] ?? '';
    if (value.trim() !== '') chosen[argument.name] = value;
  }
  return chosen;
}
