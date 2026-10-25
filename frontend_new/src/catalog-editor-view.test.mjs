import assert from 'node:assert/strict';
import {test} from 'node:test';

const {CatalogRequestError} = await import('./api.ts');
const {CATALOG_KIND_VIEWS, DISCARD_PROMPT, STALE_FILE_MESSAGE, canSave, deleteFailure, deletePrompt, hasUnsavedWork, isDirty, newIdProblem, normalizeLineBreaks, saveFailure, sortedIds} =
  await import('./catalog-editor-view.ts');

const NEW_PROFILE_TEXT = CATALOG_KIND_VIEWS.profiles.template;

test('a new id must be <category>/<name> with the characters the server accepts', () => {
  assert.equal(newIdProblem('coding/full', []), null);
  assert.equal(newIdProblem('  a.b/c_d-1  ', []), null);
  for (const bad of ['', '   ', 'one', 'a/b/c', '/b', 'a/', 'a b/c', 'a/b c', '-a/b', 'a/.b', 'a/_b', 'a/b:c', 'é/x']) {
    assert.notEqual(newIdProblem(bad, []), null, JSON.stringify(bad));
  }
});

test('a new id that names an existing file is refused', () => {
  assert.match(newIdProblem('jad/default', ['jad/default']), /already exists/);
  assert.match(newIdProblem(' jad/default ', ['jad/default']), /already exists/);
  assert.equal(newIdProblem('jad/other', ['jad/default']), null);
});

test('the id rule names the offending part', () => {
  assert.match(newIdProblem('good/ba d', []), /"ba d"/);
  assert.match(newIdProblem('-x/y', []), /"-x"/);
});

test('the template is a loadable profile body', () => {
  assert.deepEqual(JSON.parse(NEW_PROFILE_TEXT), {schema_version: 1});
});

test('a 412 on replace is a stale file and on create is an existing file', () => {
  const refused = new CatalogRequestError(412, 'profile "a/b" changed since it was read; read it again');
  assert.deepEqual(saveFailure(refused, false), {kind: 'stale', message: STALE_FILE_MESSAGE});
  assert.deepEqual(saveFailure(new CatalogRequestError(412, 'profile "a/b" already exists'), true), {
    kind: 'exists',
    message: 'profile "a/b" already exists',
  });
});

test('a 422 keeps the loader message and anything else is other', () => {
  assert.deepEqual(saveFailure(new CatalogRequestError(422, 'toolset: not available'), false), {
    kind: 'invalid',
    message: 'toolset: not available',
  });
  assert.equal(saveFailure(new CatalogRequestError(428, 'If-Match is required'), false).kind, 'other');
  assert.equal(saveFailure(new CatalogRequestError(413, 'too large'), false).kind, 'other');
  assert.deepEqual(saveFailure(new Error('network down'), false), {kind: 'other', message: 'network down'});
  assert.deepEqual(saveFailure('plain', false), {kind: 'other', message: 'plain'});
});

test('a delete refused with 409 reports the profile as in use with the server text', () => {
  assert.deepEqual(deleteFailure(new CatalogRequestError(409, 'profile "a/b" is needed by session test')), {
    kind: 'in-use',
    message: 'profile "a/b" is needed by session test',
  });
  assert.equal(deleteFailure(new CatalogRequestError(412, 'changed')).kind, 'stale');
  assert.equal(deleteFailure(new CatalogRequestError(404, 'gone')).kind, 'other');
  assert.equal(deleteFailure(new Error('x')).kind, 'other');
});

test('ids sort without changing the input', () => {
  const input = ['test/default', 'jad/test', 'jad/default'];
  assert.deepEqual(sortedIds(input), ['jad/default', 'jad/test', 'test/default']);
  assert.deepEqual(input, ['test/default', 'jad/test', 'jad/default']);
});

test('dirty means the text differs from the saved text', () => {
  assert.equal(isDirty('a', 'a'), false);
  assert.equal(isDirty('a ', 'a'), true);
});

