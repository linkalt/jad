import {Fragment, Suspense, lazy, memo, useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState} from 'react';
import type {ChangeEvent, CSSProperties, SVGProps} from 'react';
import * as stylex from '@stylexjs/stylex';
import {
  AppShell,
  Avatar,
  Badge,
  Button,
  Card,
  CodeBlock,
  ChatLayout,
  ChatMessage,
  ChatMessageBubble,
  ChatMessageList,
  ChatMessageMetadata,
  ChatSystemMessage,
  ChatToolCalls,
  Heading,
  HStack,
  Icon,
  IconButton,
  Layout,
  LayoutContent,
  SideNav,
  SideNavItem,
  SideNavSection,
  StatusDot,
  Text,
  VStack,
  VisuallyHidden,
} from '@astryxdesign/core';
import type {
  ChatToolCallItem,
  StatusDotVariant,
} from '@astryxdesign/core';
import {ChatComposerDrawer} from '@astryxdesign/core/Chat';
import {useToast} from '@astryxdesign/core/Toast';
import {
  hasUnreadActivityError,
  isRunOutcomeUnknown,
  markActivityNotificationsRead,
  prependActivityNotification,
  runOutcomeUnknownNotification,
  unreadActivityNotificationCount,
  type ActivityNotification,
  type NewActivityNotification,
} from './activity-notifications';
import {hookReportNotification, type HookReport} from './hook-report-view';
import {
  answerSessionRunInteraction,
  attachSessionRun,
  compactSession,
  writeSessionStateContext,
  extendSessionRunInteraction,
  frontCompactSessionFromMessage,
  resetSession,
  fetchSession,
  fetchSessionHistory,
  fetchSessionMessage,
  fetchSessions,
  updateSessionProfileModelDefault,
  stopSessionRun,
  streamSessionRun,
  type SessionPage,
  type SessionRunFailure,
  type SessionSnapshot,
  type RootLibraryAsset,
  type WebUiInteraction,
  type WebUiMessage,
  type WebUiMessageStep,
  type WebUiPart,
  type WebUiHostedToolPart,
} from './api';
import appConfig from './config.json';
import {readAttachmentBlocks} from './attachment-reading';
import {buildRunInput, userMessageImages} from './image-attachments';
import {SentFiles} from './components/FileAttachments';
import {SentImages} from './components/ImageAttachments';
import type {AttachedFile, AttachedImage} from './image-attachments';
import {createCoalescedRefresh} from './coalesced-refresh';
import {deepEqual} from './deepEqual';
import {selectedModelForSession} from './model-selection';
import {selectionAfterPanelToggle, type RightPanelId} from './right-panel';
import {createRowHeightStore, type RowHeightStore} from './row-heights';
import {
  captureScrollPosition,
  findScrollParent,
  holdAnchorPosition,
  revealScrollOptions,
  type ScrollPositionSnapshot,
} from './components/scroll-position';
import {suppressAttachedRunTranscriptTail} from './active-run-transcript';
import {stopRunAfterConfirmation} from './stop-run';
import {
  isSessionRunning,
  sessionUiStateForRun,
  snapshotEndsLocalRun,
  withSnapshotRun,
  type SessionUiState,
} from './session-run-status';
import {shouldSkipTranscriptLoad} from './transcript-load-policy';
import {
  EMPTY_HISTORY_BLOCKS,
  forgetHistoryBlock,
  forgetHistoryBlocks,
  historyBlockFor,
  historyEtagFromHeader,
  historyMessagesFor,
  putHistoryBlock,
  type HistoryBlock,
  type HistoryBlocks,
} from './history-blocks';
import {
  hiddenWebUiStepIds,
  isWebUiPartHidden,
  visibleWebUiItems,
  webUiStepAtPartIndex,
} from './web-ui-steps';
import {applyPart, applyPartDelta} from './messagePatch';
import {ComposerPane} from './components/ComposerPane';
import {StateContextDrawer} from './components/StateContextDrawer';
import {ComposerErrorBoundary} from './components/ComposerErrorBoundary';
import {
  COMPOSER_DESYNC_EVENT,
  describeDesync,
  installDesyncWatch,
  shouldInstallDesyncWatch,
  type ComposerDesync,
} from './desync-watch';
import {ApprovalDrawer} from './components/ApprovalDrawer';
import {LazyLoadBoundary} from './components/LazyLoadBoundary';
import {DraftRequestDrawer} from './components/DraftRequestDrawer';
import {interactionFailureTitle} from './approval-answer';
import type {DraftDecision} from './draft-answer';
import type {ComposerInsertActions} from './components/ComposerPane';
import {ModelPicker} from './components/ModelPicker';
import {CitationList} from './components/CitationList';
import {InlineImages, StoredImages, StoredImageScopeProvider} from './components/StoredImages';
import {hostedToolStatus} from './web-search-view';
import {FloatingPanel} from './components/FloatingPanel';
import {NotificationPanel} from './components/NotificationPanel';
import {CheckpointPanel} from './components/CheckpointPanel';
import {JadReadme} from './components/JadReadme';
import {MarkdownText} from './components/MarkdownText';
import {SelectionMenu} from './components/SelectionMenu';
import type {QuoteAnchor} from './components/SelectionMenu';
import {quoteDraftForSelection} from './selection-quote';
import {PromptLibraryPanel} from './components/PromptLibraryPanel';
import {RunSettingsPanel} from './components/RunSettingsPanel';
import {SessionSearchPanel} from './components/SessionSearchPanel';
import {EarlierMessagesControl, useTranscriptWindow} from './components/TranscriptWindow';
import {UsageDetailsDrawer} from './components/UsageDetailsDrawer';
import {
  ResizeHandle,
  useResizable,
  type ResizableRegion,
} from '@astryxdesign/core/Resizable';
import {pixel} from '@astryxdesign/core/Resizable/utils';
import {
  borderVars,
  colorVars,
  fontWeightVars,
  radiusVars,
  sizeVars,
  spacingVars,
  typeScaleVars,
  typographyVars,
} from '@astryxdesign/core/theme/tokens.stylex';
import {
  addPinnedView,
  isPinnedViewKey,
  profileOfViewKey,
  activeKeyAfterPrune,
  nextRecentViewKeys,
  retainedPinnedViewKeys,
  prunePinnedViews,
  restoreViews,
  rowRunStatus,
  parseCollapsedGroups,
  groupByCategory,
  sessionListGroups,
  sessionListRows,
  setGroupCollapsed,
  sessionRefOfViewKey,
  viewKey,
  profileLabel,
  viewLabel,
  type PinnedView,
  type SessionListGroup,
} from './session-views';
import {placeRunFailures, runFailureText} from './run-failures';
import {ProviderRetryNotice} from './components/ProviderRetryNotice';
import {SessionGroupList} from './components/SessionGroupList';
import {interactionEndNotice, type InteractionEnd} from './interaction-end';
import type {ProviderRetryWait} from './provider-retry-view';
import {removeViewKeys, removedViewKeys} from './view-cache';
import {canAttachRunToView, recordOwnedRunTurn, updateViewState} from './view-state';

function ResetChatIcon(props: SVGProps<SVGSVGElement>) {
  return (
    <svg
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth={1.5}
      strokeLinecap="round"
      strokeLinejoin="round"
      {...props}
    >
      <path d="M4 7h16" />
      <path d="M10 11v6M14 11v6" />
      <path d="M5.5 7l1 13h11l1-13M9 7V4h6v3" />
    </svg>
  );
}

function StateContextIcon(props: SVGProps<SVGSVGElement>) {
  return (
    <svg
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth={1.8}
      strokeLinecap="round"
      strokeLinejoin="round"
      {...props}
    >
      <path d="M5 6h14M5 12h14M5 18h8" />
    </svg>
  );
}

function FrontCompactionIcon(props: SVGProps<SVGSVGElement>) {
  return (
    <svg
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth={1.8}
      strokeLinecap="round"
      strokeLinejoin="round"
      {...props}
    >
      <circle cx="5" cy="12" r="1" />
      <circle cx="12" cy="12" r="1" />
      <circle cx="19" cy="12" r="1" />
    </svg>
  );
}

/** A failed run, drawn where the user message it belongs to ends. */
function RunFailureRow({failure}: {failure: SessionRunFailure}) {
  return (
    <ChatSystemMessage
      variant="default"
      icon={<Icon icon="close" color="error" size="sm" />}
    >
      {runFailureText(failure)}
    </ChatSystemMessage>
  );
}

function isActiveTranscriptUserMessage(message: MessageRecord): boolean {
  return message.sender === 'user' && /^session-user-o(0|[1-9][0-9]*)$/.test(message.id);
}

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
  sectionHeading: {
    minWidth: 0,
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
  // The assistant name row carries a negative block margin of its own, so the
  // bubble contributes the rest of the gap between the identity row and the
  // message body.
  assistantBubble: {
    marginBlockStart: spacingVars['--spacing-4'],
  },
  // Reasoning rows and tool rows share one tight stack, so the group itself
  // carries no block margin; the stack gap supplies the row rhythm.
  activityGroup: {
    width: '100%',
    marginBlockStart: spacingVars['--spacing-0'],
  },
  toolCallsFlush: {
    marginBlockStart: spacingVars['--spacing-0'],
  },
  // Hidden probes used only to resolve the fonts a tool row renders with.
  textProbe: {
    position: 'absolute',
    insetBlockStart: 0,
    visibility: 'hidden',
    pointerEvents: 'none',
    whiteSpace: 'nowrap',
  },
  textProbeBody: {
    fontFamily: typographyVars['--font-family-body'],
    fontSize: typeScaleVars['--text-supporting-size'],
    fontWeight: fontWeightVars['--font-weight-normal'],
  },
  textProbeCode: {
    fontFamily: typographyVars['--font-family-code'],
    fontSize: typeScaleVars['--text-supporting-size'],
    fontWeight: fontWeightVars['--font-weight-medium'],
  },
  reasoningContent: {
    color: colorVars['--color-text-secondary'],
    fontSize: typeScaleVars['--text-supporting-size'],
    lineHeight: typeScaleVars['--text-supporting-leading'],
  },
  traceList: {
    listStyleType: 'none',
    margin: spacingVars['--spacing-0'],
    padding: spacingVars['--spacing-0'],
  },
  traceRow: {
    minWidth: 0,
    paddingBlock: spacingVars['--spacing-1'],
    borderBlockEnd: `${borderVars['--border-width']} solid ${colorVars['--color-border']}`,
  },
  traceMarker: {
    display: 'grid',
    flexShrink: 0,
    placeItems: 'center',
    width: sizeVars['--size-element-sm'],
    height: sizeVars['--size-element-sm'],
    borderRadius: radiusVars['--radius-full'],
    backgroundColor: colorVars['--color-background-muted'],
  },
  traceCopy: {
    minWidth: 0,
    flex: 1,
  },
  inspectorNote: {
    borderBlockStart: `${borderVars['--border-width']} solid ${colorVars['--color-border']}`,
    paddingBlockStart: spacingVars['--spacing-3'],
  },
});

