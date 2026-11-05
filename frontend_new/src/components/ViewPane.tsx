/**
 * @file ViewPane.tsx
 * @output One conversation pane: a view's transcript, and the composer when this
 * pane is the focused one.
 *
 * A pane is a view (`session` or `session\u0000profile`), so every value here is
 * derived from that view key alone: the transcript window, the row width, the
 * live run's retry and reconnect notices, and the failures that belong to it.
 * The composer arrives as a node because the destructive conversation actions,
 * the model drawer and the draft drawer belong to the focused pane only —
 * `App` renders exactly one of them.
 */

import {ChatLayout, ChatMessage, ChatMessageBubble, ChatMessageList, Heading, HStack, Icon, IconButton, Spinner, Text, VStack, VisuallyHidden} from '@astryxdesign/core';
import {type ResizableRegion, ResizeHandle, useResizable} from '@astryxdesign/core/Resizable';
import {pixel} from '@astryxdesign/core/Resizable/utils';
import {Tooltip} from '@astryxdesign/core/Tooltip';
import {colorVars, spacingVars} from '@astryxdesign/core/theme/tokens.stylex';
import * as stylex from '@stylexjs/stylex';
import {Fragment, useMemo, useRef, useState, type ReactNode} from 'react';
import {type RootLibraryAsset, type RunStreamReconnectState, type SessionPage, type SessionRunFailure, type WebUiMessage} from '../api';
import {type MessageRecord, type RunRetryState, type Session} from '../app-types';
import appConfig from '../config.json';
import {type HistoryBlocks} from '../history-blocks';
import {type AttachedFile, type AttachedImage} from '../image-attachments';
import {paneTitleParts} from '../pane-title';
import {PANE_HEADER_Z_INDEX} from '../panel-layers';
import {conversationPanelAfterToggle, type ConversationPanelId} from '../right-panel';
import {type SlashCommand, type SlashCommandName} from '../slash-commands';
import {deferredRowStyle, useRowHeightRef} from '../row-defer';
import {answersOfCompactedTurns, forkPointMessage} from '../session-fork';
import {profileMessageBusyNote, profileMessageRunLabel} from '../session-views';
import {useActiveTranscript} from '../use-active-transcript';
import {useRowWidth} from '../use-row-width';
import {AssistantIdentity} from './AssistantMessage';
import {type ComposerInsertActions, type ComposerSubmitOptions, ComposerPane} from './ComposerPane';
import {ComposerErrorBoundary} from './ComposerErrorBoundary';
import {JadReadme} from './JadReadme';
import {MarkdownText} from './MarkdownText';
import {PromptLibraryPanel} from './PromptLibraryPanel';
import {ResourceWatchPanel} from './ResourceWatchPanel';
import {ProviderRetryNotice} from './ProviderRetryNotice';
import {RunFailureRow} from './RunFailureRow';
import {RunInspector} from './RunInspector';
import {RunReconnectNotice} from './RunReconnectNotice';
import {StoredImageScopeProvider} from './StoredImages';
import {WebUiParts} from './TranscriptParts';
import {TranscriptRow} from './TranscriptRow';
import {EarlierMessagesControl} from './TranscriptWindow';

