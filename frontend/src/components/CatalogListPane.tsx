/**
 * @file CatalogListPane.tsx
 * @input The files of one catalog kind, the search text and the filter choice
 * @output A search bar, a filter, a New button and a table of the matching files
 * @position The right panel of the catalog editor while no file is open
 */
import {useMemo, useState} from 'react';

import {Banner, Button, HStack, StackItem, Text, TextInput, VStack} from '@astryxdesign/core';
import {EmptyState} from '@astryxdesign/core/EmptyState';
import {Link} from '@astryxdesign/core/Link';
import {Selector} from '@astryxdesign/core/Selector';
import {Table, proportional, useTableColumnResize, useTableSortable, type TablePlugin, type TableColumn} from '@astryxdesign/core/Table';

import type {CatalogFileInfo, CatalogKind} from '../api';
import {
  catalogRows,
  filterCatalogFiles,
  filterOptions,
  filterValues,
  filtersBySession,
  sortCatalogRows,
  type CatalogRow,
  type SortEntry,
} from '../catalog-list-view';
import type {CatalogKindView} from '../catalog-editor-view';
import appConfig from '../config.json' with {type: 'json'};

export interface CatalogListPaneProps {
  kind: CatalogKind;
  view: CatalogKindView;
  files: readonly CatalogFileInfo[];
  isLoading: boolean;
  error: string | null;
  isDisabled: boolean;
  query: string;
  filter: string;
  onQueryChange: (query: string) => void;
  onFilterChange: (filter: string) => void;
  onOpen: (id: string) => void;
  onNew: () => void;
  onRetry: () => void;
}

const DEFAULT_SORT: SortEntry[] = [
  {sortKey: 'category', direction: 'ascending'},
  {sortKey: 'id', direction: 'ascending'},
];

function tableColumns(showSession: boolean): TableColumn<CatalogRow>[] {
  const columns: TableColumn<CatalogRow>[] = [
    {key: 'category', header: 'Category', sortable: true, renderCell: row => <Text type="supporting" color="secondary">{row.category}</Text>},
    {key: 'id', header: 'ID', sortable: true, width: proportional(1.4, {minWidth: 120}), renderCell: row => <Link isStandalone>{row.id}</Link>},
    {key: 'name', header: 'Name', sortable: true, width: proportional(1.4, {minWidth: 120}), renderCell: row => <Text type="body">{row.name}</Text>},
  ];
  if (showSession) {
    columns.unshift({key: 'sessions', header: 'Session', sortable: true, width: proportional(1.4, {minWidth: 120}), renderCell: row => <Text type="supporting" color="secondary">{row.sessions}</Text>});
  }
  return columns;
}

export function CatalogListPane({
  kind,
  view,
  files,
  isLoading,
  error,
  isDisabled,
  query,
  filter,
  onQueryChange,
  onFilterChange,
  onOpen,
  onNew,
  onRetry,
}: CatalogListPaneProps) {
  const options = useMemo(() => filterOptions(kind, files), [kind, files]);
  const known = useMemo(() => filterValues(options), [options]);
  const activeFilter = known.includes(filter) ? filter : known[0];
  const [sort, setSort] = useState<SortEntry[]>(DEFAULT_SORT);
  const [columnWidths, setColumnWidths] = useState<Record<string, number>>({});
  const matched = useMemo(() => filterCatalogFiles(kind, files, query, activeFilter), [kind, files, query, activeFilter]);
  const rows = useMemo(() => sortCatalogRows(catalogRows(kind, matched), sort), [kind, matched, sort]);
  const plural = view.plural.toLowerCase();
  const showSession = filtersBySession(kind);
  const columns = useMemo(() => tableColumns(showSession), [showSession]);
  const sortPlugin = useTableSortable<CatalogRow>({sort, onSortChange: next => setSort([...next])});
  const resizePlugin = useTableColumnResize({
    columnWidths,
    onColumnResizeEnd: updates => setColumnWidths(previous => ({...previous, ...updates})),
    columns: columns as TableColumn<Record<string, unknown>>[],
  }) as TablePlugin<CatalogRow>;
  const openPlugin = useMemo<TablePlugin<CatalogRow>>(
    () => ({
      transformBodyRow: (props, item) => ({
        ...props,
        htmlProps: {
          ...props.htmlProps,
          style: {...props.htmlProps.style, cursor: isDisabled ? 'default' : 'pointer'},
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
            placeholder={showSession ? `Search ${plural} by session, category, ID or name` : `Search ${plural} by category, ID or name`}
            startIcon="search"
            hasClear
            value={query}
            onChange={onQueryChange}
          />
        </StackItem>
        <Selector
          label={showSession ? 'Session' : 'Category'}
          isLabelHidden
          options={options}
          value={activeFilter}
          onChange={onFilterChange}
          width={appConfig.catalog_category_filter_width_px}
        />
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
