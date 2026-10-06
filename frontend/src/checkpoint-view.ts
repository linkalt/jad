/**
 * @file checkpoint-view.ts
 * @input The JSON of GET /sessions/{id}/checkpoints and POST .../restore
 * @output Validated checkpoint rows, the restore modes a row allows, and the wording of a restore plan
 * @position Pure model behind the Checkpoints panel
 *
 * A checkpoint is a snapshot of the workspace taken before a run changed it. The
 * server decides what a restore does; this file only reads the answer without
 * trusting its shape, and works out what the user may ask for. A safe restore
 * needs the snapshot taken when the run ended, because it reverts only what the
 * run changed; a run cut short by a restart has none, so only a full restore is
 * offered for it. Neither tree id is kept: the panel never needs the git detail.
 */

export type CheckpointKind = 'run' | 'pre_restore';
export type CheckpointState = 'open' | 'complete' | 'skipped' | 'failed';
export type RestoreMode = 'safe' | 'full';

export interface CheckpointRow {
  id: string;
  kind: CheckpointKind;
  state: CheckpointState;
  turnId: string;
  sessionTurn: number;
  createdAt: string;
  finishedAt?: string;
  fileCount?: number;
  warning?: string;
  /** For an undo point: the checkpoint whose restore it can reverse. */
  restoredFrom?: string;
  /** The workspace before the run exists, so a full restore is possible. */
  hasBefore: boolean;
  /** The workspace after the run exists, so a safe restore is possible. */
  hasAfter: boolean;
}

export interface CheckpointList {
  enabled: boolean;
  /** Newest first, as the server sends them. */
  checkpoints: CheckpointRow[];
}

