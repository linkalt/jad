import {SideNavItem, Spinner, StatusDot, Text, VisuallyHidden} from '@astryxdesign/core';
import type {SessionRunStatus} from '../api';
import {sessionUiStateForRun, type SessionUiState} from '../session-run-status';
import {
  isGroupExpanded,
  rowHasRunningRun,
  rowRunStatus,
  type SessionListGroup,
  type SessionListRow,
  rowTurnCount,
  viewLabel,
} from '../session-views';

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
}: SessionGroupListProps) {
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
    const isRunning = rowHasRunningRun(row, session, runningViewKeys);
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
            icon={
              hasRunning ? (
                <Spinner size="sm" aria-label={`${group.title} running`} />
              ) : (
                <StatusDot variant={state} label={`${group.title} ${state}`} />
              )
            }
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
