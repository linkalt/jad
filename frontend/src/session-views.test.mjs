import test from 'node:test';
import assert from 'node:assert/strict';

import {
  addPinnedView,
  profileLabel,
  hasPinnedView,
  groupByCategory,
  isGroupExpanded,
  parseCollapsedGroups,
  sessionListGroups,
  setGroupCollapsed,
  isPinnedViewKey,
  parseStoredViews,
  profileOfViewKey,
  activeKeyAfterPrune,
  nextRecentViewKeys,
  prunePinnedViews,
  retainedPinnedViewKeys,
  restoreViews,
  rowHasRunningRun,
  rowRunStatus,
  rowTurnCount,
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
  const one = addPinnedView([], {sessionRef: 'session1', profile: 'chatgpt'});
  assert.deepEqual(one, [{sessionRef: 'session1', profile: 'chatgpt'}]);
  assert.deepEqual(addPinnedView(one, {sessionRef: 'session1', profile: 'chatgpt'}), one);
  assert.equal(hasPinnedView(one, {sessionRef: 'session1', profile: 'chatgpt'}), true);
  assert.equal(hasPinnedView(one, {sessionRef: 'session1'}), false);

  const two = addPinnedView(one, {sessionRef: 'session1', profile: 'default'});
  assert.equal(two.length, 2);
  // There is no cap: a third row is added too.
  assert.equal(addPinnedView(two, {sessionRef: 'session2', profile: 'x'}).length, 3);
});

test('a row whose session disappeared is dropped', () => {
  const views = [
    {sessionRef: 'session1', profile: 'chatgpt'},
    {sessionRef: 'gone', profile: 'chatgpt'},
  ];
  assert.deepEqual(prunePinnedViews(views, [{id: 'session1'}, {id: 'session2'}]), [
    {sessionRef: 'session1', profile: 'chatgpt'},
  ]);
  assert.deepEqual(prunePinnedViews(views, []), []);
});

test('a row whose profile the session dropped, or made its default, is dropped', () => {
  const views = [
    {sessionRef: 's1', profile: 'p1'},
    {sessionRef: 's1', profile: 'p2'},
    {sessionRef: 's1', profile: 'gone'},
    {sessionRef: 's2', profile: 'x'},
  ];
  assert.deepEqual(
    prunePinnedViews(views, [{id: 's1', profiles: ['p1', 'p2'], activeProfile: 'p2'}, {id: 's2'}]),
    [{sessionRef: 's1', profile: 'p1'}, {sessionRef: 's2', profile: 'x'}],
  );
  // A session whose profiles are not known keeps its rows.
  assert.deepEqual(prunePinnedViews(views.slice(0, 1), [{id: 's1', profiles: []}]), views.slice(0, 1));
});

test('an active profile row whose pin was pruned falls back to its session row', () => {
  const known = new Set(['s1']);
  const kept = [{sessionRef: 's1', profile: 'p1'}];
  assert.equal(activeKeyAfterPrune(viewKey('s1', 'p2'), kept, known), 's1');
  assert.equal(activeKeyAfterPrune(viewKey('s1', 'p1'), kept, known), viewKey('s1', 'p1'));
  assert.equal(activeKeyAfterPrune('s1', [], known), 's1');
  assert.equal(activeKeyAfterPrune(viewKey('gone', 'p1'), [], known), viewKey('gone', 'p1'));
});

test('the visit order puts the active pinned view first and forgets dropped pins', () => {
  const a = viewKey('s1', 'a');
  const b = viewKey('s1', 'b');
  const c = viewKey('s1', 'c');
  const pinned = new Set([a, b, c]);
  let recent = nextRecentViewKeys([], a, pinned);
  recent = nextRecentViewKeys(recent, b, pinned);
  recent = nextRecentViewKeys(recent, c, pinned);
  assert.deepEqual(recent, [c, b, a]);
  assert.deepEqual(nextRecentViewKeys(recent, a, pinned), [a, c, b]);
  // A session's own row is not a pin: it leads nothing and does not displace one.
  assert.deepEqual(nextRecentViewKeys(recent, 's1', pinned), [c, b, a]);
  assert.deepEqual(nextRecentViewKeys(recent, c, new Set([c, a])), [c, a]);
});

