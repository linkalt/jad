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

const slotLimits = {maxCount: 3, maxBytes: 1000, accept: ['image/png']};

function recorder() {
  let id = 0;
  const committed = [];
  const notices = [];
  return {
    committed,
    notices,
    deps: (slots, extra = {}) => ({
      draftKey: 'd',
      generation: slots.generation('d'),
      limits: slotLimits,
      slots,
      makeId: () => `i${++id}`,
      read: async f => `data:${f.name}`,
      commit: image => committed.push(image),
      notify: text => notices.push(text),
      ...extra,
    }),
  };
}

const png = name => file(name, 'image/png', 5);

test('two adds that overlap, with no queue, still never exceed maxCount', async () => {
  const {ImageSlots, addImageFiles} = await import('./image-attachments.ts');
  const slots = new ImageSlots();
  const r = recorder();
  let open;
  const gate = new Promise(resolve => { open = resolve; });
  const read = async f => { await gate; return `data:${f.name}`; };
  // Both start before either read finishes: only the reservation can tell them apart.
  const first = addImageFiles([png('a1.png'), png('a2.png')], r.deps(slots, {read}));
  const second = addImageFiles([png('b1.png'), png('b2.png')], r.deps(slots, {read}));
  assert.equal(slots.count('d'), 3);
  open();
  await Promise.all([first, second]);
  assert.equal(r.committed.length, 3);
  assert.match(r.notices.join('|'), /b2\.png: a message holds at most 3 images/);
});

test('through the queue the adds also keep call order and the limit', async () => {
  const {ImageSlots, addImageFiles, createSerialQueue} = await import('./image-attachments.ts');
  const slots = new ImageSlots();
  const r = recorder();
  const serial = createSerialQueue();
  await Promise.all([
    serial(() => addImageFiles([png('a1.png'), png('a2.png')], r.deps(slots))),
    serial(() => addImageFiles([png('b1.png'), png('b2.png')], r.deps(slots))),
  ]);
  assert.deepEqual(r.committed.map(i => i.name), ['a1.png', 'a2.png', 'b1.png']);
  assert.equal(slots.count('d'), 3);
});

test('a file that cannot be read gives its place back and is reported', async () => {
  const {ImageSlots, addImageFiles} = await import('./image-attachments.ts');
  const slots = new ImageSlots();
  const r = recorder();
  const read = async f => { if (f.name === 'bad.png') throw new Error('x'); return 'data:ok'; };
  await addImageFiles([png('bad.png'), png('ok.png')], r.deps(slots, {read}));
  assert.deepEqual(r.committed.map(i => i.name), ['ok.png']);
  assert.equal(slots.count('d'), 1);
  assert.deepEqual(r.notices, ['bad.png: could not be read']);
});

test('slots are per draft, and release never goes below zero', async () => {
  const {ImageSlots} = await import('./image-attachments.ts');
  const slots = new ImageSlots();
  slots.reserve('a', 2);
  assert.equal(slots.count('b'), 0);
  slots.release('a', 5);
  assert.equal(slots.count('a'), 0);
  slots.reserve('a', 1);
  slots.clear('a');
  assert.equal(slots.count('a'), 0);
  assert.equal(slots.generation('a'), 1);
  assert.equal(slots.generation('b'), 0);
});

test('an add that commits nothing does not call commit', async () => {
  const {ImageSlots, addImageFiles} = await import('./image-attachments.ts');
  const r = recorder();
  await addImageFiles([file('x.txt', 'text/plain', 5)], r.deps(new ImageSlots()));
  assert.equal(r.committed.length, 0);
  assert.match(r.notices[0], /not a supported/);
});

test('a file still being read when the draft is sent is dropped with a notice, not carried into the next draft', async () => {
  const {ImageSlots, addImageFiles} = await import('./image-attachments.ts');
  const slots = new ImageSlots();
  const r = recorder();
  let open;
  const gate = new Promise(resolve => { open = resolve; });
  const read = async f => { await gate; return `data:${f.name}`; };
  const pending = addImageFiles([png('late.png')], r.deps(slots, {read}));
  slots.clear('d'); // the user sent the message while the file was being read
  open();
  await pending;
  assert.equal(r.committed.length, 0);
  assert.equal(slots.count('d'), 0, 'its place was freed by the send, and a stale add must not take it back');
  assert.match(r.notices[0], /late\.png: was still loading when the message was sent/);
});

