import assert from 'node:assert/strict';
import test from 'node:test';

import {
  SLASH_COMMANDS,
  builtInSlashCommands,
  createSlashSource,
  librarySlug,
  mcpPromptArgumentValues,
  mcpPromptArgumentsReady,
  mcpResourceVariableValues,
  rankSlashEntries,
  slashEntries,
} from './slash-commands.ts';

const summary = (kind, id, name, description = '') => ({id, kind, name, description, source: 'x'});
const CATALOG = {
  prompts: [
    summary('prompt', 'p1', 'Software engineering', 'Code agent'),
    summary('prompt', 'p2', 'Bug finding'),
  ],
  templates: [summary('template', 't1', 'ChatGPT template')],
  skills: [summary('skill', 's1', 'Review')],
};
const ids = items => items.map(item => item.id);

test('a name becomes a typeable slug, and a name with no letters still gets one', () => {
  assert.equal(librarySlug('Software engineering'), 'software-engineering');
  assert.equal(librarySlug('  A -- B / C!  '), 'a-b-c');
  assert.equal(librarySlug('日本語 テスト'), '日本語-テスト');
  assert.equal(librarySlug('***'), 'untitled');
});

test('commands come first, then prompts, templates and skills, each prefixed by its kind', () => {
  assert.deepEqual(ids(slashEntries(SLASH_COMMANDS, CATALOG).map(entry => entry.item)), [
    'compact',
    'state',
    'reset',
    'plan',
    'prompt:software-engineering',
    'prompt:bug-finding',
    'template:chatgpt-template',
    'skill:review',
  ]);
});

test('two items with one name get distinct keys, and a prefix keeps them off the commands', () => {
  const entries = slashEntries(SLASH_COMMANDS, {
    prompts: [summary('prompt', 'a', 'Same'), summary('prompt', 'b', 'Same')],
    templates: [],
    skills: [],
  });
  assert.deepEqual(ids(entries.map(entry => entry.item)).slice(SLASH_COMMANDS.length), ['prompt:same', 'prompt:same-2']);
});

test('the selection carries what the app needs to run or fetch the entry', () => {
  const entries = slashEntries(SLASH_COMMANDS, CATALOG);
  assert.deepEqual(entries[0].item.auxiliaryData, {type: 'command', name: 'compact'});
  assert.deepEqual(entries[SLASH_COMMANDS.length].item.auxiliaryData, {
    type: 'library',
    kind: 'prompt',
    id: 'p1',
    name: 'Software engineering',
  });
});

test('typing a kind prefix lists that kind, and a bare slash lists everything in order', () => {
  const entries = slashEntries(SLASH_COMMANDS, CATALOG);
  assert.deepEqual(ids(rankSlashEntries(entries, 'prompt:', 50)), [
    'prompt:software-engineering',
    'prompt:bug-finding',
  ]);
  assert.equal(rankSlashEntries(entries, '', 50).length, SLASH_COMMANDS.length + 4);
  assert.deepEqual(ids(rankSlashEntries(entries, 'template:', 50)), ['template:chatgpt-template']);
});

test('a name start beats a middle match, and a command alias or description is found', () => {
  const entries = slashEntries(SLASH_COMMANDS, CATALOG);
  assert.deepEqual(ids(rankSlashEntries(entries, 'bug', 50)), ['prompt:bug-finding']);
  assert.deepEqual(ids(rankSlashEntries(entries, 'clear', 50)), ['reset']);
  assert.deepEqual(ids(rankSlashEntries(entries, 'code agent', 50)), ['prompt:software-engineering']);
  assert.deepEqual(ids(rankSlashEntries(entries, 'zzz', 50)), []);
  assert.deepEqual(ids(rankSlashEntries(entries, 'e', 3)).length, 3, 'the cap applies to matches');
});

const source = (options = {}) =>
  createSlashSource({commands: SLASH_COMMANDS, maxResults: 50, refreshMs: 1000, ...options});

test('the first search loads the library, and a bare search lists everything in order', async () => {
  let loads = 0;
  const menu = source({
    loadCatalog: async () => {
      loads += 1;
      return CATALOG;
    },
  });
  assert.equal(ids(await menu.search('')).length, 8);
  assert.deepEqual(ids(await menu.search('skill:')), ['skill:review']);
  assert.equal(loads, 1, 'typing filters the loaded catalog');
});

