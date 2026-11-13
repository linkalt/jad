import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import {after, test} from 'node:test';

const originalFetch = globalThis.fetch;
const requests = [];
let reply = () => new Response('{}', {status: 200});
globalThis.fetch = async (input, init = {}) => {
  requests.push({url: String(input), init});
  return reply();
};
const {fetchRootLibrary, fetchRootLibraryAsset, expandRootLibraryCommand, fetchWorkspaces, parseWorkspaces} = await import('./api.ts');
const {
  catalogScope,
  effectiveWorkspace,
  groupByWorkspace,
  hasSharedScopeChoice,
  isWorkspaceLayout,
  libraryWorkspace,
  newSessionNote,
  newSessionRef,
  UNAVAILABLE_WORKSPACE_NOTE,
  workspaceOfSessionRef,
  workspaceOptions,
  workspacesOf,
} = await import('./workspaces.ts');
const {sessionListGroups} = await import('./session-views.ts');

after(() => {
  globalThis.fetch = originalFetch;
});

const json = (value, status = 200) => new Response(JSON.stringify(value), {status});
const read = name => readFileSync(new URL(name, import.meta.url), 'utf8');
const row = (id, rootId, rootLabel, extra = {}) => ({id, title: id, rootId, rootLabel, profiles: ['a/p'], ...extra});
const names = sections => sections.map(section => [section.workspace?.label, section.groups.map(group => group.sessionRef)]);

const listed = [
  {id: 'beta', label: 'Beta'},
  {id: 'start', label: 'Start here', start: true},
  {id: 'alpha', label: 'Alpha'},
  {id: 'down', label: 'Down', unavailable: 'the folder is gone'},
];

test('the workspaces are the served listed ones, the start workspace first and the others by label', () => {
  assert.deepEqual(workspacesOf(listed, []).map(workspace => workspace.id), ['start', 'alpha', 'beta']);
});

test('a listed workspace that is not served cannot hold a session or be chosen', () => {
  assert.equal(workspacesOf(listed, []).some(workspace => workspace.id === 'down'), false);
  assert.equal(effectiveWorkspace(workspacesOf(listed, []), 'down')?.id, 'start');
});

test('a listed workspace that is not served is shown greyed out, never chosen, and never with the server\'s reason', () => {
  const served = workspacesOf(listed, []);
  assert.deepEqual(workspaceOptions(served, listed), [
    {value: 'start', label: 'Start here'},
    {value: 'alpha', label: 'Alpha'},
    {value: 'beta', label: 'Beta'},
    {value: 'down', label: 'Down', disabled: true, description: UNAVAILABLE_WORKSPACE_NOTE},
  ]);
  // The reason is an error text with file and folder names; it must not reach the screen.
  const withPath = [...listed, {id: 'gone', label: 'Gone', unavailable: '/home/me/gone/.jad/workspace.json: no such file'}];
  assert.equal(JSON.stringify(workspaceOptions(workspacesOf(withPath, []), withPath)).includes('/home/me'), false);
  // Outside the workspace layout nothing is listed, so nothing is greyed out.
  assert.deepEqual(workspaceOptions([{id: 'a', label: 'A'}], []), [{value: 'a', label: 'A'}]);
  // A served workspace is never also shown as unavailable.
  assert.equal(workspaceOptions(served, [...listed, {id: 'alpha', label: 'Alpha', unavailable: 'x'}]).filter(option => option.value === 'alpha').length, 1);
});

test('the rail switcher is built from the served and the listed workspaces', () => {
  const nav = read('components/SessionNavigation.tsx');
  assert.match(nav, /workspaceOptions\(workspaces, listedWorkspaces\)/);
  assert.match(read('App.tsx'), /listedWorkspaces=\{workspaceList\.workspaces\}/);
});

test('outside the workspace layout the workspaces are the session roots, with their labels', () => {
  const sessions = [row('b:s1', 'b', 'Second'), row('a:s2', 'a', 'First'), row('a:s3', 'a', 'First')];
  assert.deepEqual(workspacesOf([], sessions), [{id: 'a', label: 'First'}, {id: 'b', label: 'Second'}]);
  assert.equal(isWorkspaceLayout([]), false);
  assert.equal(isWorkspaceLayout(listed), true);
});

test('the old layout\'s default root keeps its label and gives one flat list', () => {
  const sessions = [row('s1', 'local', 'Local sessions'), row('s2', 'local', 'Local sessions')];
  const workspaces = workspacesOf([], sessions);
  assert.deepEqual(workspaces, [{id: 'local', label: 'Local sessions'}]);
  const sections = groupByWorkspace(sessionListGroups(sessions), workspaces);
  assert.deepEqual(names(sections), [[undefined, ['s1', 's2']]]);
});

test('a root with no label shows its id, never a path', () => {
  assert.deepEqual(workspacesOf([], [row('x', 'proj', '')]), [{id: 'proj', label: 'proj'}]);
});

