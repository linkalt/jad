import {useState} from 'react';
import {HStack, Text, TextInput, VStack} from '@astryxdesign/core';
import {Selector} from '@astryxdesign/core/Selector';
import {ChatComposerDrawer} from '@astryxdesign/core/Chat';
import {Icon} from '@astryxdesign/core/Icon';
import type {ModelOption} from '../app-types';
import {modelChoiceRows} from '../model-selection';

interface ModelDrawerProps {
  sessionName?: string;
  options: readonly ModelOption[];
  selectedModelId?: string;
  selectedEffort?: string;
  isDisabled?: boolean;
  onChange: (choice: {modelId: string; effort?: string}) => void;
}

/**
 * Column shares of the drawer width. Every row uses the same three widths, so a
 * column starts at the same place in every row — a list of ragged `A|B|C` cells
 * would drift with the length of each id. They add up to less than the full
 * width because the gaps and the trailing chevron take the rest.
 */
const ID_COLUMN_WIDTH = '46%';
const PROVIDER_COLUMN_WIDTH = '20%';
const EFFORT_COLUMN_WIDTH = '30%';

/**
 * The box around the running model's id. It is a background tint, not a text
 * colour, so the id keeps the drawer's own foreground; the blue surface token
 * carries its light and dark value, so the highlight survives light mode.
 */
const SELECTED_ID_BOX = {
  backgroundColor: 'var(--color-background-blue)',
  borderRadius: 'var(--radius-element)',
};

/** The tint under the row the pointer is on, in the open list only. */
const HOVERED_ROW = {
  backgroundColor: 'var(--color-background-gray)',
  borderRadius: 'var(--radius-element)',
};

/**
 * The composer's model drawer: a compact list of every model the session
 * offers — model-id, provider and a reasoning-effort drop-down per row.
 *
 * It opens on the running model's row alone, with the expand chevron at the end
 * of that row; clicking the row opens the whole list with a search field above
 * it; a row under the pointer is tinted. Clicking any row folds the list back
 * (another row is picked first), while an effort drop-down changes in place.
 * The running model is the one whose id sits in a highlighted box.
 *
 * Rows are stacks rather than an Astryx Table: a table draws a divider per row
 * and re-bases the theme's colour tokens inside every cell, which would flatten
 * the highlight. The shared column widths above give the alignment a table would.
 */
export function ModelDrawer({
  sessionName = 'Current session',
  options,
  selectedModelId,
  selectedEffort,
  isDisabled = false,
  onChange,
}: ModelDrawerProps) {
  const [isExpanded, setIsExpanded] = useState(false);
  const [query, setQuery] = useState('');
  const [hoveredId, setHoveredId] = useState<string | null>(null);

  const rows = modelChoiceRows(options, selectedModelId, selectedEffort, query, isExpanded);
  const trimmedQuery = query.trim();
  const isSearching = isExpanded && rows.length === 0;

  const choose = (option: ModelOption, effort?: string) => {
    onChange(effort === undefined ? {modelId: option.id} : {modelId: option.id, effort});
  };

  /** What a click on a row does: open the list, collapse it from the running row, or pick that model in place. */
  const rowAction = (option: ModelOption, effort: string | undefined, isRunning: boolean) => () => {
    if (isDisabled) return;
    if (!isExpanded) {
      setIsExpanded(true);
    } else {
      if (!isRunning) choose(option, effort);
      setIsExpanded(false);
      setQuery('');
    }
  };

  return (
    <ChatComposerDrawer>
      {/*
        The drawer body lays its children out in a row and leaves each at its
        content width, so the content claims the full width explicitly.
      */}
      <VStack gap={2} width="100%" onClick={event => event.stopPropagation()}>
        {isExpanded && (
          <TextInput
            label="Search models"
            isLabelHidden
            placeholder={`Search models in ${sessionName}`}
            startIcon="search"
            hasClear
            size="sm"
            width="100%"
            value={query}
            onChange={setQuery}
          />
        )}
        {isSearching ? (
          <Text type="supporting" color="secondary" role="status" aria-live="polite">
            No models match “{trimmedQuery}”.
          </Text>
        ) : (
          <VStack gap={1} width="100%">
            {rows.map(row => {
              const efforts = row.option.efforts ?? [];
              return (
                <HStack
                  key={row.option.id}
                  gap={2}
                  align="center"
                  width="100%"
                  data-model-row={row.isSelected ? 'selected' : 'choice'}
                  paddingInline={1}
                  paddingBlock={0.5}
                  style={isExpanded && !isDisabled && hoveredId === row.option.id ? HOVERED_ROW : undefined}
                  onMouseEnter={() => setHoveredId(row.option.id)}
                  onMouseLeave={() => setHoveredId(null)}
                  tabIndex={isDisabled ? -1 : 0}
                  onClick={rowAction(row.option, row.markedEffort, row.isSelected)}
                  onKeyDown={event => {
                    if (event.key !== 'Enter' && event.key !== ' ') return;
                    event.preventDefault();
                    rowAction(row.option, row.markedEffort, row.isSelected)();
                  }}
                >
                  <HStack align="center" width={ID_COLUMN_WIDTH}>
                    <HStack
                      align="center"
                      paddingInline={1}
                      paddingBlock={0.5}
                      style={row.isSelected ? SELECTED_ID_BOX : undefined}
                    >
                      <Text type="label" maxLines={1}>{row.option.id}</Text>
                    </HStack>
                  </HStack>
                  <HStack align="center" width={PROVIDER_COLUMN_WIDTH}>
                    <Text
                      type="label"
                      color={row.isSelected ? undefined : 'secondary'}
                      weight={row.isSelected ? 'semibold' : undefined}
                      maxLines={1}
                    >
                      {row.option.provider ?? '—'}
                    </Text>
                  </HStack>
                  {/* The drop-down is its own control: its clicks must not reach the row. */}
                  <HStack
                    align="center"
                    width={EFFORT_COLUMN_WIDTH}
                    onClick={event => event.stopPropagation()}
                  >
                    {efforts.length === 0 ? (
                      <Text type="supporting" color="secondary">
                        Not configurable
                      </Text>
                    ) : (
                      <Selector
                        label={`Reasoning effort for ${row.option.id}`}
                        isLabelHidden
                        size="sm"
                        options={efforts}
                        {...(row.markedEffort === undefined ? {} : {value: row.markedEffort})}
                        isDisabled={isDisabled}
                        onChange={effort => choose(row.option, effort)}
                      />
                    )}
                  </HStack>
                  {/* The way into the list sits at the end of the row, not beside the id. */}
                  {isExpanded ? null : <Icon icon="chevronDown" size="sm" />}
                </HStack>
              );
            })}
          </VStack>
        )}
      </VStack>
    </ChatComposerDrawer>
  );
}
