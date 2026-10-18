import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import test from 'node:test';

// In the collapsed rail a session's profile rows live in the SideNavItem's hover
// flyout, and the flyout closes 200ms after the pointer leaves it — which any
// second layer does, even one drawn inside the flyout's own box. A row menu
// mounted outside the flyout therefore took the pointer out of it: the flyout
// closed, its rows unmounted, and the menu — anchored to a row that no longer
// existed — fell back to the viewport corner. Measured in Chrome with Playwright:
// the menu jumped from x=228 to x=4 and the profile list vanished.
//
// So a profile row's menu is rendered inside the group that owns the row, which
// is the flyout's DOM subtree when the rail is collapsed, and closes with that
// layer instead of outliving it.
const groupList = readFileSync(
  new URL('./components/SessionGroupList.tsx', import.meta.url),
  'utf8',
);
const navigation = readFileSync(
  new URL('./components/SessionNavigation.tsx', import.meta.url),
  'utf8',
);
const rowMenu = readFileSync(
  new URL('./components/RowContextMenu.tsx', import.meta.url),
  'utf8',
);

test('a profile row menu is rendered inside its own group, beside the rows it acts on', () => {
  const rows = groupList.indexOf('{group.children.map(row => renderRow(row, session, true))}');
  const menu = groupList.indexOf('<RowContextMenu');
  const close = groupList.indexOf('</SideNavItem>');
  assert.ok(rows >= 0, 'the group must render its profile rows');
  assert.ok(menu > rows, 'the row menu must follow the rows inside the same SideNavItem');
  assert.ok(close > menu, 'the row menu must stay inside the SideNavItem that hosts the rows');
});

test('the rail-level menu does not also render a profile row menu', () => {
  assert.match(
    navigation,
    /target=\{rowMenuTarget\?\.kind === 'profile' \? null : rowMenuTarget\}/,
    'SessionNavigation must leave profile row menus to their group',
  );
  assert.match(
    navigation,
    /isOpen=\{rowMenuTarget !== null && rowMenuTarget\.kind !== 'profile'\}/,
    'the rail-level menu must not open for a profile row',
  );
});

test('a profile row menu is only shown for a row of the group rendering it', () => {
  assert.match(
    groupList,
    /rowMenuTarget\.sessionRef === group\.sessionRef/,
    'a group must only host the menu of its own rows',
  );
  assert.match(groupList, /kind: 'profile', id: row\.key, label: fullLabel, sessionRef: row\.sessionRef/, 'profile row targets must carry their session');
});

test('a row menu closes with the layer that hosts it', () => {
  assert.match(rowMenu, /closest\('\[popover\]'\)/, 'RowContextMenu must find the layer hosting its anchor');
  assert.match(rowMenu, /addEventListener\('toggle'/, 'it must listen for that layer closing');
  assert.match(rowMenu, /\.newState === 'closed'/, 'only a close of the hosting layer dismisses the menu');
});

// A profile is created from the catalog, not from a row, so New has nothing to
// create there. Session and category rows keep it.
test('only a profile row menu drops the New entry', () => {
  assert.match(
    rowMenu,
    /\{target\.kind === 'profile' \? null : \(\s*<Tooltip content="New"/,
    'the New entry must be skipped for a profile row only',
  );
  assert.match(rowMenu, /<Tooltip content="Edit"/, 'Edit stays for every row kind');
  assert.match(rowMenu, /<Tooltip content="Delete"/, 'Delete stays for every row kind');
});
