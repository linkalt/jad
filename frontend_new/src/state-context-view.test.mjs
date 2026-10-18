import assert from 'node:assert/strict';
import test from 'node:test';
import {stateContextPreviewView} from './state-context-view.ts';

test('a preview with text is shown even though the server reports written=false', () => {
  assert.deepEqual(
    stateContextPreviewView({written: false, preview: true, role: 'developer', text: 'block', bytes: 5}),
    {kind: 'block', text: 'block', summary: '5 bytes, role developer'},
  );
});

test('a result without text has nothing to show', () => {
  assert.deepEqual(stateContextPreviewView({written: false, preview: true}), {kind: 'empty'});
  assert.deepEqual(stateContextPreviewView({written: false, preview: false}), {kind: 'empty'});
});

test('the summary leaves out what the server did not send', () => {
  assert.deepEqual(stateContextPreviewView({written: false, preview: true, text: 'x', bytes: 1}), {
    kind: 'block',
    text: 'x',
    summary: '1 bytes',
  });
  assert.deepEqual(stateContextPreviewView({written: false, preview: true, text: 'x'}), {
    kind: 'block',
    text: 'x',
    summary: null,
  });
});