test('one workspace is a flat list without a heading, and no sessions give no section', () => {
  const [only] = workspacesOf(listed.slice(1, 2), []);
  assert.deepEqual(names(groupByWorkspace(sessionListGroups([row('s', 'start', 'x')]), [only])), [[undefined, ['s']]]);
  assert.deepEqual(groupByWorkspace([], [only]), []);
});

test('several workspaces give one section each, in order, with the sessions in list order', () => {
  const sessions = [
    row('beta:b1', 'beta', 'Beta'),
    row('start:s1', 'start', 'Start here'),
    row('beta:b2', 'beta', 'Beta'),
    row('start:s2', 'start', 'Start here'),
  ];
  const sections = groupByWorkspace(sessionListGroups(sessions), workspacesOf(listed, sessions));
  assert.deepEqual(names(sections), [
    ['Start here', ['start:s1', 'start:s2']],
    ['Alpha', []],
    ['Beta', ['beta:b1', 'beta:b2']],
  ]);
});

test('a session whose root is not listed gets a section of its own instead of vanishing', () => {
  const workspaces = workspacesOf(listed, []);
  const sections = groupByWorkspace(sessionListGroups([row('lost:s', 'lost', 'Lost'), row('start:s', 'start', 'S')]), workspaces);
  assert.deepEqual(names(sections).at(-1), ['lost', ['lost:s']]);
  const orphan = groupByWorkspace(sessionListGroups([{id: 'old', title: 'old', profiles: ['a/p']}]), workspaces);
  assert.deepEqual(names(orphan).at(-1), ['Unknown workspace', ['old']]);
});

test('a session with no root keeps its place when it is the only kind there is', () => {
  const sessions = [{id: 'old', title: 'old', profiles: ['a/p']}];
  const workspaces = workspacesOf([], sessions);
  assert.deepEqual(names(groupByWorkspace(sessionListGroups(sessions), workspaces)), [[undefined, ['old']]]);
});

test('a session row maps its root and its label', () => {
  const mapping = read('session-mapping.ts');
  assert.match(mapping, /snapshot\.root_id \? \{rootId: snapshot\.root_id\}/);
  assert.match(mapping, /snapshot\.root_label \? \{rootLabel: snapshot\.root_label\}/);
  assert.equal(mapping.includes('category'), false);
});

test('the chosen workspace is the pick if it is served, else the start workspace, else the first', () => {
  const workspaces = workspacesOf(listed, []);
  assert.equal(effectiveWorkspace(workspaces, 'beta')?.id, 'beta');
  assert.equal(effectiveWorkspace(workspaces, undefined)?.id, 'start');
  assert.equal(effectiveWorkspace(workspaces, 'nope')?.id, 'start');
  assert.equal(effectiveWorkspace(workspacesOf([], [row('x', 'only', 'Only')]), undefined)?.id, 'only');
  assert.equal(effectiveWorkspace([], undefined), undefined);
});

test('a catalog request carries an explicit scope in the workspace layout, and none outside it', () => {
  for (const kind of ['profiles', 'models', 'toolsets', 'mcp', 'sessions']) {
    assert.equal(catalogScope(kind, false, 'proj', 'global'), undefined, `old layout, ${kind}`);
  }
  assert.equal(catalogScope('profiles', true, 'proj', 'global'), 'workspace:proj');
  assert.equal(catalogScope('models', true, 'proj', 'global'), 'global');
  assert.equal(catalogScope('toolsets', true, 'proj', 'workspace'), 'workspace:proj');
  assert.equal(catalogScope('mcp', true, 'proj', 'workspace'), 'global');
  assert.equal(catalogScope('sessions', true, 'proj', 'workspace'), undefined);
  assert.equal(hasSharedScopeChoice('models') && hasSharedScopeChoice('toolsets'), true);
  assert.equal(hasSharedScopeChoice('mcp') || hasSharedScopeChoice('profiles'), false);
});

test('switching the workspace changes the scope of its profiles', () => {
  assert.notEqual(catalogScope('profiles', true, 'a', 'global'), catalogScope('profiles', true, 'b', 'global'));
});

test('a new session goes to the chosen workspace, unless its name names another', () => {
  assert.equal(newSessionRef('work', 'proj', 2), 'proj:work');
  assert.equal(newSessionRef('  work ', 'proj', 3), 'proj:work');
  assert.equal(newSessionRef('other:work', 'proj', 2), 'other:work');
  assert.equal(newSessionRef('work', 'proj', 1), 'work');
  assert.equal(newSessionRef('work', undefined, 2), 'work');
  assert.equal(newSessionRef('', 'proj', 2), '');
});

