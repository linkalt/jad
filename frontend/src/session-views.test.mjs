import test from 'node:test';
import assert from 'node:assert/strict';

import {
  addPinnedView,
  profileSelection,
  hasPinnedView,
  groupByCategory,
  isGroupExpanded,
  parseCollapsedGroups,
  sessionListGroups,
  setGroupCollapsed,
  isPinnedViewKey,
  parseStoredViews,
  profileOfViewKey,
  prunePinnedViews,
  removePinnedView,
  restoreViews,
  rowHasRunningRun,
  rowRunStatus,
  sessionListRows,
  sessionRefOfViewKey,
  viewKey,
  viewLabel,
} from './session-views.ts';

const session = (id, title) => ({id, title});

test('a view key names a session, and the profile when it is pinned', () => {
  assert.equal(viewKey('session1'), 'session1');
  assert.equal(viewKey('session1', 'chatgpt'), `session1\u0000chatgpt`);
  // A root-qualified reference keeps its own colon; only the separator is a NUL.
  assert.equal(viewKey('root:session1', 'chatgpt'), `root:session1\u0000chatgpt`);
  assert.equal(sessionRefOfViewKey(`root:session1\u0000chatgpt`), 'root:session1');
  assert.equal(profileOfViewKey(`root:session1\u0000chatgpt`), 'chatgpt');
  assert.equal(sessionRefOfViewKey('session1'), 'session1');
  assert.equal(profileOfViewKey('session1'), undefined);
  assert.equal(isPinnedViewKey('session1'), false);
  assert.equal(isPinnedViewKey(viewKey('session1', 'chatgpt')), true);
});

test('pinning a profile adds one row and never a duplicate', () => {
  const one = addPinnedView([], {sessionRef: 'session1', profile: 'chatgpt'}, 4);
  assert.deepEqual(one, [{sessionRef: 'session1', profile: 'chatgpt'}]);
  assert.deepEqual(addPinnedView(one, {sessionRef: 'session1', profile: 'chatgpt'}, 4), one);
  assert.equal(hasPinnedView(one, {sessionRef: 'session1', profile: 'chatgpt'}), true);
  assert.equal(hasPinnedView(one, {sessionRef: 'session1'}), false);

  const two = addPinnedView(one, {sessionRef: 'session1', profile: 'default'}, 4);
  assert.equal(two.length, 2);
  // The limit holds the list to its configured size.
  assert.deepEqual(addPinnedView(two, {sessionRef: 'session2', profile: 'x'}, 2), two);
});

test('removing a row drops that view and leaves the session row alone', () => {
  const views = [
    {sessionRef: 'session1', profile: 'chatgpt'},
    {sessionRef: 'session1', profile: 'default'},
  ];
  assert.deepEqual(removePinnedView(views, viewKey('session1', 'chatgpt')), [
    {sessionRef: 'session1', profile: 'default'},
  ]);
  // The session's own row is not a pinned view: asking to remove it does nothing.
  assert.deepEqual(removePinnedView(views, 'session1'), views);
  assert.deepEqual(removePinnedView(views, viewKey('session2', 'chatgpt')), views);
});

test('a row whose session disappeared is dropped', () => {
  const views = [
    {sessionRef: 'session1', profile: 'chatgpt'},
    {sessionRef: 'gone', profile: 'chatgpt'},
  ];
  assert.deepEqual(prunePinnedViews(views, ['session1', 'session2']), [
    {sessionRef: 'session1', profile: 'chatgpt'},
  ]);
  assert.deepEqual(prunePinnedViews(views, []), []);
});

