import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import test from 'node:test';

// The catalog settings dialog: both of its axes are dragged, every row of the
// file table ends in a delete control, and one confirm asks every question —
// including whether a session's transcripts and tool state go with it.
//
// Source assertions, like the rest of these tests: the repo has no React render
// harness, so what is pinned is the wiring and the props, not the pixels. The
// geometry itself was measured in the running app; the numbers quoted below are
// those measurements, and `catalog-layout.test.mjs` pins the rules they feed.
const component = name => readFileSync(new URL(`./components/${name}.tsx`, import.meta.url), 'utf8');
const dialog = component('CatalogEditorDialog');
const pane = component('CatalogListPane');
const confirm = component('CatalogConfirmDialog');
const listView = readFileSync(new URL('./catalog-list-view.ts', import.meta.url), 'utf8');
const config = JSON.parse(readFileSync(new URL('./config.json', import.meta.url), 'utf8'));

/** The action column's slice of the column builder, where the cell is written. */
function actionColumn() {
  const start = pane.indexOf("key: 'action'");
  assert.ok(start > 0, 'the table has an action column');
  return pane.slice(start, pane.indexOf('return columns;', start));
}

test('the dialog sizes and the confirm width are configuration, not literals', () => {
  for (const key of [
    'catalog_dialog_width_px',
    'catalog_dialog_min_width_px',
    'catalog_dialog_max_width_px',
    'catalog_dialog_viewport_margin_px',
    'catalog_dialog_max_height_percent',
    'catalog_dialog_chrome_height_px',
    'catalog_body_height_px',
    'catalog_body_min_height_px',
    'catalog_action_column_width_px',
    'catalog_confirm_dialog_width_px',
  ]) {
    assert.equal(typeof config[key], 'number', `${key} must exist`);
    assert.ok(config[key] > 0, `${key} must be a positive size`);
  }
  assert.ok(config.catalog_dialog_min_width_px < config.catalog_dialog_width_px, 'the default sits inside the bounds');
  assert.ok(config.catalog_dialog_width_px < config.catalog_dialog_max_width_px, 'and nothing can drag past them');
  assert.ok(config.catalog_body_min_height_px < config.catalog_body_height_px, 'the body default clears its floor');
  // The floor is not merely "a little shorter than the default": the file menu has
  // to stay usable while the table still gets a few rows. Measured in the app: a
  // table row is 37px, the header row 29px, the pane's own chrome (padding, gaps,
  // the search row and the count line) is 149px — so 300 leaves four whole rows.
  const rows = Math.floor((config.catalog_body_min_height_px - 149) / 37);
  assert.ok(rows >= 4, `the narrowest body still shows whole rows (${rows})`);
  const floors = 3 * config.catalog_table_text_column_min_width_px + config.catalog_action_column_width_px;
  assert.ok(
    config.catalog_dialog_min_width_px > config.catalog_file_list_width_px + floors,
    'the narrowest width still leaves the table its column floors',
  );
});

