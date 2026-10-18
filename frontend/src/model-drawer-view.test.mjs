import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import test from 'node:test';

// The composer's model drawer is a compact list — model-id, provider and a
// reasoning-effort drop-down per row — with no heading, no titles and no row
// dividers. Easy regressions this pins:
//
//  * no Astryx Table: it draws a divider per row, and its header cells own the
//    column widths, which made the gaps between the columns uneven.
//  * the running model is marked by a box around its id alone — a background
//    tint, not a text colour — and the tint comes from a token with a light-mode
//    value, so the highlight does not vanish there.
//  * clicking any row folds the open list back (another row is picked first);
//    an effort drop-down changes in place and keeps it open.
//  * no hover tooltip on the model id: it floated over the row above it.
//
// The rows themselves (collapsed = the running model alone, expanded = every
// model filtered by the search) are covered in model-selection.test.mjs.
const drawer = readFileSync(new URL('./components/ModelDrawer.tsx', import.meta.url), 'utf8');

test('the drawer builds its rows from stacks, not from a table', () => {
  assert.ok(!/from '@astryxdesign\/core\/Table'/.test(drawer), 'a table would draw dividers');
  assert.ok(!/<Table\b/.test(drawer), 'the drawer renders no Astryx Table');
  assert.ok(!/TableHeader|TableBody|dividers=/.test(drawer), 'no header row and no row dividers');
  assert.match(drawer, /rows\.map\(row => \{/, 'the rows are mapped explicitly');
  assert.match(drawer, /data-model-row=\{row\.isSelected \? 'selected' : 'choice'\}/, 'each row carries its selection state');
});

test('the drawer has no heading and no column titles', () => {
  assert.ok(!/Heading/.test(drawer), 'the drawer carries no heading of its own');
  assert.ok(!/header:/.test(drawer), 'no column titles are rendered');
});

test('every row uses the same three column widths', () => {
  const widthConstants = [...drawer.matchAll(/const (ID|PROVIDER|EFFORT)_COLUMN_WIDTH = '(\d+)%'/g)].map(match => match[2]);
  assert.deepEqual(widthConstants, ['46', '20', '30'], 'the three column shares are fixed');
  const row = drawer.slice(drawer.indexOf('rows.map(row => {'), drawer.indexOf('</VStack>', drawer.indexOf('rows.map(row => {')));
  for (const constant of ['ID_COLUMN_WIDTH', 'PROVIDER_COLUMN_WIDTH', 'EFFORT_COLUMN_WIDTH']) {
    assert.ok(row.includes(`width={${constant}}`), `${constant} must size its column in every row`);
  }
  assert.match(row, /gap=\{2\}/, 'the row keeps one gap between every column');
});

test('the expand chevron sits at the end of the row', () => {
  const row = drawer.slice(drawer.indexOf('rows.map(row => {'), drawer.indexOf('</VStack>', drawer.indexOf('rows.map(row => {')));
  const idColumn = row.slice(row.indexOf('ID_COLUMN_WIDTH'), row.indexOf('PROVIDER_COLUMN_WIDTH'));
  assert.ok(!idColumn.includes('chevronDown'), 'the chevron is not beside the model id any more');
  const effortColumn = row.indexOf('EFFORT_COLUMN_WIDTH');
  const chevron = row.indexOf('chevronDown');
  assert.ok(chevron > effortColumn, 'the chevron comes after the effort column, at the row end');
  assert.match(row.slice(chevron - 60, chevron), /\{isExpanded \? null :/, 'it is the collapsed row that carries it');
});

test('the reasoning effort is a drop-down per row', () => {
  assert.match(drawer, /import \{Selector\} from '@astryxdesign\/core\/Selector'/, 'the effort column uses a Selector');
  assert.match(drawer, /<Selector\s+label=\{`Reasoning effort for \$\{row\.option\.id\}`\}/, 'each drop-down is labelled for its own model');
  assert.match(drawer, /options=\{efforts\}/, 'the drop-down lists the model’s efforts');
  assert.match(drawer, /onChange=\{effort => choose\(row\.option, effort\)\}/, 'choosing an effort picks that model and effort');
  assert.match(drawer, /onClick=\{event => event\.stopPropagation\(\)\}/, 'a drop-down click must not also trigger the row');
  assert.match(drawer, /Not configurable/, 'a model without efforts says so instead of showing an empty drop-down');
});

test('the search field belongs to the expanded list only', () => {
  assert.match(drawer, /\{isExpanded && \(\s*<TextInput/, 'the search input renders only while the list is open');
  assert.match(drawer, /placeholder=\{`Search models in \$\{sessionName\}`\}/, 'the search input is labelled for the model search');
});

test('a row opens the list, picks in place, and the list stays open', () => {
  assert.match(drawer, /onClick=\{rowAction\(row\.option, row\.markedEffort, row\.isSelected\)\}/, 'the row is the control');
  const rowAction = drawer.slice(drawer.indexOf('const rowAction ='), drawer.indexOf('return ('));
  assert.match(rowAction, /if \(!isExpanded\) \{\s*setIsExpanded\(true\);/, 'a click opens the list when collapsed');
  assert.match(
    rowAction,
    /\} else \{\s*if \(!isRunning\) choose\(option, effort\);\s*setIsExpanded\(false\);\s*setQuery\(''\);/,
    'any row folds the list and clears the search; another row is picked first',
  );
  assert.equal((rowAction.match(/setIsExpanded\(false\)/g) ?? []).length, 1, 'one fold path, shared by every row');
  assert.match(drawer, /tabIndex=\{isDisabled \? -1 : 0\}/, 'a row stays reachable from the keyboard');
  assert.match(drawer, /event\.key !== 'Enter' && event\.key !== ' '/, 'Enter and Space act like a click');
});

test('the open list tints the row under the pointer', () => {
  assert.match(drawer, /const HOVERED_ROW = \{\s*backgroundColor: 'var\(--color-background-gray\)'/, 'the hover tint is a theme token strong enough to see (muted is ~5% opacity)');
  assert.match(drawer, /style=\{isExpanded && !isDisabled && hoveredId === row\.option\.id \? HOVERED_ROW : undefined\}/, 'only the open list, only an enabled drawer, only the hovered row');
  assert.match(drawer, /onMouseEnter=\{\(\) => setHoveredId\(row\.option\.id\)\}/, 'entering a row marks it');
  assert.match(drawer, /onMouseLeave=\{\(\) => setHoveredId\(null\)\}/, 'leaving clears it');
});

test('the open list carries no arrow icon', () => {
  assert.ok(!/arrowUp|chevronUp/.test(drawer), 'no up arrow is drawn on the running row');
});

test('the running model is marked by a box around its id alone', () => {
  assert.ok(!/icon="check"|icon='check'/.test(drawer), 'the check glyph is gone');
  const boxes = drawer.match(/style=\{row\.isSelected \? SELECTED_ID_BOX : undefined\}/g) ?? [];
  assert.equal(boxes.length, 1, 'the highlight box appears once, around the id');
  const rowStart = drawer.lastIndexOf('<HStack', drawer.indexOf('data-model-row'));
  const rowTag = drawer.slice(rowStart, drawer.indexOf('>', rowStart));
  assert.ok(!rowTag.includes('SELECTED_ID_BOX'), 'the row itself carries no selection highlight, only its id');
  const box = drawer.slice(drawer.indexOf('const SELECTED_ID_BOX'), drawer.indexOf('export function ModelDrawer'));
  assert.match(box, /backgroundColor: 'var\(--color-background-blue\)'/, 'the box is a tinted background, not a text colour');
  assert.match(box, /borderRadius: 'var\(--radius-/, 'the box is rounded with a radius token');
  assert.ok(!/color:/.test(box), 'the id keeps the drawer’s own text colour');
});

test('the model id carries no hover tooltip', () => {
  assert.ok(!/tooltip=/.test(drawer), 'the tooltip floated over the row above the id');
});