test('an add that was offered before a send but runs after it is stale too', async () => {
  const {ImageSlots, addImageFiles, createSerialQueue} = await import('./image-attachments.ts');
  const slots = new ImageSlots();
  const r = recorder();
  const serial = createSerialQueue();
  let open;
  const gate = new Promise(resolve => { open = resolve; });
  // The generation is read when the user offers the files, as the hook does.
  const firstDeps = r.deps(slots, {read: async f => { await gate; return f.name; }});
  const secondDeps = r.deps(slots);
  const first = serial(() => addImageFiles([png('a.png')], firstDeps));
  const second = serial(() => addImageFiles([png('b.png')], secondDeps));
  slots.clear('d'); // sent while `second` waits behind `first`
  open();
  await Promise.all([first, second]);
  assert.equal(r.committed.length, 0);
  assert.equal(slots.count('d'), 0);
  assert.equal(r.notices.filter(n => /still loading/.test(n)).length, 2);
});

test('after a send the draft takes new files again', async () => {
  const {ImageSlots, addImageFiles} = await import('./image-attachments.ts');
  const slots = new ImageSlots();
  const r = recorder();
  await addImageFiles([png('a.png'), png('b.png'), png('c.png')], r.deps(slots));
  assert.equal(slots.count('d'), 3);
  slots.clear('d');
  await addImageFiles([png('d.png')], r.deps(slots));
  assert.deepEqual(r.committed.map(i => i.name), ['a.png', 'b.png', 'c.png', 'd.png']);
  assert.equal(slots.count('d'), 1);
});

test('without the queue a later small add can commit first; the queue keeps call order', async () => {
  const {ImageSlots, addImageFiles, createSerialQueue} = await import('./image-attachments.ts');
  const slow = f => new Promise(resolve => setTimeout(() => resolve(`data:${f.name}`), f.name === 'big.png' ? 30 : 1));
  const run = async queued => {
    const slots = new ImageSlots();
    const r = recorder();
    const serial = queued ? createSerialQueue() : task => task();
    await Promise.all([
      serial(() => addImageFiles([png('big.png')], r.deps(slots, {read: slow}))),
      serial(() => addImageFiles([png('small.png')], r.deps(slots, {read: slow}))),
    ]);
    return r.committed.map(i => i.name);
  };
  assert.deepEqual(await run(false), ['small.png', 'big.png']);
  assert.deepEqual(await run(true), ['big.png', 'small.png']);
});

test('the queue keeps running after a task throws, and reports that error to its caller', async () => {
  const {createSerialQueue} = await import('./image-attachments.ts');
  const serial = createSerialQueue();
  const order = [];
  const first = serial(async () => { order.push('first'); throw new Error('boom'); });
  const second = serial(async () => { order.push('second'); });
  await assert.rejects(first, /boom/);
  await second;
  assert.deepEqual(order, ['first', 'second']);
});

test('a stale read that fails does not free a place a newer add holds', async () => {
  const {ImageSlots, addImageFiles} = await import('./image-attachments.ts');
  const slots = new ImageSlots();
  const r = recorder();
  let fail;
  const failing = new Promise((_, reject) => { fail = reject; });
  let open;
  const gate = new Promise(resolve => { open = resolve; });
  const stale = addImageFiles([png('old.png')], r.deps(slots, {read: () => failing}));
  slots.clear('d'); // sent while old.png was being read
  const fresh = addImageFiles([png('new.png')], r.deps(slots, {read: async f => { await gate; return f.name; }}));
  assert.equal(slots.count('d'), 1, 'new.png holds one place');
  fail(new Error('read failed'));
  await stale;
  assert.equal(slots.count('d'), 1, 'the stale failure must not give new.png place back');
  open();
  await fresh;
  assert.deepEqual(r.committed.map(i => i.name), ['new.png']);
});