test('both axes are dragged regions, each remembered under its own key', () => {
  assert.match(dialog, /const widthRegion = useResizable\(\{[\s\S]*?autoSaveId: 'jad-catalog-dialog-width',\s*\}\)/);
  assert.match(dialog, /direction: 'vertical',\s*autoSaveId: 'jad-catalog-dialog-height',/, 'the height has its own region and key');
  assert.match(dialog, /defaultSize: pixel\(appConfig\.catalog_dialog_width_px\)/);
  assert.match(dialog, /defaultSize: pixel\(bodyDefaultHeight\(viewport\.height, appConfig\.catalog_body_height_px, appConfig\.catalog_body_viewport_percent\)\)/, 'the body opens at the cap it always had');
  assert.match(dialog, /<Dialog[\s\S]*?width=\{widthRegion\.size\}/, 'the surface takes the dragged width');
  assert.match(dialog, /height=\{heightRegion\.size\}/, 'the body takes the dragged height');
  assert.match(dialog, /maxHeight=\{dialogMaxHeight\(appConfig\.catalog_dialog_max_height_percent\)\}/);
  assert.equal(/width=\{\d/.test(dialog), false, 'no hard-coded width');
  assert.equal(/height=\{\d/.test(dialog), false, 'no hard-coded height');
  assert.equal((dialog.match(/appConfig\.catalog_dialog_width_px/g) ?? []).length, 1, 'the default is read once');
});

test('each floor is reconciled with what the window can paint', () => {
  // Measured at a 640 px window before the change: the region clamped and painted 608,
  // yet it still reported 720 as its minimum — `aria-valuemin` above its own
  // `aria-valuemax`. Nothing overflowed only because the hook's clamp lets the maximum
  // win; a region larger than its paint box is the one way this dialog could ever need
  // to scroll, so the floor now follows the bound instead of relying on that.
  assert.match(dialog, /const widthFloor = regionFloor\(appConfig\.catalog_dialog_min_width_px, widthBound\)/);
  assert.match(dialog, /const heightFloor = regionFloor\(appConfig\.catalog_body_min_height_px, heightBound\)/);
  assert.match(dialog, /minSize: pixel\(widthFloor\)/);
  assert.match(dialog, /minSize: pixel\(heightFloor\)/);
  assert.equal((dialog.match(/appConfig\.catalog_dialog_min_width_px/g) ?? []).length, 1, 'the configured width floor is read once, into the rule');
  assert.equal((dialog.match(/appConfig\.catalog_body_min_height_px/g) ?? []).length, 1, 'and so is the height floor');
  assert.ok(dialog.indexOf('const widthBound') < dialog.indexOf('const widthFloor'), 'the floor is derived from the bound, not the other way round');
  assert.ok(dialog.indexOf('const heightBound') < dialog.indexOf('const heightFloor'), 'on both axes');
  assert.equal(/minSize: pixel\(appConfig\./.test(dialog), false, 'no region is handed the raw configured floor');
});

test('each ceiling is what the surface can paint, and both follow the window', () => {
  // Measured before the fix: a 200 px width drag on a 1238 px window left 1206
  // painted and 1240 stored, and the next drag moved the edge by nothing for its
  // first 34 px. The height had the same shape waiting in 92dvh. Both bounds come
  // from the window, so both are reachable in the first drag.
  assert.match(dialog, /const widthBound = dialogWidthBound\(\s*viewport\.width,\s*appConfig\.catalog_dialog_viewport_margin_px,\s*appConfig\.catalog_dialog_max_width_px,\s*\)/);
  assert.match(dialog, /const heightBound = bodyHeightBound\(\s*viewport\.height,\s*appConfig\.catalog_dialog_max_height_percent,\s*appConfig\.catalog_dialog_chrome_height_px,\s*\)/);
  assert.match(dialog, /maxSize: pixel\(widthBound\)/);
  assert.match(dialog, /maxSize: pixel\(heightBound\)/);
  assert.equal((dialog.match(/appConfig\.catalog_dialog_max_width_px/g) ?? []).length, 1, 'the configured maximum is only the second ceiling');
  assert.match(dialog, /const \[viewport, setViewport\] = useState\(\(\) => \(\{width: window\.innerWidth, height: window\.innerHeight\}\)\)/);
  assert.match(dialog, /window\.addEventListener\('resize', onResize\)/);
  assert.match(dialog, /return \(\) => window\.removeEventListener\('resize', onResize\)/, 'and it is taken down again');
  assert.match(dialog, /setViewport\(\{width: window\.innerWidth, height: window\.innerHeight\}\)/, 'a shrinking window re-clamps both');
});

test('a centred drag asks the region for twice the pointer distance, on both axes', () => {
  // Measured: a 100 px drag on either edge widened the box by 100 and moved the
  // edge under the cursor by 50, because the dialog grows from its middle.
  assert.match(dialog, /_onResizeMove: \(delta: number\) => region\.props\._onResizeMove\(centredEdgeDelta\(delta\)\)/);
  assert.match(dialog, /const widthDragProps = centredProps\(widthRegion\);/);
  assert.match(dialog, /const heightDragProps = centredProps\(heightRegion\);/);
  assert.equal((dialog.match(/centredEdgeDelta\(delta\)/g) ?? []).length, 1, 'one doubling, shared by both handles');
  assert.match(dialog, /resizable=\{widthDragProps\}/);
  assert.match(dialog, /resizable=\{heightDragProps\}/);
  assert.equal(/resizable=\{widthRegion\.props\}/.test(dialog), false, 'the bare region would lag the pointer');
});

test('closing the dialog leaves both dragged sizes alone', () => {
  const reset = dialog.slice(dialog.indexOf('if (!isOpen) {'), dialog.indexOf('}, [isOpen]);'));
  assert.ok(reset.length > 0, 'the dialog still resets what the list was showing on close');
  assert.match(reset, /setColumnWidths\(\{\}\)/, 'the column widths it owns are cleared');
  assert.equal(/widthRegion|heightRegion/.test(reset), false, 'the regions, which persist themselves, are not');
});

test('all four edges are handles, and each drags outwards', () => {
  const handles = [...dialog.matchAll(/<ResizeHandle[\s\S]*?\/>/g)].map(match => match[0]);
  assert.equal(handles.length, 4, 'one per edge');
  const labelled = Object.fromEntries(handles.map(tag => [/label="([^"]+)"/.exec(tag)[1], tag]));
  assert.equal(Object.keys(labelled).length, 4, 'each edge says which one it is');
  // Inline, the handle was a full-height rule 16px inside the surface: a stray panel
  // divider rather than an edge. The surface is the nearest positioned ancestor, so
  // an overlay lands on the edge itself.
  for (const tag of handles) assert.equal(/hasDivider/.test(tag), false, 'no rule inside the dialog');

  // The two the library anchors itself: end edges, outward drag is positive.
  assert.match(labelled['Resize catalog dialog'], /direction="horizontal"[\s\S]*?resizable=\{widthDragProps\}/);
  assert.match(labelled['Resize catalog dialog height'], /direction="vertical"[\s\S]*?resizable=\{heightDragProps\}/);
  for (const label of ['Resize catalog dialog', 'Resize catalog dialog height']) {
    assert.equal(/isReversed/.test(labelled[label]), false, `${label} grows on a positive drag`);
    assert.match(labelled[label], /\s(isAlwaysVisible)\s/, 'the pills the user is told about stay');
  }

  // The start edges: reversed so a drag outwards grows too, re-anchored onto the edge
  // the library's band does not use, and quiet at rest.
  assert.match(
    labelled['Resize catalog dialog from the left edge'],
    /direction="horizontal"[\s\S]*?isAlwaysVisible=\{false\}[\s\S]*?isReversed[\s\S]*?xstyle=\{frameStyles\.startEdge\}[\s\S]*?resizable=\{widthDragProps\}/,
  );
  assert.match(
    labelled['Resize catalog dialog from the top edge'],
    /direction="vertical"[\s\S]*?isAlwaysVisible=\{false\}[\s\S]*?isReversed[\s\S]*?xstyle=\{frameStyles\.topEdge\}[\s\S]*?resizable=\{heightDragProps\}/,
  );
  // Eight grips would read as decoration; the four the library does not anchor stay bare.
  assert.equal((dialog.match(/\sisAlwaysVisible\s/g) ?? []).length, 2, 'pills on the right and bottom only');
});

test('four corners move both regions, and claim no accessible control', () => {
  assert.match(dialog, /<CatalogCornerGrips width=\{widthRegion\.props\} height=\{heightRegion\.props\} \/>/);
  const grips = readFileSync(new URL('./components/CatalogCornerGrips.tsx', import.meta.url), 'utf8');

  // The accessible path is the edges: a grip is hidden from assistive tech and never
  // enters the tab order, so nobody is told about a control a keyboard cannot drive.
  // Anchored to the JSX, not to the word: the header comment explains the choice and
  // would otherwise satisfy this on its own.
  assert.match(grips, /aria-hidden data-corner=\{corner\}/);
  assert.equal(/tabIndex/.test(grips), false, 'a grip is never focusable');
  assert.equal(/role=/.test(grips), false, 'and never announced as a separator it cannot drive');

  // One gesture, both regions, and the same doubling the edges get, so a corner and an
  // edge cannot disagree about how far a centred box moves.
  assert.match(grips, /cornerDelta\(current\.corner/);
  assert.match(grips, /width\._onResizeMove\(centredEdgeDelta\(delta\.inline\)\)/);
  assert.match(grips, /height\._onResizeMove\(centredEdgeDelta\(delta\.block\)\)/);
  assert.match(grips, /width\._onResizeStart\(\);/);
  assert.match(grips, /height\._onResizeStart\(\);/);

  // Capture, as the library's own handler does it: without it a corner would only work
  // while the pointer stayed inside its square, and a cancelled gesture must end
  // without reporting a resize end.
  assert.match(grips, /setPointerCapture\?\.\(event\.pointerId\)/);
  assert.match(grips, /onPointerMove: \(event: ReactPointerEvent<HTMLElement>\): void => \{/);
  assert.match(grips, /onLostPointerCapture: \(event: ReactPointerEvent<HTMLElement>\): void => end\(event, false\)/);
  assert.match(grips, /onPointerCancel: \(event: ReactPointerEvent<HTMLElement>\): void => end\(event, false\)/);
  assert.match(grips, /width\._onResizeCancel\?\.\(\)/, 'a cancelled gesture releases the region without ending it');
  assert.match(grips, /if \(didComplete\) \{[\s\S]*?width\._onResizeEnd\(\)/, 'a released pointer does end it');

  // Both diagonals get a cursor, and the size is the library's band rather than a
  // second constant for the same edge.
  assert.match(grips, /cursor: 'nwse-resize'/);
  assert.match(grips, /cursor: 'nesw-resize'/);
  assert.match(grips, /const BAND = 'var\(--resize-handle-hit-area, var\(--spacing-4\)\)'/);
  const gripsCode = grips.replace(/\/\*[\s\S]*?\*\//g, '').replace(/^\s*\/\/.*$/gm, '');
  assert.equal(/\d+px/.test(gripsCode), false, 'no restated band width in the code');
});

test('the surface never scrolls: scrolling belongs to the panes inside it', () => {
  // Reported for Firefox 158 at 1737x820, DPR 1.0909, stored width 1429.5830993652344:
  // the surface reported `scrollWidth - clientWidth` 1 and `scrollHeight - clientHeight`
  // 1, with computed overflow auto/auto — what the styled `dialog` resolves to — so one
  // pixel the engine rounded away painted both bars. The pixel did not reproduce
  // here (56 fractional seeds, both engines, that same window and ratio), so the rule
  // is structural rather than a chase: the surface cannot scroll at all, and the two
  // places that genuinely scroll keep their own scrollers.
  assert.match(dialog, /surface: \{overflow: 'clip'\}/, 'the surface is a frame, not a scroll container');
  assert.match(dialog, /<Dialog[\s\S]*?xstyle=\{frameStyles\.surface\}/, 'and the surface is handed it');
  // Verified at runtime in both engines, because StyleX class order is not something
  // to assume: computed overflow on dialog[open] is 'clip' in Chromium and Firefox.
  // `hidden` would be a different claim — it scrolls by script and by keyboard.
  assert.equal(/surface: \{overflow: '(auto|scroll|hidden|visible)'\}/.test(dialog), false, 'no scrollable surface');
  assert.equal((dialog.match(/overflow: '/g) ?? []).length, 1, 'one declaration decides it, so nothing re-opens the surface');
  // Clipping takes nothing away: the file table scrolls inside its own pane, as does
  // the schema tab, and the editor is sized to its pane rather than growing it.
  assert.match(pane, /<StackItem size="fill" isScrollable>/, 'the file table keeps its own scroller');
  assert.match(dialog, /<StackItem size="fill" isScrollable id=\{panelId\('schema'\)\}/);
  assert.match(dialog, /<JsonCodeEditor[\s\S]*?height="100%"/, 'the JSON editor fills its pane instead of pushing the surface');
});

test('every kind gets the same last Action column, unsortable and undraggable', () => {
  const column = actionColumn();
  assert.match(pane, /key: 'action',[\s\S]*?header: 'Action',/);
  assert.ok(pane.indexOf("key: 'action'") > pane.indexOf("key: 'name'"), 'the action column comes last');
  assert.ok(pane.indexOf("key: 'action'") > pane.indexOf("key: 'sessions'"), 'and after the Session column profiles add');
  assert.match(column, /resizable: false/, 'no drag handle on its border');
  assert.match(column, /width: actionColumnWidth/);
  assert.match(column, /align: 'center'/, 'the icon sits in the middle of its room, not at the leading edge');
  assert.match(pane, /const actionColumnWidth = pixel\(appConfig\.catalog_action_column_width_px\);/);
  assert.equal(/pixel\(\d/.test(pane), false, 'no hard-coded column width');
  assert.equal(/sortable/.test(column), false, 'the header offers no sort');
  assert.equal(/action/.test(listView), false, 'the pure sorter never learns to sort by it');
});

test('the Action column is wide enough for its own header', () => {
  // Measured in the browser at the narrowest dialog width: the header cell keeps
  // 8px of padding on each side (compact density, checked on th and td alike) and
  // "Action" is 43.8px at the table's 14px label size, so the word needs 60px of
  // column and was clipped to `Act…` at 56. 64 keeps it whole with a little room
  // for a font metric that rounds the other way.
  const headerFloor = 2 * 8 + 44;
  assert.ok(config.catalog_action_column_width_px >= headerFloor, 'the word reads in full');
  assert.ok(config.catalog_action_column_width_px < 2 * headerFloor, 'while staying an icon column');
  const buttonRoom = config.catalog_action_column_width_px - 2 * 8;
  assert.equal((buttonRoom - 28) % 2, 0, 'the 28px button centres on a whole pixel');
});

test('the delete is one control per row, and the click stops before the row opens', () => {
  const column = actionColumn();
  assert.match(column, /<Tooltip content="Delete">/);
  assert.match(column, /icon=\{<Icon icon=\{TrashIcon\} size="sm" \/>\}/);
  assert.match(column, /variant="destructive"/);
  assert.match(column, /label=\{`Delete \$\{row\.id\}`\}/);
  assert.match(column, /isDisabled=\{isDisabled\}/);
  const stop = column.indexOf('event.stopPropagation()');
  const ask = column.indexOf('onDelete(');
  assert.ok(stop > 0 && ask > stop, 'the row click is stopped before the delete is asked for');
  assert.match(column, /onDelete\(\{id: row\.id, etag: row\.etag\}\)/, 'the row hands over the version it was listed at');
  assert.match(dialog, /onDelete=\{target => setPending\(\{kind: 'row-delete', \.\.\.target\}\)\}/);
});

test('one confirm stands behind both deletes', () => {
  // `AlertDialog` cannot carry the checkbox — a string description and no body
  // slot — so the one question is now a `Dialog`. What replaces it is one surface,
  // not a second one beside it.
  assert.equal(/AlertDialog/.test(dialog), false, 'the alert cannot hold the box');
  assert.equal((dialog.match(/<CatalogConfirmDialog/g) ?? []).length, 1, 'exactly one confirm, mounted by both deletes');
  assert.match(dialog, /isOpen=\{pending !== null\}/, 'and it opens for every question');
  assert.match(dialog, /onConfirm=\{confirmPending\}/);
  assert.match(dialog, /onCancel=\{\(\) => setPending\(null\)\}/);
  // The alert's properties, kept: an interrupting modal with a focus trap, Escape
  // through the layer stack, and Cancel — not Delete — as the focused default.
  // Anchored to the JSX line, not to the word: the header comment explains the role
  // and would otherwise satisfy the assertion by itself.
  assert.match(confirm, /^ {6}role="alertdialog"$/m, 'the modal keeps the role an alert promised');
  assert.match(confirm, /<Button variant="ghost" label="Cancel" data-autofocus onClick=\{onCancel\} \/>/);
  assert.match(confirm, /purpose="form"/, 'the form purpose keeps Escape and blocks a backdrop click');
  assert.match(confirm, /if \(!open\) onCancel\(\)/, 'Escape answers with Cancel');
  assert.ok(confirm.indexOf('data-autofocus') < confirm.indexOf('variant="destructive"'), 'Cancel is written first');
});

test('the transcript checkbox lives in the confirm, not in the editor', () => {
  assert.equal(/Also delete its transcripts/.test(dialog), false, 'gone from the editor pane');
  assert.equal(/CheckboxInput/.test(dialog), false, 'and so is its import');
  assert.equal(/\bdeleteState\b(?!:)/.test(dialog.slice(0, dialog.indexOf('const confirmPending'))), false, 'no state of its own above the handlers');
  assert.match(confirm, /<CheckboxInput[\s\S]*?value=\{deleteState\}[\s\S]*?onChange=\{setDeleteState\}/);
  assert.match(confirm, /label=\{DELETE_STATE_LABEL\}/);
  assert.match(confirm, /export const DELETE_STATE_LABEL = 'Also delete its transcripts and tool state when deleting';/, 'the precise wording, transcripts *and* tool state');
  // Every open starts unticked, and a cancel leaves nothing behind: the answer is
  // reset when the surface opens, which is when the question changes.
  assert.match(confirm, /useLayoutEffect\(\(\) => \{\s*if \(isOpen\) setDeleteState\(false\);\s*\}, \[isOpen\]\)/);
  // The question re-reads the box, so the sentence and the request cannot disagree.
  assert.match(confirm, /const question = prompt\(deleteState\)/);
});

test('sessions and profiles are offered the state choice, and nothing else is', () => {
  // Sessions own a state folder and profiles own their share of the state in every
  // session that ran them. Models, toolsets and MCP servers keep none — the kind view
  // carries a null note for those, so the confirm shows no dead control.
  assert.match(dialog, /stateNote=\{isDelete \? view\.stateNote : null\}/, 'the kind decides, and only a delete asks');
  assert.match(confirm, /\{stateNote !== null && \(/, 'the box is conditional in the confirm');
});

test('only the sessions route is told the answer, and the gap is written where it is dropped', () => {
  // The profile box is a placeholder: the generic catalogue route reads the kind, the
  // id and the version, and ignores a query. The value is therefore dropped here, and
  // the drop says so — a silent no-op is the one thing this must not be.
  const api = readFileSync(new URL('./api.ts', import.meta.url), 'utf8');
  assert.match(api, /const query = kind === 'sessions' && options\.deleteState === true \? '\?state=delete' : '';/);
  assert.match(api, /The profile side is not wired server-side yet/);
});

test('the ticked answer reaches the request, from either path', () => {
  const editor = dialog.slice(dialog.indexOf('const performDelete'), dialog.indexOf('const performRowDelete'));
  const row = dialog.slice(dialog.indexOf('const performRowDelete'), dialog.indexOf('const reloadCurrent'));
  assert.match(editor, /await deleteCatalogFile\(kind, draft\.id, draft\.etag, \{deleteState\}\)/, 'the editor sends the answer');
  assert.match(row, /await deleteCatalogFile\(kind, id, etag, \{deleteState\}\)/, 'so does the row, which used to hard-code false');
  assert.equal(/deleteState: (true|false)/.test(dialog), false, 'neither path decides for the user');
  assert.match(dialog, /const confirmPending = \(deleteState: boolean\): void =>/, 'the answer arrives from the confirm');
  assert.match(confirm, /onConfirm\(deleteState\)/, 'and the confirm reports the box it shows');
  for (const block of [editor, row]) {
    assert.match(block, /await reloadList\(\)/);
    assert.match(block, /onChanged\(\)/);
    assert.match(block, /title: 'Not deleted'/);
    assert.match(block, /finally \{\s*setIsBusy\(false\);/);
  }
});