type SessionState = SessionUiState;
interface RunInteractionState {
  viewKey: string;
  turnId: string;
  pending: WebUiInteraction | undefined;
  received: boolean;
}
interface RunRetryState {
  turnId: string;
  wait: ProviderRetryWait;
}
interface InteractionUiState {
  answeringInteractionId?: string;
  extendingInteractionId?: string;
  error?: string;
}
type TraceState = 'complete' | 'running' | 'pending';
interface Session {
  id: string;
  title: string;
  time: string;
  turns: number;
  profileTurns?: Record<string, number>;
  state: SessionState;
  source: 'live' | 'sample';
  sessionRef?: string;
  rootLabel?: string;
  /** Groups sessions in the list; absent when the session has none. */
  category?: string;
  activeProfile?: string;
  profiles?: string[];
  profileNames?: Record<string, string>;
  modelDefault?: string;
  models?: string[];
  frontCompaction?: SessionSnapshot['front_compaction'];
  model?: {
    provider?: string;
    model?: string;
  };
  usage?: SessionSnapshot['usage'];
  /** The run the server is executing for this session, when there is one. */
  run?: SessionSnapshot['run'];
  /** The run of each path profile that has one, keyed by profile name. */
  runsByProfile?: SessionSnapshot['runs_by_profile'];
  /**
   * The profile this row is pinned to. A row without one is the session's own
   * row and follows the stored default; a pinned row is a session of its own
   * that shares the session's `setting.json`.
   */
  profile?: string;
}

interface ReasoningStep {
  label: string;
  detail: string;
  status: TraceState;
}

type MessageRecord = Omit<WebUiMessage, 'parts'> & {
  /** Data URLs of the images this tab just sent; the server's copy replaces them on the next load. */
  sentImages?: string[];
  /** Names of the files this tab just sent; the server's list replaces them after a reload. */
  sentFiles?: string[];
  parts?: WebUiPart[];
  reasoning?: ReasoningStep[];
  tools?: ChatToolCallItem[];
};

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

/** Storage is best effort: a blocked or full store must not break the list. */
function readStoredViews(key: string): unknown {
  try {
    const raw = window.localStorage.getItem(key);
    return raw === null ? undefined : JSON.parse(raw);
  } catch {
    return undefined;
  }
}

function writeStoredViews(key: string, value: {views: PinnedView[]; activeViewKey: string}): void {
  try {
    window.localStorage.setItem(key, JSON.stringify(value));
  } catch {
    // Ignored: the rows live for this page even when they cannot be persisted.
  }
}

/** The groups the user collapsed; best effort, like the pinned rows. */
function readCollapsedGroups(key: string): string[] {
  return parseCollapsedGroups(readStoredViews(key));
}

function writeCollapsedGroups(key: string, value: readonly string[]): void {
  try {
    window.localStorage.setItem(key, JSON.stringify(value));
  } catch {
    // Ignored: the groups keep their state for this page.
  }
}

// Off-screen transcript rows are skipped by the renderer. Without this, every
// keystroke in the composer costs a full layout of the scroll container's
// contents (the composer dock is sticky inside the same scroller), which made
// typing slow in long sessions — measured ~42ms of synchronous work per
// keystroke against a 36k-pixel scroll height, versus ~10ms with this enabled.
//
// The intrinsic size is the part that has to be right. A deferred row renders
// at that size until the engine lays it out, and the scroll *range* changes by
// the difference when it does — measured on a long session, 695–927px per row,
// one row every 200–1500ms of scrolling, in both directions as rows are
// re-created by a re-render. Firefox does not compensate that change (the
// content travels with the range), so it reads as the transcript jumping while
// you scroll. The engine's own `auto` keyword only remembers a size while the
// DOM element lives, which a re-render ends, so the height every row was last
// measured at is kept in `row-heights.ts` and fed back here.
let rowHeightStore: RowHeightStore | null = null;
let rowHeightObserver: ResizeObserver | null = null;
/** The width rows are rendering at, learned from the rows themselves. */
let latestRowWidthPx = 0;
const rowHeightIds = new WeakMap<Element, string>();

function rowHeights(): RowHeightStore {
  rowHeightStore ??= createRowHeightStore({
    storage: window.localStorage,
    storageKey: appConfig.row_height_storage_key,
    maxEntries: appConfig.row_height_max_entries,
    saveDelayMs: appConfig.row_height_save_delay_ms,
    widthBucketPx: appConfig.row_height_width_bucket_px,
  });
  return rowHeightStore;
}

/** Skip-this-row styling, at the height this row had the last time we saw it. */
function deferredRowStyle(messageId: string | null): CSSProperties | undefined {
  if (!appConfig.defer_offscreen_transcript_rows) {
    return undefined;
  }
  const remembered =
    messageId === null ? null : rowHeights().heightFor(messageId, latestRowWidthPx);
  return {
    contentVisibility: 'auto',
    containIntrinsicSize: `auto ${remembered ?? appConfig.transcript_row_estimate_px}px`,
  };
}

/**
 * Measures a row as the engine lays it out, so the next render can skip it at
 * that height. One observer serves every row.
 */
function useRowHeightRef(messageId: string | null): (node: HTMLElement | null) => void {
  const observedRef = useRef<Element | null>(null);
  return useCallback(
    (node: HTMLElement | null) => {
      if (observedRef.current !== null) {
        rowHeightObserver?.unobserve(observedRef.current);
        observedRef.current = null;
      }
      if (node === null || messageId === null || !appConfig.defer_offscreen_transcript_rows) {
        return;
      }
      rowHeightObserver ??= new ResizeObserver(entries => {
        for (const entry of entries) {
          const id = rowHeightIds.get(entry.target);
          if (id === undefined) {
            continue;
          }
          const width = entry.contentRect.width;
          if (width > 0) {
            latestRowWidthPx = width;
          }
          const height = entry.borderBoxSize?.[0]?.blockSize ?? entry.contentRect.height;
          // A row the engine has not laid out yet reports its own intrinsic
          // size, which is what we already assume for it; storing that would
          // turn the estimate into a "measurement".
          const assumed = rowHeights().heightFor(id, width) ?? appConfig.transcript_row_estimate_px;
          if (Math.abs(height - assumed) < 1) {
            continue;
          }
          rowHeights().remember(id, height, width);
        }
      });
      rowHeightIds.set(node, messageId);
      rowHeightObserver.observe(node);
      observedRef.current = node;
    },
    [messageId],
  );
}

// Revealing steps inserts content next to the control that was pressed, so the
// reader is held on that control until the rows around it stop changing size.
const REVEAL_STEPS_SCROLL_OPTIONS = revealScrollOptions('steps', appConfig);

function messageTime(): string {
  return new Date().toLocaleTimeString([], {
    hour: 'numeric',
    minute: '2-digit',
  });
}

function oneLine(value: string): string {
  return value.replace(/\s+/g, ' ').trim();
}

const REASONING_ROW_NAME = 'Think';

function reasoningTitleText(value: string, maxPx: number): string {
  const normalized = oneLine(value);
  const maxLength = appConfig.reasoning_title_max_length;
  const limited =
    normalized.length > maxLength
      ? `${normalized.slice(0, maxLength - 1)}…`
      : normalized;
  return cutRowText(limited, maxPx);
}

const baseReasoning: Omit<ReasoningStep, 'status'>[] = [
  {
    label: 'Frame the request',
    detail: 'Separate the visual goal from implementation constraints.',
  },
  {
    label: 'Inspect the harness surface',
    detail: 'Locate the session, transcript, composer, and run inspector regions.',
  },
  {
    label: 'Draft a component plan',
    detail: 'Prefer Astryx layout, chat, status, and navigation primitives.',
  },
  {
    label: 'Check interaction paths',
    detail: 'Keep sending, stopping, switching sessions, and inspecting runs obvious.',
  },
  {
    label: 'Validate the mock output',
    detail: 'Confirm the response reads as a plausible agent run before shipping.',
  },
];

const defaultTools: ChatToolCallItem[] = [
  {
    key: 'read-components',
    name: 'read_components',
    status: 'complete',
    target: 'Astryx component map',
    duration: '420ms',
    node: 'workspace',
  },
  {
    key: 'draft-layout',
    name: 'draft_layout',
    status: 'complete',
    target: 'Jad regions',
    duration: '680ms',
    node: 'planner',
    additions: 3,
  },
];

/**
 * Demo sessions. Live sessions are deliberately absent: they are owned by the
 * backend and arrive on every poll under their own names, so there is no
 * placeholder row standing in for a session that may not exist.
 */
const initialSessions: Session[] = [
  {
    id: 'atlas',
    title: 'Atlas onboarding flow',
    time: '2 min ago',
    turns: 8,
    state: 'success',
    source: 'sample',
  },
  {
    id: 'settings',
    title: 'Settings surface audit',
    time: '18 min ago',
    turns: 5,
    state: 'warning',
    source: 'sample',
  },
  {
    id: 'empty',
    title: 'Empty state exploration',
    time: 'Yesterday',
    turns: 3,
    state: 'success',
    source: 'sample',
  },
];

