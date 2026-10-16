import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import test from 'node:test';

// Firefox counts the mouseup of the right-click that opened the left panel's row
// menu as a light-dismiss press, so a Popover left on its default native
// `popover="auto"` dismissal vanished in the same gesture that opened it (Chrome
// does not, which is why it looked like a Firefox-only bug; measured with
// Playwright's Firefox: the menu was gone by the time the right-click returned).
// Astryx's own ContextMenu keeps manual dismissal for exactly this reason, so
// the menu must keep hasLightDismiss={false} and dismiss itself from a document
// mousedown outside the menu.
const source = readFileSync(
  new URL('./components/RowContextMenu.tsx', import.meta.url),
  'utf8',
);

test('the left panel row menu does not use the popover native light dismiss', () => {
  assert.match(source, /hasLightDismiss=\{false\}/, 'RowContextMenu.tsx must pass hasLightDismiss={false}');
});

test('the left panel row menu dismisses itself on a press outside it', () => {
  assert.match(source, /document\.addEventListener\('mousedown'/, 'no document mousedown dismissal');
  assert.match(source, /contains\(event\.target/, 'the dismissal must check the press is outside the menu');
});
