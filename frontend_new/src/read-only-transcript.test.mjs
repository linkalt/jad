import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import test from 'node:test';

import {readOnlySegments} from './read-only-transcript.ts';
import {subagentTranscriptMessages} from './subagent-transcript.ts';
import {pairToolParts} from './web-ui-part-groups.ts';

const text = value => ({type: 'text', text: value});
const reasoning = value => ({type: 'reasoning', text: value});
const call = (callId, name = 'read') => ({type: 'tool_call', callId, name, arguments: '{}'});
const result = (callId, output = 'ok', status = 'success') => ({type: 'tool_result', callId, output, status});
const message = (parts, extra = {}) => ({key: 'm', role: 'assistant', parts, ...extra});

function shape(segments) {
  return segments.map(segment =>
    segment.type === 'content'
      ? `text`
      : segment.rows
          .map(row =>
            row.type === 'tools'
              ? `tools[${row.calls.map(entry => `${entry.call.callId}${entry.result ? '+' : '-'}`).join(',')}${row.orphans.length > 0 ? `|orphans:${row.orphans.map(o => o.result.callId)}` : ''}]`
              : row.type,
          )
          .join(' '),
  );
}

test('calls pair with the result that follows them, by call id and not by position', () => {
  const segments = readOnlySegments(
    message([call('a'), call('b'), result('b', 'second'), result('a', 'first')]),
  );
  assert.deepEqual(shape(segments), ['tools[a+,b+]']);
  const [row] = segments[0].rows;
  assert.equal(row.calls[0].result.output, 'first');
  assert.equal(row.calls[1].result.output, 'second');
});

test('a result with no call before it is an orphan; a call with no result stays unpaired', () => {
  assert.deepEqual(shape(readOnlySegments(message([result('x'), call('a')]))), ['tools[a-|orphans:x]']);
  // A result that comes before its call does not answer it.
  const {calls, orphans} = pairToolParts([
    {part: result('a'), index: 0},
    {part: call('a'), index: 1},
  ]);
  assert.equal(calls[0].result, undefined);
  assert.deepEqual(orphans.map(item => item.index), [0]);
});

test('text, reasoning and tool rows keep their order; reasoning and tools stack, text stands apart', () => {
  const segments = readOnlySegments(
    message([reasoning('think'), call('a'), result('a'), text('answer'), reasoning('again'), text('  '), text('end')]),
  );
  assert.deepEqual(shape(segments), ['reasoning tools[a+]', 'text', 'reasoning', 'text']);
});

test('blank text and reasoning parts are skipped', () => {
  assert.deepEqual(shape(readOnlySegments(message([text(' '), reasoning('\n')]))), []);
});

test('a run of tool calls ends at a step boundary', () => {
  const parts = [call('a'), result('a'), call('b'), result('b')];
  assert.deepEqual(shape(readOnlySegments(message(parts))), ['tools[a+,b+]']);
  const steps = [
    {id: '1', startPart: 0, endPart: 2, hasTools: true},
    {id: '2', startPart: 2, endPart: 4, hasTools: true},
  ];
  assert.deepEqual(shape(readOnlySegments(message(parts, {steps}))), ['tools[a+] tools[b+]']);
});

test('a hosted tool part is its own row', () => {
  const hosted = {type: 'hosted_tool', callId: 'h', name: 'web_search', status: 'completed'};
  assert.deepEqual(shape(readOnlySegments(message([hosted, call('a')]))), ['hosted tools[a-]']);
});

test('row keys are scoped by the message, so two messages never collide', () => {
  const parts = [reasoning('r'), call('a'), result('a'), text('t')];
  const keys = messageKey =>
    readOnlySegments({key: messageKey, role: 'assistant', parts}).flatMap(segment =>
      segment.type === 'content'
        ? [segment.row.key]
        : segment.rows.flatMap(row => (row.type === 'tools' ? [row.key, ...row.calls.map(c => c.key)] : [row.key])),
    );
  const first = keys('one');
  const second = keys('two');
  assert.equal(new Set([...first, ...second]).size, first.length + second.length);
  assert.ok(first.every(key => key.startsWith('one:')));
});

