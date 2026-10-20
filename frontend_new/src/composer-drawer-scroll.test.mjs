import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import test from 'node:test';

// The three composer drawers — the draft request, the profile list and the model
// list — used to grow with their content, so a long draft or a long list pushed
// past the pane with no scrollbar. Each one now caps its body at the configured
// height and scrolls inside it.
//
// Source assertions, like the rest of these tests: the repo has no React render
// harness, so what is pinned is the props the body is given, not the pixels.
const drawers = ['DraftRequestDrawer', 'ProfileDrawer', 'ModelDrawer'].map(name => ({
  name,
  source: readFileSync(new URL(`./components/${name}.tsx`, import.meta.url), 'utf8'),
}));
const config = JSON.parse(readFileSync(new URL('./config.json', import.meta.url), 'utf8'));

// The root content stack of each drawer, with whitespace flattened: gap and width
// as before, then — for the two drawers that click through to stop the composer
// closing — their own handler, then the scroll cap. The stacks nested inside carry
// no `isScrollable`, so only this one shape matches.
const CAPPED_BODY =
  /<VStack gap=\{2\} width="100%" (?:onClick=\{event => event\.stopPropagation\(\)\} )?isScrollable style=\{\{maxHeight: appConfig\.composer_drawer_max_height_px\}\} >/;

test('the drawer cap is configuration, not a literal', () => {
  assert.equal(typeof config.composer_drawer_max_height_px, 'number', 'the key must exist');
  assert.ok(config.composer_drawer_max_height_px > 0, 'and it must be a positive height');
  for (const {name, source} of drawers) {
    assert.match(
      source,
      /import appConfig from '\.\.\/config\.json';/,
      `${name} reads the cap from the config`,
    );
  }
});

test('every composer drawer scrolls its body inside that cap', () => {
  for (const {name, source} of drawers) {
    assert.match(source.replace(/\s+/g, ' '), CAPPED_BODY, `${name} caps and scrolls its drawer body`);
    assert.equal(
      (source.match(/appConfig\.composer_drawer_max_height_px/g) ?? []).length,
      1,
      `${name} caps its body once, in one place`,
    );
    assert.ok(
      !/maxHeight: \d/.test(source),
      `${name} carries no hard-coded cap beside the configured one`,
    );
  }
});