test('the list shows each session, then the rows pinned to it', () => {
  const rows = sessionListRows(
    [session('session1', 'One'), session('session2', 'Two')],
    [
      {sessionRef: 'session1', profile: 'chatgpt'},
      {sessionRef: 'session2', profile: 'alternate'},
      {sessionRef: 'session1', profile: 'default'},
    ],
  );
  assert.deepEqual(rows, [
    {key: 'session1', sessionRef: 'session1', label: 'One', isPinned: false},
    {key: `session1\u0000chatgpt`, sessionRef: 'session1', profile: 'chatgpt', label: 'One: chatgpt', isPinned: true},
    {key: `session1\u0000default`, sessionRef: 'session1', profile: 'default', label: 'One: default', isPinned: true},
    {key: 'session2', sessionRef: 'session2', label: 'Two', isPinned: false},
    {key: `session2\u0000alternate`, sessionRef: 'session2', profile: 'alternate', label: 'Two: alternate', isPinned: true},
  ]);
  // A row for a session the list does not know about is not shown at all.
  assert.deepEqual(sessionListRows([session('session2', 'Two')], [{sessionRef: 'gone', profile: 'x'}]).length, 1);
});

test('a view label names the session, and the profile when it is pinned', () => {
  assert.deepEqual(viewLabel('One', 'chatgpt'), 'One: chatgpt');
  assert.equal(viewLabel('  One  '), 'One');
});

test('a row shows the run of its own profile, not the session default', () => {
  const run = {status: 'running'};
  const session = {run: {status: 'completed'}, runsByProfile: {chatgpt: run}};
  const rows = sessionListRows([{id: 'session1', title: 'One'}], [{sessionRef: 'session1', profile: 'chatgpt'}]);
  assert.deepEqual(rowRunStatus(rows[0], session), {status: 'completed'});
  assert.deepEqual(rowRunStatus(rows[1], session), run);
  assert.equal(rowRunStatus(undefined, session), undefined);
  assert.equal(rowRunStatus(rows[0], undefined), undefined);
});

test('run markers stay profile-specific while the session row aggregates pinned runs', () => {
  const rows = sessionListRows(
    [{id: 'session1', title: 'One'}],
    [{sessionRef: 'session1', profile: 'chatgpt'}, {sessionRef: 'session1', profile: 'alternate'}],
  );
  const session = {
    activeProfile: 'default',
    run: {status: 'completed'},
    runsByProfile: {
      default: {status: 'completed'},
      chatgpt: {status: 'running'},
      alternate: {status: 'completed'},
    },
  };

  assert.equal(rowHasRunningRun(rows[0], session, new Set()), true);
  assert.equal(rowHasRunningRun(rows[1], session, new Set()), true);
  assert.equal(rowHasRunningRun(rows[2], session, new Set()), false);
  assert.equal(
    rowHasRunningRun(rows[0], {...session, runsByProfile: {}}, new Set([rows[1].key])),
    true,
  );
  assert.equal(
    rowHasRunningRun(rows[1], {...session, runsByProfile: {}}, new Set([rows[2].key])),
    false,
  );
});

test('a stored arrangement is restored as it was, defensively', () => {
  const stored = {
    views: [{sessionRef: 'session1', profile: 'chatgpt'}],
    activeViewKey: `session1\u0000chatgpt`,
  };
  assert.deepEqual(restoreViews({stored, liveSessionId: 'live', limit: 8}), {
    views: [{sessionRef: 'session1', profile: 'chatgpt'}],
    activeViewKey: `session1\u0000chatgpt`,
    restored: true,
  });
  // A stored selection without pinned rows is still a choice worth restoring.
  assert.deepEqual(
    restoreViews({stored: {views: [], activeViewKey: 'session2'}, liveSessionId: 'live', limit: 8}),
    {views: [], activeViewKey: 'session2', restored: true},
  );
  // Duplicates, junk entries, and an over-long list are rejected.
  assert.deepEqual(
    parseStoredViews(
      {
        views: [
          {sessionRef: 'session1', profile: 'chatgpt'},
          {sessionRef: 'session1', profile: 'chatgpt'},
          {sessionRef: 'session1'},
          {sessionRef: '', profile: 'x'},
          'nope',
        ],
      },
      8,
    ),
    {views: [{sessionRef: 'session1', profile: 'chatgpt'}], activeViewKey: ''},
  );
  assert.equal(parseStoredViews({views: [{sessionRef: 'a', profile: 'p'}, {sessionRef: 'b', profile: 'q'}]}, 1).views.length, 1);
  assert.deepEqual(parseStoredViews(undefined, 8), {views: [], activeViewKey: ''});
  assert.deepEqual(parseStoredViews('garbage', 8), {views: [], activeViewKey: ''});
});

