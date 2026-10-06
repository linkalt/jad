import assert from 'node:assert/strict';
import test from 'node:test';

import {
  checkpointDescription,
  checkpointTitle,
  parseCheckpointList,
  parseRestorePlan,
  planChangesFiles,
  planSummary,
  previewPaths,
  restoreChoice,
} from './checkpoint-view.ts';

const complete = {
  id: 'cp-1',
  kind: 'run',
  turn_id: 'turn-1',
  session_turn: 3,
  created_at: '2026-10-06T01:00:00.000Z',
  finished_at: '2026-10-06T01:00:05.000Z',
  state: 'complete',
  before_tree: 'aaa',
  after_tree: 'bbb',
  file_count: 12,
};

test('a list answer becomes rows without the tree ids', () => {
  const list = parseCheckpointList({enabled: true, checkpoints: [complete]});
  assert.equal(list.enabled, true);
  assert.equal(list.checkpoints.length, 1);
  const [row] = list.checkpoints;
  assert.equal(row.id, 'cp-1');
  assert.equal(row.sessionTurn, 3);
  assert.equal(row.fileCount, 12);
  assert.equal(row.hasBefore, true);
  assert.equal(row.hasAfter, true);
  assert.equal('before_tree' in row, false);
  assert.equal('beforeTree' in row, false);
});

test('a disabled session answers with no checkpoints and is still valid', () => {
  assert.deepEqual(parseCheckpointList({enabled: false, checkpoints: []}), {enabled: false, checkpoints: []});
});

test('a list answer that is not what the endpoint documents is refused', () => {
  const bad = [
    null,
    [],
    {enabled: 'yes', checkpoints: []},
    {enabled: true},
    {enabled: true, checkpoints: 'none'},
    {enabled: true, checkpoints: [null]},
    {enabled: true, checkpoints: [{...complete, id: ''}]},
    {enabled: true, checkpoints: [{...complete, kind: 'other'}]},
    {enabled: true, checkpoints: [{...complete, state: 'pending'}]},
    {enabled: true, checkpoints: [{...complete, session_turn: 'three'}]},
    {enabled: true, checkpoints: [{...complete, created_at: 5}]},
  ];
  for (const value of bad) {
    assert.throws(() => parseCheckpointList(value), /invalid checkpoint data/, JSON.stringify(value));
  }
});

test('a complete checkpoint offers safe first and full second', () => {
  const [row] = parseCheckpointList({enabled: true, checkpoints: [complete]}).checkpoints;
  assert.deepEqual(restoreChoice(row), {canRestore: true, modes: ['safe', 'full'], defaultMode: 'safe'});
});

test('a checkpoint with no run-end snapshot offers only full, as the server insists', () => {
  const {after_tree, ...open} = {...complete, state: 'open'};
  const [row] = parseCheckpointList({enabled: true, checkpoints: [open]}).checkpoints;
  assert.deepEqual(restoreChoice(row), {canRestore: true, modes: ['full'], defaultMode: 'full'});
});

test('a skipped or failed checkpoint cannot be restored and says why', () => {
  const {before_tree, after_tree, ...skipped} = {
    ...complete,
    state: 'skipped',
    warning: 'the workspace has more than 20000 files',
  };
  const [row] = parseCheckpointList({enabled: true, checkpoints: [skipped]}).checkpoints;
  const choice = restoreChoice(row);
  assert.equal(choice.canRestore, false);
  assert.deepEqual(choice.modes, []);
  assert.equal(choice.reason, 'the workspace has more than 20000 files');

  const {before_tree: b2, after_tree: a2, warning: w2, ...failed} = {...complete, state: 'failed'};
  const [failedRow] = parseCheckpointList({enabled: true, checkpoints: [failed]}).checkpoints;
  assert.equal(restoreChoice(failedRow).canRestore, false);
  assert.match(restoreChoice(failedRow).reason, /No state was saved/);
});

