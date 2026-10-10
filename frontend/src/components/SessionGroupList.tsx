import {useState} from 'react';
import * as stylex from '@stylexjs/stylex';
import {Badge, Icon, IconButton, SideNavItem, StatusDot} from '@astryxdesign/core';
import {ContextMenu} from '@astryxdesign/core/ContextMenu';
import type {SessionRunStatus} from '../api';
import {sessionUiStateForRun, type SessionUiState} from '../session-run-status';
import {
  isGroupExpanded,
  rowHasRunningRun,
  rowRunStatus,
  type SessionListGroup,
  type SessionListRow,
  viewLabel,
} from '../session-views';

const styles = stylex.create({
  // The remove control stays mounted so the row's shape never changes under the
  // pointer; only its own display toggles.
  rowActionHidden: {
    display: 'none',
  },
});

type SessionGroupSession = {
  id: string;
  title: string;
  turns: number;
  state: SessionUiState;
  activeProfile?: string;
  run?: {status?: SessionRunStatus};
  runsByProfile?: Record<string, {status?: SessionRunStatus}>;
};

export type SessionGroupListProps = {
  groups: SessionListGroup[];
  sessions: ReadonlyArray<SessionGroupSession>;
  rowTurns: Readonly<Record<string, number>>;
  runningViewKeys: ReadonlySet<string>;
  activeViewKey: string;
  collapsedRefs: readonly string[];
  canPinProfile: boolean;
  onCollapsedChange: (sessionRef: string, isCollapsed: boolean) => void;
  onSelect: (viewKey: string) => void;
  onOpenProfile: (sessionRef: string, profile: string) => void;
  onRemoveView: (viewKey: string) => void;
};

/** The children of a `SideNavSection`: one collapsible group per session, one row per profile. */
export function SessionGroupList({
  groups,
  sessions,
  rowTurns,
  runningViewKeys,
  activeViewKey,
  collapsedRefs,
  canPinProfile,
  onCollapsedChange,
  onSelect,
  onOpenProfile,
  onRemoveView,
}: SessionGroupListProps) {
  // Hover is tracked on the row itself (the context-menu trigger), not on the
  // row button: the remove control sits beside that button.
  const [hoveredViewKey, setHoveredViewKey] = useState<string | null>(null);

  const rowState = (row: SessionListRow, session: SessionGroupSession): SessionUiState => {
    if (rowHasRunningRun(row, session, runningViewKeys)) {
      return 'accent';
    }
    if (row.profile === undefined) {
      return session.state;
    }
    return sessionUiStateForRun(rowRunStatus(row, session)?.status);
  };

  const renderRow = (row: SessionListRow, session: SessionGroupSession, isChild: boolean) => {
    const state = rowState(row, session);
    // The visible label of a child is the profile; its accessible names carry the
    // session too, because the same profile name repeats in every group.
    const fullLabel = isChild ? viewLabel(session.title, row.label) : row.label;
    const isUnpinnedProfile = row.profile !== undefined && !row.isPinned;
    const isPinnedProfile = row.profile !== undefined && row.isPinned;
    const item = (
      <SideNavItem
        label={row.label}
        isSelected={row.key === activeViewKey}
        isDisabled={isUnpinnedProfile && !canPinProfile}
        onClick={() =>
          row.profile !== undefined && !row.isPinned
            ? onOpenProfile(row.sessionRef, row.profile)
            : onSelect(row.key)
        }
        icon={<StatusDot variant={state} label={`${fullLabel} ${state}`} />}
        actions={
          isPinnedProfile ? (
            <IconButton
              label={`Remove ${fullLabel}`}
              icon={<Icon icon="close" size="sm" />}
              variant="ghost"
              size="sm"
              xstyle={hoveredViewKey === row.key ? undefined : styles.rowActionHidden}
              onClick={() => onRemoveView(row.key)}
            />
          ) : undefined
        }
        endContent={
          row.profile === undefined ? (
            <Badge label={String(rowTurns[row.key] ?? session.turns)} />
          ) : undefined
        }
      />
    );
    // Every child row sits in the same wrapper, so pinning a profile does not
    // swap the element type: React keeps the row (and its keyboard focus).
    // The menu only has an entry on a pinned profile; elsewhere the browser's
    // own menu shows.
    return (
      <ContextMenu
        key={row.key}
        label={`${fullLabel} actions`}
        isDisabled={!isPinnedProfile}
        items={[
          {
            id: `remove-${row.key}`,
            label: `Remove ${fullLabel}`,
            onClick: () => onRemoveView(row.key),
          },
        ]}
        onMouseEnter={() => setHoveredViewKey(row.key)}
        onMouseLeave={() => setHoveredViewKey(current => (current === row.key ? null : current))}
      >
        {item}
      </ContextMenu>
    );
  };

  return (
    <>
      {groups.map(group => {
        const session = sessions.find(candidate => candidate.id === group.sessionRef);
        if (session === undefined) {
          return null;
        }
        if (group.children.length === 0) {
          return renderRow(group.ownRow, session, false);
        }
        const hasRunning = [...group.children, group.ownRow].some(row =>
          rowHasRunningRun(row, session, runningViewKeys),
        );
        const state: SessionUiState = hasRunning ? 'accent' : session.state;
        return (
          <SideNavItem
            key={group.sessionRef}
            label={group.title}
            icon={<StatusDot variant={state} label={`${group.title} ${state}`} />}
            collapsible={{
              isCollapsed: !isGroupExpanded(group, collapsedRefs),
              onCollapsedChange: isCollapsed => onCollapsedChange(group.sessionRef, isCollapsed),
            }}
          >
            {group.children.map(row => renderRow(row, session, true))}
          </SideNavItem>
        );
      })}
    </>
  );
}
