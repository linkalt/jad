import assert from 'node:assert/strict';
import {existsSync} from 'node:fs';
import test from 'node:test';

import {
  BUILTIN_MENTION_FILES,
  commandTrigger,
  createComposerTriggers,
  createSessionFileSource,
  mentionItems,
  rankMentionFiles,
  resolveMentionFiles,
} from './composer-triggers.ts';

const FILES = [
  'frontend/src/App.tsx',
  'frontend/src/components/ComposerPane.tsx',
  'backend/src/tools/native/edit.ts',
  'frontend/src/row-heights.ts',
];

test('a configured list replaces the built-in one only once it has entries', () => {
  assert.deepEqual(resolveMentionFiles([], BUILTIN_MENTION_FILES), BUILTIN_MENTION_FILES);
  assert.deepEqual(resolveMentionFiles(undefined, ['a.ts']), ['a.ts']);
  assert.deepEqual(resolveMentionFiles(['x.ts'], ['a.ts']), ['x.ts']);
});

test('an empty query keeps the list order and respects the cap', () => {
  assert.deepEqual(rankMentionFiles(FILES, '', 10), FILES);
  assert.deepEqual(rankMentionFiles(FILES, '  ', 2), FILES.slice(0, 2));
});

test('a file name beats the directories it sits in', () => {
  assert.deepEqual(rankMentionFiles(FILES, 'composer', 10), [
    'frontend/src/components/ComposerPane.tsx',
  ]);
  // 'native' only appears in a directory, so the match still works but ranks last.
  assert.deepEqual(rankMentionFiles(FILES, 'native', 10), ['backend/src/tools/native/edit.ts']);
});

test('an exact path wins, and matching is case-insensitive', () => {
  assert.deepEqual(rankMentionFiles(FILES, 'frontend/src/row-heights.ts', 10), [
    'frontend/src/row-heights.ts',
  ]);
  assert.deepEqual(rankMentionFiles(FILES, 'TSX', 10), [
    'frontend/src/App.tsx',
    'frontend/src/components/ComposerPane.tsx',
  ]);
});

test('a query with no match returns nothing, and the cap applies to matches', () => {
  assert.deepEqual(rankMentionFiles(FILES, 'zzz', 10), []);
  assert.deepEqual(rankMentionFiles(FILES, 'ts', 2), [
    'frontend/src/App.tsx',
    'frontend/src/components/ComposerPane.tsx',
  ]);
});

test('the menu items carry the path as both id and label', () => {
  assert.deepEqual(mentionItems(FILES, 'row', 10), [
    {id: 'frontend/src/row-heights.ts', label: 'frontend/src/row-heights.ts'},
  ]);
});

test('the @ trigger searches the list, bootstraps it, and inserts a path chip', () => {
  const [trigger] = createComposerTriggers({files: FILES, maxResults: 2});
  assert.equal(trigger.character, '@');
  assert.equal(trigger.menuLabel, 'Files');
  assert.equal(trigger.emptySearchResultsText, 'No matching files');
  assert.deepEqual(trigger.searchSource.bootstrap(), [
    {id: 'frontend/src/App.tsx', label: 'frontend/src/App.tsx'},
    {id: 'frontend/src/components/ComposerPane.tsx', label: 'frontend/src/components/ComposerPane.tsx'},
  ]);
  assert.deepEqual(trigger.searchSource.search('row'), [
    {id: 'frontend/src/row-heights.ts', label: 'frontend/src/row-heights.ts'},
  ]);
  // The chip shows the path; the serialized value is the reference itself.
  assert.deepEqual(trigger.onSelect({id: 'frontend/src/App.tsx', label: 'frontend/src/App.tsx'}), {
    value: '@frontend/src/App.tsx',
    label: 'frontend/src/App.tsx',
    variant: 'blue',
  });
});

test('an empty command list leaves the slash menu out entirely', () => {
  assert.deepEqual(createComposerTriggers({files: FILES, maxResults: 5}).length, 1);
  assert.deepEqual(createComposerTriggers({files: FILES, maxResults: 5, commands: []}).length, 1);
});

test('commands add a slash trigger that matches aliases and inserts a yellow chip', () => {
  const triggers = createComposerTriggers({
    files: FILES,
    maxResults: 5,
    commands: [
      {name: 'compact', keywords: ['shrink', 'summarise']},
      {name: 'clear'},
    ],
  });
  assert.equal(triggers.length, 2);
  const command = triggers[1];
  assert.equal(command.character, '/');
  assert.deepEqual(
    command.searchSource.search('shrink').map(item => item.id),
    ['compact'],
  );
  assert.deepEqual(
    command.searchSource.bootstrap().map(item => item.label),
    ['/compact', '/clear'],
  );
  assert.deepEqual(command.onSelect({id: 'compact', label: '/compact'}), {
    value: '/compact',
    label: '/compact',
    variant: 'yellow',
  });
});

