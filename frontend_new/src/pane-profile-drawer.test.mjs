import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import test from 'node:test';

// The composer's profile drawer lists the profiles one session has and points
// the pane at the chosen one, in place. It has to use the session list's own
// view keys: a key rebuilt from the profile names would name a view the app does
// not have, and the stored default profile would get a second row beside the
// session's own.
const app = readFileSync(new URL('./App.tsx', import.meta.url), 'utf8');
const drawer = readFileSync(new URL('./components/ProfileDrawer.tsx', import.meta.url), 'utf8');

const infoOpen = app.indexOf('const paneInfoDrawer = (');
const chromeOpen = app.indexOf('const paneComposerChrome = (');
const runCommandOpen = app.indexOf('const runPaneCommand = (');
const requestOpen = app.indexOf('const requestPaneCommand = (');
const toolbarOpen = app.indexOf('const composerToolbar = (');
const footerOpen = app.indexOf('const composerFooter = (');

test('the profile drawer is a set of the panes showing it', () => {
  assert.match(app, /const \[profileDrawers, setProfileDrawers\] = useState<PaneDrawerSet>\(NO_PANE_DRAWERS\);/);
  assert.match(
    app,
    /const closePaneDrawersFor = useCallback\(\(viewKey: string\) => \{[\s\S]*?setProfileDrawers\(current => closePaneDrawer\(current, viewKey\)\);/,
    'a pane that leaves takes its profile drawer with it',
  );
});

test('the drawer is built from its own pane and gated on its own view key', () => {
  assert.ok(infoOpen > -1 && chromeOpen > infoOpen, 'the info drawer is built per pane');
  const block = app.slice(infoOpen, chromeOpen);
  assert.match(block, /hasPaneDrawer\(profileDrawers, pane\.viewKey\)/, 'only this pane opens it');
  assert.match(block, /rows=\{profileDrawerRows\}/, 'the list holds every profile, not this pane\'s');
  assert.match(block, /selectedKey=\{pane\.viewKey\}/, 'the conversation it is showing now');
  assert.match(block, /openViewKeys=\{paneOpenViewKeys\}/, 'and which views are already on screen');
  assert.match(block, /onChange=\{targetKey => switchPaneProfile\(pane, targetKey\)\}/, 'the pick carries the pane');
  for (const read of ['activeRowSession', 'activeSession', 'activeViewKey']) {
    assert.equal(block.includes(read), false, `the drawer must not read ${read}`);
  }
});

test('the rows are every session\'s own rails rows, not keys invented from names', () => {
  const builder = app.slice(app.indexOf('const profileDrawerRows = '), app.indexOf('const switchPaneProfile = ('));
  assert.match(builder, /allProfileRows\(listGroups\)/, 'every session\'s rows, in the rail order');
  assert.equal(builder.includes('profileNames'), false, 'no key is rebuilt from a profile name');
  assert.equal(builder.includes('viewKey('), false, 'the drawer never invents a view key');
  assert.equal(builder.includes('activeViewKey'), false, 'the list does not depend on the focused view');
});

test('a pick replaces that pane\'s view and closes the drawers of the old one', () => {
  const swap = app.slice(app.indexOf('const switchPaneProfile = ('), infoOpen);
  assert.match(swap, /closePaneDrawersFor\(pane\.viewKey\);/, 'the old view\'s drawers close with it');
  assert.match(swap, /replacePaneView\(current, pane\.viewKey, targetKey\)/, 'the pane keeps its slot');
  assert.match(swap, /current\.keys\.includes\(targetKey\)[\s\S]*?focusPane\(current, targetKey\)/, 'a view on screen once');
  assert.equal(swap.includes('activeViewKey'), false, 'a pick never acts on the focused view');
  assert.equal(swap.includes('setActiveViewKey'), false);
});

test('the three info drawers share one slot per pane', () => {
  const run = app.slice(runCommandOpen, requestOpen);
  assert.match(
    run,
    /case 'profile':[\s\S]*?setModelDrawers\(current => closePaneDrawer\(current, viewKey\)\);\s*setUsageDrawers\(current => closePaneDrawer\(current, viewKey\)\);\s*setProfileDrawers\(current => togglePaneDrawer\(current, viewKey\)\);/,
    'opening the profile list closes this pane\'s other info drawers',
  );
  assert.match(run, /case 'model':[\s\S]*?setProfileDrawers\(current => closePaneDrawer\(current, viewKey\)\);/, 'and the other way round');
  assert.match(run, /case 'usage':[\s\S]*?setProfileDrawers\(current => closePaneDrawer\(current, viewKey\)\);/);
});

test('a row already open in another pane cannot be picked', () => {
  assert.match(drawer, /data-profile-row=\{isSelected \? 'selected' : isOpenElsewhere \? 'open' : 'choice'\}/);
  assert.match(drawer, /if \(isDisabled \|\| isOpenElsewhere\) return;/, 'a press on it does nothing');
  assert.match(drawer, /tabIndex=\{isDisabled \|\| isOpenElsewhere \? -1 : 0\}/, 'and it is out of the tab order');
  assert.match(drawer, /\{\.\.\.\(isOpenElsewhere \? \{'aria-disabled': true\} : \{\}\)\}/, 'assistive tech hears it is disabled');
  assert.match(drawer, /\.\.\.\(isOpenElsewhere \? ALREADY_OPEN_ROW : \{\}\)/, 'and it is dimmed');
  assert.equal(drawer.includes('<Text type="supporting" color="secondary">open</Text>'), false, 'no extra label on the row');
});

test('the toolbar control opens it for its own pane', () => {  const toolbar = app.slice(toolbarOpen, footerOpen);
  assert.match(toolbar, /const isProfileOpenHere = hasPaneDrawer\(profileDrawers, pane\.viewKey\);/, 'no focus gating');
  assert.match(toolbar, /label="Profile"/);
  assert.match(toolbar, /icon=\{<Icon icon="app:profile" size="sm" \/>\}/, 'the profile glyph, not the model one');
  assert.match(toolbar, /onClick=\{\(\) => requestPaneCommand\(pane\.viewKey, 'profile'\)\}/);
  const control = toolbar.slice(toolbar.indexOf('label="Profile"'), toolbar.indexOf('{options.length > 0'));
  assert.equal(control.includes('isDisabled'), false, 'the control opens the drawer even while its run is going');
  assert.match(
    app,
    /<ProfileDrawer[\s\S]*?isDisabled=\{pane\.sessionRef === undefined\}/,
    'only a pane with no session refuses a pick',
  );
  assert.match(
    toolbar,
    /hasProfileToSwitchTo\(profileDrawerRows, pane\.viewKey\)/,
    'a list with nothing but this conversation offers no control',
  );
});

test('a run in flight leaves the profile list usable', () => {
  // The reported bug: with a run going, the drawer's one row was inert, so the
  // list could not be expanded at all, and once open every row ignored a press.
  // A pick only changes which conversation this pane shows — the run keeps
  // going, keyed by session, with or without a pane watching it.
  const profileDrawer = app.slice(app.indexOf('<ProfileDrawer'), app.indexOf('hasPaneDrawer(modelDrawers'));
  assert.notEqual(profileDrawer, '', 'the profile drawer block is found');
  assert.equal(
    /pane\.isRunning|isCompactionInProgress|isModelDefaultUpdating|resettingViewKey|frontCompactingViewKey/.test(profileDrawer),
    false,
    'nor a compaction, nor a pending setting write, closes the profile list',
  );
  assert.match(profileDrawer, /isDisabled=\{pane\.sessionRef === undefined\}/, 'the drawer carries only the missing-session guard');
  // The model picker is the opposite case: it writes a stored setting, so its
  // own run and the other in-flight writes still hold it closed.
  const modelDrawer = app.slice(app.indexOf('<ModelDrawer'), app.indexOf('hasPaneDrawer(usageDrawers'));
  assert.match(modelDrawer, /pane\.isRunning/, 'the model picker keeps its guards');
  assert.match(modelDrawer, /isCompactionInProgress/);
});

test('the drawer follows the model drawer: one row closed, every row open', () => {
  assert.match(drawer, /profileChoiceRows\(rows, selectedKey, query, isExpanded, openViewKeys\)/, 'the pure row rule decides');
  assert.match(drawer, /data-profile-row=\{isSelected \? 'selected' : isOpenElsewhere \? 'open' : 'choice'\}/, 'each row carries its selection');
  assert.match(drawer, /placeholder="Search profiles in every session"/, 'a search over every session');
  assert.equal(drawer.includes('data-profile-header'), false, 'no column header row');
  assert.equal(/Profile name|Profile id/.test(drawer), false, 'no column labels at all');
  const rowBlock = drawer.slice(drawer.indexOf('{choiceRows.map('), drawer.indexOf('{isExpanded ? null'));
  assert.ok(
    rowBlock.indexOf('width={LABEL_COLUMN_WIDTH}') < rowBlock.indexOf('width={PATH_COLUMN_WIDTH}') &&
      rowBlock.indexOf('width={PATH_COLUMN_WIDTH}') < rowBlock.indexOf('width={SESSION_COLUMN_WIDTH}'),
    'a row keeps the header\'s order: name, id, session',
  );
  assert.match(
    drawer,
    /\{row\.profile \?\? row\.defaultProfile \?\? '—'\}/,
    'the id column shows the profile\'s own id, else the session\'s stored default',
  );
  assert.match(drawer, /\{isExpanded \? null : <Icon icon="chevronDown" size="sm" \/>\}/, 'the way into the list');
  assert.match(drawer, /if \(!isSelected\) onChange\(row\.key\);/, 'the current row folds the list instead of switching');
  assert.equal(drawer.includes('stored default'), false, 'no placeholder id: the server names the default');
  assert.equal(/TableHeader|TableBody/.test(drawer), false, 'stacks, not a table');
});
