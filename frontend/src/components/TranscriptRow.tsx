import {Avatar, ChatMessage, ChatMessageBubble, ChatMessageMetadata, ChatSystemMessage, HStack, Icon, Text, VStack} from '@astryxdesign/core';
import {type CSSProperties, memo} from 'react';
import {type MessageRecord, type Session} from '../app-types';
import {deepEqual} from '../deepEqual';
import {userMessageImages} from '../image-attachments';
import {deferredRowStyle, useRowHeightRef} from '../row-defer';
import {AssistantMessage, CopyMessageButton, DeliveryStatus} from './AssistantMessage';
import {SentFiles} from './FileAttachments';
import {SentImages} from './ImageAttachments';
import {MarkdownText} from './MarkdownText';
import {StoredImages} from './StoredImages';

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
    onLoadFull,
    isFrontCompactionSelectable = false,
    isFrontCompactionSelected = false,
    onSelectFrontCompaction,
  }: {
    message: MessageRecord;
    model: Session['model'] | undefined;
    onLoadFull?: (message: MessageRecord) => void | Promise<void>;
    isFrontCompactionSelectable?: boolean;
    isFrontCompactionSelected?: boolean;
    onSelectFrontCompaction?: (message: MessageRecord) => void;
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
            <VStack align="end" gap={0.5}>
              {userImages.sent.length > 0 && <SentImages sources={userImages.sent} />}
              {userImages.stored.length > 0 && <StoredImages names={userImages.stored} />}
              {userImages.files.length > 0 && <SentFiles names={userImages.files} />}
              {userImages.text !== '' && (
                <ChatMessageBubble>
                  <MarkdownText density="compact" contentWidth="100%">{userImages.text}</MarkdownText>
                </ChatMessageBubble>
              )}
              <ChatMessageMetadata
                footer={
                  <HStack align="center" gap={2}>
                    {message.timestamp ? (
                      <Text type="inherit" color="inherit">{message.timestamp}</Text>
                    ) : null}
                    <CopyMessageButton text={userImages.text} noun="message" />
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

    return <AssistantMessage message={message} model={model} onLoadFull={onLoadFull} />;
  },
  // The poll and the session list rebuild messages and models as fresh objects,
  // so fall back to a structural compare when identities differ.
  (previous, next) =>
    (previous.message === next.message || deepEqual(previous.message, next.message)) &&
    (previous.model === next.model || deepEqual(previous.model, next.model)) &&
    previous.isFrontCompactionSelectable === next.isFrontCompactionSelectable &&
    previous.isFrontCompactionSelected === next.isFrontCompactionSelected &&
    previous.onSelectFrontCompaction === next.onSelectFrontCompaction,
);