test('a new profile with a typed name is unsaved work even when the template is untouched', () => {
  assert.equal(hasUnsavedWork('new', NEW_PROFILE_TEXT, NEW_PROFILE_TEXT, '', NEW_PROFILE_TEXT), false);
  assert.equal(hasUnsavedWork('new', NEW_PROFILE_TEXT, NEW_PROFILE_TEXT, '  ', NEW_PROFILE_TEXT), false);
  assert.equal(hasUnsavedWork('new', NEW_PROFILE_TEXT, NEW_PROFILE_TEXT, 'a/b', NEW_PROFILE_TEXT), true);
  assert.equal(hasUnsavedWork('new', '{}', NEW_PROFILE_TEXT, '', NEW_PROFILE_TEXT), true);
});

test('an existing profile is unsaved work only when the text changed, and nothing is while loading or empty', () => {
  assert.equal(hasUnsavedWork('existing', 'a', 'a', '', NEW_PROFILE_TEXT), false);
  assert.equal(hasUnsavedWork('existing', 'ab', 'a', '', NEW_PROFILE_TEXT), true);
  assert.equal(hasUnsavedWork('loading', 'x', '', 'a/b', NEW_PROFILE_TEXT), false);
  assert.equal(hasUnsavedWork('none', 'x', '', 'a/b', NEW_PROFILE_TEXT), false);
});

const gate = overrides => ({
  mode: 'existing',
  hasUnsavedWork: true,
  isChecked: true,
  problemCount: 0,
  isBusy: false,
  idProblem: null,
  ...overrides,
});

test('Save needs a change, a finished clean check and no request in flight', () => {
  assert.equal(canSave(gate({})), true);
  assert.equal(canSave(gate({hasUnsavedWork: false})), false);
  assert.equal(canSave(gate({isChecked: false})), false);
  assert.equal(canSave(gate({problemCount: 1})), false);
  assert.equal(canSave(gate({isBusy: true})), false);
});

test('Create works on the untouched template once the name is valid', () => {
  assert.equal(canSave(gate({mode: 'new', hasUnsavedWork: false})), true);
  assert.equal(canSave(gate({mode: 'new', idProblem: 'Enter a name'})), false);
  assert.equal(canSave(gate({mode: 'new', problemCount: 2})), false);
  assert.equal(canSave(gate({mode: 'new', isChecked: false})), false);
});

test('nothing can be saved without an open file', () => {
  assert.equal(canSave(gate({mode: 'none'})), false);
  assert.equal(canSave(gate({mode: 'loading'})), false);
});

test('line breaks are normalised to the editor form', () => {
  assert.equal(normalizeLineBreaks('{\r\n  "a": 1\r\n}\r\n'), '{\n  "a": 1\n}\n');
  assert.equal(normalizeLineBreaks('a\rb\nc'), 'a\nb\nc');
  assert.equal(normalizeLineBreaks('no breaks'), 'no breaks');
});

test('a model template is a loadable model body that still needs its model name', () => {
  const template = JSON.parse(CATALOG_KIND_VIEWS.models.template);
  assert.equal(template.schema_version, 1);
  assert.equal(template.model, '');
});

test('the unsaved check uses the template of the kind being edited', () => {
  const profile = CATALOG_KIND_VIEWS.profiles.template;
  const model = CATALOG_KIND_VIEWS.models.template;
  assert.equal(hasUnsavedWork('new', model, '', '', model), false);
  assert.equal(hasUnsavedWork('new', model, '', '', profile), true);
});

test('the duplicate-name message names the kind', () => {
  assert.match(newIdProblem('a/b', ['a/b'], 'model'), /A model named "a\/b" already exists/);
  assert.match(newIdProblem('a/b', ['a/b']), /A profile named/);
});

test('a session name is <name> or <root>:<name>', () => {
  for (const good of ['demo', 'local:demo', 'a.b_c-1', '  demo  ']) {
    assert.equal(newIdProblem(good, [], 'session', 'session'), null, good);
  }
  for (const bad of ['', '   ', 'a/b', 'a:b:c', ':x', 'x:', '-a', 'a b', 'local:-x', 'é', 'x'.repeat(129)]) {
    assert.notEqual(newIdProblem(bad, [], 'session', 'session'), null, JSON.stringify(bad));
  }
  assert.equal(newIdProblem('x'.repeat(128), [], 'session', 'session'), null);
  assert.match(newIdProblem('local:demo', ['local:demo'], 'session', 'session'), /A session named "local:demo" already exists/);
});

