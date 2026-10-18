import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import test from 'node:test';

import appConfig from './config.json' with {type: 'json'};
import {movePaneBoundary} from './pane-resize.ts';

// Three reports shaped this rule. First, resizing a middle pane's right divider
// also resized the pane on its left, because the row re-derived every width from
// the stored ones on each move. Second, the divider jammed against the next pane
// the moment that pane was folded instead of folding the row the way a window
// resize does. Third, a divider after a stub was a dead control when the only
// pane with room left was on its left. So: the right side gives first, the left
// gives only when the right is exhausted, and every other pane keeps its width.
const app = readFileSync(new URL('./App.tsx', import.meta.url), 'utf8');
const column = readFileSync(new URL('./components/PaneColumn.tsx', import.meta.url), 'utf8');

const MIN = appConfig.conversation_pane_min_size;
const FOLD = appConfig.conversation_pane_fold_width;
const total = widths => widths.reduce((sum, width) => sum + width, 0);

test('dragging right grows the pane and takes the room from the pane beside it', () => {
  const before = [555, 524, 524, 57];
  const after = movePaneBoundary(before, 1, 60, MIN, FOLD);
  assert.deepEqual(after, [555, 584, 464, 57]);
  assert.equal(total(after), total(before), 'the row keeps its width');
});

test('a drag the right side can absorb leaves everything on the left alone', () => {
  const before = [555, 524, 524, 57];
  // The panes to the right hold 468px above their floors, and they grow without
  // limit when the drag goes the other way.
  for (const delta of [-400, -60, 0, 60, 400]) {
    const after = movePaneBoundary(before, 1, delta, MIN, FOLD);
    assert.equal(after[0], before[0], `the pane on the left kept its width at ${delta}`);
  }
});

test('the panes right of the divider stop at the floor, and so does the drag', () => {
  // Reported: dragging pane 2's divider all the way right shrank pane 1, so
  // "resize to the right" read as "resize to the left". A rightward drag now
  // spends only the room on its own side and stops when that is gone.
  const before = [555, 524, 60, 57];
  const after = movePaneBoundary(before, 1, 400, MIN, FOLD);
  assert.equal(after[2], FOLD, 'the nearest pane keeps the floor');
  assert.equal(after[3], FOLD, 'and so does the one after it, which gave the last pixel');
  assert.equal(after[1], 529, 'the dragged pane grows by what the right could give');
  assert.equal(after[0], 555, 'the pane on the left is not touched');
  assert.equal(total(after), total(before));
});

test('a already folded neighbour gives nothing, and the pane after it gives instead', () => {
  const after = movePaneBoundary([555, 480, FOLD, 200], 1, 100, MIN, FOLD);
  assert.deepEqual(after, [555, 580, FOLD, 100], 'the folded pane is skipped, the next one gives');
});

test('a rightward drag never reaches past the pane it moves', () => {
  // The row a narrow window ends up with: one wide pane and three stubs.
  const before = [852, 56, 56, 56];
  for (const index of [0, 1, 2]) {
    const after = movePaneBoundary(before, index, 300, MIN, FOLD);
    assert.equal(after[0], before[0], `pane 1 kept its width when divider ${index + 1} moved right`);
    assert.deepEqual(after, before, 'and nothing could grow, because the right side is at the floor');
  }
  // A leftward drag on a stub's divider does move: the pane it cannot shrink
  // takes the room from its left (see the test further down).
  assert.notDeepEqual(movePaneBoundary(before, 1, -300, MIN, FOLD), before);
});

test('dragging left shrinks the pane and hands the width to the folded pane first', () => {
  const after = movePaneBoundary([555, 700, FOLD, FOLD], 1, -100, MIN, FOLD);
  assert.equal(after[1], 600, 'the dragged pane gives up the width');
  assert.equal(after[2], FOLD + 100, 'the folded neighbour is filled before the next pane gets anything');
  assert.equal(after[3], FOLD);
});

test('a stub can be dragged back down to its floor, so a drag is undoable', () => {
  const grown = [552, 356, FOLD, FOLD];
  const back = movePaneBoundary(grown, 1, -300, MIN, FOLD);
  // What it gives up goes to the next pane, nearest first: the drag is undone,
  // and the row keeps its width.
  assert.deepEqual(back, [552, FOLD, 356, FOLD]);
  assert.equal(total(back), total(grown));
});

