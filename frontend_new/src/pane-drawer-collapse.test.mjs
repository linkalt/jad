import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import test from 'node:test';

import {
  NO_PANE_DRAWERS,
  closePaneDrawer,
  hasPaneDrawer,
  openPaneDrawer,
  paneDrawerPressOpens,
} from './pane-drawers.ts';

// Pressing a pane's exclusive drawer trigger twice must collapse it. The state
// context and the conversation-actions / reset drawers clear the pane's other
// drawers before showing their own, and deciding with `togglePaneDrawer` after
// that clear always answered "open" — the clear had already removed the key —
// so a second press re-opened the drawer it was meant to close.
const app = readFileSync(new URL('./App.tsx', import.meta.url), 'utf8');

/** The sequence App runs on one press: decide, clear this pane, open if asked. */
const press = (keys, viewKey) => {
  const shouldOpen = paneDrawerPressOpens(keys, viewKey);
  const cleared = closePaneDrawer(keys, viewKey);
  return shouldOpen ? openPaneDrawer(cleared, viewKey) : cleared;
};

test('a press opens the drawer and the next press collapses it', () => {
  const opened = press(NO_PANE_DRAWERS, 'a');
  assert.equal(hasPaneDrawer(opened, 'a'), true, 'the first press shows the drawer');
  const closed = press(opened, 'a');
  assert.equal(hasPaneDrawer(closed, 'a'), false, 'the second press collapses it');
  assert.equal(hasPaneDrawer(press(closed, 'a'), 'a'), true, 'and a third opens it again');
});

test('a press in one pane leaves the other panes\' drawers alone', () => {
  const both = openPaneDrawer(openPaneDrawer(NO_PANE_DRAWERS, 'a'), 'b');
  const afterB = press(both, 'b');
  assert.equal(hasPaneDrawer(afterB, 'b'), false, 'pane b collapses its own');
  assert.equal(hasPaneDrawer(afterB, 'a'), true, 'pane a keeps its drawer');
});

test('the decision is taken before the pane\'s drawers are cleared', () => {
  const open = app.slice(
    app.indexOf('const pressStateContextDrawer = ('),
    app.indexOf('const pressConversationActionsDrawer = ('),
  );
  assert.match(
    open,
    /const shouldOpen = paneDrawerPressOpens\(stateContextDrawers, viewKey\);\s*closeDestructiveDrawersFor\(viewKey\);\s*if \(shouldOpen\) \{\s*setStateContextDrawers\(current => openPaneDrawer\(current, viewKey\)\);/,
    'state context decides first, clears, then opens only when it decided to open',
  );
  assert.match(
    open,
    /const shouldOpen = paneDrawerPressOpens\(resetDrawers, viewKey\);\s*closeDestructiveDrawersFor\(viewKey\);\s*if \(shouldOpen\) \{\s*setResetDrawers\(current => openPaneDrawer\(current, viewKey\)\);/,
    'reset follows the same rule',
  );
  assert.equal(open.includes('togglePaneDrawer'), false, 'toggling after the clear could only reopen');
});

test('the actions trigger collapses whatever destructive drawer the pane shows', () => {
  const actions = app.slice(
    app.indexOf('const pressConversationActionsDrawer = ('),
    app.indexOf('const [runProgressByView'),
  );
  assert.match(actions, /const shouldOpen = !hasDestructiveDrawer\(viewKey\);/);
  assert.match(actions, /closeDestructiveDrawersFor\(viewKey\);\s*if \(shouldOpen\) \{/);

  const owned = app.slice(
    app.indexOf('const hasDestructiveDrawer = useCallback'),
    app.indexOf('const closeDestructiveDrawersFor = useCallback'),
  );
  for (const gate of [
    'hasPaneDrawer(actionsDrawers, viewKey)',
    'hasPaneDrawer(resetDrawers, viewKey)',
    'frontCompactionInfoViewKey === viewKey',
    'frontCompactionModeViewKey === viewKey',
    'frontCompactionTarget?.viewKey === viewKey',
  ]) {
    assert.equal(
      owned.includes(gate),
      true,
      `${gate} counts as a destructive drawer, so the trigger collapses it`,
    );
  }
});

test('the triggers and the slash commands reach the same rule', () => {
  const run = app.slice(app.indexOf('const runPaneCommand = ('), app.indexOf('const requestPaneCommand = ('));
  assert.match(run, /case 'stateContext':\s*pressStateContextDrawer\(viewKey\);/);
  assert.match(run, /case 'conversationActions':\s*pressConversationActionsDrawer\(viewKey\);/);

  const slash = app.slice(app.indexOf('const runSlashCommand = ('), app.indexOf('const reportSlashError = ('));
  assert.match(slash, /const runSlashCommand = \(viewKey: string, name: SlashCommandName\) =>/);
  assert.match(slash, /if \(name === 'state'\) \{\s*pressStateContextDrawer\(viewKey\);\s*\} else \{\s*pressResetDrawer\(viewKey\);/);
  assert.equal(slash.includes('activeViewKey'), false, 'a command acts on the pane it was typed in');

  // The reset confirmation is reachable only from the actions row: opening it
  // must not leave the actions row behind.
  assert.match(app, /onClick=\{\(\) => pressResetDrawer\(pane\.viewKey\)\}/);
  // Both composer callbacks carry their own pane, not the focused one.
  assert.match(app, /onSlashCommand=\{name => runSlashCommand\(paneViewKey, name\)\}/);
  assert.match(app, /onSlashError=\{message => reportSlashError\(paneViewKey, message\)\}/);
});