const styles = stylex.create({
  pane: {
    // The pane's own floating panel anchors here, not to the shell, so it
    // covers the pane it belongs to and never another one.
    position: 'relative',
    height: '100%',
    minHeight: 0,
    minWidth: 0,
    borderInlineStartWidth: 1,
    borderInlineStartStyle: 'solid',
    // Only the dividers between panes are drawn: the first pane has no border.
    borderInlineStartColor: 'transparent',
  },
  paneDivided: {
    borderInlineStartColor: 'var(--color-border-subtle)',
  },
  // A composer divider spans the composer row, not a one-pixel line.
  composerResizeHandle: {
    alignSelf: 'stretch',
    height: 'auto',
  },
  /**
   * A pane that is mounted but not on screen. A phone shows one conversation at
   * a time, and the others keep their transcript, their run and their scroll
   * position while they wait for their number to be tapped.
   */
  paneHidden: {
    display: 'none',
  },
  panelHost: {
    // The pane's own panel floats over the conversation, not over the pane's
    // header: its label, its running clock and its close button stay reachable
    // while a panel is open.
    position: 'relative',
    flex: '1 1 0',
    minHeight: 0,
    minWidth: 0,
  },
  chat: {
    height: '100%',
    minHeight: 0,
    minWidth: 0,
  },
  conversationFrame: {
    height: '100%',
    minHeight: 0,
    minWidth: 0,
  },
  conversationLane: {
    height: '100%',
    minHeight: 0,
    minWidth: 0,
    // The old visual header used to separate the transcript from the window
    // edge; now that it is gone this keeps the first row off the top edge.
    paddingBlockStart: spacingVars['--spacing-4'],
  },
  assistantBubble: {
    marginBlockStart: spacingVars['--spacing-4'],
  },
  header: {
    flexShrink: 0,
    paddingInline: spacingVars['--spacing-3'],
    paddingBlock: spacingVars['--spacing-1'],
    // The header keeps its place above a panel pinned to the window edge: that
    // panel spans the whole window height, and the pane's own title, its running
    // spinner and its close button must stay visible and clickable through it.
    // Its z-index arrives as an inline style from panel-layers.ts, where the
    // three stacking numbers sit together: Stylex only accepts literals or a
    // *.stylex theme inside stylex.create, and a plain exported number is
    // neither.
    position: 'relative',
  },
  /**
   * The focused pane's strip is tinted, because with several panes on screen the
   * one that owns the composer and the keyboard was only marked by the colour of
   * its label. A grey token rather than a fixed colour keeps it readable in every
   * scheme: it is a shade away from the page in light mode and a shade towards
   * light in dark mode, never a dark block over a light pane.
   */
  headerFocused: {
    backgroundColor: 'var(--color-background-gray)',
  },
  /**
   * The strip while another profile's `profile_message` call holds this pane.
   *
   * The same warm colour the composer's busy status used, at surface strength:
   * `--color-warning` paints the warning glyph at the strip's trailing edge, and
   * `--color-warning-muted` is that colour as a wash, so the label on top stays
   * readable in both schemes. It is applied after `headerFocused`, so a pane that
   * is both focused and busy wears this tone only: one strip, never two tones.
   */
  headerProfileBusy: {
    backgroundColor: colorVars['--color-warning-muted'],
  },
  titleRow: {
    flex: '1 1 auto',
    minWidth: 0,
    overflow: 'hidden',
  },
  titlePart: {
    minWidth: 0,
  },
});

export type ViewPaneProps = {
  /** The view this pane shows: `session` or `session\u0000profile`. */
  viewKey: string;
  /** What the pane is called: the session, and the profile when pinned. */
  label: string;
  /** The row's record: the pinned row's own, or the session itself. Absent until it loads. */
  session: Session | undefined;
  profile: string | undefined;
  isLive: boolean;
  isRunning: boolean;
  /** The profile whose `profile_message` call holds this view's profile, when one does. */
  profileMessageCaller?: string | undefined;
  /** Running without a pending interaction: deltas still arrive. */
  isStreaming: boolean;
  /** The run asked for a draft answer and waits for it. */
  pendingDraft: boolean;
  /** The reasoning effort the run uses, as the model drawer shows it. */
  effort: string | undefined;
  /** The provider the next run's model comes from, for the title strip. */
  provider: string | undefined;
  /** The model the next run uses, for the title strip. */
  model: string | undefined;
  /** The reasoning effort the next run uses, for the title strip. */
  effortLabel: string | undefined;
  hasLiveAttachedRun: boolean;
  isFocused: boolean;
  isReadmeShown: boolean;
  /** False when the pane is mounted but not on screen (a phone shows one pane). */
  isHidden?: boolean;
  /** The divider before this pane; false for the leftmost pane. */
  isDivided: boolean;
  /** The focused pane's composer. Other panes show no composer. */
  composer?: ReactNode;
  /** The session reference the composer's session-scoped features use. */
  composerSessionId: string;
  onSubmitComposer: (
    value: string,
    images: AttachedImage[],
    files: AttachedFile[],
    options: ComposerSubmitOptions,
  ) => boolean;
  onStopComposer: () => void;
  isComposerDisabled: boolean;
  placeholder: string;
  /** The panel this pane starts with, taken from the configured defaults. */
  defaultPanel: ConversationPanelId | null;
  /** Where this pane's own panel width persists. */
  panelStorageId: string;
  /**
   * True when this pane is the only conversation on screen: its panel then takes
   * the window's right edge, the way the single-pane app anchors it, instead of
   * the pane's edge inside the centred lane.
   */
  isPanelAtWindowEdge: boolean;
  onUsePrompt: (asset: RootLibraryAsset) => void;
  onUseTemplate: (asset: RootLibraryAsset) => void;
  onUseSkill: (asset: RootLibraryAsset) => void;
  onRegisterComposerInsert: (viewKey: string, insert: ComposerInsertActions | null) => void;
  slashCommands: readonly SlashCommand[] | undefined;
  onSlashCommand: (name: SlashCommandName) => void;
  onSlashError: (message: string) => void;
  /** The drawers and the handler toolbar: the focused pane's own. */
  composerChrome?: ReactNode;
  /**
   * Present only when this is the single pane on screen: then the composer is
   * the thing that carries the width, centred in the full-width pane with a
   * divider on each side — the transcript keeps the whole window, so its
   * scrollbar reaches the outer edge instead of stopping at a lane.
   */
  composerLane?: {widthPx: number; resize: ResizableRegion['props']};
  composerToolbar?: ReactNode;
  composerHeaderActions?: ReactNode;
  composerFooter?: ReactNode;
  onComposerError: (error: unknown) => void;
  /** The session list's own header strip; shown once more than one pane is open. */
  showHeader: boolean;
  threads: Readonly<Record<string, MessageRecord[]>>;
  historyBlocks: HistoryBlocks<MessageRecord>;
  failures: readonly SessionRunFailure[] | undefined;
  streamingMessage: WebUiMessage | null;
  reconnect: RunStreamReconnectState | undefined;
  retry: RunRetryState | undefined;
  page: SessionPage | undefined;
  isFrontCompactionSelectionMode: boolean;
  isFrontCompactionConfirmationOpen: boolean;
  frontCompactionTargetMessageId: string | undefined;
  onSelectFrontCompaction: ((message: MessageRecord) => void) | undefined;
  /** Forks the conversation before a user message; absent while forking is not possible. */
  onForkMessage?: ((message: MessageRecord, keepTurn: boolean) => void) | undefined;
  onLoadFull: (message: MessageRecord) => void;
  /** Fetches the turns before the oldest one this client holds, from the server. */
  onLoadOlderTurns: () => void;
  onFocus: () => void;
  onClose: (() => void) | undefined;
};