test('a fresh page follows the configured seed, else the live session', () => {
  assert.deepEqual(
    restoreViews({
      seed: [{sessionRef: 'session1', profile: 'chatgpt'}],
      liveSessionId: 'live',
      limit: 8,
    }),
    {views: [{sessionRef: 'session1', profile: 'chatgpt'}], activeViewKey: 'live', restored: false},
  );
  assert.deepEqual(restoreViews({liveSessionId: 'live', limit: 8}), {
    views: [],
    activeViewKey: 'live',
    restored: false,
  });
  // A seed entry without a profile names no view and is ignored.
  assert.deepEqual(
    restoreViews({seed: [{sessionRef: 'a'}, {sessionRef: 'b', profile: 'p'}], liveSessionId: 'live', limit: 8}),
    {views: [{sessionRef: 'b', profile: 'p'}], activeViewKey: 'live', restored: false},
  );
});

test('picking a profile lands on the row on screen', () => {
  const session = {activeProfile: 'default'};
  // The session's own row follows the profile it is given.
  assert.equal(profileSelection({}, 'chatgpt', session), 'session');
  assert.equal(profileSelection({}, 'default', session), 'session');
  // A pinned row shows the pick in that row.
  assert.equal(profileSelection({profile: 'chatgpt'}, 'alternate', session), 'repin');
  assert.equal(profileSelection({profile: 'chatgpt'}, 'chatgpt', session), 'repin');
  // Picking the stored default from a pinned row goes to the session's own row:
  // the same conversation must not exist twice in the list.
  assert.equal(profileSelection({profile: 'chatgpt'}, 'default', session), 'session');
});

const grouped = (id, title, profiles, activeProfile) => ({id, title, profiles, activeProfile});

test('a session with profiles is one group with a child row per profile', () => {
  const [group] = sessionListGroups([grouped('s1', 'Session1', ['p1', 'p2'], 'p1')], []);
  assert.equal(group.sessionRef, 's1');
  assert.deepEqual(
    group.children.map(child => [child.key, child.label, child.profile, child.isPinned]),
    [
      ['s1', 'p1', undefined, false],
      [`s1\u0000p2`, 'p2', 'p2', false],
    ],
  );
});

test('the stored default profile is the session own row, so it keeps following the default', () => {
  const [group] = sessionListGroups([grouped('s1', 'S', ['a', 'b', 'c'], 'b')], []);
  assert.deepEqual(group.children.map(child => child.label), ['a', 'b', 'c']);
  const own = group.children.find(child => child.label === 'b');
  assert.equal(own.key, 's1');
  assert.equal(own.profile, undefined);
});

test('a pinned profile is marked, and a pinned profile the session dropped stays listed last', () => {
  const [group] = sessionListGroups(
    [grouped('s1', 'S', ['p1', 'p2'], 'p1')],
    [{sessionRef: 's1', profile: 'p2'}, {sessionRef: 's1', profile: 'gone'}, {sessionRef: 's2', profile: 'p2'}],
  );
  assert.deepEqual(group.children.map(child => [child.label, child.isPinned]), [
    ['p1', false],
    ['p2', true],
    ['gone', true],
  ]);
});

test('a session without profiles is a group with no children, only its own row', () => {
  const [group] = sessionListGroups([grouped('s1', 'Plain', undefined, undefined)], []);
  assert.deepEqual(group.children, []);
  assert.equal(group.ownRow.key, 's1');
  assert.equal(group.ownRow.label, 'Plain');
});

