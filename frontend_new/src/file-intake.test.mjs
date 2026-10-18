import assert from 'node:assert/strict';
import {test} from 'node:test';

import {
  classifyFiles,
  dragHasFiles,
  fileRule,
  isAcceptedFile,
  parseExtensionList,
  parseMimeList,
} from './file-intake.ts';
import {
  addAttachmentFiles,
  addImageFiles,
  buildRunInput,
  dataUrlBytes,
  formatBytes,
  ImageSlots,
  planAttachmentFiles,
  userMessageImages,
} from './image-attachments.ts';

const rules = {
  imageAccept: ['image/png', 'image/jpeg'],
  fileAccept: ['application/pdf'],
  fileExtensions: ['.pdf'],
  isImageEnabled: true,
  isFileEnabled: true,
};

test('parseExtensionList and parseMimeList split one accept string', () => {
  const accept = ' .PDF, application/pdf ,image/png,,. ,.txt';
  assert.deepEqual(parseExtensionList(accept), ['.pdf', '.txt']);
  assert.deepEqual(parseMimeList(accept), ['application/pdf', 'image/png']);
  assert.deepEqual(parseExtensionList(''), []);
  assert.deepEqual(parseMimeList(''), []);
});

test('isAcceptedFile takes a MIME type or an extension, case-insensitively', () => {
  const mimes = ['application/pdf'];
  const extensions = ['.pdf'];
  assert.equal(isAcceptedFile({name: 'a.bin', type: 'Application/PDF'}, mimes, extensions), true);
  assert.equal(isAcceptedFile({name: 'REPORT.PDF', type: ''}, mimes, extensions), true);
  assert.equal(isAcceptedFile({name: 'a.txt', type: 'text/plain'}, mimes, extensions), false);
  assert.equal(isAcceptedFile({name: 'pdf', type: ''}, mimes, extensions), false);
});

test('classifyFiles sorts images, files and refusals in order', () => {
  const files = [
    new File(['x'], 'a.png', {type: 'image/png'}),
    new File(['x'], 'report.pdf', {type: 'application/pdf'}),
    new File(['x'], 'SCAN.PDF', {type: ''}),
    new File(['x'], 'notes.md', {type: 'text/markdown'}),
    new File(['x'], 'x.zip', {type: 'application/zip'}),
    new File(['x'], '', {type: 'application/octet-stream'}),
    new File(['x'], 'b.jpg', {type: 'image/jpeg'}),
  ];
  const plan = classifyFiles(files, rules);
  assert.deepEqual(plan.images.map(f => f.name), ['a.png', 'b.jpg']);
  assert.deepEqual(plan.files.map(f => f.name), ['report.pdf', 'SCAN.PDF']);
  assert.deepEqual(plan.refused, [
    {name: 'notes.md', reason: 'not a supported image or file type'},
    {name: 'x.zip', reason: 'not a supported image or file type'},
    {name: 'pasted file', reason: 'not a supported image or file type'},
  ]);
});

test('classifyFiles refuses a kind that is switched off, with its own reason', () => {
  const files = [new File(['x'], 'a.png', {type: 'image/png'}), new File(['x'], 'n.pdf')];
  const noImages = classifyFiles(files, {...rules, isImageEnabled: false});
  assert.deepEqual(noImages.images, []);
  assert.deepEqual(noImages.refused, [{name: 'a.png', reason: 'image attachments are off'}]);
  assert.equal(noImages.files.length, 1);
  const noFiles = classifyFiles(files, {...rules, isFileEnabled: false});
  assert.deepEqual(noFiles.refused, [{name: 'n.pdf', reason: 'file attachments are off'}]);
  assert.equal(noFiles.images.length, 1);
});

test('dragHasFiles is true only when the drag carries files', () => {
  assert.equal(dragHasFiles(['text/plain', 'Files']), true);
  assert.equal(dragHasFiles(['text/plain']), false);
  assert.equal(dragHasFiles(null), false);
  assert.equal(dragHasFiles(undefined), false);
});

const pdfLimits = {maxCount: 2, maxBytes: 2048, mimeTypes: ['application/pdf'], extensions: ['.pdf']};

