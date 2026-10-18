import test from 'node:test';
import assert from 'node:assert/strict';

import {
  allProfileRows,
  filterProfileRows,
  hasProfileToSwitchTo,
  profileChoiceRows,
} from './profile-rows.ts';

// The rail's rows for one session with a stored default profile: the session's
// own row (no profile path) plus the profiles pinned to it.
const rows = [
  {key: 'jad', label: 'Jad Harness Project'},
  {key: 'jad\u0000test', label: 'Testing', profile: 'test'},
  {key: 'jad\u0000review', label: 'Reviewer', profile: 'review'},
];

test('the drawer offers every profile of the session', () => {
  const expanded = profileChoiceRows(rows, 'jad', '', true);
  assert.deepEqual(
    expanded.map(row => row.row.key),
    ['jad', 'jad\u0000test', 'jad\u0000review'],
    'the stored default row comes first, in the rail order',
  );
  assert.deepEqual(
    expanded.map(row => row.isSelected),
    [true, false, false],
    'only the conversation on screen is marked',
  );
});

test('collapsed, the drawer is the current profile alone', () => {
  const collapsed = profileChoiceRows(rows, 'jad\u0000test', '', false);
  assert.deepEqual(collapsed.map(row => row.row.key), ['jad\u0000test'], 'one row: where the pane is now');
  assert.equal(collapsed[0].isSelected, true);
  // A pane restored against a profile the session no longer lists must not open
  // an empty drawer: it falls back to the first row, unmarked.
  const fallback = profileChoiceRows(rows, 'elsewhere', '', false);
  assert.deepEqual(fallback.map(row => row.row.key), ['jad'], 'the first row, so the drawer has a body');
  assert.equal(fallback[0].isSelected, false, 'and it is not marked as the current profile');
  assert.equal(profileChoiceRows([], 'jad', '', false).length, 0, 'a session with no rows shows nothing');
});

test('the list holds every session, its own row first then its profiles', () => {
  const groups = [
    {
      sessionRef: 'jad',
      title: 'Jad Harness Project',
      children: [
        {key: 'jad', label: 'Default'},
        {key: 'jad\u0000jad/test', label: 'Testing', profile: 'jad/test'},
      ],
    },
    {
      sessionRef: 'test',
      title: 'Testing session',
      children: [{key: 'test\u0000test/test', label: 'Test', profile: 'test/test'}],
    },
  ];
  const all = allProfileRows(groups);
  assert.deepEqual(
    all.map(row => row.key),
    ['jad', 'jad\u0000jad/test', 'test', 'test\u0000test/test'],
    'per session in list order; the own row is added for a session that lacks one',
  );
  assert.deepEqual(
    all.map(row => row.label),
    ['Default', 'Testing', 'Testing session', 'Test'],
    'the own row keeps the rail\'s label (Default) instead of repeating the session title',
  );
  assert.deepEqual(
    all.map(row => row.session),
    ['Jad Harness Project', 'Jad Harness Project', 'Testing session', 'Testing session'],
    'every row names the session it belongs to',
  );
  assert.equal(all[0].profile, undefined, 'the own row follows the stored default');
  assert.equal(all[3].profile, 'test/test');
  assert.equal(new Set(all.map(row => row.key)).size, all.length, 'a key appears once');
});

test('a row already on screen in another pane is marked, not offered', () => {
  // The pane shows 'jad' (its own row); 'jad\u0000test' is open in another pane.
  const choice = profileChoiceRows(rows, 'jad', '', true, new Set(['jad\u0000test', 'jad']));
  assert.deepEqual(
    choice.map(row => [row.row.key, row.isSelected, row.isOpenElsewhere]),
    [
      ['jad', true, false],
      ['jad\u0000test', false, true],
      ['jad\u0000review', false, false],
    ],
    'the pane\'s own row stays selectable (a press folds the list), the other pane\'s row is disabled',
  );
  // Without a set of open views nothing is disabled.
  assert.deepEqual(
    profileChoiceRows(rows, 'jad', '', true).map(row => row.isOpenElsewhere),
    [false, false, false],
  );
  // Collapsed, the row the pane is on is never disabled.
  const collapsed = profileChoiceRows(rows, 'jad\u0000review', '', false, new Set(['jad\u0000review']));
  assert.deepEqual(collapsed.map(row => [row.isSelected, row.isOpenElsewhere]), [[true, false]]);
  // Nor is a collapsed fallback row another pane happens to hold: it is the only
  // way into the list, so disabling it would leave the drawer unopenable.
  const fallback = profileChoiceRows(rows, 'somewhere-else', '', false, new Set(['jad']));
  assert.deepEqual(fallback.map(row => [row.row.key, row.isOpenElsewhere]), [['jad', false]]);
});

test('the search matches the label, the session and the path', () => {
  const groups = [
    {
      sessionRef: 'jad',
      title: 'Jad Harness Project',
      children: [{key: 'jad\u0000jad/test', label: 'Testing', profile: 'jad/test'}],
    },
    {
      sessionRef: 'test',
      title: 'Testing session',
      children: [{key: 'test\u0000test/test', label: 'Test', profile: 'test/test'}],
    },
  ];
  const all = allProfileRows(groups);
  assert.deepEqual(filterProfileRows(all, 'harness').map(row => row.key), ['jad', 'jad\u0000jad/test'], 'by session name');
  assert.deepEqual(filterProfileRows(all, 'jad/test').map(row => row.key), ['jad\u0000jad/test'], 'by profile path');
  assert.deepEqual(filterProfileRows(all, 'test').map(row => row.key), ['jad\u0000jad/test', 'test', 'test\u0000test/test'], 'by label too');
  assert.equal(filterProfileRows(all, '   ').length, all.length, 'a blank query is no filter');
  assert.notEqual(filterProfileRows(all, ''), all, 'the filtered list is a copy');
});

test('the own row carries the profile the server reports as the stored default', () => {
  const groups = [
    {
      sessionRef: 'jad',
      title: 'Jad Harness Project',
      defaultProfile: 'jad/default',
      children: [
        {key: 'jad', label: 'Default'},
        {key: 'jad\u0000jad/test', label: 'Testing', profile: 'jad/test'},
      ],
    },
    {
      sessionRef: 'atlas',
      title: 'Atlas onboarding flow',
      children: [{key: 'atlas\0atlas/main', label: 'Main', profile: 'atlas/main'}],
    },
  ];
  const all = allProfileRows(groups);
  assert.deepEqual(
    all.map(row => [row.key, row.profile, row.defaultProfile]),
    [
      ['jad', undefined, 'jad/default'],
      ['jad\u0000jad/test', 'jad/test', undefined],
      ['atlas', undefined, undefined],
      ['atlas\u0000atlas/main', 'atlas/main', undefined],
    ],
    'the own row names no profile of its own, so the server keeps resolving the default; it shows that default\'s id',
  );
  // Searching by the default's id finds the own row.
  assert.deepEqual(filterProfileRows(all, 'jad/default').map(row => row.key), ['jad']);
});

test('a list holding only the current conversation offers no control', () => {
  const only = [{key: 'jad', label: 'Jad Harness Project', session: 'Jad Harness Project'}];
  assert.equal(hasProfileToSwitchTo(only, 'jad'), false);
  assert.equal(hasProfileToSwitchTo(rows, 'jad'), true, 'another row exists');
  assert.equal(hasProfileToSwitchTo([], 'jad'), false, 'no rows, no control');
});
