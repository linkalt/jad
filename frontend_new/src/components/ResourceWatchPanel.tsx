import {useCallback, useEffect, useRef, useState} from 'react';
import {Button, Icon, IconButton, List, ListItem, Text, VStack} from '@astryxdesign/core';
import type {ResizableRegion} from '@astryxdesign/core/Resizable';
import {
  fetchSessionMCPResourceEvents,
  fetchSessionMCPResources,
  setSessionMCPResourceWatch,
  type MCPResource,
} from '../api';
import appConfig from '../config.json';
import {
  isResourceWatched,
  mergeResourceEvents,
  type MCPResourceEvent,
  type MCPResourceSubscription,
} from '../resource-watch';
import {FloatingPanel} from './FloatingPanel';

type ResourceWatchPanelProps = {
  /** The session the resources belong to; without one the panel says so. */
  sessionId: string | undefined;
  profile: string | undefined;
  region: ResizableRegion;
  /** True with a single pane on screen: the panel takes the window's right edge. */
  isEdgeFixed?: boolean;
  onClose: () => void;
};

function ReloadIcon() {
  return (
    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={1.5}>
      <path d="M20.5 12a8.5 8.5 0 1 1-2.5-6" strokeLinecap="round" strokeLinejoin="round" />
      <path d="M20.5 3.5v5h-5" strokeLinecap="round" strokeLinejoin="round" />
    </svg>
  );
}

function eventLabel(event: MCPResourceEvent): string {
  if (event.kind === 'list_changed') return `${event.server}: the resource list changed`;
  return `${event.server}: ${event.uri ?? 'a resource'} changed`;
}

function eventTime(at: string): string {
  const time = new Date(at);
  return Number.isNaN(time.getTime()) ? at : time.toLocaleTimeString();
}

/**
 * The MCP resources of one conversation's profile, each with a watch toggle, and the change
 * notifications of the watched ones. While the panel is open it asks for new notifications
 * every `mcp_resource_events_poll_ms`; the model is told about the changes at the start of
 * its next run whether the panel is open or not.
 */