const initialThreads: Record<string, MessageRecord[]> = {
  atlas: [
    {
      id: 'atlas-system',
      sender: 'system',
      text: 'Run started · 12:41 PM',
      timestamp: '12:41 PM',
    },
    {
      id: 'atlas-user',
      sender: 'user',
      text: 'Build a settings screen for the new agent workspace. Keep the navigation compact and make tool activity easy to scan.',
      timestamp: '12:42 PM',
    },
    {
      id: 'atlas-assistant',
      sender: 'assistant',
      text: 'I mapped the request into a compact app shell with a **conversation-first** center column and an inspector that keeps the run trace close without competing with the response.\n\n- **Center:** conversation and composer\n- **Inspector:** reasoning and tool activity\n- **Navigation:** compact session history',
      timestamp: '12:43 PM',
      reasoning: baseReasoning.map(step => ({...step, status: 'complete'})),
      tools: defaultTools,
    },
  ],
  settings: [
    {
      id: 'settings-system',
      sender: 'system',
      text: 'Run started · 12:25 PM',
      timestamp: '12:25 PM',
    },
    {
      id: 'settings-user',
      sender: 'user',
      text: 'Audit the current settings surface and call out the highest-risk interaction gaps.',
      timestamp: '12:26 PM',
    },
    {
      id: 'settings-assistant',
      sender: 'assistant',
      text: 'The primary gap is **discoverability**: the settings groups are present, but the current hierarchy makes the next action hard to predict.\n\n> Recommendation: expose the next action before secondary configuration.',
      timestamp: '12:27 PM',
      reasoning: baseReasoning.map(step => ({...step, status: 'complete'})),
      tools: defaultTools,
    },
  ],
  empty: [
    {
      id: 'empty-system',
      sender: 'system',
      text: 'Run started · Yesterday',
      timestamp: 'Yesterday',
    },
    {
      id: 'empty-user',
      sender: 'user',
      text: 'Explore a calm first-run state for a workspace with no agent history.',
      timestamp: 'Yesterday',
    },
    {
      id: 'empty-assistant',
      sender: 'assistant',
      text: 'A useful empty state should explain what the agent can do, show one concrete starting prompt, and leave room for the user to bring their own task.\n\n`Start with a task` is a better first action than a blank transcript.',
      timestamp: 'Yesterday',
      reasoning: baseReasoning.map(step => ({...step, status: 'complete'})),
      tools: defaultTools,
    },
  ],
};

function sessionUiId(snapshot: SessionSnapshot, current: readonly Session[]): string {
  const sampleCollision = current.some(
    session => session.source === 'sample' && session.id === snapshot.id,
  );
  return sampleCollision ? `live:${snapshot.id}` : snapshot.id;
}

function sessionFromSnapshot(snapshot: SessionSnapshot, id: string): Session {
  return {
    id,
    title: oneLine(snapshot.session_name) || snapshot.session_id || snapshot.id,
    time: snapshot.turns > 0 ? 'Local transcript' : 'Ready',
    turns: snapshot.turns,
    ...(snapshot.profile_turns === undefined ? {} : {profileTurns: snapshot.profile_turns}),
    state: sessionUiStateForRun(snapshot.run?.status),
    source: 'live',
    sessionRef: snapshot.id,
    ...(snapshot.runs_by_profile === undefined
      ? {}
      : {runsByProfile: snapshot.runs_by_profile}),
    ...(snapshot.root_label ? {rootLabel: snapshot.root_label} : {}),
    ...(snapshot.category ? {category: snapshot.category} : {}),
    ...(snapshot.active_profile === undefined ? {} : {activeProfile: snapshot.active_profile}),
    ...(snapshot.profiles === undefined ? {} : {profiles: snapshot.profiles}),
    ...(snapshot.profile_names === undefined ? {} : {profileNames: snapshot.profile_names}),
    ...(snapshot.model_default === undefined ? {} : {modelDefault: snapshot.model_default}),
    ...(snapshot.models === undefined ? {} : {models: snapshot.models}),
    ...(snapshot.front_compaction === undefined
      ? {}
      : {frontCompaction: snapshot.front_compaction}),
    model: snapshot.model,
    ...(snapshot.usage ? {usage: snapshot.usage} : {}),
    ...(snapshot.run ? {run: snapshot.run} : {}),
  };
}

function getTraceForSteps(
  steps: Omit<ReasoningStep, 'status'>[],
  progress: number,
): ReasoningStep[] {
  return steps.map((step, index) => ({
    ...step,
    status:
      index < progress
        ? 'complete'
        : index === progress
          ? 'running'
          : 'pending',
  }));
}

function getStatusDotVariant(status: TraceState): StatusDotVariant {
  if (status === 'complete') {
    return 'success';
  }
  if (status === 'running') {
    return 'accent';
  }
  return 'neutral';
}

function getStatusLabel(status: TraceState): string {
  if (status === 'complete') {
    return 'Complete';
  }
  if (status === 'running') {
    return 'In progress';
  }
  return 'Queued';
}

function ReasoningBlock({
  steps,
  content = '',
  rowWidthPx,
  isFlush = false,
  isStreaming = false,
}: {
  steps: ReasoningStep[];
  content?: string;
  /** Width available to the row; the title is cut to it so the name stays whole. */
  rowWidthPx: number;
  /** Drop the row's own block margin; the parent supplies the spacing. */
  isFlush?: boolean;
  isStreaming?: boolean;
}) {
  const hasContent = content.trim().length > 0;
  const hasSteps = steps.length > 0;
  const title = reasoningTitleText(
    content ||
      steps.find(step => step.status === 'running')?.detail ||
      steps[0]?.detail ||
      '',
    toolTitleBudgetPx(rowWidthPx, REASONING_ROW_NAME),
  );
  const completedSteps = steps.filter(step => step.status === 'complete').length;

  if (!hasContent && !hasSteps) {
    return null;
  }

  const item: ChatToolCallItem = {
    key: `reasoning-${title}-${steps.length}`,
    name: REASONING_ROW_NAME,
    status: isStreaming ? 'running' : 'complete',
    target: title || undefined,
    resultDetail: (
      <VStack gap={2} width="100%">
        {hasContent && (
          <MarkdownText
            density="compact"
            contentWidth="100%"
            xstyle={styles.reasoningContent}
          >
            {content}
          </MarkdownText>
        )}
        {hasSteps && (
          <>
            <Text type="supporting" color="secondary">
              {completedSteps}/{steps.length} steps
            </Text>
            <VStack as="ol" gap={1} xstyle={styles.traceList}>
              {steps.map(step => (
                <HStack as="li" key={step.label} align="start" gap={2} xstyle={styles.traceRow}>
                  <HStack align="center" justify="center" xstyle={styles.traceMarker}>
                    <StatusDot
                      variant={getStatusDotVariant(step.status)}
                      label={getStatusLabel(step.status)}
                      isPulsing={step.status === 'running'}
                      icon={
                        step.status === 'complete' ? (
                          <Icon icon="check" size="xsm" />
                        ) : undefined
                      }
                    />
                  </HStack>
                  <VStack gap={0.5} xstyle={styles.traceCopy}>
                    <Text type="label">{step.label}</Text>
                    <Text type="supporting" color="secondary">
                      {step.detail}
                    </Text>
                  </VStack>
                </HStack>
              ))}
            </VStack>
          </>
        )}
      </VStack>
    ),
  };

  return (
    <ChatToolCalls
      calls={[item]}
      xstyle={isFlush ? styles.toolCallsFlush : undefined}
    />
  );
}

type WebUiToolCall = Extract<WebUiPart, {type: 'tool_call'}>;
type WebUiToolResult = Extract<WebUiPart, {type: 'tool_result'}>;

function prettyToolArguments(argumentsText: string): string {
  try {
    const parsed = JSON.parse(argumentsText) as unknown;
    return JSON.stringify(parsed, null, 2) ?? argumentsText;
  } catch {
    return argumentsText;
  }
}

// A tool or reasoning row shares its width between the tool name and the title,
// and the row layout gives the title `flex-shrink: 10` while the tool name only
// keeps a `4ch` minimum - so any title that does not fit clips the name to
// `writ…`. Titles are therefore cut to the measured row width, which lets the
// title carry the truncation instead of the name.
const TOOL_ROW_CHROME_PX = 96;
const TOOL_TITLE_SAFETY_PX = 8;
const TOOL_TITLE_FALLBACK_PX_PER_CHAR = 7;

type RowTextFont = 'body' | 'code';

const resolvedRowFonts: Partial<Record<RowTextFont, string>> = {};
let rowTextCanvas: HTMLCanvasElement | null = null;

function resolveRowFont(font: RowTextFont): string {
  const cached = resolvedRowFonts[font];
  if (cached || typeof document === 'undefined') {
    return cached ?? `${TOOL_TITLE_FALLBACK_PX_PER_CHAR}px`;
  }
  const probe = document.createElement('span');
  probe.setAttribute('aria-hidden', 'true');
  probe.className =
    stylex.props(
      styles.textProbe,
      font === 'code' ? styles.textProbeCode : styles.textProbeBody,
    ).className ?? '';
  // Measure inside the theme, where the rows render: on `document.body` the
  // `--font-family-*` tokens only see the :root defaults, so the budget would
  // be computed from a different face than the row is drawn with.
  const probeHost =
    document.getElementById('root')?.firstElementChild ?? document.body;
  probeHost.appendChild(probe);
  const probeStyles = getComputedStyle(probe);
  const resolved = `${probeStyles.fontWeight} ${probeStyles.fontSize} ${probeStyles.fontFamily}`;
  probe.remove();
  resolvedRowFonts[font] = resolved;
  return resolved;
}

/** Width of row text in px, measured with the font the row renders it in. */
function measureRowText(text: string, font: RowTextFont): number {
  if (typeof document === 'undefined') {
    return text.length * TOOL_TITLE_FALLBACK_PX_PER_CHAR;
  }
  rowTextCanvas ??= document.createElement('canvas');
  const context = rowTextCanvas.getContext('2d');
  if (!context) {
    return text.length * TOOL_TITLE_FALLBACK_PX_PER_CHAR;
  }
  context.font = resolveRowFont(font);
  return context.measureText(text).width;
}

/** Cut text to a pixel width, ending with an ellipsis when it does not fit. */
function cutRowText(text: string, maxPx: number): string {
  if (maxPx <= 0) {
    return '';
  }
  if (measureRowText(text, 'body') <= maxPx) {
    return text;
  }
  let fits = 0;
  let tooLong = text.length;
  while (fits < tooLong) {
    const middle = Math.ceil((fits + tooLong) / 2);
    if (measureRowText(`${text.slice(0, middle)}…`, 'body') <= maxPx) {
      fits = middle;
    } else {
      tooLong = middle - 1;
    }
  }
  return fits > 0 ? `${text.slice(0, fits)}…` : '…';
}