test('every kind has its own hint, delete note and template', () => {
  for (const kind of ['profiles', 'models', 'sessions', 'toolsets', 'mcp']) {
    const view = CATALOG_KIND_VIEWS[kind];
    assert.ok(view.idHint.length > 0 && view.deleteNote.length > 0 && view.noun.length > 0, kind);
    JSON.parse(view.template);
  }
  assert.equal(CATALOG_KIND_VIEWS.sessions.idForm, 'session');
  assert.equal(CATALOG_KIND_VIEWS.profiles.idForm, 'category/name');
  assert.notEqual(CATALOG_KIND_VIEWS.sessions.deleteNote, CATALOG_KIND_VIEWS.profiles.deleteNote);
});

test('a delete from the editor and a delete from the list ask the same question', () => {
  const view = CATALOG_KIND_VIEWS.profiles;
  const question = deletePrompt('profiles', view, {id: 'coding/review', deleteState: false});
  assert.deepEqual(question, {
    title: 'Delete this profile?',
    description: `coding/review will be removed. ${view.deleteNote} Its transcripts and tool state are kept in every session that ran it.`,
    actionLabel: 'Delete',
  });
  // A profile is copied into each session that runs it, so its state is spread over
  // those sessions rather than sitting in a folder of its own. One sentence says so.
  assert.match(
    deletePrompt('profiles', view, {id: 'coding/review', deleteState: true}).description,
    /\. Its transcripts and tool state are deleted in every session that ran it\.$/,
  );
  assert.equal(
    deletePrompt('profiles', view, {id: 'coding/review', deleteState: true}).title,
    question.title,
    'the box changes the consequence, not the question',
  );
  const unnamed = deletePrompt('models', CATALOG_KIND_VIEWS.models, {id: null, deleteState: false});
  assert.equal(unnamed.description.startsWith('This model will be removed. '), true);
});

test('the state box is offered for the two kinds that own state, and nowhere else', () => {
  // Sessions own a state folder; profiles own their share of the state in every
  // session that ran them. Models, toolsets and MCP servers keep none, so a box there
  // would be a control that cannot change anything.
  assert.equal(typeof CATALOG_KIND_VIEWS.sessions.stateNote, 'string');
  assert.equal(typeof CATALOG_KIND_VIEWS.profiles.stateNote, 'string');
  for (const kind of ['models', 'toolsets', 'mcp']) {
    assert.equal(CATALOG_KIND_VIEWS[kind].stateNote, null, kind);
    const view = CATALOG_KIND_VIEWS[kind];
    assert.equal(
      deletePrompt(kind, view, {id: 'a/b', deleteState: true}).description,
      deletePrompt(kind, view, {id: 'a/b', deleteState: false}).description,
      `${kind} has nothing for a tick to change`,
    );
  }
  assert.notEqual(CATALOG_KIND_VIEWS.sessions.stateNote, CATALOG_KIND_VIEWS.profiles.stateNote, 'each says what unticking keeps');
});

test('a session delete mentions its own state folder, and the tick alone decides which way', () => {
  const view = CATALOG_KIND_VIEWS.sessions;
  const kept = deletePrompt('sessions', view, {id: 'demo', deleteState: false});
  assert.equal(kept.title, 'Delete this session?');
  assert.equal(kept.actionLabel, 'Delete');
  assert.match(kept.description, /Its transcripts and tool state are kept\.$/);
  assert.match(deletePrompt('sessions', view, {id: 'demo', deleteState: true}).description, /are deleted too\.$/);
  assert.equal(
    deletePrompt('sessions', view, {id: 'demo', deleteState: true}).title,
    kept.title,
    'the box changes the consequence, not the question',
  );
});

test('the discard question keeps its own wording', () => {
  assert.deepEqual(DISCARD_PROMPT, {
    title: 'Discard your changes?',
    description: 'The text has changes that are not saved.',
    actionLabel: 'Discard',
  });
});
