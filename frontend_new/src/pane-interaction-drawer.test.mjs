import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import test from 'node:test';

// A run that asks for a draft answer waits for it, so the drawer
// belongs to the pane whose run is waiting: answering it from the focused pane
// would have written to whatever view happened to be active. These anchors keep
// the drawer built per view, and keep the two answer paths reading their target
// instead of the active view.
const app = readFileSync(new URL('./App.tsx', import.meta.url), 'utf8');

const drawerOpen = app.indexOf('const interactionDrawerFor = (');
const paneChromeOpen = app.indexOf('const paneComposerChrome = (');
const answerOpen = app.indexOf('const answerInteraction = useCallback((');
const extendOpen = app.indexOf('const extendInteraction = useCallback((');
const compactOpen = app.indexOf('const compactSessionFor = useCallback(');

test('the drawer is built from the target view, not the active one', () => {
  assert.ok(drawerOpen > -1, 'App.tsx must build a per-view interaction drawer');
  assert.ok(paneChromeOpen > drawerOpen, 'every pane builds its own chrome from that builder');
  const block = app.slice(drawerOpen, paneChromeOpen);
  assert.match(block, /runInteractionStatesByView\[target\.viewKey\]/);
  assert.match(block, /streamingMessagesByView\[target\.viewKey\]\?\.pending/);
  assert.match(block, /interactionUiByView\[target\.viewKey\]/);
  assert.match(block, /<DraftRequestDrawer/);
  assert.match(block, /answerInteraction\(target, interaction, body\)/);
  assert.match(block, /extendInteraction\(target, interaction\)/);
});

test('answering and extending act on their own view', () => {
  assert.ok(answerOpen > -1, 'answerInteraction must exist');
  assert.ok(extendOpen > answerOpen, 'extendInteraction follows answerInteraction');
  assert.ok(compactOpen > extendOpen, 'the interaction handlers stay together');
  for (const [name, start, end] of [
    ['answerInteraction', answerOpen, extendOpen],
    ['extendInteraction', extendOpen, compactOpen],
  ]) {
    const block = app.slice(start, end);
    assert.match(block, /target: \{viewKey: string; session: Session \| undefined; title: string\}/, `${name} takes its target`);
    assert.match(block, /profileOfViewKey\(viewKey\)/, `${name} resolves that view's profile`);
    for (const read of ['activeSession', 'activeProfile', 'activeViewKey', 'activeRowSession']) {
      assert.equal(
        block.includes(read),
        false,
        `${name} must not read ${read}: that answers whichever pane is focused`,
      );
    }
  }
});

test('every pane gets the drawer for its own pending question', () => {
  assert.match(
    app,
    /composerChrome=\{paneComposerChrome\(pane\)\}/,
    'every pane builds its own chrome, focused or not',
  );
  const builder = app.slice(paneChromeOpen, app.indexOf('const runPaneCommand = ('));
  assert.match(
    builder,
    /interactionDrawerFor\(\{viewKey: pane\.viewKey, session: pane\.session, title: pane\.label\}\)/,
    'the interaction drawer of the pane being built comes first',
  );
  assert.match(builder, /paneCommandDrawer\(pane\)/, 'then its destructive drawers');
  assert.match(builder, /paneInfoDrawer\(pane\)/, 'then its own info drawer');
});
