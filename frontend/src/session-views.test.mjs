import test from 'node:test';
import assert from 'node:assert/strict';

import {
  addPinnedView,
  profileSelection,
  hasPinnedView,
  isPinnedViewKey,
  parseStoredViews,
  profileMenuTargets,
  profileOfViewKey,
  prunePinnedViews,
  removePinnedView,
  restoreViews,
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

test('the profile menu leaves out the profile the session already shows', () => {
  assert.deepEqual(
    profileMenuTargets({
      id: 'session1',
      title: 'One',
      profiles: ['default', 'chatgpt', 'alternate'],
      activeProfile: 'default',
    }),
    [
      {profile: 'chatgpt', label: 'One: chatgpt'},
      {profile: 'alternate', label: 'One: alternate'},
    ],
  );
  // A session without profiles offers nothing to pin.
  assert.deepEqual(profileMenuTargets({id: 'session2', title: 'Two'}), []);
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