test('an undo point is titled and described as one', () => {
  const [row] = parseCheckpointList({
    enabled: true,
    checkpoints: [{...complete, id: 'cp-2', kind: 'pre_restore', restored_from: 'cp-1'}],
  }).checkpoints;
  assert.equal(checkpointTitle(row), 'Before a restore');
  assert.equal(row.restoredFrom, 'cp-1');
  assert.match(checkpointDescription(row), /undoes a restore/);
});

test('a run checkpoint is titled by its turn and counts files in the singular', () => {
  const [row] = parseCheckpointList({enabled: true, checkpoints: [{...complete, file_count: 1}]}).checkpoints;
  assert.equal(checkpointTitle(row), 'Turn 3');
  assert.equal(checkpointDescription(row), 'Saved · 1 file');
});

const dryRun = {
  restore: {
    checkpoint_id: 'cp-1',
    mode: 'safe',
    dry_run: true,
    restored: ['a.txt', 'b.txt'],
    deleted: ['c.txt'],
    conflicts: ['d.txt'],
  },
};

test('a dry-run answer is read, and has no undo point', () => {
  const plan = parseRestorePlan(dryRun);
  assert.equal(plan.dryRun, true);
  assert.deepEqual(plan.restored, ['a.txt', 'b.txt']);
  assert.deepEqual(plan.deleted, ['c.txt']);
  assert.deepEqual(plan.conflicts, ['d.txt']);
  assert.equal(plan.undoCheckpointId, undefined);
  assert.equal(planChangesFiles(plan), true);
});

test('a real restore carries the undo checkpoint', () => {
  const plan = parseRestorePlan({restore: {...dryRun.restore, dry_run: false, undo_checkpoint_id: 'cp-9'}});
  assert.equal(plan.dryRun, false);
  assert.equal(plan.undoCheckpointId, 'cp-9');
});

test('a restore answer that is not what the endpoint documents is refused', () => {
  const r = dryRun.restore;
  const bad = [
    null,
    {},
    {restore: null},
    {restore: {...r, checkpoint_id: 7}},
    {restore: {...r, mode: 'partial'}},
    {restore: {...r, dry_run: 'true'}},
    {restore: {...r, restored: 'a.txt'}},
    {restore: {...r, deleted: [1]}},
    {restore: {...r, conflicts: undefined}},
  ];
  for (const value of bad) {
    assert.throws(() => parseRestorePlan(value), /invalid restore data/, JSON.stringify(value));
  }
});

test('the summary uses the future tense for a dry run and the past for a real one', () => {
  const plan = parseRestorePlan(dryRun);
  assert.equal(planSummary(plan), 'Restores 2 files, deletes 1; leaves 1 changed file alone.');
  const done = parseRestorePlan({restore: {...dryRun.restore, dry_run: false}});
  assert.equal(planSummary(done), 'Restored 2 files, deleted 1; leaves 1 changed file alone.');
});

test('a plan with nothing to change says so, and cannot be confirmed', () => {
  const empty = parseRestorePlan({restore: {...dryRun.restore, restored: [], deleted: [], conflicts: []}});
  assert.equal(planChangesFiles(empty), false);
  assert.match(planSummary(empty), /already matches/);
});

test('a plan whose every path was changed since the run explains why nothing happens', () => {
  const blocked = parseRestorePlan({
    restore: {...dryRun.restore, restored: [], deleted: [], conflicts: ['d.txt', 'e.txt']},
  });
  assert.equal(planChangesFiles(blocked), false);
  assert.equal(planSummary(blocked), 'Nothing can be reverted safely; 2 files changed since the run.');
});

test('a long list of paths is cut at the configured limit and counts what it hid', () => {
  const paths = ['a', 'b', 'c', 'd', 'e'];
  assert.deepEqual(previewPaths(paths, 3), {shown: ['a', 'b', 'c'], hidden: 2});
  assert.deepEqual(previewPaths(paths, 5), {shown: paths, hidden: 0});
  assert.deepEqual(previewPaths(paths, 50), {shown: paths, hidden: 0});
  assert.deepEqual(previewPaths(paths, 0), {shown: [], hidden: 5});
  assert.deepEqual(previewPaths(paths, -4), {shown: [], hidden: 5});
  assert.deepEqual(previewPaths(paths, Number.NaN), {shown: [], hidden: 5});
});