/**
 * Width a row title may use so its tool name keeps full width: the measured row
 * width minus the fixed row furniture (status icon, gaps, count, chevron,
 * padding), the name itself and a safety margin.
 */
function toolTitleBudgetPx(rowWidthPx: number, toolName: string): number {
  if (rowWidthPx <= 0) {
    return 0;
  }
  return Math.max(
    0,
    rowWidthPx -
      TOOL_ROW_CHROME_PX -
      measureRowText(toolName, 'code') -
      TOOL_TITLE_SAFETY_PX,
  );
}

function compactToolValue(value: unknown, maxChars: number): string {
  if (typeof value === 'string') {
    if (value.length === 0) {
      return '""';
    }
    return value.length > maxChars
      ? `"${value.slice(0, maxChars)}…" (${value.length} chars)`
      : value;
  }
  if (value === null) {
    return 'null';
  }
  if (typeof value === 'number' || typeof value === 'boolean') {
    return String(value);
  }
  if (Array.isArray(value)) {
    const json = JSON.stringify(value);
    return json && json.length <= maxChars ? json : `[${value.length} items]`;
  }
  if (typeof value === 'object') {
    const json = JSON.stringify(value);
    return json && json.length <= maxChars
      ? json
      : `{${Object.keys(value).length} keys}`;
  }
  return String(value);
}

function toolTarget(argumentsText: string, budgetPx: number): string {
  if (budgetPx <= 0) {
    return '';
  }
  try {
    const parsed = JSON.parse(argumentsText) as unknown;
    if (
      typeof parsed === 'object' &&
      parsed !== null &&
      !Array.isArray(parsed)
    ) {
      const entries = Object.entries(parsed as Record<string, unknown>);
      if (entries.length === 0) {
        return 'no arguments';
      }
      return cutRowText(
        entries
          .map(
            ([key, value]) =>
              `${key}: ${compactToolValue(value, appConfig.tool_title_value_max_chars)}`,
          )
          .join(' · '),
        budgetPx,
      );
    }
  } catch {
    // Keep the generic target when arguments are not JSON.
  }
  return cutRowText('arguments available', budgetPx);
}

/**
 * The title of a call whose arguments are still streaming: the tool name is
 * already known, and the first string field (usually `path`) plus the growing
 * size tell the user which long `write`/`edit` body is arriving.
 */
function partialToolTarget(argumentsText: string, budgetPx: number): string {
  if (budgetPx <= 0) {
    return '';
  }
  const progress = argumentsText.length < 1000
    ? `${argumentsText.length} chars received`
    : `${(argumentsText.length / 1000).toFixed(1)}k chars received`;
  const pathMatch = /"(?:path|file_path|file)"\s*:\s*"((?:[^"\\]|\\.)*)"/.exec(argumentsText);
  const path = pathMatch?.[1];
  if (path !== undefined && path.length > 0) {
    return cutRowText(`${path} · ${progress}`, budgetPx);
  }
  return cutRowText(progress, budgetPx);
}

function visibleToolOutput(output: string): string {
  const limit = appConfig.tool_output_max_chars;
  if (output.length <= limit) {
    return output;
  }
  return `${output.slice(0, limit)}\n\n[Output truncated in the UI]`;
}

/** The message a truncated part came from, with the action that loads it whole. */
interface TruncatedSource {
  message: MessageRecord;
  onLoadFull: (message: MessageRecord) => void | Promise<void>;
}

function loadFullAction(
  source: TruncatedSource | undefined,
): (() => void | Promise<void>) | undefined {
  // A message of a live run has no cursor yet, so the server cannot re-read it
  // whole; the button comes back once the stored message replaces it.
  if (source === undefined || source.message.cursor === undefined) {
    return undefined;
  }
  return () => source.onLoadFull(source.message);
}

function truncatedSize(bytes: number): string {
  if (bytes >= 1024 * 1024) {
    return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
  }
  if (bytes >= 1024) {
    return `${Math.round(bytes / 1024)} KB`;
  }
  return `${bytes} B`;
}

/**
 * A part the server cut for transport. The text continues on the server, and
 * the message carries the cursor that re-reads it whole.
 */
function TruncatedNotice({
  bytes,
  source,
}: {
  bytes: number;
  source: TruncatedSource | undefined;
}) {
  const [isLoading, setIsLoading] = useState(false);
  const onLoad = loadFullAction(source);
  return (
    <HStack align="center" gap={2}>
      <Text type="supporting" color="secondary">
        Truncated for transport · {truncatedSize(bytes)} total
      </Text>
      {onLoad !== undefined && (
        <Button
          label={isLoading ? 'Loading…' : 'Load full'}
          variant="ghost"
          size="sm"
          isDisabled={isLoading}
          onClick={() => {
            // The notice unmounts when the whole message replaces the capped
            // one; on failure it has to be usable again.
            setIsLoading(true);
            void Promise.resolve(onLoad()).finally(() => setIsLoading(false));
          }}
        />
      )}
    </HStack>
  );
}

function toolResultLabel(status: WebUiToolResult['status']): string {
  if (status === 'success') {
    return 'Success';
  }
  if (status === 'error') {
    return 'Error';
  }
  return 'Result';
}

function toolCallStatus(result?: WebUiToolResult): 'running' | 'complete' | 'error' {
  // A result part is only emitted once the call finished, so a missing result
  // is the only "still running" case. An unclassifiable status means the tool
  // returned output without a metadata line — finished, not running.
  if (!result) {
    return 'running';
  }
  return result.status === 'error' ? 'error' : 'complete';
}

function toolCallItem(
  call: WebUiToolCall,
  result: WebUiToolResult | undefined,
  rowWidthPx: number,
  source?: TruncatedSource,
): ChatToolCallItem {
  const resultStatus = result?.status;
  const titleBudgetPx = toolTitleBudgetPx(rowWidthPx, call.name);
  // A call is in progress while the arguments stream, and while the tool runs
  // after that.
  const isReceivingArguments = call.partial === true && result === undefined;
  return {
    key: `tool-${call.callId}`,
    name: call.name,
    status: toolCallStatus(result),
    target: isReceivingArguments
      ? partialToolTarget(call.arguments, titleBudgetPx)
      : toolTarget(call.arguments, titleBudgetPx),
    errorMessage: resultStatus === 'error' ? 'The tool returned an error.' : undefined,
    resultDetail: (
      <VStack gap={2} width="100%">
        {result && (
          <HStack align="center" gap={2}>
            <Badge
              label={toolResultLabel(result.status)}
              variant={result.status === 'error' ? 'error' : result.status === 'success' ? 'success' : 'neutral'}
            />
          </HStack>
        )}
        <CodeBlock
          code={prettyToolArguments(call.arguments)}
          language="json"
          size="sm"
          width="100%"
          container="card"
          maxHeight={appConfig.tool_arguments_max_height_px}
          hasCopyButton
        />
        {result && (
          <CodeBlock
            code={visibleToolOutput(result.output)}
            language="text"
            size="sm"
            width="100%"
            container="card"
            maxHeight={appConfig.tool_result_max_height_px}
            hasCopyButton
          />
        )}
        {result?.images !== undefined && <StoredImages names={result.images} />}
        {result?.inlineImages !== undefined && result.inlineImages > 0 && source !== undefined && (
          <InlineImages
            messageId={source.message.id}
            callId={result.callId}
            count={result.inlineImages}
            cursor={source.message.cursor}
          />
        )}
        {result?.truncated !== undefined && (
          <TruncatedNotice bytes={result.truncated.bytes} source={source} />
        )}
      </VStack>
    ),
  };
}

function OrphanToolResultBlock({
  result,
  source,
}: {
  result: WebUiToolResult;
  source?: TruncatedSource;
}) {
  return (
    <Card variant="muted" padding={2}>
      <VStack gap={1} width="100%">
        <HStack align="center" gap={2}>
          <Text type="label" weight="semibold">Tool result</Text>
          <Badge
            label={toolResultLabel(result.status)}
            variant={result.status === 'error' ? 'error' : result.status === 'success' ? 'success' : 'neutral'}
          />
        </HStack>
        <CodeBlock
          code={visibleToolOutput(result.output)}
          language="text"
          size="sm"
          width="100%"
          container="section"
          maxHeight={appConfig.tool_result_max_height_px}
          hasCopyButton
        />
        {result.images !== undefined && <StoredImages names={result.images} />}
        {result.inlineImages !== undefined && result.inlineImages > 0 && source !== undefined && (
          <InlineImages
            messageId={source.message.id}
            callId={result.callId}
            count={result.inlineImages}
            cursor={source.message.cursor}
          />
        )}
        {result.truncated !== undefined && (
          <TruncatedNotice bytes={result.truncated.bytes} source={source} />
        )}
      </VStack>
    </Card>
  );
}

function toolActivityForParts(
  parts: WebUiPart[],
  rowWidthPx: number,
): ChatToolCallItem[] {
  const calls = new Map<string, ChatToolCallItem>();
  for (const part of parts) {
    if (part.type === 'tool_call') {
      calls.set(part.callId, {
        key: `tool-${part.callId}`,
        name: part.name,
        status: 'running',
        target: toolTarget(part.arguments, toolTitleBudgetPx(rowWidthPx, part.name)),
      });
    }
  }
  for (const part of parts) {
    if (part.type !== 'tool_result') {
      continue;
    }
    const call = calls.get(part.callId);
    if (call) {
      calls.set(part.callId, {
        ...call,
        status: part.status === 'error' ? 'error' : part.status === 'success' ? 'complete' : 'running',
        errorMessage: part.status === 'error' ? 'The tool returned an error.' : undefined,
      });
    }
  }
  return [...calls.values()];
}

type WebUiReasoningPart = Extract<WebUiPart, {type: 'reasoning'}>;
type WebUiContentPart = Extract<WebUiPart, {type: 'text'}>;
type WebUiToolPart = WebUiToolCall | WebUiToolResult;
type IndexedWebUiPart<T extends WebUiPart> = {part: T; index: number};
type WebUiPartGroup =
  | {
      type: 'reasoning';
      item: IndexedWebUiPart<WebUiReasoningPart>;
    }
  | {
      type: 'tools';
      items: IndexedWebUiPart<WebUiToolPart>[];
    }
  | {
      type: 'hosted';
      item: IndexedWebUiPart<WebUiHostedToolPart>;
    }
  | {
      type: 'content';
      item: IndexedWebUiPart<WebUiContentPart>;
    };

