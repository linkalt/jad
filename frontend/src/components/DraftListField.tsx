/**
 * @file DraftListField.tsx
 * @input One array field of a draft and the entries the user has made of it
 * @output A native list editor: one input per entry, with add, remove and reorder
 * @position The array row of the request_draft drawer
 *
 * An array is one field, not one input per index: entries are added, removed and
 * moved instead of retyping brackets and commas. Entries keep the type the
 * proposal gave them; an object or nested array reads as one JSON entry, and an
 * entry the user adds picks its own type.
 */
import {
  Button,
  HStack,
  Icon,
  IconButton,
  SegmentedControl,
  SegmentedControlItem,
  StackItem,
  Text,
  TextArea,
  TextInput,
  VStack,
} from '@astryxdesign/core';

import {
  DRAFT_ADDED_FIELD_KINDS,
  addListItem,
  editListItem,
  moveListItem,
  removeListItem,
  retypeListItem,
  type DraftAddedFieldKind,
  type DraftListItem,
} from '../draft-answer';

export interface DraftListFieldProps {
  label: string;
  items: readonly DraftListItem[];
  isDisabled: boolean;
  /** Hands out the id of an entry the user adds; ids stay unique across the drawer. */
  nextItemId: () => string;
  onChange: (items: DraftListItem[]) => void;
}

export function DraftListField({
  label,
  items,
  isDisabled,
  nextItemId,
  onChange,
}: DraftListFieldProps) {
  return (
    <VStack gap={1} width="100%" role="group" aria-label={label}>
      <Text type="label">{label}</Text>
      {items.length === 0 ? (
        <Text type="supporting" color="secondary">
          No items yet.
        </Text>
      ) : null}
      {items.map((item, index) => {
        const itemLabel = `${label} item ${index + 1}`;
        const onText = (value: string): void => onChange(editListItem(items, index, value));
        return (
          <VStack key={item.id} gap={1} width="100%">
            <HStack gap={1} align="end" width="100%">
              <StackItem size="fill">
                {item.kind === 'json' || item.kind === 'string' ? (
                  <TextArea
                    label={itemLabel}
                    value={item.text}
                    rows={item.kind === 'json' ? 3 : 2}
                    size="sm"
                    isDisabled={isDisabled}
                    onChange={onText}
                  />
                ) : (
                  <TextInput
                    label={itemLabel}
                    value={item.text}
                    size="sm"
                    isDisabled={isDisabled}
                    onChange={onText}
                  />
                )}
              </StackItem>
              <IconButton
                label={`Move ${itemLabel} up`}
                icon={<Icon icon="arrowUp" size="sm" />}
                variant="ghost"
                size="sm"
                isDisabled={isDisabled || index === 0}
                onClick={() => onChange(moveListItem(items, index, -1))}
              />
              <IconButton
                label={`Move ${itemLabel} down`}
                icon={<Icon icon="arrowDown" size="sm" />}
                variant="ghost"
                size="sm"
                isDisabled={isDisabled || index === items.length - 1}
                onClick={() => onChange(moveListItem(items, index, 1))}
              />
              <IconButton
                label={`Remove ${itemLabel}`}
                icon={<Icon icon="close" size="sm" />}
                variant="ghost"
                size="sm"
                isDisabled={isDisabled}
                onClick={() => onChange(removeListItem(items, index))}
              />
            </HStack>
            {item.added === true ? (
              <SegmentedControl
                label={`${itemLabel} type`}
                size="sm"
                value={item.kind}
                onChange={(value) =>
                  onChange(retypeListItem(items, index, value as DraftAddedFieldKind))
                }
              >
                {DRAFT_ADDED_FIELD_KINDS.map((kind) => (
                  <SegmentedControlItem key={kind.id} value={kind.id} label={kind.label} />
                ))}
              </SegmentedControl>
            ) : null}
          </VStack>
        );
      })}
      <HStack>
        <Button
          label={`Add item to ${label}`}
          variant="ghost"
          size="sm"
          isDisabled={isDisabled}
          onClick={() => onChange(addListItem(items, nextItemId()))}
        />
      </HStack>
    </VStack>
  );
}
