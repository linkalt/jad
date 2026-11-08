/**
 * @file SubagentTranscript.tsx
 * @input A tool result's `subagent` summary, and where its steps are read from (session, profile, call id)
 * @output The right panel's subagent section: a "Show Transcript · N steps" button that,
 *   once pressed, loads the child's steps and shows them as a read-only conversation under
 *   the "Subagent transcript" heading
 * @position Passed to ToolCallDetail as its children by ActivityPanel
 *
 * The transcript costs nothing until it is asked for. While closed this renders one button
 * whose label reads the summary's step count: no request, no validation of steps, no
 * mapping, no ReadOnlyTranscript (those live in `OpenSubagentTranscript`, mounted only
 * while open). The first open fetches that one call's steps (loading state, then the
 * shared ReadOnlyTranscript; a failure shows the error with a retry). A small store keeps
 * the fetched steps per call, so hiding and reopening does not fetch again; nothing is
 * prefetched or polled. The host keys this component by the open call, so every open, and
 * every switch to another call, starts closed. The shared renderer opens no panel.
 */
import {useEffect, useMemo, useState} from 'react';
import {Button, Text, VStack} from '@astryxdesign/core';
import {fetchSubagentTranscript, type WebUiSubagentSummary, type WebUiSubagentTranscript} from '../api';
import appConfig from '../config.json';
import {subagentTranscriptMessages} from '../subagent-transcript';
import {createSubagentTranscriptStore, subagentTranscriptKey} from '../subagent-transcript-store';
import {ReadOnlyTranscript} from './ReadOnlyTranscript';

const transcriptStore = createSubagentTranscriptStore(appConfig.subagent_transcript_cache_entries);

export interface SubagentTranscriptProps {
  summary: WebUiSubagentSummary;
  /** The session on screen and its profile: the transcript is read from there. */
  sessionRef?: string;
  profile?: string;
  callId: string;
}

export function SubagentTranscript({summary, sessionRef, profile, callId}: SubagentTranscriptProps) {
  const [isOpen, setIsOpen] = useState(false);
  // The panel without a session has nowhere to read the steps from.
  if (sessionRef === undefined || typeof summary.steps !== 'number') {
    return null;
  }
  const label = `${isOpen ? 'Hide' : 'Show'} Transcript · ${summary.steps} ${summary.steps === 1 ? 'step' : 'steps'}`;
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
          agent={summary.agent}
          steps={summary.steps}
          sessionRef={sessionRef}
          profile={profile}
          callId={callId}
        />
      )}
    </VStack>
  );
}

type Load =
  | {state: 'loading'}
  | {state: 'ready'; transcript: WebUiSubagentTranscript}
  | {state: 'failed'; message: string};

function OpenSubagentTranscript({
  agent,
  steps,
  sessionRef,
  profile,
  callId,
}: {
  agent?: string;
  steps: number;
  sessionRef: string;
  profile?: string;
  callId: string;
}) {
  const key = subagentTranscriptKey(sessionRef, profile, callId, steps);
  const [attempt, setAttempt] = useState(0);
  const [load, setLoad] = useState<Load>(() => {
    const cached = transcriptStore.peek(key);
    return cached === undefined ? {state: 'loading'} : {state: 'ready', transcript: cached};
  });
  useEffect(() => {
    const cached = transcriptStore.peek(key);
    if (cached !== undefined) {
      setLoad({state: 'ready', transcript: cached});
      return undefined;
    }
    let isCurrent = true;
    setLoad({state: 'loading'});
    transcriptStore
      .load(key, () => fetchSubagentTranscript(sessionRef, callId, profile))
      .then(
        transcript => isCurrent && setLoad({state: 'ready', transcript}),
        error => isCurrent && setLoad({state: 'failed', message: error instanceof Error ? error.message : String(error)}),
      );
    return () => {
      isCurrent = false;
    };
  }, [key, sessionRef, callId, profile, attempt]);
  const transcript = load.state === 'ready' ? load.transcript : undefined;
  const messages = useMemo(
    () => (transcript === undefined ? [] : subagentTranscriptMessages(transcript)),
    [transcript],
  );
  return (
    <VStack gap={2} width="100%">
      <Text type="supporting" color="secondary">
        {agent ? `Subagent transcript · ${agent}` : 'Subagent transcript'}
      </Text>
      {load.state === 'loading' && (
        <Text type="supporting" color="secondary">
          Loading transcript…
        </Text>
      )}
      {load.state === 'failed' && (
        <VStack gap={1} width="100%">
          <Text type="supporting" color="secondary">
            Could not load the transcript: {load.message}
          </Text>
          <Button label="Retry" variant="ghost" size="sm" onClick={() => setAttempt(count => count + 1)} />
        </VStack>
      )}
      {load.state === 'ready' &&
        (messages.length === 0 ? (
          <Text type="supporting" color="secondary">
            No steps to show.
          </Text>
        ) : (
          <ReadOnlyTranscript messages={messages} />
        ))}
    </VStack>
  );
}
