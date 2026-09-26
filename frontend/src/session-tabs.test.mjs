import test from 'node:test';
import assert from 'node:assert/strict';

import {
  addTab,
  closeOtherTabs,
  closeTab,
  findTab,
  isPinnedViewKey,
  neighbourTabId,
  parseStoredTabs,
  pruneTabs,
  restoreTabs,
  retargetTab,
  sessionRefOfViewKey,
  tabFromTarget,
  tabId,
  tabLabel,
  tabMenuTargets,
  tabRunStatus,
  viewKey,
} from './session-tabs.ts';

const session = (sessionRef, profile) =>
  profile === undefined ? tabFromTarget({sessionRef}) : tabFromTarget({sessionRef, profile});

test('a view is identified by its session and, when pinned, its profile', () => {
  assert.equal(tabId({sessionRef: 'session1'}), 'session1');
  assert.equal(tabId({sessionRef: 'session1', profile: 'chatgpt'}), `session1\u0000chatgpt`);
  // Two profiles of one session are two views, and the pinned one is not the
  // unpinned one either.
  assert.notEqual(tabId({sessionRef: 'session1', profile: 'chatgpt'}), tabId({sessionRef: 'session1'}));
  assert.notEqual(
    tabId({sessionRef: 'session1', profile: 'chatgpt'}),
    tabId({sessionRef: 'session1', profile: 'default'}),
  );
  // A key with a root-qualified reference stays parseable: the reference keeps
  // its own colon and the separator is the only NUL.
  assert.equal(viewKey('root:session1', 'chatgpt'), `root:session1\u0000chatgpt`);
  assert.equal(sessionRefOfViewKey(viewKey('root:session1', 'chatgpt')), 'root:session1');
  assert.equal(sessionRefOfViewKey('session1'), 'session1');
  assert.equal(isPinnedViewKey('session1'), false);
  assert.equal(isPinnedViewKey(viewKey('session1', 'chatgpt')), true);
});

test('opening a target never duplicates a view and stops at the limit', () => {
  const open = addTab([session('session1')], {sessionRef: 'session1'}, 4);
  assert.deepEqual(open, [session('session1')]);

  const two = addTab(open, {sessionRef: 'session1', profile: 'chatgpt'}, 4);
  assert.deepEqual(two.map(tab => tab.id), ['session1', `session1\u0000chatgpt`]);
  assert.equal(findTab(two, {sessionRef: 'session1', profile: 'chatgpt'})?.id, `session1\u0000chatgpt`);
  assert.equal(findTab(two, {sessionRef: 'session2'}), undefined);

  const full = addTab(two, {sessionRef: 'session2'}, 2);
  assert.deepEqual(full, two);
});

test('a tab holds its target only, never the menu labels it was built from', () => {
  const target = {sessionRef: 'session1', profile: 'chatgpt', label: 'One: chatgpt', sessionLabel: 'One'};
  assert.deepEqual(tabFromTarget(target), {
    sessionRef: 'session1',
    profile: 'chatgpt',
    id: `session1\u0000chatgpt`,
  });
  const opened = addTab([], target, 4);
  assert.deepEqual(Object.keys(opened[0] ?? {}).sort(), ['id', 'profile', 'sessionRef']);
});

test('a sidebar click retargets the open tab and folds a duplicate view', () => {
  const tabs = [session('session1'), session('session2')];
  const retargeted = retargetTab(tabs, 'session1', {sessionRef: 'session3'});
  assert.deepEqual(retargeted.map(tab => tab.id), ['session3', 'session2']);

  // Clicking a session that another tab already shows removes the duplicate
  // rather than opening the same view twice.
  const folded = retargetTab(tabs, 'session1', {sessionRef: 'session2'});
  assert.deepEqual(folded.map(tab => tab.id), ['session2']);

  const same = retargetTab(tabs, 'session1', {sessionRef: 'session1'});
  assert.deepEqual(same, tabs);
});

test('closing a tab hands the view to its neighbour and never empties the strip', () => {
  const tabs = [session('session1'), session('session2'), session('session3')];
  assert.deepEqual(closeTab(tabs, 'session2').map(tab => tab.id), ['session1', 'session3']);
  assert.equal(neighbourTabId(tabs, 'session2'), 'session3');
  assert.equal(neighbourTabId(tabs, 'session3'), 'session2');
  assert.equal(neighbourTabId(tabs, 'session1'), 'session2');

  const single = [session('session1')];
  assert.deepEqual(closeTab(single, 'session1'), single);
  assert.equal(neighbourTabId(single, 'session1'), 'session1');

  assert.deepEqual(closeOtherTabs(tabs, 'session2'), [session('session2')]);
  assert.deepEqual(closeOtherTabs(tabs, 'missing'), tabs);
});