test('the command trigger alone also caps its menu', () => {
  const trigger = commandTrigger([{name: 'a'}, {name: 'b'}, {name: 'c'}], 2);
  assert.deepEqual(
    trigger.searchSource.bootstrap().map(item => item.id),
    ['a', 'b'],
  );
});

test('every built-in path is relative, unique, and exists in this repository', () => {
  assert.ok(BUILTIN_MENTION_FILES.length > 0);
  assert.equal(new Set(BUILTIN_MENTION_FILES).size, BUILTIN_MENTION_FILES.length);
  assert.deepEqual(
    BUILTIN_MENTION_FILES.filter(path => path.startsWith('/')),
    [],
  );
  // The list is checked in for this repository, so a stale entry is a bug the
  // test can catch: the paths are resolved two levels up from frontend/src.
  const repoRoot = new URL('../../', import.meta.url);
  assert.deepEqual(
    BUILTIN_MENTION_FILES.filter(path => !existsSync(new URL(path, repoRoot))),
    [],
  );
});

test('the session source lists what its loader returns, and filters it locally', async () => {
  let loads = 0;
  const source = createSessionFileSource({
    loadFiles: async () => {
      loads += 1;
      return FILES;
    },
    fallbackFiles: ['fallback.ts'],
    maxResults: 10,
  });

  assert.deepEqual((await source.bootstrap()).map(item => item.id), FILES);
  assert.deepEqual(
    (await source.search('composer')).map(item => item.id),
    ['frontend/src/components/ComposerPane.tsx'],
  );
  assert.equal(loads, 1, 'search answers from the list bootstrap fetched');
});

test('every menu open asks the server again, so new files appear', async () => {
  let version = 1;
  const source = createSessionFileSource({
    loadFiles: async () => [`v${version}.ts`],
    fallbackFiles: [],
    maxResults: 10,
  });

  assert.deepEqual((await source.bootstrap()).map(item => item.id), ['v1.ts']);
  version = 2;
  assert.deepEqual((await source.bootstrap()).map(item => item.id), ['v2.ts']);
});

test('a search before bootstrap still loads once', async () => {
  let loads = 0;
  const source = createSessionFileSource({
    loadFiles: async () => {
      loads += 1;
      return FILES;
    },
    fallbackFiles: [],
    maxResults: 10,
  });

  assert.deepEqual(
    (await source.search('row-heights')).map(item => item.id),
    ['frontend/src/row-heights.ts'],
  );
  assert.equal(loads, 1);
});

test('a failing listing falls back to the static list instead of an empty menu', async () => {
  let offline = true;
  const source = createSessionFileSource({
    loadFiles: async () => {
      if (offline) {
        throw new Error('listing unavailable');
      }
      return FILES;
    },
    fallbackFiles: ['fallback.ts'],
    maxResults: 10,
  });

  assert.deepEqual((await source.bootstrap()).map(item => item.id), ['fallback.ts']);
  offline = false;
  assert.deepEqual((await source.bootstrap()).map(item => item.id), FILES);
});

test('cancel aborts the request in flight and the menu keeps a usable list', async () => {
  let seen;
  const source = createSessionFileSource({
    loadFiles: signal => {
      seen = signal;
      return new Promise((_resolve, reject) => {
        signal.addEventListener('abort', () => reject(new Error('aborted')), {once: true});
      });
    },
    fallbackFiles: ['fallback.ts'],
    maxResults: 10,
  });

  const pending = source.bootstrap();
  assert.equal(seen.aborted, false);
  source.cancel();
  assert.equal(seen.aborted, true);
  assert.deepEqual((await pending).map(item => item.id), ['fallback.ts']);
});

test('the results cap applies to a fetched list too', async () => {
  const source = createSessionFileSource({
    loadFiles: async () => FILES,
    fallbackFiles: [],
    maxResults: 2,
  });
  assert.deepEqual((await source.bootstrap()).map(item => item.id), FILES.slice(0, 2));
});

test('a trigger built over a session source reports its loading state', async () => {
  const triggers = createComposerTriggers({
    files: ['static.ts'],
    maxResults: 10,
    sessionFiles: async () => FILES,
  });
  const mention = triggers.find(trigger => trigger.character === '@');
  assert.ok(mention);
  assert.equal(mention.loadingText, 'Loading files…');
  assert.deepEqual((await mention.searchSource.bootstrap()).map(item => item.id), FILES);
});

test('without a session loader the trigger stays on the static list', async () => {
  const triggers = createComposerTriggers({files: FILES, maxResults: 10});
  const mention = triggers.find(trigger => trigger.character === '@');
  assert.deepEqual((await mention.searchSource.bootstrap()).map(item => item.id), FILES);
});
