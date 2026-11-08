import assert from 'node:assert/strict';
import test from 'node:test';

import {completedSubagentSteps} from './subagent-transcript.ts';

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
