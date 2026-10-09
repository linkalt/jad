import assert from 'node:assert/strict';
import {test} from 'node:test';

import {applyPart, applyPartDelta} from './messagePatch.ts';

function makeMessage(parts) {
  const text = parts.filter(part => part.type === 'text').map(part => part.text).join('');
  const reasoningContent = parts
    .filter(part => part.type === 'reasoning')
    .map(part => part.text)
    .join('\n\n')
    .trim();
  return {
    id: 'run-1',
    sender: 'assistant',
    text,
    timestamp: 'now',
    parts,
    ...(reasoningContent.length > 0 ? {reasoningContent} : {}),
  };
}

function rebuild(message, index, part) {
  if (index > message.parts.length) return message;
  const parts = message.parts.slice();
  parts[index] = part;
  const text = parts.filter(item => item.type === 'text').map(item => item.text).join('');
  const reasoningContent = parts
    .filter(item => item.type === 'reasoning')
    .map(item => item.text)
    .join('\n\n')
    .trim();
  const next = {...message, parts, text};
  if (reasoningContent.length > 0) next.reasoningContent = reasoningContent;
  else delete next.reasoningContent;
  return next;
}

test('appends to the final text part without changing other parts or the original message', () => {
  const original = makeMessage([
    {type: 'text', text: 'intro'},
    {type: 'tool_call', callId: 'c1', name: 'search', arguments: '{}'},
    {type: 'text', text: 'answer'},
  ]);

  const next = applyPartDelta(original, 2, ' more');

  assert.equal(next.text, 'introanswer more');
  assert.equal(next.parts[2].text, 'answer more');
  assert.equal(original.parts[2].text, 'answer');
  assert.notEqual(next.parts, original.parts);
});

test('rebuilds text when a delta targets a non-final text part', () => {
  const original = makeMessage([
    {type: 'text', text: 'first'},
    {type: 'reasoning', text: 'thinking'},
    {type: 'text', text: 'last'},
  ]);

  const next = applyPartDelta(original, 0, '!');

  assert.equal(next.text, 'first!last');
  assert.equal(next.reasoningContent, 'thinking');
});

test('appends reasoning incrementally only when trimming remains correct', () => {
  const parts = [
    {type: 'reasoning', text: 'plan'},
    {type: 'text', text: 'visible'},
    {type: 'reasoning', text: 'answer'},
  ];
  const next = applyPartDelta(makeMessage(parts), 2, ' now');
  assert.equal(next.reasoningContent, 'plan\n\nanswer now');
  assert.equal(next.text, 'visible');

  const whitespace = makeMessage([{type: 'reasoning', text: 'answer '}]);
  const afterWhitespace = applyPartDelta(whitespace, 0, 'now');
  assert.equal(afterWhitespace.reasoningContent, 'answer now');
});

test('tool deltas do not rebuild the message text projections', () => {
  const original = makeMessage([
    {type: 'text', text: 'visible'},
    {type: 'reasoning', text: 'private thought'},
    {type: 'tool_call', callId: 'c1', name: 'search', arguments: '{'},
    {type: 'tool_result', callId: 'c1', output: 'out', status: 'success'},
  ]);

  const call = applyPartDelta(original, 2, '}');
  const result = applyPartDelta(original, 3, 'put');

  assert.equal(call.parts[2].arguments, '{}');
  assert.equal(call.text, original.text);
  assert.equal(call.reasoningContent, original.reasoningContent);
  assert.equal(result.parts[3].output, 'output');
  assert.equal(result.text, original.text);
  assert.equal(result.reasoningContent, original.reasoningContent);
});

test('delta results match a full projection rebuild across mixed parts and whitespace', () => {
  const samples = [
    [{type: 'text', text: 'one'}, {type: 'text', text: 'two'}],
    [{type: 'reasoning', text: 'plan'}, {type: 'reasoning', text: 'answer'}],
    [{type: 'reasoning', text: '  '}, {type: 'reasoning', text: 'answer '}],
    [
      {type: 'text', text: 'visible'},
      {type: 'tool_call', callId: 'c1', name: 'search', arguments: '{}'},
      {type: 'reasoning', text: 'thought'},
      {type: 'tool_result', callId: 'c1', output: 'done', status: 'success'},
    ],
  ];
  const appends = ['x', ' ', '\n', '  more'];

  for (const sample of samples) {
    for (let index = 0; index < sample.length; index += 1) {
      for (const append of appends) {
        const original = makeMessage(sample);
        const part = original.parts[index];
        const grown = part.type === 'tool_call'
          ? {...part, arguments: part.arguments + append}
          : part.type === 'tool_result'
            ? {...part, output: part.output + append}
            : {...part, text: part.text + append};
        assert.deepEqual(applyPartDelta(original, index, append), rebuild(original, index, grown));
      }
    }
  }
});

test('part replacements still recompute both derived fields', () => {
  const original = makeMessage([
    {type: 'text', text: 'visible'},
    {type: 'reasoning', text: 'thought'},
  ]);

  const next = applyPart(original, 0, {type: 'reasoning', text: 'new thought'});

  assert.equal(next.text, '');
  assert.equal(next.reasoningContent, 'new thought\n\nthought');
  assert.equal(applyPart(original, 4, {type: 'text', text: 'ignored'}), original);
});

test('empty deltas and missing part indexes are no-ops', () => {
  const original = makeMessage([{type: 'text', text: 'visible'}]);
  assert.equal(applyPartDelta(original, 0, ''), original);
  assert.equal(applyPartDelta(original, 4, 'ignored'), original);
});

test('a web_search part is never grown by a text delta and does not change the text fields', () => {
  const search = {type: 'web_search', callId: 'ws_1', query: 'q', status: 'completed'};
  const original = makeMessage([{type: 'text', text: 'answer'}, search]);
  assert.equal(applyPartDelta(original, 1, ' more'), original);
  assert.deepEqual(original.parts[1], search);

  const patched = applyPart(original, 1, {...search, status: 'failed'});
  assert.equal(patched.text, 'answer');
  assert.equal(patched.parts[1].status, 'failed');
  assert.equal(patched.parts[1].text, undefined);
  assert.equal(patched.reasoningContent, undefined);
});

test('cited text keeps its citations through a part replacement', () => {
  const citations = [{url: 'https://go.dev', title: 'Go'}];
  const original = makeMessage([{type: 'text', text: 'Go'}]);
  const patched = applyPart(original, 0, {type: 'text', text: 'Go 1.27', citations});
  assert.equal(patched.text, 'Go 1.27');
  assert.deepEqual(patched.parts[0].citations, citations);
});
