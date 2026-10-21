import {useMemo, useState} from 'react';
import {List, ListItem, StatusDot, Text, TextInput, Token} from '@astryxdesign/core';
import type {ResizableRegion} from '@astryxdesign/core/Resizable';
import appConfig from '../config.json';
import {
  matchProfileSearchRows,
  profileSearchDescription,
  profileSearchRows,
  type ProfileSearchSession,
} from '../profile-search';
import type {SessionListGroup} from '../session-views';
import {FloatingPanel} from './FloatingPanel';
import {sessionDotXstyle} from './session-dot';

type ProfileSearchPanelProps = {
  region: ResizableRegion;
  /** The rail's groups: the search offers the rows they render. */
  groups: readonly SessionListGroup[];
  sessions: readonly ProfileSearchSession[];
  rowTurns: Readonly<Record<string, number>>;
  runningViewKeys: ReadonlySet<string>;
  activeViewKey: string;
  /** Called with the chosen row's view key, which is what the rail's click opens. */
  onSelectView: (viewKey: string) => void;
  onClose: () => void;
};

/**
 * Third dataset for the shared left panel: a searchable list of profile rows.
 *
 * It is the same shell as the prompt library and the run settings, which is the
 * point of the floating panel — a new view costs a content component plus a
 * region in App, and nothing else. The rows and their numbers come from the same
 * model the rail renders, so choosing a result is choosing a row.
 */
export function ProfileSearchPanel({
  region,
  groups,
  sessions,
  rowTurns,
  runningViewKeys,
  activeViewKey,
  onSelectView,
  onClose,
}: ProfileSearchPanelProps) {
  const [query, setQuery] = useState('');

  const rows = useMemo(
    () => profileSearchRows(groups, sessions, rowTurns, runningViewKeys),
    [groups, sessions, rowTurns, runningViewKeys],
  );
  const results = useMemo(
    () => matchProfileSearchRows(rows, query, appConfig.profile_search_max_results),
    [rows, query],
  );

  const isSearching = query.trim().length > 0;

  return (
    <FloatingPanel
      side="start"
      label="Profile search"
      title="Profile search"
      subtitle={
        isSearching
          ? `${results.length} of ${rows.length} profiles`
          : `${rows.length} profiles`
      }
      region={region}
      onClose={onClose}
    >
      <TextInput
        label="Search profiles"
        isLabelHidden
        placeholder="Search by profile or session"
        startIcon="search"
        hasClear
        size="sm"
        value={query}
        onChange={setQuery}
      />
      {results.length === 0 ? (
        <Text type="supporting" color="secondary">
          {isSearching ? `No profiles match “${query.trim()}”.` : 'No profiles yet.'}
        </Text>
      ) : (
        <List
          hasDividers
          header={
            <Text type="supporting" color="secondary">
              Select a profile to open it.
            </Text>
          }
        >
          {results.map(row => (
            <ListItem
              key={row.key}
              label={row.label}
              description={profileSearchDescription(row)}
              startContent={
                <StatusDot
                  variant={row.dot.variant}
                  label={`${row.fullLabel} ${row.dot.word}`}
                  xstyle={sessionDotXstyle(row.dot)}
                />
              }
              endContent={row.key === activeViewKey ? <Token label="open" size="sm" /> : undefined}
              onClick={() => onSelectView(row.key)}
            />
          ))}
        </List>
      )}
    </FloatingPanel>
  );
}