test('searches that start together share one load', async () => {
  let loads = 0;
  const menu = source({
    loadCatalog: async () => {
      loads += 1;
      return CATALOG;
    },
  });
  await Promise.all([menu.search(''), menu.search('a'), menu.bootstrap()]);
  assert.equal(loads, 1);
});

test('a catalog older than refreshMs is fetched again, so a new prompt appears', async () => {
  let clock = 0;
  let catalog = CATALOG;
  let loads = 0;
  const menu = source({
    now: () => clock,
    loadCatalog: async () => {
      loads += 1;
      return catalog;
    },
  });
  assert.equal(ids(await menu.search('')).length, 8);
  catalog = {...CATALOG, prompts: [...CATALOG.prompts, summary('prompt', 'p3', 'Fresh')]};
  clock = 999;
  assert.equal(ids(await menu.search('')).length, 8);
  clock = 1000;
  assert.ok(ids(await menu.search('')).includes('prompt:fresh'));
  assert.equal(loads, 2);
});

test('a failing library load leaves the built-in commands usable and is retried', async () => {
  let offline = true;
  const menu = source({
    loadCatalog: async () => {
      if (offline) throw new Error('offline');
      return CATALOG;
    },
  });
  assert.deepEqual(ids(await menu.search('')), ['compact', 'state', 'reset', 'plan']);
  assert.deepEqual(ids(await menu.search('res')), ['reset']);
  offline = false;
  assert.equal(ids(await menu.search('')).length, 8);
});

test('the source answers a promise, so Astryx takes its async path and waits for the load', () => {
  const menu = source({loadCatalog: async () => CATALOG});
  assert.ok(menu.search('') instanceof Promise);
});

const MCP_PROMPTS = [
  {server: 'gh', name: 'review_pr', description: 'Review a pull request', arguments: [{name: 'pr', required: true}, {name: 'focus'}]},
  {server: 'gh', name: 'Review PR', arguments: []},
  {server: 'docs', name: 'standup', title: 'Daily stand-up', arguments: []},
];

test('MCP prompts follow the library as mcp:<server>:<slug>, a taken slug numbered', () => {
  const entries = slashEntries(SLASH_COMMANDS, CATALOG, MCP_PROMPTS);
  assert.deepEqual(ids(entries.map(entry => entry.item)).slice(SLASH_COMMANDS.length + 4), ['mcp:gh:review-pr', 'mcp:gh:review-pr-2', 'mcp:docs:standup']);
  const picked = entries[SLASH_COMMANDS.length + 4].item.auxiliaryData;
  assert.equal(picked.type, 'mcp');
  assert.equal(picked.prompt.name, 'review_pr');
  assert.deepEqual(ids(rankSlashEntries(entries, 'mcp:', 50)), ['mcp:gh:review-pr', 'mcp:gh:review-pr-2', 'mcp:docs:standup']);
  assert.deepEqual(ids(rankSlashEntries(entries, 'stand-up', 50)), ['mcp:docs:standup'], 'the title is a match term');
});

test('the library and the MCP prompts load side by side, and either may fail alone', async () => {
  let libraryOffline = false;
  let mcpOffline = true;
  let mcpLoads = 0;
  const menu = source({
    loadCatalog: async () => {
      if (libraryOffline) throw new Error('offline');
      return CATALOG;
    },
    loadMCPPrompts: async () => {
      mcpLoads += 1;
      if (mcpOffline) throw new Error('MCP prompts are unavailable');
      return MCP_PROMPTS;
    },
  });
  assert.equal(ids(await menu.search('')).length, 8, 'the library is listed while MCP fails');
  mcpOffline = false;
  assert.equal(ids(await menu.search('')).length, 11, 'a failed load is retried next search');
  assert.equal(mcpLoads, 2);
  assert.equal(ids(await menu.search('')).length, 11);
  assert.equal(mcpLoads, 2, 'a full load answers until refreshMs');
});

test('a failing library still shows the MCP prompts', async () => {
  const menu = source({
    loadCatalog: async () => {
      throw new Error('offline');
    },
    loadMCPPrompts: async () => MCP_PROMPTS,
  });
  assert.deepEqual(ids(await menu.search('')), ['compact', 'state', 'reset', 'plan', 'mcp:gh:review-pr', 'mcp:gh:review-pr-2', 'mcp:docs:standup']);
});