function isWebUiToolPart(part: WebUiPart): part is WebUiToolPart {
  return part.type === 'tool_call' || part.type === 'tool_result';
}

function groupWebUiParts(
  parts: WebUiPart[],
  steps?: readonly WebUiMessageStep[],
): WebUiPartGroup[] {
  const groups: WebUiPartGroup[] = [];
  // Group only adjacent tool parts; reasoning and text always flush the tool run.
  let toolItems: IndexedWebUiPart<WebUiToolPart>[] = [];

  const flushTools = () => {
    if (toolItems.length > 0) {
      groups.push({type: 'tools', items: toolItems});
      toolItems = [];
    }
  };

  parts.forEach((part, index) => {
    if ((part.type === 'reasoning' || part.type === 'text') && part.text.trim().length === 0) {
      return;
    }
    if (isWebUiToolPart(part)) {
      const previousTool = toolItems[0];
      if (
        previousTool !== undefined &&
        webUiStepAtPartIndex(steps, previousTool.index)?.id !==
          webUiStepAtPartIndex(steps, index)?.id
      ) {
        flushTools();
      }
      toolItems.push({part, index});
      return;
    }

    flushTools();
    if (part.type === 'reasoning') {
      groups.push({type: 'reasoning', item: {part, index}});
      return;
    }
    if (part.type === 'hosted_tool') {
      groups.push({type: 'hosted', item: {part, index}});
      return;
    }
    groups.push({type: 'content', item: {part, index}});
  });
  flushTools();

  return groups;
}

type WebUiActivityGroupPart = Extract<
  WebUiPartGroup,
  {type: 'reasoning' | 'tools' | 'hosted'}
>;
type WebUiContentGroupPart = Extract<WebUiPartGroup, {type: 'content'}>;
type IndexedWebUiActivityGroup = {group: WebUiActivityGroupPart; index: number};
type WebUiSegment =
  | {type: 'activity'; items: IndexedWebUiActivityGroup[]}
  | {type: 'content'; item: {group: WebUiContentGroupPart; index: number}};

function webUiGroupKey(group: WebUiPartGroup, index: number): string {
  if (group.type === 'tools') {
    return `tools-${group.items[0]?.index ?? index}`;
  }
  return `${group.type}-${group.item.index}`;
}

function webUiGroupIndex(group: WebUiPartGroup): number {
  return group.type === 'tools' ? group.items[0]?.index ?? -1 : group.item.index;
}

/**
 * Reasoning rows and tool rows are the same Astryx call row, so consecutive
 * ones are laid out in a single tight stack: they keep the tool-to-tool row
 * rhythm instead of the wider block spacing used around Markdown text.
 */
function segmentWebUiGroups(groups: WebUiPartGroup[]): WebUiSegment[] {
  const segments: WebUiSegment[] = [];
  let activity: IndexedWebUiActivityGroup[] = [];

  const flushActivity = () => {
    if (activity.length > 0) {
      segments.push({type: 'activity', items: activity});
      activity = [];
    }
  };

  groups.forEach((group, index) => {
    if (group.type === 'content') {
      flushActivity();
      segments.push({type: 'content', item: {group, index}});
      return;
    }
    activity.push({group, index});
  });
  flushActivity();

  return segments;
}

function WebUiActivityGroup({
  group,
  rowWidthPx,
  isStreaming,
  source,
}: {
  group: WebUiActivityGroupPart;
  rowWidthPx: number;
  isStreaming: boolean;
  source?: TruncatedSource;
}) {
  if (group.type === 'reasoning') {
    const {part} = group.item;
    return (
      <VStack gap={1} width="100%">
        <ReasoningBlock
          steps={[]}
          content={part.text}
          rowWidthPx={rowWidthPx}
          isFlush
          isStreaming={isStreaming}
        />
        {part.truncated !== undefined && (
          <TruncatedNotice bytes={part.truncated.bytes} source={source} />
        )}
      </VStack>
    );
  }

  if (group.type === 'hosted') {
    const {part} = group.item;
    const status = hostedToolStatus(part.status);
    // The provider ran this tool itself, so there is no client result to wait
    // for. The row shows what the provider reported; expanding it shows the raw
    // item, like any other tool.
    return (
      <VStack gap={1} width="100%" xstyle={styles.activityGroup}>
        <ChatToolCalls
          calls={[
            {
              key: `hosted-${part.callId}`,
              name: part.name,
              status,
              target: part.target ?? '',
              errorMessage: status === 'error' ? 'The provider tool failed.' : undefined,
              resultDetail:
                part.raw === undefined ? undefined : (
                  <VStack gap={2} width="100%">
                    <CodeBlock
                      code={prettyToolArguments(part.raw)}
                      language="json"
                      size="sm"
                      width="100%"
                      container="card"
                      maxHeight={appConfig.tool_result_max_height_px}
                      hasCopyButton
                    />
                    {part.truncated !== undefined && (
                      <TruncatedNotice bytes={part.truncated.bytes} source={source} />
                    )}
                  </VStack>
                ),
            },
          ]}
          xstyle={styles.toolCallsFlush}
        />
        {part.images !== undefined && <StoredImages names={part.images} />}
      </VStack>
    );
  }

  const toolCallItems = group.items
    .filter(({part}) => part.type === 'tool_call')
    .map(({part, index}) => {
      if (part.type !== 'tool_call') {
        return undefined;
      }
      const itemIndex = group.items.findIndex(item => item.index === index);
      const result = group.items
        .slice(itemIndex + 1)
        .map(item => item.part)
        .find(
          (candidate): candidate is WebUiToolResult =>
            candidate.type === 'tool_result' && candidate.callId === part.callId,
        );
      return toolCallItem(part, result, rowWidthPx, source);
    })
    .filter((item): item is ChatToolCallItem => item !== undefined);
  const firstToolCallIndex = group.items.find(
    ({part}) => part.type === 'tool_call',
  )?.index;

  return (
    <VStack gap={1} width="100%" xstyle={styles.activityGroup}>
      {group.items.map(({part, index}, itemIndex) => {
        if (part.type === 'tool_call') {
          return index === firstToolCallIndex ? (
            <ChatToolCalls
              key={`tool-calls-${index}`}
              calls={toolCallItems}
              xstyle={styles.toolCallsFlush}
            />
          ) : null;
        }
        const hasCall = group.items
          .slice(0, itemIndex)
          .some(
            ({part: candidate}) =>
              candidate.type === 'tool_call' && candidate.callId === part.callId,
          );
        return hasCall ? null : (
          <OrphanToolResultBlock
            key={`tool-result-${part.callId}-${index}`}
            result={part}
            source={source}
          />
        );
      })}
    </VStack>
  );
}

function WebUiParts({
  parts,
  steps,
  rowWidthPx,
  isStreaming = false,
  source,
}: {
  parts: WebUiPart[];
  steps?: WebUiMessageStep[];
  rowWidthPx: number;
  isStreaming?: boolean;
  source?: TruncatedSource;
}) {
  const segments = segmentWebUiGroups(groupWebUiParts(parts, steps));
  const lastPartIndex = parts.length - 1;
  const [revealedSteps, setRevealedSteps] = useState(0);
  const revealScrollAnchorRef = useRef<ScrollPositionSnapshot | null>(null);

  useLayoutEffect(() => {
    const snapshot = revealScrollAnchorRef.current;
    revealScrollAnchorRef.current = null;
    holdAnchorPosition(snapshot, REVEAL_STEPS_SCROLL_OPTIONS);
  }, [revealedSteps]);

  /**
   * A tool-producing provider response is one step. Hide its complete range —
   * reasoning, assistant text, calls, and results — while keeping the terminal
   * no-tool assistant summary visible.
   */
  const visibleSteps = appConfig.message_step_window + revealedSteps;
  const hiddenStepIds = hiddenWebUiStepIds(steps, visibleSteps);

  return (
    <VStack gap={3} width="100%">
      {hiddenStepIds.size > 0 ? (
        <VStack align="center" gap={1} width="100%">
          <Button
            label="Load earlier steps"
            variant="ghost"
            size="sm"
            icon={<Icon icon="arrowUp" size="sm" />}
            onClick={event => {
              const scroller = findScrollParent(event.currentTarget);
              revealScrollAnchorRef.current = captureScrollPosition(
                scroller,
                event.currentTarget,
              );
              setRevealedSteps(current => current + appConfig.message_step_window_page);
            }}
          />
          <Text type="supporting" color="secondary">
            {hiddenStepIds.size} earlier{' '}
            {hiddenStepIds.size === 1 ? 'step is' : 'steps are'} not shown
          </Text>
        </VStack>
      ) : null}
      {segments.map((segment, segmentIndex) => {
        if (segment.type === 'content') {
          const contentPart = segment.item.group.item.part;
          const partIndex = segment.item.group.item.index;
          if (isWebUiPartHidden(steps, hiddenStepIds, partIndex)) {
            return null;
          }
          return (
            <VStack
              key={webUiGroupKey(segment.item.group, segment.item.index)}
              gap={1}
              width="100%"
            >
              <MarkdownText
                density="compact"
                contentWidth="100%"
                isStreaming={
                  isStreaming && segment.item.group.item.index === lastPartIndex
                }
                headingLevelStart={3}
              >
                {contentPart.text}
              </MarkdownText>
              {contentPart.citations !== undefined && contentPart.citations.length > 0 && (
                <CitationList citations={contentPart.citations} />
              )}
              {contentPart.truncated !== undefined && (
                <TruncatedNotice bytes={contentPart.truncated.bytes} source={source} />
              )}
            </VStack>
          );
        }

        const visibleItems = visibleWebUiItems(
          segment.items,
          item => webUiGroupIndex(item.group),
          steps,
          hiddenStepIds,
        );
        if (visibleItems.length === 0) {
          return null;
        }
        return (
          <VStack key={`activity-${segmentIndex}`} gap={0} width="100%">
            {visibleItems.map(({group, index}) => (
              <WebUiActivityGroup
                key={webUiGroupKey(group, index)}
                group={group}
                rowWidthPx={rowWidthPx}
                isStreaming={
                  isStreaming && webUiGroupIndex(group) === lastPartIndex
                }
                source={source}
              />
            ))}
          </VStack>
        );
      })}
    </VStack>
  );
}

