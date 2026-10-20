import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import test from 'node:test';

import {
  APP_WINDOW_Z_INDEX,
  PANE_HEADER_Z_INDEX,
  PANE_PANEL_Z_INDEX,
} from './panel-layers.ts';

// Two panels share one shell with opposite requirements: a pane's own panel must
// stay under that pane's title strip (so the strip's label, spinner and ✕ stay
// clickable), while the app's left window must float above every pane. The bug
// this guards against is a pane header painting over the left window, which
// happens the moment the header's number is the larger of the two.
const floatingPanel = readFileSync(new URL('./components/FloatingPanel.tsx', import.meta.url), 'utf8');
const viewPane = readFileSync(new URL('./components/ViewPane.tsx', import.meta.url), 'utf8');

test('the stacking order is pane panel < pane header < app window', () => {
  assert.ok(
    PANE_PANEL_Z_INDEX < PANE_HEADER_Z_INDEX,
    'a pane panel sits under its own title strip',
  );
  assert.ok(
    PANE_HEADER_Z_INDEX < APP_WINDOW_Z_INDEX,
    'the app window sits above every pane header',
  );
});

test('the shell picks its layer, instead of hard-coding one number', () => {
  assert.match(
    floatingPanel,
    /layer\?: 'app' \| 'pane';/,
    'the panel says which window it belongs to',
  );
  assert.match(floatingPanel, /layer = 'app',/, 'the app window is the default');
  assert.match(
    floatingPanel,
    /style=\{\{zIndex: layer === 'app' \? APP_WINDOW_Z_INDEX : PANE_PANEL_Z_INDEX\}\}/,
    'the layer decides the number',
  );
  // The edge styles carry position only: a z-index there would win over the layer.
  const edgeStyles = floatingPanel.slice(floatingPanel.indexOf('const styles = stylex.create'), floatingPanel.indexOf('card: {'));
  assert.equal(/zIndex/.test(edgeStyles), false, 'no edge style pins a z-index of its own');
});

test('the panels inside a pane opt into the pane layer, the app window does not', () => {
  for (const name of ['RunInspector', 'PromptLibraryPanel']) {
    const source = readFileSync(new URL(`./components/${name}.tsx`, import.meta.url), 'utf8');
    assert.match(source, /<FloatingPanel\s*side="end"\s*layer="pane"/, `${name} floats inside its pane`);
  }
  for (const name of ['NotificationPanel', 'CheckpointPanel', 'ProfileSearchPanel', 'RunSettingsPanel', 'PanesPanel']) {
    const source = readFileSync(new URL(`./components/${name}.tsx`, import.meta.url), 'utf8');
    assert.equal(source.includes('layer="pane"'), false, `${name} is part of the app's left window`);
  }
});

test('the pane header takes its number from the same list', () => {
  assert.match(viewPane, /import \{PANE_HEADER_Z_INDEX\} from '\.\.\/panel-layers';/);
  assert.match(viewPane, /style=\{\{zIndex: PANE_HEADER_Z_INDEX\}\}/, 'the strip is above its own panel');
  // Stylex only accepts literals or a *.stylex theme inside stylex.create, so
  // the header's number cannot sit in its style object.
  const headerStyle = viewPane.slice(viewPane.indexOf('header: {'), viewPane.indexOf('titleRow: {'));
  assert.equal(/zIndex/.test(headerStyle), false, 'the header style carries position only');
});
