import assert from 'node:assert/strict';
import {existsSync, readFileSync} from 'node:fs';
import test from 'node:test';

import {matchProfileSearchRows, profileSearchDescription, profileSearchRows} from './profile-search.ts';
import {sessionListGroups, viewKey} from './session-views.ts';

// The panel used to search sessions. The rows a user scrolls and clicks are
// profiles, whose names appear in no session title, so typing one found nothing:
// `Backend1` matched zero rows while the rail showed it. The searchable set is
// therefore the rail's own rows — every profile row, plus a session's own row
// when it has no profile rows to offer.
const sessions = [
  {
    id: 'proj/alpha',
    title: 'Jad Harness Project',
    turns: 9,
    state: 'success',
    profiles: ['Backend1', 'Backend0'],
    activeProfile: 'Backend0',
    profileTurns: {Backend1: 14, Backend0: 252},
  },
  {id: 'proj/solo', title: 'Lonely Session', turns: 3, state: 'warning'},
];
const groups = sessionListGroups(sessions);
const rows = profileSearchRows(groups, sessions, {}, new Set());
const read = path => readFileSync(new URL(path, import.meta.url), 'utf8');
const panel = read('./components/ProfileSearchPanel.tsx');
const app = read('./App.tsx');
const config = read('./config.json');

const BACKEND1 = viewKey('proj/alpha', 'Backend1');
const labels = results => results.map(row => row.label);
const select = (query, limit = 50) => matchProfileSearchRows(rows, query, limit);
const find = (list, key) => list.find(row => row.key === key);

test('(a) a profile name finds its own row', () => {
  assert.deepEqual(labels(select('Backend1')), ['Backend1']);
  assert.deepEqual(labels(select('backend1')), ['Backend1'], 'the match is case-insensitive');
  assert.deepEqual(labels(select('  backend1  ')), ['Backend1'], 'and the query is trimmed');
  assert.deepEqual(select('Reviewer'), [], 'a name no row carries still finds nothing');
});

test('(b) a session title finds its profile rows', () => {
  const found = select('Jad Harness');
  assert.deepEqual(labels(found), ['Backend1', 'Backend0'], 'both profiles of that session');
  assert.ok(
    found.every(row => row.sessionTitle === 'Jad Harness Project'),
    'the session is what matched, so the rows carry it',
  );
  assert.deepEqual(labels(select('lonely')), ['Lonely Session'], 'another session is not swept in');
  assert.deepEqual(labels(select('proj/alpha')), ['Backend1', 'Backend0'], 'the session id matches too');
});

test('(c) a result opens that profile, not the bare session', () => {
  const [result] = select('Backend1');
  assert.equal(result.key, BACKEND1, 'the view key names the profile');
  assert.notEqual(result.key, 'proj/alpha', 'not the session id');
  assert.equal(
    result.key,
    viewKey('proj/alpha', 'Backend1'),
    'the same key the rail row carries, so selecting it opens the same view',
  );
  assert.match(
    panel,
    /onClick=\{\(\) => onSelectView\(row\.key\)\}/,
    'the panel opens the row key, which is what a rail click opens',
  );
  assert.equal(/onSelectSession|activeSessionId/.test(panel), false, 'the session-only selection is gone');
});

test('(d) a session with no profile rows still yields its own row', () => {
  const solo = find(rows, 'proj/solo');
  assert.notEqual(solo, undefined, 'the session stays reachable from the panel');
  assert.equal(solo.label, 'Lonely Session');
  assert.equal(solo.fullLabel, 'Lonely Session', 'no session: profile form for its own row');
  assert.deepEqual(labels(select('lonely')), ['Lonely Session']);
  assert.equal(rows.length, 3, 'and it does not appear twice');
});

test('(e) the result cap is honoured, and is the configured one', () => {
  assert.equal(select('', 1).length, 1);
  assert.equal(select('', 2).length, 2);
  assert.equal(select('', 50).length, rows.length, 'under the cap every row is offered');
  const cap = JSON.parse(config).profile_search_max_results;
  assert.equal(typeof cap, 'number', 'the key the panel reads must exist');
  assert.ok(cap > 0, 'and it must be a positive cap');
  assert.ok(
    panel.includes('matchProfileSearchRows(rows, query, appConfig.profile_search_max_results)'),
    'the panel takes the cap from the config',
  );
  assert.equal(config.includes('session_search_max_results'), false, 'the session-shaped key is gone');
});
test('a row shows the turns and the state the rail shows', () => {
  assert.equal(find(rows, BACKEND1).turns, 14, 'the server count for that profile');
  // The stored default profile's row is the session's own view (`key` = the id,
  // no `profile`), so it carries the session's count, exactly as the rail row does.
  assert.equal(find(rows, 'proj/alpha').turns, 9, 'the default profile row follows the session');
  assert.equal(find(rows, 'proj/solo').turns, 3, 'the session own count');
  assert.equal(
    find(profileSearchRows(groups, sessions, {[BACKEND1]: 99}, new Set()), BACKEND1).turns,
    99,
    'a row open in this tab reports its live count',
  );
  const running = profileSearchRows(groups, sessions, {}, new Set([BACKEND1]));
  assert.equal(find(running, BACKEND1).state, 'accent', 'a running row is accent');
  const failed = [{...sessions[0], runsByProfile: {Backend1: {status: 'failed'}}}, sessions[1]];
  const failedRows = profileSearchRows(sessionListGroups(failed), failed, {}, new Set());
  assert.equal(find(failedRows, BACKEND1).state, 'warning', 'a failed run is the warning the rail shows');
  assert.equal(find(rows, 'proj/solo').state, 'warning', 'a session own row follows the session state');
});