test('a stored default that is not listed still gets its own child row, first', () => {
  const [group] = sessionListGroups([grouped('s1', 'S', ['p1'], 'other')], []);
  assert.deepEqual(group.children.map(child => [child.key, child.label]), [
    ['s1', 'other'],
    [`s1\u0000p1`, 'p1'],
  ]);
});

test('groups are expanded unless the user collapsed them, even the one holding the active row', () => {
  const groups = sessionListGroups(
    [grouped('s1', 'A', ['p1', 'p2'], 'p1'), grouped('s2', 'B', ['q1', 'q2'], 'q1')],
    [],
  );
  assert.equal(isGroupExpanded(groups[0], []), true);
  assert.equal(isGroupExpanded(groups[0], ['s1']), false);
  assert.equal(isGroupExpanded(groups[0], ['s2']), true);
});

test('collapsing and expanding a group updates the collapsed set without duplicates', () => {
  assert.deepEqual(setGroupCollapsed([], 's1', true), ['s1']);
  assert.deepEqual(setGroupCollapsed(['s1'], 's1', true), ['s1']);
  assert.deepEqual(setGroupCollapsed(['s1', 's2'], 's1', false), ['s2']);
});

test('a stored collapsed set keeps only non-empty strings, once each', () => {
  assert.deepEqual(parseCollapsedGroups(['a', 'a', '', 3, null, 'b']), ['a', 'b']);
  assert.deepEqual(parseCollapsedGroups('nope'), []);
  assert.deepEqual(parseCollapsedGroups(undefined), []);
});

test('an unlisted stored default has one own row, and a view pinned for it keeps its own removable row', () => {
  const [group] = sessionListGroups(
    [grouped('s1', 'S', ['p1'], 'other')],
    [{sessionRef: 's1', profile: 'other'}],
  );
  assert.deepEqual(group.children.map(child => child.key), ['s1', `s1\u0000p1`, `s1\u0000other`]);
});

test('a view pinned for the profile that became the stored default keeps a removable row', () => {
  const [group] = sessionListGroups(
    [grouped('s1', 'S', ['p1', 'p2'], 'p2')],
    [{sessionRef: 's1', profile: 'p2'}],
  );
  assert.deepEqual(group.children.map(child => [child.key, child.isPinned]), [
    [`s1\u0000p1`, false],
    ['s1', false],
    [`s1\u0000p2`, true],
  ]);
});

test('a profile listed twice is one child row', () => {
  const [group] = sessionListGroups([grouped('s1', 'S', ['p1', 'p2', 'p2'], 'p1')], []);
  assert.deepEqual(group.children.map(child => child.label), ['p1', 'p2']);
});

const inCategory = (id, category) => ({...grouped(id, id, ['p'], 'p'), category});

test('a session carries its category into the group, and an empty one counts as none', () => {
  const groups = sessionListGroups([inCategory('a', 'work'), inCategory('b', ''), grouped('c', 'c', ['p'], 'p')], []);
  assert.deepEqual(groups.map(group => group.category), ['work', undefined, undefined]);
});

test('sessions are split by category in first-seen order, uncategorised last', () => {
  const groups = sessionListGroups(
    [inCategory('a', 'work'), grouped('n', 'n', ['p'], 'p'), inCategory('b', 'test'), inCategory('c', 'work')],
    [],
  );
  assert.deepEqual(
    groupByCategory(groups).map(section => [section.category, section.groups.map(group => group.sessionRef)]),
    [['work', ['a', 'c']], ['test', ['b']], [undefined, ['n']]],
  );
});

test('no sessions give no sections, and no categories give a single unnamed section', () => {
  assert.deepEqual(groupByCategory([]), []);
  const [only, ...rest] = groupByCategory(sessionListGroups([grouped('a', 'a', ['p'], 'p')], []));
  assert.equal(only.category, undefined);
  assert.equal(only.groups.length, 1);
  assert.equal(rest.length, 0);
});
