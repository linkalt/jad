import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import test from 'node:test';

// With one conversation on screen the lane is centred, so a panel anchored to
// the pane kept its scrollbar in the middle of the window. A single pane puts
// its panel on the window's right edge instead — the right window of the
// single-pane app — and the four-pane arrangement keeps one panel per pane.
const app = readFileSync(new URL('./App.tsx', import.meta.url), 'utf8');
const floating = readFileSync(new URL('./components/FloatingPanel.tsx', import.meta.url), 'utf8');
const pane = readFileSync(new URL('./components/ViewPane.tsx', import.meta.url), 'utf8');
const inspector = readFileSync(new URL('./components/RunInspector.tsx', import.meta.url), 'utf8');
const prompts = readFileSync(new URL('./components/PromptLibraryPanel.tsx', import.meta.url), 'utf8');
const config = JSON.parse(readFileSync(new URL('./config.json', import.meta.url), 'utf8'));

test('the shared panel can pin itself to the window edge', () => {
  assert.match(floating, /viewportEndEdge: \{\s*position: 'fixed',\s*insetBlockStart: 0,\s*insetBlockEnd: 0,\s*insetInlineEnd: 0,/, 'the fixed edge spans the window');
  assert.match(floating, /isEdgeFixed = false,/, 'off by default, so the four-pane panels are unchanged');
  assert.match(
    floating,
    /side === 'start'\s*\?\s*styles\.startEdge\s*:\s*isEdgeFixed\s*\?\s*styles\.viewportEndEdge\s*:\s*styles\.endEdge/,
    'only an end panel takes the window edge',
  );
});

test('both right-edge panels pass the flag through', () => {
  for (const [name, file] of [['RunInspector', inspector], ['PromptLibraryPanel', prompts]]) {
    assert.match(file, /isEdgeFixed\?: boolean;/, `${name} accepts the flag`);
    assert.match(file, /isEdgeFixed=\{isEdgeFixed\}/, `${name} forwards it to the panel`);
  }
  assert.match(pane, /isEdgeFixed=\{isPanelAtWindowEdge\}/, 'the pane hands it to the inspector');
  assert.match(
    pane,
    /<PromptLibraryPanel\s+region=\{panelRegion\}\s+isEdgeFixed=\{isPanelAtWindowEdge\}/,
    'and to the prompt library',
  );
});

test('the pane is at the window edge exactly when it is the only one on screen', () => {
  assert.equal(config.conversation_pane_panel_window_edge, true, 'the toggle has a default in config');
  assert.match(
    app,
    /isPanelAtWindowEdge=\{\s*shownPaneKeys\.length === 1 &&\s*appConfig\.conversation_pane_panel_window_edge\s*\}/,
    'one visible pane takes the window edge; several keep their own',
  );
});