export interface RestorePlan {
  checkpointId: string;
  mode: RestoreMode;
  dryRun: boolean;
  restored: string[];
  deleted: string[];
  conflicts: string[];
  undoCheckpointId?: string;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function optionalString(value: unknown): string | undefined {
  return typeof value === 'string' && value.length > 0 ? value : undefined;
}

function stringList(value: unknown): string[] | undefined {
  if (!Array.isArray(value) || value.some(entry => typeof entry !== 'string')) {
    return undefined;
  }
  return value as string[];
}

function parseRow(value: unknown): CheckpointRow | undefined {
  if (!isRecord(value)) {
    return undefined;
  }
  const {id, kind, state, turn_id, session_turn, created_at} = value;
  if (
    typeof id !== 'string' ||
    id.length === 0 ||
    (kind !== 'run' && kind !== 'pre_restore') ||
    (state !== 'open' && state !== 'complete' && state !== 'skipped' && state !== 'failed') ||
    typeof turn_id !== 'string' ||
    typeof session_turn !== 'number' ||
    !Number.isFinite(session_turn) ||
    typeof created_at !== 'string'
  ) {
    return undefined;
  }
  const finishedAt = optionalString(value.finished_at);
  const warning = optionalString(value.warning);
  const restoredFrom = optionalString(value.restored_from);
  return {
    id,
    kind,
    state,
    turnId: turn_id,
    sessionTurn: session_turn,
    createdAt: created_at,
    ...(finishedAt === undefined ? {} : {finishedAt}),
    ...(typeof value.file_count === 'number' && Number.isFinite(value.file_count)
      ? {fileCount: value.file_count}
      : {}),
    ...(warning === undefined ? {} : {warning}),
    ...(restoredFrom === undefined ? {} : {restoredFrom}),
    hasBefore: optionalString(value.before_tree) !== undefined,
    hasAfter: optionalString(value.after_tree) !== undefined,
  };
}

/** Reads the list answer; throws when any part of it is not what the endpoint documents. */
export function parseCheckpointList(value: unknown): CheckpointList {
  if (!isRecord(value) || typeof value.enabled !== 'boolean' || !Array.isArray(value.checkpoints)) {
    throw new Error('the server returned invalid checkpoint data');
  }
  const checkpoints: CheckpointRow[] = [];
  for (const entry of value.checkpoints) {
    const row = parseRow(entry);
    if (row === undefined) {
      throw new Error('the server returned invalid checkpoint data');
    }
    checkpoints.push(row);
  }
  return {enabled: value.enabled, checkpoints};
}

/** Reads the restore answer, a dry run or a real one. */
export function parseRestorePlan(value: unknown): RestorePlan {
  const restore = isRecord(value) ? value.restore : undefined;
  if (!isRecord(restore)) {
    throw new Error('the server returned invalid restore data');
  }
  const restored = stringList(restore.restored);
  const deleted = stringList(restore.deleted);
  const conflicts = stringList(restore.conflicts);
  if (
    typeof restore.checkpoint_id !== 'string' ||
    (restore.mode !== 'safe' && restore.mode !== 'full') ||
    typeof restore.dry_run !== 'boolean' ||
    restored === undefined ||
    deleted === undefined ||
    conflicts === undefined
  ) {
    throw new Error('the server returned invalid restore data');
  }
  const undoCheckpointId = optionalString(restore.undo_checkpoint_id);
  return {
    checkpointId: restore.checkpoint_id,
    mode: restore.mode,
    dryRun: restore.dry_run,
    restored,
    deleted,
    conflicts,
    ...(undoCheckpointId === undefined ? {} : {undoCheckpointId}),
  };
}

export interface RestoreChoice {
  /** Whether any restore is possible: the workspace before the run was saved. */
  canRestore: boolean;
  /** The modes the server will accept for this checkpoint, safest first. */
  modes: RestoreMode[];
  /** The mode to start on. */
  defaultMode: RestoreMode;
  /** Why nothing can be restored; set only when `canRestore` is false. */
  reason?: string;
}

/** What may be asked of one checkpoint; mirrors the server's own refusals. */
export function restoreChoice(row: CheckpointRow): RestoreChoice {
  if (!row.hasBefore) {
    return {
      canRestore: false,
      modes: [],
      defaultMode: 'safe',
      reason:
        row.warning ??
        (row.state === 'skipped' ? 'This snapshot was skipped.' : 'No state was saved for this checkpoint.'),
    };
  }
  if (!row.hasAfter) {
    return {canRestore: true, modes: ['full'], defaultMode: 'full'};
  }
  return {canRestore: true, modes: ['safe', 'full'], defaultMode: 'safe'};
}

export function checkpointTitle(row: CheckpointRow): string {
  return row.kind === 'pre_restore' ? 'Before a restore' : `Turn ${row.sessionTurn}`;
}

export function checkpointStateLabel(row: CheckpointRow): string {
  switch (row.state) {
    case 'open':
      return 'Not closed (running or interrupted)';
    case 'complete':
      return 'Saved';
    case 'skipped':
      return 'Skipped';
    case 'failed':
      return 'Failed';
  }
}

function pluralize(count: number, word: string): string {
  return `${count} ${word}${count === 1 ? '' : 's'}`;
}

export function checkpointDescription(row: CheckpointRow): string {
  const parts: string[] = [checkpointStateLabel(row)];
  if (row.fileCount !== undefined) {
    parts.push(pluralize(row.fileCount, 'file'));
  }
  if (row.kind === 'pre_restore') {
    parts.push('undoes a restore');
  }
  return parts.join(' · ');
}

/** The one-line outcome of a plan, in the tense of whether it has happened. */
export function planSummary(plan: RestorePlan): string {
  const writes = plan.restored.length;
  const removals = plan.deleted.length;
  if (writes === 0 && removals === 0) {
    return plan.conflicts.length > 0
      ? `Nothing can be reverted safely; ${pluralize(plan.conflicts.length, 'file')} changed since the run.`
      : 'Nothing to change: the workspace already matches this checkpoint.';
  }
  const verb = plan.dryRun ? ['Restores', 'deletes'] : ['Restored', 'deleted'];
  const parts = [`${verb[0]} ${pluralize(writes, 'file')}`, `${verb[1]} ${removals}`];
  const base = parts.join(', ');
  return plan.conflicts.length > 0
    ? `${base}; leaves ${pluralize(plan.conflicts.length, 'changed file')} alone.`
    : `${base}.`;
}

/** Whether confirming a plan would change anything. */
export function planChangesFiles(plan: RestorePlan): boolean {
  return plan.restored.length > 0 || plan.deleted.length > 0;
}

export interface PathPreview {
  shown: string[];
  /** How many paths the limit hid. */
  hidden: number;
}

/** The first `limit` paths of a long list; a limit below one shows none and counts all. */
export function previewPaths(paths: readonly string[], limit: number): PathPreview {
  const keep = Number.isFinite(limit) ? Math.max(0, Math.floor(limit)) : 0;
  return {shown: paths.slice(0, keep), hidden: Math.max(0, paths.length - keep)};
}

/** What a checkpoint does not protect, shown beside the restore button. */
export const CHECKPOINT_NOT_COVERED =
  'Not covered: files outside the working directory, ignored files, git history, network or database effects, and anything a background process changes later.';

/** The warning for the mode that overwrites without checking. */
export const FULL_RESTORE_WARNING =
  'A full restore also overwrites changes you made after the run and deletes files created since. An undo checkpoint is taken first.';
