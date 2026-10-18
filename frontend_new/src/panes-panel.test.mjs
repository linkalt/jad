import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import test from 'node:test';

// Desktop had no way to see how many conversations were on screen: the count
// lived only in the mobile top bar's numbers. It now sits in the left footer
// beside a Panes icon — plain text, because Astryx badges are for status, not
// counts — and the icon opens the same left floating window the other footer
// panels use.
const app = readFileSync(new URL('./App.tsx', import.meta.url), 'utf8');
const nav = readFileSync(new URL('./components/SessionNavigation.tsx', import.meta.url), 'utf8');
const panel = readFileSync(new URL('./components/PanesPanel.tsx', import.meta.url), 'utf8');

test('the footer carries the count in its label, not as text beside the icon', () => {
  assert.match(nav, /import \{paneCountLabel\} from '\.\.\/pane-count';/);
  assert.match(nav, /label=\{paneCountLabel\(paneCount, paneLimit\)\}/, 'tooltip and accessible name spell it out');
  assert.match(nav, /content=\{paneCountLabel\(paneCount, paneLimit\)\}/, 'so does the tooltip');
  assert.match(nav, /icon=\{<Icon icon="viewColumns" size="sm" \/>\}/);
  assert.match(nav, /variant=\{openLeftPanel === 'panes' \? 'secondary' : 'ghost'\}/);
  assert.match(nav, /onClick=\{\(\) => onSelectLeftPanel\('panes'\)\}/);
  // The footer is one icon per control: the "3 of 4" text that used to sit beside
  // it is gone, and nothing else in the footer prints the count.
  assert.equal(nav.includes('paneCountText'), false, 'no short count text');
  assert.equal(/<Text[\s\S]{0,120}paneCount/.test(nav), false, 'no count beside the icon');
  assert.equal(nav.includes('Badge'), false, 'a badge is for status, not for a count');
  assert.match(nav, /\/\*\* How many conversations are on screen right now, for the footer's count\. \*\/\s*paneCount: number;/, 'the count arrives as a prop');
});

test('the app passes the live pane count and opens the panel in the left window', () => {
  assert.match(app, /paneCount=\{panes\.keys\.length\}/);
  const block = app.slice(app.indexOf("{leftPanel === 'panes' && ("), app.indexOf('{hasOpenedProfileEditor && ('));
  assert.match(block, /<PanesPanel/, 'the panes view is part of the left floating window');
  assert.match(block, /region=\{leftPanelRegion\}/, 'it shares the one window');
  assert.match(block, /rows=\{panes\.keys\.map\(\(paneViewKey, index\)/, 'one row per pane on screen');
  assert.match(block, /number: index \+ 1/, 'numbered the way the mobile bar numbers them');
  assert.match(block, /title: paneTitleText\(\{/, 'the same title line the pane strip shows');
  assert.match(block, /isRunning: pane\.isRunning/, 'a running pane says so');
  assert.match(block, /isFocused: paneViewKey === activeViewKey/, 'the focused pane is marked');
  assert.match(
    block,
    /focusViewPane\(viewKey\);\s*revealPaneView\(viewKey\);\s*closeLeftPanel\(\);/s,
    'choosing a row focuses it, scrolls the row to it, and closes the window',
  );
  assert.match(block, /limit=\{paneLimit\}/, 'the cap comes from the configuration');
});

test('the panel lists the panes and closes with the window', () => {
  assert.match(panel, /subtitle=\{paneCountSummary\(rows\.length, limit, running\)\}/);
  assert.match(panel, /<FloatingPanel\s*side="start"\s*label="Panes"/);
  assert.match(panel, /isSelected=\{row\.isFocused\}/);
  assert.match(panel, /onClick=\{\(\) => onFocusPane\(row\.viewKey\)\}/);
  assert.match(panel, /<Spinner size="sm" aria-label=\{`Pane \$\{row\.number\} running`\} \/>/);
  assert.equal(panel.includes('IconButton'), false, 'a row holds no second button inside its own click target');
});
