import {AppShell, Button, HStack, Heading, Icon, IconButton, Layout, LayoutContent, Text, VStack, VisuallyHidden} from '@astryxdesign/core';
import {useAppShellMobile} from '@astryxdesign/core/AppShell';
import {ChatComposerDrawer} from '@astryxdesign/core/Chat';
import {ResizeHandle, useResizable} from '@astryxdesign/core/Resizable';
import {pixel} from '@astryxdesign/core/Resizable/utils';
import {useToast} from '@astryxdesign/core/Toast';
import {colorVars, spacingVars} from '@astryxdesign/core/theme/tokens.stylex';
import * as stylex from '@stylexjs/stylex';
import {Fragment, Suspense, lazy, useCallback, useEffect, useMemo, useRef, useState, type ReactNode} from 'react';
import {type ActivityNotification, type NewActivityNotification, hasUnreadActivityError, isRunOutcomeUnknown, markActivityNotificationsRead, prependActivityNotification, runOutcomeUnknownNotification, unreadActivityNotificationCount} from './activity-notifications';
import {type RootLibraryAsset, type RunStreamReconnectState, type SessionPage, type SessionRunFailure, type SessionSnapshot, type WebUiInteraction, type WebUiMessage, type WebUiPart, answerSessionRunInteraction, attachSessionRun, compactSession, extendSessionRunInteraction, fetchSession, fetchSessionHistory, fetchSessionMessage, fetchSessions, frontCompactSessionFromMessage, resetSession, stopSessionRun, streamSessionRun, updateSessionProfileModelDefault, writeSessionStateContext} from './api';import {type InteractionUiState, type MessageRecord, type RunInteractionState, type RunRetryState, type Session} from './app-types';
import {interactionFailureTitle} from './approval-answer';
import {createCoalescedRefresh} from './coalesced-refresh';
import {type ColorScheme} from './color-scheme';
import {FrontCompactionIcon, ResetChatIcon, StateContextIcon} from './components/AppIcons';
import {ApprovalDrawer} from './components/ApprovalDrawer';
import {CheckpointPanel} from './components/CheckpointPanel';
import {type ComposerInsertActions} from './components/ComposerPane';
import {DraftRequestDrawer} from './components/DraftRequestDrawer';
import {FoldedPane} from './components/FoldedPane';
import {LazyLoadBoundary} from './components/LazyLoadBoundary';
import {PaneColumn} from './components/PaneColumn';
import {ModelDrawer} from './components/ModelDrawer';
import {NotificationPanel} from './components/NotificationPanel';
import {PanesPanel} from './components/PanesPanel';
import {ProfileDrawer} from './components/ProfileDrawer';
import {RunSettingsPanel} from './components/RunSettingsPanel';
import {type QuoteAnchor, SelectionMenu} from './components/SelectionMenu';
import {SessionNavigation} from './components/SessionNavigation';
import {SessionSearchPanel} from './components/SessionSearchPanel';
import {StateContextDrawer} from './components/StateContextDrawer';
import {TokenRequestDialog} from './components/TokenRequestDialog';
import {isActiveTranscriptUserMessage} from './components/TranscriptRow';
import {ViewPane} from './components/ViewPane';
import {UsageDetailsDrawer} from './components/UsageDetailsDrawer';
import appConfig from './config.json' with {type: 'json'};
import {deepEqual} from './deepEqual';
import {COMPOSER_DESYNC_EVENT, type ComposerDesync, describeDesync, installDesyncWatch, shouldInstallDesyncWatch} from './desync-watch';
import {type DraftDecision} from './draft-answer';
import {EMPTY_HISTORY_BLOCKS, type HistoryBlock, type HistoryBlocks, forgetHistoryBlock, forgetHistoryBlocks, historyBlockFor, historyEtagFromHeader, putHistoryBlock} from './history-blocks';
import {type HookReport, hookReportNotification} from './hook-report-view';
import {type AttachedFile, type AttachedImage, buildRunInput} from './image-attachments';
import {type InteractionEnd, interactionEndNotice} from './interaction-end';
import {type LeftPanelId, leftPanelAfterToggle} from './left-panel';
import {applyPart, applyPartDelta} from './messagePatch';
import {modelOptionsForSession, defaultEffortFor, formatRunModel, resolveRunModelChoice, type RunModelChoice} from './model-selection';
import {NO_PANE_DRAWERS, closePaneDrawer, hasPaneDrawer, openPaneDrawer, paneDrawerPressOpens, togglePaneDrawer, type PaneDrawerSet} from './pane-drawers';
import {canOpenPane, closePane, ensurePane, focusPane, paneCapacity, parsePaneState, prunePanes, replaceFocusedPane, replacePaneView, type PaneState} from './pane-layout';
import {allProfileRows, hasProfileToSwitchTo, type ProfileSourceRow} from './profile-rows';
import {paneAfterStep, paneShortcutFor} from './pane-shortcuts';
import {fitPaneWidths} from './pane-fold';
import {movePaneBoundary} from './pane-resize';
import {paneTitleText} from './pane-title';
import {useElementWidth} from './use-element-width';
import {type ProviderRetryWait} from './provider-retry-view';
import {type ConversationPanelId} from './right-panel';
import {selectionQuoteTarget} from './selection-target';
import {sessionFromSnapshot} from './session-mapping';
import {isSessionRunning, sessionUiStateForRun, snapshotEndsLocalRun, withSnapshotRun} from './session-run-status';
import {type PinnedView, type SessionListRow, activeKeyAfterPrune, isPinnedViewKey, nextRecentViewKeys, profileLabel, profileOfViewKey, prunePinnedViews, restoreViews, retainedPinnedViewKeys, rowProfileMessageCaller, rowRunStatus, sessionListGroups, sessionRefOfViewKey, setGroupCollapsed, viewKey, viewLabel, withPaneViewKeys} from './session-views';
import {SLASH_COMMANDS, type SlashCommandName} from './slash-commands';
import {forgetSnapshotEtags, rememberSnapshotEtag} from './snapshot-etags';
import {stopRunAfterConfirmation} from './stop-run';
import {readCollapsedGroups, readStoredPanes, readStoredViews, writeCollapsedGroups, writeStoredPanes, writeStoredViews} from './stored-views';
import {shouldSkipTranscriptLoad} from './transcript-load-policy';
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

/**
 * The arrangement this page opens with: the panes it left behind, and the single
 * configured view when it has none. A restored profile pane pins its row, so the
 * two storages cannot disagree and the first refresh keeps the pane.
 */
const INITIAL_PANES = parsePaneState(
  readStoredPanes(appConfig.conversation_panes_storage_key),
  INITIAL_VIEWS.activeViewKey,
  appConfig.conversation_panes_max,
);

function messageTime(): string {
  return new Date().toLocaleTimeString([], {
    hour: 'numeric',
    minute: '2-digit',
  });
}

const CatalogEditorDialog = lazy(() =>
  import('./components/CatalogEditorDialog').then(module => ({default: module.CatalogEditorDialog})),
);

/**
 * The commands a pane's composer controls issue. Each one belongs to a pane and
 * runs against it directly: a drawer never has to wait for that pane to hold
 * focus first.
 */
type PaneCommandKind =
  | 'profile'
  | 'model'
  | 'usage'
  | 'stateContext'
  | 'conversationActions'
  | 'compact';

/**
 * What a destructive composer command needs to act on one pane: its own view,
 * session reference, profile and record, so two panes cannot reset or compact
 * each other's conversation.
 */
type PaneActionTarget = {
  viewKey: string;
  sessionRef: string | undefined;
  profile: string | undefined;
  title: string;
  session: Session;
  isLive: boolean;
  isRunning: boolean;
};

/**
 * Reports the shell's own breakpoint up to App.
 *
 * AppShell provides the mobile context to its children, so only a component
 * rendered *inside* the shell can read it; App needs the same answer to show one
 * conversation at a time on a phone.
 */
function MobileBreakpointReporter({onBreakpoint}: {onBreakpoint: (isMobile: boolean) => void}) {
  const {isMobile} = useAppShellMobile();
  useEffect(() => {
    onBreakpoint(isMobile);
  }, [isMobile, onBreakpoint]);
  return null;
}

