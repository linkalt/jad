/**
 * @file CheckpointPanel.tsx
 * @input The active session and profile, and whether a run is in flight
 * @output The right-edge panel that lists a session's checkpoints and restores one
 * @position The undo surface for files the agent changed
 *
 * Restoring is never one click. Choosing a checkpoint first asks the server what
 * the restore would do (a dry run) and shows the answer; only a second, separate
 * click on the confirm button changes files. Switching mode asks again, because
 * the two modes touch different files. While a run is active every restore
 * control is disabled, and the server refuses it too.
 */
import {useCallback, useEffect, useRef, useState} from 'react';

import {Button, HStack, Icon, SegmentedControl, SegmentedControlItem, StatusDot, Text, VStack} from '@astryxdesign/core';
import {CodeBlock} from '@astryxdesign/core/CodeBlock';
import {EmptyState} from '@astryxdesign/core/EmptyState';
import {List, ListItem} from '@astryxdesign/core/List';
import type {ResizableRegion} from '@astryxdesign/core/Resizable';

import {fetchSessionCheckpoints, restoreSessionCheckpoint} from '../api';
import {
  CHECKPOINT_NOT_COVERED,
  FULL_RESTORE_WARNING,
  checkpointDescription,
  checkpointTitle,
  planChangesFiles,
  planSummary,
  previewPaths,
  restoreChoice,
  type CheckpointList,
  type CheckpointRow,
  type RestoreMode,
  type RestorePlan,
} from '../checkpoint-view';
import {FloatingPanel} from './FloatingPanel';

export interface CheckpointPanelProps {
  sessionId: string;
  profile: string | undefined;
  /** True while a run is in flight; restoring would change files under it. */
  isRunning: boolean;
  region: ResizableRegion;
  /** How many paths of one group are listed before "and N more". */
  pathsPreviewLimit: number;
  /** The tallest a list of paths grows before it scrolls. */
  pathsMaxHeightPx: number;
  onClose: () => void;
}

type Review = {
  row: CheckpointRow;
  mode: RestoreMode;
  /** The dry run for this row and mode; `loading` until the server answers. */
  preview: {state: 'loading'} | {state: 'ready'; plan: RestorePlan} | {state: 'error'; message: string};
  isApplying: boolean;
  applyError: string | null;
  /** Set once a real restore finished. */
  done: RestorePlan | null;
};

function checkpointTime(value: string): string {
  const time = Date.parse(value);
  if (Number.isNaN(time)) {
    return value;
  }
  return new Intl.DateTimeFormat(undefined, {dateStyle: 'short', timeStyle: 'short'}).format(time);
}

function stateVariant(row: CheckpointRow): 'success' | 'warning' | 'error' | 'neutral' {
  switch (row.state) {
    case 'complete':
      return 'success';
    case 'failed':
      return 'error';
    case 'open':
      return 'warning';
    case 'skipped':
      return 'neutral';
  }
}

function messageOf(error: unknown): string {
  return error instanceof Error ? error.message : 'request failed';
}

function PathGroup({
  title,
  paths,
  limit,
  maxHeightPx,
}: {
  title: string;
  paths: string[];
  limit: number;
  maxHeightPx: number;
}) {
  if (paths.length === 0) {
    return null;
  }
  const {shown, hidden} = previewPaths(paths, limit);
  return (
    <VStack gap={1} width="100%">
      <Text type="supporting" color="secondary">
        {`${title} (${paths.length})`}
      </Text>
      <CodeBlock
        code={shown.join('\n')}
        language="text"
        size="sm"
        width="100%"
        container="card"
        maxHeight={maxHeightPx}
        hasCopyButton
      />
      {hidden > 0 ? (
        <Text type="supporting" color="secondary">
          {`and ${hidden} more`}
        </Text>
      ) : null}
    </VStack>
  );
}

