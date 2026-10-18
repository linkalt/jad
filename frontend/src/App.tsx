import {AppShell, Button, ChatLayout, ChatMessage, ChatMessageBubble, ChatMessageList, HStack, Heading, Icon, IconButton, Layout, LayoutContent, Text, VStack, VisuallyHidden} from '@astryxdesign/core';
import {ChatComposerDrawer} from '@astryxdesign/core/Chat';
import {ResizeHandle, useResizable} from '@astryxdesign/core/Resizable';
import {pixel} from '@astryxdesign/core/Resizable/utils';
import {useToast} from '@astryxdesign/core/Toast';
import {colorVars, spacingVars} from '@astryxdesign/core/theme/tokens.stylex';
import * as stylex from '@stylexjs/stylex';
import {Fragment, Suspense, lazy, useCallback, useEffect, useMemo, useRef, useState} from 'react';
import {type ActivityNotification, type NewActivityNotification, hasUnreadActivityError, isRunOutcomeUnknown, markActivityNotificationsRead, prependActivityNotification, runOutcomeUnknownNotification, unreadActivityNotificationCount} from './activity-notifications';
import {type RootLibraryAsset, type RunStreamReconnectState, type SessionPage, type SessionRunFailure, type SessionSnapshot, type WebUiInteraction, type WebUiMessage, type WebUiPart, answerSessionRunInteraction, attachSessionRun, compactSession, extendSessionRunInteraction, fetchSession, fetchSessionHistory, fetchSessionMessage, fetchSessions, frontCompactSessionFromMessage, resetSession, stopSessionRun, streamSessionRun, writeSessionStateContext} from './api';
import {type InteractionUiState, type MessageRecord, type RunInteractionState, type RunRetryState, type Session} from './app-types';
import {interactionFailureTitle} from './approval-answer';
import {createCoalescedRefresh} from './coalesced-refresh';
import {type ColorScheme} from './color-scheme';
import {FrontCompactionIcon, ResetChatIcon, StateContextIcon} from './components/AppIcons';
import {ApprovalDrawer} from './components/ApprovalDrawer';
import {AssistantIdentity} from './components/AssistantMessage';
import {CheckpointPanel} from './components/CheckpointPanel';
import {ComposerErrorBoundary} from './components/ComposerErrorBoundary';
import {type ComposerInsertActions, ComposerPane} from './components/ComposerPane';
import {DraftRequestDrawer} from './components/DraftRequestDrawer';
import {JadReadme} from './components/JadReadme';
import {LazyLoadBoundary} from './components/LazyLoadBoundary';
import {MarkdownText} from './components/MarkdownText';
import {ModelDrawer} from './components/ModelDrawer';
import {NotificationPanel} from './components/NotificationPanel';
import {PromptLibraryPanel} from './components/PromptLibraryPanel';
import {ProviderRetryNotice} from './components/ProviderRetryNotice';
import {RunFailureRow} from './components/RunFailureRow';
import {RunInspector} from './components/RunInspector';
import {RunReconnectNotice} from './components/RunReconnectNotice';
import {RunSettingsPanel} from './components/RunSettingsPanel';
import {type QuoteAnchor, SelectionMenu} from './components/SelectionMenu';
import {type LeftPanelId, SessionNavigation} from './components/SessionNavigation';
import {SessionSearchPanel} from './components/SessionSearchPanel';
import {StateContextDrawer} from './components/StateContextDrawer';
import {StoredImageScopeProvider} from './components/StoredImages';
import {TokenRequestDialog} from './components/TokenRequestDialog';
import {ReasoningBlock, WebUiParts} from './components/TranscriptParts';
import {TranscriptRow, isActiveTranscriptUserMessage} from './components/TranscriptRow';
import {EarlierMessagesControl} from './components/TranscriptWindow';
import {UsageDetailsDrawer} from './components/UsageDetailsDrawer';
import appConfig from './config.json';
import {deepEqual} from './deepEqual';
import {COMPOSER_DESYNC_EVENT, type ComposerDesync, describeDesync, installDesyncWatch, shouldInstallDesyncWatch} from './desync-watch';
import {type DraftDecision} from './draft-answer';
import {EMPTY_HISTORY_BLOCKS, type HistoryBlock, type HistoryBlocks, forgetHistoryBlock, forgetHistoryBlocks, historyBlockFor, historyEtagFromHeader, putHistoryBlock} from './history-blocks';
import {type HookReport, hookReportNotification} from './hook-report-view';
import {type AttachedFile, type AttachedImage, buildRunInput} from './image-attachments';
import {type InteractionEnd, interactionEndNotice} from './interaction-end';
import {applyPart, applyPartDelta} from './messagePatch';
import {modelOptionsForSession, resolveRunModelChoice, resolveRunModelSelection, type RunModelChoice} from './model-selection';
import {type ProviderRetryWait} from './provider-retry-view';
import {type RightPanelId, selectionAfterPanelToggle} from './right-panel';
import {deferredRowStyle, useRowHeightRef} from './row-defer';
import {baseReasoning, defaultTools, getTraceForSteps, initialSessions, initialThreads} from './sample-data';
import {selectionQuoteTarget} from './selection-target';
import {sessionFromSnapshot, sessionUiId} from './session-mapping';
import {isSessionRunning, sessionUiStateForRun, snapshotEndsLocalRun, withSnapshotRun} from './session-run-status';
import {type PinnedView, activeKeyAfterPrune, addPinnedView, isPinnedViewKey, nextRecentViewKeys, profileLabel, profileOfViewKey, prunePinnedViews, restoreViews, retainedPinnedViewKeys, rowRunStatus, sessionListGroups, sessionListRows, sessionRefOfViewKey, setGroupCollapsed, viewKey, viewLabel} from './session-views';
import {SLASH_COMMANDS, type SlashCommandName} from './slash-commands';
import {forgetSnapshotEtags, rememberSnapshotEtag} from './snapshot-etags';
import {stopRunAfterConfirmation} from './stop-run';
import {readCollapsedGroups, readStoredViews, writeCollapsedGroups, writeStoredViews} from './stored-views';
import {shouldSkipTranscriptLoad} from './transcript-load-policy';
import {useActiveTranscript} from './use-active-transcript';
import {useRowWidth} from './use-row-width';
import {removeViewKeys, removedViewKeys} from './view-cache';
import {canAttachRunToView, recordOwnedRunTurn, updateViewState} from './view-state';

