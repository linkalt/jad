import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import test from 'node:test';

// The composer's header row is one run of icons: the prompt-library toggle,
// the toolbar actions (model picker, usage details, state context) and then
// the attach and preview buttons. It used to be two clusters — gap={4} outside,
// gap={1} inside each — which left a 16px space between the last toolbar action
// and the attach-image button that only made sense while those two buttons sat
// in their own highlighted surface. That surface is gone, so the spacing has to
// be even; the repo has no React render harness, so the gap props are what
// there is to assert.
const pane = readFileSync(new URL('./components/ComposerPane.tsx', import.meta.url), 'utf8');

const rowOpen = pane.indexOf('headerActions={');
const rowClose = pane.indexOf('headerContext={');

function headerRow() {
  assert.ok(rowOpen > -1 && rowClose > rowOpen, 'ComposerPane.tsx must render the header row');
  return pane.slice(rowOpen, rowClose);
}

test('the header row is one block that still holds every icon it spaces', () => {
  const row = headerRow();
  const needles = [
    'prompt library',
    '{toolbarActions}',
    'Attach image',
    'Attach file',
    'Hide markdown preview',
  ];
  for (const needle of needles) {
    assert.ok(row.includes(needle), `the header row must still hold ${needle}`);
  }
});

test('every gap in the header row is the same', () => {
  const gaps = [...headerRow().matchAll(/gap=\{([\d.]+)\}/g)].map(match => match[1]);
  assert.ok(gaps.length >= 2, `expected the row's nested stacks, found ${gaps.length} gaps`);
  assert.deepEqual(
    [...new Set(gaps)],
    [gaps[0]],
    `the header row mixes gaps, so a cluster boundary shows as a wider space: ${gaps.join(', ')}`,
  );
});
