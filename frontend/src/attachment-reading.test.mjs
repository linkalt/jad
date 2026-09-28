import assert from 'node:assert/strict';
import {test} from 'node:test';

import {readAttachmentBlocks, readTextPrefix} from './attachment-reading.ts';

test('reads a prefix with File.text-compatible UTF-8 and truncation behavior', async () => {
  const cases = [
    ['', 0],
    ['short', 5],
    ['short', 4],
    ['A😀B', 2],
    ['\uFEFFheading', 4],
    [new Uint8Array([0xef, 0xbb, 0xbf, 0xf0, 0x9f]), 0],
    [new Uint8Array([0x61, 0xf0, 0x9f, 0x92, 0xa9, 0x62]), 2],
  ];

  for (const [content, limit] of cases) {
    const file = new File([content], 'sample.txt');
    const fullText = await file.text();
    assert.deepEqual(await readTextPrefix(file, limit), {
      text: fullText.slice(0, limit),
      truncated: fullText.length > limit,
    });
  }
});

test('slices only a bounded UTF-8 prefix, including enough bytes to detect truncation', async () => {
  const file = new File(['x'.repeat(10_000)], 'large.txt');
  const originalSlice = file.slice.bind(file);
  let requestedEnd;
  file.slice = (start, end, contentType) => {
    requestedEnd = end;
    return originalSlice(start, end, contentType);
  };

  const result = await readTextPrefix(file, 12);

  assert.equal(requestedEnd, (12 + 1) * 4 + 3);
  assert.deepEqual(result, {text: 'x'.repeat(12), truncated: true});
});

test('bounds concurrent reads and preserves the file-picker order', async () => {
  let active = 0;
  let peakActive = 0;
  const makeFile = (name, text) => {
    const file = new File([], name);
    file.slice = () => ({
      text: async () => {
        active += 1;
        peakActive = Math.max(peakActive, active);
        await new Promise(resolve => setTimeout(resolve, 5));
        active -= 1;
        return text;
      },
    });
    return file;
  };

  const blocks = await readAttachmentBlocks(
    [makeFile('first.txt', 'one'), makeFile('second.txt', 'two'), makeFile('third.txt', 'three')],
    10,
    2,
  );

  assert.equal(peakActive, 2);
  assert.deepEqual(blocks, [
    '**Attached: first.txt**\n\n```\none\n```',
    '**Attached: second.txt**\n\n```\ntwo\n```',
    '**Attached: third.txt**\n\n```\nthree\n```',
  ]);
});

test('stops scheduling attachments after a read fails and rejects with that error', async () => {
  const failure = new Error('read failed');
  const started = [];
  const makeFile = (name, action) => {
    const file = new File([], name);
    file.slice = () => ({text: action});
    return file;
  };
  const files = [
    makeFile('broken.txt', async () => {
      started.push('broken.txt');
      throw failure;
    }),
    makeFile('in-flight.txt', async () => {
      started.push('in-flight.txt');
      await new Promise(resolve => setTimeout(resolve, 5));
      return 'ok';
    }),
    makeFile('not-started.txt', async () => {
      started.push('not-started.txt');
      return 'ok';
    }),
  ];

  await assert.rejects(readAttachmentBlocks(files, 10, 2), error => error === failure);
  assert.deepEqual(started.sort(), ['broken.txt', 'in-flight.txt']);
});