test('a tab whose session disappeared is dropped, all of them included', () => {
  const tabs = [session('session1'), session('session2', 'chatgpt'), session('gone')];
  assert.deepEqual(
    pruneTabs(tabs, ['session1', 'session2']).map(tab => tab.id),
    ['session1', `session2\u0000chatgpt`],
  );
  assert.deepEqual(pruneTabs([session('gone')], ['session1']), []);
});

test('labels name the session, and the profile when the tab is pinned to one', () => {
  assert.equal(tabLabel({sessionRef: 'session1'}, 'Project one'), 'Project one');
  assert.equal(tabLabel({sessionRef: 'session1', profile: 'chatgpt'}, 'Project one'), 'Project one: chatgpt');
  // Without a title the reference is what there is to show.
  assert.equal(tabLabel({sessionRef: 'session1'}), 'session1');
  assert.equal(tabLabel({sessionRef: 'session1', profile: 'chatgpt'}, '   '), 'session1: chatgpt');
});

test('the add menu offers the session and one entry per profile', () => {
  assert.deepEqual(
    tabMenuTargets([
      {sessionRef: 'session1', label: 'One', profiles: ['default', 'chatgpt']},
      {sessionRef: 'session2', label: 'Two'},
    ]),
    [
      {sessionRef: 'session1', label: 'One', sessionLabel: 'One'},
      {sessionRef: 'session1', profile: 'default', label: 'One: default', sessionLabel: 'One'},
      {sessionRef: 'session1', profile: 'chatgpt', label: 'One: chatgpt', sessionLabel: 'One'},
      {sessionRef: 'session2', label: 'Two', sessionLabel: 'Two'},
    ],
  );
});

test('a tab shows the run of its own profile, not the session default', () => {
  const run = {status: 'running'};
  const session = {
    run: {status: 'completed'},
    runsByProfile: {chatgpt: run},
  };
  assert.deepEqual(tabRunStatus({sessionRef: 'session1'}, session), {status: 'completed'});
  assert.deepEqual(tabRunStatus({sessionRef: 'session1', profile: 'chatgpt'}, session), run);
  assert.equal(tabRunStatus({sessionRef: 'session1', profile: 'other'}, session), undefined);
  assert.equal(tabRunStatus(undefined, session), undefined);
});

test('a stored set is restored as it was, defensively', () => {
  const stored = {
    tabs: [{sessionRef: 'session1'}, {sessionRef: 'session2', profile: 'chatgpt'}],
    activeTabId: `session2\u0000chatgpt`,
  };
  assert.deepEqual(restoreTabs({stored, liveSessionId: 'live', limit: 8}), {
    tabs: [session('session1'), session('session2', 'chatgpt')],
    activeTabId: `session2\u0000chatgpt`,
    // A set that was restored is a choice the user already made.
    restored: true,
  });

  // An active id that no longer exists falls back to the first tab.
  assert.equal(
    restoreTabs({stored: {...stored, activeTabId: 'gone'}, liveSessionId: 'live', limit: 8}).activeTabId,
    'session1',
  );
  // Duplicates, junk entries, and an over-long list are all rejected.
  assert.deepEqual(
    parseStoredTabs(
      {tabs: [{sessionRef: 'session1'}, {sessionRef: 'session1'}, {profile: 'x'}, 'nope', {sessionRef: ''}]},
      8,
    ).tabs.map(tab => tab.id),
    ['session1'],
  );
  assert.deepEqual(parseStoredTabs({tabs: [{sessionRef: 'a'}, {sessionRef: 'b'}]}, 1).tabs.map(tab => tab.id), ['a']);
  assert.deepEqual(parseStoredTabs(undefined, 8), {tabs: [], activeTabId: ''});
  assert.deepEqual(parseStoredTabs('garbage', 8), {tabs: [], activeTabId: ''});
  assert.deepEqual(parseStoredTabs({tabs: 'nope'}, 8), {tabs: [], activeTabId: ''});
});

test('a fresh page follows the configured seed, else the live session', () => {
  assert.deepEqual(
    restoreTabs({seed: [{sessionRef: 'session1', profile: 'chatgpt'}], liveSessionId: 'live', limit: 8}),
    {tabs: [session('session1', 'chatgpt')], activeTabId: `session1\u0000chatgpt`, restored: false},
  );
  assert.deepEqual(restoreTabs({liveSessionId: 'live', limit: 8}), {
    tabs: [session('live')],
    activeTabId: 'live',
    restored: false,
  });
  // The seed is capped and junk targets are ignored.
  assert.deepEqual(
    restoreTabs({seed: [{sessionRef: 'a'}, {sessionRef: 'b'}, {sessionRef: ''}], liveSessionId: 'live', limit: 1}),
    {tabs: [session('a')], activeTabId: 'a', restored: false},
  );
});
