import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import test from 'node:test';

// The right end of the composer row is the run inspector plus the two controls
// that were asked to sit beside it: the compaction control first, then the
// conversation-actions trigger (the one that holds reset and delete, and which
// the icon set can only give a warning glyph). Both used to live in the left
// cluster with the order the other way round, so this pins the slot, the order
// and the glyph rather than the styling around them.
//
// The composer chrome is built in App and handed to the focused pane, so these
// anchors name the constants App builds rather than the props of one element.
const app = readFileSync(new URL('./App.tsx', import.meta.url), 'utf8');
const pane = readFileSync(new URL('./components/ComposerPane.tsx', import.meta.url), 'utf8');

const contextOpen = app.indexOf('composerHeaderActions = (');
const toolbarOpen = app.indexOf('composerToolbar = (');
const footerOpen = app.indexOf('composerFooter = (');

test('the row passes both controls as the header-context actions, compaction first', () => {
  assert.ok(contextOpen > -1, 'App.tsx must build headerContextActions');
  assert.ok(toolbarOpen > contextOpen, 'headerContextActions must be declared before toolbarActions');
  assert.ok(footerOpen > toolbarOpen, 'footerNote must follow toolbarActions');
  const block = app.slice(contextOpen, toolbarOpen);
  assert.match(block, /label="Conversation actions"/, 'the trigger belongs to the header-context actions');
  assert.match(block, /'Compact conversation history'/, 'the compaction control belongs to them too');
  assert.ok(
    block.indexOf('Compact conversation history') < block.indexOf('Conversation actions'),
    'the compaction control comes first, then the conversation-actions trigger',
  );
  assert.match(
    block,
    /if \(readmeInPane \|\| !pane\.isLive \|\| pane\.sessionRef === undefined\)/,
    'both controls keep the guards the row had: no README view, a live session with a reference',
  );
});

test('the conversation-actions trigger wears the warning glyph, not the 3-dot', () => {
  const block = app.slice(contextOpen, toolbarOpen);
  assert.match(block, /icon=\{<Icon icon="warning" size="sm" \/>\}/, 'the trigger must use the warning icon');
  assert.ok(!block.includes('moreHorizontal'), 'the trigger no longer uses the 3-dot icon');
});

test('the theme registers the glyph that trigger asks for', () => {
  const icons = readFileSync(new URL('./themes/neutral/icons.tsx', import.meta.url), 'utf8');
  assert.match(icons, /^ {2}warning:/m, 'the theme must register a warning icon');
});

test('the left cluster no longer carries either control', () => {
  const toolbar = app.slice(toolbarOpen, footerOpen);
  assert.ok(!toolbar.includes('Conversation actions'), 'the 3-dot is still in toolbarActions');
  assert.ok(
    !toolbar.includes('Compact conversation history'),
    'the compaction control is still in toolbarActions',
  );
  assert.match(toolbar, /label=\{isUsageOpenHere \? 'Hide usage details'/, 'the usage control stays');
  assert.match(toolbar, /label="State context"/, 'the state-context control stays');
});

test('the composer header renders the slot in its right-aligned area, before the run inspector', () => {
  const headerContext = pane.slice(pane.indexOf('headerContext={'), pane.indexOf('footerActions='));
  assert.ok(headerContext.length > 0, 'ComposerPane.tsx must render headerContext');
  assert.match(headerContext, /\{headerContextActions\}/, 'the slot must render the controls');
  assert.ok(
    headerContext.indexOf('{headerContextActions}') < headerContext.indexOf('run inspector'),
    'the controls come before the run inspector toggle',
  );
});

// With several restored panes the README covers none of them, so the session
// list must keep highlighting the focused row: the selection survives reload.
test('the session navigation clears its selection only when the README fills the pane', () => {
  assert.ok(
    app.includes("activeViewKey={readmeInPane ? '' : activeViewKey}"),
    'the navigation must blank activeViewKey on readmeInPane',
  );
  assert.ok(
    !app.includes("activeViewKey={isReadmeShown ? ''"),
    'the navigation must not blank activeViewKey on isReadmeShown',
  );
});

test('conversation actions trigger is disabled while its pane or destructive operations are busy', () => {
  const block = app.slice(contextOpen, toolbarOpen);
  assert.match(block, /const isBlocked\s*=\s*pane\.isRunning\s*\|\|[\s\S]*?frontCompactingViewKey !== null/);
  assert.match(block, /label="Conversation actions"[\s\S]*?isDisabled=\{isBlocked\}[\s\S]*?requestPaneCommand\(pane\.viewKey, 'conversationActions'\)/);
});