const styles = stylex.create({
  shell: {
    position: 'relative',
    height: '100%',
    minHeight: 0,
    minWidth: 0,
    backgroundColor: colorVars['--color-background-body'],
  },
  singleLine: {
    display: 'block',
    minWidth: 0,
    maxWidth: '100%',
    overflow: 'hidden',
    textOverflow: 'ellipsis',
    whiteSpace: 'nowrap',
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
  composerResizeHandle: {
    alignSelf: 'stretch',
    height: 'auto',
  },
  // The assistant name row carries a negative block margin of its own, so the
  // bubble contributes the rest of the gap between the identity row and the
  // message body.
  assistantBubble: {
    marginBlockStart: spacingVars['--spacing-4'],
  },
});

const LIVE_SESSION_ID = appConfig.live_session_id;

/**
 * The rows this page opens with: the profile rows pinned last time, else the
 * configured seed, with the selection falling back to the live session — which
 * is what the app opened before a session could be viewed per profile.
 */
const INITIAL_VIEWS = restoreViews({
  stored: readStoredViews(appConfig.session_views_storage_key),
  seed: appConfig.session_views,
  liveSessionId: LIVE_SESSION_ID,
});

function messageTime(): string {
  return new Date().toLocaleTimeString([], {
    hour: 'numeric',
    minute: '2-digit',
  });
}

const CatalogEditorDialog = lazy(() =>
  import('./components/CatalogEditorDialog').then(module => ({default: module.CatalogEditorDialog})),
);

function App({
  colorScheme,
  onToggleColorScheme,
}: {
  colorScheme: ColorScheme;
  onToggleColorScheme: () => void;
}) {
  const showToast = useToast();
  const showToastRef = useRef(showToast);
  useEffect(() => {
    showToastRef.current = showToast;
  }, [showToast]);
  const [sessions, setSessions] = useState<Session[]>(initialSessions);
  // The configured live session is a preference, not a row: the sidebar lists
  // whatever the backend reports, and this id is only used to choose which of
  // those live sessions to open on.
  // The rows this client pinned to a session, and the view on screen. A pinned
  // row is a conversation of its own, so switching between rows never disturbs
  // the transcript or the run of the one left behind.
  const [pinnedViews, setPinnedViews] = useState<PinnedView[]>(INITIAL_VIEWS.views);
  const [activeViewKey, setActiveViewKey] = useState<string>(INITIAL_VIEWS.activeViewKey);
  const activeSessionId = sessionRefOfViewKey(activeViewKey);
  const activeProfile = profileOfViewKey(activeViewKey);
  const isPinnedView = isPinnedViewKey(activeViewKey);
  const recentPinnedViewKeysRef = useRef<string[]>([]);
  const previousViewKeysRef = useRef<Set<string> | null>(null);
  if (previousViewKeysRef.current === null) {
    previousViewKeysRef.current = new Set([
      ...initialSessions.map(session => session.id),
      ...INITIAL_VIEWS.views.map(view => viewKey(view.sessionRef, view.profile)),
      INITIAL_VIEWS.activeViewKey,
    ]);
  }
  const [isModelDefaultUpdating] = useState(false);
  // A restored arrangement is the user's own: the "open the configured session"
  // preference only applies to a page that starts from nothing.
  const hasChosenSessionRef = useRef(INITIAL_VIEWS.restored);
  /**
   * Whether a session list has actually arrived from the server.
   *
   * Until it has, the rows on screen are local placeholders: a tab that names a
   * live session would look stale and be dropped — and that loss persisted.
   */
  const hasLoadedSessionsRef = useRef(false);
  const [threads, setThreads] = useState<Record<string, MessageRecord[]>>(initialThreads);

  /**
   * Turns a compaction removed, folded from the backup. They are static between
   * compactions, so a block is fetched once per identity the snapshot hands out
   * — a committed step re-sends the live tail, not the history. One block per
   * view: leaving a row must not take that row's history with it.
   */
  const [historyBlocks, setHistoryBlocks] =
    useState<HistoryBlocks<MessageRecord>>(EMPTY_HISTORY_BLOCKS);
  const historyBlocksRef = useRef<HistoryBlocks<MessageRecord>>(EMPTY_HISTORY_BLOCKS);
  const updateHistoryBlocks = useCallback(
    (update: (blocks: HistoryBlocks<MessageRecord>) => HistoryBlocks<MessageRecord>) => {
      const next = update(historyBlocksRef.current);
      if (next === historyBlocksRef.current) {
        return;
      }
      historyBlocksRef.current = next;
      setHistoryBlocks(next);
    },
    [],
  );
  const [isProfileEditorOpen, setIsProfileEditorOpen] = useState(false);
  const [hasOpenedProfileEditor, setHasOpenedProfileEditor] = useState(false);
  const [rightPanel, setRightPanel] = useState<RightPanelId | null>(
    appConfig.show_inspector_by_default
      ? 'inspector'
      : appConfig.show_prompt_library_by_default ? 'prompts' : null,
  );
  // The Jad README stands in for the transcript until a row is chosen.
  const [isReadmeShown, setIsReadmeShown] = useState(appConfig.start_with_readme);
  const showReadme = useCallback(() => setIsReadmeShown(true), []);
  const rightPanelRef = useRef<RightPanelId | null>(rightPanel);
  const setRightPanelMode = useCallback((nextPanel: RightPanelId | null) => {
    rightPanelRef.current = nextPanel;
    setRightPanel(nextPanel);
  }, []);
  const toggleInspector = useCallback(() => {
    setRightPanelMode(selectionAfterPanelToggle(rightPanelRef.current, 'inspector'));
  }, [setRightPanelMode]);
  const togglePromptLibrary = useCallback(() => {
    setRightPanelMode(selectionAfterPanelToggle(rightPanelRef.current, 'prompts'));
  }, [setRightPanelMode]);
  const toggleCheckpoints = useCallback(() => {
    setRightPanelMode(selectionAfterPanelToggle(rightPanelRef.current, 'checkpoints'));
  }, [setRightPanelMode]);
  // One left slot, several datasets: the panel shows whichever view was picked
  // last, so two panels never stack on the same edge.
  const [leftPanel, setLeftPanel] = useState<LeftPanelId | null>(null);
  const [runningViewKeys, setRunningViewKeys] = useState<ReadonlySet<string>>(
    new Set(),
  );
  const [compactingSessionRef, setCompactingSessionRef] = useState<string | null>(null);
  const [resetDrawerViewKey, setResetDrawerViewKey] = useState<string | null>(null);
  // The two destructive conversation actions live in a composer drawer rather
  // than in the toolbar row, where each sat one mis-click away from Compact.
  const [composerActionsViewKey, setComposerActionsViewKey] = useState<string | null>(null);
  const [stateContextViewKey, setStateContextViewKey] = useState<string | null>(null);
  const [resettingViewKey, setResettingViewKey] = useState<string | null>(null);
  const [resetError, setResetError] = useState<string | null>(null);
  const [frontCompactionInfoViewKey, setFrontCompactionInfoViewKey] = useState<string | null>(null);
  const [frontCompactionModeViewKey, setFrontCompactionModeViewKey] = useState<string | null>(null);
  const [frontCompactionTarget, setFrontCompactionTarget] = useState<{
    viewKey: string;
    message: MessageRecord;
  } | null>(null);
  const [frontCompactionError, setFrontCompactionError] = useState<string | null>(null);
  const [frontCompactingViewKey, setFrontCompactingViewKey] = useState<string | null>(null);
  const [notifications, setNotifications] = useState<ActivityNotification[]>([]);
  const notificationSequenceRef = useRef(0);
  const openNotifications = useCallback(() => {
    setRightPanelMode('notifications');
    setNotifications(current => markActivityNotificationsRead(current));
  }, [setRightPanelMode]);
  // The Activity item toggles; the toast action below stays open-only, so a live
  // event can never close the panel it was announcing.
  const toggleNotifications = useCallback(() => {
    const nextPanel = selectionAfterPanelToggle(rightPanelRef.current, 'notifications');
    if (nextPanel === null) {
      setRightPanelMode(null);
      return;
    }
    openNotifications();
  }, [openNotifications, setRightPanelMode]);
  const recordNotification = useCallback((details: NewActivityNotification) => {
    const createdAt = Date.now();
    const id = `notification-${createdAt}-${++notificationSequenceRef.current}`;
    const notification: ActivityNotification = {
      ...details,
      id,
      createdAt,
      isRead: rightPanelRef.current === 'notifications',
    };
    setNotifications(current =>
      prependActivityNotification(current, notification, appConfig.notification_history_limit),
    );
    const isError = details.kind === 'error';
    let dismissToast: (() => void) | undefined;
    dismissToast = showToastRef.current({
      body: isError ? details.title : details.message,
      type: details.kind,
      uniqueID: id,
      ...(isError ? {
        isAutoHide: false,
        endContent: (
          <Button
            label="Review activity"
            variant="ghost"
            size="sm"
            onClick={() => {
              openNotifications();
              dismissToast?.();
            }}
          />
        ),
      } : {}),
    });
  }, [openNotifications]);
  const [isUsageDrawerCollapsed, setIsUsageDrawerCollapsed] = useState(true);
  const [isModelDrawerOpen, setIsModelDrawerOpen] = useState(false);
  const [runModelChoice, setRunModelChoice] = useState<RunModelChoice | null>(null);
  const [runProgressByView, setRunProgressByView] = useState<Record<string, number>>({});
  const runProgress = runProgressByView[activeViewKey] ?? baseReasoning.length;
  const setRunProgressForView = useCallback((viewKey: string, progress: number) => {
    setRunProgressByView(current => updateViewState(current, viewKey, () => progress));
  }, []);
  const [streamingMessagesByView, setStreamingMessagesByView] =
    useState<Record<string, WebUiMessage>>({});
  const streamingMessage = streamingMessagesByView[activeViewKey] ?? null;
  const setStreamingMessageForView = useCallback((viewKey: string, message: WebUiMessage | null) => {
    setStreamingMessagesByView(current =>
      updateViewState(current, viewKey, () => message ?? undefined),
    );
  }, []);
  const [runInteractionStatesByView, setRunInteractionStatesByView] =
    useState<Record<string, RunInteractionState>>({});
  // The provider retry each view's run is waiting on; absent when it is not waiting.
  const [runRetriesByView, setRunRetriesByView] =
    useState<Record<string, RunRetryState>>({});
  // The reconnect each view's run stream is in; absent while the stream is up.
  const [runReconnectsByView, setRunReconnectsByView] =
    useState<Record<string, RunStreamReconnectState>>({});
  const [interactionUiByView, setInteractionUiByView] =
    useState<Record<string, InteractionUiState>>({});
  const activeInteractionUi = interactionUiByView[activeViewKey];
  const answeringInteractionId = activeInteractionUi?.answeringInteractionId ?? null;
  const extendingInteractionId = activeInteractionUi?.extendingInteractionId ?? null;
  const interactionAnswerError = activeInteractionUi?.error ?? null;
  const updateInteractionUiForView = useCallback((
    viewKey: string,
    update: (current: InteractionUiState) => InteractionUiState,
  ) => {
    setInteractionUiByView(current => updateViewState(current, viewKey, previous => {
      const next = update(previous ?? {});
      return next.answeringInteractionId === undefined &&
        next.extendingInteractionId === undefined && next.error === undefined
        ? undefined
        : next;
    }));
  }, []);
  // The run stream sends increments instead of a snapshot per provider delta, so
  // patches must be applied only to the message owned by the same view.
  const applyRunDelta = useCallback((
    viewKey: string,
    messageId: string,
    index: number,
    append: string,
  ) => {
    setStreamingMessagesByView(current => updateViewState(current, viewKey, message =>
      message !== undefined && message.id === messageId
        ? applyPartDelta(message, index, append)
        : message,
    ));
  }, []);
  const applyRunPart = useCallback((
    viewKey: string,
    messageId: string,
    index: number,
    part: WebUiPart,
  ) => {
    setStreamingMessagesByView(current => updateViewState(current, viewKey, message =>
      message !== undefined && message.id === messageId
        ? applyPart(message, index, part)
        : message,
    ));
  }, []);
  const conversation = useResizable({
    defaultSize: pixel(appConfig.conversation_lane_default_size),
    minSize: pixel(appConfig.conversation_lane_min_size),
    maxSize: pixel(appConfig.conversation_lane_max_size),
    direction: 'horizontal',
    autoSaveId: 'jad-conversation-width',
  });
  // The in-flight assistant bubble is rendered here rather than through
  // AssistantMessage, so it measures its own row width for title fitting.
  const [liveBubbleRef, liveBubbleWidthPx] = useRowWidth();
  const liveRowHeightRef = useRowHeightRef(null);
  const inspector = useResizable({
    defaultSize: pixel(appConfig.inspector_default_size),
    minSize: pixel(appConfig.inspector_min_size),
    maxSize: pixel(appConfig.inspector_max_size),
    direction: 'horizontal',
    autoSaveId: 'jad-run-inspector',
  });
  // The narrower region for the side panels: session search and run settings
  // on the left, the prompt library on the right. One region serves them all,
  // so the width the user dragged is the width each of them opens at.
  const leftPanelRegion = useResizable({
    defaultSize: pixel(appConfig.left_panel_default_size),
    minSize: pixel(appConfig.left_panel_min_size),
    maxSize: pixel(appConfig.left_panel_max_size),
    direction: 'horizontal',
    autoSaveId: 'jad-prompt-library',
  });

  // Filled by ComposerPane, which owns the drafts. Views outside the composer
  // write through this instead of lifting draft state back into App.
  const insertIntoComposerRef = useRef<ComposerInsertActions | null>(null);
  const registerComposerInsert = useCallback((insert: ComposerInsertActions | null) => {
    insertIntoComposerRef.current = insert;
  }, []);
  const selectLeftPanel = useCallback((panel: LeftPanelId) => {
    setLeftPanel(current => (current === panel ? null : panel));
  }, []);
  const usePrompt = useCallback((prompt: RootLibraryAsset) => {
    insertIntoComposerRef.current?.prompt(prompt.content, prompt.name);
  }, []);
  const useTemplate = useCallback((template: RootLibraryAsset) => {
    insertIntoComposerRef.current?.text(template.content);
  }, []);
  const useSkill = useCallback((skill: RootLibraryAsset) => {
    insertIntoComposerRef.current?.skill(skill.content, skill.name);
  }, []);
  /**
   * The quote action for a selection in the conversation: the draft it would
   * insert, and where its menu belongs on screen.
   */
  const [quoteTarget, setQuoteTarget] = useState<{
    draft: string;
    anchor: QuoteAnchor;
  } | null>(null);
  const dismissQuote = useCallback(() => setQuoteTarget(null), []);
  const quoteSelectionIntoComposer = useCallback(() => {
    if (quoteTarget !== null) {
      insertIntoComposerRef.current?.text(quoteTarget.draft);
    }
    setQuoteTarget(null);
  }, [quoteTarget]);
  useEffect(() => {
    if (!appConfig.selection_quote_enabled) {
      return;
    }
    // A double or triple click finishes selecting after its mouseup, so the
    // selection is read once the event has been handled.
    let pendingRead: number | undefined;
    const readSelection = () => {
      window.clearTimeout(pendingRead);
      pendingRead = window.setTimeout(() => setQuoteTarget(selectionQuoteTarget()), 0);
    };
    // A selection is made with the pointer or with the keyboard, so both ends
    // of the gesture are watched.
    document.addEventListener('mouseup', readSelection);
    document.addEventListener('keyup', readSelection);
    /*
     * Scrolling moves the text out from under the menu, and a live run re-pins
     * the transcript on every token, so clearing on scroll would drop the menu
     * the moment it appeared. The selection itself survives a scroll, so the
     * menu is re-measured against it instead — and only while there is one to
     * move.
     */
    const handleScroll = () =>
      setQuoteTarget(current => (current === null ? current : selectionQuoteTarget()));
    window.addEventListener('scroll', handleScroll, true);
    return () => {
      window.clearTimeout(pendingRead);
      document.removeEventListener('mouseup', readSelection);
      document.removeEventListener('keyup', readSelection);
      window.removeEventListener('scroll', handleScroll, true);
    };
  }, []);
  // Switching rows changes what is on screen, and a menu kept from the previous
  // row would insert text the user is no longer looking at.
  useEffect(() => {
    setQuoteTarget(null);
  }, [activeViewKey]);
  const runAbortRefs = useRef(new Map<string, AbortController>());
  const stopPendingViewKeysRef = useRef(new Set<string>());
  /** Interaction work is keyed by view so another profile can answer independently. */
  const answeringInteractionRefs = useRef(new Map<string, string>());
  const extendingInteractionRefs = useRef(new Map<string, string>());
  /**
   * Questions this tab sent an answer for, and views where this tab asked for the stop.
   * They outlive the request on purpose: the stream's end event can arrive after the
   * POST resolved, and a tab must not announce its own answer as another window's.
   */
  const answeredHereRefs = useRef(new Set<string>());
  const stoppedHereViewKeysRef = useRef(new Set<string>());
  const runTimeoutRefs = useRef(new Map<string, number>());
  const activeRunViewKeysRef = useRef(new Set<string>());
  const attachedRunViewKeysRef = useRef(new Set<string>());
  const hydratedTranscriptSessionIdsRef = useRef(new Set<string>());
  // The turn and stream attachment each belong to a specific profile view.
  const attachedTurnRefs = useRef(new Map<string, string>());
  const activeRunTurnRefs = useRef(new Map<string, string>());
  const setRunRunningForView = useCallback((viewKey: string, running: boolean) => {
    // A local run edits the thread ahead of the server, so the view's etag no
    // longer describes it.
    snapshotEtagsRef.current.delete(viewKey);
    if (running) {
      activeRunViewKeysRef.current.add(viewKey);
    } else {
      activeRunViewKeysRef.current.delete(viewKey);
    }
    setRunningViewKeys(current => {
      if (current.has(viewKey) === running) return current;
      const next = new Set(current);
      if (running) next.add(viewKey);
      else next.delete(viewKey);
      return next;
    });
  }, []);
  const recordRunStarted = useCallback((viewKey: string, turnId: string) => {
    // The next poll that reports a non-running state clears the attached turn
    // (see the attach effect).
    recordOwnedRunTurn(activeRunTurnRefs.current, attachedTurnRefs.current, viewKey, turnId);
    setRunInteractionStatesByView(current => updateViewState(current, viewKey, existing =>
      existing?.turnId === turnId
        ? existing
        : {viewKey, turnId, pending: undefined, received: false},
    ));
  }, []);
  const recordRunInteraction = useCallback((
    viewKey: string,
    turnId: string,
    pending: WebUiInteraction | undefined,
  ) => {
    if (activeRunTurnRefs.current.get(viewKey) !== turnId) return;
    setRunInteractionStatesByView(current => updateViewState(current, viewKey, () => ({
      viewKey,
      turnId,
      pending,
      received: true,
    })));
  }, []);
  const recordRunRetry = useCallback((
    viewKey: string,
    turnId: string,
    wait: ProviderRetryWait | undefined,
  ) => {
    if (activeRunTurnRefs.current.get(viewKey) !== turnId) return;
    setRunRetriesByView(current => updateViewState(current, viewKey, () =>
      wait === undefined ? undefined : {turnId, wait},
    ));
  }, []);
  /**
   * Tells this tab why a question left the run, unless this tab caused it. The server
   * sends the reason once, on the event that clears the question, so there is nothing
   * to look up later; a tab that attaches afterwards is told only that nothing waits.
   */
  const recordInteractionEnd = useCallback((
    viewKey: string,
    sessionTitle: string,
    turnId: string,
    end: InteractionEnd,
  ) => {
    if (activeRunTurnRefs.current.get(viewKey) !== turnId) return;
    const answeredHere = answeredHereRefs.current.delete(end.interactionId);
    const notice = interactionEndNotice(end, {
      answeredHere,
      stoppedHere: stoppedHereViewKeysRef.current.has(viewKey),
    });
    if (notice === undefined) return;
    recordNotification({
      sessionId: viewKey,
      sessionTitle,
      kind: 'info',
      title: notice.title,
      message: notice.message,
    });
  }, [recordNotification]);
  const recordHookReport = useCallback((
    viewKey: string,
    sessionTitle: string,
    turnId: string,
    report: HookReport,
  ) => {
    if (activeRunTurnRefs.current.get(viewKey) !== turnId) return;
    recordNotification(hookReportNotification(viewKey, sessionTitle, report));
  }, [recordNotification]);
  const recordRunReconnect = useCallback((
    viewKey: string,
    controller: AbortController,
    state: RunStreamReconnectState | undefined,
  ) => {
    // A stale run's reconnect must not paint a notice over a newer run of the view.
    if (runAbortRefs.current.get(viewKey) !== controller) return;
    setRunReconnectsByView(current => updateViewState(current, viewKey, () => state));
  }, []);
  const clearRunInteraction = useCallback((viewKey: string, turnId?: string) => {
    const activeTurnId = activeRunTurnRefs.current.get(viewKey);
    if (turnId !== undefined && activeTurnId !== undefined && activeTurnId !== turnId) return;
    activeRunTurnRefs.current.delete(viewKey);
    setRunReconnectsByView(current => updateViewState(current, viewKey, () => undefined));
    // A run that is no longer ours cannot still be waiting on the provider.
    setRunRetriesByView(current => updateViewState(current, viewKey, () => undefined));
    setRunInteractionStatesByView(current => updateViewState(current, viewKey, state =>
      state !== undefined && turnId !== undefined && state.turnId !== turnId
        ? state
        : undefined,
    ));
  }, []);
  const clearInteractionUiForView = useCallback((viewKey: string) => {
    answeringInteractionRefs.current.delete(viewKey);
    extendingInteractionRefs.current.delete(viewKey);
    stoppedHereViewKeysRef.current.delete(viewKey);
    setInteractionUiByView(current => updateViewState(current, viewKey, () => undefined));
  }, []);
  const activeSessionIdRef = useRef<string>(activeSessionId);
  const activeViewKeyRef = useRef<string>(activeViewKey);
  const activeProfileRef = useRef<string | undefined>(activeProfile);
  const activeSessionEntryRef = useRef<Session | undefined>(undefined);
  const refreshSessionsRef = useRef<(() => void) | null>(null);
  // Conditional polling: each resource is revalidated with the fingerprint of
  // the last body the client accepted.
  const sessionsEtagRef = useRef<string | undefined>(undefined);
  // Per view, and only while that view's caches hold the body it names: eviction
  // and resets delete the entry, so a 304 can never meet an empty view.
  const snapshotEtagsRef = useRef<Map<string, string>>(new Map());
  const activeViewRef = useRef<{
    viewKey: string;
    ref: string | undefined;
    profile: string | undefined;
  }>({
    viewKey: activeViewKey,
    ref: undefined,
    profile: activeProfile,
  });
  const loadingOlderRef = useRef(new Set<string>());
  /**
   * Messages the client fetched whole, per session.
   *
   * The snapshot keeps sending the capped copy of a large part, so a hydrated
   * message has to outlive the next poll: every server payload for a session is
   * passed through this overlay, keyed by message id, which is stable because
   * ids come from the transcript offset.
   */
  const hydratedMessagesRef = useRef<Map<string, Map<string, WebUiMessage>>>(new Map());
  const transcriptResetEpochRef = useRef(new Map<string, number>());
  const applyHydrated = useCallback(
    (uiId: string, messages: WebUiMessage[]): WebUiMessage[] => {
      const overlay = hydratedMessagesRef.current.get(uiId);
      if (overlay === undefined || overlay.size === 0) {
        return messages;
      }
      let changed = false;
      const next = messages.map(message => {
        const full = overlay.get(message.id);
        if (full === undefined || deepEqual(full, message)) {
          return message;
        }
        changed = true;
        return full;
      });
      return changed ? next : messages;
    },
    [],
  );
  const loadFullMessage = useCallback(
    (message: MessageRecord) => {
      const uiId = activeViewKeyRef.current;
      if (!previousViewKeysRef.current?.has(uiId)) {
        return;
      }
      const resetEpoch = transcriptResetEpochRef.current.get(uiId) ?? 0;
      const profile = profileOfViewKey(uiId);
      const sessionRef = activeSessionEntryRef.current?.sessionRef;
      if (sessionRef === undefined) {
        return;
      }
      return fetchSessionMessage(
        sessionRef,
        message.id,
        message.cursor,
        profile,
      )
        .then(full => {
          if ((transcriptResetEpochRef.current.get(uiId) ?? 0) !== resetEpoch) {
            return;
          }
          const overlay = hydratedMessagesRef.current.get(uiId) ?? new Map();
          overlay.set(message.id, full);
          hydratedMessagesRef.current.set(uiId, overlay);
          setThreads(current => {
            const existing = current[uiId];
            const index = existing?.findIndex(candidate => candidate.id === message.id) ?? -1;
            if (existing === undefined || index < 0) {
              return current;
            }
            const next = [...existing];
            next[index] = full;
            return {...current, [uiId]: next};
          });
          // A message from the history block is not part of the thread.
          const block = historyBlocksRef.current[uiId];
          if (block !== undefined) {
            const index = block.messages.findIndex(candidate => candidate.id === message.id);
            if (index >= 0) {
              const messages = [...block.messages];
              messages[index] = full;
              updateHistoryBlocks(blocks => putHistoryBlock(blocks, uiId, {...block, messages}));
            }
          }
        })
        .catch(error => {
          console.warn('Unable to load the full message:', error);
        });
    },
    [updateHistoryBlocks],
  );
  // How much of the transcript is still on the server, per session.
  const [sessionPages, setSessionPages] = useState<Record<string, SessionPage>>({});
  // Runs that failed, per view. The server keeps them beside the transcript.
  const [sessionFailures, setSessionFailures] = useState<
    Record<string, readonly SessionRunFailure[]>
  >({});

  // Live sessions come from the backend, so the active id can outlive its row —
  // on the first paint (before the first poll answers) and after a session is
  // deleted server-side. Reconcile instead of rendering a phantom: the
  // configured live session when the backend has it, otherwise the first live
  // session, otherwise whatever the list starts with. A session the user picked
  // is never taken away from them.
  useEffect(() => {
    if (sessions.length === 0 || !hasLoadedSessionsRef.current) {
      // The rows of the first paint are placeholders: retargeting a tab against
      // them would fold the views this page just restored.
      return;
    }
    const preferredLive =
      sessions.find(session => session.id === LIVE_SESSION_ID) ??
      sessions.find(session => session.source === 'live');
    if (!sessions.some(session => session.id === activeSessionId)) {
      // The view on screen is gone: fall back to a session that exists.
      const fallback = (preferredLive ?? sessions[0]).id;
      setActiveViewKey(prev => (isPinnedViewKey(prev) ? viewKey(fallback, profileOfViewKey(prev)) : fallback));
      hasChosenSessionRef.current = false;
      return;
    }
    if (
      !isPinnedView &&
      !hasChosenSessionRef.current &&
      preferredLive !== undefined &&
      activeSessionId !== preferredLive.id
    ) {
      setActiveViewKey(preferredLive.id);
    }
  }, [activeSessionId, sessions]);

  const activeSession =
    sessions.find(session => session.id === activeSessionId) ?? sessions[0];

  /** The rows the list shows: every session, and the profiles pinned to it. */
  const listRows = useMemo(
    () => sessionListRows(sessions, pinnedViews),
    [pinnedViews, sessions],
  );
  const activeRow = listRows.find(row => row.key === activeViewKey);
  const listGroups = useMemo(
    () => sessionListGroups(
      sessions.map(session => ({
        id: session.id,
        title: session.title,
        ...(session.category === undefined ? {} : {category: session.category}),
        ...(session.profiles === undefined ? {} : {profiles: session.profiles}),
        ...(session.profileNames === undefined ? {} : {profileNames: session.profileNames}),
        ...(session.activeProfile === undefined ? {} : {activeProfile: session.activeProfile}),
      })),
    ),
    [sessions],
  );
  const [collapsedGroups, setCollapsedGroups] = useState<string[]>(() =>
    readCollapsedGroups(appConfig.session_groups_storage_key),
  );
  const changeGroupCollapsed = useCallback((sessionRef: string, isCollapsed: boolean) => {
    setCollapsedGroups(current => setGroupCollapsed(current, sessionRef, isCollapsed));
  }, []);
  useEffect(() => {
    writeCollapsedGroups(appConfig.session_groups_storage_key, collapsedGroups);
  }, [collapsedGroups]);

  /**
   * What each row's own snapshot reported about it, keyed by row.
   *
   * The session's own row is the list's record; a pinned row keeps one here, and
   * its snapshot fills it. That record is what a row *is* to the rest of the app:
   * its own transcript, model, profiles and usage, sharing the session's
   * `setting.json` with the other rows.
   */
  const [rowSessions, setRowSessions] = useState<Record<string, Session>>({});

  const forgetViewCaches = useCallback((viewKeys: ReadonlySet<string>) => {
    if (viewKeys.size === 0) {
      return;
    }
    for (const key of viewKeys) {
      transcriptResetEpochRef.current.set(
        key,
        (transcriptResetEpochRef.current.get(key) ?? 0) + 1,
      );
      hydratedMessagesRef.current.delete(key);
      hydratedTranscriptSessionIdsRef.current.delete(key);
    }
    forgetSnapshotEtags(snapshotEtagsRef.current, viewKeys);
    updateHistoryBlocks(blocks => forgetHistoryBlocks(blocks, viewKeys));
    setThreads(current => removeViewKeys(current, viewKeys));
    setSessionPages(current => removeViewKeys(current, viewKeys));
    setSessionFailures(current => removeViewKeys(current, viewKeys));
    setRowSessions(current => removeViewKeys(current, viewKeys));
    // Keep loadingOlderRef entries until their requests settle; their captured
    // reset epoch now prevents an evicted result from repopulating the cache.
  }, [updateHistoryBlocks]);

  const activeRowSession: Session = !isPinnedView
    ? activeSession
    : rowSessions[activeViewKey] ?? {
        id: activeViewKey,
        sessionRef: activeSessionId,
        profile: activeProfile,
        title: viewLabel(activeSession.title, activeProfile === undefined ? undefined : profileLabel(activeProfile, activeSession.profileNames)),
        time: activeSession.time,
        turns: 0,
        state: sessionUiStateForRun(rowRunStatus(activeRow, activeSession)?.status),
        source: 'live',
        activeProfile,
        ...(activeSession.profiles === undefined
          ? {}
          : {profiles: activeSession.profiles}),
        ...(activeSession.profileNames === undefined
          ? {}
          : {profileNames: activeSession.profileNames}),
      };

  /** Apply a live result to the row that initiated it, not its master session. */
  const updateViewSessionRecord = useCallback(
    (
      key: string,
      update: (session: Session) => Session,
      fallback?: Session,
    ) => {
      const sessionRef = sessionRefOfViewKey(key);
      if (isPinnedViewKey(key)) {
        setRowSessions(current => {
          const existing = current[key] ?? fallback;
          if (existing === undefined) {
            return current;
          }
          const next = update(existing);
          return current[key] === next ? current : {...current, [key]: next};
        });
        return;
      }
      setSessions(current => {
        let changed = false;
        const next = current.map(session => {
          if (session.id !== sessionRef) {
            return session;
          }
          const updated = update(session);
          if (updated !== session) {
            changed = true;
          }
          return updated;
        });
        return changed ? next : current;
      });
    },
    [],
  );

  /**
   * Fills a pinned row's record from its own snapshot.
   *
   * `sessionFromSnapshot` is the same builder the list uses, so a row is a
   * session in every respect — except that the name it shows is the session's
   * name plus the profile it is pinned to.
   */
  const applyRowSnapshot = useCallback(
    (key: string, sessionRef: string, profile: string | undefined, snapshot: SessionSnapshot) => {
      if (profile === undefined) {
        return;
      }
      setRowSessions(current => {
        const fromSnapshot = sessionFromSnapshot(snapshot, key);
        const record: Session = {
          ...fromSnapshot,
          id: key,
          sessionRef,
          profile,
          title: viewLabel(fromSnapshot.title, profileLabel(profile, fromSnapshot.profileNames)),
        };
        return deepEqual(current[key], record) ? current : {...current, [key]: record};
      });
    },
    [],
  );

  const rowTurns = useMemo(() => {
    const turns: Record<string, number> = {};
    for (const [key, record] of Object.entries(rowSessions)) {
      turns[key] = record.turns;
    }
    return turns;
  }, [rowSessions]);

  const isLiveSession = activeSession?.source === 'live';
  const isRunning = isSessionRunning(
    runningViewKeys.has(activeViewKey),
    rowRunStatus(activeRow, activeSession)?.status,
  );
  const hasLiveAttachedRun =
    isLiveSession &&
    (rowRunStatus(activeRow, activeSession)?.status === 'running' ||
      attachedRunViewKeysRef.current.has(activeViewKey));
  const {
    inputHistory,
    visibleMessages,
    hiddenCount,
    revealEarlier,
    placedFailures,
    streamingMessageCommitted,
    liveToolParts,
    tools,
  } = useActiveTranscript({
    threads,
    historyBlocks,
    viewKey: activeViewKey,
    hasLiveAttachedRun,
    failures: sessionFailures[activeViewKey],
    isLiveSession,
    isRunning,
    streamingMessage,
    fallbackTools: defaultTools,
  });
  const activeSessionRef = activeSession?.sessionRef;
  // Where a tool result's stored images are read from; memoised so the rows do not refetch.
  const storedImageScope = useMemo(
    () => (activeSessionRef === undefined ? undefined : {sessionRef: activeSessionRef, profile: activeProfile}),
    [activeSessionRef, activeProfile],
  );
  const liveImageScope = useMemo(
    () => (storedImageScope === undefined ? undefined : {...storedImageScope, live: true}),
    [storedImageScope],
  );
  const activeSessionPage = sessionPages[activeViewKey];
  // Older turns exist on the server but are not part of this client's copy yet.
  const hasOlderTurns =
    isLiveSession &&
    activeSessionPage?.has_more === true &&
    activeSessionPage.next_before !== null;

  /**
   * Fetches the turns before the oldest one this client holds. The cursor comes
   * from the server, so the transcript is extended in place instead of being
   * reloaded, and already-known messages are dropped by id.
   */
  const loadOlderMessages = useCallback(
    (session: Session | undefined, uiId: string) => {
      const page = sessionPages[uiId];
      if (
        !previousViewKeysRef.current?.has(uiId) ||
        session?.source !== 'live' ||
        session.sessionRef === undefined ||
        page === undefined ||
        !page.has_more ||
        page.next_before === null ||
        loadingOlderRef.current.has(uiId)
      ) {
        return;
      }
      loadingOlderRef.current.add(uiId);
      const resetEpoch = transcriptResetEpochRef.current.get(uiId) ?? 0;
      const profile = profileOfViewKey(uiId);
      void fetchSession(session.sessionRef, {
        before: page.next_before,
        generation: page.generation,
        turns: appConfig.transcript_page_turns,
        ...(profile === undefined ? {} : {profile}),
      })
        .then(result => {
          if (
            (transcriptResetEpochRef.current.get(uiId) ?? 0) !== resetEpoch ||
            result.kind !== 'ok'
          ) {
            return;
          }
          if (result.value.page?.stale_cursor) {
            // Compaction replaced the transcript after this page was issued.
            // A normal snapshot reload also refreshes the backup history block.
            refreshSessionsRef.current?.();
            return;
          }
          const older = applyHydrated(uiId, result.value.messages ?? []);
          setThreads(current => {
            const existing = current[uiId] ?? [];
            const known = new Set(existing.map(message => message.id));
            const prepended = older.filter(message => !known.has(message.id));
            if (prepended.length === 0) {
              return current;
            }
            return {...current, [uiId]: [...prepended, ...existing]};
          });
          if (result.value.page !== undefined) {
            const pageInfo = result.value.page;
            setSessionPages(current => ({...current, [uiId]: pageInfo}));
          }
        })
        .catch(error => {
          console.warn('Unable to load earlier turns:', error);
        })
        .finally(() => {
          loadingOlderRef.current.delete(uiId);
        });
    },
    [applyHydrated, sessionPages],
  );

  const revealEarlierMessages = useCallback(
    (node: HTMLElement | null) => {
      if (hiddenCount > 0) {
        revealEarlier(node);
        return;
      }
      // Nothing loaded is hidden any more: the rest of the transcript has to be
      // fetched before it can be revealed.
      loadOlderMessages(activeSession, activeViewKey);
    },
    [activeSession, activeViewKey, hiddenCount, loadOlderMessages, revealEarlier],
  );
  const isCompactionInProgress = compactingSessionRef !== null;
  const isResetDrawerOpen = resetDrawerViewKey === activeViewKey;
  const isComposerActionsDrawerOpen = composerActionsViewKey === activeViewKey;
  const isStateContextDrawerOpen = stateContextViewKey === activeViewKey;
  const isCurrentViewResetting = resettingViewKey === activeViewKey;
  const frontCompactionSettings = activeRowSession.frontCompaction ?? {
    enabled: true,
    delete_mode: 'permanent' as const,
  };
  const isFrontCompactionInfoDrawerOpen = frontCompactionInfoViewKey === activeViewKey;
  const isFrontCompactionConfirmationOpen = frontCompactionTarget?.viewKey === activeViewKey;
  const isFrontCompactionMode =
    frontCompactionModeViewKey === activeViewKey &&
    frontCompactionSettings.enabled &&
    isLiveSession &&
    !isRunning &&
    !isCompactionInProgress &&
    !isModelDefaultUpdating &&
    resettingViewKey === null &&
    frontCompactingViewKey === null;
  const isFrontCompactionSelectionMode =
    isFrontCompactionMode && !isFrontCompactionConfirmationOpen;
  const isFrontCompactionWorkflowOpen =
    frontCompactionModeViewKey === activeViewKey ||
    isFrontCompactionInfoDrawerOpen ||
    isFrontCompactionConfirmationOpen;
  const isCurrentViewFrontCompacting = frontCompactingViewKey === activeViewKey;
  const openFrontCompactionConfirmation = useCallback((message: MessageRecord) => {
    if (
      !isFrontCompactionSelectionMode ||
      !frontCompactionSettings.enabled ||
      !isLiveSession ||
      isRunning ||
      isCompactionInProgress ||
      isModelDefaultUpdating ||
      resettingViewKey !== null ||
      frontCompactingViewKey !== null
    ) {
      return;
    }
    setIsUsageDrawerCollapsed(true);
    setIsModelDrawerOpen(false);
    setStateContextViewKey(null);
    setResetDrawerViewKey(null);
    setFrontCompactionInfoViewKey(null);
    setFrontCompactionError(null);
    setFrontCompactionTarget({viewKey: activeViewKey, message});
  }, [
    activeViewKey,
    frontCompactingViewKey,
    frontCompactionSettings.enabled,
    isCompactionInProgress,
    isFrontCompactionSelectionMode,
    isLiveSession,
    isModelDefaultUpdating,
    isRunning,
    resettingViewKey,
  ]);
  const isActiveSessionCompacting = activeSessionRef !== undefined && compactingSessionRef === activeSessionRef;
  const unreadNotificationCount = unreadActivityNotificationCount(notifications);
  const hasUnreadError = hasUnreadActivityError(notifications);

  /**
   * The question this composer can answer. Prefer independent run state so the
   * prompt is not gated on a message completing; message.pending remains a
   * compatibility fallback. The question stays on the view where its run waits.
   */
  const activeRunRetry = runRetriesByView[activeViewKey];
  const activeRunReconnect = runReconnectsByView[activeViewKey];
  const activeRunInteractionState = runInteractionStatesByView[activeViewKey];
  const pendingInteraction = activeRunInteractionState?.received
    ? activeRunInteractionState.pending
    : streamingMessage?.pending;

  const reasoningSteps = useMemo(() => {
    const steps = isLiveSession ? [] : baseReasoning;
    return getTraceForSteps(steps, isRunning ? runProgress : steps.length);
  }, [isLiveSession, isRunning, runProgress]);

  /**
   * Drops this client's view of a run without touching the run itself: the
   * server keeps it, so switching sessions or reloading can pick it up again.
   */
  const detachRun = useCallback((viewKey: string = activeViewKeyRef.current) => {
    const controller = runAbortRefs.current.get(viewKey);
    if (controller !== undefined) {
      runAbortRefs.current.delete(viewKey);
      controller.abort();
    }
    attachedTurnRefs.current.delete(viewKey);
    attachedRunViewKeysRef.current.delete(viewKey);
    const timeout = runTimeoutRefs.current.get(viewKey);
    if (timeout !== undefined) {
      window.clearTimeout(timeout);
      runTimeoutRefs.current.delete(viewKey);
    }
    setStreamingMessageForView(viewKey, null);
    clearRunInteraction(viewKey);
    clearInteractionUiForView(viewKey);
    setRunRunningForView(viewKey, false);
    setRunProgressForView(viewKey, baseReasoning.length);
  }, [
    clearInteractionUiForView,
    clearRunInteraction,
    setRunProgressForView,
    setRunRunningForView,
    setStreamingMessageForView,
  ]);

  /**
   * Stops the run for real. Unsubscribing only closes this client's stream —
   * the server would keep running the turn — so a stop has to be asked for.
   */
  const stopRun = useCallback(() => {
    const targetViewKey = activeViewKeyRef.current;
    const session = activeSessionEntryRef.current;
    if (session?.source !== 'live' || !session.sessionRef) {
      detachRun(targetViewKey);
      return;
    }
    if (stopPendingViewKeysRef.current.has(targetViewKey)) return;

    stopPendingViewKeysRef.current.add(targetViewKey);
    stoppedHereViewKeysRef.current.add(targetViewKey);
    const sessionRef = session.sessionRef;
    const profile = profileOfViewKey(targetViewKey);
    const targetController = runAbortRefs.current.get(targetViewKey) ?? null;
    const stopSignal = AbortSignal.timeout(appConfig.stop_request_timeout_ms);
    const detachStoppedView = () => {
      if (
        targetController !== null &&
        runAbortRefs.current.get(targetViewKey) === targetController
      ) {
        detachRun(targetViewKey);
      } else {
        refreshSessionsRef.current?.();
      }
    };
    void stopRunAfterConfirmation(
      () => stopSessionRun(sessionRef, profile, stopSignal),
      detachStoppedView,
    ).then(stopped => {
      if (!stopped) {
        stoppedHereViewKeysRef.current.delete(targetViewKey);
        detachStoppedView();
        recordNotification({
          sessionId: session.id,
          sessionTitle: activeRowSession.title,
          kind: 'info',
          title: 'No active run to stop',
          message: 'The server did not find a running request for this profile. Its status was refreshed; try Stop again if it is still working.',
        });
      }
    }).catch(error => {
      stoppedHereViewKeysRef.current.delete(targetViewKey);
      recordNotification({
        sessionId: session.id,
        sessionTitle: activeRowSession.title,
        kind: 'error',
        title: 'Unable to stop run',
        message: error instanceof Error ? error.message : String(error),
      });
      console.warn('Unable to stop the run:', error);
    }).finally(() => {
      stopPendingViewKeysRef.current.delete(targetViewKey);
    });
  }, [activeRowSession.title, detachRun, recordNotification]);

  /**
   * Answers the question a run is waiting on.
   *
   * A refusal — the deadline passed, or the run is no longer waiting for that
   * question — is reported as a notification, and nothing is undone locally: the
   * run's own stream publishes the question's end, so the drawer closes from the
   * server's state rather than from an assumption here.
   */
  const answerInteraction = useCallback((
    interaction: WebUiInteraction,
    body: {decision: DraftDecision; draft?: unknown; reason?: string},
  ) => {
    const session = activeSession;
    const viewKey = activeViewKey;
    const profile = activeProfile;
    if (session?.source !== 'live' || !session.sessionRef) {
      return;
    }
    if (answeringInteractionRefs.current.has(viewKey)) {
      return;
    }
    answeringInteractionRefs.current.set(viewKey, interaction.id);
    answeredHereRefs.current.add(interaction.id);
    updateInteractionUiForView(viewKey, current => ({
      ...current,
      answeringInteractionId: interaction.id,
      error: undefined,
    }));
    const signal = AbortSignal.timeout(appConfig.interaction_answer_timeout_ms);
    void answerSessionRunInteraction(
      session.sessionRef,
      {interaction_id: interaction.id, ...body},
      profile,
      signal,
    ).catch(error => {
      // A refused answer did not decide anything, so the end that follows is not ours.
      answeredHereRefs.current.delete(interaction.id);
      const message = error instanceof Error ? error.message : String(error);
      updateInteractionUiForView(viewKey, current => ({...current, error: message}));
      recordNotification({
        sessionId: session.id,
        sessionTitle: activeRowSession.title,
        kind: 'error',
        title: interactionFailureTitle(interaction.kind, 'answer'),
        message,
      });
      console.warn('Unable to answer the interaction:', error);
    }).finally(() => {
      if (answeringInteractionRefs.current.get(viewKey) === interaction.id) {
        answeringInteractionRefs.current.delete(viewKey);
        updateInteractionUiForView(viewKey, current => ({
          ...current,
          answeringInteractionId: undefined,
        }));
      }
    });
  }, [
    activeRowSession.title,
    activeSession,
    activeProfile,
    activeViewKey,
    recordNotification,
    updateInteractionUiForView,
  ]);

  /**
   * Pushes the question's deadline out by the session's own step.
   *
   * The new deadline is not computed here: the run republishes the question with
   * its own `expires_at`, so the countdown follows the server's record and every
   * open tab shows the same time left.
   */
  const extendInteraction = useCallback((interaction: WebUiInteraction) => {
    const session = activeSession;
    const viewKey = activeViewKey;
    const profile = activeProfile;
    if (session?.source !== 'live' || !session.sessionRef) {
      return;
    }
    if (extendingInteractionRefs.current.has(viewKey)) {
      return;
    }
    extendingInteractionRefs.current.set(viewKey, interaction.id);
    updateInteractionUiForView(viewKey, current => ({
      ...current,
      extendingInteractionId: interaction.id,
      error: undefined,
    }));
    const signal = AbortSignal.timeout(appConfig.interaction_answer_timeout_ms);
    void extendSessionRunInteraction(
      session.sessionRef,
      {interaction_id: interaction.id},
      profile,
      signal,
    ).catch(error => {
      const message = error instanceof Error ? error.message : String(error);
      updateInteractionUiForView(viewKey, current => ({...current, error: message}));
      recordNotification({
        sessionId: session.id,
        sessionTitle: activeRowSession.title,
        kind: 'error',
        title: interactionFailureTitle(interaction.kind, 'extend'),
        message,
      });
      console.warn('Unable to extend the interaction:', error);
    }).finally(() => {
      if (extendingInteractionRefs.current.get(viewKey) === interaction.id) {
        extendingInteractionRefs.current.delete(viewKey);
        updateInteractionUiForView(viewKey, current => ({
          ...current,
          extendingInteractionId: undefined,
        }));
      }
    });
  }, [
    activeRowSession.title,
    activeSession,
    activeProfile,
    activeViewKey,
    recordNotification,
    updateInteractionUiForView,
  ]);

  const compactActiveSession = useCallback(async () => {
    const sessionRef = activeSession?.sessionRef;
    if (
      !isLiveSession ||
      !sessionRef ||
      isRunning ||
      isCompactionInProgress ||
      resettingViewKey !== null ||
      frontCompactingViewKey !== null
    ) {
      return;
    }

    setCompactingSessionRef(sessionRef);
    try {
      // Compaction rewrites one profile's transcript, so it must be the profile
      // this view is showing — not the session's stored default.
      const result = await compactSession(sessionRef, activeProfileRef.current);
      if (result.usage !== undefined) {
        updateViewSessionRecord(
          activeViewKey,
          session => ({...session, usage: result.usage}),
          activeRowSession,
        );
      }
      const message = result.compacted
        ? `History compacted: retained ${result.retained_items} of ${result.compacted_items} items.`
        : `No compaction needed; ${result.retained_items} items retained.`;
      recordNotification({
        sessionId: activeSessionId,
        sessionTitle: activeRowSession.title,
        kind: 'info',
        title: result.compacted ? 'History compacted' : 'No compaction needed',
        message,
      });
    } catch (error) {
      recordNotification({
        sessionId: activeSessionId,
        sessionTitle: activeRowSession.title,
        kind: 'error',
        title: 'Compaction failed',
        message: error instanceof Error ? error.message : String(error),
      });
    } finally {
      setCompactingSessionRef(current => current === sessionRef ? null : current);
    }
  }, [
    activeSession?.sessionRef,
    activeRowSession,
    activeRowSession.title,
    activeSessionId,
    activeViewKey,
    isCompactionInProgress,
    isLiveSession,
    isRunning,
    recordNotification,
    resettingViewKey,
    frontCompactingViewKey,
    updateViewSessionRecord,
  ]);

  const requestStateContext = useCallback(
    async (prompt: string, preview: boolean) => {
      const sessionRef = activeSession?.sessionRef;
      if (!sessionRef) {
        throw new Error('no live session is selected');
      }
      const result = await writeSessionStateContext(
        sessionRef,
        {prompt, preview},
        activeProfileRef.current,
      );
      if (!preview && result.written) {
        recordNotification({
          sessionId: activeSessionId,
          sessionTitle: activeRowSession.title,
          kind: 'info',
          title: 'State context written',
          message: `A ${result.bytes ?? 0}-byte block will be sent with the next request.`,
        });
      }
      return result;
    },
    [activeSession?.sessionRef, activeRowSession.title, activeSessionId, recordNotification],
  );

  const resetActiveSession = useCallback(async () => {
    const sessionRef = activeSession?.sessionRef;
    const targetViewKey = activeViewKey;
    const targetProfile = profileOfViewKey(targetViewKey);
    const targetRowSession = activeRowSession;
    if (
      !isLiveSession ||
      !sessionRef ||
      isRunning ||
      isCompactionInProgress ||
      isModelDefaultUpdating ||
      resettingViewKey !== null ||
      frontCompactingViewKey !== null
    ) {
      return;
    }

    setResettingViewKey(targetViewKey);
    setResetError(null);
    try {
      await resetSession(sessionRef, targetProfile);
      transcriptResetEpochRef.current.set(
        targetViewKey,
        (transcriptResetEpochRef.current.get(targetViewKey) ?? 0) + 1,
      );
      hydratedMessagesRef.current.delete(targetViewKey);
      hydratedTranscriptSessionIdsRef.current.delete(targetViewKey);
      loadingOlderRef.current.delete(targetViewKey);
      setThreads(current => ({...current, [targetViewKey]: []}));
      setSessionFailures(current => removeViewKeys(current, new Set([targetViewKey])));
      setSessionPages(current => {
        if (!(targetViewKey in current)) {
          return current;
        }
        const next = {...current};
        delete next[targetViewKey];
        return next;
      });
      updateHistoryBlocks(blocks => forgetHistoryBlock(blocks, targetViewKey));
      updateViewSessionRecord(
        targetViewKey,
        session => ({
          ...session,
          turns: 0,
          usage: {
            completed_responses: 0,
            responses_with_usage: 0,
            turn_count: 0,
            history_partial: false,
          },
        }),
        targetRowSession,
      );
      setResetDrawerViewKey(null);
      setFrontCompactionInfoViewKey(current => current === targetViewKey ? null : current);
      setFrontCompactionTarget(current => current?.viewKey === targetViewKey ? null : current);
      setFrontCompactionModeViewKey(current => current === targetViewKey ? null : current);
      setFrontCompactionError(null);
      sessionsEtagRef.current = undefined;
      snapshotEtagsRef.current.delete(targetViewKey);
      recordNotification({
        sessionId: targetViewKey,
        sessionTitle: targetRowSession.title,
        kind: 'info',
        title: 'Chat reset',
        message: 'The selected profile row was cleared.',
      });
      refreshSessionsRef.current?.();
    } catch (error) {
      transcriptResetEpochRef.current.set(
        targetViewKey,
        (transcriptResetEpochRef.current.get(targetViewKey) ?? 0) + 1,
      );
      const message = error instanceof Error ? error.message : String(error);
      setResetError(message);
      sessionsEtagRef.current = undefined;
      snapshotEtagsRef.current.delete(targetViewKey);
      recordNotification({
        sessionId: targetViewKey,
        sessionTitle: targetRowSession.title,
        kind: 'error',
        title: 'Chat reset failed',
        message,
      });
      refreshSessionsRef.current?.();
    } finally {
      setResettingViewKey(current => current === targetViewKey ? null : current);
    }
  }, [
    activeRowSession,
    activeSession?.sessionRef,
    activeViewKey,
    isCompactionInProgress,
    isLiveSession,
    isModelDefaultUpdating,
    isRunning,
    recordNotification,
    resettingViewKey,
    frontCompactingViewKey,
    updateHistoryBlocks,
    updateViewSessionRecord,
  ]);

  const frontCompactActiveSession = useCallback(async (message: MessageRecord) => {
    const sessionRef = activeSession?.sessionRef;
    const targetViewKey = activeViewKey;
    const targetProfile = profileOfViewKey(targetViewKey);
    const targetRowSession = activeRowSession;
    if (
      !isLiveSession ||
      !sessionRef ||
      !frontCompactionSettings.enabled ||
      !isActiveTranscriptUserMessage(message) ||
      isRunning ||
      isCompactionInProgress ||
      isModelDefaultUpdating ||
      resettingViewKey !== null ||
      frontCompactingViewKey !== null
    ) {
      return;
    }

    setFrontCompactingViewKey(targetViewKey);
    setFrontCompactionError(null);
    transcriptResetEpochRef.current.set(
      targetViewKey,
      (transcriptResetEpochRef.current.get(targetViewKey) ?? 0) + 1,
    );
    try {
      const result = await frontCompactSessionFromMessage(
        sessionRef,
        message.id,
        targetProfile,
      );
      hydratedMessagesRef.current.delete(targetViewKey);
      hydratedTranscriptSessionIdsRef.current.delete(targetViewKey);
      loadingOlderRef.current.delete(targetViewKey);
      setThreads(current => ({...current, [targetViewKey]: []}));
      setSessionFailures(current => removeViewKeys(current, new Set([targetViewKey])));
      setSessionPages(current => {
        if (!(targetViewKey in current)) {
          return current;
        }
        const next = {...current};
        delete next[targetViewKey];
        return next;
      });
      updateHistoryBlocks(blocks => forgetHistoryBlock(blocks, targetViewKey));
      setFrontCompactionInfoViewKey(null);
      setFrontCompactionTarget(null);
      setFrontCompactionModeViewKey(null);
      sessionsEtagRef.current = undefined;
      snapshotEtagsRef.current.delete(targetViewKey);
      recordNotification({
        sessionId: targetViewKey,
        sessionTitle: targetRowSession.title,
        kind: 'info',
        title: 'History trimmed',
        message: result.delete_mode === 'recoverable'
          ? `Removed messages were archived as ${result.archive_name}.`
          : 'The selected provider run and all later active messages were removed.',
      });
      refreshSessionsRef.current?.();
    } catch (error) {
      const messageText = error instanceof Error ? error.message : String(error);
      setFrontCompactionError(messageText);
      sessionsEtagRef.current = undefined;
      snapshotEtagsRef.current.delete(targetViewKey);
      recordNotification({
        sessionId: targetViewKey,
        sessionTitle: targetRowSession.title,
        kind: 'error',
        title: 'History trim failed',
        message: messageText,
      });
      refreshSessionsRef.current?.();
    } finally {
      setFrontCompactingViewKey(current => current === targetViewKey ? null : current);
    }
  }, [
    activeRowSession,
    activeSession?.sessionRef,
    activeViewKey,
    frontCompactionSettings.enabled,
    frontCompactingViewKey,
    isCompactionInProgress,
    isLiveSession,
    isModelDefaultUpdating,
    isRunning,
    recordNotification,
    resettingViewKey,
    updateHistoryBlocks,
  ]);

  const submitMessage = useCallback(
    (value: string, images: readonly AttachedImage[], files: readonly AttachedFile[]) => {
      const trimmedValue = value.trim();
      if (
        (!trimmedValue && images.length === 0 && files.length === 0) ||
        isRunning ||
        resettingViewKey === activeViewKey ||
        frontCompactingViewKey === activeViewKey
      ) {
        return false;
      }

      // The view's own transcript and run slot: a pinned tab must not write into
      // the session's default profile.
      const sessionId = activeViewKey;
      const sessionRef = activeSession?.sessionRef;
      const isLiveRun = activeSession?.source === 'live' && typeof sessionRef === 'string';
      const userMessage: MessageRecord = {
        id: `${sessionId}-user-${Date.now()}`,
        sender: 'user',
        text: trimmedValue,
        timestamp: messageTime(),
        ...(images.length === 0 ? {} : {sentImages: images.map(image => image.dataUrl)}),
        ...(files.length === 0 ? {} : {sentFiles: files.map(file => file.name)}),
      };

      setThreads(current => ({
        ...current,
        [sessionId]: [...(current[sessionId] ?? []), userMessage],
      }));
      updateViewSessionRecord(
        sessionId,
        session => ({...session, turns: session.turns + 1, state: 'accent'}),
        activeRowSession,
      );
      attachedRunViewKeysRef.current.delete(sessionId);
      setRunRunningForView(sessionId, true);
      setRunProgressForView(sessionId, isLiveRun ? 0 : 1);
      setStreamingMessageForView(sessionId, null);
      clearRunInteraction(sessionId);
      clearInteractionUiForView(sessionId);

      if (isLiveRun && sessionRef) {
        const controller = new AbortController();
        runAbortRefs.current.set(sessionId, controller);
        const runModel = resolveRunModelSelection(activeRowSession, runModelChoice);
        void streamSessionRun(sessionRef, buildRunInput(trimmedValue, images, files), {
          signal: controller.signal,
          ...(runModel === undefined ? {} : {model: runModel}),
          ...(activeProfile === undefined ? {} : {profile: activeProfile}),
          onRun: run => recordRunStarted(sessionId, run.turn_id),
          onInteraction: (turnId, interaction) =>
            recordRunInteraction(sessionId, turnId, interaction),
          onInteractionEnd: (turnId, end) =>
            recordInteractionEnd(sessionId, activeRowSession.title, turnId, end),
          onHookReport: (turnId, report) =>
            recordHookReport(sessionId, activeRowSession.title, turnId, report),
          onProviderRetry: (turnId, wait) => recordRunRetry(sessionId, turnId, wait),
          onReconnecting: state => recordRunReconnect(sessionId, controller, state),
          onMessage: message => {
            setStreamingMessageForView(sessionId, message);
            setRunProgressForView(sessionId, 1);
          },
          onDelta: (messageId, index, append) =>
            applyRunDelta(sessionId, messageId, index, append),
          onPart: (messageId, index, part) =>
            applyRunPart(sessionId, messageId, index, part),
          onCompleted: message => {
            setStreamingMessageForView(sessionId, message);
            setRunProgressForView(sessionId, 2);
          },
          onUsage: usage => {
            updateViewSessionRecord(
              sessionId,
              session => ({...session, usage}),
              activeRowSession,
            );
          },
        })
          .then(result => {
            if (controller.signal.aborted || runAbortRefs.current.get(sessionId) !== controller) {
              return;
            }
            if (isRunOutcomeUnknown(result)) {
              // The run's retained state expired during a reconnect. A failed run
              // leaves no transcript record, so say the outcome is unknown instead
              // of staying silent. The refresh in `finally` loads the transcript.
              recordNotification(
                runOutcomeUnknownNotification(sessionId, activeRowSession.title),
              );
              updateViewSessionRecord(
                sessionId,
                session => ({...session, state: 'warning'}),
                activeRowSession,
              );
              return;
            }
            const assistantMessage: MessageRecord = result.message ?? {
              id: `${sessionId}-assistant-${Date.now()}`,
              sender: 'assistant',
              text: 'The run completed without text output.',
              timestamp: messageTime(),
              parts: [],
              status: 'complete',
            };
            setThreads(current => {
              const existing = current[sessionId] ?? [];
              const existingIndex = existing.findIndex(
                message => message.id === assistantMessage.id,
              );
              if (existingIndex < 0) {
                return {
                  ...current,
                  [sessionId]: [...existing, assistantMessage],
                };
              }
              const next = [...existing];
              next[existingIndex] = assistantMessage;
              return {...current, [sessionId]: next};
            });
            updateViewSessionRecord(
              sessionId,
              session => ({...session, state: 'success'}),
              activeRowSession,
            );
          })
          .catch(error => {
            if (controller.signal.aborted || runAbortRefs.current.get(sessionId) !== controller) {
              return;
            }
            const message = error instanceof Error ? error.message : String(error);
            recordNotification({
              sessionId,
              sessionTitle: activeRowSession.title,
              kind: 'error',
              title: 'Run failed',
              message,
            });
            updateViewSessionRecord(
              sessionId,
              session => ({...session, state: 'warning'}),
              activeRowSession,
            );
          })
          .finally(() => {
            if (runAbortRefs.current.get(sessionId) !== controller) {
              return;
            }
            runAbortRefs.current.delete(sessionId);
            attachedRunViewKeysRef.current.delete(sessionId);
            clearRunInteraction(sessionId);
            clearInteractionUiForView(sessionId);
            setStreamingMessageForView(sessionId, null);
            setRunProgressForView(sessionId, isLiveRun ? 0 : baseReasoning.length);
            setRunRunningForView(sessionId, false);
            // The committed transcript is authoritative now: refresh instead of
            // waiting for the next poll, which this run had slowed to the
            // streaming cadence.
            refreshSessionsRef.current?.();
          });
        return true;
      }

      const timeout = window.setTimeout(() => {
        if (runTimeoutRefs.current.get(sessionId) !== timeout) return;
        runTimeoutRefs.current.delete(sessionId);
        const assistantMessage: MessageRecord = {
          id: `${sessionId}-assistant-${Date.now()}`,
          sender: 'assistant',
          text: 'The mock run is complete. The response, reasoning trace, and tool events are ready for a real agent stream to replace.\n\n**Next step:** connect the stream lifecycle to these same UI regions.',
          timestamp: messageTime(),
          reasoning: baseReasoning.map(step => ({...step, status: 'complete'})),
          tools: defaultTools,
        };

        setThreads(current => ({
          ...current,
          [sessionId]: [...(current[sessionId] ?? []), assistantMessage],
        }));
        updateViewSessionRecord(
          sessionId,
          session => ({...session, state: 'success'}),
          activeRowSession,
        );
        clearRunInteraction(sessionId);
        clearInteractionUiForView(sessionId);
        setRunProgressForView(sessionId, baseReasoning.length);
        setRunRunningForView(sessionId, false);
      }, appConfig.mock_run_delay_ms);
      runTimeoutRefs.current.set(sessionId, timeout);
      return true;
    },
    [
      activeSession,
      activeSessionId,
      activeViewKey,
      activeProfile,
      activeRowSession,
      runModelChoice,
      isRunning,
      recordNotification,
      resettingViewKey,
      frontCompactingViewKey,
      applyRunDelta,
      applyRunPart,
      recordRunStarted,
      recordInteractionEnd,
      recordHookReport,
      recordRunInteraction,
      recordRunRetry,
      recordRunReconnect,
      clearRunInteraction,
      clearInteractionUiForView,
      setRunProgressForView,
      setRunRunningForView,
      setStreamingMessageForView,
      updateViewSessionRecord,
    ],
  );

  /**
   * Re-attaches to a run that is already in flight on the server — the case
   * after a reload, where only the server still knows about it. The stream
   * replays the current snapshot of the message, then continues live.
   */
  const attachToRun = useCallback(
    (
      sessionId: string,
      sessionRef: string,
      turnId: string,
      sessionTitle: string,
      profile: string | undefined,
      fallbackSession: Session,
    ) => {
      if (!canAttachRunToView(attachedTurnRefs.current, runAbortRefs.current, sessionId, turnId)) {
        return;
      }
      attachedTurnRefs.current.set(sessionId, turnId);
      attachedRunViewKeysRef.current.add(sessionId);
      const controller = new AbortController();
      runAbortRefs.current.set(sessionId, controller);
      setRunRunningForView(sessionId, true);
      setRunProgressForView(sessionId, 1);
      setStreamingMessageForView(sessionId, null);
      clearRunInteraction(sessionId);
      clearInteractionUiForView(sessionId);
      recordRunStarted(sessionId, turnId);

      void attachSessionRun(sessionRef, {
        signal: controller.signal,
        ...(profile === undefined ? {} : {profile}),
        onRun: run => recordRunStarted(sessionId, run.turn_id),
        onInteraction: (pendingTurnId, interaction) =>
          recordRunInteraction(sessionId, pendingTurnId, interaction),
        onInteractionEnd: (endTurnId, end) =>
          recordInteractionEnd(sessionId, sessionTitle, endTurnId, end),
        onHookReport: (reportTurnId, report) =>
          recordHookReport(sessionId, sessionTitle, reportTurnId, report),
        onProviderRetry: (pendingTurnId, wait) =>
          recordRunRetry(sessionId, pendingTurnId, wait),
        onReconnecting: state => recordRunReconnect(sessionId, controller, state),
        onMessage: message => setStreamingMessageForView(sessionId, message),
        onDelta: (messageId, index, append) =>
          applyRunDelta(sessionId, messageId, index, append),
        onPart: (messageId, index, part) =>
          applyRunPart(sessionId, messageId, index, part),
        onCompleted: message => setStreamingMessageForView(sessionId, message),
        onUsage: usage => {
          updateViewSessionRecord(
            sessionId,
            session => ({...session, usage}),
            fallbackSession,
          );
        },
      })
        .then(result => {
          if (controller.signal.aborted || runAbortRefs.current.get(sessionId) !== controller) {
            return;
          }
          if (isRunOutcomeUnknown(result)) {
            // Attached, then the connection dropped and the server released the
            // run before the reconnect. Only that case sets `idle`; an attach that
            // finds nothing running stays silent.
            recordNotification(runOutcomeUnknownNotification(sessionId, sessionTitle));
            updateViewSessionRecord(
              sessionId,
              session => ({...session, state: 'warning'}),
              fallbackSession,
            );
            return;
          }
          if (result.message === undefined) {
            return;
          }
          const completed = result.message;
          setThreads(current => {
            const existing = current[sessionId] ?? [];
            if (existing.some(message => message.id === completed.id)) {
              return current;
            }
            return {...current, [sessionId]: [...existing, completed]};
          });
          updateViewSessionRecord(
            sessionId,
            session => ({...session, state: 'success'}),
            fallbackSession,
          );
        })
        .catch(error => {
          if (controller.signal.aborted || runAbortRefs.current.get(sessionId) !== controller) {
            return;
          }
          recordNotification({
            sessionId,
            sessionTitle,
            kind: 'error',
            title: 'Live run connection failed',
            message: error instanceof Error ? error.message : String(error),
          });
          updateViewSessionRecord(
            sessionId,
            session => ({...session, state: 'warning'}),
            fallbackSession,
          );
          console.warn('Unable to attach to the running session:', error);
        })
        .finally(() => {
          if (runAbortRefs.current.get(sessionId) !== controller) {
            return;
          }
          // Keep the discovered turn until the next snapshot reports that it
          // finished, preventing an attach loop while another client streams it.
          runAbortRefs.current.delete(sessionId);
          attachedRunViewKeysRef.current.delete(sessionId);
          clearRunInteraction(sessionId);
          clearInteractionUiForView(sessionId);
          setStreamingMessageForView(sessionId, null);
          setRunProgressForView(sessionId, baseReasoning.length);
          setRunRunningForView(sessionId, false);
          // The committed transcript is authoritative now: let the poll replace
          // the locally appended copy instead of waiting for the next tick.
          refreshSessionsRef.current?.();
        });
    },
    [
      applyRunDelta,
      applyRunPart,
      clearInteractionUiForView,
      clearRunInteraction,
      recordNotification,
      recordInteractionEnd,
      recordHookReport,
      recordRunInteraction,
      recordRunRetry,
      recordRunReconnect,
      recordRunStarted,
      setRunProgressForView,
      setRunRunningForView,
      setStreamingMessageForView,
      updateViewSessionRecord,
    ],
  );

  /** Move the visible composer and transcript without disturbing another view's run. */
  const activateView = useCallback((nextViewKey: string) => {
    hasChosenSessionRef.current = true;
    setIsReadmeShown(false);
    if (nextViewKey === activeViewKeyRef.current) {
      return;
    }
    setActiveViewKey(nextViewKey);
  }, []);

  /** A list click: the session's own row, following its stored default profile. */
  const selectSession = useCallback(
    (sessionId: string) => {
      activateView(viewKey(sessionId));
    },
    [activateView],
  );

  /** A profile row click: pin that profile to a row of its own and open it. */
  const openProfileView = useCallback(
    (sessionRef: string, profile: string) => {
      setPinnedViews(current =>
        addPinnedView(current, {sessionRef, profile}),
      );
      activateView(viewKey(sessionRef, profile));
    },
    [activateView],
  );

  // A server-confirmed session list owns which view caches are still useful.
  // Keep first-paint placeholders until that list arrives, then evict unpinned
  // views and sessions that disappeared. Their next visit can hydrate from the
  // authoritative transcript again.
  useEffect(() => {
    if (!hasLoadedSessionsRef.current) {
      return;
    }
    const knownIds = new Set(sessions.map(session => session.id));
    const prunedPinnedViews = prunePinnedViews(pinnedViews, sessions);
    const retainedPinnedViews = prunedPinnedViews.length === pinnedViews.length
      ? pinnedViews
      : prunedPinnedViews;
    if (retainedPinnedViews !== pinnedViews) {
      setPinnedViews(retainedPinnedViews);
      const nextActiveKey = activeKeyAfterPrune(activeViewKey, retainedPinnedViews, knownIds);
      if (nextActiveKey !== activeViewKey) {
        setActiveViewKey(nextActiveKey);
      }
    }

    const pinnedKeys = new Set(
      retainedPinnedViews.map(view => viewKey(view.sessionRef, view.profile)),
    );
    const recentKeys = nextRecentViewKeys(recentPinnedViewKeysRef.current, activeViewKey, pinnedKeys);
    recentPinnedViewKeysRef.current = recentKeys;

    // A later visit to an evicted pinned view hydrates from the transcript again.
    const retainedViewKeys = new Set(knownIds);
    for (const key of retainedPinnedViewKeys(
      pinnedKeys,
      recentKeys,
      activeViewKey,
      appConfig.session_view_cache_max,
      runningViewKeys,
    )) {
      retainedViewKeys.add(key);
    }
    const activeSessionRef = sessionRefOfViewKey(activeViewKey);
    if (knownIds.has(activeSessionRef)) {
      retainedViewKeys.add(activeViewKey);
    }
    const previousViewKeys = previousViewKeysRef.current;
    previousViewKeysRef.current = retainedViewKeys;
    if (previousViewKeys !== null) {
      forgetViewCaches(removedViewKeys(previousViewKeys, retainedViewKeys));
    }
  }, [activeViewKey, forgetViewCaches, pinnedViews, runningViewKeys, sessions]);

  useEffect(() => {
    writeStoredViews(appConfig.session_views_storage_key, {views: pinnedViews, activeViewKey});
  }, [activeViewKey, pinnedViews]);

  useEffect(() => {
    setIsModelDrawerOpen(false);
    setStateContextViewKey(null);
    setResetDrawerViewKey(null);
    setResetError(null);
    setFrontCompactionInfoViewKey(null);
    setFrontCompactionTarget(null);
    setFrontCompactionModeViewKey(null);
    setFrontCompactionError(null);
  }, [activeProfile, activeViewKey]);

  useEffect(() => {
    const previous = activeViewRef.current;
    activeViewRef.current = {viewKey: activeViewKey, ref: activeSessionRef, profile: activeProfile};
    if (
      previous.ref !== activeSessionRef ||
      previous.viewKey !== activeViewKey ||
      previous.profile !== activeProfile
    ) {
      // The list is revalidated in full on a switch — a run detached on the way
      // here may have been dropped from this client's copy of that session. The
      // view's own snapshot keeps its etag, which already covers run state.
      sessionsEtagRef.current = undefined;
    }
    activeSessionIdRef.current = activeSessionId;
    activeViewKeyRef.current = activeViewKey;
    activeProfileRef.current = activeProfile;
    // Switching to a session that has a run in flight must attach at once: the
    // transcript of that session is stale until the fresh snapshot arrives.
    refreshSessionsRef.current?.();
  }, [activeProfile, activeSessionId, activeSessionRef, activeViewKey]);

  useEffect(() => {
    activeSessionEntryRef.current = activeSession;
  }, [activeSession]);

  useEffect(() => {
    let cancelled = false;
    const sessionRefreshController = new AbortController();
    const transcriptRefreshController = new AbortController();
    // The transcript of the session being viewed, fetched separately from the
    // list because only one transcript is ever needed at a time.
    // The block is fetched only when the snapshot's reference names a copy other
    // than the one being held: it is static between compactions, so a committed
    // step never re-downloads it.
    const ensureHistory = async (
      sessionRef: string,
      uiId: string,
      reference: SessionSnapshot['history'],
      profile: string | undefined,
      signal: AbortSignal,
    ): Promise<HistoryBlock<MessageRecord> | undefined> => {
      const resetEpoch = transcriptResetEpochRef.current.get(uiId) ?? 0;
      const held = historyBlockFor(historyBlocksRef.current, uiId);
      if (reference === undefined) {
        // A page: the caller already holds the block, and it is not restated.
        return held;
      }
      if (reference.etag === null) {
        // The snapshot says there is no block. The messages go — they are gone
        // server-side too — but the answer stays as a record, so a transcript
        // refresh that gets skipped does not ask the endpoint for it again.
        updateHistoryBlocks(blocks => putHistoryBlock(blocks, uiId, {etag: null, messages: []}));
        return undefined;
      }
      if (held !== undefined && held.etag === reference.etag) {
        return held;
      }
      const result = await fetchSessionHistory(sessionRef, held?.etag ?? undefined, profile, signal);
      if (cancelled || (transcriptResetEpochRef.current.get(uiId) ?? 0) !== resetEpoch) {
        return undefined;
      }
      if (result.kind === 'ok') {
        const next = {etag: reference.etag, messages: result.value.messages};
        updateHistoryBlocks(blocks => putHistoryBlock(blocks, uiId, next));
        return next;
      }
      if (result.kind === 'not-modified' && held !== undefined) {
        const next = {...held, etag: reference.etag};
        updateHistoryBlocks(blocks => putHistoryBlock(blocks, uiId, next));
        return next;
      }
      return held;
    };

    /**
     * The block of a view whose transcript a stream owns.
     *
     * The snapshot that would name the block is the very fetch the policy skips,
     * so a view whose transcript has never been hydrated has nothing to
     * revalidate against — and waiting for the run to end would leave the row
     * without its history for as long as it streams. The endpoint itself is the
     * answer: it serves the block and names it, and the answer is recorded in
     * the shape a snapshot would have used, so no later poll asks again.
     */
    const lookUpHistoryBlock = async (
      sessionRef: string,
      uiId: string,
      profile: string | undefined,
      signal: AbortSignal,
    ): Promise<void> => {
      if (historyBlockFor(historyBlocksRef.current, uiId) !== undefined) {
        return;
      }
      const resetEpoch = transcriptResetEpochRef.current.get(uiId) ?? 0;
      const result = await fetchSessionHistory(sessionRef, undefined, profile, signal);
      if (
        cancelled ||
        (transcriptResetEpochRef.current.get(uiId) ?? 0) !== resetEpoch ||
        result.kind !== 'ok'
      ) {
        return;
      }
      const next = {etag: historyEtagFromHeader(result.etag), messages: result.value.messages};
      updateHistoryBlocks(blocks => putHistoryBlock(blocks, uiId, next));
    };

    let requestedTranscript: {
      sessionRef: string;
      uiId: string;
      profile: string | undefined;
    } | null = null;
    const transcriptRefresh = createCoalescedRefresh(async () => {
      const request = requestedTranscript;
      if (
        cancelled ||
        request === null ||
        !previousViewKeysRef.current?.has(request.uiId)
      ) {
        return;
      }
      const {sessionRef, uiId, profile} = request;
      const resetEpoch = transcriptResetEpochRef.current.get(uiId) ?? 0;
      // A client-owned run already has its optimistic turn in memory, so leave
      // it alone. A fresh page attached to a run still needs one transcript
      // snapshot; after that, the stream owns updates until the run completes.
      if (shouldSkipTranscriptLoad({
        sessionId: uiId,
        activeRunSessionId: activeRunViewKeysRef.current.has(uiId) ? uiId : null,
        attachedRunSessionId: attachedRunViewKeysRef.current.has(uiId) ? uiId : null,
        transcriptHydrated: hydratedTranscriptSessionIdsRef.current.has(uiId),
      })) {
        // The stream owns the transcript, but not the backup: a view whose first
        // snapshot never landed still needs its history, and it cannot wait for a
        // run that may stream for minutes.
        await lookUpHistoryBlock(sessionRef, uiId, profile, transcriptRefreshController.signal);
        return;
      }
      const result = await fetchSession(sessionRef, {
        etag: snapshotEtagsRef.current.get(uiId),
        ...(profile === undefined ? {} : {profile}),
        signal: transcriptRefreshController.signal,
      });
      if (
        cancelled ||
        (transcriptResetEpochRef.current.get(uiId) ?? 0) !== resetEpoch ||
        result.kind !== 'ok'
      ) {
        return;
      }
      const snapshot = result.value;
      await ensureHistory(
        sessionRef,
        uiId,
        snapshot.history,
        profile,
        transcriptRefreshController.signal,
      );
      if (
        cancelled ||
        (transcriptResetEpochRef.current.get(uiId) ?? 0) !== resetEpoch
      ) {
        return;
      }
      const messages = applyHydrated(uiId, snapshot.messages ?? []);
      setThreads(current => {
        const previous = current[uiId];
        // Reuse the existing array when the payload is structurally
        // identical: a new identity re-renders every row of that thread.
        if (previous !== undefined && deepEqual(previous, messages)) {
          return current;
        }
        return {...current, [uiId]: messages};
      });
      if (snapshot.page !== undefined) {
        setSessionPages(current => ({...current, [uiId]: snapshot.page!}));
      }
      setSessionFailures(current => {
        const failures = snapshot.failures ?? [];
        const previous = current[uiId] ?? [];
        if (deepEqual(previous, failures)) {
          return current;
        }
        if (failures.length === 0) {
          return removeViewKeys(current, new Set([uiId]));
        }
        return {...current, [uiId]: failures};
      });
      hydratedTranscriptSessionIdsRef.current.add(uiId);
      rememberSnapshotEtag(snapshotEtagsRef.current, uiId, result.etag);
      applyRowSnapshot(uiId, sessionRef, profile, snapshot);
    }, error => {
      if (!cancelled) {
        console.warn('Unable to load the session transcript:', error);
      }
    });
    const loadTranscript = (sessionRef: string, uiId: string, profile: string | undefined) => {
      requestedTranscript = {sessionRef, uiId, profile};
      transcriptRefresh.request();
    };

    const refreshSessionList = () => {
      const resetEpochsAtStart = new Map(transcriptResetEpochRef.current);
      const wasResetDuringRequest = (uiId: string) =>
        (resetEpochsAtStart.get(uiId) ?? 0) !==
        (transcriptResetEpochRef.current.get(uiId) ?? 0);
      return fetchSessions(sessionsEtagRef.current, sessionRefreshController.signal)
        .then(result => {
          if (cancelled) {
            return;
          }
          const view = activeViewRef.current;
          if (view.ref !== undefined) {
            loadTranscript(view.ref, view.viewKey, view.profile);
          }
          if (result.kind !== 'ok') {
            return;
          }
          hasLoadedSessionsRef.current = true;
          sessionsEtagRef.current = result.etag;
          const snapshots = result.value;
          const liveItems = snapshots.map(snapshot => ({
            snapshot,
            id: sessionUiId(snapshot, initialSessions),
          }));
          const activeRunViewKeys = new Set(activeRunViewKeysRef.current);
          const activeRunSessionRefs = new Set(
            [...activeRunViewKeys].map(sessionRefOfViewKey),
          );
          setThreads(current => {
            const next = {...current};
            let hasChange = false;
            for (const item of liveItems) {
              if (
                wasResetDuringRequest(item.id) ||
                activeRunViewKeys.has(item.id) ||
                view.viewKey === item.id
              ) {
                // The run's own stream and the transcript request own these.
                continue;
              }
              const messages = item.snapshot.messages;
              if (messages === undefined) {
                continue;
              }
              const previous = current[item.id];
              if (previous !== undefined && deepEqual(previous, messages)) {
                continue;
              }
              next[item.id] = messages;
              hasChange = true;
            }
            return hasChange ? next : current;
          });
          setSessions(current => {
            const liveIds = new Set(liveItems.map(item => item.id));
            const liveSessions = liveItems.map(item => {
              if (wasResetDuringRequest(item.id)) {
                return current.find(session => session.id === item.id) ??
                  sessionFromSnapshot(item.snapshot, item.id);
              }
              if (activeRunSessionRefs.has(item.id)) {
                const existing = current.find(session => session.id === item.id);
                if (existing) {
                  // The stream owns the rest of this object, but never the run:
                  // without the fresh `turn_id` a detached client cannot attach.
                  return withSnapshotRun(existing, item.snapshot);
                }
              }
              return sessionFromSnapshot(item.snapshot, item.id);
            });
            const samples = current.filter(
              session => session.source === 'sample' && !liveIds.has(session.id),
            );
            const nextSessions = [...liveSessions, ...samples];
            // Same reasoning as the threads above: the poll rebuilds every session
            // object, so keep the previous array when nothing actually changed.
            return deepEqual(current, nextSessions) ? current : nextSessions;
          });
        });
    };
    const sessionRefresh = createCoalescedRefresh(refreshSessionList, error => {
      if (!cancelled) {
        console.warn('Unable to load configured sessions:', error);
      }
    });
    const loadSessions = () => sessionRefresh.request();

    refreshSessionsRef.current = loadSessions;
    loadSessions();
    // While this client streams a run, the stream already carries every commit,
    // so the transcript only needs to catch up occasionally. The session poll is
    // different: it is the backstop that recognizes a run finishing when the
    // stream goes quiet, so it keeps the fast cadence while a run is believed
    // active — `session_refresh_interval_ms_streaming` is that cadence.
    const refreshInterval = isRunning
      ? appConfig.session_refresh_interval_ms_streaming
      : appConfig.session_refresh_interval_ms;
    const refreshTimer = window.setInterval(loadSessions, refreshInterval);
    return () => {
      cancelled = true;
      sessionRefresh.dispose();
      transcriptRefresh.dispose();
      sessionRefreshController.abort();
      transcriptRefreshController.abort();
      refreshSessionsRef.current = null;
      window.clearInterval(refreshTimer);
    };
  }, [applyHydrated, applyRowSnapshot, isRunning, updateHistoryBlocks]);

  // The active session carries the run the server is executing, so a page that
  // reloads mid-run attaches as soon as the session list has settled instead of
  // waiting for the next poll.
  useEffect(() => {
    const run = rowRunStatus(activeRow, activeSession);
    if (run === undefined || run.status !== 'running') {
      if (attachedTurnRefs.current.has(activeViewKey) && !runAbortRefs.current.has(activeViewKey)) {
        attachedTurnRefs.current.delete(activeViewKey);
      }
      return;
    }
    if (activeSession.source !== 'live' || activeSession.sessionRef === undefined) {
      return;
    }
    attachToRun(
      activeViewKey,
      activeSession.sessionRef,
      run.turn_id,
      activeRowSession.title,
      activeProfile,
      activeRowSession,
    );
  }, [activeProfile, activeRowSession, activeSession, activeRow, activeViewKey, attachToRun]);

  /**
   * The stream is the fast path to completion; the poll is the backstop.
   *
   * A connection that stops delivering frames — a sleeping laptop, a dropped
   * network — neither errors nor ends, so the stream promise stays pending and
   * the composer would stay blocked until a reload. A session snapshot names the
   * run it reports by turn, so a fresh one that says this client's own turn has
   * finished is authoritative: drop the local run rather than wait for a stream
   * that may never speak again. The committed transcript takes over.
   */
  useEffect(() => {
    const localTurnId = activeRunTurnRefs.current.get(activeViewKey);
    if (localTurnId === undefined) {
      return;
    }
    if (!snapshotEndsLocalRun(localTurnId, rowRunStatus(activeRow, activeSession))) {
      return;
    }
    detachRun(activeViewKey);
    refreshSessionsRef.current?.();
  }, [activeRow, activeSession, activeViewKey, detachRun]);

  useEffect(() => {
    return () => {
      for (const controller of runAbortRefs.current.values()) {
        controller.abort();
      }
      runAbortRefs.current.clear();
      for (const timeout of runTimeoutRefs.current.values()) {
        window.clearTimeout(timeout);
      }
      runTimeoutRefs.current.clear();
    };
  }, []);

  /**
   * Dev builds watch the composer's DOM for edits React did not make — the
   * divergence report 0005 ends in a blank page. The evidence goes to the
   * notifications, which outlive a tab the crash would otherwise empty.
   */
  useEffect(() => {
    if (!shouldInstallDesyncWatch(appConfig.composer_desync_watch)) {
      return;
    }
    const dispose = installDesyncWatch();
    const onDesync = (event: Event) => {
      const desync = (event as CustomEvent<ComposerDesync>).detail;
      recordNotification({
        sessionId: activeViewKeyRef.current,
        sessionTitle: 'Composer',
        kind: 'error',
        title: 'Composer DOM desync (dev)',
        message: describeDesync(desync),
      });
    };
    window.addEventListener(COMPOSER_DESYNC_EVENT, onDesync);
    return () => {
      window.removeEventListener(COMPOSER_DESYNC_EVENT, onDesync);
      dispose();
    };
  }, [recordNotification]);

  /** A contained composer crash is evidence too: record it where the others are. */
  const reportComposerError = useCallback(
    (error: unknown) => {
      recordNotification({
        sessionId: activeViewKeyRef.current,
        sessionTitle: 'Composer',
        kind: 'error',
        title: 'The composer was replaced after an error',
        message: error instanceof Error ? error.message : String(error),
      });
    },
    [recordNotification],
  );

  if (!activeSession) {
    return null;
  }

  /**
   * Both destructive conversation actions are refused while a run, a
   * compaction or a settings write is in flight — the conditions the two
   * toolbar buttons used to carry, now shared by the menu's items so the pair
   * cannot drift apart.
   */
  const isDestructiveActionDisabled =
    isRunning ||
    isCompactionInProgress ||
    isModelDefaultUpdating ||
    resettingViewKey !== null ||
    frontCompactingViewKey !== null;
  const isModelPickerDisabled = activeSession.sessionRef === undefined || isDestructiveActionDisabled;
  const runModelChoiceView = resolveRunModelChoice(activeRowSession, runModelChoice);

  /** Runs a `/` command the way its toolbar button does; a busy session refuses it with a notice. */
  const runSlashCommand = (name: SlashCommandName) => {
    if (isDestructiveActionDisabled) {
      recordNotification({
        sessionId: activeSessionId,
        sessionTitle: activeRowSession.title,
        kind: 'info',
        title: `/${name} is unavailable`,
        message: 'Wait for the run, compaction or settings change in progress to finish.',
      });
      return;
    }
    if (name === 'compact') {
      void compactActiveSession();
      return;
    }
    setComposerActionsViewKey(null);
    setIsUsageDrawerCollapsed(true);
    setIsModelDrawerOpen(false);
    setFrontCompactionInfoViewKey(null);
    setFrontCompactionTarget(null);
    setFrontCompactionModeViewKey(null);
    setFrontCompactionError(null);
    setResetError(null);
    if (name === 'state') {
      setResetDrawerViewKey(null);
      setStateContextViewKey(activeViewKey);
    } else {
      setStateContextViewKey(null);
      setResetDrawerViewKey(activeViewKey);
    }
  };
  const reportSlashError = (message: string) => {
    recordNotification({
      sessionId: activeSessionId,
      sessionTitle: activeRowSession.title,
      kind: 'error',
      title: 'Library item not inserted',
      message,
    });
  };

  const composerPane = (
    <ComposerPane
      activeSessionId={activeSessionId}
      draftKey={activeViewKey}
      inputHistory={inputHistory}
      onSubmit={submitMessage}
      onStop={stopRun}
      isRunning={isRunning}
      isDisabled={isReadmeShown}
      placeholder={isReadmeShown ? 'Choose a session to start chatting' : 'Ask the agent to inspect a task…'}
      showInspector={rightPanel === 'inspector'}
      onToggleInspector={toggleInspector}
      isPromptLibraryOpen={rightPanel === 'prompts'}
      onTogglePromptLibrary={togglePromptLibrary}
      onRegisterInsert={registerComposerInsert}
      slashCommands={isLiveSession && activeSession.sessionRef ? SLASH_COMMANDS : undefined}
      onSlashCommand={runSlashCommand}
      onSlashError={reportSlashError}
      drawer={
        pendingInteraction?.kind === 'approval' ? (
          <ApprovalDrawer
            pending={pendingInteraction}
            isSubmitting={answeringInteractionId === pendingInteraction.id}
            isExtending={extendingInteractionId === pendingInteraction.id}
            error={interactionAnswerError}
            reasonMaxLength={appConfig.interaction_reason_max_length}
            deadlineTickMs={appConfig.interaction_deadline_tick_ms}
            previewMaxHeightPx={appConfig.approval_preview_max_height_px}
            onAnswer={body => answerInteraction(pendingInteraction, body)}
            onExtend={() => extendInteraction(pendingInteraction)}
          />
        ) : pendingInteraction !== undefined ? (
          <DraftRequestDrawer
            pending={pendingInteraction}
            isSubmitting={answeringInteractionId === pendingInteraction.id}
            isExtending={extendingInteractionId === pendingInteraction.id}
            error={interactionAnswerError}
            reasonMaxLength={appConfig.interaction_reason_max_length}
            deadlineTickMs={appConfig.interaction_deadline_tick_ms}
            onAnswer={body => answerInteraction(pendingInteraction, body)}
            onExtend={() => extendInteraction(pendingInteraction)}
          />
        ) : isStateContextDrawerOpen ? (
          <StateContextDrawer
            isDisabled={isDestructiveActionDisabled}
            previewMaxHeightPx={appConfig.state_context_preview_max_height_px}
            onPreview={prompt => requestStateContext(prompt, true)}
            onWrite={prompt => requestStateContext(prompt, false)}
            onClose={() => setStateContextViewKey(null)}
            onLateError={message => recordNotification({
              sessionId: activeSessionId,
              sessionTitle: activeRowSession.title,
              kind: 'error',
              title: 'State context failed',
              message,
            })}
          />
        ) : isComposerActionsDrawerOpen ? (
          <ChatComposerDrawer>
            <VStack gap={1} width="100%">
              <Text type="supporting" color="secondary">
                Both actions change data in this profile row, and each asks for confirmation before it runs. Other profile rows, model settings and your unsent draft are untouched.
              </Text>
              <HStack align="center" gap={1}>
                {frontCompactionSettings.enabled ? (
                  <Button
                    label={
                      !isFrontCompactionWorkflowOpen
                        ? 'Delete a run'
                        : isFrontCompactionSelectionMode
                          ? 'Exit delete-run selection'
                          : 'Cancel delete-run workflow'
                    }
                    icon={<Icon icon={FrontCompactionIcon} size="sm" />}
                    variant="destructive"
                    size="sm"
                    isDisabled={isDestructiveActionDisabled}
                    onClick={() => {
                      setComposerActionsViewKey(null);
                      setStateContextViewKey(null);
                      setIsModelDrawerOpen(false);
                      setFrontCompactionError(null);
                      setResetDrawerViewKey(null);
                      if (isFrontCompactionWorkflowOpen) {
                        setFrontCompactionInfoViewKey(null);
                        setFrontCompactionModeViewKey(null);
                        setFrontCompactionTarget(null);
                        return;
                      }
                      setIsUsageDrawerCollapsed(true);
                      setFrontCompactionInfoViewKey(activeViewKey);
                      setFrontCompactionModeViewKey(null);
                      setFrontCompactionTarget(null);
                    }}
                  />
                ) : null}
                <Button
                  label="Reset conversation"
                  icon={<Icon icon={ResetChatIcon} size="sm" />}
                  variant="destructive"
                  size="sm"
                  isDisabled={isDestructiveActionDisabled}
                  onClick={() => {
                    setComposerActionsViewKey(null);
                    setStateContextViewKey(null);
                    setIsUsageDrawerCollapsed(true);
                    setIsModelDrawerOpen(false);
                    setFrontCompactionInfoViewKey(null);
                    setFrontCompactionTarget(null);
                    setFrontCompactionModeViewKey(null);
                    setFrontCompactionError(null);
                    setResetError(null);
                    setResetDrawerViewKey(current =>
                      current === activeViewKey ? null : activeViewKey,
                    );
                  }}
                />
                <Button
                  label="Cancel"
                  variant="ghost"
                  size="sm"
                  onClick={() => setComposerActionsViewKey(null)}
                />
              </HStack>
            </VStack>
          </ChatComposerDrawer>
        ) : isResetDrawerOpen ? (
          <ChatComposerDrawer>
            <VStack gap={1} width="100%">
              <Text type="supporting" color="secondary">
                This permanently clears the messages, compacted history, recovery archives, and transcript caches and backups for this profile row. Other profile rows, model settings, and your unsent draft are unchanged.
              </Text>
              {resetError ? (
                <Text type="supporting" color="secondary">{resetError}</Text>
              ) : null}
              <HStack align="center" gap={1}>
                <Button
                  label="Cancel"
                  variant="ghost"
                  size="sm"
                  isDisabled={isCurrentViewResetting}
                  onClick={() => setResetDrawerViewKey(null)}
                />
                <Button
                  label="Reset chat"
                  variant="destructive"
                  size="sm"
                  isLoading={isCurrentViewResetting}
                  isDisabled={
                    !isLiveSession ||
                    activeSession.sessionRef === undefined ||
                    isRunning ||
                    isCompactionInProgress ||
                    isModelDefaultUpdating ||
                    frontCompactingViewKey !== null
                  }
                  onClick={() => void resetActiveSession()}
                />
              </HStack>
            </VStack>
          </ChatComposerDrawer>
        ) : isFrontCompactionInfoDrawerOpen ? (
          <ChatComposerDrawer>
            <VStack gap={1} width="100%">
              <Text type="supporting" color="secondary">
                Choose a user message in this profile row's active transcript. Selecting one opens a confirmation to remove that provider run and all later active messages. Compacted backup history is read-only and cannot be selected.
              </Text>
              <HStack align="center" gap={1}>
                <Button
                  label="Cancel"
                  variant="ghost"
                  size="sm"
                  onClick={() => setFrontCompactionInfoViewKey(null)}
                />
                <Button
                  label="Select a user message"
                  variant="secondary"
                  size="sm"
                  isDisabled={
                    !isLiveSession ||
                    activeSession.sessionRef === undefined ||
                    !frontCompactionSettings.enabled ||
                    isRunning ||
                    isCompactionInProgress ||
                    isModelDefaultUpdating ||
                    resettingViewKey !== null ||
                    frontCompactingViewKey !== null
                  }
                  onClick={() => {
                    setFrontCompactionInfoViewKey(null);
                    setFrontCompactionModeViewKey(activeViewKey);
                    setFrontCompactionError(null);
                  }}
                />
              </HStack>
            </VStack>
          </ChatComposerDrawer>
        ) : isFrontCompactionConfirmationOpen && frontCompactionTarget ? (
          <ChatComposerDrawer>
            <VStack gap={1} width="100%">
              <Text type="supporting" color="secondary">
                This removes the provider run containing the selected user message and every later active message from this profile row only. Compacted backup history is read-only and cannot be selected here.
              </Text>
              {frontCompactionSettings.delete_mode === 'recoverable' ? (
                <Text type="supporting" color="secondary">
                  A dated JSONL archive will be saved outside the active transcript before removal.
                </Text>
              ) : (
                <Text type="supporting" color="secondary">
                  No recovery archive will be created.
                </Text>
              )}
              {frontCompactionError ? (
                <Text type="supporting" color="secondary">{frontCompactionError}</Text>
              ) : null}
              <HStack align="center" gap={1}>
                <Button
                  label="Cancel"
                  variant="ghost"
                  size="sm"
                  isDisabled={isCurrentViewFrontCompacting}
                  onClick={() => {
                    setFrontCompactionTarget(null);
                    setFrontCompactionError(null);
                  }}
                />
                <Button
                  label={frontCompactionSettings.delete_mode === 'recoverable'
                    ? 'Archive and delete from here'
                    : 'Delete from here onward'}
                  variant="destructive"
                  size="sm"
                  isLoading={isCurrentViewFrontCompacting}
                  isDisabled={
                    !isLiveSession ||
                    activeSession.sessionRef === undefined ||
                    isRunning ||
                    isCompactionInProgress ||
                    isModelDefaultUpdating ||
                    resettingViewKey !== null ||
                    frontCompactingViewKey !== null
                  }
                  onClick={() => {
                    const target = frontCompactionTarget;
                    if (target?.viewKey === activeViewKey) {
                      void frontCompactActiveSession(target.message);
                    }
                  }}
                />
              </HStack>
            </VStack>
          </ChatComposerDrawer>
        ) : isModelDrawerOpen ? (
          <ModelDrawer
            sessionName={activeRowSession.title}
            options={modelOptionsForSession(activeRowSession)}
            selectedModelId={runModelChoiceView.option?.id}
            selectedEffort={runModelChoiceView.effort}
            isDisabled={isModelPickerDisabled}
            onChange={choice => setRunModelChoice({sessionKey: activeRowSession.id, ...choice})}
          />
        ) : isUsageDrawerCollapsed ? null : (
          <UsageDetailsDrawer
            sessionName={activeRowSession.title}
            usage={activeRowSession.usage}
          />
        )
      }
      headerContextActions={
        !isReadmeShown && isLiveSession && activeSession.sessionRef ? (
          <>
            {/*
             * The two destructive actions share one trigger, and the row's right
             * end holds the compaction control first, then that trigger, then the
             * run inspector. The trigger wears the warning glyph — the icon set
             * ships no delete/trash icon — and each action still asks for
             * confirmation in the composer drawer.
             */}
            <IconButton
              label={isActiveSessionCompacting ? 'Compacting conversation history' : 'Compact conversation history'}
              icon={<Icon icon="arrowsUpDown" size="sm" />}
              variant="ghost"
              size="sm"
              isDisabled={
                isRunning ||
                isCompactionInProgress ||
                resettingViewKey !== null ||
                frontCompactingViewKey !== null
              }
              onClick={() => void compactActiveSession()}
            />
            <IconButton
              label="Conversation actions"
              tooltip="Reset this conversation or delete a run"
              icon={<Icon icon="warning" size="sm" />}
              variant={
                isComposerActionsDrawerOpen ||
                isResetDrawerOpen ||
                isFrontCompactionWorkflowOpen
                  ? 'secondary'
                  : 'ghost'
              }
              size="sm"
              onClick={() => {
                setFrontCompactionError(null);
                setResetError(null);
                setStateContextViewKey(null);
                setIsModelDrawerOpen(false);
                setComposerActionsViewKey(current =>
                  current === activeViewKey ? null : activeViewKey,
                );
              }}
            />
          </>
        ) : undefined
      }
      toolbarActions={isReadmeShown ? undefined : (
        <HStack align="center" gap={1}>
          {modelOptionsForSession(activeRowSession).length > 0 ? (
            <IconButton
              label="Model"
              tooltip={
                runModelChoiceView.option === undefined
                  ? 'Select a model'
                  : `Current model: ${runModelChoiceView.option.id}`
              }
              icon={<Icon icon="app:model" size="sm" />}
              variant={isModelDrawerOpen ? 'secondary' : 'ghost'}
              size="sm"
              onClick={() => {
                setComposerActionsViewKey(null);
                setStateContextViewKey(null);
                setResetDrawerViewKey(null);
                setFrontCompactionInfoViewKey(null);
                setFrontCompactionTarget(null);
                setFrontCompactionModeViewKey(null);
                setFrontCompactionError(null);
                setIsUsageDrawerCollapsed(true);
                setIsModelDrawerOpen(open => !open);
              }}
            />
          ) : null}
          <IconButton
            label={isUsageDrawerCollapsed ? 'Show usage details' : 'Hide usage details'}
            icon={<Icon icon="info" size="sm" />}
            variant="ghost"
            size="sm"
            onClick={() => {
              setStateContextViewKey(null);
              setIsModelDrawerOpen(false);
              setIsUsageDrawerCollapsed(collapsed => !collapsed);
            }}
          />
          {isLiveSession && activeSession.sessionRef && (
            <IconButton
              label="State context"
              tooltip="Write a state-context block with your own prompt"
              icon={<Icon icon={StateContextIcon} size="sm" />}
              variant={isStateContextDrawerOpen ? 'secondary' : 'ghost'}
              size="sm"
              isDisabled={isDestructiveActionDisabled}
              onClick={() => {
                setComposerActionsViewKey(null);
                setIsUsageDrawerCollapsed(true);
                setIsModelDrawerOpen(false);
                setResetDrawerViewKey(null);
                setFrontCompactionInfoViewKey(null);
                setFrontCompactionTarget(null);
                setFrontCompactionModeViewKey(null);
                setFrontCompactionError(null);
                setStateContextViewKey(current =>
                  current === activeViewKey ? null : activeViewKey,
                );
              }}
            />
          )}
        </HStack>
      )}
      footerNote={
        <HStack align="center" gap={2}>
          <Text type="supporting" color="secondary">
            {appConfig.composer_markdown_shortcuts
              ? 'Enter to send · Shift+Enter for a new line · Ctrl/⌘B, I, E, K to format'
              : 'Enter to send · Shift+Enter for a new line'}
          </Text>
        </HStack>
      }
    />
  );
  /**
   * The composer, contained: an error thrown while React patches its DOM must
   * take the composer instead of the page — report 0005's blank screen.
   */
  const composer = (
    <ComposerErrorBoundary onError={reportComposerError}>{composerPane}</ComposerErrorBoundary>
  );
  const centeredComposer = (
    <HStack align="stretch" justify="center" width="100%">
      <ResizeHandle
        direction="horizontal"
        isReversed
        hasDivider={false}
        isAlwaysVisible={false}
        xstyle={styles.composerResizeHandle}
        label="Resize conversation width from left border"
        resizable={conversation.props}
      >
        <VisuallyHidden>Resize conversation width from left border</VisuallyHidden>
      </ResizeHandle>
      <VStack width={conversation.size} maxWidth="100%">
        {composer}
      </VStack>
      <ResizeHandle
        direction="horizontal"
        hasDivider={false}
        isAlwaysVisible={false}
        xstyle={styles.composerResizeHandle}
        label="Resize conversation width from right border"
        resizable={conversation.props}
      >
        <VisuallyHidden>Resize conversation width from right border</VisuallyHidden>
      </ResizeHandle>
    </HStack>
  );

  return (
    <AppShell
      variant="surface"
      height="fill"
      contentPadding={0}
      sideNav={
        <SessionNavigation
          groups={listGroups}
          collapsedRefs={collapsedGroups}
          onCollapsedChange={changeGroupCollapsed}
          rowTurns={rowTurns}
          sessions={sessions}
          runningViewKeys={runningViewKeys}
          activeViewKey={isReadmeShown ? '' : activeViewKey}
          onSelect={activateView}
          onOpenProfile={openProfileView}
          openLeftPanel={leftPanel}
          onSelectLeftPanel={selectLeftPanel}
          rightPanel={rightPanel}
          unreadCount={unreadNotificationCount}
          hasUnreadError={hasUnreadError}
          onToggleNotifications={toggleNotifications}
          onToggleCheckpoints={toggleCheckpoints}
          onShowReadme={showReadme}
          isReadmeShown={isReadmeShown}
          colorScheme={colorScheme}
          onToggleColorScheme={onToggleColorScheme}
          onOpenProfileEditor={() => {
            setHasOpenedProfileEditor(true);
            setIsProfileEditorOpen(true);
          }}
        />
      }
    >
      <VStack xstyle={styles.shell}>
        {/* The quote action for a selection in the conversation. A hidden layer
            takes no room in the stack, so it sits with the shell's children. */}
        <SelectionMenu
          anchor={quoteTarget?.anchor ?? null}
          onQuote={quoteSelectionIntoComposer}
          onDismiss={dismissQuote}
        />
        <Layout
          height="fill"
          content={
            <LayoutContent
              isScrollable={false}
              padding={0}
              role="region"
              label="Agent conversation"
            >
              {/* The visual header is gone; this keeps the session title as the
                  page's top-level heading for assistive technology. The README
                  brings its own h1. */}
              {!isReadmeShown && (
                <VisuallyHidden as="div">
                  <Heading level={1}>{activeRowSession.title}</Heading>
                </VisuallyHidden>
              )}
              <StoredImageScopeProvider value={storedImageScope}>
              <ChatLayout
                density="balanced"
                xstyle={styles.chat}
                composer={centeredComposer}
                {...(isReadmeShown ? {tabIndex: 0, role: 'region', 'aria-label': 'Jad README'} : {})}
              >
                <HStack
                  align="stretch"
                  justify="center"
                  gap={0}
                  width="100%"
                  height="100%"
                  xstyle={styles.conversationFrame}
                >
                  <VStack
                    width={conversation.size}
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
                      isStreaming={isRunning && pendingInteraction === undefined}
                    >
                      {(hiddenCount > 0 || hasOlderTurns) && (
                        <EarlierMessagesControl
                          hiddenCount={hiddenCount}
                          hasMore={hasOlderTurns}
                          onReveal={revealEarlierMessages}
                        />
                      )}
                      {visibleMessages.map(message => (
                        <Fragment key={message.id}>
                          <TranscriptRow
                            message={message}
                            model={activeRowSession.model}
                            onLoadFull={loadFullMessage}
                            isFrontCompactionSelectable={isFrontCompactionSelectionMode}
                            isFrontCompactionSelected={
                              isFrontCompactionConfirmationOpen &&
                              frontCompactionTarget?.message.id === message.id
                            }
                            onSelectFrontCompaction={isFrontCompactionSelectionMode
                              ? openFrontCompactionConfirmation
                              : undefined}
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
                        (!isLiveSession || streamingMessage !== null || hasLiveAttachedRun) &&
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
                                model={activeRowSession.model}
                                isRunning={pendingInteraction === undefined}
                              />
                            }
                          >
                            {isLiveSession && streamingMessage ? (
                              <StoredImageScopeProvider value={liveImageScope}>
                                <WebUiParts
                                  parts={streamingMessage.parts}
                                  steps={streamingMessage.steps}
                                  rowWidthPx={liveBubbleWidthPx}
                                  isStreaming={pendingInteraction === undefined}
                                />
                              </StoredImageScopeProvider>
                            ) : (
                              <VStack gap={3} width="100%">
                                <ReasoningBlock
                                  steps={reasoningSteps}
                                  rowWidthPx={liveBubbleWidthPx}
                                  isFlush
                                  isStreaming={pendingInteraction === undefined}
                                />
                                <MarkdownText
                                  density="compact"
                                  contentWidth="100%"
                                  isStreaming={pendingInteraction === undefined}
                                  headingLevelStart={3}
                                >
                                  {pendingInteraction === undefined
                                    ? 'Astra is working through the request…'
                                    : 'Astra is waiting for your approval…'}
                                </MarkdownText>
                              </VStack>
                            )}
                          </ChatMessageBubble>
                        </ChatMessage>
                      )}
                      {isRunning && activeRunReconnect !== undefined && (
                        <RunReconnectNotice state={activeRunReconnect} />
                      )}
                      {isRunning && activeRunRetry !== undefined && (
                        <ProviderRetryNotice
                          wait={activeRunRetry.wait}
                          tickMs={appConfig.provider_retry_tick_ms}
                        />
                      )}
                    </ChatMessageList>
                    )}
                  </VStack>
                </HStack>
              </ChatLayout>
              </StoredImageScopeProvider>
            </LayoutContent>
          }
        />
        {leftPanel === 'search' && (
          <SessionSearchPanel
            region={leftPanelRegion}
            sessions={sessions}
            activeSessionId={activeSessionId}
            onSelectSession={sessionId => {
              selectSession(sessionId);
              setLeftPanel(null);
            }}
            onClose={() => setLeftPanel(null)}
          />
        )}
        {leftPanel === 'settings' && (
          <RunSettingsPanel
            region={leftPanelRegion}
            onClose={() => setLeftPanel(null)}
            sessionTitle={activeRowSession.title}
            sessionTurns={activeRowSession.turns}
            isLiveSession={isLiveSession}
          />
        )}
        {rightPanel === 'inspector' && (
          <RunInspector
            activeSession={activeRowSession}
            steps={reasoningSteps}
            tools={tools}
            liveToolParts={liveToolParts}
            inspector={inspector}
            isRunning={isRunning}
            onClose={() => setRightPanelMode(null)}
          />
        )}
        {rightPanel === 'notifications' && (
          <NotificationPanel
            notifications={notifications}
            region={inspector}
            onClose={() => setRightPanelMode(null)}
            onClear={() => setNotifications([])}
            onSelectSession={sessionId => {
              selectSession(sessionId);
              setRightPanelMode(null);
            }}
          />
        )}
        {rightPanel === 'prompts' && (
          <PromptLibraryPanel
            region={leftPanelRegion}
            onClose={() => setRightPanelMode(null)}
            onUsePrompt={usePrompt}
            onUseTemplate={useTemplate}
            onUseSkill={useSkill}
          />
        )}
        {rightPanel === 'checkpoints' && (
          <CheckpointPanel
            sessionId={activeSessionId}
            profile={activeProfile}
            isRunning={isRunning}
            region={inspector}
            restorePollMs={appConfig.checkpoint_restore_poll_ms}
            pathsPreviewLimit={appConfig.checkpoint_paths_preview_limit}
            pathsMaxHeightPx={appConfig.checkpoint_paths_max_height_px}
            onClose={() => setRightPanelMode(null)}
          />
        )}
      </VStack>
      {hasOpenedProfileEditor && (
        <LazyLoadBoundary name="the profile editor">
          <Suspense fallback={null}>
            <CatalogEditorDialog
              isOpen={isProfileEditorOpen}
              onClose={() => setIsProfileEditorOpen(false)}
              onChanged={() => refreshSessionsRef.current?.()}
            />
          </Suspense>
        </LazyLoadBoundary>
      )}
      <TokenRequestDialog />
    </AppShell>
  );
}

export default App;
