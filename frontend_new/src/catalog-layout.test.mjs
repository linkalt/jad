import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import {test} from 'node:test';

import {
  DIALOG_CORNERS,
  bodyDefaultHeight,
  bodyHeightBound,
  centredEdgeDelta,
  columnHandles,
  columnWidthAt,
  cornerDelta,
  dialogMaxHeight,
  dialogWidthBound,
  headerColumnFloorPx,
  regionFloor,
  tableMinimumWidthPx,
} from './catalog-layout.ts';

const config = JSON.parse(readFileSync(new URL('./config.json', import.meta.url), 'utf8'));

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

/**
 * The catalog's profile table in render order — five columns, the widest set any kind
 * shows — as the resize plugin reads it: the three text columns at their shared weight
 * with their shared floor, Session among them, Category with no width of its own (weight
 * 1 and the library's 60px flexible floor, which is the `aria-valuenow` its handle
 * reported), and Action last as the table's flexible remainder.
 */
function profileColumns(overrides = {}) {
  const text = {weight: config.catalog_table_text_column_weight, minWidthPx: config.catalog_table_text_column_min_width_px};
  const columns = [
    {key: 'sessions', ...text},
    {key: 'category', weight: 1, minWidthPx: 60},
    {key: 'id', ...text},
    {key: 'name', ...text},
    {key: 'action', weight: config.catalog_action_column_weight, minWidthPx: config.catalog_action_column_min_width_px},
  ];
  return columns.map(column => ({...column, ...(overrides[column.key] ?? {})}));
}

test('the width model reproduces the widths the browser painted', () => {
  // Calibration against the measured row (Firefox, 1440x820, table 843px, Action still a
  // fixed 64): 209.73 / 149.82 / 209.73 / 209.73 / 64. If the model did not land on those,
  // the numbers below would be arithmetic about nothing.
  const before = profileColumns({action: {weight: null, minWidthPx: 64}});
  const near = (actual, expected) => assert.ok(Math.abs(actual - expected) < 0.05, `${actual} is not ${expected}`);
  near(columnWidthAt(before, 'sessions', 843), 209.73);
  near(columnWidthAt(before, 'category', 843), 149.82);
  near(columnWidthAt(before, 'id', 843), 209.73);
  near(columnWidthAt(before, 'name', 843), 209.73);
  assert.equal(columnWidthAt(before, 'action', 843), 64, 'a fixed column ignores the table width');
});

test('every column boundary carries exactly one handle and the last column none', () => {
  const handles = columnHandles(profileColumns());
  assert.deepEqual(
    handles.map(handle => handle.key),
    ['sessions', 'category', 'id', 'name', 'action'],
    'every column takes part, in render order',
  );
  // The two defects this replaced, both of them boundary arithmetic: with `resizable: false`
  // the plugin dropped Action from its set, and Name — then the last flexible column — got
  // no handle at all, so nothing could drag the name|action boundary.
  assert.deepEqual(
    handles.filter(handle => handle.hasHandle).map(handle => handle.key),
    ['sessions', 'category', 'id', 'name'],
    'one handle per boundary, and none on the table\'s right edge',
  );
  assert.equal(handles.at(-1).hasHandle, false, 'the remainder column owns no handle of its own');
  assert.equal(handles.find(handle => handle.key === 'name').neighbor, 'action', 'Name trades width with Action');
  assert.equal(handles.find(handle => handle.key === 'id').neighbor, 'name', 'and ID with Name, as before');
  assert.equal(handles.find(handle => handle.key === 'action').neighbor, null, 'nothing sits beyond the remainder');
  assert.equal(handles.filter(handle => handle.hasHandle).length, handles.length - 1, 'no boundary twice, none missing');
});

test('the shape this change rejected loses the name|action boundary or grows an orphan handle', () => {
  // Negative controls for the assertion above, both shapes the column could have had.
  // 1. Opting out: the plugin filters the column out of its set, and the last flexible
  //    column left behind is Name — which then owns no handle. That is the defect.
  const opted = columnHandles(profileColumns({action: {isResizable: false}}));
  assert.equal(opted.some(handle => handle.key === 'action'), false, 'an opted-out column never enters a drag snapshot');
  assert.equal(opted.find(handle => handle.key === 'name').hasHandle, false, 'so the last flexible column keeps no handle');
  // 2. A fixed last column that stays in the set: it grows a handle on the table's right
  //    edge, where the plugin's last-column rule overwrites the width the drag computes
  //    with `tableWidth - sum(others)` and drops that snapshot from the committed updates —
  //    a control that says "resize column Action" and cannot.
  const fixed = columnHandles(profileColumns({action: {weight: null, minWidthPx: 72}}));
  assert.equal(fixed.at(-1).hasHandle, true, 'a fixed last column does get a handle');
  assert.equal(fixed.at(-1).neighbor, null, 'with nothing on its far side to trade width with');
});

