import {Avatar, ChatMessage, ChatMessageBubble, ChatMessageMetadata, ChatSystemMessage, HStack, Icon, IconButton, Text, VStack} from '@astryxdesign/core';
import {type CSSProperties, memo} from 'react';
import {type MessageRecord, type Session} from '../app-types';
import appConfig from '../config.json';
import {deepEqual} from '../deepEqual';
import {userMessageImages} from '../image-attachments';
import {deferredRowStyle, useRowHeightRef} from '../row-defer';
import {AssistantMessage, CopyMessageButton, DeliveryStatus} from './AssistantMessage';
import {ForkIcon} from './AppIcons';
import {SentFiles} from './FileAttachments';
import {SentImages} from './ImageAttachments';
import {MarkdownText} from './MarkdownText';
import {StoredImages} from './StoredImages';

/**
 * The width cap for a user bubble, in CSS terms.
 *
 * Astryx caps a bubble at 80% of the column it sits in. That leaves the bubble
 * too narrow to read once the column itself is small — a phone pane — so below
 * the configured minimum the cap becomes the whole column, which is how the
 * assistant rows are drawn:
 *
 * - column wider than 1.25 × minimum → 80% of it, unchanged;
 * - column between the minimum and 1.25 × it → exactly the minimum;
 * - column no wider than the minimum → 100%, the full block.
 */
const userBubbleStyle: CSSProperties = {
  maxWidth: `min(100%, max(80%, ${appConfig.user_bubble_min_width}px))`,
};

export function isActiveTranscriptUserMessage(message: MessageRecord): boolean {
  return message.sender === 'user' && /^session-user-o(0|[1-9][0-9]*)$/.test(message.id);
}

/**
 * One transcript row, memoised.
 *
 * A few hundred turns render tens of thousands of DOM nodes, and every App
 * state change — an outside click, a panel toggle, the session poll — used to
 * rebuild all of them, because the rows were created inline during App's render.
 * Only these two props drive a row's output, so React can skip the subtree
 * whenever they are unchanged.
 */