test('a pinned view keeps its caches while active, running or recently visited', () => {
  const [a, b, c, d] = ['a', 'b', 'c', 'd'].map(profile => viewKey('s1', profile));
  const pinned = new Set([a, b, c, d]);
  const recent = [c, b, a];
  const none = new Set();
  const retained = (active, max, running = none) =>
    [...retainedPinnedViewKeys(pinned, recent, active, max, running)].sort();
  assert.deepEqual(retained(c, 2), [a, b, c].sort());
  assert.deepEqual(retained(c, 1), [b, c].sort());
  assert.deepEqual(retained('s1', 2), [b, c].sort());
  // Only the active view and running views stay when the cap is zero or invalid.
  assert.deepEqual(retained(c, 0), [c]);
  assert.deepEqual(retained(c, -1), [c]);
  assert.deepEqual(retained(c, Number.NaN), [c]);
  assert.deepEqual(retained(c, 1.5), [c]);
  // A running view outside the recent window, even one never visited, survives.
  assert.deepEqual(retained(c, 0, new Set([a, d])), [a, c, d].sort());
  // A running key that is not pinned is not invented.
  assert.deepEqual(retained(c, 0, new Set(['s1'])), [c]);
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
    {key: 'session1', sessionRef: 'session1', label: 'One'},
    {key: `session1\u0000chatgpt`, sessionRef: 'session1', profile: 'chatgpt', label: 'One: chatgpt'},
    {key: `session1\u0000default`, sessionRef: 'session1', profile: 'default', label: 'One: default'},
    {key: 'session2', sessionRef: 'session2', label: 'Two'},
    {key: `session2\u0000alternate`, sessionRef: 'session2', profile: 'alternate', label: 'Two: alternate'},
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
  assert.deepEqual(restoreViews({stored, liveSessionId: 'live'}), {
    views: [{sessionRef: 'session1', profile: 'chatgpt'}],
    activeViewKey: `session1\u0000chatgpt`,
    restored: true,
  });
  // A stored selection without pinned rows is still a choice worth restoring.
  assert.deepEqual(
    restoreViews({stored: {views: [], activeViewKey: 'session2'}, liveSessionId: 'live'}),
    {views: [], activeViewKey: 'session2', restored: true},
  );
  // Duplicates and junk entries are rejected.
  assert.deepEqual(
    parseStoredViews({
      views: [
        {sessionRef: 'session1', profile: 'chatgpt'},
        {sessionRef: 'session1', profile: 'chatgpt'},
        {sessionRef: 'session1'},
        {sessionRef: '', profile: 'x'},
        'nope',
      ],
    }),
    {views: [{sessionRef: 'session1', profile: 'chatgpt'}], activeViewKey: ''},
  );
  assert.equal(parseStoredViews({views: [{sessionRef: 'a', profile: 'p'}, {sessionRef: 'b', profile: 'q'}]}).views.length, 2);
  assert.deepEqual(parseStoredViews(undefined), {views: [], activeViewKey: ''});
  assert.deepEqual(parseStoredViews('garbage'), {views: [], activeViewKey: ''});
});

test('a fresh page follows the configured seed, else the live session', () => {
  assert.deepEqual(
    restoreViews({
      seed: [{sessionRef: 'session1', profile: 'chatgpt'}],
      liveSessionId: 'live'
    }),
    {views: [{sessionRef: 'session1', profile: 'chatgpt'}], activeViewKey: 'live', restored: false},
  );
  assert.deepEqual(restoreViews({liveSessionId: 'live'}), {
    views: [],
    activeViewKey: 'live',
    restored: false,
  });
  // A seed entry without a profile names no view and is ignored.
  assert.deepEqual(
    restoreViews({seed: [{sessionRef: 'a'}, {sessionRef: 'b', profile: 'p'}], liveSessionId: 'live'}),
    {views: [{sessionRef: 'b', profile: 'p'}], activeViewKey: 'live', restored: false},
  );
});

const grouped = (id, title, profiles, activeProfile) => ({id, title, profiles, activeProfile});

test('a session with profiles is one group with a child row per profile', () => {
  const [group] = sessionListGroups([grouped('s1', 'Session1', ['p1', 'p2'], 'p1')]);
  assert.equal(group.sessionRef, 's1');
  assert.deepEqual(
    group.children.map(child => [child.key, child.label, child.profile]),
    [
      ['s1', 'p1', undefined],
      [`s1\u0000p2`, 'p2', 'p2'],
    ],
  );
});

test('the stored default profile is the session own row, so it keeps following the default', () => {
  const [group] = sessionListGroups([grouped('s1', 'S', ['a', 'b', 'c'], 'b')]);
  assert.deepEqual(group.children.map(child => child.label), ['a', 'b', 'c']);
  const own = group.children.find(child => child.label === 'b');
  assert.equal(own.key, 's1');
  assert.equal(own.profile, undefined);
});

