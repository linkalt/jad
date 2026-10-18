import assert from 'node:assert/strict';
import {readFile} from 'node:fs/promises';
import test from 'node:test';

import {
  hiddenWebUiStepIds,
  isWebUiPartHidden,
  visibleWebUiItems,
} from './web-ui-steps.ts';

const steps = [
  {id: 'response-1', startPart: 0, endPart: 4, hasTools: true},
  {id: 'response-2', startPart: 4, endPart: 7, hasTools: true},
  {id: 'response-summary', startPart: 7, endPart: 8, hasTools: false},
];

test('hides every part of earlier tool steps but keeps the final assistant summary', () => {
  const hidden = hiddenWebUiStepIds(steps, 1);

  assert.deepEqual([...hidden], ['response-1']);
  for (let index = 0; index < 4; index += 1) {
    assert.equal(isWebUiPartHidden(steps, hidden, index), true);
  }
  for (let index = 4; index < 8; index += 1) {
    assert.equal(isWebUiPartHidden(steps, hidden, index), false);
  }
});

test('defaults to summary-only visibility', async () => {
  const config = JSON.parse(await readFile(new URL('./config.json', import.meta.url), 'utf8'));
  assert.equal(config.message_step_window, 0);
});

test('zero visible tool steps hides every tool step while preserving the summary', () => {
  const hidden = hiddenWebUiStepIds(steps, 0);

  assert.deepEqual([...hidden], ['response-1', 'response-2']);
  assert.equal(isWebUiPartHidden(steps, hidden, 0), true);
  assert.equal(isWebUiPartHidden(steps, hidden, 7), false);
});

test('removes activity stacks with no visible groups and retains mixed stacks', () => {
  const hidden = hiddenWebUiStepIds(steps, 0);
  const activityGroups = [
    {id: 'reasoning-one', partIndex: 0},
    {id: 'tools-one', partIndex: 2},
    {id: 'summary', partIndex: 7},
  ];

  assert.deepEqual(
    visibleWebUiItems(activityGroups.slice(0, 2), item => item.partIndex, steps, hidden),
    [],
  );
  assert.deepEqual(
    visibleWebUiItems(activityGroups, item => item.partIndex, steps, hidden),
    [activityGroups[2]],
  );
});

test('keeps the latest tool step visible if a run has no final assistant summary', () => {
  const unfinishedSteps = steps.slice(0, 2);
  assert.deepEqual([...hiddenWebUiStepIds(unfinishedSteps, 0)], ['response-1']);
});

test('revealing enough steps clears the hidden set', () => {
  assert.deepEqual([...hiddenWebUiStepIds(steps, 2)], []);
});

test('resolves part ranges in a large step list', () => {
  const manySteps = Array.from({length: 10_000}, (_, index) => ({
    id: `step-${index}`,
    startPart: index * 4,
    endPart: (index + 1) * 4,
    hasTools: true,
  }));
  const hidden = hiddenWebUiStepIds(manySteps, 5);

  assert.equal(isWebUiPartHidden(manySteps, hidden, 0), true);
  assert.equal(isWebUiPartHidden(manySteps, hidden, 39_996), false);
  assert.equal(isWebUiPartHidden(manySteps, hidden, 39_999), false);
});

test('leaves legacy messages without step ranges visible', () => {
  const hidden = hiddenWebUiStepIds(undefined, 0);
  assert.equal(isWebUiPartHidden(undefined, hidden, 0), false);
});
