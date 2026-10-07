import assert from 'node:assert/strict';
import test from 'node:test';

import {
  buildEditedDraft,
  addListItem,
  canExtendDraft,
  draftDeadlineLabel,
  editListItem,
  isListEdited,
  listItemsOf,
  moveListItem,
  removeListItem,
  retypeListItem,
  updateListItemForm,
  draftExtendLabel,
  draftFields,
  hasAddedFields,
  hasDraftChanges,
  hasDraftEdits,
  hasJsonDraftChanges,
  reviewDraftAnswer,
  reviewDraftJsonAnswer,
} from './draft-answer.ts';

const field = (draft, label) => draftFields(draft).find((entry) => entry.label === label);

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
      ['steps', 'list', '[\n  "audit the copy",\n  "rewrite the intro"\n]'],
      ['reviewers', 'list', '[\n  {\n    "name": "sam",\n    "role": "editor"\n  }\n]'],
    ],
  );
});

test('an array is one field whose entries keep the type the proposal gave them', () => {
  const fields = draftFields({mix: ['a', 2, true, null, {k: 1}, [3]], empty: []});
  const mix = fields.find((field) => field.label === 'mix');
  assert.ok(mix);
  assert.deepEqual(
    mix.items.map((item) => [item.id, item.kind, item.text]),
    [
      ['0', 'string', 'a'],
      ['1', 'number', '2'],
      ['2', 'boolean', 'true'],
      ['3', 'null', ''],
      ['4', 'object', '{\n  "k": 1\n}'],
      ['5', 'json', '[\n  3\n]'],
    ],
  );
  assert.deepEqual(mix.items[4].form.base, {k: 1}, 'an object entry carries a form, one input per key');
  assert.equal(mix.items[5].form, undefined, 'a nested array stays one JSON entry');
  assert.equal(draftFields({o: [{}]})[0].items[0].kind, 'json', 'an object with no keys has nothing to show as a form');
  const empty = fields.find((field) => field.label === 'empty');
  assert.deepEqual([empty.kind, empty.items], ['list', []]);
  assert.equal(fields.filter((field) => field.label.startsWith('mix[')).length, 0, 'no row per index');
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
  assert.equal(fields[2].items[0].kind, 'json', 'a nested array is one JSON entry');
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
  };
  const stepsKey = JSON.stringify(['steps']);
  const reviewersKey = JSON.stringify(['reviewers']);
  const lists = {
    [stepsKey]: editListItem(field(DRAFT, 'steps').items, 1, 'audit the tone'),
    [reviewersKey]: updateListItemForm(field(DRAFT, 'reviewers').items, 0, {
      edits: {[JSON.stringify(['role'])]: 'lead'},
    }),
  };
  assert.deepEqual(buildEditedDraft(DRAFT, edits, [], lists), {
    value: {
      ...DRAFT,
      goal: 'Rewrite the intro only',
      rounds: 5,
      aggressive: true,
      scope: {area: 'landing', keepUrls: false},
      steps: ['audit the copy', 'audit the tone'],
      reviewers: [{name: 'sam', role: 'lead'}],
    },
  });
  assert.deepEqual(DRAFT.steps, ['audit the copy', 'rewrite the intro'], 'the proposal is never mutated');
});