test('an MCP prompt is ready when its required arguments have text; empty optional ones are left out', () => {
  const [review] = MCP_PROMPTS;
  assert.equal(mcpPromptArgumentsReady(review, {}), false);
  assert.equal(mcpPromptArgumentsReady(review, {pr: '   '}), false);
  assert.equal(mcpPromptArgumentsReady(review, {pr: '42'}), true);
  assert.deepEqual(mcpPromptArgumentValues(review, {pr: '42', focus: '', other: 'x'}), {pr: '42'});
  assert.deepEqual(mcpPromptArgumentValues(review, {pr: '42', focus: 'tests'}), {pr: '42', focus: 'tests'});
});

test('/plan is listed only while the composer has plan mode', () => {
  assert.deepEqual(builtInSlashCommands(true).map(command => command.name), ['compact', 'state', 'reset', 'plan']);
  assert.deepEqual(builtInSlashCommands(false).map(command => command.name), ['compact', 'state', 'reset']);
  const entries = slashEntries(builtInSlashCommands(true), CATALOG);
  assert.deepEqual(ids(rankSlashEntries(entries, 'read-only', 50)), ['plan']);
  // A library item named Plan does not take the command's word.
  const planned = slashEntries(SLASH_COMMANDS, {...CATALOG, prompts: [summary('prompt', 'p9', 'Plan')]});
  assert.ok(ids(planned.map(entry => entry.item)).includes('prompt:plan'));
});

test('custom commands follow the built-ins as /<command>, before the library', () => {
  const command = (id, word, extra = {}) => ({id, kind: 'command', name: word, description: '', source: `${word}.md`, command: word, takesArguments: false, ...extra});
  const catalog = {
    ...CATALOG,
    commands: [command('c1', 'git:commit', {description: 'Commit staged', argumentHint: '<message>', takesArguments: true}), command('c2', 'lint'), command('c3', 'lint')],
  };
  const entries = slashEntries(SLASH_COMMANDS, catalog);
  assert.deepEqual(ids(entries.map(entry => entry.item)).slice(SLASH_COMMANDS.length, SLASH_COMMANDS.length + 3), ['git:commit', 'lint', 'prompt:software-engineering']);
  const picked = entries[SLASH_COMMANDS.length].item;
  assert.equal(picked.label, '/git:commit');
  assert.deepEqual(picked.auxiliaryData, {type: 'custom', command: catalog.commands[0]});
  assert.deepEqual(ids(rankSlashEntries(entries, 'git', 50)), ['git:commit']);
  assert.deepEqual(ids(rankSlashEntries(entries, 'staged', 50)), ['git:commit'], 'the description is a match term');
  assert.deepEqual(ids(rankSlashEntries(entries, '<message>', 50)), ['git:commit'], 'the argument hint is a match term');
});

const RESOURCES = [
  {server: 'docs', uri: 'docs://guide', name: 'Style guide', title: 'Guide', variables: []},
  {server: 'docs', uriTemplate: 'docs://pages/{name}', name: 'page', variables: ['name']},
];

test('MCP resources follow the MCP prompts as /resource:<server>:<slug>', () => {
  const entries = slashEntries(SLASH_COMMANDS, CATALOG, [], RESOURCES);
  assert.deepEqual(ids(entries.map(entry => entry.item)).slice(-2), ['resource:docs:style-guide', 'resource:docs:page']);
  assert.deepEqual(entries.at(-1).item.auxiliaryData, {type: 'resource', resource: RESOURCES[1]});
  assert.deepEqual(ids(rankSlashEntries(entries, 'resource:', 50)), ['resource:docs:style-guide', 'resource:docs:page']);
  assert.deepEqual(ids(rankSlashEntries(entries, 'docs://pages', 50)), ['resource:docs:page'], 'the URI template is a match term');
  assert.deepEqual(mcpResourceVariableValues(RESOURCES[1], {name: 'intro', other: 'x'}), {name: 'intro'});
  assert.deepEqual(mcpResourceVariableValues(RESOURCES[1], {name: '  '}), {});
});

test('a failing resource load leaves the rest of the menu and is retried', async () => {
  let offline = true;
  let loads = 0;
  const menu = source({
    loadCatalog: async () => CATALOG,
    loadMCPResources: async () => {
      loads += 1;
      if (offline) throw new Error('offline');
      return RESOURCES;
    },
  });
  assert.equal(ids(await menu.search('')).length, SLASH_COMMANDS.length + 4);
  offline = false;
  assert.equal(ids(await menu.search('')).length, SLASH_COMMANDS.length + 6);
  assert.equal(loads, 2);
});
