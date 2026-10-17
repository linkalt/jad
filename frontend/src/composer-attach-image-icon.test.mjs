import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import test from 'node:test';

// The attach-image button used to borrow the semantic `upload` icon, which reads
// as "send" (an arrow into a tray) rather than "add a picture". It now draws the
// svgrepo `image` outline — a frame holding a mountain and a sun — from a local
// component, the way the markdown preview toggle supplies its own eye glyph.
//
// The source SVG carried a white background plate, a fixed 800px box and a
// hardcoded black stroke. None of the three may come back: the plate would paint
// a white square behind the glyph, and the theme owns the colour while `Icon`
// owns the size.
const source = readFileSync(new URL('./components/ComposerPane.tsx', import.meta.url), 'utf8');

/** The source of one top-level function component, from its declaration to its closing brace. */
function component(name) {
  const start = source.indexOf(`function ${name}(`);
  assert.ok(start > -1, `ComposerPane.tsx must still define ${name}`);
  const end = source.indexOf('\n}\n', start);
  assert.ok(end > start, `${name} must end on a top-level brace`);
  return source.slice(start, end);
}

/** The declarations of a component slice, with its comments dropped. */
function declarations(slice) {
  return slice.replaceAll(/\/\*[\s\S]*?\*\//g, '').replaceAll(/\/\/[^\n]*/g, '');
}

/** The attach-image IconButton, from its label to the picker that follows it. */
function attachImageButton() {
  const start = source.indexOf('label="Attach image"');
  assert.ok(start > -1, 'ComposerPane.tsx must still render the attach-image button');
  const end = source.indexOf('<ImageAttachmentPicker', start);
  assert.ok(end > start, 'the image picker must still follow the attach-image button');
  return source.slice(start, end);
}

test('the attach-image button draws the picture glyph', () => {
  assert.match(
    attachImageButton(),
    /icon=\{<Icon icon=\{ImageIcon\} size="sm" \/>\}/,
    'the button must render the picture component through Icon',
  );
  assert.doesNotMatch(
    source,
    /icon="upload"/,
    'the semantic upload glyph must not come back to the attach-image button',
  );
});

test('the picture glyph keeps the svgrepo outline and its frame', () => {
  const icon = declarations(component('ImageIcon'));
  // The frame and the mountain, then the sun, as the source SVG drew them.
  assert.match(icon, /M21 16V20C21 20\.5523/, 'the outer frame must stay');
  assert.match(icon, /M21 16L15\.4829 12\.3219/, 'the mountain must stay');
  assert.match(icon, /<circle cx="8" cy="9" r="2"/, 'the sun must stay');
  assert.match(icon, /viewBox="0 0 24 24"/, 'the glyph must stay on the 24-unit grid');
});

test('the picture glyph follows the theme instead of the source file', () => {
  const icon = declarations(component('ImageIcon'));
  assert.match(icon, /stroke="currentColor"/, 'the stroke must take the icon colour');
  assert.doesNotMatch(icon, /fill="white"/, 'the white background plate must not come back');
  assert.doesNotMatch(icon, /width=|height=/, 'Icon must size the glyph, not the SVG');
  assert.doesNotMatch(icon, /#[0-9a-fA-F]{3,8}\b/, 'no literal colour');
  assert.doesNotMatch(icon, /\d+px/, 'no literal pixel length');
});

test('the picture glyph matches the sibling outline icons', () => {
  // PreviewIcon is the other local outline glyph; a shared stroke width keeps
  // the toolbar's icons the same weight.
  const strokeWidth = name => declarations(component(name)).match(/strokeWidth=\{([\d.]+)\}/)?.[1];
  assert.equal(strokeWidth('ImageIcon'), strokeWidth('PreviewIcon'));
  assert.equal(strokeWidth('ImageIcon'), '1.5');
});
