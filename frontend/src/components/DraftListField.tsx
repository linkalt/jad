/**
 * @file DraftListField.tsx
 * @input One array field of a draft and the entries the user has made of it
 * @output A native list editor, and the row form both it and the drawer render
 * @position The array row of the request_draft drawer
 *
 * An array is one field, not one input per index: entries are added, removed and
 * moved instead of retyping brackets and commas. A scalar entry is one input. An
 * object entry is a small form of its own, one input per key, so nothing has to be
 * written as JSON; that form is the same row renderer the drawer uses for the
 * root draft, so a key that is itself an array of objects nests the same way.
 * Only an entry the form cannot express (an empty object, a nested array) stays
 * one JSON input, and an entry the user adds picks its own type.
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
  draftFields,
  editListItem,
  listItemsOf,
  moveListItem,
  removeListItem,
  retypeListItem,
  updateListItemForm,
  type DraftAddedFieldKind,
  type DraftListEdits,
  type DraftListItem,
} from '../draft-answer';

export interface DraftFieldRowsProps {
  /** The object whose keys become rows. */
  base: unknown;
  edits: Readonly<Record<string, string>>;
  lists: DraftListEdits;
  /** Put before every row's label, so rows of different entries stay distinguishable. */
  labelPrefix?: string;
  isDisabled: boolean;
  /** Hands out the id of an entry the user adds; ids stay unique across the drawer. */
  nextItemId: () => string;
  onEdit: (key: string, text: string) => void;
  onList: (key: string, items: DraftListItem[]) => void;
}

/** One input per scalar and one list editor per array of `base`, in the proposal's order. */
export function DraftFieldRows({
  base,
  edits,
  lists,
  labelPrefix = '',
  isDisabled,
  nextItemId,
  onEdit,
  onList,
}: DraftFieldRowsProps) {
  return (
    <>
      {draftFields(base).map((field) => {
        const label = `${labelPrefix}${field.label}`;
        if (field.kind === 'list') {
          return (
            <DraftListField
              key={field.key}
              label={label}
              items={listItemsOf(field, lists)}
              isDisabled={isDisabled}
              nextItemId={nextItemId}
              onChange={(items) => onList(field.key, items)}
            />
          );
        }
        const text = edits[field.key] ?? field.text;
        return field.kind === 'json' || field.kind === 'string' ? (
          <TextArea
            key={field.key}
            label={label}
            value={text}
            rows={field.kind === 'json' ? 4 : 3}
            size="sm"
            isDisabled={isDisabled}
            onChange={(value: string) => onEdit(field.key, value)}
          />
        ) : (
          <TextInput
            key={field.key}
            label={label}
            value={text}
            size="sm"
            isDisabled={isDisabled}
            onChange={(value: string) => onEdit(field.key, value)}
          />
        );
      })}
    </>
  );
}

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
        const controls = (
          <>
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
          </>
        );
        if (item.kind === 'object' && item.form !== undefined) {
          const form = item.form;
          return (
            <VStack
              key={item.id}
              gap={1}
              width="100%"
              role="group"
              aria-label={itemLabel}
              paddingInlineStart={2}
            >
              <HStack gap={1} align="center" width="100%">
                <StackItem size="fill">
                  <Text type="label" color="secondary">
                    {`Item ${index + 1}`}
                  </Text>
                </StackItem>
                {controls}
              </HStack>
              <DraftFieldRows
                base={form.base}
                edits={form.edits}
                lists={form.lists}
                labelPrefix={`${itemLabel} `}
                isDisabled={isDisabled}
                nextItemId={nextItemId}
                onEdit={(key, text) =>
                  onChange(updateListItemForm(items, index, {edits: {...form.edits, [key]: text}}))
                }
                onList={(key, next) =>
                  onChange(updateListItemForm(items, index, {lists: {...form.lists, [key]: next}}))
                }
              />
            </VStack>
          );
        }
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
              {controls}
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