test('planAttachmentFiles applies the file rule: type, empty, size and count, with file wording', () => {
  const rule = fileRule(pdfLimits);
  const big = new File(['x'], 'big.pdf');
  Object.defineProperty(big, 'size', {value: 5000});
  const plan = planAttachmentFiles(
    [
      new File(['x'], 'a.pdf'),
      new File(['x'], 'b.txt'),
      new File([], 'empty.pdf'),
      big,
      new File(['x'], 'c.pdf'),
      new File(['x'], 'd.pdf'),
    ],
    0,
    rule,
  );
  assert.deepEqual(plan.accepted.map(f => f.name), ['a.pdf', 'c.pdf']);
  assert.deepEqual(plan.rejected, [
    {name: 'b.txt', reason: 'not a supported file type'},
    {name: 'empty.pdf', reason: 'the file is empty'},
    {name: 'big.pdf', reason: 'larger than 2 KiB'},
    {name: 'd.pdf', reason: 'a message holds at most 2 files'},
  ]);
  // Files already attached count against the limit.
  assert.deepEqual(planAttachmentFiles([new File(['x'], 'e.pdf')], 2, rule).rejected, [
    {name: 'e.pdf', reason: 'a message holds at most 2 files'},
  ]);
});

test('a pasted file with no name is called "pasted file"', () => {
  const plan = planAttachmentFiles([new File(['x'], '', {type: 'application/zip'})], 0, fileRule(pdfLimits));
  assert.deepEqual(plan.rejected, [{name: 'pasted file', reason: 'not a supported file type'}]);
});

test('addAttachmentFiles commits files with their names and the source file', async () => {
  const slots = new ImageSlots();
  const committed = [];
  const notices = [];
  const a = new File(['%PDF-1'], 'a.pdf');
  const b = new File(['%PDF-2'], 'b.pdf');
  await addAttachmentFiles([a, b], {
    draftKey: 'k',
    generation: slots.generation('k'),
    rule: fileRule(pdfLimits),
    slots,
    makeId: (() => {
      let n = 0;
      return () => `file-${++n}`;
    })(),
    read: async file => `data:application/pdf;base64,${file.name}`,
    commit: (attachment, file) => committed.push([attachment, file]),
    notify: notice => notices.push(notice),
  });
  assert.deepEqual(committed.map(([attachment]) => attachment), [
    {id: 'file-1', name: 'a.pdf', dataUrl: 'data:application/pdf;base64,a.pdf'},
    {id: 'file-2', name: 'b.pdf', dataUrl: 'data:application/pdf;base64,b.pdf'},
  ]);
  assert.equal(committed[0][1], a);
  assert.deepEqual(notices, ['']);
  assert.equal(slots.count('k'), 2);
});

test('image and file attachments of one draft count their own places', async () => {
  const imageSlots = new ImageSlots();
  const fileSlots = new ImageSlots();
  const done = [];
  const deps = (slots, rule) => ({
    draftKey: 'k',
    generation: 0,
    rule,
    slots,
    makeId: () => 'id',
    read: async () => 'data:x;base64,AA==',
    commit: attachment => done.push(attachment.name),
    notify: () => {},
  });
  const imageRule = {
    accepts: () => true,
    badType: 'x',
    maxCount: 1,
    maxBytes: 100,
    plural: 'images',
    fallbackName: 'pasted image',
  };
  await addAttachmentFiles([new File(['x'], 'i.png')], deps(imageSlots, imageRule));
  await addAttachmentFiles([new File(['x'], 'f.pdf')], deps(fileSlots, fileRule(pdfLimits)));
  assert.deepEqual(done, ['i.png', 'f.pdf']);
  assert.equal(imageSlots.count('k'), 1);
  assert.equal(fileSlots.count('k'), 1);
});

