import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import test from 'node:test';

// A composer drawer belongs to the pane that opened it. It used to be one
// app-wide value rendered only by the focused pane, so opening the model picker
// or the usage details in pane 1 closed pane 2's, and the reset confirmation
// disappeared the moment focus moved. These anchors keep every drawer a set of
// view keys, built per pane and rendered by every pane.
const app = readFileSync(new URL('./App.tsx', import.meta.url), 'utf8');

const commandOpen = app.indexOf('const paneCommandDrawer = (');
const infoOpen = app.indexOf('const paneInfoDrawer = (');
const chromeOpen = app.indexOf('const paneComposerChrome = (');
const runCommandOpen = app.indexOf('const runPaneCommand = (');
const requestOpen = app.indexOf('const requestPaneCommand = (');
const toolbarOpen = app.indexOf('const composerToolbar = (');
const headerActionsOpen = app.indexOf('const composerHeaderActions = (');

test('every drawer is a set of the views showing it', () => {
  for (const [label, name] of [
    ['model', 'modelDrawers'],
    ['usage', 'usageDrawers'],
    ['state context', 'stateContextDrawers'],
    ['conversation actions', 'actionsDrawers'],
    ['reset', 'resetDrawers'],
  ]) {
    assert.match(
      app,
      new RegExp(`const \\[${name}, set\\w+\\] = useState<PaneDrawerSet>\\(NO_PANE_DRAWERS\\);`),
      `the ${label} drawer is one set of view keys`,
    );
  }
  for (const gone of [
    'isModelDrawerOpen',
    'isUsageDrawerCollapsed',
    'stateContextViewKey',
    'composerActionsViewKey',
    'resetDrawerViewKey',
    'pendingPaneCommand',
  ]) {
    assert.equal(app.includes(gone), false, `${gone} is gone: one value could only describe one pane`);
  }
});

test('the info drawers are built from their own pane', () => {
  assert.ok(infoOpen > -1, 'App.tsx must build a per-pane info drawer');
  assert.ok(chromeOpen > infoOpen, 'the pane chrome follows the info drawer');
  const block = app.slice(infoOpen, chromeOpen);
  assert.match(block, /hasPaneDrawer\(modelDrawers, pane\.viewKey\)/);
  assert.match(block, /hasPaneDrawer\(usageDrawers, pane\.viewKey\)/);
  assert.match(block, /selectedModelId=\{pane\.modelChoice\?\.option\?\.id\}/, 'its own stored pick');
  assert.match(block, /usage=\{pane\.session\.usage\}/, 'its own usage');
  for (const read of ['activeRowSession', 'activeSession', 'activeViewKey']) {
    assert.equal(block.includes(read), false, `the info drawer must not read ${read}`);
  }
});

test('the destructive drawers are built from their own pane too', () => {
  assert.ok(commandOpen > -1, 'App.tsx must build a per-pane command drawer');
  const block = app.slice(commandOpen, infoOpen);
  for (const gate of [
    'hasPaneDrawer(stateContextDrawers, pane.viewKey)',
    'hasPaneDrawer(actionsDrawers, pane.viewKey)',
    'hasPaneDrawer(resetDrawers, pane.viewKey)',
    'frontCompactionInfoViewKey === pane.viewKey',
    'frontCompactionTarget.viewKey === pane.viewKey',
  ]) {
    assert.match(block, new RegExp(gate.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')), `${gate} gates the drawer`);
  }
  // The confirmation the user confirmed is the one that runs.
  assert.match(block, /onClick=\{\(\) => void resetSessionFor\(target\)\}/);
  assert.match(block, /frontCompactActiveSession\(frontCompactionTarget\.message, target\)/);
  assert.match(block, /resetErrorByView\[pane\.viewKey\]/, 'each pane shows its own reset error');
  // Nothing in the drawers may act on the focused view.
  for (const read of ['activeRowSession', 'activeSession', 'activeViewKey', 'activeSessionId']) {
    assert.equal(block.includes(read), false, `the destructive drawers must not read ${read}`);
  }
});

test('every pane renders its own drawers, focused or not', () => {
  assert.ok(chromeOpen > -1, 'App.tsx must build one chrome per pane');
  const builder = app.slice(chromeOpen, runCommandOpen);
  assert.match(
    builder,
    /interactionDrawerFor\(\{viewKey: pane\.viewKey, session: pane\.session, title: pane\.label\}\)/,
    'a question the pane\'s run is waiting on comes first',
  );
  assert.match(builder, /paneCommandDrawer\(pane\)/, 'then its destructive drawers');
  assert.match(builder, /paneInfoDrawer\(pane\)/, 'then its info drawer');
  assert.equal(builder.includes('isFocused'), false, 'no pane needs focus to render its own drawer');
  assert.match(app, /composerChrome=\{paneComposerChrome\(pane\)\}/, 'each pane is handed its own chrome');
});

test('a pane command carries the view it belongs to and runs at once', () => {
  assert.ok(runCommandOpen > -1, 'runPaneCommand exists');
  const run = app.slice(runCommandOpen, requestOpen);
  assert.match(run, /const runPaneCommand = \(kind: PaneCommandKind, viewKey: string\): void =>/);
  assert.equal(run.includes('activeViewKey'), false, 'a command never runs against the active view');
  assert.match(run, /setModelDrawers\(current => togglePaneDrawer\(current, viewKey\)\)/);
  assert.match(run, /setUsageDrawers\(current => togglePaneDrawer\(current, viewKey\)\)/);
  // The exclusive drawers clear their pane first, so they decide before the
  // clear and collapse on a second press (see pane-drawer-collapse.test.mjs).
  assert.match(run, /case 'stateContext':\s*pressStateContextDrawer\(viewKey\);/);
  assert.match(run, /case 'conversationActions':\s*pressConversationActionsDrawer\(viewKey\);/);
  assert.match(run, /compactSessionFor\(actionTargetOf\(pane, pane\.session\)\)/, 'compaction takes its own target');

  const request = app.slice(requestOpen, headerActionsOpen);
  assert.match(request, /focusViewPane\(viewKey\);\s*runPaneCommand\(kind, viewKey\);/, 'no queue behind a focus change');
});

test('the toolbar and the actions trigger reflect their own pane', () => {
  const toolbar = app.slice(toolbarOpen, app.indexOf('const composerFooter = ('));
  assert.match(toolbar, /const isModelOpenHere = hasPaneDrawer\(modelDrawers, pane\.viewKey\);/, 'no focus gating');
  assert.match(toolbar, /const isUsageOpenHere = hasPaneDrawer\(usageDrawers, pane\.viewKey\);/);
  assert.match(toolbar, /const isStateContextOpenHere = hasPaneDrawer\(stateContextDrawers, pane\.viewKey\);/);

  const header = app.slice(headerActionsOpen, toolbarOpen);
  assert.match(
    header,
    /hasPaneDrawer\(actionsDrawers, pane\.viewKey\) \|\|\s*hasPaneDrawer\(resetDrawers, pane\.viewKey\)/,
    'the warning trigger lights up for its own pane\'s drawers',
  );
});

test('a pane that leaves the arrangement takes its drawers with it', () => {
  const effect = app.slice(app.indexOf('const previousPaneKeysRef = useRef'), commandOpen);
  assert.match(effect, /if \(!panes\.keys\.includes\(key\)\) \{\s*closePaneDrawersFor\(key\);/);
});