export const TranscriptRow = memo(
  function TranscriptRow({
    message,
    model,
    effort,
    onLoadFull,
    isFrontCompactionSelectable = false,
    isFrontCompactionSelected = false,
    onSelectFrontCompaction,
    onFork,
  }: {
    message: MessageRecord;
    model: Session['model'] | undefined;
    effort?: string;
    onLoadFull?: (message: MessageRecord) => void | Promise<void>;
    isFrontCompactionSelectable?: boolean;
    isFrontCompactionSelected?: boolean;
    onSelectFrontCompaction?: (message: MessageRecord) => void;
    /** Forks the conversation before this message; only a message of the live transcript can be. */
    /** Forks the conversation at the message: before it (keepTurn false) or after its turn. */
    onFork?: (message: MessageRecord, keepTurn: boolean) => void;
  }) {
    const rowHeightRef = useRowHeightRef(
      message.sender === 'assistant' ? null : message.id,
    );
    if (message.sender === 'system') {
      return (
        <ChatSystemMessage
          variant="divider"
          icon={<Icon icon="check" color="success" size="sm" />}
          ref={rowHeightRef}
          style={deferredRowStyle(message.id)}
        >
          {message.text}
        </ChatSystemMessage>
      );
    }

    if (message.sender === 'user') {
      const isSelectable =
        isFrontCompactionSelectable &&
        isActiveTranscriptUserMessage(message) &&
        onSelectFrontCompaction !== undefined;
      const isSelected =
        isFrontCompactionSelected && isActiveTranscriptUserMessage(message);
      const selectionStyle: CSSProperties | undefined = isSelectable
        ? {
            backgroundColor: 'var(--color-background-blue)',
            border: 'var(--border-width) solid var(--color-border-blue)',
            borderRadius: 'var(--radius-container)',
            cursor: 'pointer',
            padding: 'var(--spacing-2)',
          }
        : isSelected
          ? {
              backgroundColor: 'var(--color-background-cyan)',
              border: 'var(--border-width) solid var(--color-border-blue)',
              borderRadius: 'var(--radius-container)',
              padding: 'var(--spacing-2)',
            }
          : undefined;
      const rowStyle = selectionStyle === undefined
        ? deferredRowStyle(message.id)
        : {...deferredRowStyle(message.id), ...selectionStyle};

      const userImages = userMessageImages(message);

      return (
        <ChatMessage
          sender="user"
          ref={rowHeightRef}
          style={rowStyle}
          role={isSelectable ? 'group' : undefined}
          tabIndex={isSelectable ? 0 : undefined}
          aria-description={isSelectable
            ? 'Selectable user message. Press Enter or Space to review deleting this provider run and later active messages.'
            : undefined}
          onClick={isSelectable ? event => {
            const target = event.target;
            if (target instanceof Element && target.closest('a, button, input, textarea, select')) {
              return;
            }
            if (window.getSelection()?.toString()) return;
            onSelectFrontCompaction?.(message);
          } : undefined}
          onKeyDown={isSelectable ? event => {
            if (event.target !== event.currentTarget || (event.key !== 'Enter' && event.key !== ' ')) {
              return;
            }
            event.preventDefault();
            onSelectFrontCompaction?.(message);
          } : undefined}
        >
          <HStack align="start" gap={1} justify="end" width="100%">
            {/* The bubble's cap is `max(80%, 280px)` of this column. A
                shrink-to-fit column makes that a circular percentage: it settles
                once and then never grows, so the bubble kept the width of the
                pane it was first laid out in. A definite width gives the cap a
                basis that follows the row. */}
            <VStack align="end" gap={0.5} style={{flex: '1 1 0%', minWidth: 0}}>
              {userImages.sent.length > 0 && <SentImages sources={userImages.sent} />}
              {userImages.stored.length > 0 && <StoredImages names={userImages.stored} />}
              {userImages.files.length > 0 && <SentFiles names={userImages.files} />}
              {userImages.text !== '' && (
                <ChatMessageBubble style={userBubbleStyle}>
                  <MarkdownText density="compact" contentWidth="100%" isStreaming={false}>{userImages.text}</MarkdownText>
                </ChatMessageBubble>
              )}
              <ChatMessageMetadata
                footer={
                  <HStack align="center" gap={2}>
                    {message.timestamp ? (
                      <Text type="inherit" color="inherit">{message.timestamp}</Text>
                    ) : null}
                    <CopyMessageButton text={userImages.text} noun="message" />
                    {onFork !== undefined && isActiveTranscriptUserMessage(message) ? (
                      <IconButton
                        label={appConfig.fork_message_label}
                        tooltip={`${appConfig.fork_message_label}: a new profile of this session with the conversation before this message`}
                        icon={<Icon icon={ForkIcon} size="xsm" color="secondary" />}
                        variant="ghost"
                        size="sm"
                        onClick={() => onFork(message, false)}
                      />
                    ) : null}
                    <DeliveryStatus status="read" />
                  </HStack>
                }
              />
            </VStack>
            <Avatar name="You" size="sm" />
          </HStack>
        </ChatMessage>
      );
    }

    return (
      <AssistantMessage
        message={message}
        model={model}
        effort={effort}
        onLoadFull={onLoadFull}
        onForkAfter={onFork === undefined || message.status === 'streaming' ? undefined : () => onFork(message, true)}
      />
    );
  },
  // The poll and the session list rebuild messages and models as fresh objects,
  // so fall back to a structural compare when identities differ.
  (previous, next) =>
    (previous.message === next.message || deepEqual(previous.message, next.message)) &&
    (previous.model === next.model || deepEqual(previous.model, next.model)) &&
    previous.effort === next.effort &&
    previous.isFrontCompactionSelectable === next.isFrontCompactionSelectable &&
    previous.isFrontCompactionSelected === next.isFrontCompactionSelected &&
    previous.onSelectFrontCompaction === next.onSelectFrontCompaction &&
    previous.onFork === next.onFork,
);
