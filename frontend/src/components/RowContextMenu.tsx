import {Icon, IconButton, VStack} from '@astryxdesign/core';
import {Popover} from '@astryxdesign/core/Popover';
import {Tooltip} from '@astryxdesign/core/Tooltip';
import type {RefObject} from 'react';
import {PencilIcon, PlusIcon, TrashIcon} from './AppIcons';
export type RowMenuTarget = {
  /** Which left-panel row the menu was opened for. */
  kind: 'category' | 'session' | 'profile';
  /** The row id (category key, session ref, or row key). */
  id: string;
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
      isOpen={isOpen}
      onOpenChange={onOpenChange}
      content={
        <VStack gap={1}>
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