function AgentAvatar({isRunning = false}: {isRunning?: boolean}) {
  return (
    <Avatar
      name="Astra"
      size="sm"
      status={
        <StatusDot
          variant={isRunning ? 'accent' : 'success'}
          label={isRunning ? 'Working' : 'Ready'}
          isPulsing={isRunning}
        />
      }
    />
  );
}

const CatalogEditorDialog = lazy(() =>
  import('./components/CatalogEditorDialog').then(module => ({default: module.CatalogEditorDialog})),
);

/** Datasets the floating left panel can show. One slot, so only one at a time. */
const LEFT_PANELS = {
  prompts: {label: 'prompt library'},
  search: {label: 'session search'},
  settings: {label: 'run settings'},
} as const;

type LeftPanelId = keyof typeof LEFT_PANELS;

function NotificationBellIcon(props: SVGProps<SVGSVGElement>) {
  return (
    <svg
      {...props}
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth={1.75}
      strokeLinecap="round"
      strokeLinejoin="round"
    >
      <path d="M18 8a6 6 0 0 0-12 0c0 7-3 7-3 9h18c0-2-3-2-3-9" />
      <path d="M10 21h4" />
    </svg>
  );
}

function SessionNavigation({
  groups,
  collapsedRefs,
  onCollapsedChange,
  rowTurns,
  sessions,
  runningViewKeys,
  activeViewKey,
  onSelect,
  onOpenProfile,
  openLeftPanel,
  onSelectLeftPanel,
  rightPanel,
  unreadCount,
  hasUnreadError,
  onToggleNotifications,
  onToggleCheckpoints,
  onShowReadme,
  isReadmeShown,
  onOpenProfileEditor,
}: {
  /** Every session as a group, with a child row for each of its profiles. */
  groups: SessionListGroup[];
  /** The sessions whose group the user collapsed. */
  collapsedRefs: readonly string[];
  onCollapsedChange: (sessionRef: string, isCollapsed: boolean) => void;
  /** What each row's own transcript holds, when its snapshot has reported it. */
  rowTurns: Readonly<Record<string, number>>;
  sessions: Session[];
  runningViewKeys: ReadonlySet<string>;
  activeViewKey: string;
  onSelect: (viewKey: string) => void;
  onOpenProfile: (sessionRef: string, profile: string) => void;
  openLeftPanel: LeftPanelId | null;
  onSelectLeftPanel: (panel: LeftPanelId) => void;
  rightPanel: RightPanelId | null;
  unreadCount: number;
  hasUnreadError: boolean;
  onToggleNotifications: () => void;
  onToggleCheckpoints: () => void;
  onShowReadme: () => void;
  isReadmeShown: boolean;
  onOpenProfileEditor: () => void;
}) {
  const categorySections = useMemo(() => groupByCategory(groups), [groups]);
  return (
    <SideNav
      aria-label="Agent sessions"
      footerIcons={
        <HStack align="center" justify="between" gap={2} width="100%">
          <IconButton
            data-panel-toggle=""
            label="Search sessions"
            icon={<Icon icon="search" size="sm" />}
            variant={openLeftPanel === 'search' ? 'secondary' : 'ghost'}
            size="sm"
            onClick={() => onSelectLeftPanel('search')}
          />
          <Avatar name="You" size="sm" />
        </HStack>
      }
      collapsible
    >
      {categorySections.map(section => (
        <SideNavSection
          key={section.category ?? ''}
          title={section.category ?? appConfig.session_uncategorized_title}
        >
          <SessionGroupList
            groups={section.groups}
            sessions={sessions}
            rowTurns={rowTurns}
            runningViewKeys={runningViewKeys}
            activeViewKey={activeViewKey}
            collapsedRefs={collapsedRefs}
            onCollapsedChange={onCollapsedChange}
            onSelect={onSelect}
            onOpenProfile={onOpenProfile}
          />
        </SideNavSection>
      ))}
      <SideNavSection title="Workspace">
        <SideNavItem
          data-panel-toggle=""
          label="Prompt library"
          icon={<Icon icon="copy" size="sm" />}
          isSelected={openLeftPanel === 'prompts'}
          onClick={() => onSelectLeftPanel('prompts')}
        />
        <SideNavItem
          data-panel-toggle=""
          label="Run settings"
          icon={<Icon icon="funnel" size="sm" />}
          isSelected={openLeftPanel === 'settings'}
          onClick={() => onSelectLeftPanel('settings')}
        />
        <SideNavItem
          label="Edit catalog"
          icon={<Icon icon="wrench" size="sm" />}
          onClick={onOpenProfileEditor}
        />
      </SideNavSection>
      <SideNavSection title="Activity">
        <SideNavItem
          data-panel-toggle=""
          label="Notifications"
          icon={<Icon icon={NotificationBellIcon} size="sm" />}
          isSelected={rightPanel === 'notifications'}
          endContent={unreadCount > 0 ? (
            <Badge label={String(unreadCount)} variant={hasUnreadError ? 'error' : 'info'} />
          ) : undefined}
          onClick={onToggleNotifications}
        />
        <SideNavItem
          data-panel-toggle=""
          label="README"
          icon={<Icon icon="app:readme" size="sm" />}
          isSelected={isReadmeShown}
          onClick={onShowReadme}
        />
        <SideNavItem
          data-panel-toggle=""
          label="Checkpoints"
          icon={<Icon icon="clock" size="sm" />}
          isSelected={rightPanel === 'checkpoints'}
          onClick={onToggleCheckpoints}
        />
      </SideNavSection>
    </SideNav>
  );
}

function RunInspector({
  activeSession,
  steps,
  tools,
  liveToolParts,
  inspector,
  isRunning,
  onClose,
}: {
  activeSession: Session;
  steps: ReasoningStep[];
  tools: ChatToolCallItem[];
  liveToolParts: WebUiPart[] | null;
  inspector: ResizableRegion;
  isRunning: boolean;
  onClose: () => void;
}) {
  const completedSteps = steps.filter(step => step.status === 'complete').length;
  const isLive = activeSession.source === 'live';
  // Rows measure their own width so titles fit the floating panel too.
  const [toolListRef, toolListWidthPx] = useRowWidth();
  const toolItems = liveToolParts
    ? toolActivityForParts(liveToolParts, toolListWidthPx)
    : isLive
      ? []
      : tools;

  return (
    <FloatingPanel
      side="end"
      label="Run inspector"
      title="Run inspector"
      subtitle={activeSession.title}
      region={inspector}
      onClose={onClose}
    >

        <Card variant="muted" padding={3}>
          <VStack gap={2}>
            <HStack align="center" justify="between" gap={2}>
              <HStack align="center" gap={2}>
                <StatusDot
                  variant={isRunning ? 'accent' : 'success'}
                  label={isRunning ? 'Run in progress' : 'Run complete'}
                  isPulsing={isRunning}
                />
                <Text type="label" weight="semibold">
                  {isRunning ? 'Working' : 'Ready'}
                </Text>
              </HStack>
              {steps.length > 0 && (
                <Text type="supporting" color="secondary">
                  {completedSteps}/{steps.length}
                </Text>
              )}
            </HStack>
            <Text type="supporting" color="secondary">
              {isLive
                ? 'Live response events and reasoning appear in the conversation when available.'
                : 'Reasoning and tool events are simulated for this preview.'}
            </Text>
          </VStack>
        </Card>

        {(!isLive || steps.length > 0) && (
          <VStack as="section" gap={2}>
            <HStack align="center" justify="between" gap={2}>
              <Heading level={2} accessibilityLevel={3} xstyle={styles.sectionHeading}>
                Reasoning trace
              </Heading>
              <Text type="supporting" color="secondary">
                {isLive ? 'Live' : 'Preview'}
              </Text>
            </HStack>
            <ReasoningBlock steps={steps} rowWidthPx={toolListWidthPx} />
          </VStack>
        )}

        <VStack as="section" gap={2} ref={toolListRef}>
          <HStack align="center" justify="between" gap={2}>
            <Heading level={2} accessibilityLevel={3}>
              Tool activity
            </Heading>
            <Badge label={String(toolItems.length)} />
          </HStack>
          <ChatToolCalls calls={toolItems} defaultIsExpanded />
        </VStack>

        <VStack gap={2} xstyle={styles.inspectorNote}>
          <Text type="label" weight="semibold">
            Harness note
          </Text>
          <Text type="supporting" color="secondary">
            {isLive
              ? 'Live provider reasoning is shown within the assistant response when available.'
              : 'The seeded preview keeps these regions as the contract for run state.'}
          </Text>
        </VStack>
    </FloatingPanel>
  );
}

function AssistantIdentity({model}: {model?: Session['model']}) {
  const providerName = model?.provider?.trim() || 'provider_name';
  const modelId = model?.model?.trim() || 'model_id';

  return (
    <HStack align="center" gap={2} xstyle={styles.assistantIdentity}>
      <Text type="label" weight="semibold">{providerName}</Text>
      <Text type="supporting" color="secondary" xstyle={styles.assistantModel}>
        {modelId}
      </Text>
    </HStack>
  );
}

type CopyState = 'idle' | 'copied' | 'error';

/**
 * Measured width available to message rows. The conversation lane is clamped by
 * its container (the run inspector takes part of it, and narrow windows squeeze
 * it), so a configured lane size is not the width rows actually render at.
 */
function useRowWidth(): [(node: HTMLElement | null) => void, number] {
  const [rowWidthPx, setRowWidthPx] = useState(0);
  const observerRef = useRef<ResizeObserver | null>(null);

  const measure = useCallback((node: HTMLElement) => {
    const computed = getComputedStyle(node);
    const inlinePadding =
      (Number.parseFloat(computed.paddingInlineStart) || 0) +
      (Number.parseFloat(computed.paddingInlineEnd) || 0);
    setRowWidthPx(Math.max(0, node.clientWidth - inlinePadding));
  }, []);

  const attach = useCallback(
    (node: HTMLElement | null) => {
      observerRef.current?.disconnect();
      observerRef.current = null;
      if (!node) {
        return;
      }
      measure(node);
      const observer = new ResizeObserver(() => measure(node));
      observer.observe(node);
      observerRef.current = observer;
    },
    [measure],
  );

  useEffect(() => () => observerRef.current?.disconnect(), []);

  return [attach, rowWidthPx];
}

