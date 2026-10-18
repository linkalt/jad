import {useState} from 'react';
import {HStack, Text, TextInput, VStack} from '@astryxdesign/core';
import {ChatComposerDrawer} from '@astryxdesign/core/Chat';
import {Icon} from '@astryxdesign/core/Icon';
import {
  profileChoiceRows,
  type ProfileSourceRow,
} from '../profile-rows';

interface ProfileDrawerProps {
  /** Every profile of every session, in the rail's order. */
  rows: readonly ProfileSourceRow[];
  /** The view key the pane is showing now. */
  selectedKey?: string;
  /** The views already on screen: their rows are marked and cannot be picked. */
  openViewKeys?: ReadonlySet<string>;
  isDisabled?: boolean;
  /** The view key of the profile the user picked: the conversation to show instead. */
  onChange: (viewKey: string) => void;
}

/**
 * Column shares of the drawer width, shared by every row so a column starts at
 * the same place in each of them. The remainder is taken by the gaps and the
 * trailing chevron.
 */
const LABEL_COLUMN_WIDTH = '34%';
const PATH_COLUMN_WIDTH = '30%';
const SESSION_COLUMN_WIDTH = '28%';

/**
 * The box around the current profile's label. A background tint rather than a
 * text colour, so the label keeps the drawer's own foreground; the blue surface
 * token carries its light and dark value.
 */
const SELECTED_LABEL_BOX = {
  backgroundColor: 'var(--color-background-blue)',
  borderRadius: 'var(--radius-element)',
};

/** The tint under the collapsed row (always) and under the row the pointer is on in the open list. */
const ROW_TINT = {
  backgroundColor: 'var(--color-background-gray)',
  borderRadius: 'var(--radius-element)',
};

/** A row whose view is already on screen in another pane: readable, not pickable. */
const ALREADY_OPEN_ROW = {
  opacity: 0.55,
  cursor: 'default',
};

/** No pane is showing any of the rows. */
const NO_VIEW_KEYS: ReadonlySet<string> = new Set<string>();

/**
 * The composer's profile drawer: every profile of every session, so a
 * conversation can be pointed at another profile — in this session or any other
 * — without going back to the rail.
 *
 * It follows the model drawer exactly. Closed, it is the current profile's row
 * alone, with the expand chevron at its end; clicking that row opens the whole
 * list with a search field above it; a row under the pointer is tinted; picking
 * a row hands its view key to the caller and folds the list back. The profile
 * the pane shows now is the one whose label sits in a highlighted box.
 *
 * Rows whose view another pane already shows are dimmed and cannot be picked: a
 * view is on screen once, so the list never offers a second copy of a
 * conversation. They carry no extra label — the dimming is the whole signal.
 *
 * The keys come from the rail, so every row here is a view the app already
 * knows how to open. The session column repeats each row's session, because the
 * same profile name can exist in several of them.
 */
export function ProfileDrawer({
  rows,
  selectedKey,
  openViewKeys = NO_VIEW_KEYS,
  isDisabled = false,
  onChange,
}: ProfileDrawerProps) {
  const [isExpanded, setIsExpanded] = useState(false);
  const [query, setQuery] = useState('');
  const [hoveredKey, setHoveredKey] = useState<string | null>(null);

  const choiceRows = profileChoiceRows(rows, selectedKey, query, isExpanded, openViewKeys);
  const trimmedQuery = query.trim();
  const isSearching = isExpanded && choiceRows.length === 0;

  /** What a click on a row does: open the list, fold it from the current row, or switch to that profile. */
  const rowAction = (row: ProfileSourceRow, isSelected: boolean, isOpenElsewhere: boolean) => () => {
    if (isDisabled || isOpenElsewhere) return;
    if (!isExpanded) {
      setIsExpanded(true);
      return;
    }
    setIsExpanded(false);
    setQuery('');
    if (!isSelected) onChange(row.key);
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
            label="Search profiles"
            isLabelHidden
            placeholder="Search profiles in every session"
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
            No profiles match “{trimmedQuery}”.
          </Text>
        ) : (
          <VStack gap={1} width="100%">
            {choiceRows.map(({row, isSelected, isOpenElsewhere}) => (
              <HStack
                key={row.key}
                gap={2}
                align="center"
                width="100%"
                data-profile-row={isSelected ? 'selected' : isOpenElsewhere ? 'open' : 'choice'}
                paddingInline={1}
                paddingBlock={0.5}
                {...(isOpenElsewhere ? {'aria-disabled': true} : {})}
                style={{
                  ...(!isExpanded || (!isDisabled && hoveredKey === row.key) ? ROW_TINT : {}),
                  ...(isOpenElsewhere ? ALREADY_OPEN_ROW : {}),
                }}
                onMouseEnter={() => setHoveredKey(row.key)}
                onMouseLeave={() => setHoveredKey(null)}
                tabIndex={isDisabled || isOpenElsewhere ? -1 : 0}
                onClick={rowAction(row, isSelected, isOpenElsewhere)}
                onKeyDown={event => {
                  if (event.key !== 'Enter' && event.key !== ' ') return;
                  event.preventDefault();
                  rowAction(row, isSelected, isOpenElsewhere)();
                }}
              >
                <HStack align="center" width={LABEL_COLUMN_WIDTH}>
                  <HStack
                    align="center"
                    paddingInline={1}
                    paddingBlock={0.5}
                    style={isSelected ? SELECTED_LABEL_BOX : undefined}
                  >
                    <Text type="label" maxLines={1}>{row.label}</Text>
                  </HStack>
                </HStack>
                <HStack align="center" width={PATH_COLUMN_WIDTH}>
                  <Text
                    type="label"
                    color={isSelected ? undefined : 'secondary'}
                    weight={isSelected ? 'semibold' : undefined}
                    maxLines={1}
                  >
                    {/* A session's own row shows the profile the server says it stores. */}
                    {row.profile ?? row.defaultProfile ?? '—'}
                  </Text>
                </HStack>
                <HStack align="center" width={SESSION_COLUMN_WIDTH}>
                  <Text
                    type="label"
                    color={isSelected ? undefined : 'secondary'}
                    maxLines={1}
                  >
                    {row.session ?? '—'}
                  </Text>
                </HStack>
                {/* The way into the list sits at the end of the row, not beside the label. */}
                {isExpanded ? null : <Icon icon="chevronDown" size="sm" />}
              </HStack>
            ))}
          </VStack>
        )}
      </VStack>
    </ChatComposerDrawer>
  );
}