export function ResourceWatchPanel({sessionId, profile, region, isEdgeFixed, onClose}: ResourceWatchPanelProps) {
  const [resources, setResources] = useState<MCPResource[] | null>(null);
  const [failures, setFailures] = useState<{server: string; error: string}[]>([]);
  const [subscriptions, setSubscriptions] = useState<MCPResourceSubscription[]>([]);
  const [events, setEvents] = useState<MCPResourceEvent[]>([]);
  const [busyKey, setBusyKey] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [isLoading, setIsLoading] = useState(false);
  const latestRef = useRef(0);

  const pollEvents = useCallback(async (signal?: AbortSignal) => {
    if (sessionId === undefined) return;
    const answer = await fetchSessionMCPResourceEvents(sessionId, latestRef.current, profile, signal);
    latestRef.current = answer.latest;
    setSubscriptions(answer.subscriptions);
    setEvents(held => mergeResourceEvents(held, answer, appConfig.mcp_resource_events_shown_max));
  }, [profile, sessionId]);

  const refresh = useCallback(async (signal?: AbortSignal) => {
    if (sessionId === undefined) return;
    setIsLoading(true);
    setError(null);
    try {
      const listed = await fetchSessionMCPResources(sessionId, profile, signal);
      setResources(listed.resources.filter(resource => resource.uri !== undefined));
      setFailures(listed.failures);
      await pollEvents(signal);
    } catch (reason) {
      if (signal?.aborted) return;
      setError(reason instanceof Error ? reason.message : String(reason));
    } finally {
      if (!signal?.aborted) setIsLoading(false);
    }
  }, [pollEvents, profile, sessionId]);

  // A new session or profile starts over: its servers and its event log are its own.
  useEffect(() => {
    latestRef.current = 0;
    setResources(null);
    setEvents([]);
    setSubscriptions([]);
    const request = new AbortController();
    void refresh(request.signal);
    return () => request.abort();
  }, [refresh]);

  useEffect(() => {
    if (sessionId === undefined) return undefined;
    const request = new AbortController();
    const timer = window.setInterval(() => {
      pollEvents(request.signal).catch(reason => {
        if (!request.signal.aborted) setError(reason instanceof Error ? reason.message : String(reason));
      });
    }, appConfig.mcp_resource_events_poll_ms);
    return () => {
      window.clearInterval(timer);
      request.abort();
    };
  }, [pollEvents, sessionId]);

  const toggleWatch = useCallback(async (resource: MCPResource) => {
    if (sessionId === undefined || resource.uri === undefined) return;
    const key = `${resource.server}\n${resource.uri}`;
    const on = !isResourceWatched(subscriptions, resource.server, resource.uri);
    setBusyKey(key);
    setError(null);
    try {
      await setSessionMCPResourceWatch(sessionId, resource.server, resource.uri, on, profile);
      await pollEvents();
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : String(reason));
    } finally {
      setBusyKey(null);
    }
  }, [pollEvents, profile, sessionId, subscriptions]);

  const subtitle = sessionId === undefined
    ? 'No conversation'
    : `${subscriptions.length} watched`;

  return (
    <FloatingPanel
      side="end"
      layer="pane"
      label="Watched resources"
      title="Watched resources"
      subtitle={subtitle}
      region={region}
      isEdgeFixed={isEdgeFixed}
      onClose={onClose}
      footer={
        <IconButton
          label="Refresh resources"
          icon={<Icon icon={ReloadIcon} size="sm" />}
          variant="ghost"
          size="sm"
          isLoading={isLoading}
          isDisabled={sessionId === undefined}
          onClick={() => void refresh()}
        />
      }
    >
      <VStack gap={4}>
        <Text type="supporting" color="secondary">
          A watched resource tells JAD when it changes; the model hears about the changes at the start of its next run.
          Only servers JAD starts as a process (stdio) can be watched.
        </Text>
        {error !== null ? <Text type="supporting" color="secondary">{error}</Text> : null}
        {failures.map(failure => (
          <Text key={failure.server} type="supporting" color="secondary">
            {failure.server}: {failure.error}
          </Text>
        ))}
        {sessionId === undefined ? (
          <Text type="supporting" color="secondary">Open a conversation to see its resources.</Text>
        ) : resources === null ? (
          <Text type="supporting" color="secondary">Loading resources…</Text>
        ) : resources.length === 0 ? (
          <Text type="supporting" color="secondary">
            This profile has no MCP resources. A toolset turns them on with resources: true.
          </Text>
        ) : (
          <List hasDividers header={<Text type="supporting" color="secondary">Resources</Text>}>
            {resources.map(resource => {
              const uri = resource.uri ?? '';
              const watched = isResourceWatched(subscriptions, resource.server, uri);
              const key = `${resource.server}\n${uri}`;
              return (
                <ListItem
                  key={key}
                  label={resource.title ?? resource.name}
                  description={<Text type="supporting" color="secondary">{resource.server} · {uri}</Text>}
                  endContent={
                    <Button
                      label={watched ? 'Stop watching' : 'Watch'}
                      variant={watched ? 'secondary' : 'ghost'}
                      size="sm"
                      isDisabled={busyKey !== null}
                      onClick={() => void toggleWatch(resource)}
                    />
                  }
                />
              );
            })}
          </List>
        )}
        {events.length > 0 ? (
          <List hasDividers header={<Text type="supporting" color="secondary">Changes</Text>}>
            {events.map(event => (
              <ListItem
                key={event.seq}
                label={eventLabel(event)}
                description={<Text type="supporting" color="secondary">{eventTime(event.at)}</Text>}
              />
            ))}
          </List>
        ) : null}
      </VStack>
    </FloatingPanel>
  );
}
