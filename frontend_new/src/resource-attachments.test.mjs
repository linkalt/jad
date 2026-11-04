import assert from 'node:assert/strict';
import test from 'node:test';

import {resourceAttachmentFiles, resourceAttachmentName} from './resource-attachments.ts';

test('resourceAttachmentFiles turns images and PDFs into Files with their bytes, names and types', async () => {
  const {images, files} = resourceAttachmentFiles([
    {type: 'image', uri: 'docs://img/logo', mimeType: 'image/png', data: 'iVBORw=='},
    {type: 'file', uri: 'docs://report', mimeType: 'application/pdf', name: 'report.pdf', data: 'JVBERg=='},
  ], 'logo');
  assert.equal(images.length, 1);
  assert.equal(files.length, 1);
  assert.equal(images[0].name, 'logo.png');
  assert.equal(images[0].type, 'image/png');
  assert.deepEqual([...new Uint8Array(await images[0].arrayBuffer())], [0x89, 0x50, 0x4e, 0x47]);
  assert.equal(files[0].name, 'report.pdf');
  assert.equal(await files[0].text(), '%PDF');
});

test('resourceAttachmentName keeps an extension, adds the image one, and falls back to the resource name', () => {
  assert.equal(resourceAttachmentName({type: 'image', uri: 'docs://a/chart.webp', mimeType: 'image/webp', data: ''}, 'x'), 'chart.webp');
  assert.equal(resourceAttachmentName({type: 'image', uri: 'docs://a/chart', mimeType: 'image/jpeg', data: ''}, 'x'), 'chart.jpg');
  assert.equal(resourceAttachmentName({type: 'image', uri: 'docs://', mimeType: 'image/gif', data: ''}, 'logo'), 'logo.gif');
  assert.equal(resourceAttachmentName({type: 'image', uri: 'docs://a b?v=1', mimeType: 'image/png', data: ''}, 'x'), 'a_b.png');
});