async function copyMarkdownToClipboard(value: string): Promise<void> {
  if (navigator.clipboard?.writeText) {
    await navigator.clipboard.writeText(value);
    return;
  }

  const textarea = document.createElement('textarea');
  textarea.value = value;
  textarea.setAttribute('readonly', 'true');
  textarea.style.position = 'fixed';
  textarea.style.inset = '0';
  textarea.style.opacity = '0';
  document.body.appendChild(textarea);
  textarea.select();
  const copied = document.execCommand('copy');
  textarea.remove();
  if (!copied) {
    throw new Error('Clipboard access is unavailable');
  }
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

function copyActionLabel(state: CopyState): string {
  if (state === 'copied') {
    return 'Copied as Markdown';
  }
  if (state === 'error') {
    return 'Copy failed';
  }
  return 'Copy as Markdown';
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
function DeliveryStatus({status}: {status: 'read' | 'delivered'}) {
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

function AssistantMessage({
  message,
  model,
  onLoadFull,
}: {
  message: MessageRecord;
  model?: Session['model'];
  onLoadFull?: (message: MessageRecord) => void | Promise<void>;
}) {
  const [rowRef, rowWidthPx] = useRowWidth();
  const rowHeightRef = useRowHeightRef(message.id);
  const [copyState, setCopyState] = useState<CopyState>('idle');
  const resetTimerRef = useRef<number | null>(null);
  const markdown = assistantOutputMarkdown(message);

  useEffect(() => {
    return () => {
      if (resetTimerRef.current !== null) {
        window.clearTimeout(resetTimerRef.current);
      }
    };
  }, []);

  const setTemporaryCopyState = useCallback((state: CopyState) => {
    if (resetTimerRef.current !== null) {
      window.clearTimeout(resetTimerRef.current);
    }
    setCopyState(state);
    resetTimerRef.current = window.setTimeout(() => {
      resetTimerRef.current = null;
      setCopyState('idle');
    }, appConfig.copy_feedback_duration_ms);
  }, []);

  const copyAsMarkdown = useCallback(async () => {
    try {
      await copyMarkdownToClipboard(markdown);
      setTemporaryCopyState('copied');
    } catch {
      setTemporaryCopyState('error');
    }
  }, [markdown, setTemporaryCopyState]);

  return (
    <ChatMessage
      sender="assistant"
      avatar={<AgentAvatar />}
      ref={rowHeightRef}
      style={deferredRowStyle(message.id)}
    >
      <ChatMessageBubble
        ref={rowRef}
        variant="ghost"
        width="100%"
        xstyle={styles.assistantBubble}
        name={<AssistantIdentity model={model} />}
        metadata={
          <ChatMessageMetadata
            footer={
              <HStack align="center" gap={2}>
                {message.timestamp ? (
                  <Text type="inherit" color="inherit">{message.timestamp}</Text>
                ) : null}
                <IconButton
                  label={copyActionLabel(copyState)}
                  tooltip={copyActionLabel(copyState)}
                  icon={
                    <Icon
                      icon={copyState === 'copied' ? 'check' : copyState === 'error' ? 'error' : 'copy'}
                      size="xsm"
                      color="secondary"
                    />
                  }
                  variant="ghost"
                  size="sm"
                  isDisabled={markdown.length === 0}
                  onClick={() => { void copyAsMarkdown(); }}
                />
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
              rowWidthPx={rowWidthPx}
              source={onLoadFull === undefined ? undefined : {message, onLoadFull}}
            />
          ) : (
            <>
              {(message.reasoning || message.reasoningContent) && (
                <ReasoningBlock
                  steps={message.reasoning ?? []}
                  content={message.reasoningContent}
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
            <VStack gap={2}>
              <HStack align="center" gap={2}>
                <Text type="label" weight="semibold">
                  Tool activity
                </Text>
                <Badge label={String(message.tools.length)} />
              </HStack>
              <ChatToolCalls calls={message.tools} defaultIsExpanded />
            </VStack>
          )}
        </VStack>
      </ChatMessageBubble>
    </ChatMessage>
  );
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
const TranscriptRow = memo(
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

/**
 * The assistant's own words live in an Astryx message bubble. The row's
 * controls, the user's own messages and the tool documents all sit outside it,
 * so the quote action is offered from inside that bubble and nowhere else.
 */
const ASSISTANT_BUBBLE_SELECTOR = '.astryx-chat-message-bubble[data-sender="assistant"]';

/** Tool call rows and their result detail are the tool's output, not a reply. */
const TOOL_OUTPUT_SELECTOR = '.astryx-chat-tool-calls';

function elementOf(node: Node): Element | null {
  return node.nodeType === Node.ELEMENT_NODE ? (node as Element) : node.parentElement;
}

/**
 * The draft a selection should insert, with the place its menu belongs.
 *
 * Both ends of the range are checked: a selection that starts in the agent's
 * answer and ends in a tool document would quote raw tool text, and one that
 * reaches across messages would quote timestamps and controls into the draft.
 */
function selectionQuoteTarget(): {draft: string; anchor: QuoteAnchor} | null {
  const selection = window.getSelection();
  if (selection === null || selection.isCollapsed || selection.rangeCount === 0) {
    return null;
  }
  const range = selection.getRangeAt(0);
  const start = elementOf(range.startContainer);
  const end = elementOf(range.endContainer);
  if (start === null || end === null) {
    return null;
  }
  const startBubble = start.closest(ASSISTANT_BUBBLE_SELECTOR);
  const endBubble = end.closest(ASSISTANT_BUBBLE_SELECTOR);
  // A triple click ends at the start of the next block, which can lie outside
  // the bubble; that is still inside the answer if no text was crossed to get there.
  let endsInside = endBubble !== null;
  if (!endsInside && startBubble !== null) {
    const beyondBubble = range.cloneRange();
    beyondBubble.setStart(startBubble, startBubble.childNodes.length);
    endsInside = beyondBubble.toString().trim() === '';
  }
  const draft = quoteDraftForSelection(
    {
      text: selection.toString(),
      isInsideAssistantMessage: startBubble !== null && endsInside,
      isInsideToolOutput:
        start.closest(TOOL_OUTPUT_SELECTOR) !== null ||
        end.closest(TOOL_OUTPUT_SELECTOR) !== null,
    },
    appConfig.selection_quote_max_chars,
  );
  if (draft === null) {
    return null;
  }
  const rect = range.getBoundingClientRect();
  const isAbove = rect.top - appConfig.selection_quote_menu_gap_px >= appConfig.selection_quote_menu_min_top_px;
  return {
    draft,
    anchor: {
      x: rect.left,
      y: isAbove
        ? rect.top - appConfig.selection_quote_menu_gap_px
        : rect.bottom + appConfig.selection_quote_menu_gap_px,
      isAbove,
    },
  };
}

function App() {
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
  const [isModelDefaultUpdating, setIsModelDefaultUpdating] = useState(false);
  const isModelDefaultUpdatingRef = useRef(false);
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
    appConfig.show_inspector_by_default ? 'inspector' : null,
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
  const toggleCheckpoints = useCallback(() => {
    setRightPanelMode(selectionAfterPanelToggle(rightPanelRef.current, 'checkpoints'));
  }, [setRightPanelMode]);
  // One left slot, several datasets: the panel shows whichever workspace view
  // was picked last, so two panels never stack on the same edge. The closed
  // state remembers that choice, because the composer toggle reopens it.
  const [leftPanel, setLeftPanel] = useState<LeftPanelId | null>(
    appConfig.show_prompt_library_by_default ? 'prompts' : null,
  );
  const [lastLeftPanel, setLastLeftPanel] = useState<LeftPanelId>('prompts');
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
  // The left-hand counterpart of the inspector, hosting whichever workspace
  // dataset is selected. One region serves them all, so the width the user
  // dragged is the width every left-hand view opens at.
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
  const toggleLeftPanel = useCallback(() => {
    setLeftPanel(current => (current ? null : lastLeftPanel));
  }, [lastLeftPanel]);
  const selectLeftPanel = useCallback((panel: LeftPanelId) => {
    setLastLeftPanel(panel);
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
  // "Attach context" reads the chosen files in the browser and hands their text
  // to the composer through the same registered insert the prompt library uses,
  // so no draft state has to move back up into App.
  const attachInputRef = useRef<HTMLInputElement | null>(null);
  const handleAttachFiles = useCallback(async (event: ChangeEvent<HTMLInputElement>) => {
    const files = Array.from(event.target.files ?? []);
    event.target.value = '';
    if (files.length === 0) {
      return;
    }
    const blocks = await readAttachmentBlocks(
      files,
      appConfig.attach_context_max_chars_per_file,
      appConfig.attach_context_read_concurrency,
    );
    insertIntoComposerRef.current?.text(blocks.join('\n\n'));
  }, []);
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
  const clearRunInteraction = useCallback((viewKey: string, turnId?: string) => {
    const activeTurnId = activeRunTurnRefs.current.get(viewKey);
    if (turnId !== undefined && activeTurnId !== undefined && activeTurnId !== turnId) return;
    activeRunTurnRefs.current.delete(viewKey);
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
  const snapshotEtagRef = useRef<string | undefined>(undefined);
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
    if (viewKeys.has(activeViewKeyRef.current)) {
      snapshotEtagRef.current = undefined;
    }
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

  // A session of its own: the picker shows the model this row runs.
  const selectedModel = selectedModelForSession(
    activeRowSession.models,
    activeRowSession.modelDefault,
    activeRowSession.model?.model,
  );
  const selectSessionModel = useCallback(async (modelDefault: string) => {
    const sessionRef = activeSession?.sessionRef;
    const targetViewKey = activeViewKey;
    const targetProfile = activeProfile;
    const targetRowSession = activeRowSession;
    if (
      sessionRef === undefined ||
      targetRowSession.modelDefault === modelDefault ||
      resettingViewKey !== null ||
      frontCompactingViewKey !== null ||
      isModelDefaultUpdatingRef.current
    ) {
      return;
    }
    isModelDefaultUpdatingRef.current = true;
    setIsModelDefaultUpdating(true);
    try {
      const result = await updateSessionProfileModelDefault(
        sessionRef,
        modelDefault,
        targetProfile,
      );
      updateViewSessionRecord(targetViewKey, session => ({
        ...session,
        activeProfile: result.active_profile,
        profiles: result.profiles,
        modelDefault: result.model_default,
        models: result.models,
        model: result.model,
      }), targetRowSession);
      sessionsEtagRef.current = undefined;
      if (activeViewKeyRef.current === targetViewKey) {
        snapshotEtagRef.current = undefined;
      }
      refreshSessionsRef.current?.();
    } catch (error) {
      recordNotification({
        sessionId: activeSessionId,
        sessionTitle: targetRowSession.title,
        kind: 'error',
        title: 'Unable to change the profile model',
        message: error instanceof Error ? error.message : String(error),
      });
    } finally {
      isModelDefaultUpdatingRef.current = false;
      setIsModelDefaultUpdating(false);
    }
  }, [
    activeSession,
    activeSessionId,
    activeViewKey,
    activeProfile,
    activeRowSession,
    recordNotification,
    resettingViewKey,
    frontCompactingViewKey,
    updateViewSessionRecord,
  ]);
  const threadMessages = threads[activeViewKey] ?? [];
  // The history block renders ahead of the transcript: it holds the turns a
  // compaction removed, which are older than everything the transcript carries.
  const messages = useMemo(() => {
    const block = historyMessagesFor(historyBlocks, activeViewKey);
    return block === undefined ? threadMessages : [...block, ...threadMessages];
  }, [threadMessages, activeViewKey, historyBlocks]);

  const isLiveSession = activeSession?.source === 'live';
  const isRunning = isSessionRunning(
    runningViewKeys.has(activeViewKey),
    rowRunStatus(activeRow, activeSession)?.status,
  );
  const hasLiveAttachedRun =
    isLiveSession &&
    (rowRunStatus(activeRow, activeSession)?.status === 'running' ||
      attachedRunViewKeysRef.current.has(activeViewKey));
  const transcriptMessages = useMemo(
    () => suppressAttachedRunTranscriptTail(messages, hasLiveAttachedRun),
    [messages, hasLiveAttachedRun],
  );
  const {visibleMessages, hiddenCount, revealEarlier} = useTranscriptWindow({
    messages: transcriptMessages,
    sessionId: activeViewKey,
    initialCount: appConfig.transcript_window_messages,
    pageSize: appConfig.transcript_window_page_messages,
  });
  const placedFailures = useMemo(
    () => placeRunFailures(sessionFailures[activeViewKey], visibleMessages.map(message => message.id)),
    [sessionFailures, activeViewKey, visibleMessages],
  );
  const activeSessionRef = activeSession?.sessionRef;
  // Where a tool result's stored images are read from; memoised so the rows do not refetch.
  const storedImageScope = useMemo(
    () => (activeSessionRef === undefined ? undefined : {sessionRef: activeSessionRef, profile: activeProfile}),
    [activeSessionRef, activeProfile],
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
  const streamingMessageCommitted = streamingMessage !== null && transcriptMessages.some(
    message => message.id === streamingMessage.id,
  );

  /**
   * The question this composer can answer. Prefer independent run state so the
   * prompt is not gated on a message completing; message.pending remains a
   * compatibility fallback. The question stays on the view where its run waits.
   */
  const activeRunRetry = runRetriesByView[activeViewKey];
  const activeRunInteractionState = runInteractionStatesByView[activeViewKey];
  const pendingInteraction = activeRunInteractionState?.received
    ? activeRunInteractionState.pending
    : streamingMessage?.pending;

  const reasoningSteps = useMemo(() => {
    const steps = isLiveSession ? [] : baseReasoning;
    return getTraceForSteps(steps, isRunning ? runProgress : steps.length);
  }, [isLiveSession, isRunning, runProgress]);

  const liveToolParts = useMemo(() => {
    if (!isLiveSession || (isRunning && !streamingMessage)) {
      return null;
    }
    const sourceMessage = streamingMessage ?? [...messages]
      .reverse()
      .find(message => message.sender === 'assistant' && message.parts?.some(part => part.type === 'tool_call'));
    return sourceMessage?.parts ?? null;
  }, [isLiveSession, isRunning, messages, streamingMessage]);

  const tools = useMemo(() => {
    const latestAssistant = [...messages]
      .reverse()
      .find(message => message.sender === 'assistant' && message.tools);
    const sourceTools = latestAssistant?.tools ?? defaultTools;

    if (!isRunning || isLiveSession) {
      return sourceTools;
    }

    return sourceTools.map(
      (tool, index): ChatToolCallItem => ({
        ...tool,
        status: index === 0 ? 'running' : 'pending',
        duration: undefined,
      }),
    );
  }, [isLiveSession, isRunning, messages]);

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
      if (activeViewKeyRef.current === targetViewKey) {
        snapshotEtagRef.current = undefined;
      }
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
      if (activeViewKeyRef.current === targetViewKey) {
        snapshotEtagRef.current = undefined;
      }
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
      if (activeViewKeyRef.current === targetViewKey) {
        snapshotEtagRef.current = undefined;
      }
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
      if (activeViewKeyRef.current === targetViewKey) {
        snapshotEtagRef.current = undefined;
      }
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
        void streamSessionRun(sessionRef, buildRunInput(trimmedValue, images, files), {
          signal: controller.signal,
          ...(activeProfile === undefined ? {} : {profile: activeProfile}),
          onRun: run => recordRunStarted(sessionId, run.turn_id),
          onInteraction: (turnId, interaction) =>
            recordRunInteraction(sessionId, turnId, interaction),
          onInteractionEnd: (turnId, end) =>
            recordInteractionEnd(sessionId, activeRowSession.title, turnId, end),
          onHookReport: (turnId, report) =>
            recordHookReport(sessionId, activeRowSession.title, turnId, report),
          onProviderRetry: (turnId, wait) => recordRunRetry(sessionId, turnId, wait),
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
      // A different view: nothing the client holds applies to it. The list is
      // revalidated in full too — a run detached on the way here may have been
      // dropped from this client's copy of that session.
      snapshotEtagRef.current = undefined;
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
        etag: snapshotEtagRef.current,
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
      // A prior view's request can finish after a switch. Keep its useful cached
      // transcript, but do not overwrite the active view's conditional token.
      if (activeViewRef.current.viewKey === uiId) {
        snapshotEtagRef.current = result.etag;
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

  const composerPane = (
    <ComposerPane
      activeSessionId={activeSessionId}
      draftKey={activeViewKey}
      onSubmit={submitMessage}
      onStop={stopRun}
      isRunning={isRunning}
      isDisabled={isReadmeShown}
      placeholder={isReadmeShown ? 'Choose a session to start chatting' : 'Ask the agent to inspect a task…'}
      showInspector={rightPanel === 'inspector'}
      onToggleInspector={toggleInspector}
      isLeftPanelOpen={leftPanel !== null}
      leftPanelLabel={LEFT_PANELS[leftPanel ?? lastLeftPanel].label}
      onToggleLeftPanel={toggleLeftPanel}
      onRegisterInsert={registerComposerInsert}
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
        ) : isUsageDrawerCollapsed ? null : (
          <UsageDetailsDrawer
            sessionName={activeRowSession.title}
            usage={activeRowSession.usage}
          />
        )
      }
      toolbarActions={isReadmeShown ? undefined : (
        <HStack align="center" gap={1}>
          {activeRowSession.models && activeRowSession.models.length > 0 ? (
            <ModelPicker
              models={activeRowSession.models}
              selectedModel={selectedModel}
              isDisabled={
                activeSession.sessionRef === undefined ||
                isRunning ||
                isCompactionInProgress ||
                isModelDefaultUpdating ||
                resettingViewKey !== null ||
                frontCompactingViewKey !== null
              }
              onSelect={model => { void selectSessionModel(model); }}
            />
          ) : null}
          {appConfig.attach_context_enabled ? (
            <>
              <IconButton
                label="Attach context"
                icon={<Icon icon="copy" size="sm" />}
                variant="ghost"
                size="sm"
                onClick={() => attachInputRef.current?.click()}
              />
              <input
                ref={attachInputRef}
                type="file"
                multiple
                accept={appConfig.attach_context_accept}
                onChange={handleAttachFiles}
                hidden
              />
            </>
          ) : null}
          <IconButton
            label={isUsageDrawerCollapsed ? 'Show usage details' : 'Hide usage details'}
            icon={<Icon icon="info" size="sm" />}
            variant="ghost"
            size="sm"
            onClick={() => {
              setStateContextViewKey(null);
              setIsUsageDrawerCollapsed(collapsed => !collapsed);
            }}
          />
          {isLiveSession && activeSession.sessionRef && (
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
          )}
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
          {isLiveSession && activeSession.sessionRef && (
            /*
             * The two destructive actions share one trigger, last in the row.
             * Neither is a stray click away from the compact button any more,
             * and each still asks for confirmation in the composer drawer.
             */
            <IconButton
              label="Conversation actions"
              tooltip="Reset this conversation or delete a run"
              icon={<Icon icon="moreHorizontal" size="sm" />}
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
                setComposerActionsViewKey(current =>
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
                          avatar={<AgentAvatar isRunning={pendingInteraction === undefined} />}
                          ref={liveRowHeightRef}
                          style={deferredRowStyle(null)}
                        >
                          <ChatMessageBubble
                            ref={liveBubbleRef}
                            variant="ghost"
                            width="100%"
                            xstyle={styles.assistantBubble}
                            name={<AssistantIdentity model={activeRowSession.model} />}
                          >
                            {isLiveSession && streamingMessage ? (
                              <WebUiParts
                                parts={streamingMessage.parts}
                                steps={streamingMessage.steps}
                                rowWidthPx={liveBubbleWidthPx}
                                isStreaming={pendingInteraction === undefined}
                              />
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
        {leftPanel === 'prompts' && (
          <PromptLibraryPanel
            region={leftPanelRegion}
            onClose={() => setLeftPanel(null)}
            onUsePrompt={usePrompt}
            onUseTemplate={useTemplate}
            onUseSkill={useSkill}
          />
        )}
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
    </AppShell>
  );
}

export default App;