test('the note under the name says which workspace gets the session', () => {
  const workspaces = workspacesOf(listed, []);
  assert.equal(newSessionNote('work', 'beta', workspaces), 'Created as beta:work in workspace “Beta”.');
  assert.equal(newSessionNote('alpha:work', 'beta', workspaces), 'Created as alpha:work in workspace “Alpha”.');
  assert.equal(newSessionNote('', 'beta', workspaces), undefined);
  assert.equal(newSessionNote('work', 'start', workspaces.slice(0, 1)), undefined);
});

test('the workspace of a reference is its root part', () => {
  assert.equal(workspaceOfSessionRef('proj:s'), 'proj');
  assert.equal(workspaceOfSessionRef('s'), undefined);
});

test('a library request names the session\'s workspace only in the workspace layout', () => {
  assert.equal(libraryWorkspace(true, 'proj'), 'proj');
  assert.equal(libraryWorkspace(false, 'proj'), undefined);
  assert.equal(libraryWorkspace(true, undefined), undefined);
  assert.equal(libraryWorkspace(true, ''), undefined);
});

test('the library calls carry the workspace in the query, and none without one', async () => {
  requests.length = 0;
  reply = () => json({prompts: [], templates: [], skills: [], commands: []});
  await fetchRootLibrary(undefined, 'a b');
  await fetchRootLibrary();
  reply = () => json({asset: {id: 'x', kind: 'prompt', name: 'n', description: '', source: 's', content: 'c'}});
  await fetchRootLibraryAsset('prompt', 'x', undefined, 'w');
  reply = () => json({command: 'c', text: 't'});
  await expandRootLibraryCommand('c', '', 'w');
  assert.deepEqual(requests.map(request => request.url.replace(/^.*\/api/, '')), [
    '/library?workspace=a%20b',
    '/library',
    '/library/prompt/x?workspace=w',
    '/library/command/c/expand?workspace=w',
  ]);
});

test('the workspace list is read without its directory, labels default to the id, and bad entries are dropped', () => {
  const parsed = parseWorkspaces({
    workspaces: [
      {id: 'a', label: ' Alpha ', directory: '/srv/secret/a', start: true},
      {id: 'b', label: '', directory: '/srv/b'},
      {id: 'c', label: 'C', unavailable: 'gone'},
      {label: 'no id'},
      {id: '', label: 'empty'},
      7,
    ],
  });
  assert.deepEqual(parsed, [
    {id: 'a', label: 'Alpha', start: true},
    {id: 'b', label: 'b'},
    {id: 'c', label: 'C', unavailable: 'gone'},
  ]);
  assert.equal(JSON.stringify(parsed).includes('/srv'), false);
  assert.throws(() => parseWorkspaces({}), /invalid workspaces/);
});

test('an old layout or an older server answers no workspaces, and a failing server is an error, not an empty list', async () => {
  reply = () => json({workspaces: []});
  assert.deepEqual(await fetchWorkspaces(), []);
  reply = () => new Response('not found', {status: 404});
  assert.deepEqual(await fetchWorkspaces(), []);
  reply = () => json({error: 'boom'}, 500);
  await assert.rejects(fetchWorkspaces(), /boom/);
});

test('the catalog dialog sends its scope with every file request and refuses to list before the workspaces are known', () => {
  const dialog = read('components/CatalogEditorDialog.tsx');
  assert.equal(/fetchCatalogFile\(kind, id\)/.test(dialog), false, 'an unscoped read');
  assert.equal(/fetchCatalogFiles\(target, signal\)/.test(dialog), false, 'an unscoped list');
  assert.match(dialog, /fetchCatalogFile\(kind, id, undefined, scopeOf\(kind\)\)/);
  assert.match(dialog, /fetchCatalogFiles\(target, signal, scopeOf\(target\)\)/);
  assert.match(dialog, /saveCatalogFile\(kind, id, text, [^;]*scopeOf\(kind\)\)/);
  assert.equal((dialog.match(/deleteCatalogFile\(kind, [^;]*scope: scopeOf\(kind\)\}\)/g) ?? []).length, 2);
  assert.match(dialog, /if \(!isOpen \|\| !isWorkspaceReady\) return undefined/);
  assert.match(dialog, /const id = isCreate \? createdId : draft\.id/);
});

test('the rail, the dialog and the panes are given the workspace, and no category is read anywhere in the UI', () => {
  const app = read('App.tsx');
  assert.match(app, /<SessionNavigation[^>]*\n\s*groups=\{listGroups\}\n\s*workspaces=\{workspaces\}/);
  assert.match(app, /libraryWorkspace=\{libraryWorkspace\(libraryLayout, pane\.session\?\.rootId\)\}/);
  assert.match(app, /selectedWorkspace=\{selectedWorkspace\}/);
  for (const name of ['App.tsx', 'session-views.ts', 'session-mapping.ts', 'components/SessionNavigation.tsx', 'components/CatalogListPane.tsx', 'components/CatalogEditorDialog.tsx']) {
    assert.equal(/session\.category|\.category\b.*session|categorySectionLabel|groupByCategory/.test(read(name)), false, name);
  }
});
