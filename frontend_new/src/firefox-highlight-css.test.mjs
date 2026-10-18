import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import test from 'node:test';

// Firefox paints `code::highlight(x)` only on text directly in <code>. Astryx's
// CodeBlock puts each line in a <div data-line>, so index.css repeats every
// token rule on the descendants. A token type Astryx adds later needs a rule too.
const css = readFileSync(new URL('./index.css', import.meta.url), 'utf8');
const styles = readFileSync(
  new URL('../node_modules/@astryxdesign/core/dist/CodeBlock/highlightStyles.js', import.meta.url),
  'utf8',
);

test('every Astryx syntax token has a descendant ::highlight rule for Firefox', () => {
  const list = /TOKEN_TYPES = \[([^\]]*)\]/.exec(styles);
  assert.ok(list, 'TOKEN_TYPES not found in Astryx highlightStyles.js');
  const tokens = [...list[1].matchAll(/'([a-z-]+)'/g)].map(match => match[1]);
  assert.ok(tokens.length >= 13, `expected the known token list, got ${tokens.length}`);
  for (const token of tokens) {
    const rule = new RegExp(
      `\\.astryx-code-block code \\*::highlight\\(astryx-${token}\\)\\s*\\{\\s*color:\\s*var\\(--color-syntax-${token}\\)`,
    );
    assert.match(css, rule, `index.css has no Firefox rule for astryx-${token}`);
  }
});
