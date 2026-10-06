import assert from 'node:assert/strict';
import test from 'node:test';

import {approvalRows, interactionFailureTitle, reviewApprovalAnswer, truncationNote} from './approval-answer.ts';

test('a bash preview becomes one row per field, without the tool name', () => {
  const rows = approvalRows({tool: 'bash', command: 'rm -rf build', cwd: '/work', timeout_ms: 5000});
  assert.deepEqual(
    rows.map((row) => [row.label, row.text]),
    [
      ['command', 'rm -rf build'],
      ['cwd', '/work'],
      ['timeout_ms', '5000'],
    ],
  );
  assert.equal(rows.every((row) => row.truncatedBytes === undefined), true);
});

test('a cut field says how long it really was and gets no row of its own', () => {
  const rows = approvalRows({tool: 'write', path: 'a.txt', content: 'abc', content_truncated_bytes: 9000, content_bytes: 9000});
  assert.deepEqual(
    rows.map((row) => row.label),
    ['path', 'content', 'content_bytes'],
  );
  const content = rows.find((row) => row.label === 'content');
  assert.equal(content?.truncatedBytes, 9000);
  assert.equal(rows.find((row) => row.label === 'path')?.truncatedBytes, undefined);
  assert.match(truncationNote(9000), /9000 bytes/);
});

test('booleans and objects are shown as text, an MCP preview keeps its server', () => {
  const rows = approvalRows({tool: 'search', server: 'zvec', arguments: '{"q":"x"}', replaceAll: true, extra: {a: 1}});
  assert.deepEqual(
    rows.map((row) => [row.label, row.text]),
    [
      ['server', 'zvec'],
      ['arguments', '{"q":"x"}'],
      ['replaceAll', 'true'],
      ['extra', '{\n  "a": 1\n}'],
    ],
  );
});

test('a preview that is not an object has no rows', () => {
  assert.deepEqual(approvalRows(undefined), []);
  assert.deepEqual(approvalRows(null), []);
  assert.deepEqual(approvalRows(['x']), []);
});

test('a non-numeric or negative cut marker is ignored', () => {
  const rows = approvalRows({tool: 'bash', command: 'x', command_truncated_bytes: '10'});
  assert.equal(rows.length, 1);
  assert.equal(rows[0].truncatedBytes, undefined);
  const negative = approvalRows({tool: 'bash', command: 'x', command_truncated_bytes: -1});
  assert.equal(negative[0].truncatedBytes, undefined);
});

test('allow posts a bare accept, whatever reason text is left over', () => {
  assert.deepEqual(reviewApprovalAnswer({decision: 'accept', reason: 'stale text', reasonMaxLength: 10}), {
    body: {decision: 'accept'},
  });
});

test('deny needs a reason and trims it', () => {
  assert.deepEqual(reviewApprovalAnswer({decision: 'decline', reason: '   ', reasonMaxLength: 100}), {
    error: 'Say why you are denying it, so the agent can try something else.',
  });
  assert.deepEqual(reviewApprovalAnswer({decision: 'decline', reason: '  use a dry run  ', reasonMaxLength: 100}), {
    body: {decision: 'decline', reason: 'use a dry run'},
  });
});

test('deny refuses a reason longer than the endpoint accepts', () => {
  const tooLong = reviewApprovalAnswer({decision: 'decline', reason: 'x'.repeat(11), reasonMaxLength: 10});
  assert.deepEqual(tooLong, {error: 'Keep the reason under 10 characters.'});
  const exact = reviewApprovalAnswer({decision: 'decline', reason: 'x'.repeat(10), reasonMaxLength: 10});
  assert.equal('body' in exact, true);
});

test('a refused answer or extension names an approval as an approval and anything else as a draft', () => {
  assert.equal(interactionFailureTitle('approval', 'answer'), 'Unable to answer the approval');
  assert.equal(interactionFailureTitle('approval', 'extend'), 'Unable to extend the approval');
  assert.equal(interactionFailureTitle('draft', 'answer'), 'Unable to answer the draft');
  assert.equal(interactionFailureTitle(undefined, 'extend'), 'Unable to extend the draft');
});
