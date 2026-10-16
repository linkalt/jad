import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import test from 'node:test';

// The composer's icon row is grouped: related icons sit in one highlighted
// surface, and a group's tone is what tells one group from another. These
// assertions pin the grouping and the token-only coloring of that surface.
// Neither has a unit-level home — the repo has no React render harness — so a
// dropped tone, a literal color or an ungrouped button would otherwise only
// show up in the browser.
const pane = readFileSync(new URL('./components/ComposerPane.tsx', import.meta.url), 'utf8');
const group = readFileSync(new URL('./components/IconGroup.tsx', import.meta.url), 'utf8');

const groupOpen = pane.indexOf('<IconGroup');
const groupClose = pane.indexOf('</IconGroup>');

test('the attach buttons share one highlighted group', () => {
  assert.ok(
    groupOpen > -1 && groupClose > groupOpen,
    'ComposerPane.tsx must wrap the attach buttons in an IconGroup',
  );
  const inside = pane.slice(groupOpen, groupClose);
  assert.match(inside, /label="Attach image"/, 'the image button belongs to the group');
  assert.match(inside, /label="Attach file"/, 'the file button belongs to the group');
  assert.match(inside, /tone="blue"/, 'the group must name the tone that highlights it');
  assert.match(
    pane.slice(0, groupOpen),
    /isImageAttachEnabled \|\| isFileAttachEnabled \?/,
    'the group renders only while at least one picker is enabled',
  );
});

test('the markdown preview button stays outside the attachment group', () => {
  assert.ok(
    pane.indexOf('Hide markdown preview') > groupClose,
    'the preview toggle is not part of the attachment group',
  );
});

test('every tone is a palette pair and the surface is token-only', () => {
  const declared = group.match(/export type IconGroupTone = ([^;]+);/);
  assert.ok(declared, 'IconGroup.tsx must declare IconGroupTone');
  const tones = declared[1].split('|').map(name => name.trim().replaceAll("'", ''));
  assert.ok(tones.length > 0, 'the tone union must not be empty');
  for (const tone of tones) {
    assert.match(
      group,
      new RegExp(`backgroundColor: colorVars\\['--color-background-${tone}'\\]`),
      `tone ${tone} needs a --color-background-${tone} token`,
    );
    assert.match(
      group,
      new RegExp(`borderColor: colorVars\\['--color-border-${tone}'\\]`),
      `tone ${tone} needs a --color-border-${tone} token`,
    );
  }
  assert.match(group, /borderRadius: radiusVars\['--radius-full'\]/);
  assert.match(group, /borderWidth: borderVars\['--border-width'\]/);
  assert.doesNotMatch(group, /#[0-9a-fA-F]{3,8}\b/, 'no literal color in IconGroup.tsx');
  assert.doesNotMatch(group, /\d+px/, 'no literal pixel length in IconGroup.tsx');
});

test('the highlighted group is a labelled group for assistive technology', () => {
  assert.match(group, /role="group"/);
  assert.match(group, /aria-label=\{label\}/);
});
