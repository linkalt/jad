import assert from 'node:assert/strict';
import test from 'node:test';

import {hasToolResultImages, toolResultText} from './tool-result-view.ts';

test('a result with no images keeps its output as it is', () => {
  assert.equal(hasToolResultImages({}), false);
  assert.equal(hasToolResultImages({images: [], inlineImages: 0}), false);
  assert.equal(toolResultText('[MCP image content]', {}), '[MCP image content]');
  assert.equal(toolResultText('plain text', {images: []}), 'plain text');
});

test('stored and inline images both count', () => {
  assert.equal(hasToolResultImages({images: ['a.png']}), true);
  assert.equal(hasToolResultImages({inlineImages: 1}), true);
});

test('a result that is only image placeholders shows no text', () => {
  assert.equal(toolResultText('[MCP image content]', {inlineImages: 1}), undefined);
  assert.equal(toolResultText('[MCP image content]\n[MCP image content]', {images: ['a.png', 'b.png']}), undefined);
  assert.equal(toolResultText('', {inlineImages: 1}), undefined);
});

test('text next to the images stays, without the placeholder lines', () => {
  assert.equal(toolResultText('caption\n[MCP image content]', {inlineImages: 1}), 'caption');
  assert.equal(toolResultText('[MCP file: a.pdf]\n[MCP image content]', {images: ['a.png']}), '[MCP file: a.pdf]');
});
