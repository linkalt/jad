import {Button, Divider, HStack, Icon, IconButton, SideNav, SideNavItem, Spinner, VStack, VisuallyHidden} from '@astryxdesign/core';
import {useAppShellMobile} from '@astryxdesign/core/AppShell';
import {useSideNavRenderMode} from '@astryxdesign/core/SideNav';
import {Popover} from '@astryxdesign/core/Popover';
import {Tooltip} from '@astryxdesign/core/Tooltip';
import {Fragment, useCallback, useMemo, useRef, useState} from 'react';
import {type Session} from '../app-types';
import {type ColorScheme} from '../color-scheme';
import appConfig from '../config.json';
import {type LeftPanelId} from '../left-panel';
import {paneCountLabel} from '../pane-count';
import {type SessionListGroup, categorySectionLabel, groupByCategory, groupHasRunningRun} from '../session-views';
import {NotificationBellIcon} from './AppIcons';
import {MoonIcon, SunIcon} from './ColorSchemeIcon';
import {FolderIcon} from './FolderIcon';
import {RowContextMenu, type RowMenuTarget} from './RowContextMenu';
import {SessionGroupList} from './SessionGroupList';

/**
 * Close the hover flyout of a collapsed-rail row, if one is open.
 *
 * In the collapsed rail a session row with profiles shows that profile list as a
 * hover flyout: a popover `SideNavItem` renders as a sibling of the row inside
 * the row's own wrapper. The session's right-click menu opens where the flyout
 * is, so leaving the flyout up stacks the two panels on one spot — and the
 * flyout does not reliably go away by itself, because the panel's own invoker is
 * exempt from its light dismiss. Closing it here makes the row menu the only
 * panel, the way the flyout's own light dismiss would: Astryx syncs a
 * browser-initiated close back into the flyout's state.
 */
function closeHoverFlyout(anchor: HTMLElement): void {
  const flyout = anchor.parentElement?.querySelector<HTMLElement>('[popover]');
  if (flyout?.matches(':popover-open')) {
    flyout.hidePopover();
  }
}

