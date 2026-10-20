import {SideNavItem, Spinner, StatusDot, Text, VisuallyHidden} from '@astryxdesign/core';
import {Tooltip} from '@astryxdesign/core/Tooltip';
import type {RefObject} from 'react';
import type {SessionRunStatus} from '../api';
import {sessionUiStateForRun, type SessionUiState} from '../session-run-status';
import {
  groupHasRunningRun,
  isGroupExpanded,
  profileMessageRunLabel,
  rowHasRunningRun,
  rowProfileMessageCaller,
  rowRunStatus,
  type SessionListGroup,
  type SessionListRow,
  rowTurnCount,
  viewLabel,
} from '../session-views';
import {RowContextMenu, type RowMenuTarget} from './RowContextMenu';

type SessionGroupSession = {
  id: string;
  title: string;
  turns: number;
  profileTurns?: Readonly<Record<string, number>>;
  state: SessionUiState;
  activeProfile?: string;
  run?: {status?: SessionRunStatus; source?: string; caller?: string};
  runsByProfile?: Record<string, {status?: SessionRunStatus; source?: string; caller?: string}>;
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
  /**
   * Opens a view in a pane of its own instead of replacing the focused one.
   * A modifier-click and the row's own right-click menu ask for this.
   */
  onOpenPane: (viewKey: string) => void;
  /** Whether another pane may be opened at the configured maximum, for a row's menu. */
  canOpenPane: boolean;
  /** How many panes the configuration allows, for a row's menu. */
  paneLimit: number;
  /** Opens the shared right-click menu anchored to the right-clicked row. */
  onRowContextMenu: (target: RowMenuTarget, anchor: HTMLElement) => void;
  /**
   * The row menu that is currently open, when it belongs to a profile row of
   * one of these groups. Each group hosts its own rows' menu so the menu is a
   * child of the group in the DOM: in the collapsed rail the group's rows live
   * inside the hover flyout, and a menu rendered outside it would take the
   * pointer out of the flyout and close it (moving the menu's anchor away).
   */
  rowMenuTarget: RowMenuTarget | null;
  /** The right-clicked row element the menu is anchored to. */
  rowMenuAnchorRef: RefObject<HTMLElement | null>;
  onRowMenuOpenChange: (isOpen: boolean) => void;
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
  onOpenPane,
  canOpenPane,
  paneLimit,
  onRowContextMenu,
  rowMenuTarget,
  rowMenuAnchorRef,
  onRowMenuOpenChange,
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
    const caller = rowProfileMessageCaller(row, session);
    return (
      <SideNavItem
        key={row.key}
        label={row.label}
        isSelected={row.key === activeViewKey}
        onClick={event => {
          // Choosing a row swaps it into the focused pane. A pane of its own is
          // always explicit: a modifier-click here, or the row's own menu.
          if (event.metaKey || event.ctrlKey || event.altKey) {
            onOpenPane(row.key);
            return;
          }
          onSelect(row.key);
        }}
        onContextMenu={event => {
          event.preventDefault();
          event.stopPropagation();
          onRowContextMenu(
            isChild
              ? {kind: 'profile', id: row.key, label: fullLabel, sessionRef: row.sessionRef}
              : {kind: 'session', id: row.sessionRef, label: fullLabel, sessionRef: row.sessionRef},
            event.currentTarget,
          );
        }}
        icon={
          isRunning ? (
            caller === undefined ? (
              <Spinner size="sm" aria-label={`${fullLabel} running`} />
            ) : (
              <Tooltip content={profileMessageRunLabel(caller)} placement="end">
                <Spinner size="sm" aria-label={`${fullLabel}: ${profileMessageRunLabel(caller)}`} />
              </Tooltip>
            )
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
        // A profile row's menu is rendered inside this group, next to the rows
        // it acts on: in the collapsed rail those rows live in the hover flyout,
        // so a menu mounted outside it would sit in a second layer, take the
        // pointer out of the flyout and let it close — and with the flyout gone
        // its row (the menu's anchor) is unmounted, which drops the menu at the
        // viewport edge. Rendered here it is part of the flyout's own subtree.
        const profileTarget =
          rowMenuTarget !== null &&
          rowMenuTarget.kind === 'profile' &&
          rowMenuTarget.sessionRef === group.sessionRef
            ? rowMenuTarget
            : null;
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
                {kind: 'session', id: group.sessionRef, label: group.title, sessionRef: group.sessionRef},
                event.currentTarget,
              );
            }}
            collapsible={{
              isCollapsed: !isGroupExpanded(group, collapsedRefs),
              onCollapsedChange: isCollapsed => onCollapsedChange(group.sessionRef, isCollapsed),
            }}
          >
            {group.children.map(row => renderRow(row, session, true))}
            {profileTarget === null ? null : (
              <RowContextMenu
                target={profileTarget}
                anchorRef={rowMenuAnchorRef}
                isOpen
                onOpenChange={onRowMenuOpenChange}
                onOpenPane={onOpenPane}
                canOpenPane={canOpenPane}
                paneLimit={paneLimit}
              />
            )}
          </SideNavItem>
        );
      })}
    </>
  );
}