function App({
  colorScheme,
  onToggleColorScheme,
}: {
  colorScheme: ColorScheme;
  onToggleColorScheme: () => void;
}) {
  const showToast = useToast();
  // Below the shell's breakpoint only one conversation fits on screen.
  const [isMobile, setIsMobile] = useState(false);
  const showToastRef = useRef(showToast);
  useEffect(() => {
    showToastRef.current = showToast;
  }, [showToast]);
  const [sessions, setSessions] = useState<Session[]>([]);
  // The configured live session is a preference, not a row: the sidebar lists
  // whatever the backend reports, and this id is only used to choose which of
  // those live sessions to open on.
  // The rows this client pinned to a session, and the view on screen. A pinned
  // row is a conversation of its own, so switching between rows never disturbs
  // the transcript or the run of the one left behind.
  const [pinnedViews, setPinnedViews] = useState<PinnedView[]>(INITIAL_VIEWS.views);
  /**
   * The panes on screen: up to `conversation_panes_max` views, and the focused
   * one. One pane is today's behaviour; the arrangement is the user's own and
   * outlives a reload.
   */
  const [panes, setPanes] = useState<PaneState>(INITIAL_PANES);
  /** The panes on screen for work that runs outside a render, like the loader. */
  const paneKeysRef = useRef<readonly string[]>(panes.keys);
  if (paneKeysRef.current !== panes.keys) {
    paneKeysRef.current = panes.keys;
  }
  /** The pane the user works in: it owns the composer and the destructive actions. */
  const activeViewKey = panes.focusedKey;
  /**
   * Choosing a view in the list swaps it into the focused pane. The setter keeps
   * the state-setter shape `setActiveViewKey` had, including the updater form.
   */
  const setActiveViewKey = useCallback((update: string | ((current: string) => string)) => {
    setPanes(current => {
      const next = typeof update === 'function' ? update(current.focusedKey) : update;
      return replaceFocusedPane(current, next);
    });
  }, []);
  const activeSessionId = sessionRefOfViewKey(activeViewKey);
  const activeProfile = profileOfViewKey(activeViewKey);
  const isPinnedView = isPinnedViewKey(activeViewKey);
  const recentPinnedViewKeysRef = useRef<string[]>([]);
  const previousViewKeysRef = useRef<Set<string> | null>(null);
  if (previousViewKeysRef.current === null) {
    previousViewKeysRef.current = new Set([
      ...INITIAL_VIEWS.views.map(view => viewKey(view.sessionRef, view.profile)),
      INITIAL_VIEWS.activeViewKey,
    ]);
  }
  const [isModelDefaultUpdating, setIsModelDefaultUpdating] = useState(false);
  // A restored arrangement is the user's own: the "open the configured session"
  // preference only applies to a page that starts from nothing.
  const hasChosenSessionRef = useRef(INITIAL_VIEWS.restored);
  /**
   * Whether a session list has actually arrived from the server.
   *
   * Until it has, there are no rows at all: judging a restored tab against an
   * empty list would drop it — and that loss persisted.
   */
  const hasLoadedSessionsRef = useRef(false);
  const [threads, setThreads] = useState<Record<string, MessageRecord[]>>({});

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
  // The inspector and the prompt library belong to a conversation, so each pane
  // starts with the configured one instead of a shared panel showing it.
  const initialPanePanel: ConversationPanelId | null =
    appConfig.show_inspector_by_default
      ? 'inspector'
      : appConfig.show_prompt_library_by_default ? 'prompts' : null;
  // The Jad README stands in for the transcript until a row is chosen.
  const [isReadmeShown, setIsReadmeShown] = useState(appConfig.start_with_readme);
  const showReadme = useCallback(() => setIsReadmeShown(true), []);
  // One left slot, several datasets: session search, the running configuration
  // and the two activity panels that used to claim the app's right edge. Nothing
  // the left footer opens floats on the far side of the app, and two panels
  // never stack on one edge.
  const [leftPanel, setLeftPanel] = useState<LeftPanelId | null>(null);
  const leftPanelRef = useRef<LeftPanelId | null>(leftPanel);
  const [runningViewKeys, setRunningViewKeys] = useState<ReadonlySet<string>>(
    new Set(),
  );
  const [compactingSessionRef, setCompactingSessionRef] = useState<string | null>(null);
  const [resettingViewKey, setResettingViewKey] = useState<string | null>(null);
  // One error per view: two panes can each show their own reset confirmation.
  const [resetErrorByView, setResetErrorByView] = useState<Record<string, string>>({});
  // The delete-run workflow is armed in one pane at a time: it puts that pane's
  // transcript into selection mode, so the target is a single record carrying
  // the view it belongs to.
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
  /**
   * Every control that shows a panel in the left window hands it the view it
   * wants. The same item closes the window it already holds, another item hands
   * the window over, and opening the activity list marks it read.
   */
  const selectLeftPanel = useCallback((panel: LeftPanelId) => {
    const nextPanel = leftPanelAfterToggle(leftPanelRef.current, panel);
    leftPanelRef.current = nextPanel;
    setLeftPanel(nextPanel);
    if (nextPanel === 'notifications') {
      setNotifications(current => markActivityNotificationsRead(current));
    }
  }, []);
  /** Closing is not a toggle: the window is already gone when its own close button runs. */
  const closeLeftPanel = useCallback(() => {
    leftPanelRef.current = null;
    setLeftPanel(null);
  }, []);
  // The toast action is open-only, so a live event can never close the panel it
  // was announcing.
  const openNotifications = useCallback(() => {
    leftPanelRef.current = 'notifications';
    setLeftPanel('notifications');
    setNotifications(current => markActivityNotificationsRead(current));
  }, []);
  const recordNotification = useCallback((details: NewActivityNotification) => {
    const createdAt = Date.now();
    const id = `notification-${createdAt}-${++notificationSequenceRef.current}`;
    const notification: ActivityNotification = {
      ...details,
      id,
      createdAt,
      isRead: leftPanelRef.current === 'notifications',
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
  // Which panes are showing which composer drawer. Every drawer belongs to the
  // pane that opened it, so these are sets of view keys: opening one in pane 1
  // must not close the drawer pane 2 opened, and a drawer must stay put when
  // focus moves.
  const [usageDrawers, setUsageDrawers] = useState<PaneDrawerSet>(NO_PANE_DRAWERS);
  const [modelDrawers, setModelDrawers] = useState<PaneDrawerSet>(NO_PANE_DRAWERS);
  const [profileDrawers, setProfileDrawers] = useState<PaneDrawerSet>(NO_PANE_DRAWERS);
  const [stateContextDrawers, setStateContextDrawers] = useState<PaneDrawerSet>(NO_PANE_DRAWERS);
  const [actionsDrawers, setActionsDrawers] = useState<PaneDrawerSet>(NO_PANE_DRAWERS);
  const [resetDrawers, setResetDrawers] = useState<PaneDrawerSet>(NO_PANE_DRAWERS);
  /** Closes every drawer one pane opened, leaving the other panes' drawers alone. */
  const closePaneDrawersFor = useCallback((viewKey: string) => {
    setUsageDrawers(current => closePaneDrawer(current, viewKey));
    setModelDrawers(current => closePaneDrawer(current, viewKey));
    setProfileDrawers(current => closePaneDrawer(current, viewKey));
    setStateContextDrawers(current => closePaneDrawer(current, viewKey));
    setActionsDrawers(current => closePaneDrawer(current, viewKey));
    setResetDrawers(current => closePaneDrawer(current, viewKey));
  }, []);
  /**
   * Whether this pane shows any of its destructive drawers: the actions row, the
   * reset confirmation, or the delete-run workflow. They share one trigger, so
   * pressing it has to collapse whichever of them is showing.
   */
  const hasDestructiveDrawer = useCallback((viewKey: string): boolean =>
    hasPaneDrawer(actionsDrawers, viewKey) ||
    hasPaneDrawer(resetDrawers, viewKey) ||
    frontCompactionInfoViewKey === viewKey ||
    frontCompactionModeViewKey === viewKey ||
    frontCompactionTarget?.viewKey === viewKey,
  [actionsDrawers, resetDrawers, frontCompactionInfoViewKey, frontCompactionModeViewKey, frontCompactionTarget]);
  /** Clears this pane's drawers and the destructive workflow state behind them. */
  const closeDestructiveDrawersFor = useCallback((viewKey: string) => {
    closePaneDrawersFor(viewKey);
    setFrontCompactionInfoViewKey(null);
    setFrontCompactionModeViewKey(null);
    setFrontCompactionTarget(null);
    setFrontCompactionError(null);
  }, [closePaneDrawersFor]);
  /**
   * The state-context trigger: shows its drawer, or collapses it on a second
   * press. Either way this pane's other drawers close, and no other pane's do.
   */
  const pressStateContextDrawer = (viewKey: string): void => {
    const shouldOpen = paneDrawerPressOpens(stateContextDrawers, viewKey);
    closeDestructiveDrawersFor(viewKey);
    if (shouldOpen) {
      setStateContextDrawers(current => openPaneDrawer(current, viewKey));
    }
  };
  /**
   * The reset trigger: shows the confirmation, or collapses it on a second press.
   * It is reached from the actions row and from `/reset`.
   */
  const pressResetDrawer = (viewKey: string): void => {
    const shouldOpen = paneDrawerPressOpens(resetDrawers, viewKey);
    closeDestructiveDrawersFor(viewKey);
    if (shouldOpen) {
      setResetDrawers(current => openPaneDrawer(current, viewKey));
    }
  };
  /**
   * The actions trigger: shows the actions row, or collapses whatever destructive
   * drawer this pane is showing — a second press must close, not swap the reset
   * confirmation back for the actions row.
   */
  const pressConversationActionsDrawer = (viewKey: string): void => {
    const shouldOpen = !hasDestructiveDrawer(viewKey);
    closeDestructiveDrawersFor(viewKey);
    if (shouldOpen) {
      setActionsDrawers(current => openPaneDrawer(current, viewKey));
    }
  };
  const [streamingMessagesByView, setStreamingMessagesByView] =
    useState<Record<string, WebUiMessage>>({});
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
  // The lane renders inside the conversation frame, never wider than it, so the
  // frame's measured width is the lane's real ceiling. Without it a saved width
  // larger than the window stays in the region: the border is drawn clamped, a
  // drag spends its first pixels shrinking the invisible excess, and the border
  // does not move. Zero before the first measurement, which keeps the configured
  // maximum until there is a real one.
  const [conversationFrameRef, conversationFramePx] = useElementWidth();
  const laneMaxPx =
    conversationFramePx > 0
      ? Math.max(
          appConfig.conversation_lane_min_size,
          conversationFramePx - 2 * appConfig.conversation_lane_grab_size,
        )
      : appConfig.conversation_lane_max_size;
  const conversation = useResizable({
    defaultSize: pixel(appConfig.conversation_lane_default_size),
    minSize: pixel(appConfig.conversation_lane_min_size),
    maxSize: pixel(laneMaxPx),
    direction: 'horizontal',
    autoSaveId: 'jad-conversation-width',
  });
  // The narrower region for the side panels: session search, the run settings
  // and the two activity panels all share the left window, so the width the user
  // dragged is the width each of them opens at.
  const leftPanelRegion = useResizable({
    defaultSize: pixel(appConfig.left_panel_default_size),
    minSize: pixel(appConfig.left_panel_min_size),
    maxSize: pixel(appConfig.left_panel_max_size),
    direction: 'horizontal',
    autoSaveId: 'jad-prompt-library',
  });

  // Filled by each ComposerPane, which owns its drafts. Views outside a composer
  // write through that pane's own handle, so a prompt taken from one pane's
  // library lands in that pane's composer and not in whichever pane is focused.
  const insertIntoComposerByViewRef = useRef(new Map<string, ComposerInsertActions>());
  const insertIntoComposerFor = useCallback((viewKey: string) => (
    insertIntoComposerByViewRef.current.get(viewKey) ?? null
  ), []);
  const registerComposerInsert = useCallback((viewKey: string, insert: ComposerInsertActions | null) => {
    if (insert === null) {
      insertIntoComposerByViewRef.current.delete(viewKey);
      return;
    }
    insertIntoComposerByViewRef.current.set(viewKey, insert);
  }, []);
  const usePrompt = useCallback((viewKey: string, prompt: RootLibraryAsset) => {
    insertIntoComposerFor(viewKey)?.prompt(prompt.content, prompt.name);
  }, [insertIntoComposerFor]);
  const useTemplate = useCallback((viewKey: string, template: RootLibraryAsset) => {
    insertIntoComposerFor(viewKey)?.text(template.content);
  }, [insertIntoComposerFor]);
  const useSkill = useCallback((viewKey: string, skill: RootLibraryAsset) => {
    insertIntoComposerFor(viewKey)?.skill(skill.content, skill.name);
  }, [insertIntoComposerFor]);
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
      insertIntoComposerFor(activeViewKey)?.text(quoteTarget.draft);
    }
    setQuoteTarget(null);
  }, [quoteTarget, insertIntoComposerFor, activeViewKey]);
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

  // Sessions come from the backend, so the active id can outlive its row: on the
  // first paint (before the first poll answers) and after a session is deleted
  // server-side. Reconcile instead of rendering a phantom: the configured live
  // session when the backend has it, otherwise the first session it lists. A
  // session the user picked is never taken away from them.
  useEffect(() => {
    if (sessions.length === 0 || !hasLoadedSessionsRef.current) {
      // Nothing has arrived yet: retargeting a tab against an empty list would
      // fold the views this page just restored.
      return;
    }
    const preferredLive =
      sessions.find(session => session.id === LIVE_SESSION_ID) ?? sessions[0];
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

  // Undefined only before the first session list arrives, or when the backend
  // has no sessions at all: the shell renders nothing until a row exists.
  const activeSession: Session | undefined =
    sessions.find(session => session.id === activeSessionId) ?? sessions[0];

  const listGroups = useMemo(
    () => sessionListGroups(
      sessions.map(session => ({
        id: session.id,
        title: session.title,
        ...(session.category === undefined ? {} : {category: session.category}),
        ...(session.profiles === undefined ? {} : {profiles: session.profiles}),
        ...(session.profileNames === undefined ? {} : {profileNames: session.profileNames}),
        ...(session.activeProfile === undefined ? {} : {activeProfile: session.activeProfile}),
        ...(session.defaultProfile === undefined ? {} : {defaultProfile: session.defaultProfile}),
      })),
    ),
    [sessions],
  );
  /**
   * The rows the list shows, flattened from the same grouped model the rail
   * renders: the session's own row and one row per profile. Panes and the active
   * view are looked up here, so a profile row is a row for them too — it carries
   * its label and its run status instead of falling back to the session.
   */
  const listRows = useMemo(() => {
    const rows = new Map<string, SessionListRow>();
    for (const group of listGroups) {
      rows.set(group.ownRow.key, group.ownRow);
      for (const child of group.children) {
        if (!rows.has(child.key)) {
          rows.set(child.key, child);
        }
      }
    }
    return [...rows.values()];
  }, [listGroups]);
  const activeRow = listRows.find(row => row.key === activeViewKey);
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

  const activeRowSession: Session | undefined = !isPinnedView
    ? activeSession
    : rowSessions[activeViewKey] ??
      (activeSession === undefined
        ? undefined
        : {
        id: activeViewKey,
        sessionRef: activeSessionId,
        profile: activeProfile,
        // A pinned view restored before the first session list has no master row
        // yet: name it after the view rather than dereference nothing. The first
        // poll replaces this record with the session's own.
        title: viewLabel(
          activeSession?.title ?? activeViewKey,
          activeProfile === undefined ? undefined : profileLabel(activeProfile, activeSession?.profileNames),
        ),
        time: activeSession?.time ?? '',
        turns: 0,
        state: sessionUiStateForRun(rowRunStatus(activeRow, activeSession)?.status),
        activeProfile,
        ...(activeSession?.profiles === undefined
          ? {}
          : {profiles: activeSession.profiles}),
        ...(activeSession?.profileNames === undefined
          ? {}
          : {profileNames: activeSession.profileNames}),
      });

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

  const hasLiveSession = activeSession !== undefined;
  const isRunning = isSessionRunning(
    runningViewKeys.has(activeViewKey),
    rowRunStatus(activeRow, activeSession)?.status,
  );
  const activeSessionRef = activeSession?.sessionRef;

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
        session === undefined ||
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

  const isCompactionInProgress = compactingSessionRef !== null;
  const frontCompactionSettings = activeRowSession?.frontCompaction ?? {
    enabled: true,
    delete_mode: 'permanent' as const,
  };
  const isFrontCompactionConfirmationOpen = frontCompactionTarget?.viewKey === activeViewKey;
  const isFrontCompactionMode =
    frontCompactionModeViewKey === activeViewKey &&
    frontCompactionSettings.enabled &&
    hasLiveSession &&
    !isRunning &&
    !isCompactionInProgress &&
    !isModelDefaultUpdating &&
    resettingViewKey === null &&
    frontCompactingViewKey === null;
  const isFrontCompactionSelectionMode =
    isFrontCompactionMode && !isFrontCompactionConfirmationOpen;
  const openFrontCompactionConfirmation = useCallback((message: MessageRecord) => {
    if (
      !isFrontCompactionSelectionMode ||
      !frontCompactionSettings.enabled ||
      !hasLiveSession ||
      isRunning ||
      isCompactionInProgress ||
      isModelDefaultUpdating ||
      resettingViewKey !== null ||
      frontCompactingViewKey !== null
    ) {
      return;
    }
    closePaneDrawersFor(activeViewKey);
    setFrontCompactionInfoViewKey(null);
    setFrontCompactionError(null);
    setFrontCompactionTarget({viewKey: activeViewKey, message});
  }, [
    activeViewKey,
    frontCompactingViewKey,
    frontCompactionSettings.enabled,
    isCompactionInProgress,
    isFrontCompactionSelectionMode,
    hasLiveSession,
    isModelDefaultUpdating,
    isRunning,
    resettingViewKey,
  ]);
  const unreadNotificationCount = unreadActivityNotificationCount(notifications);
  const hasUnreadError = hasUnreadActivityError(notifications);

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
    setStreamingMessageForView(viewKey, null);
    clearRunInteraction(viewKey);
    clearInteractionUiForView(viewKey);
    setRunRunningForView(viewKey, false);
  }, [
    clearInteractionUiForView,
    clearRunInteraction,
    setRunRunningForView,
    setStreamingMessageForView,
  ]);

  /**
   * Stops the run for real. Unsubscribing only closes this client's stream —
   * the server would keep running the turn — so a stop has to be asked for.
   */
  const stopRun = useCallback((target: {viewKey: string; session: Session | undefined; title: string}) => {
    const targetViewKey = target.viewKey;
    const session = target.session;
    if (session === undefined || !session.sessionRef) {
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
          sessionTitle: target.title,
          kind: 'info',
          title: 'No active run to stop',
          message: 'The server did not find a running request for this profile. Its status was refreshed; try Stop again if it is still working.',
        });
      }
    }).catch(error => {
      stoppedHereViewKeysRef.current.delete(targetViewKey);
      recordNotification({
        sessionId: session.id,
        sessionTitle: target.title,
        kind: 'error',
        title: 'Unable to stop run',
        message: error instanceof Error ? error.message : String(error),
      });
      console.warn('Unable to stop the run:', error);
    }).finally(() => {
      stopPendingViewKeysRef.current.delete(targetViewKey);
    });
  }, [detachRun, recordNotification]);

  /**
   * Answers the question a run is waiting on.
   *
   * A refusal — the deadline passed, or the run is no longer waiting for that
   * question — is reported as a notification, and nothing is undone locally: the
   * run's own stream publishes the question's end, so the drawer closes from the
   * server's state rather than from an assumption here.
   */
  const answerInteraction = useCallback((
    target: {viewKey: string; session: Session | undefined; title: string},
    interaction: WebUiInteraction,
    body: {decision: DraftDecision; draft?: unknown; reason?: string},
  ) => {
    const session = target.session;
    const viewKey = target.viewKey;
    const profile = profileOfViewKey(viewKey);
    if (session === undefined || !session.sessionRef) {
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
        sessionTitle: target.title,
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
  const extendInteraction = useCallback((
    target: {viewKey: string; session: Session | undefined; title: string},
    interaction: WebUiInteraction,
  ) => {
    const session = target.session;
    const viewKey = target.viewKey;
    const profile = profileOfViewKey(viewKey);
    if (session === undefined || !session.sessionRef) {
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
        sessionTitle: target.title,
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
    recordNotification,
    updateInteractionUiForView,
  ]);

  /** Compacts one pane's conversation, whichever pane holds focus. */
  const compactSessionFor = useCallback(async (target: PaneActionTarget) => {
    const sessionRef = target.sessionRef;
    if (
      !target.isLive ||
      !sessionRef ||
      target.isRunning ||
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
      const result = await compactSession(sessionRef, target.profile);
      if (result.usage !== undefined) {
        updateViewSessionRecord(
          target.viewKey,
          session => ({...session, usage: result.usage}),
          target.session,
        );
      }
      const message = result.compacted
        ? `History compacted: retained ${result.retained_items} of ${result.compacted_items} items.`
        : `No compaction needed; ${result.retained_items} items retained.`;
      recordNotification({
        sessionId: sessionRefOfViewKey(target.viewKey),
        sessionTitle: target.title,
        kind: 'info',
        title: result.compacted ? 'History compacted' : 'No compaction needed',
        message,
      });
    } catch (error) {
      recordNotification({
        sessionId: sessionRefOfViewKey(target.viewKey),
        sessionTitle: target.title,
        kind: 'error',
        title: 'Compaction failed',
        message: error instanceof Error ? error.message : String(error),
      });
    } finally {
      setCompactingSessionRef(current => current === sessionRef ? null : current);
    }
  }, [
    isCompactionInProgress,
    recordNotification,
    resettingViewKey,
    frontCompactingViewKey,
    updateViewSessionRecord,
  ]);

  /** Writes a state-context block for one pane's profile. */
  const requestStateContext = useCallback(
    async (prompt: string, preview: boolean, target: PaneActionTarget) => {
      const sessionRef = target.sessionRef;
      if (!sessionRef) {
        throw new Error('no live session is selected');
      }
      const result = await writeSessionStateContext(
        sessionRef,
        {prompt, preview},
        target.profile,
      );
      if (!preview && result.written) {
        recordNotification({
          sessionId: sessionRefOfViewKey(target.viewKey),
          sessionTitle: target.title,
          kind: 'info',
          title: 'State context written',
          message: `A ${result.bytes ?? 0}-byte block will be sent with the next request.`,
        });
      }
      return result;
    },
    [recordNotification],
  );

  /**
   * Clears one pane's conversation.
   *
   * The pane is the target, not the focused view: each pane shows its own reset
   * confirmation, and the one the user confirmed is the one that must be reset.
   */
  const resetSessionFor = useCallback(async (target: PaneActionTarget) => {
    const sessionRef = target.sessionRef;
    const targetViewKey = target.viewKey;
    const targetProfile = target.profile;
    const targetRowSession = target.session;
    if (
      !target.isLive ||
      !sessionRef ||
      target.isRunning ||
      isCompactionInProgress ||
      isModelDefaultUpdating ||
      resettingViewKey !== null ||
      frontCompactingViewKey !== null
    ) {
      return;
    }

    setResettingViewKey(targetViewKey);
    setResetErrorByView(current => updateViewState(current, targetViewKey, () => undefined));
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
      setResetDrawers(current => closePaneDrawer(current, targetViewKey));
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
      setResetErrorByView(current => updateViewState(current, targetViewKey, () => message));
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
    isCompactionInProgress,
    isModelDefaultUpdating,
    recordNotification,
    resettingViewKey,
    frontCompactingViewKey,
    updateHistoryBlocks,
    updateViewSessionRecord,
  ]);

  /** Removes one pane's provider run from the selected message onward. */
  const frontCompactActiveSession = useCallback(async (message: MessageRecord, target: PaneActionTarget) => {
    const sessionRef = target.sessionRef;
    const targetViewKey = target.viewKey;
    const targetProfile = target.profile;
    const targetRowSession = target.session;
    if (
      !target.isLive ||
      !sessionRef ||
      target.session.frontCompaction?.enabled === false ||
      !isActiveTranscriptUserMessage(message) ||
      target.isRunning ||
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
    frontCompactingViewKey,
    isCompactionInProgress,
    isModelDefaultUpdating,
    recordNotification,
    resettingViewKey,
    updateHistoryBlocks,
  ]);

  /**
   * Sends from one pane.
   *
   * `target` is the pane's own view, session and run state, so a background pane
   * writes into its transcript and its run slot and nowhere else.
   */
  const submitMessage = useCallback(
    (
      target: {
        viewKey: string;
        session: Session | undefined;
        profile: string | undefined;
        title: string;
        isRunning: boolean;
      },
      value: string,
      images: readonly AttachedImage[],
      files: readonly AttachedFile[],
    ) => {
      const trimmedValue = value.trim();
      if (
        (!trimmedValue && images.length === 0 && files.length === 0) ||
        target.isRunning ||
        resettingViewKey === target.viewKey ||
        frontCompactingViewKey === target.viewKey ||
        // A row the server has not described yet has no transcript to run in:
        // the composer keeps its draft instead of writing a message nowhere.
        target.session?.sessionRef === undefined
      ) {
        return false;
      }

      // The view's own transcript and run slot: a pinned tab must not write into
      // the session's default profile.
      const sessionId = target.viewKey;
      const sessionRef = target.session?.sessionRef;
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
        target.session,
      );
      attachedRunViewKeysRef.current.delete(sessionId);
      setRunRunningForView(sessionId, true);
      setStreamingMessageForView(sessionId, null);
      clearRunInteraction(sessionId);
      clearInteractionUiForView(sessionId);

      const controller = new AbortController();
      runAbortRefs.current.set(sessionId, controller);
      void streamSessionRun(sessionRef, buildRunInput(trimmedValue, images, files), {
        signal: controller.signal,
        ...(target.profile === undefined ? {} : {profile: target.profile}),
        onRun: run => recordRunStarted(sessionId, run.turn_id),
        onInteraction: (turnId, interaction) =>
          recordRunInteraction(sessionId, turnId, interaction),
        onInteractionEnd: (turnId, end) =>
          recordInteractionEnd(sessionId, target.title, turnId, end),
        onHookReport: (turnId, report) =>
          recordHookReport(sessionId, target.title, turnId, report),
        onProviderRetry: (turnId, wait) => recordRunRetry(sessionId, turnId, wait),
        onReconnecting: state => recordRunReconnect(sessionId, controller, state),
        onMessage: message => {
          setStreamingMessageForView(sessionId, message);
        },
        onDelta: (messageId, index, append) =>
          applyRunDelta(sessionId, messageId, index, append),
        onPart: (messageId, index, part) =>
          applyRunPart(sessionId, messageId, index, part),
        onCompleted: message => {
          setStreamingMessageForView(sessionId, message);
        },
        onUsage: usage => {
          updateViewSessionRecord(
            sessionId,
            session => ({...session, usage}),
            target.session,
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
              runOutcomeUnknownNotification(sessionId, target.title),
            );
            updateViewSessionRecord(
              sessionId,
              session => ({...session, state: 'warning'}),
              target.session,
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
            target.session,
          );
        })
        .catch(error => {
          if (controller.signal.aborted || runAbortRefs.current.get(sessionId) !== controller) {
            return;
          }
          const message = error instanceof Error ? error.message : String(error);
          recordNotification({
            sessionId,
            sessionTitle: target.title,
            kind: 'error',
            title: 'Run failed',
            message,
          });
          updateViewSessionRecord(
            sessionId,
            session => ({...session, state: 'warning'}),
            target.session,
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
          setRunRunningForView(sessionId, false);
          // The committed transcript is authoritative now: refresh instead of
          // waiting for the next poll, which this run had slowed to the
          // streaming cadence.
          refreshSessionsRef.current?.();
        });
      return true;

      return true;
    },
    [
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

  /**
   * Open a view in an extra pane, keeping the panes already on screen. This is
   * what a row's own control, its menu entry and a modifier-click all call.
   */
  const openViewInPane = useCallback((nextViewKey: string) => {
    hasChosenSessionRef.current = true;
    setIsReadmeShown(false);
    setPanes(current => ensurePane(current, nextViewKey, appConfig.conversation_panes_max));
  }, []);

  /** How many panes the configuration allows, and whether one more fits. */
  const paneLimit = paneCapacity(appConfig.conversation_panes_max);
  const canOpenAnotherPane = canOpenPane(panes, paneLimit);

  /** A click inside a pane makes it the one the composer and actions belong to. */
  const focusViewPane = useCallback((nextViewKey: string) => {
    setPanes(current => focusPane(current, nextViewKey));
  }, []);

  /** Close one pane. The last one stays, so the shell always shows a conversation. */
  const closeViewPane = useCallback((nextViewKey: string) => {
    setPanes(current => closePane(current, nextViewKey));
  }, []);

  /**
   * Scroll the pane row to one pane.
   *
   * Panes share one row that scrolls when even their folded floor does not fit,
   * so a pane the user cannot see is still there. Choosing it in the left
   * window has to scroll the row to it: focusing a pane nobody can look at is
   * exactly the trap the fold exists to remove.
   */
  const revealPaneView = (viewKey: string): void => {
    const pane = [...document.querySelectorAll('[data-pane-key]')].find(
      element => element.getAttribute('data-pane-key') === viewKey,
    );
    pane?.scrollIntoView({inline: 'nearest', block: 'nearest'});
  };

  /**
   * The keyboard route through the panes: Ctrl/Cmd+Alt+Arrow moves focus along
   * the arrangement and Ctrl/Cmd+Alt+W closes the focused pane.
   *
   * Both go through the same pure state transitions the header buttons use, so a
   * pane closed by key falls back to its neighbour exactly as clicking its ✕
   * does, and the last pane cannot be closed from either.
   */
  useEffect(() => {
    if (!appConfig.pane_keyboard_shortcuts_enabled) {
      return;
    }
    const onKeyDown = (event: KeyboardEvent) => {
      const shortcut = paneShortcutFor({
        key: event.key,
        altKey: event.altKey,
        ctrlKey: event.ctrlKey,
        metaKey: event.metaKey,
        shiftKey: event.shiftKey,
      });
      if (shortcut === null) {
        return;
      }
      event.preventDefault();
      if (shortcut === 'close') {
        setPanes(current => closePane(current, current.focusedKey));
        return;
      }
      setPanes(current => focusPane(
        current,
        paneAfterStep(current.keys, current.focusedKey, shortcut === 'next' ? 1 : -1),
      ));
    };
    window.addEventListener('keydown', onKeyDown);
    return () => window.removeEventListener('keydown', onKeyDown);
  }, []);

  // A server-confirmed session list owns which view caches are still useful.
  // Wait for that list, then evict unpinned views and sessions that disappeared.
  // Their next visit can hydrate from the authoritative transcript again.
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
    // A pane whose row disappeared closes; the focused pane falls back to its
    // neighbour, exactly as closing it by hand would. The rows are the rail's own
    // model, so a profile row counts whether or not it was ever pinned.
    const rowKeys = new Set(listRows.map(row => row.key));
    for (const paneKey of panes.keys) {
      rowKeys.add(paneKey);
    }
    setPanes(current => prunePanes(current, rowKeys));
    // Every pane on screen stays warm: its transcript is hydrated and its rows
    // measured, so switching focus does not refetch.
    for (const paneKey of panes.keys) {
      pinnedKeys.add(paneKey);
    }
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
    // The warm-cache cap must not evict a pane: a key outside the retained set
    // has its snapshot load skipped, so the pane stays blank until it is focused.
    const retainedKeysWithPanes = withPaneViewKeys(retainedViewKeys, panes.keys);
    const previousViewKeys = previousViewKeysRef.current;
    previousViewKeysRef.current = retainedKeysWithPanes;
    if (previousViewKeys !== null) {
      forgetViewCaches(removedViewKeys(previousViewKeys, retainedKeysWithPanes));
    }
  }, [activeViewKey, forgetViewCaches, listRows, panes, pinnedViews, runningViewKeys, sessions]);

  useEffect(() => {
    writeStoredViews(appConfig.session_views_storage_key, {views: pinnedViews, activeViewKey});
  }, [activeViewKey, pinnedViews]);

  useEffect(() => {
    writeStoredPanes(appConfig.conversation_panes_storage_key, panes);
  }, [panes]);

  // A pane that leaves the arrangement takes its own drawers with it, so a view
  // opened again later starts with them closed.
  const previousPaneKeysRef = useRef<readonly string[]>(panes.keys);
  useEffect(() => {
    const previous = previousPaneKeysRef.current;
    previousPaneKeysRef.current = panes.keys;
    for (const key of previous) {
      if (!panes.keys.includes(key)) {
        closePaneDrawersFor(key);
        setFrontCompactionInfoViewKey(current => (current === key ? null : current));
        setFrontCompactionModeViewKey(current => (current === key ? null : current));
        setFrontCompactionTarget(current => (current?.viewKey === key ? null : current));
      }
    }
  }, [panes, closePaneDrawersFor]);

  // The delete-run workflow is armed in one pane at a time: it turns that pane's
  // transcript into selection mode, so switching away drops it rather than
  // leaving a selection mode without a pane that shows it.
  useEffect(() => {
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

    // Every visible pane hydrates its own transcript, so the pending requests are
    // kept per view: a coalesced tick loads each of them once.
    const requestedTranscripts = new Map<string, {
      sessionRef: string;
      uiId: string;
      profile: string | undefined;
    }>();
    const transcriptRefresh = createCoalescedRefresh(async () => {
      const requests = [...requestedTranscripts.values()];
      requestedTranscripts.clear();
      for (const request of requests) {
        await loadTranscriptSnapshot(request, () => cancelled);
      }
    }, error => {
      if (!cancelled) {
        console.warn('Unable to load the session transcript:', error);
      }
    });

    async function loadTranscriptSnapshot(
      request: {sessionRef: string; uiId: string; profile: string | undefined},
      isCancelled: () => boolean,
    ): Promise<void> {
      if (
        isCancelled() ||
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
    }
    const loadTranscript = (sessionRef: string, uiId: string, profile: string | undefined) => {
      requestedTranscripts.set(uiId, {sessionRef, uiId, profile});
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
          // Every pane shows a conversation, so every pane hydrates its own
          // transcript: without this a pane that was never focused stays blank
          // until someone clicks it.
          for (const paneViewKey of paneKeysRef.current) {
            if (paneViewKey === view.viewKey) {
              continue;
            }
            const paneSessionRef = sessionRefOfViewKey(paneViewKey);
            if (paneSessionRef.length === 0) {
              continue;
            }
            loadTranscript(paneSessionRef, paneViewKey, profileOfViewKey(paneViewKey));
          }
          if (result.kind !== 'ok') {
            return;
          }
          hasLoadedSessionsRef.current = true;
          sessionsEtagRef.current = result.etag;
          const snapshots = result.value;
          const liveItems = snapshots.map(snapshot => ({snapshot, id: snapshot.id}));
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
            const nextSessions = liveSessions;
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
    if (run.source !== undefined || activeSession?.sessionRef === undefined) {
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
  /**
   * Writes a pane's model default. The pane is its own target: the same control
   * in two panes must not write one profile's choice into the other.
   */
  const saveModelDefault = async (
    choice: RunModelChoice,
    target: {
      viewKey: string;
      sessionRef: string | undefined;
      profile: string | undefined;
      title: string;
    },
  ) => {
    const sessionRef = target.sessionRef;
    if (sessionRef === undefined || isModelDefaultUpdating) {
      return;
    }
    setIsModelDefaultUpdating(true);
    try {
      await updateSessionProfileModelDefault(
        sessionRef,
        formatRunModel(choice.modelId, choice.effort),
        target.profile ?? undefined,
      );
      refreshSessionsRef.current?.();
    } catch (error) {
      recordNotification({
        sessionId: sessionRefOfViewKey(target.viewKey),
        sessionTitle: target.title,
        kind: 'error',
        title: 'Model change failed',
        message: error instanceof Error ? error.message : String(error),
      });
    } finally {
      setIsModelDefaultUpdating(false);
    }
  };

  /**
   * Runs a `/` command the way its toolbar button does; a busy session refuses it
   * with a notice. The command acts on the pane it was typed in, and a command
   * pressed twice collapses the drawer it opened rather than showing it again.
   */
  const runSlashCommand = (viewKey: string, name: SlashCommandName) => {
    const pane = paneView(viewKey);
    if (isDestructiveActionDisabled) {
      recordNotification({
        sessionId: sessionRefOfViewKey(viewKey),
        sessionTitle: pane.label,
        kind: 'info',
        title: `/${name} is unavailable`,
        message: 'Wait for the run, compaction or settings change in progress to finish.',
      });
      return;
    }
    if (name === 'compact') {
      if (pane.session !== undefined) {
        void compactSessionFor(actionTargetOf(pane, pane.session));
      }
      return;
    }
    if (name === 'state') {
      pressStateContextDrawer(viewKey);
    } else {
      pressResetDrawer(viewKey);
    }
  };
  const reportSlashError = (viewKey: string, message: string) => {
    const pane = paneView(viewKey);
    recordNotification({
      sessionId: sessionRefOfViewKey(viewKey),
      sessionTitle: pane.label,
      kind: 'error',
      title: 'Library item not inserted',
      message,
    });
  };

  // The README stands in for a conversation nobody has chosen yet. Once several
  // conversations share the screen the user has chosen, so no pane gives way to it.
  const readmeInPane = isReadmeShown && panes.keys.length === 1;
  /**
   * The question a view's own run is waiting on.
   *
   * Each pane answers its own: a background run that asks for approval must be
   * answerable where it is, not only once its pane is focused. The state of
   * answering — submitting, extending, error — is per view for the same reason.
   */
  const interactionDrawerFor = (target: {viewKey: string; session: Session | undefined; title: string}) => {
    const interactionState = runInteractionStatesByView[target.viewKey];
    const interaction = interactionState?.received
      ? interactionState.pending
      : streamingMessagesByView[target.viewKey]?.pending;
    if (interaction === undefined) {
      return undefined;
    }
    const ui = interactionUiByView[target.viewKey];
    const shared = {
      pending: interaction,
      isSubmitting: ui?.answeringInteractionId === interaction.id,
      isExtending: ui?.extendingInteractionId === interaction.id,
      error: ui?.error ?? null,
      reasonMaxLength: appConfig.interaction_reason_max_length,
      deadlineTickMs: appConfig.interaction_deadline_tick_ms,
      onAnswer: (body: {decision: DraftDecision; draft?: unknown; reason?: string}) =>
        answerInteraction(target, interaction, body),
      onExtend: () => extendInteraction(target, interaction),
    };
    return interaction.kind === 'approval'
      ? <ApprovalDrawer {...shared} previewMaxHeightPx={appConfig.approval_preview_max_height_px} />
      : <DraftRequestDrawer {...shared} />;
  };
  /**
   * The drawers a pane's destructive controls open: state context, the two
   * conversation actions, the reset confirmation and the delete-run workflow.
   *
   * Each is built from its own pane and gated on that pane's view key, so a
   * confirmation stays in the pane that asked for it and its buttons act on that
   * pane's conversation, whichever pane holds focus.
   */
  const paneCommandDrawer = (pane: ReturnType<typeof paneView>): ReactNode => {
    if (pane.session === undefined) {
      return undefined;
    }
    const target = actionTargetOf(pane, pane.session);
    const settings = pane.frontCompactionSettings;
    const isResettingHere = resettingViewKey === pane.viewKey;
    const isFrontCompactingHere = frontCompactingViewKey === pane.viewKey;
    const isFrontCompactionWorkflowOpenHere =
      frontCompactionModeViewKey === pane.viewKey ||
      frontCompactionInfoViewKey === pane.viewKey ||
      frontCompactionTarget?.viewKey === pane.viewKey;
    const isDestructiveDisabled =
      pane.isRunning ||
      isCompactionInProgress ||
      isModelDefaultUpdating ||
      resettingViewKey !== null ||
      frontCompactingViewKey !== null;
    return (
        hasPaneDrawer(stateContextDrawers, pane.viewKey) ? (
          <StateContextDrawer
            isDisabled={isDestructiveDisabled}
            previewMaxHeightPx={appConfig.state_context_preview_max_height_px}
            onPreview={prompt => requestStateContext(prompt, true, target)}
            onWrite={prompt => requestStateContext(prompt, false, target)}
            onClose={() => setStateContextDrawers(current => closePaneDrawer(current, pane.viewKey))}
            onLateError={message => recordNotification({
              sessionId: sessionRefOfViewKey(pane.viewKey),
              sessionTitle: pane.label,
              kind: 'error',
              title: 'State context failed',
              message,
            })}
          />
        ) : hasPaneDrawer(actionsDrawers, pane.viewKey) ? (
          <ChatComposerDrawer>
            <VStack gap={1} width="100%">
              <Text type="supporting" color="secondary">
                Both actions change data in this profile row, and each asks for confirmation before it runs. Other profile rows, model settings and your unsent draft are untouched.
              </Text>
              <HStack align="center" gap={1}>
                {frontCompactionSettings.enabled ? (
                  <Button
                    label={
                      !isFrontCompactionWorkflowOpenHere
                        ? 'Delete a run'
                        : pane.isFrontCompactionSelectionMode
                          ? 'Exit delete-run selection'
                          : 'Cancel delete-run workflow'
                    }
                    icon={<Icon icon={FrontCompactionIcon} size="sm" />}
                    variant="destructive"
                    size="sm"
                    isDisabled={isDestructiveDisabled}
                    onClick={() => {
                      closePaneDrawersFor(pane.viewKey);
                      setFrontCompactionError(null);
                      if (isFrontCompactionWorkflowOpenHere) {
                        setFrontCompactionInfoViewKey(null);
                        setFrontCompactionModeViewKey(null);
                        setFrontCompactionTarget(null);
                        return;
                      }
                      setFrontCompactionInfoViewKey(pane.viewKey);
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
                  isDisabled={isDestructiveDisabled}
                  onClick={() => pressResetDrawer(pane.viewKey)}
                />
                <Button
                  label="Cancel"
                  variant="ghost"
                  size="sm"
                  onClick={() => setActionsDrawers(current => closePaneDrawer(current, pane.viewKey))}
                />
              </HStack>
            </VStack>
          </ChatComposerDrawer>
        ) : hasPaneDrawer(resetDrawers, pane.viewKey) ? (
          <ChatComposerDrawer>
            <VStack gap={1} width="100%">
              <Text type="supporting" color="secondary">
                This permanently clears the messages, compacted history, recovery archives, and transcript caches and backups for this profile row. Other profile rows, model settings, and your unsent draft are unchanged.
              </Text>
              {resetErrorByView[pane.viewKey] !== undefined ? (
                <Text type="supporting" color="secondary">{resetErrorByView[pane.viewKey]}</Text>
              ) : null}
              <HStack align="center" gap={1}>
                <Button
                  label="Cancel"
                  variant="ghost"
                  size="sm"
                  isDisabled={isResettingHere}
                  onClick={() => setResetDrawers(current => closePaneDrawer(current, pane.viewKey))}
                />
                <Button
                  label="Reset chat"
                  variant="destructive"
                  size="sm"
                  isLoading={isResettingHere}
                  isDisabled={
                    !pane.isLive ||
                    pane.sessionRef === undefined ||
                    pane.isRunning ||
                    isCompactionInProgress ||
                    isModelDefaultUpdating ||
                    frontCompactingViewKey !== null
                  }
                  onClick={() => void resetSessionFor(target)}
                />
              </HStack>
            </VStack>
          </ChatComposerDrawer>
        ) : frontCompactionInfoViewKey === pane.viewKey ? (
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
                    !pane.isLive ||
                    pane.sessionRef === undefined ||
                    !settings.enabled ||
                    pane.isRunning ||
                    isCompactionInProgress ||
                    isModelDefaultUpdating ||
                    resettingViewKey !== null ||
                    frontCompactingViewKey !== null
                  }
                  onClick={() => {
                    setFrontCompactionInfoViewKey(null);
                    setFrontCompactionModeViewKey(pane.viewKey);
                    setFrontCompactionError(null);
                  }}
                />
              </HStack>
            </VStack>
          </ChatComposerDrawer>
        ) : frontCompactionTarget !== null && frontCompactionTarget.viewKey === pane.viewKey ? (
          <ChatComposerDrawer>
            <VStack gap={1} width="100%">
              <Text type="supporting" color="secondary">
                This removes the provider run containing the selected user message and every later active message from this profile row only. Compacted backup history is read-only and cannot be selected here.
              </Text>
              {settings.delete_mode === 'recoverable' ? (
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
                  isDisabled={isFrontCompactingHere}
                  onClick={() => {
                    setFrontCompactionTarget(null);
                    setFrontCompactionError(null);
                  }}
                />
                <Button
                  label={settings.delete_mode === 'recoverable'
                    ? 'Archive and delete from here'
                    : 'Delete from here onward'}
                  variant="destructive"
                  size="sm"
                  isLoading={isFrontCompactingHere}
                  isDisabled={
                    !pane.isLive ||
                    pane.sessionRef === undefined ||
                    pane.isRunning ||
                    isCompactionInProgress ||
                    isModelDefaultUpdating ||
                    resettingViewKey !== null ||
                    frontCompactingViewKey !== null
                  }
                  onClick={() => {
                    if (frontCompactionTarget.viewKey === pane.viewKey) {
                      void frontCompactActiveSession(frontCompactionTarget.message, target);
                    }
                  }}
                />
              </HStack>
            </VStack>
          </ChatComposerDrawer>
        ) : null
    );
  };
  /**
   * Every profile of every session, as the session list already offers them.
   *
   * The rows are the list's own, so a profile picked here is a view the app can
   * already open: a session's stored default profile is the session's own row
   * and every other profile its own key. Building keys from the profile names
   * instead would invent a view for the default profile — a second row for the
   * same conversation, one the list does not have.
   */
  const profileDrawerRows = useMemo<ProfileSourceRow[]>(() => allProfileRows(listGroups), [listGroups]);
  /** The views already on screen: their rows are marked and cannot be picked. */
  const paneOpenViewKeys = useMemo(() => new Set(panes.keys), [panes.keys]);
  /**
   * Show another profile in one pane, in place.
   *
   * The profile may sit in this session or in any other: the pane keeps its slot
   * and its width and only its conversation changes, so a profile is reachable
   * without going back to the session list. The drawers that belonged to the old
   * view close with it: they were built for a conversation the pane no longer
   * shows.
   *
   * A view is on screen once. The drawer marks the rows that are already open
   * and does not offer them, so this branch is only a guard for any other caller:
   * a profile another pane already shows takes focus there instead of appearing
   * twice.
   */
  const switchPaneProfile = (pane: ReturnType<typeof paneView>, targetKey: string): void => {
    closePaneDrawersFor(pane.viewKey);
    if (targetKey === pane.viewKey) {
      return;
    }
    setPanes(current =>
      current.keys.includes(targetKey)
        ? focusPane(current, targetKey)
        : replacePaneView(current, pane.viewKey, targetKey),
    );
  };
  /**
   * The read-only drawers a pane's composer opens: the profile list, the model
   * picker and the usage details. Each is built from its own pane and gated on
   * that pane's view key, so opening one in a pane leaves every other pane's
   * drawer alone — and the drawer stays put when focus moves.
   */
  const paneInfoDrawer = (pane: ReturnType<typeof paneView>): ReactNode => {
    if (pane.session === undefined) {
      return undefined;
    }
    if (hasPaneDrawer(profileDrawers, pane.viewKey)) {
      return (
        <ProfileDrawer
          rows={profileDrawerRows}
          selectedKey={pane.viewKey}
          openViewKeys={paneOpenViewKeys}
          isDisabled={
            pane.sessionRef === undefined ||
            pane.isRunning ||
            isCompactionInProgress ||
            isModelDefaultUpdating ||
            resettingViewKey !== null ||
            frontCompactingViewKey !== null
          }
          onChange={targetKey => switchPaneProfile(pane, targetKey)}
        />
      );
    }
    if (hasPaneDrawer(modelDrawers, pane.viewKey)) {
      return (
        <ModelDrawer
          sessionName={pane.label}
          options={modelOptionsForSession(pane.session)}
          selectedModelId={pane.modelChoice?.option?.id}
          selectedEffort={pane.modelChoice?.effort}
          isDisabled={
            pane.sessionRef === undefined ||
            pane.isRunning ||
            isCompactionInProgress ||
            isModelDefaultUpdating ||
            resettingViewKey !== null ||
            frontCompactingViewKey !== null
          }
          onChange={choice =>
            void saveModelDefault(choice, {
              viewKey: pane.viewKey,
              sessionRef: pane.sessionRef,
              profile: pane.profile,
              title: pane.label,
            })}
        />
      );
    }
    if (hasPaneDrawer(usageDrawers, pane.viewKey)) {
      return <UsageDetailsDrawer sessionName={pane.label} usage={pane.session.usage} />;
    }
    return undefined;
  };
  /**
   * The drawer a pane's composer shows, from that pane's own state alone.
   *
   * A question its run is waiting on comes first, then the pane's destructive
   * drawers, then its read-only info drawer. Every pane builds this for itself,
   * so a drawer never moves to another pane and none of them closes when focus
   * moves.
   */
  const paneComposerChrome = (pane: ReturnType<typeof paneView>): ReactNode =>
    interactionDrawerFor({viewKey: pane.viewKey, session: pane.session, title: pane.label}) ??
    paneCommandDrawer(pane) ??
    paneInfoDrawer(pane);
  /**
   * What a pane's composer controls can be asked to do.
   *
   * Every drawer is a set of view keys, so a control acts on the pane it was
   * clicked in and nothing has to wait for a focus change first. The model picker
   * and the usage details share a pane's drawer slot: opening one closes that
   * pane's other, and only that pane's.
   */
  const runPaneCommand = (kind: PaneCommandKind, viewKey: string): void => {
    switch (kind) {
      case 'profile':
        // The three info drawers share a pane's drawer slot: opening one closes
        // that pane's other two, and only that pane's.
        setModelDrawers(current => closePaneDrawer(current, viewKey));
        setUsageDrawers(current => closePaneDrawer(current, viewKey));
        setProfileDrawers(current => togglePaneDrawer(current, viewKey));
        return;
      case 'model':
        setProfileDrawers(current => closePaneDrawer(current, viewKey));
        setUsageDrawers(current => closePaneDrawer(current, viewKey));
        setModelDrawers(current => togglePaneDrawer(current, viewKey));
        return;
      case 'usage':
        setModelDrawers(current => closePaneDrawer(current, viewKey));
        setProfileDrawers(current => closePaneDrawer(current, viewKey));
        setUsageDrawers(current => togglePaneDrawer(current, viewKey));
        return;
      case 'stateContext':
        pressStateContextDrawer(viewKey);
        return;
      case 'conversationActions':
        pressConversationActionsDrawer(viewKey);
        return;
      case 'compact': {
        const pane = paneView(viewKey);
        if (pane.session !== undefined) {
          void compactSessionFor(actionTargetOf(pane, pane.session));
        }
        return;
      }
    }
  };
  /** A pane's control runs its own command; clicking it never acts on another pane. */
  const requestPaneCommand = (viewKey: string, kind: PaneCommandKind): void => {
    focusViewPane(viewKey);
    runPaneCommand(kind, viewKey);
  };

  /** From the composer element: headerContextActions, for one pane. */
  const composerHeaderActions = (pane: ReturnType<typeof paneView>) => {
    if (readmeInPane || !pane.isLive || pane.sessionRef === undefined) {
      return undefined;
    }
    const isBlocked =
      pane.isRunning ||
      isCompactionInProgress ||
      isModelDefaultUpdating ||
      resettingViewKey !== null ||
      frontCompactingViewKey !== null;
    // The trigger shows its own pane's drawers, so a pane that is not focused
    // still shows that its actions or reset confirmation is open.
    const isActionsOpen =
      hasPaneDrawer(actionsDrawers, pane.viewKey) ||
      hasPaneDrawer(resetDrawers, pane.viewKey) ||
      frontCompactionModeViewKey === pane.viewKey ||
      frontCompactionInfoViewKey === pane.viewKey ||
      frontCompactionTarget?.viewKey === pane.viewKey;
    return (
      <>
        {/*
         * The two destructive actions share one trigger, and the row's right
         * end holds the compaction control first, then that trigger, then the
         * run inspector. The trigger wears the warning glyph — the icon set
         * ships no delete/trash icon — and each action still asks for
         * confirmation in the composer drawer.
         */}
        <IconButton
          label={pane.isCompacting ? 'Compacting conversation history' : 'Compact conversation history'}
          icon={<Icon icon="arrowsUpDown" size="sm" />}
          variant="ghost"
          size="sm"
          isDisabled={isBlocked}
          onClick={() => requestPaneCommand(pane.viewKey, 'compact')}
        />
        <IconButton
          label="Conversation actions"
          tooltip="Reset this conversation or delete a run"
          icon={<Icon icon="warning" size="sm" />}
          variant={isActionsOpen ? 'secondary' : 'ghost'}
          size="sm"
          onClick={() => requestPaneCommand(pane.viewKey, 'conversationActions')}
        />
      </>
    );
  };
  /** From the composer element: toolbarActions. */
  /** From the composer element: toolbarActions, for one pane. */
  const composerToolbar = (pane: ReturnType<typeof paneView>) => {
    if (readmeInPane) {
      return undefined;
    }
    const options = pane.session === undefined ? [] : modelOptionsForSession(pane.session);
    const choice = pane.modelChoice;
    const isBlocked =
      pane.isRunning ||
      isCompactionInProgress ||
      isModelDefaultUpdating ||
      resettingViewKey !== null ||
      frontCompactingViewKey !== null;
    // Each icon shows its own pane's drawer, whatever pane holds focus.
    const isProfileOpenHere = hasPaneDrawer(profileDrawers, pane.viewKey);
    const isModelOpenHere = hasPaneDrawer(modelDrawers, pane.viewKey);
    const isUsageOpenHere = hasPaneDrawer(usageDrawers, pane.viewKey);
    const isStateContextOpenHere = hasPaneDrawer(stateContextDrawers, pane.viewKey);
    return (
        <HStack align="center" gap={1}>
          {hasProfileToSwitchTo(profileDrawerRows, pane.viewKey) ? (
            <IconButton
              label="Profile"
              tooltip={
                pane.profile === undefined
                  ? 'Switch this conversation to another profile'
                  : `Current profile: ${pane.profile}`
              }
              icon={<Icon icon="app:profile" size="sm" />}
              variant={isProfileOpenHere ? 'secondary' : 'ghost'}
              size="sm"
              onClick={() => requestPaneCommand(pane.viewKey, 'profile')}
            />
          ) : null}
          {options.length > 0 ? (
            <IconButton
              label="Model"
              tooltip={
                choice?.option === undefined
                  ? 'Select a model'
                  : `Current model: ${choice.option.id}`
              }
              icon={<Icon icon="app:model" size="sm" />}
              variant={isModelOpenHere ? 'secondary' : 'ghost'}
              size="sm"
              onClick={() => requestPaneCommand(pane.viewKey, 'model')}
            />
          ) : null}
          <IconButton
            label={isUsageOpenHere ? 'Hide usage details' : 'Show usage details'}
            icon={<Icon icon="info" size="sm" />}
            variant="ghost"
            size="sm"
            onClick={() => requestPaneCommand(pane.viewKey, 'usage')}
          />
          {pane.isLive && pane.sessionRef !== undefined && (
            <IconButton
              label="State context"
              tooltip="Write a state-context block with your own prompt"
              icon={<Icon icon={StateContextIcon} size="sm" />}
              variant={isStateContextOpenHere ? 'secondary' : 'ghost'}
              size="sm"
              isDisabled={isBlocked}
              onClick={() => requestPaneCommand(pane.viewKey, 'stateContext')}
            />
          )}
        </HStack>
      );
  };
  /** From the composer element: footerNote. */
  const composerFooter = (
        <HStack align="center" gap={2}>
          <Text type="supporting" color="secondary">
            {appConfig.composer_markdown_shortcuts
              ? 'Enter to send · Shift+Enter for a new line · Ctrl/⌘B, I, E, K to format'
              : 'Enter to send · Shift+Enter for a new line'}
          </Text>
        </HStack>
  );
  /**
   * Everything one pane derives from its own view key.
   *
   * This mirrors the active-view values above instead of replacing them: the
   * shell (rail, right panel, notifications) still reads the focused view, and a
   * pane never reads another pane's run.
   */
  const paneView = (key: string) => {
    const row = listRows.find(candidate => candidate.key === key);
    const sessionRef = sessionRefOfViewKey(key);
    // The session record carries the per-profile run states; the pinned row's own
    // record carries its transcript facts. App reads run state from the session.
    const sessionRecord = sessions.find(session => session.id === sessionRef);
    const record = rowSessions[key] ?? sessionRecord;
    const profile = profileOfViewKey(key);
    const runStatus = rowRunStatus(row, sessionRecord)?.status;
    const isLive = record !== undefined;
    const isRunning = isSessionRunning(runningViewKeys.has(key), runStatus);
    const profileMessageCaller = rowProfileMessageCaller(row, sessionRecord, runningViewKeys);
    // The model the next run uses: the pane's title strip names its provider and
    // id, and the effort it will run at (the stored pick, else the option's own).
    const modelChoice = record === undefined ? undefined : resolveRunModelChoice(record);
    const streamingMessage = streamingMessagesByView[key] ?? null;
    const interactionState = runInteractionStatesByView[key];
    const pending = interactionState?.received ? interactionState.pending : streamingMessage?.pending;
    const settings = record?.frontCompaction ?? {enabled: true, delete_mode: 'permanent' as const};
    const isFrontCompactionMode =
      frontCompactionModeViewKey === key &&
      settings.enabled &&
      isLive &&
      !isRunning &&
      !isCompactionInProgress &&
      !isModelDefaultUpdating &&
      resettingViewKey === null &&
      frontCompactingViewKey === null;
    const isFrontCompactionConfirmationOpen = frontCompactionTarget?.viewKey === key;
    return {
      viewKey: key,
      label: row?.label ?? sessionRefOfViewKey(key),
      session: record,
      sessionRef: record?.sessionRef,
      profile,
      isLive,
      isRunning,
      profileMessageCaller,
      isCompacting: record?.sessionRef !== undefined && compactingSessionRef === record.sessionRef,
      modelChoice,
      isStreaming: isRunning && pending === undefined,
      pendingApproval: isRunning && pending !== undefined,
      effort: modelChoice?.effort,
      provider: modelChoice?.option?.provider,
      model: modelChoice?.option?.id,
      effortLabel:
        modelChoice === undefined
          ? undefined
          : modelChoice.effort ??
            (modelChoice.option === undefined ? undefined : defaultEffortFor(modelChoice.option)),
      hasLiveAttachedRun:
        isLive &&
        profileMessageCaller === undefined &&
        (runStatus === 'running' || attachedRunViewKeysRef.current.has(key)),
      streamingMessage,
      isFrontCompactionSelectionMode: isFrontCompactionMode && !isFrontCompactionConfirmationOpen,
      isFrontCompactionConfirmationOpen,
      frontCompactionSettings: settings,
    };
  };
  /** What a pane's destructive commands need: its own view, session and guards. */
  const actionTargetOf = (
    pane: ReturnType<typeof paneView>,
    session: Session,
  ): PaneActionTarget => ({
    viewKey: pane.viewKey,
    sessionRef: pane.sessionRef,
    profile: pane.profile,
    title: pane.label,
    session,
    isLive: pane.isLive,
    isRunning: pane.isRunning,
  });
  /**
   * The panes on screen. A phone shows one conversation at a time: the focused
   * pane keeps its place in the arrangement, and the others stay mounted but are
   * not painted, so a background run keeps streaming into a transcript that is
   * ready the moment its number is tapped.
   */
  const shownPaneKeys = isMobile && panes.keys.includes(activeViewKey) ? [activeViewKey] : panes.keys;
  // One pane keeps the configured lane width; several panes share the whole area,
  // because a pane narrower than the floor cannot show a conversation.
  const paneGroupWidth = shownPaneKeys.length > 1 ? '100%' : conversation.size;
  // What the panes would each hold, and how much room the row actually has. The
  // fit rule turns those into the widths they render at: every pane keeps the
  // fold floor, as many as the room allows reach the usable minimum (largest
  // first), and nothing is pushed off the row.
  const [paneGroupRef, paneGroupMeasuredPx] = useElementWidth();
  const [paneOwnWidths, setPaneOwnWidths] = useState<Record<number, number>>({});
  const reportPaneOwnWidth = useCallback((index: number, widthPx: number) => {
    setPaneOwnWidths(current => (current[index] === widthPx ? current : {...current, [index]: widthPx}));
  }, []);
  const paneFittedWidths = shownPaneKeys.length > 1
    ? fitPaneWidths(
        shownPaneKeys.map((_, index) => paneOwnWidths[index] ?? appConfig.conversation_pane_default_size),
        paneGroupMeasuredPx,
        appConfig.conversation_pane_min_size,
        appConfig.conversation_pane_fold_width,
        // The focused pane is the one holding the composer, so the room goes to
        // it first: a folded pane must never be the one you are typing into.
        shownPaneKeys.indexOf(activeViewKey),
      )
    : [];
  // A drag moves one boundary, so its result has to outrank the fit rule until
  // the row changes shape: otherwise the next fit would redistribute the row and
  // the pane left of the divider would move too.
  const [paneDragWidths, setPaneDragWidths] = useState<number[] | null>(null);
  const paneWidths = paneDragWidths ?? paneFittedWidths;
  const paneResizeSettersRef = useRef(new Map<number, (widthPx: number) => void>());
  const paneDragBaseRef = useRef<number[] | null>(null);
  /** The last widths the drag produced, so the release commits what is on screen. */
  const paneDragWidthsRef = useRef<number[] | null>(null);
  const registerPaneResize = useCallback((index: number, resize: (widthPx: number) => void) => {
    paneResizeSettersRef.current.set(index, resize);
  }, []);
  /** A divider drag starts from the widths on screen, not from the stored ones. */
  const startPaneDrag = (index: number): void => {
    paneDragBaseRef.current = paneWidths;
    void index;
  };
  /**
   * Drag one divider: the pane on its left grows by exactly what the panes on its
   * right give up, and every other pane on the row stays where it is.
   *
   * Moves are ignored unless a gesture is actually open. The pointer follow runs
   * on `window`, so a stray move after the release would otherwise keep resizing
   * the row with no button held.
   */
  const dragPaneBoundary = (index: number, delta: number): void => {
    const base = paneDragBaseRef.current;
    if (base === null || base.length < 2) {
      return;
    }
    const moved = movePaneBoundary(
      base,
      index,
      delta,
      appConfig.conversation_pane_min_size,
      appConfig.conversation_pane_fold_width,
    );
    paneDragWidthsRef.current = moved;
    setPaneDragWidths(moved);
  };
  /** Commit the moved widths back to each pane, so they persist and the row agrees. */
  const endPaneDrag = (): void => {
    if (paneDragBaseRef.current === null) {
      return;
    }
    paneDragBaseRef.current = null;
    const committed = paneDragWidthsRef.current;
    paneDragWidthsRef.current = null;
    // A keyboard resize moves the pane's own size and produces no boundary move;
    // committing the fitted widths there would undo the step.
    if (committed === null) {
      return;
    }
    committed.forEach((widthPx, index) => paneResizeSettersRef.current.get(index)?.(widthPx));
  };
  // A different row shape (another pane, another window) is what the fit rule is
  // for; the drag result stops applying.
  useEffect(() => {
    paneDragWidthsRef.current = null;
    paneDragBaseRef.current = null;
    setPaneDragWidths(null);
  }, [panes.keys.length, paneGroupMeasuredPx]);

  if (!activeSession || !activeRowSession) {
    // No session list yet — the first poll has not answered, the server has no
    // sessions, or it refused the request. There is no row to lay the shell out
    // around, so say so rather than paint an empty page, and keep the token
    // prompt reachable: it is the only way a refused tab can recover. This sits
    // below every hook: an early return above one would change the hook order
    // between renders.
    return (
      <VStack align="center" justify="center" gap={2} height="100vh" width="100%">
        <Text type="label" weight="semibold">
          No sessions
        </Text>
        <Text type="supporting" color="secondary">
          The server has not listed a session yet. It appears here as soon as it answers.
        </Text>
        <TokenRequestDialog />
      </VStack>
    );
  }

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
          activeViewKey={readmeInPane ? '' : activeViewKey}
          onSelect={activateView}
          onOpenPane={openViewInPane}
          canOpenPane={canOpenAnotherPane}
          paneLimit={paneLimit}
          paneCount={panes.keys.length}
          openLeftPanel={leftPanel}
          onSelectLeftPanel={selectLeftPanel}
          unreadCount={unreadNotificationCount}
          hasUnreadError={hasUnreadError}
          onShowReadme={showReadme}
          isReadmeShown={isReadmeShown}
          colorScheme={colorScheme}
          onToggleColorScheme={onToggleColorScheme}
          paneViews={panes.keys.map(paneViewKey => ({viewKey: paneViewKey, label: paneView(paneViewKey).label}))}
          activePaneIndex={panes.keys.indexOf(activeViewKey)}
          onFocusPane={focusViewPane}
          onOpenProfileEditor={() => {
            setHasOpenedProfileEditor(true);
            setIsProfileEditorOpen(true);
          }}
        />
      }
    >
      <VStack xstyle={styles.shell}>
        <MobileBreakpointReporter onBreakpoint={setIsMobile} />
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
              <HStack
                ref={conversationFrameRef}
                align="stretch"
                justify="center"
                gap={0}
                width="100%"
                height="100%"
                xstyle={styles.conversationFrame}
                // A single pane's two dividers sit just outside the lane, and each
                // grab zone is wider than its line. Without room reserved for it a
                // lane that reaches the frame edge puts both zones past the edge —
                // outside the visible area, where nothing can be seen or clicked —
                // so the frame keeps the grab width free on each side exactly when
                // those dividers are shown.
                style={
                  shownPaneKeys.length === 1 && !isMobile
                    ? {paddingInline: appConfig.conversation_lane_grab_size}
                    : undefined
                }
              >
                {/* The handles size one lane. With several panes the panes share
                    the whole area, so a lane width would only fight them; a phone
                    has no room to drag a lane, so they are desktop-only. */}
                {shownPaneKeys.length === 1 && !isMobile && (
                  <ResizeHandle
                    direction="horizontal"
                    isReversed
                    hasDivider={false}
                    xstyle={styles.composerResizeHandle}
                    // The grab zone is wider than the divider line, and it has to
                    // sit on the far side of it: this handle's other side is the
                    // transcript, whose scrollbar is flush against the lane's right
                    // edge and would lose its clicks to the handle's zone.
                    pillPlacement="start"
                    label="Resize conversation width from left border"
                    resizable={conversation.props}
                  />
                )}
                <HStack
                  ref={paneGroupRef}
                  align="stretch"
                  gap={0}
                  height="100%"
                  width={paneGroupWidth}
                  maxWidth="100%"
                  // When even the folded floor does not fit, the row scrolls
                  // instead of pushing a pane off the screen: every pane stays
                  // reachable, which is the promise the fold makes.
                  style={shownPaneKeys.length > 1 ? {overflowX: 'auto'} : undefined}
                  data-pane-group=""
                >
                  {panes.keys.map((paneViewKey, index) => {
                    const pane = paneView(paneViewKey);
                    const isFocusedPane = paneViewKey === activeViewKey;
                    const paneNode = (
                      <ViewPane
                        viewKey={paneViewKey}
                        label={pane.label}
                        session={pane.session}
                        profile={pane.profile}
                        isLive={pane.isLive}
                        isRunning={pane.isRunning}
                        profileMessageCaller={pane.profileMessageCaller}
                        isStreaming={pane.isStreaming}
                        pendingApproval={pane.pendingApproval}
                        effort={pane.effort}
                        provider={pane.provider}
                        model={pane.model}
                        effortLabel={pane.effortLabel}
                        hasLiveAttachedRun={pane.hasLiveAttachedRun}
                        isFocused={isFocusedPane}
                        isReadmeShown={readmeInPane && isFocusedPane}
                        isHidden={!shownPaneKeys.includes(paneViewKey)}
                        isDivided={shownPaneKeys.length > 1 && index > 0}
                        // Every pane shows its tab title strip: the session, its
                        // profile and the provider, model and effort of its next
                        // run, whether one pane or four are on screen.
                        showHeader
                        composerSessionId={sessionRefOfViewKey(paneViewKey)}
                        onSubmitComposer={(value, images, files) =>
                          submitMessage(
                            {
                              viewKey: paneViewKey,
                              session: pane.session,
                              profile: pane.profile,
                              title: pane.label,
                              isRunning: pane.isRunning,
                            },
                            value,
                            images,
                            files,
                          )}
                        onStopComposer={() =>
                          stopRun({
                            viewKey: paneViewKey,
                            session: pane.session,
                            title: pane.label,
                          })}
                        isComposerDisabled={readmeInPane}
                        placeholder={readmeInPane
                          ? 'Choose a session to start chatting'
                          : 'Ask the agent to inspect a task…'}
                        defaultPanel={index === 0 ? initialPanePanel : null}
                        panelStorageId={`${appConfig.conversation_pane_panel_storage_prefix}-${index}`}
                        // One conversation puts its panel on the window's right
                        // edge, the way the single-pane app anchors it.
                        isPanelAtWindowEdge={
                          shownPaneKeys.length === 1 &&
                          appConfig.conversation_pane_panel_window_edge
                        }
                        onUsePrompt={asset => usePrompt(paneViewKey, asset)}
                        onUseTemplate={asset => useTemplate(paneViewKey, asset)}
                        onUseSkill={asset => useSkill(paneViewKey, asset)}
                        onRegisterComposerInsert={registerComposerInsert}
                        slashCommands={pane.isLive && pane.session?.sessionRef !== undefined
                          ? SLASH_COMMANDS
                          : undefined}
                        onSlashCommand={name => runSlashCommand(paneViewKey, name)}
                        onSlashError={message => reportSlashError(paneViewKey, message)}
                        composerChrome={paneComposerChrome(pane)}
                        composerToolbar={composerToolbar(pane)}
                        composerHeaderActions={composerHeaderActions(pane)}
                        composerFooter={composerFooter}
                        onComposerError={reportComposerError}
                        threads={threads}
                        historyBlocks={historyBlocks}
                        failures={sessionFailures[paneViewKey]}
                        streamingMessage={pane.streamingMessage}
                        reconnect={runReconnectsByView[paneViewKey]}
                        retry={runRetriesByView[paneViewKey]}
                        page={sessionPages[paneViewKey]}
                        isFrontCompactionSelectionMode={pane.isFrontCompactionSelectionMode}
                        isFrontCompactionConfirmationOpen={pane.isFrontCompactionConfirmationOpen}
                        frontCompactionTargetMessageId={
                          frontCompactionTarget?.viewKey === paneViewKey
                            ? frontCompactionTarget.message.id
                            : undefined
                        }
                        onSelectFrontCompaction={
                          pane.isFrontCompactionSelectionMode
                            ? openFrontCompactionConfirmation
                            : undefined
                        }
                        onLoadFull={loadFullMessage}
                        onLoadOlderTurns={() => loadOlderMessages(pane.session, paneViewKey)}
                        onFocus={() => focusViewPane(paneViewKey)}
                        onClose={panes.keys.length > 1 ? () => closeViewPane(paneViewKey) : undefined}
                      />
                    );
                    // A pane that is not on screen (a phone) keeps its place in
                    // the arrangement without taking room from the visible one.
                    if (!shownPaneKeys.includes(paneViewKey) || shownPaneKeys.length === 1) {
                      return <Fragment key={paneViewKey}>{paneNode}</Fragment>;
                    }
                    return (
                      <PaneColumn
                        key={paneViewKey}
                        storageId={`${appConfig.conversation_pane_resize_storage_prefix}-${index}`}
                        defaultSizePx={appConfig.conversation_pane_default_size}
                        minSizePx={appConfig.conversation_pane_min_size}
                        maxSizePx={appConfig.conversation_pane_max_size}
                        foldWidthPx={appConfig.conversation_pane_fold_width}
                        widthPx={paneWidths[index]}
                        onSizeChange={widthPx => reportPaneOwnWidth(index, widthPx)}
                        onRegisterResize={resize => registerPaneResize(index, resize)}
                        onResizeStart={() => startPaneDrag(index)}
                        onResizeDelta={delta => dragPaneBoundary(index, delta)}
                        onResizeEnd={endPaneDrag}
                        isFlexible={index === panes.keys.length - 1}
                        handleLabel={`Resize pane ${index + 1}`}
                        folded={
                          <FoldedPane
                            number={index + 1}
                            label={pane.label}
                            isRunning={pane.isRunning}
                          />
                        }
                      >
                        {paneNode}
                      </PaneColumn>
                    );
                  })}
                </HStack>
                {shownPaneKeys.length === 1 && !isMobile && (
                  <ResizeHandle
                    direction="horizontal"
                    hasDivider={false}
                    xstyle={styles.composerResizeHandle}
                    // Same reason as the left border: the zone sits outside the
                    // lane, so the transcript's scrollbar stays clickable.
                    pillPlacement="end"
                    label="Resize conversation width from right border"
                    resizable={conversation.props}
                  />
                )}
              </HStack>
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
              closeLeftPanel();
            }}
            onClose={closeLeftPanel}
          />
        )}
        {leftPanel === 'settings' && (
          <RunSettingsPanel
            region={leftPanelRegion}
            onClose={closeLeftPanel}
            sessionTitle={activeRowSession.title}
            sessionTurns={activeRowSession.turns}
          />
        )}
        {leftPanel === 'notifications' && (
          <NotificationPanel
            notifications={notifications}
            region={leftPanelRegion}
            onClose={closeLeftPanel}
            onClear={() => setNotifications([])}
            onSelectSession={sessionId => {
              selectSession(sessionId);
              closeLeftPanel();
            }}
          />
        )}
        {leftPanel === 'checkpoints' && (
          <CheckpointPanel
            sessionId={activeSessionId}
            profile={activeProfile}
            isRunning={isRunning}
            region={leftPanelRegion}
            restorePollMs={appConfig.checkpoint_restore_poll_ms}
            pathsPreviewLimit={appConfig.checkpoint_paths_preview_limit}
            pathsMaxHeightPx={appConfig.checkpoint_paths_max_height_px}
            onClose={closeLeftPanel}
          />
        )}
        {leftPanel === 'panes' && (
          <PanesPanel
            rows={panes.keys.map((paneViewKey, index) => {
              const pane = paneView(paneViewKey);
              return {
                viewKey: paneViewKey,
                number: index + 1,
                title: paneTitleText({
                  sessionTitle: pane.session?.title ?? pane.label,
                  profile: pane.profile,
                  provider: pane.provider,
                  model: pane.model,
                  effort: pane.effortLabel,
                }),
                isRunning: pane.isRunning,
                isFocused: paneViewKey === activeViewKey,
              };
            })}
            limit={paneLimit}
            region={leftPanelRegion}
            onFocusPane={viewKey => {
              focusViewPane(viewKey);
              revealPaneView(viewKey);
              closeLeftPanel();
            }}
            onClose={closeLeftPanel}
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
