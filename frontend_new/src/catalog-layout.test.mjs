import assert from 'node:assert/strict';
import {test} from 'node:test';

import {
  DIALOG_CORNERS,
  bodyDefaultHeight,
  bodyHeightBound,
  centredEdgeDelta,
  cornerDelta,
  dialogMaxHeight,
  dialogWidthBound,
  regionFloor,
} from './catalog-layout.ts';

test('the surface cap is the configured share of the window, as the CSS wants it', () => {
  assert.equal(dialogMaxHeight(92), '92dvh');
  assert.equal(dialogMaxHeight(100), '100dvh');
});

test('the body opens at the height it always had', () => {
  // The old CSS cap was min(520px, 62dvh); a draggable region needs the same cap in
  // pixels. Measured at 1054 tall, 62dvh is 653, so the configured 520 wins and the
  // dialog opens exactly where it used to.
  assert.equal(bodyDefaultHeight(1054, 520, 62), 520);
  assert.equal(bodyDefaultHeight(700, 520, 62), 434, 'a short window opens shorter');
  assert.equal(bodyDefaultHeight(1054, 520, 100), 520, 'the configured height is the ceiling too');
});

test('the body stops where the surface stops it', () => {
  // Measured: the surface is clamped to 92% of the window and is 65px taller than
  // the body (footer plus the surface's own edges), so at 1054 tall the body cannot
  // pass 904 — 904 + 65 = 969 against a 969.68 ceiling.
  assert.equal(bodyHeightBound(1054, 92, 65), 904);
  assert.equal(bodyHeightBound(800, 92, 65), 671);
  assert.equal(bodyHeightBound(2000, 92, 65), 1775);
});

test('an absurdly short window still yields a usable bound', () => {
  assert.equal(bodyHeightBound(60, 92, 65), 1);
  assert.equal(bodyHeightBound(0, 92, 65), 1);
});

test('the dialog stops where the window stops it, not only at the configured maximum', () => {
  // The numbers the browser reported: 1238 wide window, the surface clamped to
  // 1206 by its own CSS, and a stored size that ran ahead of it to 1240.
  assert.equal(dialogWidthBound(1238, 32, 1600), 1206);
  assert.equal(dialogWidthBound(900, 32, 1600), 868);
  assert.equal(dialogWidthBound(2560, 32, 1600), 1600, 'a wide window keeps the configured ceiling');
});

test('an absurdly narrow window still yields a usable bound', () => {
  assert.equal(dialogWidthBound(20, 32, 1600), 1);
  assert.equal(dialogWidthBound(0, 32, 1600), 1);
});

test('a centred dialog is asked for twice the distance the pointer travelled', () => {
  // Measured before the change: a 100 px drag widened the box by 100 and moved the
  // edge under the cursor by 50 — on either axis, because both are centred. The
  // region has to hear twice the distance for the edge to stay with the pointer.
  assert.equal(centredEdgeDelta(1), 2);
  assert.equal(centredEdgeDelta(100), 200);
  assert.equal(centredEdgeDelta(-60), -120);
  assert.equal(centredEdgeDelta(0), 0, 'a still pointer asks for nothing');
});

test('a floor the window cannot paint follows the window down', () => {
  // Measured at a 640 px window before the change: the region clamped to 608 and the
  // surface painted 608, but the region still reported 720 as its minimum — a floor
  // nothing can show, held together only because the hook's clamp lets the maximum win.
  assert.equal(regionFloor(720, 1206), 720, 'a window with room keeps the configured floor');
  assert.equal(regionFloor(720, 608), 608, 'a window without room pulls the floor down to what it can paint');
  assert.equal(regionFloor(300, 671), 300);
  assert.equal(regionFloor(300, 303), 300, 'three pixels of room still hold the floor');
  assert.equal(regionFloor(300, 200), 200);
  assert.equal(regionFloor(720, 720), 720, 'a floor equal to the bound is already honest');
  // The invariant: a region can never be asked to be larger than the paint box.
  for (const bound of [1, 200, 608, 720, 1206, 4000]) {
    assert.ok(regionFloor(720, bound) <= bound, 'never above the bound: ' + bound);
    assert.ok(regionFloor(720, bound) > 0, 'never zero either: ' + bound);
  }
});

test('a corner asks both regions for their share of one gesture', () => {
  // The browser's acceptance table, as the rules the grips feed. A corner drags
  // outwards, so the start-side corners grow on a negative distance and the block
  // axis mirrors it; the doubling is the edge's own, so both agree by construction.
  const grown = (corner, dx, dy) => {
    const delta = cornerDelta(corner, dx, dy);
    return {width: centredEdgeDelta(delta.inline), height: centredEdgeDelta(delta.block)};
  };
  assert.deepEqual(grown('bottom-end', 40, 30), {width: 80, height: 60}, 'bottom-right outwards');
  assert.deepEqual(grown('top-start', -40, -30), {width: 80, height: 60}, 'top-left outwards');
  assert.deepEqual(grown('top-end', 40, -30), {width: 80, height: 60});
  assert.deepEqual(grown('bottom-start', -40, 30), {width: 80, height: 60});
  assert.deepEqual(grown('bottom-end', 0, 30), {width: 0, height: 60}, 'one axis at a time');
  assert.deepEqual(grown('top-start', -60, 0), {width: 120, height: 0});
  assert.deepEqual(grown('bottom-end', -40, -30), {width: -80, height: -60}, 'inwards narrows');
  assert.equal(DIALOG_CORNERS.length, 4);
  assert.deepEqual([...DIALOG_CORNERS].sort(), ['bottom-end', 'bottom-start', 'top-end', 'top-start'], 'four distinct corners');
  assert.equal(new Set(DIALOG_CORNERS).size, 4, 'no corner twice');
});
