import {Divider, HStack, Icon, IconButton, SideNav, SideNavItem, Spinner, VStack, VisuallyHidden} from '@astryxdesign/core';
import {useAppShellMobile} from '@astryxdesign/core/AppShell';
import {Popover} from '@astryxdesign/core/Popover';
import {Tooltip} from '@astryxdesign/core/Tooltip';
import {Fragment, useCallback, useMemo, useRef, useState} from 'react';
import {type Session} from '../app-types';
import {type ColorScheme} from '../color-scheme';
import appConfig from '../config.json';
import {type RightPanelId} from '../right-panel';
import {type SessionListGroup, groupByCategory, groupHasRunningRun} from '../session-views';
import {NotificationBellIcon} from './AppIcons';
import {MoonIcon, SunIcon} from './ColorSchemeIcon';
import {FolderIcon} from './FolderIcon';
import {RowContextMenu, type RowMenuTarget} from './RowContextMenu';
import {SessionGroupList} from './SessionGroupList';

/** Datasets the floating left panel can show. One slot, so only one at a time. */
export type LeftPanelId = 'search' | 'settings';

export function SessionNavigation({
  groups,
  collapsedRefs,
  onCollapsedChange,
  rowTurns,
  sessions,
  runningViewKeys,
  activeViewKey,
  onSelect,
  onOpenProfile,
  openLeftPanel,
  onSelectLeftPanel,
  rightPanel,
  unreadCount,
  hasUnreadError,
  onToggleNotifications,
  onToggleCheckpoints,
  onShowReadme,
  isReadmeShown,
  onOpenProfileEditor,
  colorScheme,
  onToggleColorScheme,
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
  onOpenProfile: (sessionRef: string, profile: string) => void;
  openLeftPanel: LeftPanelId | null;
  onSelectLeftPanel: (panel: LeftPanelId) => void;
  rightPanel: RightPanelId | null;
  unreadCount: number;
  hasUnreadError: boolean;
  onToggleNotifications: () => void;
  onToggleCheckpoints: () => void;
  onShowReadme: () => void;
  isReadmeShown: boolean;
  onOpenProfileEditor: () => void;
  colorScheme: ColorScheme;
  onToggleColorScheme: () => void;
}) {
  const categorySections = useMemo(() => groupByCategory(groups), [groups]);
  const [isCollapsed, setIsCollapsed] = useState(false);
  const [isMoreOpen, setIsMoreOpen] = useState(false);
  const {isMobile} = useAppShellMobile();
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
      footerIcons={
        <FooterStack align="center" justify="between" gap={isCollapsed ? 1 : 0} width="100%">
          <Tooltip content="Search sessions" placement={footerTooltipPlacement}>
            <IconButton
              data-panel-toggle=""
              label="Search sessions"
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
                  : rightPanel === 'notifications' ? 'secondary' : 'ghost'
              }
              size="sm"
              onClick={onToggleNotifications}
            />
          </Tooltip>
          <Tooltip content="Checkpoints" placement={footerTooltipPlacement}>
            <IconButton
              data-panel-toggle=""
              label="Checkpoints"
              icon={<Icon icon="clock" size="sm" />}
              variant={rightPanel === 'checkpoints' ? 'secondary' : 'ghost'}
              size="sm"
              onClick={onToggleCheckpoints}
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
        const label = section.category ?? appConfig.session_uncategorized_title;
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
                  onOpenProfile={onOpenProfile}
                  onRowContextMenu={openRowMenu}
                />
              </VStack>
            )}
          </Fragment>
        );
      })}
      <RowContextMenu
        target={rowMenuTarget}
        anchorRef={rowMenuAnchorRef}
        isOpen={rowMenuTarget !== null}
        onOpenChange={handleRowMenuOpenChange}
      />
    </SideNav>
  );
}