export function SessionNavigation({
  groups,
  collapsedRefs,
  onCollapsedChange,
  rowTurns,
  sessions,
  runningViewKeys,
  activeViewKey,
  onSelect,
  onOpenPane,
  canOpenPane,
  paneLimit,
  onDuplicate,
  canDuplicate,
  paneCount,
  openLeftPanel,
  onSelectLeftPanel,
  unreadCount,
  hasUnreadError,
  onShowReadme,
  isReadmeShown,
  onOpenProfileEditor,
  colorScheme,
  onToggleColorScheme,
  paneViews,
  activePaneIndex,
  onFocusPane,
}: {
  /** Every session as a group, with a child row for each of its profiles. */
  groups: SessionListGroup[];
  /** The sessions whose group the user collapsed. */
  collapsedRefs: readonly string[];
  onCollapsedChange: (sessionRef: string, isCollapsed: boolean) => void;
  /** What each row's own transcript holds, when its snapshot has reported it. */
  rowTurns: Readonly<Record<string, number>>;
  sessions: Session[];
  runningViewKeys: ReadonlySet<string>;
  activeViewKey: string;
  onSelect: (viewKey: string) => void;
  /** Opens a row in a pane of its own: a modifier-click or the row's menu asks for this. */
  onOpenPane: (viewKey: string) => void;
  /** Whether another pane may be opened at the configured maximum. */
  canOpenPane: boolean;
  /** How many panes the configuration allows, for the menu's tooltip. */
  paneLimit: number;
  /** Duplicates a session or profile row (the row menu's Duplicate). */
  onDuplicate?: (target: RowMenuTarget) => void;
  /** Whether a row can be duplicated (the row menu hides Duplicate otherwise). */
  canDuplicate?: (target: RowMenuTarget) => boolean;
  /** How many conversations are on screen right now, for the footer's count. */
  paneCount: number;
  openLeftPanel: LeftPanelId | null;
  onSelectLeftPanel: (panel: LeftPanelId) => void;
  unreadCount: number;
  hasUnreadError: boolean;
  onShowReadme: () => void;
  isReadmeShown: boolean;
  onOpenProfileEditor: () => void;
  colorScheme: ColorScheme;
  onToggleColorScheme: () => void;
  /** The panes on screen, in order: the mobile top bar numbers them. */
  paneViews: readonly {viewKey: string; label: string}[];
  /** Which of `paneViews` holds the focus, or -1 when it holds none. */
  activePaneIndex: number;
  onFocusPane: (viewKey: string) => void;
}) {
  const categorySections = useMemo(() => groupByCategory(groups), [groups]);
  const [isCollapsed, setIsCollapsed] = useState(false);
  const [isMoreOpen, setIsMoreOpen] = useState(false);
  const {isMobile} = useAppShellMobile();
  // The pane numbers belong to the mobile top bar alone: the same SideNav is
  // rendered once more inside the mobile drawer, and a second copy of the
  // switcher there would be a duplicate set of controls.
  const sideNavRenderMode = useSideNavRenderMode();
  const [collapsedCategories, setCollapsedCategories] = useState<readonly string[]>([]);
  const toggleCategory = useCallback((key: string) => {
    setCollapsedCategories(current =>
      current.includes(key) ? current.filter(entry => entry !== key) : [...current, key],
    );
  }, []);
  // The shared right-click menu: a row's onContextMenu anchors it to that row
  // and names the target; it opens beside the row, top-aligned with the label.
  const rowMenuAnchorRef = useRef<HTMLElement | null>(null);
  const [rowMenuTarget, setRowMenuTarget] = useState<RowMenuTarget | null>(null);
  const openRowMenu = useCallback((target: RowMenuTarget, anchor: HTMLElement) => {
    if (target.kind === 'session') {
      closeHoverFlyout(anchor);
    }
    rowMenuAnchorRef.current = anchor;
    setRowMenuTarget(target);
  }, []);
  const handleRowMenuOpenChange = useCallback((isOpen: boolean) => {
    if (!isOpen) {
      setRowMenuTarget(null);
    }
  }, []);
  // The footer icon bar is horizontal when expanded and vertical when the rail
  // is collapsed; its tooltips sit above the bar or to its end respectively.
  const footerTooltipPlacement = isCollapsed ? 'end' : 'above';
  const FooterStack = isCollapsed ? VStack : HStack;
  return (
    <SideNav
      aria-label="Agent sessions"
      header={
        sideNavRenderMode === 'topbar' && paneViews.length > 1 ? (
          // The mobile top bar carries one number per pane: with several panes
          // open on a phone only one fits on screen, so the bar is the tab strip.
          // The pane's own name is left to the conversation's title strip.
          <HStack align="center" gap={1} role="group" aria-label="Conversation panes" paddingBlock={1} paddingInline={2}>
            {paneViews.map((pane, index) => (
              <Button
                key={pane.viewKey}
                label={`Show pane ${index + 1}: ${pane.label}`}
                variant={index === activePaneIndex ? 'secondary' : 'ghost'}
                size="sm"
                onClick={() => onFocusPane(pane.viewKey)}>
                {index + 1}
              </Button>
            ))}
          </HStack>
        ) : undefined
      }
      footerIcons={
        <FooterStack align="center" justify="between" gap={isCollapsed ? 1 : 0} width="100%">
          <Tooltip content="Search profiles" placement={footerTooltipPlacement}>
            <IconButton
              data-panel-toggle=""
              label="Search profiles"
              icon={<Icon icon="search" size="sm" />}
              variant={openLeftPanel === 'search' ? 'secondary' : 'ghost'}
              size="sm"
              onClick={() => onSelectLeftPanel('search')}
            />
          </Tooltip>
          <Tooltip content="Catalog" placement={footerTooltipPlacement}>
            <IconButton
              label="Catalog"
              icon={<Icon icon="wrench" size="sm" />}
              variant="ghost"
              size="sm"
              onClick={onOpenProfileEditor}
            />
          </Tooltip>
          <Tooltip
            content={unreadCount > 0 ? `Notifications (${unreadCount} unread)` : 'Notifications'}
            placement={footerTooltipPlacement}>
            <IconButton
              data-panel-toggle=""
              label={unreadCount > 0 ? `Notifications, ${unreadCount} unread` : 'Notifications'}
              icon={<Icon icon={NotificationBellIcon} size="sm" />}
              variant={
                unreadCount > 0
                  ? hasUnreadError ? 'destructive' : 'primary'
                  : openLeftPanel === 'notifications' ? 'secondary' : 'ghost'
              }
              size="sm"
              onClick={() => onSelectLeftPanel('notifications')}
            />
          </Tooltip>
          <Tooltip content="Checkpoints" placement={footerTooltipPlacement}>
            <IconButton
              data-panel-toggle=""
              label="Checkpoints"
              icon={<Icon icon="clock" size="sm" />}
              variant={openLeftPanel === 'checkpoints' ? 'secondary' : 'ghost'}
              size="sm"
              onClick={() => onSelectLeftPanel('checkpoints')}
            />
          </Tooltip>
          {/* How many conversations share the screen, in the tooltip and the
              accessible name only — the footer keeps one icon per control, and
              the count itself is in the panel it opens. */}
          <Tooltip content={paneCountLabel(paneCount, paneLimit)} placement={footerTooltipPlacement}>
            <IconButton
              data-panel-toggle=""
              label={paneCountLabel(paneCount, paneLimit)}
              icon={<Icon icon="viewColumns" size="sm" />}
              variant={openLeftPanel === 'panes' ? 'secondary' : 'ghost'}
              size="sm"
              onClick={() => onSelectLeftPanel('panes')}
            />
          </Tooltip>
          <Popover
            label="More"
            placement={isMobile ? 'below' : 'end'}
            alignment="end"
            padding={1}
            hasAutoFocus={false}
            isOpen={isMoreOpen}
            onOpenChange={setIsMoreOpen}
            content={
              <VStack gap={1}>
                <Tooltip content="README" placement="end">
                  <IconButton
                    data-panel-toggle=""
                    label="README"
                    icon={<Icon icon="app:readme" size="sm" />}
                    variant={isReadmeShown ? 'secondary' : 'ghost'}
                    size="sm"
                    onClick={() => {
                      setIsMoreOpen(false);
                      onShowReadme();
                    }}
                  />
                </Tooltip>
                <Tooltip content="Run settings" placement="end">
                  <IconButton
                    data-panel-toggle=""
                    label="Run settings"
                    icon={<Icon icon="funnel" size="sm" />}
                    variant={openLeftPanel === 'settings' ? 'secondary' : 'ghost'}
                    size="sm"
                    onClick={() => {
                      setIsMoreOpen(false);
                      onSelectLeftPanel('settings');
                    }}
                  />
                </Tooltip>
                <Tooltip
                  content={colorScheme === 'dark' ? 'Light theme' : 'Dark theme'}
                  placement="end">
                  <IconButton
                    data-panel-toggle=""
                    label={colorScheme === 'dark' ? 'Switch to light theme' : 'Switch to dark theme'}
                    icon={<Icon icon={colorScheme === 'dark' ? SunIcon : MoonIcon} size="sm" />}
                    variant="ghost"
                    size="sm"
                    onClick={onToggleColorScheme}
                  />
                </Tooltip>
              </VStack>
            }>
            <Tooltip content="More" placement={footerTooltipPlacement}>
              <IconButton
                data-panel-toggle=""
                label="More"
                icon={<Icon icon="app:more" size="sm" />}
                variant={isMoreOpen ? 'secondary' : 'ghost'}
                size="sm"
              />
            </Tooltip>
          </Popover>
        </FooterStack>
      }
      collapsible={{isCollapsed, onCollapsedChange: setIsCollapsed}}
    >
      {categorySections.map(section => {
        const key = section.category ?? '';
        const label = categorySectionLabel(section, appConfig.session_uncategorized_title);
        const hasRunning = section.groups.some(group =>
          groupHasRunningRun(group, sessions.find(candidate => candidate.id === group.sessionRef), runningViewKeys),
        );
        return (
          <Fragment key={key}>
            <SideNavItem
              label={label}
              icon={hasRunning ? <Spinner size="sm" aria-label={`${label} running`} /> : FolderIcon}
              endContent={
                <VisuallyHidden>{collapsedCategories.includes(key) ? 'collapsed' : 'expanded'}</VisuallyHidden>
              }
              onClick={() => toggleCategory(key)}
              onContextMenu={event => {
                event.preventDefault();
                event.stopPropagation();
                openRowMenu({kind: 'category', id: key, label}, event.currentTarget);
              }}
            />
            <Divider />
            {collapsedCategories.includes(key) ? null : (
              <VStack gap={0} paddingInlineStart={2}>
                <SessionGroupList
                  groups={section.groups}
                  sessions={sessions}
                  rowTurns={rowTurns}
                  runningViewKeys={runningViewKeys}
                  activeViewKey={activeViewKey}
                  collapsedRefs={collapsedRefs}
                  onCollapsedChange={onCollapsedChange}
                  onSelect={onSelect}
                  onOpenPane={onOpenPane}
                  canOpenPane={canOpenPane}
                  paneLimit={paneLimit}
                  onDuplicate={onDuplicate}
                  canDuplicate={canDuplicate}
                  onRowContextMenu={openRowMenu}
                  rowMenuTarget={rowMenuTarget}
                  rowMenuAnchorRef={rowMenuAnchorRef}
                  onRowMenuOpenChange={handleRowMenuOpenChange}
                />
              </VStack>
            )}
          </Fragment>
        );
      })}
      {/* Profile rows host their own menu inside their group; a category or
          session row's menu lives here, beside the rail it belongs to. */}
      <RowContextMenu
        target={rowMenuTarget?.kind === 'profile' ? null : rowMenuTarget}
        anchorRef={rowMenuAnchorRef}
        isOpen={rowMenuTarget !== null && rowMenuTarget.kind !== 'profile'}
        onOpenChange={handleRowMenuOpenChange}
        onOpenPane={onOpenPane}
        canOpenPane={canOpenPane}
        paneLimit={paneLimit}
        onDuplicate={onDuplicate}
        canDuplicate={canDuplicate}
      />
    </SideNav>
  );
}
