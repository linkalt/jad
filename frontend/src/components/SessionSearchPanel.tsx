import {useMemo, useState} from 'react';
import {List, ListItem, StatusDot, Text, TextInput, Token} from '@astryxdesign/core';
import type {StatusDotVariant} from '@astryxdesign/core';
import type {ResizableRegion} from '@astryxdesign/core/Resizable';
import appConfig from '../config.json';
import {FloatingPanel} from './FloatingPanel';

type SearchableSession = {
  id: string;
  title: string;
  turns: number;
  state: StatusDotVariant;
};

type SessionSearchPanelProps = {
  region: ResizableRegion;
  sessions: readonly SearchableSession[];
  activeSessionId: string;
  /** Called with the session id when a result is chosen. */
  onSelectSession: (sessionId: string) => void;
  onClose: () => void;
};

/**
 * Third dataset for the shared left panel: a searchable list of sessions.
 *
 * It is the same shell as the prompt library and the run settings, which is the
 * point of the floating panel — a new view costs a content component plus a
 * region in App, and nothing else.
 */
export function SessionSearchPanel({
  region,
  sessions,
  activeSessionId,
  onSelectSession,
  onClose,
}: SessionSearchPanelProps) {
  const [query, setQuery] = useState('');

  const results = useMemo(() => {
    const trimmed = query.trim().toLowerCase();
    const limit = appConfig.session_search_max_results;
    const matches =
      trimmed.length === 0
        ? sessions
        : sessions.filter(session =>
            `${session.title} ${session.id} ${session.state}`
              .toLowerCase()
              .includes(trimmed),
          );
    return matches.slice(0, limit);
  }, [query, sessions]);

  const isSearching = query.trim().length > 0;

  return (
    <FloatingPanel
      side="start"
      label="Session search"
      title="Session search"
      subtitle={
        isSearching
          ? `${results.length} of ${sessions.length} sessions`
          : `${sessions.length} sessions`
      }
      region={region}
      onClose={onClose}
    >
      <TextInput
        label="Search sessions"
        isLabelHidden
        placeholder="Search by title, id or state"
        startIcon="search"
        hasClear
        size="sm"
        value={query}
        onChange={setQuery}
      />
      {results.length === 0 ? (
        <Text type="supporting" color="secondary">
          {isSearching ? `No sessions match “${query.trim()}”.` : 'No sessions yet.'}
        </Text>
      ) : (
        <List
          hasDividers
          header={
            <Text type="supporting" color="secondary">
              Select a session to open it.
            </Text>
          }
        >
          {results.map(session => (
            <ListItem
              key={session.id}
              label={session.title}
              description={`${session.turns} ${session.turns === 1 ? 'turn' : 'turns'} · ${session.state}`}
              startContent={
                <StatusDot
                  variant={session.state}
                  label={`${session.title} ${session.state}`}
                />
              }
              endContent={
                session.id === activeSessionId ? <Token label="open" size="sm" /> : undefined
              }
              onClick={() => onSelectSession(session.id)}
            />
          ))}
        </List>
      )}
    </FloatingPanel>
  );
}
