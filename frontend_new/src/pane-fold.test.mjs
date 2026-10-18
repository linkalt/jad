import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import test from 'node:test';

import appConfig from './config.json' with {type: 'json'};
import {foldedPaneTitle, fitPaneWidths, isPaneFolded} from './pane-fold.ts';

// Panes are flex columns, so widening one squeezes its neighbours. Measured on
// desktop: the fourth pane of four was 1px wide at x=1996 in a 1600px window —
// off-screen, still counted as open, and focusing it produced an 8px composer.
// The floor plus the fold is what stops that.
const app = readFileSync(new URL('./App.tsx', import.meta.url), 'utf8');
const column = readFileSync(new URL('./components/PaneColumn.tsx', import.meta.url), 'utf8');
const folded = readFileSync(new URL('./components/FoldedPane.tsx', import.meta.url), 'utf8');
const viewPane = readFileSync(new URL('./components/ViewPane.tsx', import.meta.url), 'utf8');

test('the fold starts below the configured minimum width', () => {
  const min = appConfig.conversation_pane_min_size;
  assert.ok(min > 0);
  assert.equal(isPaneFolded(min - 1, min), true, 'one pixel under the minimum is folded');
  assert.equal(isPaneFolded(min, min), false, 'exactly the minimum still shows the conversation');
  assert.equal(isPaneFolded(min + 500, min), false);
  assert.equal(isPaneFolded(1, min), true, 'the measured 1px pane folds');
});

test('an unmeasured pane is not folded, so the first paint is not a stub', () => {
  assert.equal(isPaneFolded(0), false, 'width 0 means "not measured yet"');
});

test('a folded pane is named by its number and its label', () => {
  assert.equal(foldedPaneTitle(4, 'Atlas onboarding flow'), 'Pane 4: Atlas onboarding flow');
});

test('every pane keeps a floor, and renders the width the fit rule allowed', () => {
  assert.match(
    column,
    /foldWidthPx,\s*widthPx,\s*onSizeChange,\s*onRegisterResize,\s*onResizeStart,\s*onResizeDelta,\s*onResizeEnd,\s*folded,/,
    'the column takes the floor, the allocated width and what to show when folded',
  );
  assert.match(
    column,
    /: \{flex: '0 0 auto', minWidth: foldWidthPx\}/,
    'a sized pane holds still inside its allocated width instead of pushing its neighbour out',
  );
  assert.match(
    column,
    /\? \{flex: '1 1 auto', minWidth: foldWidthPx\}/,
    'the flexible pane absorbs the rounding, with the floor as its minimum',
  );
  assert.match(column, /const isFolded = isPaneFolded\(widthPxMeasured, minSizePx\);/, 'the measured width decides');
  assert.match(column, /width=\{widthPx \?\? \(isFlexible \? undefined : region\.size\)\}/, 'the fit rule can allocate a width');
  assert.match(column, /onSizeChange\?\.\(region\.size\)/, 'the pane reports the width it holds');
  assert.match(column, /\{isFolded \? folded : children\}/, 'a folded pane shows the stub instead');
  assert.match(column, /data-pane-folded=\{isFolded \? 'true' : 'false'\}/, 'the state is inspectable');
});

test('a folded pane has no composer to type into', () => {
  assert.equal(folded.includes('Composer'), false);
  assert.match(folded, /<Spinner size="sm" aria-label=\{`Pane \$\{number\} running`\} \/>/, 'it still says it is running');
  assert.match(folded, /data-pane-folded=""/);
});

