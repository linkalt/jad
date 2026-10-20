import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import test from 'node:test';

const css = readFileSync(new URL('./index.css', import.meta.url), 'utf8');
const navigation = readFileSync(new URL('./components/SessionNavigation.tsx', import.meta.url), 'utf8');

test('a profile row is indented under its session by the same step as a session under its category', () => {
  assert.match(navigation, /<VStack gap=\{0\} paddingInlineStart=\{2\}>/);
  assert.match(
    css,
    /\[role='group'\]\[id\$='-children'\] > div \{ padding-inline-start: var\(--spacing-2\); \}/,
  );
});