test('the Action column cannot be narrower than its own header', () => {
  // Measured at the table's 600 14px: "Action" is 43.82 in Inter, 43.55 in Arial, 44.6 in
  // Tahoma, 49.85 in Verdana and 50.22 in DejaVu Sans. Inter is fetched from Google Fonts
  // with `display=swap`, so a blocked or slow fetch lands on one of the last two — which is
  // what the Firefox reporter saw at the old fixed 64: 50.22 of text in a 48px text box,
  // painted `Acti…`.
  const widestMeasuredTextPx = 50.22;
  const paddingPx = 8;
  const need = headerColumnFloorPx(widestMeasuredTextPx, paddingPx);
  assert.equal(need, 67, 'the worst case: 50.22 of text plus 8 and 8, rounded up to a whole pixel');
  const floor = config.catalog_action_column_min_width_px;
  assert.ok(floor >= need, `the whole word fits in the widest fallback measured (floor ${floor}, need ${need})`);
  assert.ok(floor - need <= 8, `and the headroom stays tight rather than decorative (${floor - need} px)`);
  assert.ok(floor < 120, 'the floor is not the 120px proportional() default');
  // The floor is not decoration: it is what the column renders at when the table is
  // squeezed to its own minimum width, and the resting width at the dialog's default
  // table width clears it. 5.78px of headroom over the text means a fallback font up to
  // 11% wider than DejaVu Sans still fits; zoom and a raised browser font size do not
  // need more, because the header's 14px text and its 8px padding are both rem-derived
  // (a 20px root measured 17.5px text and 10px padding) and so scale together.
  const columns = profileColumns();
  const squeezed = columnWidthAt(columns, 'action', tableMinimumWidthPx(columns));
  assert.ok(squeezed >= floor, `never under its floor, even at the narrowest table (${squeezed.toFixed(2)})`);
  assert.ok(squeezed < floor + 2, 'and not meaningfully above it either: the floor is what the squeeze lands on');
  const resting = columnWidthAt(columns, 'action', 843);
  assert.ok(resting >= need + 2 * paddingPx, `and opens wider than the floor at the default table width (${resting.toFixed(2)})`);
  // Still an icon column, not a strip: narrower than a text column's share, and under
  // twice the floor.
  assert.ok(resting < columnWidthAt(columns, 'name', 843), 'narrower than the columns it sits beside');
  assert.ok(resting < 2 * floor, `and under twice its floor (${resting.toFixed(2)})`);
});

test('the table fits the pane at the narrowest dialog, and the Action column is why it does', () => {
  // The pane, measured in both engines with the dialog dragged to its 720px floor: 523px
  // of content box — the 720 less the 140px file list (catalog_file_list_width_px) and
  // 57px of pane padding and the gaps either side of it. A table wider than this scrolls
  // its last column out of the pane, and the last column is the one the user is reaching
  // for: the delete button.
  const PANE_AT_MIN_DIALOG_PX = 523;
  const columns = profileColumns();
  const tableMinimum = tableMinimumWidthPx(columns);
  assert.ok(tableMinimum <= PANE_AT_MIN_DIALOG_PX, `the table minimum (${tableMinimum.toFixed(2)}) fits the narrowest pane (${PANE_AT_MIN_DIALOG_PX})`);
  // What the weight has to do with it: the plugin's cost for a flexible column is
  // floor * totalWeight / weight, which falls below the text columns' own term
  // (120 * totalWeight / 1.4) only from 72 * 1.4 / 120 = 0.84 upwards — above that the
  // floor costs the table nothing. Weight 0.6 was the regression: 696px, 173px more than
  // the pane. 0.8 still misses; 0.84 is where the Action term stops being the largest.
  assert.equal(Math.round(tableMinimumWidthPx(profileColumns({action: {weight: 0.6}}))), 696, 'the weight this column had');
  assert.ok(tableMinimumWidthPx(profileColumns({action: {weight: 0.8}})) > PANE_AT_MIN_DIALOG_PX, 'and 0.8 still overflows the pane');
  assert.ok(tableMinimumWidthPx(profileColumns({action: {weight: 0.84}})) <= PANE_AT_MIN_DIALOG_PX, '0.84 is the point where it stops');
  assert.ok(tableMinimum < tableMinimumWidthPx(profileColumns({action: {weight: 0.8}})), 'so the configured weight is past that point');
  // Resting widths, for the next person: 73.5 at the narrowest pane, 118.4 at the default
  // dialog's 843px table, 197.1 at the widest dialog's 1403px table.
  const at = tableWidth => Math.round(columnWidthAt(columns, 'action', tableWidth));
  assert.equal(at(PANE_AT_MIN_DIALOG_PX), 73);
  assert.equal(at(843), 118);
  assert.equal(at(1403), 197);
});
