import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import test from 'node:test';

// The preview used to size itself to its content inside the drawer's wrapping
// flex row, so a draft holding one long unbreakable run (a URL, a hash, an
// identifier) stretched the preview past the drawer's width and the drawer —
// whose own overflow is hidden — clipped it. Measured before the fix at a
// 1167px window: the content column grew to 1291px inside an 883px drawer and
// the "Preview" heading stretched to 1259px with it.
//
// The frame now asks for the drawer's width and may shrink below its content,
// and the body wraps such a run. These are source assertions because the repo
// has no React render harness; the layout itself was measured in the browser.
const source = readFileSync(new URL('./components/ComposerPreview.tsx', import.meta.url), 'utf8');
const styleBlock = source.slice(source.indexOf('stylex.create({'), source.indexOf('export function'));

test('the preview takes the drawer width and may shrink below its content', () => {
  assert.match(styleBlock, /frame: \{[\s\S]*?width: '100%'/, 'the frame must ask for the width');
  assert.match(styleBlock, /frame: \{[\s\S]*?minWidth: 0/, 'the frame must be allowed to shrink');
  assert.match(source, /xstyle=\{styles\.frame\}/, 'the frame style must reach the preview stack');
});

test('the preview body may shrink and keeps its clamp', () => {
  const body = styleBlock.slice(styleBlock.indexOf('body: {'), styleBlock.indexOf('content: {'));
  assert.ok(body.length > 0, 'the body style must exist');
  assert.match(body, /minWidth: 0/, 'the body must be allowed to shrink');
  assert.match(body, /maxHeight: '40vh'/, 'the clamp that keeps a long draft off the composer stays');
  assert.match(body, /overflowY: 'auto'/);
});

test('the markdown element itself wraps an unbreakable run', () => {
  // Astryx gives the markdown element its own `overflow-wrap: break-word`, which
  // leaves the min-content width alone, so the value has to be set on that
  // element: an inherited one loses to the element's own rule.
  const content = styleBlock.slice(styleBlock.indexOf('content: {'));
  assert.match(content, /overflowWrap: 'anywhere'/, 'only anywhere also shrinks the min-content width');
  assert.match(source, /xstyle=\{styles\.content\}/, 'the value must reach the markdown element');
  assert.doesNotMatch(styleBlock, /overflowWrap: 'break-word'/, 'break-word leaves the min-content width alone');
});

test('the preview body pads inline so a list cannot bleed into a scrollbar', () => {
  // Astryx draws a list with `margin-inline: -8px` on the ol/ul and
  // `padding-inline: 8px` on each li, so the list box is 8px wider than its
  // container on each side. `paddingInline={2}` is the same 8px, and it is what
  // stopped the reported horizontal scrollbar on every draft containing a list.
  assert.match(source, /paddingInline=\{2\}/, 'the body needs the inline padding a list bleeds into');
});

test('the preview styles introduce no literal colour or pixel length', () => {
  // Comments inside the block quote measured pixel sizes on purpose, so the
  // check runs on the declarations alone.
  const declarations = styleBlock.replaceAll(/\/\*[\s\S]*?\*\//g, '').replaceAll(/\/\/[^\n]*/g, '');
  assert.doesNotMatch(declarations, /#[0-9a-fA-F]{3,8}\b/);
  assert.doesNotMatch(declarations, /\d+px/);
});
