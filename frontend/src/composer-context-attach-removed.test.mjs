import assert from 'node:assert/strict';
import {readFileSync, readdirSync} from 'node:fs';
import test from 'node:test';

// The composer's "Attach context" button is gone: it was the copy-icon button
// right after the model picker, it read text files in the browser and inserted
// their text into the draft, and nothing else used its hidden input, its
// reading module or its four attach_context_* config keys. This pins that none
// of them comes back by accident — a re-added button should be a decision.
const src = new URL('./', import.meta.url);
const needles = ['attach_context', 'readAttachmentBlocks', 'attachment-reading'];

function sourceFiles(directory) {
  return readdirSync(directory, {withFileTypes: true}).flatMap(entry => {
    const child = new URL(`${entry.name}${entry.isDirectory() ? '/' : ''}`, directory);
    if (entry.isDirectory()) return sourceFiles(child);
    return /\.(ts|tsx|json)$/.test(entry.name) ? [child] : [];
  });
}

test('no frontend source mentions the removed attach-context feature', () => {
  const files = sourceFiles(src);
  // A guard against the walk silently finding nothing and passing for the wrong reason.
  assert.ok(files.length > 50, `expected to scan the src tree, walked ${files.length} files`);
  for (const file of files) {
    const text = readFileSync(file, 'utf8');
    for (const needle of needles) {
      assert.ok(!text.includes(needle), `${file.pathname} still mentions ${needle}`);
    }
  }
});

test('the composer row has no file-opening button but the copy icon', () => {
  const app = readFileSync(new URL('./App.tsx', import.meta.url), 'utf8');
  assert.ok(!app.includes('icon="copy"'), 'App.tsx still opens a picker from a copy-icon button');
  assert.ok(!app.includes('attachInputRef'), 'App.tsx still holds the attach-context input ref');
  assert.ok(!app.includes('handleAttachFiles'), 'App.tsx still holds the attach-context handler');
});

test('the composer config carries no attach-context keys', () => {
  const config = JSON.parse(readFileSync(new URL('./config.json', import.meta.url), 'utf8'));
  assert.deepEqual(
    Object.keys(config).filter(key => key.startsWith('attach_context')),
    [],
  );
});

test('the reading module and its test are gone', () => {
  const paths = sourceFiles(src).map(file => file.pathname);
  assert.ok(
    !paths.some(path => path.endsWith('/attachment-reading.ts')),
    'frontend/src/attachment-reading.ts is back',
  );
});