test('a session without profiles is a group with no children, only its own row', () => {
  const [group] = sessionListGroups([grouped('s1', 'Plain', undefined, undefined)]);
  assert.deepEqual(group.children, []);
  assert.equal(group.ownRow.key, 's1');
  assert.equal(group.ownRow.label, 'Plain');
});

test('a stored default that is not listed still gets its own child row, first', () => {
  const [group] = sessionListGroups([grouped('s1', 'S', ['p1'], 'other')]);
  assert.deepEqual(group.children.map(child => [child.key, child.label]), [
    ['s1', 'other'],
    [`s1\u0000p1`, 'p1'],
  ]);
});

test('groups are expanded unless the user collapsed them, even the one holding the active row', () => {
  const groups = sessionListGroups(
    [grouped('s1', 'A', ['p1', 'p2'], 'p1'), grouped('s2', 'B', ['q1', 'q2'], 'q1')],
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

test('a profile listed twice is one child row', () => {
  const [group] = sessionListGroups([grouped('s1', 'S', ['p1', 'p2', 'p2'], 'p1')]);
  assert.deepEqual(group.children.map(child => child.label), ['p1', 'p2']);
});

const inCategory = (id, category) => ({...grouped(id, id, ['p'], 'p'), category});

test('a session carries its category into the group, and an empty one counts as none', () => {
  const groups = sessionListGroups([inCategory('a', 'work'), inCategory('b', ''), grouped('c', 'c', ['p'], 'p')]);
  assert.deepEqual(groups.map(group => group.category), ['work', undefined, undefined]);
});

test('sessions are split by category in first-seen order, uncategorised last', () => {
  const groups = sessionListGroups(
    [inCategory('a', 'work'), grouped('n', 'n', ['p'], 'p'), inCategory('b', 'test'), inCategory('c', 'work')],
  );
  assert.deepEqual(
    groupByCategory(groups).map(section => [section.category, section.groups.map(group => group.sessionRef)]),
    [['work', ['a', 'c']], ['test', ['b']], [undefined, ['n']]],
  );
});

test('no sessions give no sections, and no categories give a single unnamed section', () => {
  assert.deepEqual(groupByCategory([]), []);
  const [only, ...rest] = groupByCategory(sessionListGroups([grouped('a', 'a', ['p'], 'p')]));
  assert.equal(only.category, undefined);
  assert.equal(only.groups.length, 1);
  assert.equal(rest.length, 0);
});

test('a profile is labelled by the name its file sets, else by its ID', () => {
  assert.equal(profileLabel('dev/coder'), 'dev/coder');
  assert.equal(profileLabel('dev/coder', {}), 'dev/coder');
  assert.equal(profileLabel('dev/coder', {'dev/coder': 'Coder'}), 'Coder');
  assert.equal(profileLabel('dev/other', {'dev/coder': 'Coder'}), 'dev/other');
});

test('child rows use the profile name but keep the profile ID as the key and the target', () => {
  const [group] = sessionListGroups(
    [{...grouped('s1', 'S', ['a/one', 'a/two', 'a/three'], 'a/two'), profileNames: {'a/one': 'One', 'a/two': 'Two'}}],
  );
  assert.deepEqual(
    group.children.map(child => [child.key, child.label, child.profile]),
    [
      [`s1\u0000a/one`, 'One', 'a/one'],
      ['s1', 'Two', undefined],
      [`s1\u0000a/three`, 'a/three', 'a/three'],
    ],
  );
});

test('the own row of a stored profile the session no longer lists uses its name too', () => {
  const [group] = sessionListGroups(
    [{...grouped('s1', 'S', ['a/one'], 'a/gone'), profileNames: {'a/gone': 'Gone'}}],
  );
  assert.equal(group.children[0].label, 'Gone');
});

test('a row shows its live count, else the server count for its profile', () => {
  const own = {key: 'a', sessionRef: 'a', label: 'A'};
  const reviewer = {key: viewKey('a', 'review/reviewer'), sessionRef: 'a', profile: 'review/reviewer', label: 'R'};
  const data = {turns: 4, profileTurns: {'review/reviewer': 7, 'dev/coder': 4}};

  assert.equal(rowTurnCount(own, data, {}), 4);
  assert.equal(rowTurnCount(own, data, {a: 5}), 5);
  assert.equal(rowTurnCount(reviewer, data, {}), 7);
  assert.equal(rowTurnCount(reviewer, data, {[reviewer.key]: 8}), 8);
  assert.equal(rowTurnCount(reviewer, data, {[reviewer.key]: 0}), 0);
  assert.equal(rowTurnCount(reviewer, {turns: 4}, {}), undefined);
  assert.equal(rowTurnCount({...reviewer, profile: 'other/x'}, data, {}), undefined);
});