test('the description names the session, the turns and the state', () => {
  assert.equal(
    profileSearchDescription(find(rows, BACKEND1)),
    'Jad Harness Project · 14 turns · success',
  );
  assert.equal(
    profileSearchDescription(find(rows, 'proj/alpha')),
    'Jad Harness Project · 9 turns · success',
  );
  assert.equal(
    profileSearchDescription(find(rows, 'proj/solo')),
    '3 turns · warning',
    'a session own row does not repeat the title already in its label',
  );
  const one = [{id: 'proj/one', title: 'One Turn', turns: 1, state: 'success'}];
  assert.equal(
    profileSearchDescription(profileSearchRows(sessionListGroups(one), one, {}, new Set())[0]),
    '1 turn · success',
  );
});

test('the panel renders the rail rows and opens the rail view', () => {
  assert.equal(
    existsSync(new URL('./components/SessionSearchPanel.tsx', import.meta.url)),
    false,
    'the old panel file is gone',
  );
  assert.ok(panel.includes('label="Profile search"'));
  assert.ok(panel.includes('title="Profile search"'));
  assert.ok(panel.includes('label="Search profiles"'));
  assert.ok(panel.includes('placeholder="Search by profile or session"'));
  assert.ok(panel.includes('No profiles match'));
  assert.ok(panel.includes('No profiles yet.'));
  assert.ok(panel.includes('Select a profile to open it.'));
  assert.ok(panel.includes('row.fullLabel'), 'the dot is labelled with the rail name');
  assert.ok(panel.includes('row.key === activeViewKey'), 'the open marker follows the active view key');
  assert.ok(panel.includes('<Token label="open" size="sm" />'));
  assert.ok(
    panel.includes('profileSearchRows(groups, sessions, rowTurns, runningViewKeys)'),
    'the rows are the rail rows',
  );
  assert.ok(
    app.includes("import {ProfileSearchPanel} from './components/ProfileSearchPanel';"),
    'App imports the renamed panel',
  );
  assert.ok(app.includes('groups={listGroups}'), 'the panel offers the rail groups');
  assert.ok(app.includes('rowTurns={rowTurns}'));
  assert.ok(app.includes('runningViewKeys={runningViewKeys}'));
  assert.match(app, /activateView\(nextViewKey\);\s*closeLeftPanel\(\);/, 'choosing a result is a rail click');
  assert.equal(/SessionSearchPanel/.test(app), false, 'App no longer names the old panel');
});

test('the button and the help copy say profiles', () => {
  const navigation = read('./components/SessionNavigation.tsx');
  assert.ok(navigation.includes('label="Search profiles"'));
  assert.ok(navigation.includes('<Tooltip content="Search profiles"'));
  assert.equal(/Search sessions/.test(navigation), false, 'no session-shaped label is left');
  const readme = read('./jad-readme.md');
  assert.ok(readme.includes('**Search profiles**'));
  assert.equal(readme.includes('**Search sessions**'), false);
});
test('the reported defect: the rail names are findable', () => {
  // test/default as it renders: one session with four profile rows whose names
  // appear in no session title. Typing `Backend` used to answer '0 of 2 sessions'.
  const live = [
    {
      id: 'proj',
      title: 'Jad Harness Project',
      turns: 0,
      state: 'success',
      profiles: ['Backend1', 'Backend0', 'Reviewer1', 'Frontend0'],
      activeProfile: 'Backend1',
      profileTurns: {Backend1: 14, Backend0: 252, Reviewer1: 43, Frontend0: 9},
    },
  ];
  const liveRows = profileSearchRows(sessionListGroups(live), live, {}, new Set());
  assert.deepEqual(
    labels(matchProfileSearchRows(liveRows, 'Backend', 50)),
    ['Backend1', 'Backend0'],
    'both backend profiles are found by name',
  );
  assert.deepEqual(labels(matchProfileSearchRows(liveRows, 'Reviewer', 50)), ['Reviewer1']);
  assert.deepEqual(labels(matchProfileSearchRows(liveRows, 'Frontend0', 50)), ['Frontend0']);
  assert.equal(liveRows.length, 4, 'one result per profile row, and no extra session row');
  const backend1 = liveRows.find(row => row.label === 'Backend1');
  // Backend1 is the session's stored default, so its row is the session's own
  // view (`key` = the id) — the rail draws it that way, and so does the search.
  assert.equal(backend1.key, 'proj', 'the stored default profile row is the session view');
  assert.equal(profileSearchDescription(backend1), 'Jad Harness Project · 0 turns · success');
  const reviewer = liveRows.find(row => row.label === 'Reviewer1');
  assert.equal(reviewer.key, viewKey('proj', 'Reviewer1'), 'any other profile is its own view');
  assert.equal(profileSearchDescription(reviewer), 'Jad Harness Project · 43 turns · success');
});
