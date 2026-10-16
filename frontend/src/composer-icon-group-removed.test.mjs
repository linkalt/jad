import assert from 'node:assert/strict';
import {existsSync, readFileSync, readdirSync} from 'node:fs';
import test from 'node:test';

// The composer's attach-image and attach-file buttons used to sit in one tinted
// surface (IconGroup: a palette background, a matching border and role="group"),
// so that the two icons read as a unit. That highlight is gone: both buttons are
// plain toolbar icons like their neighbours. This pins the removal so the
// surface does not come back by accident — and pins that removing it did not
// take the pickers, the click actions or the per-button config flags with it.
const src = new URL('./', import.meta.url);
const pane = readFileSync(new URL('./components/ComposerPane.tsx', import.meta.url), 'utf8');

function sourceFiles(directory) {
  return readdirSync(directory, {withFileTypes: true}).flatMap(entry => {
    const child = new URL(`${entry.name}${entry.isDirectory() ? '/' : ''}`, directory);
    if (entry.isDirectory()) return sourceFiles(child);
    return /\.(ts|tsx|json)$/.test(entry.name) ? [child] : [];
  });
}

/** The attach row: everything from its comment up to the preview toggle. */
function attachRow() {
  const comment = pane.indexOf('The attachment buttons are plain toolbar icons');
  const preview = pane.indexOf('Hide markdown preview');
  assert.ok(comment > -1 && preview > comment, 'the attach row must precede the preview toggle');
  return pane.slice(comment, preview);
}

/**
 * The JSX of the IconButton carrying `label`, up to the end of its start tag.
 * The tag ends on its own line, so the nested <Icon .../> on the icon prop does
 * not cut the slice short.
 */
function iconButton(label) {
  const match = pane.match(new RegExp(`${label}[\\s\\S]*?\\n\\s*\\/>`));
  assert.ok(match, `ComposerPane.tsx must still render ${label}`);
  return match[0];
}

test('the highlighted-group component and its name are gone', () => {
  assert.ok(
    !existsSync(new URL('./components/IconGroup.tsx', import.meta.url)),
    'frontend/src/components/IconGroup.tsx is back',
  );
  const files = sourceFiles(src);
  // A guard against the walk silently finding nothing and passing for the wrong reason.
  assert.ok(files.length > 50, `expected to scan the src tree, walked ${files.length} files`);
  for (const file of files) {
    assert.ok(
      !readFileSync(file, 'utf8').includes('IconGroup'),
      `${file.pathname} still mentions IconGroup`,
    );
  }
});

test('the attach buttons are plain icons with no surface around them', () => {
  const row = attachRow();
  for (const styling of ['xstyle', 'backgroundColor', 'borderColor', 'outline', 'tone']) {
    assert.ok(!row.includes(styling), `the attach row must carry no ${styling}`);
  }
  for (const label of ['label="Attach image"', 'label="Attach file"']) {
    assert.match(iconButton(label), /variant="ghost"/, `${label} must be a bare icon button`);
  }
});

test('each attach button keeps its own flag, picker and click action', () => {
  const row = attachRow();
  assert.ok(
    !row.includes('isImageAttachEnabled || isFileAttachEnabled'),
    'the two buttons must not share one render gate again',
  );
  assert.match(row, /isImageAttachEnabled \? \(/);
  assert.match(row, /isFileAttachEnabled \? \(/);
  assert.match(iconButton('label="Attach image"'), /openImagePickerRef\.current\?\.\(\)/);
  assert.match(iconButton('label="Attach file"'), /openFilePickerRef\.current\?\.\(\)/);
  assert.match(
    pane,
    /<ImageAttachmentPicker\b[\s\S]*?openRef=\{openImagePickerRef\}/,
    'the image picker must still receive its ref',
  );
  assert.match(
    pane,
    /<FileAttachmentPicker\b[\s\S]*?openRef=\{openFilePickerRef\}/,
    'the file picker must still receive its ref',
  );
});

test('the markdown preview toggle still sits beside the attach buttons', () => {
  assert.ok(pane.includes('Hide markdown preview'), 'the preview toggle must stay');
  assert.ok(
    pane.indexOf('label="Attach file"') < pane.indexOf('Hide markdown preview'),
    'the preview toggle follows the attach buttons',
  );
});
