/**
 * @file SubagentTranscript.tsx
 * @input The parent call's state (its tool result's `subagent` summary, absent while the call is unsettled), and where the steps are read from (session, profile, call id)
 * @output The right panel's subagent section: a "Show Transcript · …" button that, once
 *   pressed, shows the child's completed steps as a read-only conversation under the
 *   "Subagent transcript" heading, filling in while the child runs
 * @position Passed to ToolCallDetail as its children by ActivityPanel
 *
 * The transcript costs nothing until it is asked for. While closed this is one button and
 * no timer: no request, no mapping, no ReadOnlyTranscript. While open, `useSubagentPoll`
 * (subagent-transcript-poll.ts) reads the route at once and then every
 * `subagent_transcript_poll_ms` for as long as the call is unsettled or the answer says the
 * child is `running`; each answer replaces the store's entry, and the steps appear one
 * completed response at a time (never token by token, and never the step still waiting for
 * its tool results). A running child ends the transcript with "Working on step N+1…". The
 * host keys this component by the open call, so every open, and every switch to another
 * call, starts closed. The shared renderer opens no panel.
 */
import {useEffect, useMemo, useRef, useState} from 'react';
import {Button, Text, VStack} from '@astryxdesign/core';
import {
  fetchSubagentTranscript,
  validSubagentSummary,
  type WebUiSubagentSummary,
} from '../api';
import appConfig from '../config.json';
import {subagentTranscriptLabel, subagentTranscriptMessages} from '../subagent-transcript';
import {
  createSubagentPoller,
  type SubagentPoller,
  type SubagentPollState,
  type SubagentVisibility,
} from '../subagent-transcript-poll';
import {createSubagentTranscriptStore, subagentTranscriptKey} from '../subagent-transcript-store';
import {ReadOnlyTranscript} from './ReadOnlyTranscript';

const transcriptStore = createSubagentTranscriptStore(appConfig.subagent_transcript_cache_entries);

const documentVisibility: SubagentVisibility = {
  isVisible: () => document.visibilityState !== 'hidden',
  subscribe: listener => {
    document.addEventListener('visibilitychange', listener);
    return () => document.removeEventListener('visibilitychange', listener);
  },
};

export interface SubagentTranscriptProps {
  /** The settled result's summary; absent while the call has no result (its child is running). */
  summary?: WebUiSubagentSummary;
  /** The call has its result. */
  settled: boolean;
  /** The session on screen and its profile: the transcript is read from there. */
  sessionRef?: string;
  profile?: string;
  callId: string;
}

/**
 * Poll one call's transcript while `enabled` (the panel is open). The effect that owns the
 * poller depends only on what identifies the call, so the host re-rendering with a fresh
 * message does not restart it; the call's settled state reaches the running poller through
 * `update`. Closing stops the poller and with it every timer.
 */
function useSubagentPoll({
  enabled,
  sessionRef,
  profile,
  callId,
  settled,
  summarySteps,
}: {
  enabled: boolean;
  sessionRef?: string;
  profile?: string;
  callId: string;
  settled: boolean;
  summarySteps: number;
}) {
  const [state, setState] = useState<SubagentPollState>({phase: 'loading'});
  const pollerRef = useRef<SubagentPoller | undefined>(undefined);
  const inputRef = useRef({settled, summarySteps});
  inputRef.current = {settled, summarySteps};
  useEffect(() => {
    if (!enabled || sessionRef === undefined) return undefined;
    const poller = createSubagentPoller({
      store: transcriptStore,
      key: subagentTranscriptKey(sessionRef, profile, callId),
      fetch: () => fetchSubagentTranscript(sessionRef, callId, profile),
      pollMs: appConfig.subagent_transcript_poll_ms,
      maxDelayMs: appConfig.subagent_transcript_poll_max_delay_ms,
      visibility: documentVisibility,
      input: inputRef.current,
      onState: setState,
    });
    pollerRef.current = poller;
    poller.start();
    return () => {
      poller.stop();
      pollerRef.current = undefined;
    };
  }, [enabled, sessionRef, profile, callId]);
  useEffect(() => {
    pollerRef.current?.update({settled, summarySteps});
  }, [settled, summarySteps]);
  return {state, retry: () => pollerRef.current?.retry()};
}

export function SubagentTranscript({summary, settled, sessionRef, profile, callId}: SubagentTranscriptProps) {
  const [isOpen, setIsOpen] = useState(false);
  // History is not validated part by part, so a malformed summary (null, a negative count)
  // shows nothing. The panel without a session has nowhere to read the steps from.
  const isShown = sessionRef !== undefined && (summary === undefined || validSubagentSummary(summary) !== undefined);
  const {state, retry} = useSubagentPoll({
    enabled: isOpen && isShown,
    sessionRef,
    profile,
    callId,
    settled,
    summarySteps: summary?.steps ?? 0,
  });
  if (!isShown) {
    return null;
  }
  const label = subagentTranscriptLabel({
    isOpen,
    summary,
    transcript: isOpen && state.phase === 'ready' ? state.transcript : undefined,
  });
  return (
    <VStack gap={2} width="100%">
      <Button
        label={label}
        variant="ghost"
        size="sm"
        aria-expanded={isOpen}
        onClick={() => setIsOpen(open => !open)}
      />
      {isOpen && (
        <OpenSubagentTranscript
          agent={summary?.agent}
          omitsLastText={summary?.omitsLastText === true}
          settled={settled}
          state={state}
          onRetry={retry}
        />
      )}
    </VStack>
  );
}

function OpenSubagentTranscript({
  agent,
  omitsLastText,
  settled,
  state,
  onRetry,
}: {
  agent?: string;
  /** The result does not hold the child's last text whole, so it is not left out here. */
  omitsLastText: boolean;
  settled: boolean;
  state: SubagentPollState;
  onRetry: () => void;
}) {
  const transcript = state.phase === 'ready' ? state.transcript : undefined;
  // The snapshot's own `mayHaveNewer` and `running` ride inside `transcript`, so a refetched
  // snapshot (a new object) re-maps without a dependency of its own.
  const messages = useMemo(
    () => (transcript === undefined ? [] : subagentTranscriptMessages(transcript, {omitsLastText, unsettled: !settled})),
    [transcript, omitsLastText, settled],
  );
  const running = transcript?.running === true;
  const name = agent ?? transcript?.agent;
  return (
    <VStack gap={2} width="100%">
      <Text type="supporting" color="secondary">
        {name ? `Subagent transcript · ${name}` : 'Subagent transcript'}
      </Text>
      {state.phase === 'loading' && (
        <Text type="supporting" color="secondary">
          Loading transcript…
        </Text>
      )}
      {state.phase === 'starting' && (
        <Text type="supporting" color="secondary">
          Starting…
        </Text>
      )}
      {state.phase === 'failed' && (
        <VStack gap={1} width="100%">
          <Text type="supporting" color="secondary">
            Could not load the transcript: {state.message}
          </Text>
          <Button label="Retry" variant="ghost" size="sm" onClick={onRetry} />
        </VStack>
      )}
      {transcript !== undefined && messages.length === 0 && !running && (
        <Text type="supporting" color="secondary">
          No steps to show.
        </Text>
      )}
      {messages.length > 0 && <ReadOnlyTranscript messages={messages} />}
      {running && transcript !== undefined && (
        <Text type="supporting" color="secondary">
          Working on step {transcript.steps.length + 1}…
        </Text>
      )}
      {state.phase === 'ready' && state.retryError !== undefined && (
        <Text type="supporting" color="secondary">
          Could not refresh, trying again: {state.retryError}
        </Text>
      )}
    </VStack>
  );
}
