import {SideNavItem, Spinner, StatusDot, Text, VisuallyHidden} from '@astryxdesign/core';
import type {SessionRunStatus} from '../api';
import {sessionUiStateForRun, type SessionUiState} from '../session-run-status';
import {
  groupHasRunningRun,
  isGroupExpanded,
  rowHasRunningRun,
  rowRunStatus,
  type SessionListGroup,
  type SessionListRow,
  rowTurnCount,
  viewLabel,
} from '../session-views';
import type {RowMenuTarget} from './RowContextMenu';

type SessionGroupSession = {
  id: string;
  title: string;
  turns: number;
  profileTurns?: Readonly<Record<string, number>>;
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
  onCollapsedChange: (sessionRef: string, isCollapsed: boolean) => void;
  onSelect: (viewKey: string) => void;
  onOpenProfile: (sessionRef: string, profile: string) => void;
  /** Opens the shared right-click menu anchored to the right-clicked row. */
  onRowContextMenu: (target: RowMenuTarget, anchor: HTMLElement) => void;
};

/** The children of a category `SideNavItem`: one collapsible group per session, one row per profile. */
export function SessionGroupList({
  groups,
  sessions,
  rowTurns,
  runningViewKeys,
  activeViewKey,
  collapsedRefs,
  onCollapsedChange,
  onSelect,
  onOpenProfile,
  onRowContextMenu,
}: SessionGroupListProps) {
  const rowState = (row: SessionListRow, session: SessionGroupSession, isRunning: boolean): SessionUiState => {
    if (isRunning) {
      return 'accent';
    }
    if (row.profile === undefined) {
      return session.state;
    }
    return sessionUiStateForRun(rowRunStatus(row, session)?.status);
  };

  const renderRow = (
    row: SessionListRow,
    session: SessionGroupSession,
    isChild: boolean,
    isRunning: boolean = rowHasRunningRun(row, session, runningViewKeys),
  ) => {
    const state = rowState(row, session, isRunning);
    const turns = rowTurnCount(row, session, rowTurns);
    // The visible label of a child is the profile; its accessible names carry the
    // session too, because the same profile name repeats in every group.
    const fullLabel = isChild ? viewLabel(session.title, row.label) : row.label;
    return (
      <SideNavItem
        key={row.key}
        label={row.label}
        isSelected={row.key === activeViewKey}
        onClick={() =>
          row.profile !== undefined ? onOpenProfile(row.sessionRef, row.profile) : onSelect(row.key)
        }
        onContextMenu={event => {
          event.preventDefault();
          event.stopPropagation();
          onRowContextMenu(
            isChild
              ? {kind: 'profile', id: row.key, label: fullLabel}
              : {kind: 'session', id: row.sessionRef, label: fullLabel},
            event.currentTarget,
          );
        }}
        icon={
          isRunning ? (
            <Spinner size="sm" aria-label={`${fullLabel} running`} />
          ) : (
            <StatusDot variant={state} label={`${fullLabel} ${state}`} />
          )
        }
        endContent={
          turns === undefined ? undefined : (
            <Text type="supporting" hasTabularNumbers>
              {turns}
              <VisuallyHidden> turns</VisuallyHidden>
            </Text>
          )
        }
      />
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
          return renderRow(group.ownRow, session, false, groupHasRunningRun(group, session, runningViewKeys));
        }
        const hasRunning = groupHasRunningRun(group, session, runningViewKeys);
        const state: SessionUiState = hasRunning ? 'accent' : session.state;
        return (
          <SideNavItem
            key={group.sessionRef}
            label={group.title}
            icon={
              hasRunning ? (
                <Spinner size="sm" aria-label={`${group.title} running`} />
              ) : (
                <StatusDot variant={state} label={`${group.title} ${state}`} />
              )
            }
            onContextMenu={event => {
              event.preventDefault();
              event.stopPropagation();
              onRowContextMenu(
                {kind: 'session', id: group.sessionRef, label: group.title},
                event.currentTarget,
              );
            }}
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
