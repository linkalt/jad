import assert from 'node:assert/strict';
import {test} from 'node:test';

import {
  buildRunInput,
  describeRejected,
  parseAcceptList,
  planImageFiles,
  readImageAsDataUrl,
} from './image-attachments.ts';

const limits = {maxCount: 3, maxBytes: 1000, accept: ['image/png', 'image/jpeg']};

function file(name, type, size) {
  return new File([new Uint8Array(size).fill(7)], name, {type});
}

test('parseAcceptList trims, lowercases and drops empty entries', () => {
  assert.deepEqual(parseAcceptList(' Image/PNG, image/jpeg ,,'), ['image/png', 'image/jpeg']);
  assert.deepEqual(parseAcceptList(''), []);
});

test('planImageFiles takes supported files in order', () => {
  const plan = planImageFiles([file('a.png', 'image/png', 10), file('b.jpg', 'image/jpeg', 20)], 0, limits);
  assert.deepEqual(plan.accepted.map(f => f.name), ['a.png', 'b.jpg']);
  assert.deepEqual(plan.rejected, []);
});

test('planImageFiles refuses other types, empty files and large files, each with a reason', () => {
  const plan = planImageFiles(
    [
      file('doc.txt', 'text/plain', 10),
      file('none.png', 'image/png', 0),
      file('big.png', 'image/png', 1001),
      file('edge.png', 'image/png', 1000),
    ],
    0,
    limits,
  );
  assert.deepEqual(plan.accepted.map(f => f.name), ['edge.png']);
  assert.deepEqual(plan.rejected.map(r => r.name), ['doc.txt', 'none.png', 'big.png']);
  assert.match(plan.rejected[0].reason, /not a supported/);
  assert.match(plan.rejected[1].reason, /empty/);
  assert.match(plan.rejected[2].reason, /larger than/);
});

test('planImageFiles compares the type without regard to case', () => {
  const plan = planImageFiles([file('a.png', 'IMAGE/PNG', 5)], 0, limits);
  assert.equal(plan.accepted.length, 1);
});

test('planImageFiles counts images already attached', () => {
  const files = [1, 2, 3].map(n => file(`${n}.png`, 'image/png', 5));
  const plan = planImageFiles(files, 2, limits);
  assert.deepEqual(plan.accepted.map(f => f.name), ['1.png']);
  assert.equal(plan.rejected.length, 2);
  assert.match(plan.rejected[0].reason, /at most 3 images/);
  assert.equal(planImageFiles(files, 3, limits).accepted.length, 0);
});

test('planImageFiles names a pasted file that has no name', () => {
  const plan = planImageFiles([file('', 'text/plain', 5)], 0, limits);
  assert.equal(plan.rejected[0].name, 'pasted image');
});

test('readImageAsDataUrl matches Buffer base64 for sizes around the chunk boundary', async () => {
  for (const size of [1, 2, 3, 0x7fff, 0x8000, 0x8001, 70000]) {
    const bytes = new Uint8Array(size).map((_, at) => (at * 31 + 5) % 256);
    const blob = new Blob([bytes], {type: 'image/png'});
    const url = await readImageAsDataUrl(blob);
    assert.equal(url, `data:image/png;base64,${Buffer.from(bytes).toString('base64')}`, `size ${size}`);
  }
});

test('buildRunInput keeps a plain string when there are no images', () => {
  assert.equal(buildRunInput('hello', []), 'hello');
});

test('buildRunInput puts the text first and the images in order', () => {
  const input = buildRunInput('look', [{dataUrl: 'data:image/png;base64,AA=='}, {dataUrl: 'data:image/png;base64,BB=='}]);
  assert.deepEqual(input, [
    {type: 'text', text: 'look'},
    {type: 'image', data: 'data:image/png;base64,AA=='},
    {type: 'image', data: 'data:image/png;base64,BB=='},
  ]);
});

test('buildRunInput with images and no text still sends a text part first', () => {
  const input = buildRunInput('', [{dataUrl: 'data:image/png;base64,AA=='}]);
  assert.equal(input[0].type, 'text');
  assert.equal(input.length, 2);
});

test('describeRejected is empty without refusals and joins them otherwise', () => {
  assert.equal(describeRejected([]), '');
  assert.equal(
    describeRejected([{name: 'a', reason: 'x'}, {name: 'b', reason: 'y'}]),
    'a: x; b: y',
  );
});

test('userMessageImages hides the server placeholder only when an image is drawn', async () => {
  const {userMessageImages} = await import('./image-attachments.ts');
  const name = `${'a'.repeat(64)}.png`;
  const stored = userMessageImages({
    text: '[image] [image]',
    parts: [{type: 'text', images: [name]}],
  });
  assert.deepEqual(stored, {sent: [], stored: [name], text: ''});
  // An inline image is not listed by the server: its placeholder is all there is to show.
  const inline = userMessageImages({text: '[image]', parts: [{type: 'text'}]});
  assert.deepEqual(inline, {sent: [], stored: [], text: '[image]'});
});

test('userMessageImages keeps real text, lists each stored name once, and shows sent data URLs', async () => {
  const {userMessageImages} = await import('./image-attachments.ts');
  const name = `${'b'.repeat(64)}.jpg`;
  const view = userMessageImages({
    text: 'what is this?',
    sentImages: ['data:image/png;base64,AA=='],
    parts: [
      {type: 'text', images: [name, name]},
      {type: 'tool_result', images: ['ignored.png']},
    ],
  });
  assert.deepEqual(view, {sent: ['data:image/png;base64,AA=='], stored: [name], text: 'what is this?'});
  // A user who typed "[image]" with nothing attached keeps their words.
  assert.equal(userMessageImages({text: '[image]'}).text, '[image]');
});