test('the subagent snapshot becomes one assistant message with a range per completed step', () => {
  const messages = subagentTranscriptMessages({
    agent: 'researcher',
    steps: [
      {id: '1.1', status: 'completed', parts: [reasoning('think'), call('a'), result('a')]},
      {id: '1.2', status: 'running', parts: [text('not yet')]},
      {id: '1.3', status: 'completed', parts: []},
      {id: '1.4', status: 'completed', parts: [call('b'), result('b'), text('done')]},
    ],
  });
  assert.equal(messages.length, 1);
  const [only] = messages;
  assert.equal(only.role, 'assistant');
  // The trailing step's closing text is the tool result's output, so it is not shown.
  assert.equal(only.parts.length, 5);
  assert.deepEqual(only.steps, [
    {id: '1.1', startPart: 0, endPart: 3, hasTools: true},
    {id: '1.4', startPart: 3, endPart: 5, hasTools: true},
  ]);
  // The steps' calls pair with their own results and stay in their own step.
  assert.deepEqual(shape(readOnlySegments(only)), ['reasoning tools[a+] tools[b+]']);
});

test('no completed step, no snapshot, or a malformed snapshot gives no message', () => {
  assert.deepEqual(subagentTranscriptMessages(undefined), []);
  assert.deepEqual(subagentTranscriptMessages({steps: [{id: 'a', status: 'running', parts: [text('x')]}]}), []);
  assert.deepEqual(subagentTranscriptMessages({steps: 'no'}), []);
});

// There is no DOM in this suite, so what a click inside the snapshot can reach is pinned
// at the source: the shared renderer expands rows through the Astryx row's own chevron
// and never touches the pane's panel wiring.
const component = name => readFileSync(new URL(`./components/${name}`, import.meta.url), 'utf8');
const code = source => source.replace(/\/\*[\s\S]*?\*\//g, '').replace(/^\s*\/\/.*$/gm, '');

test('an inline expand inside the snapshot opens no panel', () => {
  for (const name of ['ReadOnlyTranscript.tsx', 'SubagentTranscript.tsx']) {
    const source = code(component(name));
    assert.doesNotMatch(source, /ActivityPanelContext|ActivityRow|activity-panel|right-panel|useContext/, `${name} must not reach the panel`);
    assert.doesNotMatch(source, /data-panel-toggle|onKeyDown|role="button"/, `${name} must not add its own click targets`);
  }
  // The section's only click targets are the Show/Hide Transcript disclosure and the retry
  // of a failed load; the renderer has none.
  assert.doesNotMatch(code(component('ReadOnlyTranscript.tsx')), /onClick/);
  assert.equal((code(component('SubagentTranscript.tsx')).match(/onClick/g) ?? []).length, 2);
  const renderer = code(component('ReadOnlyTranscript.tsx'));
  // Rows expand in place: every call and reasoning row carries its own detail.
  assert.equal((renderer.match(/resultDetail:/g) ?? []).length, 3);
  assert.match(renderer, /<ChatToolCalls/);
  // Nothing streams or runs, and the inline detail never nests a snapshot.
  assert.doesNotMatch(renderer, /isStreaming: true|isStreaming=\{true\}|Load earlier steps|useEffect|fetch\(/);
  assert.match(renderer, /<ToolCallDetail call=\{entry\.call\} result=\{entry\.result\} \/>/);
});

test('the panel hosts the snapshot below the result and the shared body does not know it', () => {
  const panel = code(component('ActivityPanel.tsx'));
  assert.match(panel, /<ToolCallDetail[^>]*>\s*\{detail\.result\?\.subagent !== undefined && \(\s*<SubagentTranscript/);
  const parts = code(component('TranscriptParts.tsx'));
  assert.doesNotMatch(parts, /SubagentTranscript|ReadOnlyTranscript/, 'TranscriptParts must not import the read-only view (no cycle)');
});

test('the panel shows the transcript behind a disclosure that starts closed on every open', () => {
  const view = code(component('SubagentTranscript.tsx'));
  // Closed by default; one click flips it; the control names the action it will take and
  // carries the step count of the summary.
  assert.match(view, /useState\(false\)/);
  assert.match(view, /\$\{isOpen \? 'Hide' : 'Show'\} Transcript/);
  assert.match(view, /onClick=\{\(\) => setIsOpen\(open => !open\)\}/);
  // The heading and the transcript exist only in the open component, mounted while open.
  assert.match(view, /\{isOpen && \(\s*<OpenSubagentTranscript/);
  assert.equal((view.match(/<ReadOnlyTranscript/g) ?? []).length, 1);
  assert.ok(view.indexOf("'Subagent transcript'") > view.indexOf('function OpenSubagentTranscript'));
  // The panel stays mounted while its detail switches, so the call's key resets the state;
  // closing the panel unmounts it, so reopening starts closed too.
  const panel = code(component('ActivityPanel.tsx'));
  assert.match(panel, /<SubagentTranscript\s+key=\{detail\.key\}/);
});
