import {useMemo} from 'react';
import type {ChatToolCallItem} from '@astryxdesign/core';
import {suppressAttachedRunTranscriptTail} from './active-run-transcript';
import type {SessionRunFailure, WebUiMessage, WebUiPart} from './api';
import {useTranscriptWindow} from './components/TranscriptWindow';
import appConfig from './config.json';
import {historyMessagesFor, type HistoryBlocks} from './history-blocks';
import {recentUserTexts} from './input-history';
import {placeRunFailures} from './run-failures';
import {latestToolCallMessage} from './transcript-facts';

type TranscriptMessage = Omit<WebUiMessage, 'parts'> & {
  parts?: WebUiPart[];
  tools?: ChatToolCallItem[];
};

/**
 * Everything App derives from the active view's message arrays.
 *
 * It lives outside App on purpose: V8 gives all closures of one render a single
 * context, and a variable any closure reads is kept in it. Callbacks that React
 * keeps across renders then pin the context of the render that made them, and
 * with it the message arrays of whichever view was active at that time — so an
 * evicted view's transcript stayed in memory. Here the arrays belong to this
 * hook's own scope, and App only receives the results.
 */
export function useActiveTranscript<Message extends TranscriptMessage>({
  threads,
  historyBlocks,
  viewKey,
  hasLiveAttachedRun,
  failures,
  isRunning,
  streamingMessage,
}: {
  threads: Readonly<Record<string, Message[]>>;
  historyBlocks: HistoryBlocks<Message>;
  viewKey: string;
  hasLiveAttachedRun: boolean;
  failures: readonly SessionRunFailure[] | undefined;
  isRunning: boolean;
  streamingMessage: WebUiMessage | null;
}) {
  const threadMessages = threads[viewKey] ?? [];
  // The history block renders ahead of the transcript: it holds the turns a
  // compaction removed, which are older than everything the transcript carries.
  const messages = useMemo(() => {
    const block = historyMessagesFor(historyBlocks, viewKey);
    return block === undefined ? threadMessages : [...block, ...threadMessages];
  }, [threadMessages, viewKey, historyBlocks]);
  const transcriptMessages = useMemo(
    () => suppressAttachedRunTranscriptTail(messages, hasLiveAttachedRun),
    [messages, hasLiveAttachedRun],
  );
  const {visibleMessages, hiddenCount, revealEarlier} = useTranscriptWindow({
    messages: transcriptMessages,
    sessionId: viewKey,
    initialCount: appConfig.transcript_window_messages,
    pageSize: appConfig.transcript_window_page_messages,
  });
  const placedFailures = useMemo(
    () => placeRunFailures(failures, visibleMessages.map(message => message.id)),
    [failures, visibleMessages],
  );
  const streamingMessageCommitted =
    streamingMessage !== null &&
    transcriptMessages.some(message => message.id === streamingMessage.id);

  const liveToolParts = useMemo(() => {
    if (isRunning && !streamingMessage) {
      return null;
    }
    const sourceMessage = streamingMessage ?? latestToolCallMessage(messages);
    return sourceMessage?.parts ?? null;
  }, [isRunning, messages, streamingMessage]);

  const inputHistory = useMemo(
    () => recentUserTexts(messages, appConfig.composer_input_history_size),
    [messages],
  );

  return {
    inputHistory,
    visibleMessages,
    hiddenCount,
    revealEarlier,
    placedFailures,
    streamingMessageCommitted,
    liveToolParts,
  };
}