test('dragging left folds the dragged pane, just as growing its neighbour can', () => {
  // Reported: "resize to the left sticks, but resizing all the way to the right
  // clamps pane 1". Shrinking used to stop at the usable minimum, so the same
  // pane could be folded to the floor one way and never the other.
  const before = [555, MIN, 524, 57];
  const shrunk = movePaneBoundary(before, 1, -100, MIN, FOLD);
  assert.deepEqual(shrunk, [555, MIN - 100, 624, 57], 'the pane goes below the minimum, the neighbour takes the room');
  assert.equal(total(shrunk), total(before), 'the row keeps its width');
  const floored = movePaneBoundary(before, 1, -900, MIN, FOLD);
  assert.equal(floored[1], FOLD, 'the drag stops at the fold floor, not at the usable minimum');
  assert.equal(total(floored), total(before));
  const back = movePaneBoundary(floored, 1, 200, MIN, FOLD);
  assert.ok(back[1] > floored[1], 'dragging back out restores the pane');
  assert.equal(total(back), total(before));
});

test('the last pane has no divider, and an unknown index moves nothing', () => {
  const before = [555, 524, 524, 57];
  assert.deepEqual(movePaneBoundary(before, 3, 100, MIN, FOLD), before);
  assert.deepEqual(movePaneBoundary(before, -1, 100, MIN, FOLD), before);
  assert.deepEqual(movePaneBoundary(before, 9, 100, MIN, FOLD), before);
});

test('a row where every pane is at the floor has nothing to give', () => {
  const before = [FOLD, FOLD, FOLD, FOLD];
  assert.deepEqual(movePaneBoundary(before, 0, 200, MIN, FOLD), before);
  assert.deepEqual(movePaneBoundary(before, 1, 200, MIN, FOLD), before);
});