export function CheckpointPanel({
  sessionId,
  profile,
  isRunning,
  region,
  pathsPreviewLimit,
  pathsMaxHeightPx,
  onClose,
}: CheckpointPanelProps) {
  const [list, setList] = useState<CheckpointList | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [isLoading, setIsLoading] = useState(true);
  const [review, setReview] = useState<Review | null>(null);
  // A late answer for an earlier session, row or mode must not overwrite a newer one.
  const loadSequence = useRef(0);
  const previewSequence = useRef(0);

  const load = useCallback(() => {
    const sequence = ++loadSequence.current;
    setIsLoading(true);
    setLoadError(null);
    fetchSessionCheckpoints(sessionId, profile)
      .then(result => {
        if (sequence === loadSequence.current) {
          setList(result);
        }
      })
      .catch((error: unknown) => {
        if (sequence === loadSequence.current) {
          setLoadError(messageOf(error));
        }
      })
      .finally(() => {
        if (sequence === loadSequence.current) {
          setIsLoading(false);
        }
      });
  }, [sessionId, profile]);

  // A new session starts with an empty view, never the last one's list or review.
  useEffect(() => {
    setList(null);
    setReview(null);
    previewSequence.current++;
    load();
  }, [load]);

  const askPreview = useCallback(
    (row: CheckpointRow, mode: RestoreMode) => {
      const sequence = ++previewSequence.current;
      setReview({row, mode, preview: {state: 'loading'}, isApplying: false, applyError: null, done: null});
      restoreSessionCheckpoint(sessionId, row.id, {dryRun: true, mode}, profile)
        .then(plan => {
          if (sequence === previewSequence.current) {
            setReview(current =>
              current === null ? current : {...current, preview: {state: 'ready', plan}},
            );
          }
        })
        .catch((error: unknown) => {
          if (sequence === previewSequence.current) {
            setReview(current =>
              current === null ? current : {...current, preview: {state: 'error', message: messageOf(error)}},
            );
          }
        });
    },
    [sessionId, profile],
  );

  const openReview = (row: CheckpointRow): void => {
    askPreview(row, restoreChoice(row).defaultMode);
  };

  const closeReview = (): void => {
    previewSequence.current++;
    setReview(null);
  };

  const apply = (): void => {
    if (review === null || review.preview.state !== 'ready' || review.isApplying) {
      return;
    }
    const {row, mode} = review;
    previewSequence.current++;
    setReview({...review, isApplying: true, applyError: null});
    restoreSessionCheckpoint(sessionId, row.id, {dryRun: false, mode}, profile)
      .then(plan => {
        setReview(current => (current === null ? current : {...current, isApplying: false, done: plan}));
        load();
      })
      .catch((error: unknown) => {
        setReview(current =>
          current === null ? current : {...current, isApplying: false, applyError: messageOf(error)},
        );
      });
  };

  const renderReview = (current: Review) => {
    const choice = restoreChoice(current.row);
    if (current.done !== null) {
      const plan = current.done;
      return (
        <VStack gap={3} width="100%">
          <Text type="label">{planSummary(plan)}</Text>
          {plan.undoCheckpointId === undefined ? null : (
            <Text type="supporting" color="secondary">
              The workspace before this restore was saved. Find it in the list as “Before a restore” to undo it.
            </Text>
          )}
          <PathGroup title="Restored" paths={plan.restored} limit={pathsPreviewLimit} maxHeightPx={pathsMaxHeightPx} />
          <PathGroup title="Deleted" paths={plan.deleted} limit={pathsPreviewLimit} maxHeightPx={pathsMaxHeightPx} />
          <PathGroup title="Left alone" paths={plan.conflicts} limit={pathsPreviewLimit} maxHeightPx={pathsMaxHeightPx} />
          <HStack>
            <Button label="Back to list" variant="secondary" size="sm" onClick={closeReview} />
          </HStack>
        </VStack>
      );
    }
    const preview = current.preview;
    return (
      <VStack gap={3} width="100%">
        <VStack gap={1} width="100%">
          <Text type="label">{`Restore ${checkpointTitle(current.row)}`}</Text>
          <Text type="supporting" color="secondary">
            {`Taken ${checkpointTime(current.row.createdAt)}. Nothing changes until you confirm.`}
          </Text>
        </VStack>
        {choice.modes.length > 1 ? (
          <VStack gap={1} width="100%">
            <SegmentedControl
              label="Restore mode"
              size="sm"
              value={current.mode}
              onChange={value => askPreview(current.row, value as RestoreMode)}
            >
              <SegmentedControlItem value="safe" label="Safe" />
              <SegmentedControlItem value="full" label="Full" />
            </SegmentedControl>
            <Text type="supporting" color="secondary">
              {current.mode === 'safe'
                ? 'Safe reverts only what the run changed and leaves files you changed since alone.'
                : FULL_RESTORE_WARNING}
            </Text>
          </VStack>
        ) : (
          <Text type="supporting" color="secondary">
            {`Only a full restore is possible here: no snapshot was taken when the run ended. ${FULL_RESTORE_WARNING}`}
          </Text>
        )}
        {preview.state === 'loading' ? (
          <Text type="supporting" color="secondary">
            Checking what would change…
          </Text>
        ) : null}
        {preview.state === 'error' ? (
          <Text type="supporting" color="secondary">
            {preview.message}
          </Text>
        ) : null}
        {preview.state === 'ready' ? (
          <VStack gap={2} width="100%">
            <Text type="label">{planSummary(preview.plan)}</Text>
            <PathGroup title="Will be restored" paths={preview.plan.restored} limit={pathsPreviewLimit} maxHeightPx={pathsMaxHeightPx} />
            <PathGroup title="Will be deleted" paths={preview.plan.deleted} limit={pathsPreviewLimit} maxHeightPx={pathsMaxHeightPx} />
            <PathGroup title="Changed since the run, left alone" paths={preview.plan.conflicts} limit={pathsPreviewLimit} maxHeightPx={pathsMaxHeightPx} />
          </VStack>
        ) : null}
        {current.applyError === null ? null : (
          <Text type="supporting" color="secondary">
            {current.applyError}
          </Text>
        )}
        {isRunning ? (
          <Text type="supporting" color="secondary">
            A run is active. Restore is available when it ends.
          </Text>
        ) : null}
        <Text type="supporting" color="secondary">
          {CHECKPOINT_NOT_COVERED}
        </Text>
        <HStack align="center" gap={1}>
          <Button
            label="Restore"
            variant="destructive"
            size="sm"
            isLoading={current.isApplying}
            isDisabled={
              isRunning ||
              current.isApplying ||
              preview.state !== 'ready' ||
              !planChangesFiles(preview.plan)
            }
            onClick={apply}
          />
          <Button label="Cancel" variant="ghost" size="sm" isDisabled={current.isApplying} onClick={closeReview} />
        </HStack>
      </VStack>
    );
  };

  const renderList = (value: CheckpointList) => {
    if (!value.enabled) {
      return (
        <EmptyState
          title="Checkpoints are off"
          description="Set checkpoint.enabled to true in this session's setting to snapshot the working directory before a run changes it."
          icon={<Icon icon="info" size="sm" />}
          isCompact
        />
      );
    }
    if (value.checkpoints.length === 0) {
      return (
        <EmptyState
          title="No checkpoints yet"
          description="A checkpoint is taken before the first run that changes files."
          icon={<Icon icon="clock" size="sm" />}
          isCompact
        />
      );
    }
    return (
      <VStack gap={3} width="100%">
        {isRunning ? (
          <Text type="supporting" color="secondary">
            A run is active. Restore is available when it ends.
          </Text>
        ) : null}
        <List header="Checkpoints" density="compact" hasDividers>
          {value.checkpoints.map(row => {
            const choice = restoreChoice(row);
            return (
              <ListItem
                key={row.id}
                label={checkpointTitle(row)}
                description={
                  <VStack gap={1}>
                    <Text type="supporting">{checkpointDescription(row)}</Text>
                    <Text type="supporting" color="secondary">
                      {checkpointTime(row.createdAt)}
                    </Text>
                    {row.warning === undefined ? null : (
                      <Text type="supporting" color="secondary">
                        {row.warning}
                      </Text>
                    )}
                  </VStack>
                }
                startContent={<StatusDot variant={stateVariant(row)} label={checkpointDescription(row)} />}
                endContent={
                  <Button
                    label="Restore…"
                    variant="ghost"
                    size="sm"
                    isDisabled={isRunning || !choice.canRestore}
                    onClick={() => openReview(row)}
                  />
                }
              />
            );
          })}
        </List>
        <Text type="supporting" color="secondary">
          {CHECKPOINT_NOT_COVERED}
        </Text>
      </VStack>
    );
  };

  return (
    <FloatingPanel
      side="end"
      label="Checkpoints"
      title="Checkpoints"
      subtitle="Undo what the agent changed"
      region={region}
      onClose={onClose}
    >
      <VStack gap={3} width="100%">
        {review === null ? (
          <HStack align="center" justify="between" gap={2}>
            <Text type="supporting" color="secondary">
              {list === null || !list.enabled ? '' : `${list.checkpoints.length} ${list.checkpoints.length === 1 ? 'checkpoint' : 'checkpoints'}`}
            </Text>
            <Button label="Refresh" variant="ghost" size="sm" isLoading={isLoading} onClick={load} />
          </HStack>
        ) : null}
        {loadError === null ? null : (
          <Text type="supporting" color="secondary">
            {loadError}
          </Text>
        )}
        {review !== null ? renderReview(review) : list !== null ? renderList(list) : null}
      </VStack>
    </FloatingPanel>
  );
}