test('the app passes the floor, the stub, and keeps every pane reachable', () => {
  assert.match(app, /foldWidthPx=\{appConfig\.conversation_pane_fold_width\}/);
  assert.match(app, /folded=\{\s*<FoldedPane\s*number=\{index \+ 1\}\s*label=\{pane\.label\}\s*isRunning=\{pane\.isRunning\}/);
  assert.match(app, /widthPx=\{paneWidths\[index\]\}/, 'each pane renders its allocated width');
  assert.match(
    app,
    /shownPaneKeys\.indexOf\(activeViewKey\),/,
    'the focused pane is the one the room is spent on first',
  );
  assert.match(
    app,
    /style=\{shownPaneKeys\.length > 1 \? \{overflowX: 'auto'\} : undefined\}/,
    'a row that cannot fit even the floor scrolls instead of hiding a pane',
  );
  assert.match(
    viewPane,
    /data-pane-key=\{viewKey\}/,
    'each pane is addressable by its view key, so the row can scroll to it',
  );
  assert.match(app, /const revealPaneView = \(viewKey: string\): void =>/, 'the recovery path exists');
  assert.match(app, /pane\?\.scrollIntoView\(\{inline: 'nearest', block: 'nearest'\}\)/, 'choosing a pane scrolls to it');
  assert.match(
    app,
    /focusViewPane\(viewKey\);\s*revealPaneView\(viewKey\);\s*closeLeftPanel\(\);/,
    'the panes panel focuses and reveals',
  );
});

test('the floor is a configured value, not a literal', () => {
  assert.equal(appConfig.conversation_pane_fold_width, 56);
  assert.ok(appConfig.conversation_pane_fold_width < appConfig.conversation_pane_min_size);
  assert.equal(/minWidth: \d+/.test(column), false, 'no width literal in the column');
});

test('panes that fit keep the widths the user dragged them to, and the last fills the row', () => {
  assert.deepEqual(fitPaneWidths([560, 560, 560, 560], 2400, 480, 56), [560, 560, 560, 720]);
  // The flex row's last column grows to fill the area, so the plan has to say so
  // too — otherwise the drag, which works from the plan, stops short of the edge.
  assert.deepEqual(fitPaneWidths([616, 560], 1788, 480, 56), [616, 1172]);
});

test('a squeezed row keeps as many usable panes as it can, not proportional slivers', () => {
  // The measured desktop case: four panes dragged to 616/560/560/560 in a
  // 1020px area. Proportional shrinking folded three of them; spending the room
  // in order of need keeps three at the minimum and folds one, and the spare
  // pixels go to the panes that are usable so the row fills exactly.
  const widths = fitPaneWidths([616, 560, 560, 560], 1020, 320, 56);
  assert.deepEqual(widths, [321, 321, 321, 57]);
  assert.equal(widths.reduce((total, width) => total + width, 0), 1020, 'the row is filled');
  assert.equal(widths.filter(width => width >= 320).length, 3, 'three panes stay usable');
  for (const width of widths) {
    assert.ok(width >= 56, 'no pane falls under the fold floor');
  }
});

test('a pane that cannot reach the minimum gets nothing, so no stub takes a conversation\'s room', () => {
  // The composer needs the wider minimum now: at 1020px only one pane of four can
  // be usable, and the others fold to the floor instead of sitting at 428px
  // showing a stub.
  const widths = fitPaneWidths([616, 560, 560, 560], 1020, 480, 56);
  assert.deepEqual(widths, [852, 56, 56, 56]);
  assert.equal(widths.reduce((total, width) => total + width, 0), 1020);
  assert.equal(widths.filter(width => width > 56 && width < 480).length, 0, 'no in-between pane');
});

test('a wider row grows the panes back towards what they asked for', () => {
  assert.deepEqual(fitPaneWidths([616, 560, 560, 560], 1660, 480, 56), [555, 524, 524, 57]);
  assert.deepEqual(
    fitPaneWidths([616, 560, 560, 560], 2400, 480, 56),
    [616, 560, 560, 664],
    'the stored widths are kept and the last pane takes the rest',
  );
});

test('no width ever goes below the floor or above the room', () => {
  for (const available of [0, 56, 200, 224, 500, 1020, 2400]) {
    const widths = fitPaneWidths([616, 560, 560, 560], available, 320, 56);
    assert.equal(widths.length, 4);
    for (const width of widths) {
      assert.ok(width >= 56, `a pane kept its floor at ${available}px`);
    }
    if (available > 0) {
      assert.ok(
        widths.reduce((total, width) => total + width, 0) <= Math.max(available, 224),
        `the plan fits at ${available}px`,
      );
    }
  }
});

test('the focused pane gets its room first, so no composer sits in a folded pane', () => {
  const widths = fitPaneWidths([616, 560, 560, 560], 1020, 320, 56, 3);
  assert.deepEqual(widths, [321, 321, 56, 322], 'the focused pane reaches the minimum before the others');
  assert.equal(widths.filter(width => width >= 320).length, 3, 'the same number of panes stay usable');
});

test('an unmeasured row keeps the stored widths instead of guessing', () => {
  assert.deepEqual(fitPaneWidths([616, 560], 0, 320, 56), [616, 560]);
  assert.deepEqual(fitPaneWidths([], 900, 320, 56), []);
});

test('a stored stub is lifted back to a usable width when the row has room', () => {
  // Closing two of four panes left the row as 56px beside 1284px: a stub whose
  // divider was already at its limit, with no way back to a readable
  // conversation except a drag that had nothing to take.
  const widths = fitPaneWidths([56, 1284], 1340, 480, 56);
  assert.deepEqual(widths, [480, 860], 'the stub reaches the usable minimum');
  assert.equal(widths.reduce((total, width) => total + width, 0), 1340, 'the row still fills the area');
  assert.ok(widths.every(width => width >= 480), 'no pane stays folded while the room is there');
  // A row whose panes are all usable keeps what the user chose.
  assert.deepEqual(fitPaneWidths([616, 560], 1340, 480, 56), [616, 724]);
  // Not enough room for every pane at the minimum: the ladder decides, as before.
  const tight = fitPaneWidths([56, 1284, 560, 560], 1340, 480, 56);
  assert.ok(tight.some(width => width < 480), 'a row that cannot hold four panes still folds');
});
