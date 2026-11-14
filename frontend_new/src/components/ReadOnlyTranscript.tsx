/**
 * @file ReadOnlyTranscript.tsx
 * @input A list of read-only messages (role, parts, optional steps) and, optionally, the width rows are cut to
 * @output A static conversation: text as Markdown, reasoning and tool calls as rows that expand in place
 * @position Shared view. The subagent snapshot in the right panel is its first caller
 *   (components/SubagentTranscript.tsx); a transcript reader passes whole transcripts the same way.
 *
 * Public API: `<ReadOnlyTranscript messages={ReadOnlyMessage[]} rowWidthPx? />`. The
 * message shape and the row selection are in `read-only-transcript.ts`.
 *
 * It reuses the conversation's own pieces (the Astryx tool-call row, ToolCallDetail,
 * ReasoningDetail, MarkdownText), but not its panel wiring: a row here expands in place
 * through the tool-call row's own chevron (Enter and Space work on it), and nothing
 * registers with the pane's right panel, so a click never opens, replaces or closes the
 * panel that may already hold this transcript. There is no streaming state, no running
 * indicator (a call with no result is shown settled), no composer, fork or checkpoint
 * chrome, and no "load earlier steps" control: every message shows all its steps.
 */
import {type ChatToolCallItem, ChatMessage, ChatMessageBubble, ChatToolCalls, VStack} from '@astryxdesign/core';
import {spacingVars} from '@astryxdesign/core/theme/tokens.stylex';
import * as stylex from '@stylexjs/stylex';
import {type ReadOnlyMessage, type ReadOnlyRow, type ReadOnlyToolCall, readOnlySegments} from '../read-only-transcript';
import {useRowWidth} from '../use-row-width';
import {hostedToolStatus} from '../web-search-view';
import {CitationList} from './CitationList';
import {MarkdownText} from './MarkdownText';
import {
  HostedToolDetail,
  OrphanToolResultBlock,
  ReasoningDetail,
  ToolCallDetail,
  TruncatedNotice,
  reasoningCallItem,
  toolCallItem,
} from './TranscriptParts';

const styles = stylex.create({
  // Rows of one stack keep the conversation's tight rhythm: the stack gap is the spacing.
  flush: {
    marginBlockStart: spacingVars['--spacing-0'],
  },
});

/** A row of a finished transcript is never in progress. */
function settled(status: ChatToolCallItem['status']): ChatToolCallItem['status'] {
  return status === 'running' || status === 'pending' ? 'complete' : status;
}

function callItem(entry: ReadOnlyToolCall, rowWidthPx: number): ChatToolCallItem {
  const item = toolCallItem(entry.call, entry.result, rowWidthPx);
  return {
    ...item,
    key: entry.key,
    status: settled(item.status),
    // No `source`: a read-only snapshot has no cursor to load a cut output whole from.
    // No children: a snapshot inside the result is never opened from here.
    resultDetail: <ToolCallDetail call={entry.call} result={entry.result} />,
  };
}

function ReasoningRow({row, rowWidthPx}: {row: Extract<ReadOnlyRow, {type: 'reasoning'}>; rowWidthPx: number}) {
  const {part} = row;
  const item = reasoningCallItem({content: part.text, steps: [], rowWidthPx, isStreaming: false});
  if (item === null) {
    return null;
  }
  return (
    <VStack gap={1} width="100%">
      <ChatToolCalls
        calls={[{...item, key: row.key, resultDetail: <ReasoningDetail content={part.text} steps={[]} />}]}
        xstyle={styles.flush}
      />
      {part.truncated !== undefined && <TruncatedNotice bytes={part.truncated.bytes} source={undefined} />}
    </VStack>
  );
}

function ToolsRow({row, rowWidthPx}: {row: Extract<ReadOnlyRow, {type: 'tools'}>; rowWidthPx: number}) {
  return (
    <VStack gap={1} width="100%">
      {row.calls.length > 0 && (
        // Several calls share one summary row that expands to the list, like the
        // conversation's grouped tools did; each call expands to its own detail.
        <ChatToolCalls calls={row.calls.map(entry => callItem(entry, rowWidthPx))} xstyle={styles.flush} />
      )}
      {row.orphans.map(({key, result}) => (
        <OrphanToolResultBlock key={key} result={result} />
      ))}
    </VStack>
  );
}

function HostedRow({row}: {row: Extract<ReadOnlyRow, {type: 'hosted'}>}) {
  const {part} = row;
  const status = settled(hostedToolStatus(part.status));
  return (
    <ChatToolCalls
      calls={[
        {
          key: row.key,
          name: part.name,
          status,
          target: part.target ?? '',
          errorMessage: status === 'error' ? 'The provider tool failed.' : undefined,
          resultDetail: part.raw === undefined ? undefined : <HostedToolDetail part={part} />,
        },
      ]}
      xstyle={styles.flush}
    />
  );
}

function TextRow({row}: {row: Extract<ReadOnlyRow, {type: 'text'}>}) {
  const {part} = row;
  return (
    <VStack gap={1} width="100%">
      <MarkdownText density="compact" contentWidth="100%" headingLevelStart={4}>
        {part.text}
      </MarkdownText>
      {part.citations !== undefined && part.citations.length > 0 && <CitationList citations={part.citations} />}
      {part.truncated !== undefined && <TruncatedNotice bytes={part.truncated.bytes} source={undefined} />}
    </VStack>
  );
}

function ReadOnlyBody({message, rowWidthPx}: {message: ReadOnlyMessage; rowWidthPx: number}) {
  return (
    <VStack gap={3} width="100%">
      {readOnlySegments(message).map((segment, index) => {
        if (segment.type === 'content') {
          return <TextRow key={segment.row.key} row={segment.row} />;
        }
        return (
          <VStack key={`activity-${index}`} gap={0} width="100%">
            {segment.rows.map(row => {
              if (row.type === 'reasoning') {
                return <ReasoningRow key={row.key} row={row} rowWidthPx={rowWidthPx} />;
              }
              if (row.type === 'hosted') {
                return <HostedRow key={row.key} row={row} />;
              }
              return <ToolsRow key={row.key} row={row} rowWidthPx={rowWidthPx} />;
            })}
          </VStack>
        );
      })}
    </VStack>
  );
}

function ReadOnlyMessageView({message, rowWidthPx}: {message: ReadOnlyMessage; rowWidthPx: number}) {
  if (message.role === 'user') {
    const text = message.parts.flatMap(part => (part.type === 'text' ? [part.text] : [])).join('\n\n');
    return text.trim() === '' ? null : (
      <ChatMessage sender="user">
        <ChatMessageBubble>
          <MarkdownText density="compact" contentWidth="100%" headingLevelStart={4} isStreaming={false}>
            {text}
          </MarkdownText>
        </ChatMessageBubble>
      </ChatMessage>
    );
  }
  return <ReadOnlyBody message={message} rowWidthPx={rowWidthPx} />;
}

export function ReadOnlyTranscript({
  messages,
  rowWidthPx,
}: {
  messages: readonly ReadOnlyMessage[];
  /** Width the row titles are cut to; measured from the transcript itself when omitted. */
  rowWidthPx?: number;
}) {
  const [measureRef, measuredWidthPx] = useRowWidth();
  if (messages.length === 0) {
    return null;
  }
  return (
    <VStack gap={3} width="100%" ref={measureRef}>
      {messages.map(message => (
        <ReadOnlyMessageView key={message.key} message={message} rowWidthPx={rowWidthPx ?? measuredWidthPx} />
      ))}
    </VStack>
  );
}
