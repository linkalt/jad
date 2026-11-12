/**
 * @file CatalogListPane.tsx
 * @input The files of one catalog kind, the search text, the filter choice and a delete request
 * @output A search bar, a filter, a New button and a table of the matching files, each row with its own delete
 * @position The right panel of the catalog editor while no file is open
 */
import {useMemo} from 'react';

import {Banner, Button, HStack, Icon, IconButton, StackItem, Text, TextInput, VStack} from '@astryxdesign/core';
import {EmptyState} from '@astryxdesign/core/EmptyState';
import {Link} from '@astryxdesign/core/Link';
import {Selector} from '@astryxdesign/core/Selector';
import {Table, proportional, useTableColumnResize, useTableSortable, type TablePlugin, type TableColumn} from '@astryxdesign/core/Table';
import {Tooltip} from '@astryxdesign/core/Tooltip';

import type {CatalogFileInfo, CatalogKind} from '../api';
import {
  catalogRows,
  filterCatalogFiles,
  filterOptions,
  filterValues,
  filtersBySession,
  groupsByWorkspace,
  sortCatalogRows,
  type WorkspaceLabels,
  type CatalogRow,
  type CatalogRowTarget,
  type SortEntry,
} from '../catalog-list-view';
import type {CatalogKindView} from '../catalog-editor-view';
import appConfig from '../config.json' with {type: 'json'};
import {TrashIcon} from './AppIcons';

export interface CatalogListPaneProps {
  kind: CatalogKind;
  view: CatalogKindView;
  files: readonly CatalogFileInfo[];
  /** Workspace labels by id, for the sessions' workspace column and filter. */
  workspaceLabels?: WorkspaceLabels;
  isLoading: boolean;
  error: string | null;
  isDisabled: boolean;
  query: string;
  filter: string;
  sort: SortEntry[];
  columnWidths: Record<string, number>;
  onSortChange: (sort: SortEntry[]) => void;
  onColumnWidthsChange: (widths: Record<string, number>) => void;
  onQueryChange: (query: string) => void;
  onFilterChange: (filter: string) => void;
  onOpen: (id: string) => void;
  onNew: () => void;
  onRetry: () => void;
  /** Asks for one row to be deleted; the confirmation belongs to the dialog. */
  onDelete: (target: CatalogRowTarget) => void;
}

/** Sorting by a column the table does not show (a session list in one workspace has no workspace column) is ignored. */
export const DEFAULT_CATALOG_SORT: SortEntry[] = [
  {sortKey: 'category', direction: 'ascending'},
  {sortKey: 'id', direction: 'ascending'},
];

const textColumnWidth = proportional(appConfig.catalog_table_text_column_weight, {
  minWidth: appConfig.catalog_table_text_column_min_width_px,
});

// The Action column is the table's trailing flexible column. The resize plugin
// drops any column marked `resizable: false` from its set — it is never in a drag
// snapshot, so it can never change — and a flexible column hands its drag to the
// next one, which makes the last flexible column the table's remainder, owning no
// handle. That is what this column wants: Name gets the handle on the name|action
// boundary, Action takes what the table has left, and no handle hangs on the
// table's right edge where the plugin's remainder rule would ignore it.
//
// Its weight is set by the width the table cannot go below, because that is the one
// thing this column can take away: the plugin charges a flexible column
// floor * totalWeight / weight, and below 72 * 1.4 / 120 = 0.84 the Action floor is
// what makes the table too wide for the pane at the dialog's narrowest. Its floor is
// the header's own text box, so that the word cannot be ellipsized by a font arriving
// in place of Inter.
const actionColumnWidth = proportional(appConfig.catalog_action_column_weight, {
  minWidth: appConfig.catalog_action_column_min_width_px,
});

function tableColumns(
  kind: CatalogKind,
  showSession: boolean,
  showEfforts: boolean,
  showWorkspace: boolean,
  onDelete: (target: CatalogRowTarget) => void,
  isDisabled: boolean,
): TableColumn<CatalogRow>[] {
  const columns: TableColumn<CatalogRow>[] = [
    {key: 'id', header: 'ID', sortable: true, width: textColumnWidth, renderCell: row => <Link isStandalone>{row.id}</Link>},
    {key: 'name', header: 'Name', sortable: true, width: textColumnWidth, renderCell: row => <Text type="body" maxLines={1}>{row.name}</Text>},
  ];
  if (showEfforts) {
    columns.push({key: 'efforts', header: 'Efforts', sortable: true, width: textColumnWidth, renderCell: row => <Text type="supporting" color="secondary" maxLines={1}>{row.efforts}</Text>});
  }
  // Sessions are grouped by workspace, which says something only with more than one; the other
  // kinds keep their category column.
  if (kind !== 'sessions' || showWorkspace) {
    columns.unshift({key: 'category', header: kind === 'sessions' ? 'Workspace' : 'Category', sortable: true, renderCell: row => <Text type="supporting" color="secondary" maxLines={1}>{row.category}</Text>});
  }
  if (showSession) {
    columns.unshift({key: 'sessions', header: 'Session', sortable: true, width: textColumnWidth, renderCell: row => <Text type="supporting" color="secondary" maxLines={1}>{row.sessions}</Text>});
  }
  columns.push({
    key: 'action',
    header: 'Action',
    width: actionColumnWidth,
    // An icon column has no text to align to a leading edge: centred, the button
    // sits in the middle of its room instead of hugging the cell's padding.
    align: 'center',
    renderCell: row => (
      <Tooltip content="Delete">
        <IconButton
          label={`Delete ${row.id}`}
          icon={<Icon icon={TrashIcon} size="sm" />}
          variant="destructive"
          size="sm"
          isDisabled={isDisabled || row.etag === undefined}
          // The whole body row opens the file, and IconButton hands the click
          // straight to its own onClick without stopping it, so the delete stops
          // it here — otherwise the row opens the file it is about to delete.
          onClick={event => {
            event.stopPropagation();
            if (row.etag !== undefined) onDelete({id: row.id, etag: row.etag});
          }}
        />
      </Tooltip>
    ),
  });
  return columns;
}