test('addImageFiles still shows refusals found before it, ahead of its own', async () => {
  const slots = new ImageSlots();
  const notices = [];
  const committed = [];
  const big = new File(['x'], 'big.png', {type: 'image/png'});
  Object.defineProperty(big, 'size', {value: 99});
  await addImageFiles([new File(['x'], 'ok.png', {type: 'image/png'}), big], {
    draftKey: 'k',
    generation: slots.generation('k'),
    limits: {maxCount: 8, maxBytes: 10, accept: ['image/png']},
    slots,
    makeId: () => 'id-1',
    read: async () => 'data:image/png;base64,eA==',
    commit: image => committed.push(image.name),
    notify: notice => notices.push(notice),
    extraRefused: [{name: 'x.zip', reason: 'not a supported image or file type'}],
  });
  assert.deepEqual(committed, ['ok.png']);
  assert.equal(notices.at(-1), 'x.zip: not a supported image or file type; big.png: larger than 1 KiB');
});

test('buildRunInput puts text, images, then files, and stays a string without attachments', () => {
  assert.equal(buildRunInput('hi', [], []), 'hi');
  assert.equal(buildRunInput('hi', []), 'hi');
  assert.deepEqual(
    buildRunInput(
      'read',
      [{dataUrl: 'data:image/png;base64,AA=='}],
      [{name: 'r.pdf', dataUrl: 'data:application/pdf;base64,JVBERi0='}],
    ),
    [
      {type: 'text', text: 'read'},
      {type: 'image', data: 'data:image/png;base64,AA=='},
      {type: 'file', name: 'r.pdf', data: 'data:application/pdf;base64,JVBERi0='},
    ],
  );
  assert.deepEqual(buildRunInput('', [], [{name: 'r.pdf', dataUrl: 'data:x'}]), [
    {type: 'text', text: ''},
    {type: 'file', name: 'r.pdf', data: 'data:x'},
  ]);
});

test('userMessageImages lists the files a message carries and hides only their placeholder', () => {
  // The server's file-only message: "[file: name]" is the text, the name is on the part.
  assert.deepEqual(
    userMessageImages({text: '[file: r.pdf]', parts: [{type: 'text', files: ['r.pdf']}]}),
    {sent: [], stored: [], files: ['r.pdf'], text: ''},
  );
  // Real text next to a file is kept.
  assert.deepEqual(
    userMessageImages({text: 'summarize', parts: [{type: 'text', files: ['r.pdf']}]}),
    {sent: [], stored: [], files: ['r.pdf'], text: 'summarize'},
  );
  // Two files, one repeated: both are listed, both placeholders are hidden.
  assert.deepEqual(
    userMessageImages({
      text: '[file: a.pdf] [file: a.pdf]',
      parts: [{type: 'text', files: ['a.pdf', 'a.pdf']}],
    }),
    {sent: [], stored: [], files: ['a.pdf', 'a.pdf'], text: ''},
  );
  // An image and a file with no text: the server writes both placeholders.
  assert.deepEqual(
    userMessageImages({
      text: '[image] [file: a.pdf]',
      parts: [{type: 'text', images: ['b'.repeat(64) + '.png'], files: ['a.pdf']}],
    }),
    {sent: [], stored: ['b'.repeat(64) + '.png'], files: ['a.pdf'], text: ''},
  );
  // A file with an inline image the server cannot list: its [image] placeholder stays.
  assert.deepEqual(
    userMessageImages({text: '[image] [file: a.pdf]', parts: [{type: 'text', files: ['a.pdf']}]}),
    {sent: [], stored: [], files: ['a.pdf'], text: '[image]'},
  );
  // What this tab just sent wins over the server's list, and the text is the user's own.
  assert.deepEqual(
    userMessageImages({text: 'look', sentFiles: ['n.pdf'], parts: [{type: 'text'}]}),
    {sent: [], stored: [], files: ['n.pdf'], text: 'look'},
  );
  // A user who typed "[file: x.pdf]" with nothing attached keeps their words.
  assert.equal(userMessageImages({text: '[file: x.pdf]'}).text, '[file: x.pdf]');
});

test('dataUrlBytes gives the decoded size of the base64 body, padding included', () => {
  const url = bytes => `data:application/pdf;base64,${Buffer.from(bytes).toString('base64')}`;
  for (const length of [1, 2, 3, 4, 5, 1000]) {
    assert.equal(dataUrlBytes(url(new Uint8Array(length))), length, `length ${length}`);
  }
  assert.equal(dataUrlBytes('data:application/pdf;base64,'), 0);
  assert.equal(formatBytes(dataUrlBytes(url(new Uint8Array(1536)))), '2 KiB');
});
