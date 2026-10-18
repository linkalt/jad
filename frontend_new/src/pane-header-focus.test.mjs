import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import test from 'node:test';

const pane = readFileSync(new URL('./components/ViewPane.tsx', import.meta.url), 'utf8');
const theme = readFileSync(new URL('./themes/neutral/theme.css', import.meta.url), 'utf8');

test('the focused pane tints its strip so the pane that owns the keyboard is visible', () => {
  assert.match(
    pane,
    /headerFocused: \{\s*backgroundColor: 'var\(--color-background-gray\)',/,
    'the tint is a theme token, so it follows the colour scheme',
  );
  assert.match(
    pane,
    /xstyle=\{\[styles\.header, isFocused \? styles\.headerFocused : undefined\]\}/,
    'only the focused pane is tinted; the others keep the page colour',
  );
  assert.equal(
    (pane.match(/backgroundColor: 'var\(--color-background-gray\)'/g) ?? []).length,
    1,
    'the tint is declared once',
  );
});

test('the tinted token exists in the scheme, and is a light grey rather than a dark block', () => {
  const declared = theme.match(/--color-background-gray: (light-dark\([^)]*\));/);
  assert.notEqual(declared, null, '--color-background-gray is defined by the theme');
  const [, value] = declared;
  const light = value.match(/^light-dark\(#([0-9a-f]{6}), #([0-9a-f]{6})\)$/);
  assert.notEqual(light, null, `expected a light/dark pair, got ${value}`);
  const luma = hex => {
    const channel = offset => parseInt(hex.slice(offset, offset + 2), 16);
    return 0.299 * channel(0) + 0.587 * channel(2) + 0.114 * channel(4);
  };
  assert.ok(luma(light[1]) > 200, `light value ${light[1]} is a light grey, not a dark one`);
  assert.ok(
    luma(light[2]) > luma('1b1b1b'),
    `dark value ${light[2]} is lighter than the page (#1b1b1b), so the tint never reads as a hole`,
  );
});
