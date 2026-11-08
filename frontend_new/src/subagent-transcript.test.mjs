import assert from 'node:assert/strict';
import test from 'node:test';

import {completedSubagentSteps, subagentTranscriptMessages} from './subagent-transcript.ts';

const text = value => [{type: 'text', text: value}];

test('no snapshot or no steps gives nothing', () => {
  assert.deepEqual(completedSubagentSteps(undefined), []);
  assert.deepEqual(completedSubagentSteps({steps: []}), []);
});

test('only completed steps with parts are kept, in order', () => {
  const steps = completedSubagentSteps({
    agent: 'researcher',
    steps: [
      {id: 'a', status: 'completed', parts: text('one')},
      {id: 'b', status: 'running', parts: text('two')},
      {id: 'c', status: 'pending', parts: text('three')},
      {id: 'd', status: 'completed', parts: []},
      {id: 'e', status: 'weird', parts: text('four')},
      {id: 'f', status: 'completed', parts: text('five')},
    ],
  });
  assert.deepEqual(steps.map(step => step.id), ['a', 'f']);
});

test('a malformed snapshot shows nothing instead of throwing', () => {
  assert.deepEqual(completedSubagentSteps({steps: 'no'}), []);
  assert.deepEqual(completedSubagentSteps({steps: [null]}), []);
  assert.deepEqual(completedSubagentSteps({agent: 3, steps: [{id: 'a', status: 'completed', parts: text('x')}]}), []);
  assert.deepEqual(completedSubagentSteps({steps: [{id: 'a', status: 'completed', parts: [{type: 'text'}]}]}), []);
});

const reasoning = value => ({type: 'reasoning', text: value});
const call = callId => ({type: 'tool_call', callId, name: 'read', arguments: '{}'});
const result = callId => ({type: 'tool_result', callId, output: 'ok', status: 'success'});
const step = (id, parts) => ({id, status: 'completed', parts});
const shown = steps => subagentTranscriptMessages({steps}).flatMap(message => message.parts);

test('with a tool call, the trailing text-only step (the summary) is dropped whole', () => {
  const [message] = subagentTranscriptMessages({
    steps: [step('1', [call('a'), result('a')]), step('2', text('final summary'))],
  });
  assert.deepEqual(message.parts, [call('a'), result('a')]);
  assert.deepEqual(message.steps, [{id: '1', startPart: 0, endPart: 2, hasTools: true}]);
});

test('with a tool call, the trailing step keeps its reasoning and loses only its text', () => {
  const [message] = subagentTranscriptMessages({
    steps: [step('1', [call('a'), result('a')]), step('2', [reasoning('wrap up'), ...text('final summary')])],
  });
  assert.deepEqual(message.parts, [call('a'), result('a'), reasoning('wrap up')]);
  assert.deepEqual(message.steps.map(s => [s.id, s.startPart, s.endPart, s.hasTools]), [
    ['1', 0, 2, true],
    ['2', 2, 3, false],
  ]);
});

test('only the trailing step loses its text; earlier text stays', () => {
  assert.deepEqual(
    shown([step('1', [...text('plan'), call('a'), result('a')]), step('2', text('summary'))]),
    [...text('plan'), call('a'), result('a')],
  );
});

test('a snapshot with a single tool step and its text keeps the calls, drops the text', () => {
  assert.deepEqual(shown([step('1', [call('a'), result('a'), ...text('done')])]), [call('a'), result('a')]);
});

test('without any tool call the snapshot is shown whole, summary included', () => {
  assert.deepEqual(shown([step('1', [reasoning('think')]), step('2', text('answer'))]), [reasoning('think'), ...text('answer')]);
  assert.deepEqual(shown([step('1', text('answer'))]), text('answer'));
});

test('nothing surviving gives no message, so no button renders', () => {
  assert.deepEqual(subagentTranscriptMessages(undefined), []);
  assert.deepEqual(subagentTranscriptMessages({steps: [{id: '1', status: 'running', parts: [call('a')]}]}), []);
  assert.deepEqual(subagentTranscriptMessages({steps: 'no'}), []);
  // Running steps do not count as tool calls: the completed text-only snapshot stays whole.
  assert.deepEqual(shown([step('1', text('x')), {id: '2', status: 'running', parts: [call('a')]}]), text('x'));
});
