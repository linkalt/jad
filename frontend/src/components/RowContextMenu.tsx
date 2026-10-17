import {Icon, IconButton, VStack} from '@astryxdesign/core';
import {Popover} from '@astryxdesign/core/Popover';
import {Tooltip} from '@astryxdesign/core/Tooltip';
import {useEffect, useRef, type RefObject} from 'react';
import {PencilIcon, PlusIcon, TrashIcon} from './AppIcons';
export type RowMenuTarget = {
  /** Which left-panel row the menu was opened for. */
  kind: 'category' | 'session' | 'profile';
  /** The row id (category key, session ref, or row key). */
  id: string;
  /**
   * The session whose list owns the row. Present for session and profile rows,
   * absent for a category, and it is what lets a group host its own rows' menu.
   */
  sessionRef?: string;
  /** The visible row label, used to name the placeholder actions. */
  label: string;
};

/**
 * Placeholder right-click menu for a category, session or profile row.
 *
 * One shared instance lives in `SessionNavigation`: a row's `onContextMenu`
 * anchors it to that row and it opens beside the row (`placement="end"`,
 * `alignment="start"`, so its top aligns with the row label), showing the
 * same icon-only vertical stack as the bottom More menu — New, Edit,
 * Delete from top to bottom, each with an `end` tooltip. The buttons only
 * dismiss the menu; no row action is wired yet.
 *
 * The menu dismisses itself rather than leaving it to the Popover's light
 * dismiss, which is the native `popover="auto"` one. Firefox counts the mouseup
 * of the right-click that opened the menu as a dismiss press, so there the menu
 * vanished the moment it appeared; Astryx's own ContextMenu dodges that race the
 * same way, with a document `mousedown` listener the opening press has already
 * passed by the time it exists.
 */
export function RowContextMenu({
  target,
  anchorRef,
  isOpen,
  onOpenChange,
}: {
  target: RowMenuTarget | null;
  anchorRef: RefObject<HTMLElement | null>;
  isOpen: boolean;
  onOpenChange: (isOpen: boolean) => void;
}) {
  const menuRef = useRef<HTMLElement | null>(null);
  useEffect(() => {
    if (!isOpen || target === null) {
      return;
    }
    const dismissOnOutsidePress = (event: MouseEvent) => {
      const menu = menuRef.current;
      if (menu !== null && !menu.contains(event.target as Node)) {
        onOpenChange(false);
      }
    };
    document.addEventListener('mousedown', dismissOnOutsidePress);
    return () => document.removeEventListener('mousedown', dismissOnOutsidePress);
  }, [isOpen, onOpenChange, target]);

  // A menu opened from a row of the collapsed rail's hover flyout is rendered
  // inside that flyout, so it is unrendered as soon as the flyout closes while
  // its own state still says open — it would reappear, misplaced, the next time
  // the flyout opens. Close it with the layer that hosts it. The anchor row
  // stays mounted when the flyout closes, so the flyout is still findable here.
  useEffect(() => {
    if (!isOpen || target === null) {
      return;
    }
    const flyout = anchorRef.current?.closest('[popover]');
    if (flyout == null) {
      return;
    }
    const closeWithFlyout = (event: Event) => {
      if ((event as Event & {newState?: string}).newState === 'closed') {
        onOpenChange(false);
      }
    };
    flyout.addEventListener('toggle', closeWithFlyout);
    return () => flyout.removeEventListener('toggle', closeWithFlyout);
  }, [anchorRef, isOpen, onOpenChange, target]);

  if (target === null) {
    return null;
  }
  const dismiss = () => onOpenChange(false);
  return (
    <Popover
      label={`${target.label} actions`}
      // The core package was built against React 18 ref types, so its
      // anchorRef wants RefObject<HTMLElement>; the ref always holds the
      // right-clicked row button while the menu is open.
      anchorRef={anchorRef as unknown as RefObject<HTMLElement>}
      placement="end"
      alignment="start"
      // Hug the icon stack: without an explicit width the surface matches the
      // wide row trigger, while max-content shrinks it to the icons.
      width="max-content"
      padding={1}
      hasAutoFocus={false}
      hasLightDismiss={false}
      isOpen={isOpen}
      onOpenChange={onOpenChange}
      content={
        <VStack ref={menuRef} gap={1}>
          <Tooltip content="New" placement="end">
            <IconButton
              label={`New ${target.label}`}
              icon={<Icon icon={PlusIcon} size="sm" />}
              variant="ghost"
              size="sm"
              onClick={dismiss}
            />
          </Tooltip>
          <Tooltip content="Edit" placement="end">
            <IconButton
              label={`Edit ${target.label}`}
              icon={<Icon icon={PencilIcon} size="sm" />}
              variant="ghost"
              size="sm"
              onClick={dismiss}
            />
          </Tooltip>
          <Tooltip content="Delete" placement="end">
            <IconButton
              label={`Delete ${target.label}`}
              icon={<Icon icon={TrashIcon} size="sm" />}
              variant="destructive"
              size="sm"
              onClick={dismiss}
            />
          </Tooltip>
        </VStack>
      }
    />
  );
}
