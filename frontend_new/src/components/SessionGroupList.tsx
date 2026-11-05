import {HStack, Icon, SideNavItem, Spinner, StatusDot, Text, VisuallyHidden} from '@astryxdesign/core';
import {Tooltip} from '@astryxdesign/core/Tooltip';
import {colorVars} from '@astryxdesign/core/theme/tokens.stylex';
import * as stylex from '@stylexjs/stylex';
import type {RefObject} from 'react';
import type {SessionRunStatus} from '../api';
import type {SessionUiState} from '../session-run-status';
import {
  groupHasRunningRun,
  isGroupExpanded,
  profileMessageBusyNote,
  profileMessageRunLabel,
  rowHasRunningRun,
  rowProfileMessageCaller,
  rowDot,
  rowUiState,
  sessionLevelDot,
  type DotMark,
  type SessionListGroup,
  type SessionListRow,
  rowTurnCount,
  viewLabel,
} from '../session-views';
import {RowContextMenu, type RowMenuTarget} from './RowContextMenu';
import {sessionDotXstyle} from './session-dot';

const styles = stylex.create({
  /**
   * A row whose profile another profile's `profile_message` call is holding.
   *
   * The same warm wash the conversation pane's header strip wears, so the row
   * and the pane that belongs to it agree on which profile is busy.
   * `rowProfileMessageCaller` decides it, so the tint, the spinner and the
   * warning mark beside it can never disagree.
   */
  rowProfileBusy: {
    backgroundColor: colorVars['--color-warning-muted'],
  },
});

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
  /** Duplicates a session or profile row (the row menu's Duplicate). */
  onDuplicate?: (target: RowMenuTarget) => void;
  /** Whether a row can be duplicated (the row menu hides Duplicate otherwise). */
  canDuplicate?: (target: RowMenuTarget) => boolean;
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
  onDuplicate,
  canDuplicate,
  onRowContextMenu,
  rowMenuTarget,
  rowMenuAnchorRef,
  onRowMenuOpenChange,
}: SessionGroupListProps) {
  const renderRow = (
    row: SessionListRow,
    session: SessionGroupSession,
    isChild: boolean,
    isRunning: boolean = rowHasRunningRun(row, session, runningViewKeys),
  ) => {
    const state = rowUiState(row, session, isRunning);
    // A session with no profile rows draws this same row as its own label, and a
    // session label wears the session fill: its dot says "session", not a run
    // state its rows already report.
    const dot: DotMark = isChild ? rowDot(state) : sessionLevelDot(state);
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
        xstyle={caller === undefined ? undefined : styles.rowProfileBusy}
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
            <StatusDot
              variant={dot.variant}
              label={`${fullLabel} ${dot.word}`}
              xstyle={sessionDotXstyle(dot)}
            />
          )
        }
        endContent={
          caller === undefined ? (
            turns === undefined ? undefined : (
              <Text type="supporting" hasTabularNumbers>
                {turns}
                <VisuallyHidden> turns</VisuallyHidden>
              </Text>
            )
          ) : (
            /* The row's busy mark, matching the pane header's: same icon, same
               colour, same tooltip text. A tooltip-wrapped icon stays passive
               (Tooltip only attaches described-by and hover events; it adds no
               focus stop), so it belongs in `endContent` rather than `actions`. */
            <HStack align="center" gap={1}>
              <Tooltip content={profileMessageBusyNote(caller)} placement="end">
                <Icon icon="warning" color="warning" size="sm" label={profileMessageBusyNote(caller)} />
              </Tooltip>
              {turns === undefined ? null : (
                <Text type="supporting" hasTabularNumbers>
                  {turns}
                  <VisuallyHidden> turns</VisuallyHidden>
                </Text>
              )}
            </HStack>
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
        const dot = sessionLevelDot(state);
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
                <StatusDot
                  variant={dot.variant}
                  label={`${group.title} ${dot.word}`}
                  xstyle={sessionDotXstyle(dot)}
                />
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
                onDuplicate={onDuplicate}
                canDuplicate={canDuplicate}
              />
            )}
          </SideNavItem>
        );
      })}
    </>
  );
}