export function CatalogListPane({
  kind,
  view,
  files,
  workspaceLabels,
  isLoading,
  error,
  isDisabled,
  query,
  filter,
  sort,
  columnWidths,
  onSortChange,
  onColumnWidthsChange,
  onQueryChange,
  onFilterChange,
  onOpen,
  onNew,
  onRetry,
  onDelete,
}: CatalogListPaneProps) {
  const options = useMemo(() => filterOptions(kind, files, workspaceLabels), [kind, files, workspaceLabels]);
  const known = useMemo(() => filterValues(options), [options]);
  const activeFilter = known.includes(filter) ? filter : known[0];
  const matched = useMemo(() => filterCatalogFiles(kind, files, query, activeFilter, workspaceLabels), [kind, files, query, activeFilter, workspaceLabels]);
  const rows = useMemo(() => sortCatalogRows(catalogRows(kind, matched, workspaceLabels), sort), [kind, matched, sort, workspaceLabels]);
  const plural = view.plural.toLowerCase();
  const showSession = filtersBySession(kind);
  const showEfforts = kind === 'models';
  const showWorkspace = useMemo(() => groupsByWorkspace(kind, files), [kind, files]);
  // A session list in one workspace has nothing to filter by.
  const showFilter = kind !== 'sessions' || showWorkspace;
  const columns = useMemo(() => tableColumns(kind, showSession, showEfforts, showWorkspace, onDelete, isDisabled), [kind, showSession, showEfforts, showWorkspace, onDelete, isDisabled]);
  const sortPlugin = useTableSortable<CatalogRow>({sort, onSortChange: next => onSortChange([...next])});
  const resizePlugin = useTableColumnResize({
    columnWidths,
    onColumnResizeEnd: updates => onColumnWidthsChange({...columnWidths, ...updates}),
    columns: columns as TableColumn<Record<string, unknown>>[],
  }) as TablePlugin<CatalogRow>;
  const openPlugin = useMemo<TablePlugin<CatalogRow>>(
    () => ({
      transformBodyRow: (props, item) => ({
        ...props,
        htmlProps: {
          ...props.htmlProps,
          'data-catalog-row': isDisabled ? 'disabled' : 'open',
          onClick: () => {
            if (!isDisabled) onOpen(item.id);
          },
        },
      }),
    }),
    [isDisabled, onOpen],
  );

  return (
    <VStack gap={3} width="100%" height="100%">
      <HStack gap={2} align="end">
        <StackItem size="fill">
          <TextInput
            label={`Search ${plural}`}
            isLabelHidden
            placeholder={
              showSession
                ? `Search ${plural} by session, category, ID or name`
                : kind === 'sessions' ? `Search ${plural} by workspace, ID or name` : `Search ${plural} by category, ID or name`
            }
            startIcon="search"
            hasClear
            value={query}
            onChange={onQueryChange}
          />
        </StackItem>
        {showFilter && (
          <Selector
            label={showSession ? 'Session' : kind === 'sessions' ? 'Workspace' : 'Category'}
            isLabelHidden
            options={options}
            value={activeFilter}
            onChange={onFilterChange}
            width={appConfig.catalog_category_filter_width_px}
          />
        )}
        <Button label={`New ${view.noun}`} variant="primary" onClick={onNew} isDisabled={isDisabled} />
      </HStack>
      <StackItem size="fill" isScrollable>
        {error !== null ? (
          <Banner
            status="error"
            title={`Could not list ${plural}`}
            description={error}
            collapsible={false}
            endContent={<Button label="Retry" size="sm" onClick={onRetry} />}
          />
        ) : isLoading ? (
          <Text type="supporting" color="secondary">Loading {plural}…</Text>
        ) : files.length === 0 ? (
          <EmptyState title={`No ${plural}`} description="Create the first one." />
        ) : rows.length === 0 ? (
          <EmptyState title="No matches" description="Change the search or the filter." />
        ) : (
          <Table
            density="compact"
            aria-label={view.plural}
            data={rows}
            columns={columns}
            idKey="id"
            hasHover
            plugins={{sort: sortPlugin, resize: resizePlugin, open: openPlugin}}
          />
        )}
      </StackItem>
      {error === null && !isLoading && files.length > 0 && (
        <Text type="supporting" color="secondary">{rows.length} of {files.length}</Text>
      )}
    </VStack>
  );
}
