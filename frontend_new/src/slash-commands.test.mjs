import assert from 'node:assert/strict';
import test from 'node:test';

import {
  SLASH_COMMANDS,
  createSlashSource,
  librarySlug,
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
  assert.deepEqual(ids(entries.map(entry => entry.item)).slice(3), ['prompt:same', 'prompt:same-2']);
});

test('the selection carries what the app needs to run or fetch the entry', () => {
  const entries = slashEntries(SLASH_COMMANDS, CATALOG);
  assert.deepEqual(entries[0].item.auxiliaryData, {type: 'command', name: 'compact'});
  assert.deepEqual(entries[3].item.auxiliaryData, {
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
  assert.equal(rankSlashEntries(entries, '', 50).length, 7);
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
  assert.equal(ids(await menu.search('')).length, 7);
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
  assert.equal(ids(await menu.search('')).length, 7);
  catalog = {...CATALOG, prompts: [...CATALOG.prompts, summary('prompt', 'p3', 'Fresh')]};
  clock = 999;
  assert.equal(ids(await menu.search('')).length, 7);
  clock = 1000;
  assert.ok(ids(await menu.search('')).includes('prompt:fresh'));
  assert.equal(loads, 2);
});

test('a failing library load leaves the three commands usable and is retried', async () => {
  let offline = true;
  const menu = source({
    loadCatalog: async () => {
      if (offline) throw new Error('offline');
      return CATALOG;
    },
  });
  assert.deepEqual(ids(await menu.search('')), ['compact', 'state', 'reset']);
  assert.deepEqual(ids(await menu.search('res')), ['reset']);
  offline = false;
  assert.equal(ids(await menu.search('')).length, 7);
});

test('the source answers a promise, so Astryx takes its async path and waits for the load', () => {
  const menu = source({loadCatalog: async () => CATALOG});
  assert.ok(menu.search('') instanceof Promise);
});
