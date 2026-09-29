import assert from 'node:assert/strict';
import test from 'node:test';

import {
  buildEditedDraft,
  canExtendDraft,
  draftDeadlineLabel,
  draftExtendLabel,
  draftFields,
  hasAddedFields,
  hasDraftChanges,
  hasDraftEdits,
  reviewDraftAnswer,
} from './draft-answer.ts';

const DRAFT = {
  goal: 'Make the page clearer',
  rounds: 2,
  aggressive: false,
  notes: null,
  scope: {area: 'landing', keepUrls: true},
  steps: ['audit the copy', 'rewrite the intro'],
  reviewers: [{name: 'sam', role: 'editor'}],
};

test('every scalar becomes a row, nested objects read as paths', () => {
  const fields = draftFields(DRAFT);
  assert.deepEqual(
    fields.map((field) => [field.label, field.kind, field.text]),
    [
      ['goal', 'string', 'Make the page clearer'],
      ['rounds', 'number', '2'],
      ['aggressive', 'boolean', 'false'],
      ['notes', 'null', ''],
      ['scope.area', 'string', 'landing'],
      ['scope.keepUrls', 'boolean', 'true'],
      ['steps[1]', 'string', 'audit the copy'],
      ['steps[2]', 'string', 'rewrite the intro'],
      ['reviewers', 'json', '[\n  {\n    "name": "sam",\n    "role": "editor"\n  }\n]'],
    ],
  );
});

test('rows the form cannot express are edited as JSON, and a key cannot collide with a path', () => {
  const fields = draftFields({'a.b': 'flat', a: {b: 'nested'}, list: [[1, 2]]});
  assert.deepEqual(
    fields.map((field) => [field.label, field.text]),
    [
      ['a.b', 'flat'],
      ['a.b', 'nested'],
      ['list', '[\n  [\n    1,\n    2\n  ]\n]'],
    ],
  );
  // Same label, different keys: the key is the path, so React and edits stay exact.
  assert.notEqual(fields[0].key, fields[1].key);
});

test('a draft whose fields the form does not show survives untouched', () => {
  assert.deepEqual(buildEditedDraft({empty: [], nested: {keep: [1, {a: true}]}}, {}), {
    value: {empty: [], nested: {keep: [1, {a: true}]}},
  });
});

test('edited rows rebuild the proposal with their own type', () => {
  const edits = {
    [JSON.stringify(['goal'])]: 'Rewrite the intro only',
    [JSON.stringify(['rounds'])]: '5',
    [JSON.stringify(['aggressive'])]: 'true',
    [JSON.stringify(['scope', 'keepUrls'])]: 'false',
    [JSON.stringify(['steps', 1])]: 'audit the tone',
    [JSON.stringify(['reviewers'])]: '[{"name":"sam"}]',
  };
  assert.deepEqual(buildEditedDraft(DRAFT, edits), {
    value: {
      ...DRAFT,
      goal: 'Rewrite the intro only',
      rounds: 5,
      aggressive: true,
      scope: {area: 'landing', keepUrls: false},
      steps: ['audit the copy', 'audit the tone'],
      reviewers: [{name: 'sam'}],
    },
  });
  assert.deepEqual(DRAFT.steps, ['audit the copy', 'rewrite the intro'], 'the proposal is never mutated');
});

test('a row that no longer parses names itself instead of throwing', () => {
  const cases = [
    ['rounds', 'two', 'rounds must be a number'],
    ['aggressive', 'yes', 'aggressive must be true or false'],
    ['notes', 'something', 'notes must stay empty'],
    ['reviewers', '[{oops}', 'reviewers must be valid JSON'],
  ];
  for (const [label, text, expectation] of cases) {
    const built = buildEditedDraft(DRAFT, {[JSON.stringify([label])]: text});
    assert.ok('error' in built, `${label} should be refused`);
    assert.equal(`${built.error.label} ${built.error.message}`, expectation);
  }
});

test('edits are recognised only when the text differs', () => {
  const fields = draftFields(DRAFT);
  const goal = fields.find((field) => field.label === 'goal');
  assert.ok(goal);
  assert.equal(hasDraftEdits(fields, {}), false);
  assert.equal(hasDraftEdits(fields, {[goal.key]: goal.text}), false, 'typing the same text is not an edit');
  assert.equal(hasDraftEdits(fields, {[goal.key]: 'other'}), true);
});

test('an answer carries the draft it proceeds with, or the reason it does not', () => {
  const fields = draftFields(DRAFT);
  assert.deepEqual(
    reviewDraftAnswer({draft: DRAFT, fields, edits: {}, decision: 'accept', reason: '', reasonMaxLength: 2000}),
    {body: {decision: 'accept'}},
  );
  assert.deepEqual(
    reviewDraftAnswer({
      draft: DRAFT,
      fields,
      edits: {[JSON.stringify(['goal'])]: 'Rewrite the intro'},
      decision: 'update',
      reason: '',
      reasonMaxLength: 2000,
    }),
    {body: {decision: 'update', draft: {...DRAFT, goal: 'Rewrite the intro'}}},
  );
  assert.deepEqual(
    reviewDraftAnswer({
      draft: DRAFT,
      fields,
      edits: {},
      decision: 'decline',
      reason: '  too broad  ',
      reasonMaxLength: 2000,
    }),
    {body: {decision: 'decline', reason: 'too broad'}},
  );
  assert.deepEqual(
    reviewDraftAnswer({draft: DRAFT, fields, edits: {}, decision: 'decline', reason: '   ', reasonMaxLength: 2000}),
    {error: 'Say why you are declining.'},
  );
  assert.deepEqual(
    reviewDraftAnswer({
      draft: DRAFT,
      fields,
      edits: {},
      decision: 'decline',
      reason: 'x'.repeat(2001),
      reasonMaxLength: 2000,
    }),
    {error: 'Keep the reason under 2000 characters.'},
  );
  assert.deepEqual(
    reviewDraftAnswer({
      draft: DRAFT,
      fields,
      edits: {[JSON.stringify(['rounds'])]: 'many'},
      decision: 'update',
      reason: '',
      reasonMaxLength: 2000,
    }),
    {error: 'rounds must be a number.'},
  );
});