test('a row that no longer parses names itself instead of throwing', () => {
  const cases = [
    ['rounds', 'two', 'rounds must be a number'],
    ['aggressive', 'yes', 'aggressive must be true or false'],
    ['notes', 'something', 'notes must stay empty'],
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

test('the full JSON editor can remove fields and change nested or array structure', () => {
  const changed = {goal: 'Keep only the requested item', scope: {include: ['one']}, steps: []};
  const text = JSON.stringify(changed, null, 2);
  assert.deepEqual(reviewDraftJsonAnswer(text), {body: {decision: 'update', draft: changed}});
  assert.equal(hasJsonDraftChanges(text, DRAFT), true);
  assert.equal(hasJsonDraftChanges(JSON.stringify(DRAFT, null, 2), DRAFT), false);
  assert.deepEqual(reviewDraftJsonAnswer('{invalid'), {error: 'Draft must be valid JSON.'});
  assert.deepEqual(reviewDraftJsonAnswer('[]'), {error: 'Draft must be a JSON object.'});
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

test('adding a __proto__ field preserves it as JSON data without mutating the prototype', () => {
  const built = buildEditedDraft({}, {}, [added('__proto__', 'json', '{"safe":true}')]);
  assert.ok('value' in built);
  assert.deepEqual(JSON.parse(JSON.stringify(built.value)), JSON.parse('{"__proto__":{"safe":true}}'));
  assert.equal(Object.getPrototypeOf(built.value), Object.prototype);
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

test('an array that was not touched is not an edit and keeps its exact value', () => {
  const draft = {steps: ['a', 'b'], empty: [], objects: [{x: 1}]};
  const fields = draftFields(draft);
  const steps = fields.find((entry) => entry.label === 'steps');
  assert.equal(isListEdited(steps, steps.items), false);
  assert.equal(hasDraftEdits(fields, {}, {[steps.key]: steps.items}), false);
  assert.deepEqual(buildEditedDraft(draft, {}, [], {[steps.key]: steps.items}), {value: draft});
  // Retyping the same text over an entry is not an edit either.
  assert.equal(isListEdited(steps, editListItem(steps.items, 0, 'a')), false);
});

test('entries can be added, removed and reordered, and the draft follows', () => {
  const draft = {steps: ['audit', 'rewrite', 'ship']};
  const steps = field(draft, 'steps');
  const key = steps.key;
  const build = (items) => buildEditedDraft(draft, {}, [], {[key]: items});

  assert.deepEqual(build(moveListItem(steps.items, 2, -1)), {value: {steps: ['audit', 'ship', 'rewrite']}});
  assert.deepEqual(build(moveListItem(steps.items, 0, 1)), {value: {steps: ['rewrite', 'audit', 'ship']}});
  assert.deepEqual(build(removeListItem(steps.items, 1)), {value: {steps: ['audit', 'ship']}});
  const added = editListItem(addListItem(steps.items, 'item-1'), 3, 'review');
  assert.deepEqual(build(added), {value: {steps: ['audit', 'rewrite', 'ship', 'review']}});
  assert.equal(hasDraftEdits([steps], {}, {[key]: added}), true);
  assert.deepEqual(build([]), {value: {steps: []}}, 'removing every entry leaves an empty array');
  assert.deepEqual(DRAFT.steps, ['audit the copy', 'rewrite the intro'], 'the proposal is never mutated');
});

test('moving past either end changes nothing', () => {
  const steps = field({steps: ['a', 'b']}, 'steps');
  assert.deepEqual(moveListItem(steps.items, 0, -1), steps.items);
  assert.deepEqual(moveListItem(steps.items, 1, 1), steps.items);
  assert.deepEqual(moveListItem(steps.items, 5, 1), steps.items);
  assert.equal(isListEdited(steps, moveListItem(steps.items, 0, -1)), false);
});

test('a reorder is an edit even when the text is unchanged', () => {
  const steps = field({steps: ['same', 'same']}, 'steps');
  assert.equal(isListEdited(steps, moveListItem(steps.items, 0, 1)), true, 'ids differ, so the order changed');
});

test('an added entry takes the type of the entry before it and can be retyped', () => {
  const numbers = field({n: [1, 2]}, 'n');
  const grown = addListItem(numbers.items, 'item-1');
  assert.equal(grown[2].kind, 'number');
  assert.equal(grown[2].added, true);
  assert.deepEqual(buildEditedDraft({n: [1, 2]}, {}, [], {[numbers.key]: editListItem(grown, 2, '3')}), {
    value: {n: [1, 2, 3]},
  });

  const empty = field({tags: []}, 'tags');
  const first = addListItem(empty.items, 'item-2');
  assert.equal(first[0].kind, 'string', 'an empty list starts with text');
  const asBool = editListItem(retypeListItem(first, 0, 'boolean'), 0, 'true');
  assert.deepEqual(buildEditedDraft({tags: []}, {}, [], {[empty.key]: asBool}), {value: {tags: [true]}});
  const asJson = editListItem(retypeListItem(first, 0, 'json'), 0, '{"a":1}');
  assert.deepEqual(buildEditedDraft({tags: []}, {}, [], {[empty.key]: asJson}), {value: {tags: [{a: 1}]}});

  const original = retypeListItem(numbers.items, 0, 'string');
  assert.equal(original[0].kind, 'number', 'an entry the proposal had keeps its type');
});

test('an entry that no longer parses names the array and its position', () => {
  const numbers = field({n: [1, 2]}, 'n');
  const bad = buildEditedDraft({n: [1, 2]}, {}, [], {[numbers.key]: editListItem(numbers.items, 1, 'two')});
  assert.ok('error' in bad);
  assert.equal(`${bad.error.label} ${bad.error.message}`, 'n item 2 must be a number');
  const blank = buildEditedDraft({n: [1]}, {}, [], {
    [numbers.key]: addListItem(numbers.items, 'item-1'),
  });
  assert.ok('error' in blank, 'a new empty number is refused, not sent as 0');
  const json = field({o: [[1]]}, 'o');
  const broken = buildEditedDraft({o: [[1]]}, {}, [], {[json.key]: editListItem(json.items, 0, '{oops')});
  assert.equal(`${broken.error.label} ${broken.error.message}`, 'o item 1 must be valid JSON');
});

const PEOPLE = {
  people: [
    {name: 'sam', age: 30, admin: true, tags: ['a'], address: {city: 'Oslo'}},
    {name: 'kim', age: 41, admin: false, tags: [], address: {city: 'Rome'}},
  ],
};
const peopleKey = JSON.stringify(['people']);
const personKey = (...path) => JSON.stringify(path);

test('an array of objects is edited one input per key, never as JSON text', () => {
  const people = field(PEOPLE, 'people');
  assert.deepEqual(people.items.map((item) => item.kind), ['object', 'object']);
  const labels = draftFields(people.items[0].form.base).map((entry) => [entry.label, entry.kind]);
  assert.deepEqual(labels, [
    ['name', 'string'],
    ['age', 'number'],
    ['admin', 'boolean'],
    ['tags', 'list'],
    ['address.city', 'string'],
  ]);
  assert.equal(isListEdited(people, people.items), false, 'an untouched form is not an edit');
  assert.deepEqual(buildEditedDraft(PEOPLE, {}, [], {[peopleKey]: people.items}), {value: PEOPLE});
});

test('editing a key of one entry changes that entry and no other', () => {
  const people = field(PEOPLE, 'people');
  const edited = updateListItemForm(people.items, 1, {
    edits: {[personKey('age')]: '42', [personKey('address', 'city')]: 'Milan'},
  });
  assert.equal(isListEdited(people, edited), true);
  assert.deepEqual(buildEditedDraft(PEOPLE, {}, [], {[peopleKey]: edited}), {
    value: {
      people: [
        PEOPLE.people[0],
        {name: 'kim', age: 42, admin: false, tags: [], address: {city: 'Milan'}},
      ],
    },
  });
  assert.equal(PEOPLE.people[1].age, 41, 'the proposal is never mutated');
  assert.equal(hasDraftChanges(draftFields(PEOPLE), {}, [], {[peopleKey]: edited}), true);
});

test('typing the original text over a key of an object entry is not an edit', () => {
  const people = field(PEOPLE, 'people');
  const same = updateListItemForm(people.items, 0, {edits: {[personKey('name')]: 'sam'}});
  assert.equal(isListEdited(people, same), false);
});

test('an array inside an object entry is edited as a list of its own', () => {
  const people = field(PEOPLE, 'people');
  const tagsKey = personKey('tags');
  const tags = field(people.items[0].form.base, 'tags');
  const grown = editListItem(addListItem(tags.items, 'item-9'), 1, 'b');
  const edited = updateListItemForm(people.items, 0, {lists: {[tagsKey]: grown}});
  assert.equal(isListEdited(people, edited), true);
  assert.deepEqual(buildEditedDraft(PEOPLE, {}, [], {[peopleKey]: edited}).value.people[0].tags, ['a', 'b']);
});

test('an object entry that cannot be built names the entry and the key', () => {
  const people = field(PEOPLE, 'people');
  const bad = updateListItemForm(people.items, 1, {edits: {[personKey('age')]: 'old'}});
  const built = buildEditedDraft(PEOPLE, {}, [], {[peopleKey]: bad});
  assert.ok('error' in built);
  assert.equal(`${built.error.label} ${built.error.message}`, 'people item 2 age must be a number');
  const review = reviewDraftAnswer({
    draft: PEOPLE,
    fields: draftFields(PEOPLE),
    edits: {},
    added: [],
    lists: {[peopleKey]: bad},
    decision: 'update',
    reason: '',
    reasonMaxLength: 100,
  });
  assert.deepEqual(review, {error: 'people item 2 age must be a number.'});
});

test('a new entry after an object entry is an object with the same keys, all blank', () => {
  const people = field(PEOPLE, 'people');
  const grown = addListItem(people.items, 'item-1');
  assert.equal(grown[2].kind, 'object');
  assert.equal(grown[2].added, true);
  assert.deepEqual(grown[2].form.base, {name: '', age: 0, admin: false, tags: [], address: {city: ''}});
  const filled = updateListItemForm(grown, 2, {
    edits: {[personKey('name')]: 'ola', [personKey('age')]: '25'},
  });
  assert.deepEqual(buildEditedDraft(PEOPLE, {}, [], {[peopleKey]: filled}).value.people[2], {
    name: 'ola',
    age: 25,
    admin: false,
    tags: [],
    address: {city: ''},
  });
});

test('a reorder or a removal of object entries carries each entry with its own edits', () => {
  const people = field(PEOPLE, 'people');
  const edited = updateListItemForm(people.items, 0, {edits: {[personKey('name')]: 'sam2'}});
  const swapped = moveListItem(edited, 0, 1);
  assert.deepEqual(
    buildEditedDraft(PEOPLE, {}, [], {[peopleKey]: swapped}).value.people.map((person) => person.name),
    ['kim', 'sam2'],
  );
  assert.deepEqual(
    buildEditedDraft(PEOPLE, {}, [], {[peopleKey]: removeListItem(edited, 1)}).value.people.map((person) => person.name),
    ['sam2'],
  );
});

test('arrays inside nested objects are edited at their own path', () => {
  const draft = {scope: {include: ['one'], name: 'x'}};
  const include = field(draft, 'scope.include');
  assert.deepEqual(include.path, ['scope', 'include']);
  const filled = editListItem(addListItem(include.items, 'item-1'), 1, 'two');
  assert.deepEqual(buildEditedDraft(draft, {}, [], {[include.key]: filled}), {
    value: {scope: {include: ['one', 'two'], name: 'x'}},
  });
});

test('an answer carries an edited array as an update and lists stay out of the way otherwise', () => {
  const draft = {goal: 'g', steps: ['a']};
  const fields = draftFields(draft);
  const steps = fields.find((entry) => entry.label === 'steps');
  const review = (lists) =>
    reviewDraftAnswer({draft, fields, edits: {}, added: [], lists, decision: 'update', reason: '', reasonMaxLength: 100});
  assert.deepEqual(review({[steps.key]: editListItem(addListItem(steps.items, 'item-1'), 1, 'b')}), {
    body: {decision: 'update', draft: {goal: 'g', steps: ['a', 'b']}},
  });
  assert.deepEqual(review(undefined), {body: {decision: 'update', draft}});
  assert.ok('error' in review({[steps.key]: addListItem(steps.items, 'item-1')}) === false, 'a blank text entry is a valid empty string');
  assert.equal(hasDraftChanges(fields, {}, [], {[steps.key]: steps.items}), false);
  assert.equal(listItemsOf(steps, {}), steps.items);
});
