import assert from 'node:assert/strict';
import test from 'node:test';

import {forkSessionRef} from './session-fork.ts';

test('forkSessionRef keeps the root, adds the suffix and a stamp, and stays a valid id', () => {
  assert.equal(forkSessionRef('local:demo', '-fork-', 36 ** 2), 'local:demo-fork-100');
  assert.equal(forkSessionRef('demo', '-fork-', 35), 'demo-fork-z');
  const long = forkSessionRef('local:' + 'a'.repeat(200), '-fork-', 1);
  const id = long.slice('local:'.length);
  assert.equal(id.length, 128);
  assert.match(id, /^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$/);
  assert.ok(id.endsWith('-fork-1'));
});

test('a fork after an answer names the user message that started its turn', async () => {
  const {forkPointMessage} = await import('./session-fork.ts');
  const messages = [
    {id: 'session-user-o0', sender: 'user'},
    {id: 'resp-1', sender: 'assistant'},
    {id: 'session-user-o90', sender: 'user'},
    {id: 'resp-1', sender: 'assistant'},
  ];
  assert.equal(forkPointMessage(messages, messages[3], true), messages[2]);
  assert.equal(forkPointMessage(messages, messages[1], true), messages[0]);
  // A fork before a message, or a user message, names the message itself.
  assert.equal(forkPointMessage(messages, messages[3], false), messages[3]);
  assert.equal(forkPointMessage(messages, messages[2], true), messages[2]);
  // A turn of the compacted history, or no user message before: the answer itself.
  const compacted = [{id: 'history-user-1', sender: 'user'}, {id: 'resp-2', sender: 'assistant'}];
  assert.equal(forkPointMessage(compacted, compacted[1], true), compacted[1]);
  assert.equal(forkPointMessage([messages[1]], messages[1], true), messages[1]);
});

test('a profile fork proposes a name in the same category and checks the names it is given', async () => {
  const {forkProfileName, forkNameProblem, sessionRefInRootOf, sessionIdOf} = await import('./session-fork.ts');
  assert.equal(forkProfileName('dev/coder', '-fork-', 36), 'coder-fork-10');
  assert.equal(forkProfileName('coder', '-fork-', 0), 'coder-fork-0');
  const long = forkProfileName('dev/' + 'b'.repeat(300), '-fork-', 1);
  assert.equal(long.length, 200);
  assert.equal(forkNameProblem(long, 'profile'), null);
  assert.equal(forkNameProblem('coder-try', 'profile'), null);
  assert.match(forkNameProblem('', 'profile'), /Enter a profile name/);
  assert.match(forkNameProblem('../x', 'session'), /Letters, digits/);
  assert.match(forkNameProblem('-x', 'profile'), /starting with a letter or digit/);
  assert.match(forkNameProblem('a'.repeat(129), 'session'), /At most 128/);
  assert.equal(sessionRefInRootOf('local:demo', 'copy'), 'local:copy');
  assert.equal(sessionRefInRootOf('demo', 'copy'), 'copy');
  assert.equal(sessionIdOf('local:demo-fork-1'), 'demo-fork-1');
});

test('the fork buttons open the name dialog and fork into a profile; the row menu duplicates', async () => {
  const {readFile} = await import('node:fs/promises');
  const app = await readFile(new URL('./App.tsx', import.meta.url), 'utf8');
  const menu = await readFile(new URL('./components/RowContextMenu.tsx', import.meta.url), 'utf8');
  const editor = await readFile(new URL('./components/CatalogEditorDialog.tsx', import.meta.url), 'utf8');
  assert.match(app, /<ForkDialog\b/);
  assert.match(app, /setPendingFork\(\{\s*request: \{\s*mode: 'fork',\s*kind: 'profile'/);
  assert.match(app, /\{profile: answer\.name/);
  assert.match(app, /onDuplicate=\{duplicateRow\}/);
  assert.ok(!/forkSession\(target\.sessionRef, \{session: newRef/.test(app), 'a message fork no longer makes a new session');
  assert.match(menu, /<Tooltip content="Duplicate" placement="end">/);
  assert.match(menu, /target\.kind === 'category' \|\| onDuplicate === undefined \|\| canDuplicate\?\.\(target\) === false \? null/);
  assert.match(editor, /<Button label="Duplicate"/);
});

test('answers of the compacted history cannot be forked after their turn', async () => {
  const {answersOfCompactedTurns} = await import('./session-fork.ts');
  const messages = [
    {id: 'resp-0', sender: 'assistant'},
    {id: 'session-user-c12', sender: 'user'},
    {id: 'resp-1', sender: 'assistant'},
    {id: 'resp-1b', sender: 'assistant'},
    {id: 'session-user-o0', sender: 'user'},
    {id: 'resp-2', sender: 'assistant'},
  ];
  assert.deepEqual([...answersOfCompactedTurns(messages)], ['resp-1', 'resp-1b'], 'an answer with no user message before it stays forkable');
});

test('only a catalog profile can be forked or duplicated', async () => {
  const {isForkableProfile} = await import('./session-fork.ts');
  for (const profile of ['main/coder', 'a/b']) assert.equal(isForkableProfile(profile), true, profile);
  for (const profile of [undefined, '', 'coder', '/coder', 'main/']) assert.equal(isForkableProfile(profile), false, String(profile));
});

test('the transcript and the row menu hide fork and Duplicate where the server refuses', async () => {
  const {readFileSync} = await import('node:fs');
  const view = readFileSync(new URL('./components/ViewPane.tsx', import.meta.url), 'utf8');
  const app = readFileSync(new URL('./App.tsx', import.meta.url), 'utf8');
  const menu = readFileSync(new URL('./components/RowContextMenu.tsx', import.meta.url), 'utf8');
  assert.match(view, /onFork=\{isFrontCompactionSelectionMode \|\| unforkableAnswers\.has\(message\.id\) \? undefined : forkMessage\}/);
  assert.match(app, /if \(effectiveProfile !== undefined && !isForkableProfile\(effectiveProfile\)\) return undefined;/);
  assert.match(app, /canDuplicate=\{canDuplicateRow\}/);
  assert.match(menu, /canDuplicate\?\.\(target\) === false \? null :/);
});