test('the drawer says how long the run keeps waiting', () => {
  const now = Date.parse('2026-09-29T10:00:00.000Z');
  assert.equal(draftDeadlineLabel('2026-09-29T10:05:00.000Z', now), '5 minutes left');
  assert.equal(draftDeadlineLabel('2026-09-29T10:01:00.000Z', now), '1 minute left');
  assert.equal(draftDeadlineLabel('2026-09-29T10:00:30.000Z', now), 'less than a minute left');
  assert.equal(draftDeadlineLabel('2026-09-29T09:59:59.000Z', now), 'the wait is over');
  assert.equal(draftDeadlineLabel('2026-09-29T12:00:00.000Z', now), '2 hours left');
  assert.equal(draftDeadlineLabel('not a date', now), '');
});

test('the drawer labels the extension step and stops at the ceiling', () => {
  assert.equal(draftExtendLabel(300_000), '+5 min');
  assert.equal(draftExtendLabel(90_000), '+1 min');
  assert.equal(draftExtendLabel(30_000), '+30 sec');
  assert.equal(draftExtendLabel(0), '');
  assert.equal(draftExtendLabel(-5_000), '');

  const now = Date.parse('2026-09-29T10:00:00.000Z');
  assert.equal(canExtendDraft('2026-09-29T10:05:00.000Z', '2026-09-29T10:30:00.000Z', now), true);
  assert.equal(
    canExtendDraft('2026-09-29T10:30:00.000Z', '2026-09-29T10:30:00.000Z', now),
    false,
    'a question at the longest wait cannot be extended',
  );
  assert.equal(
    canExtendDraft('2026-09-29T09:59:00.000Z', '2026-09-29T10:30:00.000Z', now),
    false,
    'an ended wait cannot be extended',
  );
  assert.equal(canExtendDraft('not a date', '2026-09-29T10:30:00.000Z', now), false);
});

const added = (name, kind, text) => ({id: `added-${name}`, name, kind, text});

test('a field the user adds is written at the root with the type they chose', () => {
  const built = buildEditedDraft(
    DRAFT,
    {[JSON.stringify(['rounds'])]: '3'},
    [
      added('deadline', 'string', 'Friday'),
      added('weight', 'number', '2.5'),
      added('safe', 'boolean', 'true'),
      added('options', 'json', '{"a":[1,2]}'),
    ],
  );
  assert.deepEqual(built, {
    value: {
      ...DRAFT,
      rounds: 3,
      deadline: 'Friday',
      weight: 2.5,
      safe: true,
      options: {a: [1, 2]},
    },
  });
});

test('an added row the user opened and left empty is not a change', () => {
  assert.deepEqual(buildEditedDraft(DRAFT, {}, [added('', 'string', '')]), {value: DRAFT});
  assert.equal(hasAddedFields([added('', 'string', '')]), false);
  assert.equal(hasDraftChanges(draftFields(DRAFT), {}, [added('', 'string', '')]), false);
});

test('adding a field makes the answer an update, so the button cannot drop it', () => {
  const fields = draftFields(DRAFT);
  assert.equal(hasDraftChanges(fields, {}, [added('deadline', 'string', 'Friday')]), true);
  assert.equal(hasDraftChanges(fields, {}, [added('', 'string', 'Friday')]), true, 'a value with no name yet is still a change');
  assert.deepEqual(
    reviewDraftAnswer({
      draft: DRAFT,
      fields,
      edits: {},
      added: [added('deadline', 'string', 'Friday')],
      decision: 'update',
      reason: '',
      reasonMaxLength: 2000,
    }),
    {body: {decision: 'update', draft: {...DRAFT, deadline: 'Friday'}}},
  );
});

test('a new field that cannot be applied says which rule it broke', () => {
  const cases = [
    [added('', 'string', 'Friday'), 'New field needs a name'],
    [added('a.b', 'string', 'x'), 'a.b must be a single key, without dots or brackets'],
    [added('scope[0]', 'string', 'x'), 'scope[0] must be a single key, without dots or brackets'],
    [added('goal', 'string', 'x'), 'goal already exists in the draft'],
    [added('count', 'number', 'many'), 'count must be a number'],
    [added('safe', 'boolean', 'yes'), 'safe must be true or false'],
    [added('options', 'json', '{oops}'), 'options must be valid JSON'],
  ];
  for (const [field, expectation] of cases) {
    const built = buildEditedDraft(DRAFT, {}, [field]);
    assert.ok('error' in built, `${JSON.stringify(field)} should be refused`);
    assert.equal(`${built.error.label} ${built.error.message}`, expectation);
  }
});

test('an added field cannot collide with another added field', () => {
  const built = buildEditedDraft(DRAFT, {}, [
    added('deadline', 'string', 'Friday'),
    {...added('deadline', 'string', 'Monday'), id: 'added-second'},
  ]);
  assert.ok('error' in built);
  assert.match(built.error.message, /already exists/);
});