test('the drag follows the pointer on the window, in the capture phase', () => {
  // `ResizeHandle` stops the pointer's propagation, so a bubbling window listener
  // never saw the release: the drag stayed armed after the button was let go and
  // the panes kept following the mouse. Capture runs before anything can stop it.
  assert.match(column, /window\.addEventListener\('pointerdown', trackDown, true\);/, 'the press position is read from the window');
  assert.match(column, /const follow = \(event: PointerEvent\) => \{[\s\S]*?callbacksRef\.current\.onResizeDelta\?\.\(event\.clientX - startX\);/, 'every window move drives the boundary');
  assert.match(column, /window\.addEventListener\('pointermove', follow, true\);/, 'moves are followed on the window');
  assert.match(column, /window\.addEventListener\('pointerup', finish, true\);/, 'a release anywhere ends it, and capture is what delivers that');
  assert.match(column, /window\.addEventListener\('pointercancel', finish, true\);/, 'so does an interrupted gesture');
  assert.match(column, /window\.addEventListener\('lostpointercapture', finish, true\);/, 'so does a lost pointer capture');
  assert.doesNotMatch(column, /window\.addEventListener\('pointerup', finish\);/, 'a bubbling release listener is the bug being fixed');
});

test('a release that never arrives ends the drag instead of leaving it armed', () => {
  assert.match(column, /if \(event\.buttons === 0\) \{\s*\n\s*endGestureRef\.current\?\.\(\);\s*\n\s*return;/, 'a move with no button held heals the gesture');
  assert.match(column, /if \(endGestureRef\.current !== finish\) \{\s*\n\s*return;\s*\n\s*\}/, 'ending is idempotent, so a second release cannot double-commit');
  assert.match(column, /useEffect\(\(\) => \(\) => \{\s*\n\s*endGestureRef\.current\?\.\(\);\s*\n\s*\}, \[\]\);/, 'a pane unmounted mid-drag ends it');
});

test('the divider is renewed after every gesture, so a missed pointerup cannot kill it', () => {
  // Astryx's ResizeHandle keeps its gesture in a ref and clears it only from the
  // events it sees; a drag released outside the window leaves it set, and every
  // later press bails out silently. A fresh handle per gesture fixes that.
  assert.match(column, /const \[handleGeneration, setHandleGeneration\] = useState\(0\);/, 'the column counts gestures');
  assert.match(column, /setHandleGeneration\(generation => generation \+ 1\);/, 'a finished gesture renews the handle');
  assert.match(column, /key=\{`pane-divider-\$\{handleGeneration\}`\}/, 'the handle is remounted, not just re-rendered');
});

test('the pane column reports the delta instead of resizing itself', () => {
  assert.match(column, /_onResizeStart: \(\) => \{\s*region\.props\._onResizeStart\(\);[\s\S]*?if \(isPointerDownRef\.current && pointerDownXRef\.current !== null\) \{\s*beginGesture\(pointerDownXRef\.current\);\s*\}[\s\S]*?callbacksRef\.current\.onResizeStart\?\.\(\);/, 'the start reaches the row and opens the gesture');
  assert.match(column, /_onResizeMove: \(delta: number\) => \{[\s\S]*?region\.props\._onResizeMove\(delta\);[\s\S]*?if \(endGestureRef\.current === null\) \{\s*callbacksRef\.current\.onResizeDelta\?\.\(delta\);/, 'a keyboard step moves the row too');
  assert.match(column, /resizable=\{resizeProps\}/, 'the handle uses the wrapped props');
  assert.match(column, /onRegisterResize\?\.\(region\.resize\)/, 'the row can commit a moved boundary');
  assert.match(column, /const callbacksRef = useRef\(\{onResizeStart, onResizeDelta, onResizeEnd\}\);/, 'the gesture calls the latest props, not the ones it was installed with');
});

test('the row moves the boundary and then commits it to the panes', () => {
  assert.match(app, /import \{movePaneBoundary\} from '\.\/pane-resize';/);
  assert.match(app, /const moved = movePaneBoundary\(\s*base,\s*index,\s*delta,\s*appConfig\.conversation_pane_min_size,\s*appConfig\.conversation_pane_fold_width,/, 'the drag is a boundary move with configured sizes');
  assert.match(app, /const base = paneDragBaseRef\.current;\s*\n\s*if \(base === null \|\| base\.length < 2\) \{\s*\n\s*return;\s*\n\s*\}/, 'a move with no gesture open is ignored');
  assert.match(app, /const paneWidths = paneDragWidths \?\? paneFittedWidths;/, 'a drag outranks the fit rule');
  assert.match(app, /committed\.forEach\(\(widthPx, index\) => paneResizeSettersRef\.current\.get\(index\)\?\.\(widthPx\)\);/, 'the moved widths are committed to the panes');
  assert.match(app, /onResizeStart=\{\(\) => startPaneDrag\(index\)\}/);
  assert.match(app, /onResizeDelta=\{delta => dragPaneBoundary\(index, delta\)\}/);
  assert.match(app, /onResizeEnd=\{endPaneDrag\}/);
  assert.match(app, /setPaneDragWidths\(null\);\s*\}\s*,\s*\[panes\.keys\.length, paneGroupMeasuredPx\]\)/, 'a new row shape drops the drag result');
});

test('a divider after a stub still moves left, taking the room from its left', () => {
  // Reported with three panes clamped to [wide, 56, 56]: dragging the divider
  // between the two stubs did nothing, while the divider before it worked. The
  // stub keeps its floor; the pane on its left gives the room.
  const before = [1080, FOLD, 140];
  const moved = movePaneBoundary(before, 1, -100, MIN, FOLD);
  assert.deepEqual(moved, [980, FOLD, 240], 'the stub stays at the floor, its right grows');
  assert.equal(total(moved), total(before), 'the row keeps its width');
  // Far enough to exhaust the left as well: then the row cannot move.
  const stuck = movePaneBoundary([FOLD, FOLD, 200], 1, -100, MIN, FOLD);
  assert.deepEqual(stuck, [FOLD, FOLD, 200], 'a row with nothing left to give stays put');
  // Dragging right from the same clamped row grows the stub out of the pane on
  // its right, down to that pane's floor, and leaves pane 1 alone.
  const right = movePaneBoundary(before, 1, 100, MIN, FOLD);
  assert.deepEqual(right, [1080, 140, 56], 'to the right the stub grows out of its neighbour only');
});
