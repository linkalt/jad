import {Avatar, ChatMessage, ChatMessageBubble, ChatMessageMetadata, ChatToolCalls, HStack, Icon, IconButton, StatusDot, Text, VStack} from '@astryxdesign/core';
import {spacingVars} from '@astryxdesign/core/theme/tokens.stylex';
import * as stylex from '@stylexjs/stylex';
import {type MessageRecord, type Session} from '../app-types';
import appConfig from '../config.json';
import {copyActionLabel, useCopyToClipboard} from '../copy-to-clipboard';
import {deferredRowStyle, useRowHeightRef} from '../row-defer';
import {useRowWidth} from '../use-row-width';
import {ForkIcon} from './AppIcons';
import {MarkdownText} from './MarkdownText';
import {ReasoningBlock, type WebUiContentPart, WebUiParts} from './TranscriptParts';

const styles = stylex.create({
  // The assistant name row carries a negative block margin of its own, so the
  // bubble contributes the rest of the gap between the identity row and the
  // message body.
  assistantBubble: {
    marginBlockStart: spacingVars['--spacing-4'],
  },
  assistantIdentity: {
    minWidth: 0,
    maxWidth: '100%',
  },
  assistantModel: {
    minWidth: 0,
    maxWidth: '100%',
    flexShrink: 1,
    overflow: 'hidden',
    textOverflow: 'ellipsis',
    whiteSpace: 'nowrap',
  },
});

/**
 * The Astra avatar, sized to the identity line.
 *
 * `ChatMessageBubble` renders its `name` slot at a fixed `--spacing-5` (20px)
 * height, and that row is the first box of the message, so the row's top edge
 * is the row's — and its deferred row's — own top edge. An avatar taller than
 * the row overflows above that edge and has its top clipped, which reads as a
 * flat-topped, not-quite-round circle. `xsm` is the same 20px, so the circle
 * fits the line instead of crossing it.
 */
function AgentAvatar({isRunning = false}: {isRunning?: boolean}) {
  return (
    <Avatar
      name="Astra"
      size="xsm"
      status={
        isRunning ? <StatusDot variant="accent" label="Working" isPulsing /> : undefined
      }
    />
  );
}

/**
 * The sender identity line: the Astra avatar, then the provider and the model.
 *
 * The avatar belongs to this line, not to `ChatMessage`'s `avatar` slot: that
 * slot reserves a column as wide as the avatar plus the root gap, so every
 * assistant row — label and body — is indented from the transcript edge. On
 * this line the avatar adds no indent of its own, and the live row keeps its
 * "working" dot.
 */
export function AssistantIdentity({
  model,
  effort,
  isRunning = false,
}: {
  model?: Session['model'];
  effort?: string;
  isRunning?: boolean;
}) {
  const providerName = model?.provider?.trim() || 'provider_name';
  const modelId = model?.model?.trim() || 'model_id';

  return (
    <HStack align="center" gap={2} xstyle={styles.assistantIdentity}>
      <AgentAvatar isRunning={isRunning} />
      <Text type="label" weight="semibold">{providerName}</Text>
      <Text type="supporting" color="secondary" xstyle={styles.assistantModel}>
        {modelId}
      </Text>
      {effort ? <Text type="supporting" color="secondary">{effort}</Text> : null}
    </HStack>
  );
}

function assistantOutputMarkdown(message: MessageRecord): string {
  if (message.text.length > 0 || !message.parts) {
    return message.text;
  }
  return message.parts
    .filter((part): part is WebUiContentPart => part.type === 'text')
    .map(part => part.text)
    .join('');
}