/**
 * The pane: a header strip when several panes share the screen, then the
 * transcript, then the composer dock.
 */
export function ViewPane({
  viewKey,
  label,
  session,
  profile,
  isLive,
  isRunning,
  profileMessageCaller,
  isStreaming,
  pendingDraft,
  effort,
  provider,
  model,
  effortLabel,
  hasLiveAttachedRun,
  isFocused,
  isReadmeShown,
  isHidden,
  isDivided,
  composerSessionId,
  onSubmitComposer,
  onStopComposer,
  isComposerDisabled,
  placeholder,
  onUsePrompt,
  onUseTemplate,
  onUseSkill,
  defaultPanel,
  panelStorageId,
  isPanelAtWindowEdge,
  onRegisterComposerInsert,
  slashCommands,
  onSlashCommand,
  onSlashError,
  composerChrome,
  composerLane,
  composerToolbar,
  composerHeaderActions,
  composerFooter,
  onComposerError,
  showHeader,
  threads,
  historyBlocks,
  failures,
  streamingMessage,
  reconnect,
  retry,
  page,
  isFrontCompactionSelectionMode,
  isFrontCompactionConfirmationOpen,
  frontCompactionTargetMessageId,
  onSelectFrontCompaction,
  onForkMessage,
  onLoadFull,
  onLoadOlderTurns,
  onFocus,
  onClose,
}: ViewPaneProps) {
  const [liveBubbleRef, liveBubbleWidthPx] = useRowWidth();
  const liveRowHeightRef = useRowHeightRef(null);
  const {
    inputHistory,
    visibleMessages,
    hiddenCount,
    revealEarlier,
    placedFailures,
    streamingMessageCommitted,
    liveToolParts,
  } = useActiveTranscript<MessageRecord>({
    threads,
    historyBlocks,
    viewKey,
    hasLiveAttachedRun,
    failures,
    isRunning,
    streamingMessage,
  });
  // The rows keep one fork handler while messages come and go; it reads the list when used.
  const forkMessagesRef = useRef(visibleMessages);
  forkMessagesRef.current = visibleMessages;
  const forkMessage = useMemo(() => onForkMessage === undefined
    ? undefined
    : (message: MessageRecord, keepTurn: boolean) =>
      onForkMessage(forkPointMessage(forkMessagesRef.current, message, keepTurn), keepTurn),
  [onForkMessage]);
  // Answers of the compacted history get no "Fork after this turn": the server cannot
  // find their turn.
  const unforkableAnswers = useMemo(() => answersOfCompactedTurns(visibleMessages), [visibleMessages]);
  /**
   * This pane's own right-edge panel.
   *
   * The run inspector and the prompt library describe one conversation's run, so
   * each pane keeps its own: the panel is an overlay inside the pane, it opens
   * and closes on that pane's composer toggle, and its width is its own.
   */
  const [panel, setPanel] = useState<ConversationPanelId | null>(defaultPanel);
  const panelRegion = useResizable({
    defaultSize: pixel(appConfig.inspector_default_size),
    minSize: pixel(appConfig.inspector_min_size),
    maxSize: pixel(appConfig.inspector_max_size),
    autoSaveId: panelStorageId,
  });
  const togglePanel = (target: ConversationPanelId) => {
    setPanel(current => conversationPanelAfterToggle(current, target));
  };
  /**
   * This pane's composer. `ComposerPane` owns its input and keys drafts by
   * `draftKey`, so one per pane is what makes four panes typable at once; the
   * drawers and the handler toolbar arrive only for the focused pane.
   */
  const composerNode = (
    <ComposerErrorBoundary onError={onComposerError}>
      <ComposerPane
        activeSessionId={composerSessionId}
        activeProfile={profile}
        draftKey={viewKey}
        inputHistory={inputHistory}
        onSubmit={onSubmitComposer}
        onStop={onStopComposer}
        isRunning={isRunning}
        isDisabled={isComposerDisabled}
        placeholder={placeholder}
        showInspector={panel === 'inspector'}
        onToggleInspector={() => togglePanel('inspector')}
        isPromptLibraryOpen={panel === 'prompts'}
        onTogglePromptLibrary={() => togglePanel('prompts')}
        isResourcePanelOpen={panel === 'resources'}
        onToggleResourcePanel={appConfig.resource_watch_enabled ? () => togglePanel('resources') : undefined}
        onRegisterInsert={insert => onRegisterComposerInsert(viewKey, insert)}
        slashCommands={slashCommands}
        onSlashCommand={onSlashCommand}
        onSlashError={onSlashError}
        drawer={composerChrome}
        toolbarActions={composerToolbar}
        headerContextActions={composerHeaderActions}
        footerNote={composerFooter}
      />
    </ComposerErrorBoundary>
  );
  // One pane on screen: the composer is the resizable thing, centred in the
  // pane with a divider on each side, and the transcript around it keeps the
  // whole window. The lane used to carry the width, which put the transcript's
  // scrollbar at the lane's edge instead of the window's.
  const composerBlock =
    composerLane === undefined ? (
      composerNode
    ) : (
      <HStack align="stretch" justify="center" width="100%" gap={0}>
        <ResizeHandle
          direction="horizontal"
          isReversed
          hasDivider={false}
          xstyle={styles.composerResizeHandle}
          // The grab zone is wider than the line and sits on the far side of it,
          // over the lane's edge and never over the composer's own controls.
          pillPlacement="start"
          label="Resize conversation width from left border"
          resizable={composerLane.resize}
        />
        <VStack gap={0} width={composerLane.widthPx} maxWidth="100%" style={{minWidth: 0}}>
          {composerNode}
        </VStack>
        <ResizeHandle
          direction="horizontal"
          hasDivider={false}
          xstyle={styles.composerResizeHandle}
          pillPlacement="end"
          label="Resize conversation width from right border"
          resizable={composerLane.resize}
        />
      </HStack>
    );
  const sessionRef = session?.sessionRef;
  const storedImageScope = sessionRef === undefined ? undefined : {sessionRef, profile};
  const liveImageScope = storedImageScope === undefined ? undefined : {...storedImageScope, live: true};
  const hasOlderTurns = isLive && page?.has_more === true && page.next_before !== null;
  // The strip's own name, and the detail the tooltip holds: the profile the pane
  // is pinned to and the provider, model and effort its next run uses.
  const titleParts = paneTitleParts({
    sessionTitle: session?.title ?? label,
    profile,
    provider,
    model,
    effort: effortLabel,
  });
  const sessionLabel = titleParts[0]?.label ?? label;
  const detailText = titleParts.slice(1).map(part => part.label).join(' · ');

  return (
    <VStack
      gap={0}
      height="100%"
      width="100%"
      xstyle={[styles.pane, isDivided ? styles.paneDivided : undefined, isHidden ? styles.paneHidden : undefined]}
      style={{flex: '1 1 0', minWidth: 0}}
      onClick={onFocus}
      role="group"
      aria-label={`Conversation pane: ${label}`}
      data-pane-focused={isFocused ? 'true' : 'false'}
      data-pane-scope=""
      data-pane-key={viewKey}
    >
      {/*
        The composer used to say this in its text status line, which was a
        `role="status"` live region. The header tint and the warning icon in it
        are visual only, so the announcement moves here: it states the same
        thing, unseen, and is announced when it appears.
      */}
      {profileMessageCaller === undefined ? null : (
        <VisuallyHidden as="div" role="status">
          {profileMessageBusyNote(profileMessageCaller)}
        </VisuallyHidden>
      )}
      {showHeader && (
        <HStack
          align="center"
          justify="between"
          gap={2}
          xstyle={[
            styles.header,
            isFocused ? styles.headerFocused : undefined,
            profileMessageCaller === undefined ? undefined : styles.headerProfileBusy,
          ]}
          style={{zIndex: PANE_HEADER_Z_INDEX}}
        >
          {/*
            The strip names the conversation alone: the profile it is pinned to
            and the provider, model and effort its next run uses sit in the
            tooltip, so a narrow pane still shows its own name in full.
          */}
          <Tooltip content={detailText} placement="below" isEnabled={detailText !== ''}>
            <HStack align="center" gap={1} xstyle={styles.titleRow} data-pane-title="">
              <Text
                type="supporting"
                color={isFocused ? 'primary' : 'secondary'}
                maxLines={1}
                xstyle={styles.titlePart}
              >
                {sessionLabel}
              </Text>
              {isReadmeShown ? null : (
                <VisuallyHidden as="div">
                  <Heading level={1}>{session?.title ?? label}</Heading>
                </VisuallyHidden>
              )}
            </HStack>
          </Tooltip>
          <HStack align="center" gap={1}>
            {/*
              The busy mark leads the strip's trailing group, ahead of the
              running spinner and the close button: hovering it states who holds
              the profile. The strip's warm tint is the at-a-glance signal; this
              says it in words for anyone who looks closer, and the
              `role="status"` line above the header is what a screen reader
              hears. `label` makes it a standalone status glyph rather than a
              decorative icon, per the component's own docs.
            */}
            {profileMessageCaller === undefined ? null : (
              <Tooltip content={profileMessageBusyNote(profileMessageCaller)} placement="below">
                <Icon
                  icon="warning"
                  color="warning"
                  size="sm"
                  label={profileMessageBusyNote(profileMessageCaller)}
                />
              </Tooltip>
            )}
            {isRunning ? (
              profileMessageCaller === undefined ? (
                <Spinner size="sm" aria-label={`${label} running`} />
              ) : (
                <Tooltip content={profileMessageRunLabel(profileMessageCaller)} placement="below">
                  <Spinner size="sm" aria-label={`${label}: ${profileMessageRunLabel(profileMessageCaller)}`} />
                </Tooltip>
              )
            ) : null}
            {onClose === undefined ? null : (
              <IconButton
                label="Close pane"
                tooltip="Close this conversation pane"
                icon={<Icon icon="close" size="sm" />}
                variant="ghost"
                size="sm"
                onClick={onClose}
              />
            )}
          </HStack>
        </HStack>
      )}
      <VStack gap={0} width="100%" xstyle={styles.panelHost}>
      <ChatLayout density="balanced" xstyle={styles.chat} composer={composerBlock}>
        <HStack
          align="stretch"
          justify="center"
          gap={0}
          width="100%"
          height="100%"
          xstyle={styles.conversationFrame}
        >
          {/* The scroll area is the whole pane, so the scrollbar stays on the
              window's edge; the column inside it is the composer's width, so the
              conversation follows the divider instead of floating at 100%. */}
          <VStack
            width={composerLane === undefined ? '100%' : composerLane.widthPx}
            maxWidth="100%"
            height="100%"
            xstyle={styles.conversationLane}
          >
            {isReadmeShown ? (
              <JadReadme />
            ) : (
              <ChatMessageList
                density="balanced"
                align="top"
                isStreaming={isStreaming}
              >
                {(hiddenCount > 0 || hasOlderTurns) && (
                  <EarlierMessagesControl
                    hiddenCount={hiddenCount}
                    hasMore={hasOlderTurns}
                    onReveal={node => {
                      if (hiddenCount > 0) {
                        revealEarlier(node);
                        return;
                      }
                      // Nothing loaded is hidden any more: the rest of the
                      // transcript has to be fetched before it can be revealed.
                      onLoadOlderTurns();
                    }}
                  />
                )}
                {visibleMessages.map(message => (
                  <Fragment key={message.id}>
                    <TranscriptRow
                      message={message}
                      model={session?.model}
                      effort={effort}
                      onLoadFull={onLoadFull}
                      isFrontCompactionSelectable={isFrontCompactionSelectionMode}
                      isFrontCompactionSelected={
                        isFrontCompactionConfirmationOpen &&
                        frontCompactionTargetMessageId === message.id
                      }
                      onSelectFrontCompaction={isFrontCompactionSelectionMode
                        ? onSelectFrontCompaction
                        : undefined}
                      onFork={isFrontCompactionSelectionMode || unforkableAnswers.has(message.id) ? undefined : forkMessage}
                    />
                    {placedFailures.byMessageId.get(message.id)?.map(failure => (
                      <RunFailureRow key={failure.turn_id} failure={failure} />
                    ))}
                  </Fragment>
                ))}
                {placedFailures.trailing.map(failure => (
                  <RunFailureRow key={failure.turn_id} failure={failure} />
                ))}
                {isRunning &&
                  (!isLive || streamingMessage !== null || hasLiveAttachedRun) &&
                  !streamingMessageCommitted && (
                  <ChatMessage
                    sender="assistant"
                    ref={liveRowHeightRef}
                    style={deferredRowStyle(null)}
                  >
                    <ChatMessageBubble
                      ref={liveBubbleRef}
                      variant="ghost"
                      width="100%"
                      xstyle={styles.assistantBubble}
                      name={
                        <AssistantIdentity
                          model={session?.model}
                          effort={effort}
                          isRunning={isStreaming}
                        />
                      }
                    >
                      {isLive && streamingMessage ? (
                        <StoredImageScopeProvider value={liveImageScope}>
                          <WebUiParts
                            parts={streamingMessage.parts}
                            steps={streamingMessage.steps}
                            rowWidthPx={liveBubbleWidthPx}
                            isStreaming={isStreaming}
                          />
                        </StoredImageScopeProvider>
                      ) : (
                        <VStack gap={3} width="100%">
                          <MarkdownText
                            density="compact"
                            contentWidth="100%"
                            isStreaming={isStreaming}
                            headingLevelStart={3}
                          >
                            {pendingDraft
                              ? 'Astra is waiting for your answer…'
                              : 'Astra is working through the request…'}
                          </MarkdownText>
                        </VStack>
                      )}
                    </ChatMessageBubble>
                  </ChatMessage>
                )}
                {isRunning && reconnect !== undefined && (
                  <RunReconnectNotice state={reconnect} />
                )}
                {isRunning && retry !== undefined && (
                  <ProviderRetryNotice wait={retry.wait} tickMs={appConfig.provider_retry_tick_ms} />
                )}
              </ChatMessageList>
            )}
          </VStack>
        </HStack>
      </ChatLayout>
      {/* The pane's own right-edge panel: an overlay inside this pane, so four
          panes can show four different inspectors. */}
      {panel === 'inspector' && session !== undefined && (
        <RunInspector
          activeSession={session}
          liveToolParts={liveToolParts}
          inspector={panelRegion}
          isRunning={isRunning}
          isEdgeFixed={isPanelAtWindowEdge}
          onClose={() => setPanel(null)}
        />
      )}
      {panel === 'prompts' && (
        <PromptLibraryPanel
          region={panelRegion}
          isEdgeFixed={isPanelAtWindowEdge}
          onClose={() => setPanel(null)}
          onUsePrompt={onUsePrompt}
          onUseTemplate={onUseTemplate}
          onUseSkill={onUseSkill}
        />
      )}
      {appConfig.resource_watch_enabled && panel === 'resources' && (
        <ResourceWatchPanel
          sessionId={composerSessionId === '' ? undefined : composerSessionId}
          profile={profile}
          region={panelRegion}
          isEdgeFixed={isPanelAtWindowEdge}
          onClose={() => setPanel(null)}
        />
      )}
      </VStack>
    </VStack>
  );
}