/** The copy icon on a message row: `noun` finishes "Copy …", e.g. "as Markdown". */
export function CopyMessageButton({text, noun}: {text: string; noun: string}) {
  const [copyState, copy] = useCopyToClipboard(text);
  const label = copyActionLabel(copyState, noun);
  return (
    <IconButton
      label={label}
      tooltip={label}
      icon={
        <Icon
          icon={copyState === 'copied' ? 'check' : copyState === 'error' ? 'error' : 'copy'}
          size="xsm"
          color="secondary"
        />
      }
      variant="ghost"
      size="sm"
      isDisabled={text.length === 0}
      onClick={copy}
    />
  );
}

/**
 * Delivery status on a message row.
 *
 * Astryx's `ChatMessageMetadata` separates its slots — and prefixes its
 * `status` slot — with a `·`, so a row that fills more than one slot can't be
 * separator-free. Both rows here put their whole line in the `footer` slot,
 * which the component renders verbatim: `6:16 PM  📋 ✓✓ Delivered` on an
 * assistant row, `✓✓ Read` on a user row.
 */
export function DeliveryStatus({status}: {status: 'read' | 'delivered'}) {
  const label = status === 'read' ? 'Read' : 'Delivered';
  return (
    <HStack
      align="center"
      gap={1}
      aria-label={`Message ${label.toLowerCase()}`}
    >
      <Icon icon="checkDouble" size="xsm" color="inherit" />
      <Text type="inherit" color="inherit">{label}</Text>
    </HStack>
  );
}

export function AssistantMessage({
  message,
  model,
  effort,
  onLoadFull,
  onForkAfter,
}: {
  message: MessageRecord;
  model?: Session['model'];
  effort?: string;
  onLoadFull?: (message: MessageRecord) => void | Promise<void>;
  /** Forks the conversation through the end of this message's turn. */
  onForkAfter?: () => void;
}) {
  const [rowRef, rowWidthPx] = useRowWidth();
  const rowHeightRef = useRowHeightRef(message.id);
  const markdown = assistantOutputMarkdown(message);

  return (
    <ChatMessage
      sender="assistant"
      ref={rowHeightRef}
      style={deferredRowStyle(message.id)}
    >
      <ChatMessageBubble
        ref={rowRef}
        variant="ghost"
        width="100%"
        xstyle={styles.assistantBubble}
        name={<AssistantIdentity model={model} effort={effort} />}
        metadata={
          <ChatMessageMetadata
            footer={
              <HStack align="center" gap={2}>
                {message.timestamp ? (
                  <Text type="inherit" color="inherit">{message.timestamp}</Text>
                ) : null}
                <CopyMessageButton text={markdown} noun="as Markdown" />
                {onForkAfter !== undefined ? (
                  <IconButton
                    label={appConfig.fork_after_message_label}
                    tooltip={`${appConfig.fork_after_message_label}: a new profile of this session with the conversation through the end of this turn`}
                    icon={<Icon icon={ForkIcon} size="xsm" color="secondary" />}
                    variant="ghost"
                    size="sm"
                    onClick={onForkAfter}
                  />
                ) : null}
                <DeliveryStatus status="delivered" />
              </HStack>
            }
          />
        }
      >
        <VStack gap={3} width="100%">
          {message.parts && message.parts.length > 0 ? (
            <WebUiParts
              parts={message.parts}
              steps={message.steps}
              scope={message.id}
              rowWidthPx={rowWidthPx}
              source={onLoadFull === undefined ? undefined : {message, onLoadFull}}
            />
          ) : (
            <>
              {(message.reasoning || message.reasoningContent) && (
                <ReasoningBlock
                  steps={message.reasoning ?? []}
                  content={message.reasoningContent}
                  activityKey={`${message.id}:reasoning`}
                  rowWidthPx={rowWidthPx}
                  isFlush
                />
              )}
              <MarkdownText density="compact" contentWidth="100%" headingLevelStart={3}>
                {message.text}
              </MarkdownText>
            </>
          )}
          {message.tools && message.tools.length > 0 && (
            <ChatToolCalls calls={message.tools} defaultIsExpanded />
          )}
        </VStack>
      </ChatMessageBubble>
    </ChatMessage>
  );
}
