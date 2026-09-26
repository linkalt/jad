import {memo, useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState} from 'react';
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
  SideNavHeading,
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
import {useToast} from '@astryxdesign/core/Toast';
import {
  hasUnreadActivityError,
  markActivityNotificationsRead,
  prependActivityNotification,
  unreadActivityNotificationCount,
  type ActivityNotification,
  type NewActivityNotification,
} from './activity-notifications';
import {
  attachSessionRun,
  compactSession,
  fetchSession,
  fetchSessionHistory,
  fetchSessionMessage,
  fetchSessions,
  updateSessionPathProfile,
  updateSessionProfileModelDefault,
  stopSessionRun,
  streamSessionRun,
  type SessionPage,
  type SessionSnapshot,
  type WebUiMessage,
  type WebUiMessageStep,
  type WebUiPart,
} from './api';
import appConfig from './config.json';
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
import {
  isSessionRunning,
  reconcileDetachedRunSessionIds,
  sessionUiStateForRun,
  withSnapshotRun,
  type SessionUiState,
} from './session-run-status';
import {shouldSkipTranscriptLoad} from './transcript-load-policy';
import {
  hiddenWebUiStepIds,
  isWebUiPartHidden,
  visibleWebUiItems,
  webUiStepAtPartIndex,
} from './web-ui-steps';
import {applyPart, applyPartDelta} from './messagePatch';
import {ComposerPane} from './components/ComposerPane';
import {ModelPicker} from './components/ModelPicker';
import {SessionProfilePicker} from './components/SessionProfilePicker';
import {FloatingPanel} from './components/FloatingPanel';
import {NotificationPanel} from './components/NotificationPanel';
import {MarkdownText} from './components/MarkdownText';
import {PromptLibraryPanel} from './components/PromptLibraryPanel';
import {RunSettingsPanel} from './components/RunSettingsPanel';
import {SessionSearchPanel} from './components/SessionSearchPanel';
import {EarlierMessagesControl, useTranscriptWindow} from './components/TranscriptWindow';
import {UsageDetailsDrawer} from './components/UsageDetailsDrawer';
import type {PromptTemplate} from './promptLibrary';
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
import {ContextMenu} from '@astryxdesign/core/ContextMenu';
import {
  addPinnedView,
  isPinnedViewKey,
  profileMenuTargets,
  profileOfViewKey,
  profileSelection,
  prunePinnedViews,
  removePinnedView,
  restoreViews,
  rowHasRunningRun,
  rowRunStatus,
  sessionListRows,
  sessionRefOfViewKey,
  viewKey,
  viewLabel,
  type PinnedView,
  type SessionListRow,
} from './session-views';

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
  // Off-screen probes used only to resolve the fonts a tool row renders with.
  textProbe: {
    position: 'absolute',
    insetInlineStart: '-10000px',
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
type TraceState = 'complete' | 'running' | 'pending';
interface Session {
  id: string;
  title: string;
  time: string;
  turns: number;
  state: SessionState;
  source: 'live' | 'sample';
  sessionRef?: string;
  rootLabel?: string;
  activeProfile?: string;
  pathProfiles?: string[];
  modelDefault?: string;
  models?: string[];
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
  parts?: WebUiPart[];
  reasoning?: ReasoningStep[];
  tools?: ChatToolCallItem[];
};

/**
 * History the snapshot left out: the turns a compaction removed, served from the
 * backup. `etag` is the identity the server published for it.
 */
type HistoryBlock = {uiId: string; etag: string; messages: MessageRecord[]};

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
  limit: appConfig.session_views_max,
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
    target: 'Agent harness regions',
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
    state: sessionUiStateForRun(snapshot.run?.status),
    source: 'live',
    sessionRef: snapshot.id,
    ...(snapshot.runs_by_profile === undefined
      ? {}
      : {runsByProfile: snapshot.runs_by_profile}),
    ...(snapshot.root_label ? {rootLabel: snapshot.root_label} : {}),
    ...(snapshot.active_profile === undefined ? {} : {activeProfile: snapshot.active_profile}),
    ...(snapshot.path_profiles === undefined ? {} : {pathProfiles: snapshot.path_profiles}),
    ...(snapshot.model_default === undefined ? {} : {modelDefault: snapshot.model_default}),
    ...(snapshot.models === undefined ? {} : {models: snapshot.models}),
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
  if (source === undefined) {
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
        <VStack gap={1} width="100%">
          <Text type="supporting" color="secondary">Arguments</Text>
          <CodeBlock
            code={prettyToolArguments(call.arguments)}
            language="json"
            size="sm"
            width="100%"
            container="section"
            hasCopyButton
          />
        </VStack>
        {result && (
          <VStack gap={1} width="100%">
            <HStack align="center" gap={2}>
              <Text type="supporting" color="secondary">Result</Text>
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
              hasCopyButton
            />
            {result.truncated !== undefined && (
              <TruncatedNotice bytes={result.truncated.bytes} source={source} />
            )}
          </VStack>
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
          hasCopyButton
        />
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
    groups.push({type: 'content', item: {part, index}});
  });
  flushTools();

  return groups;
}

type WebUiActivityGroupPart = Extract<WebUiPartGroup, {type: 'reasoning' | 'tools'}>;
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
  rows,
  rowTurns,
  sessions,
  runningViewKeys,
  activeViewKey,
  onSelect,
  onOpenProfile,
  onRemoveView,
  canPinProfile,
  onNewSession,
  openLeftPanel,
  onSelectLeftPanel,
  rightPanel,
  unreadCount,
  hasUnreadError,
  onToggleNotifications,
}: {
  /** Every session, each followed by the profile rows pinned to it. */
  rows: SessionListRow[];
  /** What each row's own transcript holds, when its snapshot has reported it. */
  rowTurns: Readonly<Record<string, number>>;
  sessions: Session[];
  runningViewKeys: ReadonlySet<string>;
  activeViewKey: string;
  onSelect: (viewKey: string) => void;
  onOpenProfile: (sessionRef: string, profile: string) => void;
  onRemoveView: (viewKey: string) => void;
  canPinProfile: boolean;
  onNewSession: () => void;
  openLeftPanel: LeftPanelId | null;
  onSelectLeftPanel: (panel: LeftPanelId) => void;
  rightPanel: RightPanelId | null;
  unreadCount: number;
  hasUnreadError: boolean;
  onToggleNotifications: () => void;
}) {
  // The ✕ of a pinned row appears while the pointer is over that row, so the
  // list stays quiet; the row's context menu is the keyboard path to the same
  // removal.
  const [hoveredViewKey, setHoveredViewKey] = useState<string | null>(null);
  return (
    <SideNav
      aria-label="Agent sessions"
      header={
        <SideNavHeading
          superheading="Local playground"
          heading="Forge"
          subheading="Agent harness"
          icon={<Icon icon="wrench" color="accent" />}
        />
      }
      topContent={
        <Button
          label="New session"
          variant="primary"
          width="100%"
          icon={<Icon icon="arrowUp" size="sm" />}
          onClick={onNewSession}
        />
      }
      footer={
        <VStack gap={1}>
          <HStack align="center" gap={2}>
            <StatusDot variant="success" label="Backend ready" />
            <Text type="supporting" color="secondary">
              Live session available
            </Text>
          </HStack>
          <Text type="supporting" color="secondary">
            Live sessions stream from the backend; seeded runs remain previews.
          </Text>
        </VStack>
      }
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
      <SideNavSection title="Recent runs">
        {rows.map(row => {
          const session = sessions.find(candidate => candidate.id === row.sessionRef);
          if (session === undefined) {
            return null;
          }
          const run = rowRunStatus(row, session);
          const state = rowHasRunningRun(row, session, runningViewKeys)
            ? 'accent'
            : row.profile === undefined
              ? session.state
              : sessionUiStateForRun(run?.status);
          // A session row offers its other profiles; the profile it already
          // shows is left out, because its row is this one.
          const pinnedProfiles = profileMenuTargets({
            id: session.id,
            title: session.title,
            ...(session.pathProfiles === undefined ? {} : {profiles: session.pathProfiles}),
            ...(session.activeProfile === undefined ? {} : {activeProfile: session.activeProfile}),
          });
          const menuItems = row.isPinned
            ? [
                {
                  id: `remove-${row.key}`,
                  label: `Remove ${row.label}`,
                  onClick: () => onRemoveView(row.key),
                },
              ]
            : pinnedProfiles.length === 0
              ? [{id: 'no-profiles', label: 'No other profiles', isDisabled: true}]
              : pinnedProfiles.map(target => ({
                  id: `pin-${target.profile}`,
                  label: target.label,
                  isDisabled: !canPinProfile,
                  onClick: () => onOpenProfile(row.sessionRef, target.profile),
                }));
          return (
            <ContextMenu
              key={row.key}
              label={`${row.label} actions`}
              items={menuItems}
            >
              <SideNavItem
                label={row.label}
                isSelected={row.key === activeViewKey}
                onClick={() => onSelect(row.key)}
                onMouseEnter={() => setHoveredViewKey(row.key)}
                onMouseLeave={() => setHoveredViewKey(current => (current === row.key ? null : current))}
                icon={<StatusDot variant={state} label={`${row.label} ${state}`} />}
                endContent={
                  row.isPinned ? (
                    hoveredViewKey === row.key ? (
                      <IconButton
                        label={`Remove ${row.label}`}
                        icon={<Icon icon="close" size="sm" />}
                        variant="ghost"
                        size="sm"
                        onClick={() => onRemoveView(row.key)}
                      />
                    ) : undefined
                  ) : (
                    <Badge label={String(rowTurns[row.key] ?? session.turns)} />
                  )
                }
              />
            </ContextMenu>
          );
        })}
      </SideNavSection>
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
            timestamp={message.timestamp}
            footer={
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
            }
            status="delivered"
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
  }: {
    message: MessageRecord;
    model: Session['model'] | undefined;
    onLoadFull?: (message: MessageRecord) => void | Promise<void>;
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
      return (
        <ChatMessage
          sender="user"
          avatar={<Avatar name="You" size="sm" />}
          ref={rowHeightRef}
          style={deferredRowStyle(message.id)}
        >
          <ChatMessageBubble
            metadata={<ChatMessageMetadata timestamp={message.timestamp} status="read" />}
          >
            <MarkdownText density="compact" contentWidth="100%">{message.text}</MarkdownText>
          </ChatMessageBubble>
        </ChatMessage>
      );
    }

    return <AssistantMessage message={message} model={model} onLoadFull={onLoadFull} />;
  },
  // The poll and the session list rebuild messages and models as fresh objects,
  // so fall back to a structural compare when the identities differ.
  (previous, next) =>
    (previous.message === next.message || deepEqual(previous.message, next.message)) &&
    (previous.model === next.model || deepEqual(previous.model, next.model)),
);

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
  const [isProfileSwitching, setIsProfileSwitching] = useState(false);
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
   * compactions, so the block is fetched once per identity the snapshot hands
   * out — a committed step re-sends the live tail, not the history.
   */
  const [historyBlock, setHistoryBlock] = useState<HistoryBlock | null>(null);
  const historyRef = useRef<HistoryBlock | null>(null);
  const setHistory = useCallback((next: HistoryBlock | null) => {
    historyRef.current = next;
    setHistoryBlock(next);
  }, []);
  const [rightPanel, setRightPanel] = useState<RightPanelId | null>(
    appConfig.show_inspector_by_default ? 'inspector' : null,
  );
  const rightPanelRef = useRef<RightPanelId | null>(rightPanel);
  const setRightPanelMode = useCallback((nextPanel: RightPanelId | null) => {
    rightPanelRef.current = nextPanel;
    setRightPanel(nextPanel);
  }, []);
  const toggleInspector = useCallback(() => {
    setRightPanelMode(selectionAfterPanelToggle(rightPanelRef.current, 'inspector'));
  }, [setRightPanelMode]);
  // One left slot, several datasets: the panel shows whichever workspace view
  // was picked last, so two panels never stack on the same edge. The closed
  // state remembers that choice, because the composer toggle reopens it.
  const [leftPanel, setLeftPanel] = useState<LeftPanelId | null>(
    appConfig.show_prompt_library_by_default ? 'prompts' : null,
  );
  const [lastLeftPanel, setLastLeftPanel] = useState<LeftPanelId>('prompts');
  const [runningSessionId, setRunningSessionId] = useState<string | null>(null);
  const [detachedRunSessionIds, setDetachedRunSessionIds] = useState<ReadonlySet<string>>(
    new Set(),
  );
  const [compactingSessionRef, setCompactingSessionRef] = useState<string | null>(null);
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
  const [runProgress, setRunProgress] = useState(baseReasoning.length);
  const [streamingMessage, setStreamingMessage] = useState<WebUiMessage | null>(null);
  // The run stream sends increments instead of a snapshot per provider delta, so
  // the message on screen is merged here; the id check keeps a stale stream from
  // patching the message a newer run already replaced.
  const applyRunDelta = useCallback((messageId: string, index: number, append: string) => {
    setStreamingMessage(current =>
      current !== null && current.id === messageId ? applyPartDelta(current, index, append) : current,
    );
  }, []);
  const applyRunPart = useCallback((messageId: string, index: number, part: WebUiPart) => {
    setStreamingMessage(current =>
      current !== null && current.id === messageId ? applyPart(current, index, part) : current,
    );
  }, []);
  const conversation = useResizable({
    defaultSize: pixel(appConfig.conversation_lane_default_size),
    minSize: pixel(appConfig.conversation_lane_min_size),
    maxSize: pixel(appConfig.conversation_lane_max_size),
    direction: 'horizontal',
    autoSaveId: 'agent-harness-conversation-width',
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
    autoSaveId: 'agent-harness-run-inspector',
  });
  // The left-hand counterpart of the inspector, hosting whichever workspace
  // dataset is selected. One region serves them all, so the width the user
  // dragged is the width every left-hand view opens at.
  const leftPanelRegion = useResizable({
    defaultSize: pixel(appConfig.left_panel_default_size),
    minSize: pixel(appConfig.left_panel_min_size),
    maxSize: pixel(appConfig.left_panel_max_size),
    direction: 'horizontal',
    autoSaveId: 'agent-harness-prompt-library',
  });

  // Filled by ComposerPane, which owns the drafts. Views outside the composer
  // (the prompt library) write through this instead of lifting draft state back
  // into App, which is what made typing slow on long sessions.
  const insertIntoComposerRef = useRef<((text: string) => void) | null>(null);  const registerComposerInsert = useCallback(
    (insert: ((text: string) => void) | null) => {
      insertIntoComposerRef.current = insert;
    },
    [],
  );
  const toggleLeftPanel = useCallback(() => {
    setLeftPanel(current => (current ? null : lastLeftPanel));
  }, [lastLeftPanel]);
  const selectLeftPanel = useCallback((panel: LeftPanelId) => {
    setLastLeftPanel(panel);
    setLeftPanel(current => (current === panel ? null : panel));
  }, []);
  const usePrompt = useCallback((prompt: PromptTemplate) => {
    insertIntoComposerRef.current?.(prompt.body);
  }, []);
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
    const limit = appConfig.attach_context_max_chars_per_file;
    const blocks = await Promise.all(
      files.map(async file => {
        const text = await file.text();
        const body =
          text.length > limit
            ? `${text.slice(0, limit)}\n\n… truncated at ${limit} characters`
            : text;
        return `**Attached: ${file.name}**\n\n\`\`\`\n${body}\n\`\`\``;
      }),
    );
    insertIntoComposerRef.current?.(blocks.join('\n\n'));
  }, []);
  const runAbortRef = useRef<AbortController | null>(null);
  const runTimeoutRef = useRef<number | null>(null);
  const activeRunSessionIdRef = useRef<string | null>(null);
  const attachedRunSessionIdRef = useRef<string | null>(null);
  const hydratedTranscriptSessionIdsRef = useRef(new Set<string>());
  // The run this client is streaming, if any. It is how a page reload
  // re-attaches to a run it did not start.
  const attachedTurnRef = useRef<string | null>(null);
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
          const block = historyRef.current;
          if (block !== null && block.uiId === uiId) {
            const index = block.messages.findIndex(candidate => candidate.id === message.id);
            if (index >= 0) {
              const messages = [...block.messages];
              messages[index] = full;
              setHistory({...block, messages});
            }
          }
        })
        .catch(error => {
          console.warn('Unable to load the full message:', error);
        });
    },
    [setHistory],
  );
  // How much of the transcript is still on the server, per session.
  const [sessionPages, setSessionPages] = useState<Record<string, SessionPage>>({});

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
  const profileRowLimitReached = pinnedViews.length >= appConfig.session_views_max;

  /**
   * What each row's own snapshot reported about it, keyed by row.
   *
   * The session's own row is the list's record; a pinned row keeps one here, and
   * its snapshot fills it. That record is what a row *is* to the rest of the app:
   * its own transcript, model, profiles and usage, sharing the session's
   * `setting.json` with the other rows.
   */
  const [rowSessions, setRowSessions] = useState<Record<string, Session>>({});

  const activeRowSession: Session = !isPinnedView
    ? activeSession
    : rowSessions[activeViewKey] ?? {
        id: activeViewKey,
        sessionRef: activeSessionId,
        profile: activeProfile,
        title: viewLabel(activeSession.title, activeProfile),
        time: activeSession.time,
        turns: 0,
        state: sessionUiStateForRun(rowRunStatus(activeRow, activeSession)?.status),
        source: 'live',
        activeProfile,
        ...(activeSession.pathProfiles === undefined
          ? {}
          : {pathProfiles: activeSession.pathProfiles}),
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
          title: viewLabel(fromSnapshot.title, profile),
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
      isProfileSwitching ||
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
        pathProfiles: result.path_profiles,
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
    isProfileSwitching,
    recordNotification,
    updateViewSessionRecord,
  ]);
  const threadMessages = threads[activeViewKey] ?? [];
  // The history block renders ahead of the transcript: it holds the turns a
  // compaction removed, which are older than everything the transcript carries.
  const messages = useMemo(() => {
    const block =
      historyBlock !== null && historyBlock.uiId === activeViewKey
        ? historyBlock.messages
        : undefined;
    return block === undefined || block.length === 0
      ? threadMessages
      : [...block, ...threadMessages];
  }, [threadMessages, activeViewKey, historyBlock]);

  const isLiveSession = activeSession?.source === 'live';
  const isRunning = isSessionRunning(
    runningSessionId === activeViewKey,
    rowRunStatus(activeRow, activeSession)?.status,
    detachedRunSessionIds.has(activeViewKey),
  );
  const hasLiveAttachedRun =
    isLiveSession &&
    (rowRunStatus(activeRow, activeSession)?.status === 'running' ||
      detachedRunSessionIds.has(activeViewKey) ||
      attachedRunSessionIdRef.current === activeViewKey);
  const rememberDetachedRun = useCallback(() => {
    if (!isRunning || !isLiveSession) {
      return;
    }
    setDetachedRunSessionIds(current => {
      if (current.has(activeViewKey)) {
        return current;
      }
      const next = new Set(current);
      next.add(activeViewKey);
      return next;
    });
  }, [activeViewKey, isLiveSession, isRunning]);
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
  const activeSessionRef = activeSession?.sessionRef;
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
      const profile = profileOfViewKey(uiId);
      void fetchSession(session.sessionRef, {
        before: page.next_before,
        generation: page.generation,
        turns: appConfig.transcript_page_turns,
        ...(profile === undefined ? {} : {profile}),
      })
        .then(result => {
          if (result.kind !== 'ok') {
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
  const selectSessionPathProfile = useCallback(
    async (profile: string) => {
      const sessionRef = activeSession?.sessionRef;
      if (
        sessionRef === undefined ||
        activeSession.activeProfile === profile ||
        isRunning ||
        isCompactionInProgress ||
        isProfileSwitching ||
        isModelDefaultUpdating
      ) {
        return;
      }
      setIsProfileSwitching(true);
      try {
        const result = await updateSessionPathProfile(sessionRef, profile);
        setSessions(current =>
          current.map(session => {
            if (session.sessionRef !== sessionRef) {
              return session;
            }
            const next = {
              ...session,
              activeProfile: result.active_profile,
              pathProfiles: result.path_profiles,
            };
            if (result.model_default === undefined) {
              delete next.modelDefault;
            } else {
              next.modelDefault = result.model_default;
            }
            if (result.models === undefined) {
              delete next.models;
            } else {
              next.models = result.models;
            }
            return next;
          }),
        );
        snapshotEtagRef.current = undefined;
        refreshSessionsRef.current?.();
      } catch (error) {
        recordNotification({
          sessionId: activeSessionId,
          sessionTitle: activeSession?.title ?? 'Session',
          kind: 'error',
          title: 'Unable to change session profile',
          message: error instanceof Error ? error.message : String(error),
        });
      } finally {
        setIsProfileSwitching(false);
      }
    },
    [
      activeSession,
      activeSessionId,
      isCompactionInProgress,
      isModelDefaultUpdating,
      isProfileSwitching,
      isRunning,
      recordNotification,
    ],
  );
  const isActiveSessionCompacting = activeSessionRef !== undefined && compactingSessionRef === activeSessionRef;
  const unreadNotificationCount = unreadActivityNotificationCount(notifications);
  const hasUnreadError = hasUnreadActivityError(notifications);
  const streamingMessageCommitted = streamingMessage !== null && transcriptMessages.some(
    message => message.id === streamingMessage.id,
  );

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
  const detachRun = useCallback(() => {
    attachedTurnRef.current = null;
    attachedRunSessionIdRef.current = null;
    activeRunSessionIdRef.current = null;
    runAbortRef.current?.abort();
    runAbortRef.current = null;
    if (runTimeoutRef.current !== null) {
      window.clearTimeout(runTimeoutRef.current);
      runTimeoutRef.current = null;
    }
    setStreamingMessage(null);
    setRunningSessionId(null);
    setRunProgress(baseReasoning.length);
  }, []);

  /**
   * Stops the run for real. Unsubscribing only closes this client's stream —
   * the server would keep running the turn — so a stop has to be asked for.
   */
  const stopRun = useCallback(() => {
    const session = activeSessionEntryRef.current;
    detachRun();
    if (session?.source === 'live' && session.sessionRef) {
      void stopSessionRun(session.sessionRef, activeProfileRef.current).catch(error => {
        recordNotification({
          sessionId: session.id,
          sessionTitle: activeRowSession.title,
          kind: 'error',
          title: 'Unable to stop run',
          message: error instanceof Error ? error.message : String(error),
        });
        console.warn('Unable to stop the run:', error);
      });
    }
  }, [activeRowSession.title, detachRun, recordNotification]);

  const compactActiveSession = useCallback(async () => {
    const sessionRef = activeSession?.sessionRef;
    if (!isLiveSession || !sessionRef || isRunning || isCompactionInProgress) {
      return;
    }

    setCompactingSessionRef(sessionRef);
    try {
      // Compaction rewrites one profile's transcript, so it must be the profile
      // this view is showing — not the session's stored default.
      const result = await compactSession(sessionRef, activeProfileRef.current);
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
    activeRowSession.title,
    activeSessionId,
    isCompactionInProgress,
    isLiveSession,
    isRunning,
    recordNotification,
  ]);

  const submitMessage = useCallback(
    (value: string) => {
      const trimmedValue = value.trim();
      if (!trimmedValue || isRunning) {
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
      attachedRunSessionIdRef.current = null;
      activeRunSessionIdRef.current = sessionId;
      setRunningSessionId(sessionId);
      setRunProgress(isLiveRun ? 0 : 1);
      setStreamingMessage(null);

      if (isLiveRun && sessionRef) {
        const controller = new AbortController();
        runAbortRef.current = controller;
        void streamSessionRun(sessionRef, trimmedValue, {
          signal: controller.signal,
          ...(activeProfile === undefined ? {} : {profile: activeProfile}),
          onMessage: message => {
            setStreamingMessage(message);
            setRunProgress(1);
          },
          onDelta: applyRunDelta,
          onPart: applyRunPart,
          onCompleted: message => {
            setStreamingMessage(message);
            setRunProgress(2);
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
            if (controller.signal.aborted || runAbortRef.current !== controller) {
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
            if (controller.signal.aborted || runAbortRef.current !== controller) {
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
            if (runAbortRef.current !== controller) {
              return;
            }
            runAbortRef.current = null;
            activeRunSessionIdRef.current = null;
            setStreamingMessage(null);
            setRunProgress(isLiveRun ? 0 : baseReasoning.length);
            setRunningSessionId(current => (current === sessionId ? null : current));
          });
        return true;
      }

      runTimeoutRef.current = window.setTimeout(() => {
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
        activeRunSessionIdRef.current = null;
        setRunProgress(baseReasoning.length);
        setRunningSessionId(current => (current === sessionId ? null : current));
        runTimeoutRef.current = null;
      }, appConfig.mock_run_delay_ms);
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
      if (attachedTurnRef.current === turnId || runAbortRef.current !== null) {
        return;
      }
      attachedTurnRef.current = turnId;
      attachedRunSessionIdRef.current = sessionId;
      activeRunSessionIdRef.current = sessionId;
      const controller = new AbortController();
      runAbortRef.current = controller;
      setRunningSessionId(sessionId);
      setRunProgress(1);
      setStreamingMessage(null);

      void attachSessionRun(sessionRef, {
        signal: controller.signal,
        ...(profile === undefined ? {} : {profile}),
        onMessage: message => setStreamingMessage(message),
        onDelta: applyRunDelta,
        onPart: applyRunPart,
        onCompleted: message => setStreamingMessage(message),
        onUsage: usage => {
          updateViewSessionRecord(
            sessionId,
            session => ({...session, usage}),
            fallbackSession,
          );
        },
      })
        .then(result => {
          if (controller.signal.aborted || runAbortRef.current !== controller) {
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
          if (controller.signal.aborted || runAbortRef.current !== controller) {
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
          if (runAbortRef.current !== controller) {
            return;
          }
          // `attachedTurnRef` stays on this turn: the discovery effect clears it
          // once the session reports the run as finished, which also prevents a
          // re-attach loop while the run is still streaming for other clients.
          runAbortRef.current = null;
          attachedRunSessionIdRef.current = null;
          activeRunSessionIdRef.current = null;
          setStreamingMessage(null);
          setRunProgress(baseReasoning.length);
          setRunningSessionId(current => (current === sessionId ? null : current));
          // The committed transcript is authoritative now: let the poll replace
          // the locally appended copy instead of waiting for the next tick.
          refreshSessionsRef.current?.();
        });
    },
    [recordNotification, updateViewSessionRecord],
  );

  const createSession = useCallback(() => {
    rememberDetachedRun();
    detachRun();
    const sessionId = `run-${Date.now()}`;
    const newSession: Session = {
      id: sessionId,
      title: 'New agent run',
      time: 'Now',
      turns: 1,
      state: 'accent',
      source: 'sample',
    };
    setSessions(current => [newSession, ...current]);
    setThreads(current => ({
      ...current,
      [sessionId]: [
        {
          id: `${sessionId}-system`,
          sender: 'system',
          text: 'New run ready · reasoning preview enabled',
          timestamp: 'Now',
        },
      ],
    }));
    hasChosenSessionRef.current = true;
    setActiveViewKey(sessionId);
    setRunProgress(0);
  }, [detachRun, rememberDetachedRun]);

  /**
   * Moves this client to another view.
   *
   * The view left behind detaches only here: a run in flight keeps going on the
   * server and is picked up again when its row is selected once more. Selecting
   * what is already on screen is a no-op, so a click can never tear down the
   * stream a view owns.
   */
  const activateView = useCallback(
    (nextViewKey: string) => {
      hasChosenSessionRef.current = true;
      if (nextViewKey === activeViewKeyRef.current) {
        return;
      }
      rememberDetachedRun();
      detachRun();
      setActiveViewKey(nextViewKey);
      setRunProgress(baseReasoning.length);
    },
    [detachRun, rememberDetachedRun],
  );

  /** A list click: the session's own row, following its stored default profile. */
  const selectSession = useCallback(
    (sessionId: string) => {
      activateView(viewKey(sessionId));
    },
    [activateView],
  );

  /** The profile menu: pin that profile to a row of its own and open it. */
  const openProfileView = useCallback(
    (sessionRef: string, profile: string) => {
      setPinnedViews(current =>
        addPinnedView(current, {sessionRef, profile}, appConfig.session_views_max),
      );
      activateView(viewKey(sessionRef, profile));
    },
    [activateView],
  );

  /**
   * The picker inside a pinned row: show another profile in it.
   *
   * The session's stored default is untouched — that is the session's own row —
   * so the row becomes the view of the profile that was picked.
   */
  const repinRow = useCallback(
    (key: string, profile: string) => {
      const sessionRef = sessionRefOfViewKey(key);
      setPinnedViews(current =>
        addPinnedView(removePinnedView(current, key), {sessionRef, profile}, appConfig.session_views_max),
      );
      activateView(viewKey(sessionRef, profile));
    },
    [activateView],
  );

  /** The row's ✕: drop the row. Nothing on the server is touched. */
  const removePinnedRow = useCallback(
    (key: string) => {
      if (key === activeViewKeyRef.current) {
        // Fall back to the session's own row before the row disappears.
        activateView(viewKey(sessionRefOfViewKey(key)));
      }
      setPinnedViews(current => removePinnedView(current, key));
    },
    [activateView],
  );

  // The sidebar gets exact view identities so one profile's detached run marks
  // its own row. The session row can still aggregate runs from pinned profiles.
  const sidebarRunningViewKeys = useMemo(() => {
    const viewKeys = new Set(detachedRunSessionIds);
    if (runningSessionId !== null) {
      viewKeys.add(runningSessionId);
    }
    return viewKeys;
  }, [detachedRunSessionIds, runningSessionId]);

  // A row whose session disappeared server-side is dropped. Only a list that came
  // from the server can say that: pruning against the placeholders of the first
  // paint would drop the rows that were just restored, and persist that loss.
  useEffect(() => {
    if (!hasLoadedSessionsRef.current) {
      return;
    }
    const knownIds = sessions.map(session => session.id);
    setPinnedViews(current => {
      const pruned = prunePinnedViews(current, knownIds);
      return pruned.length === current.length ? current : pruned;
    });
  }, [sessions]);

  useEffect(() => {
    writeStoredViews(appConfig.session_views_storage_key, {views: pinnedViews, activeViewKey});
  }, [activeViewKey, pinnedViews]);

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
    ): Promise<HistoryBlock | null> => {
      const held = historyRef.current !== null && historyRef.current.uiId === uiId
        ? historyRef.current
        : null;
      if (reference === undefined) {
        // A page: the caller already holds the block, and it is not restated.
        return held;
      }
      if (reference.etag === null) {
        if (held !== null) {
          setHistory(null);
        }
        return null;
      }
      if (held !== null && held.etag === reference.etag) {
        return held;
      }
      const result = await fetchSessionHistory(sessionRef, held?.etag, profile);
      if (result.kind === 'ok') {
        const next = {uiId, etag: reference.etag, messages: result.value.messages};
        setHistory(next);
        return next;
      }
      if (result.kind === 'not-modified' && held !== null) {
        const next = {...held, etag: reference.etag};
        setHistory(next);
        return next;
      }
      return held;
    };

    const loadTranscript = (sessionRef: string, uiId: string, profile: string | undefined) => {
      // A client-owned run already has its optimistic turn in memory, so leave
      // it alone. A fresh page attached to a run still needs one transcript
      // snapshot; after that, the stream owns updates until the run completes.
      if (shouldSkipTranscriptLoad({
        sessionId: uiId,
        activeRunSessionId: activeRunSessionIdRef.current,
        attachedRunSessionId: attachedRunSessionIdRef.current,
        transcriptHydrated: hydratedTranscriptSessionIdsRef.current.has(uiId),
      })) {
        return;
      }
      void fetchSession(sessionRef, {
        etag: snapshotEtagRef.current,
        ...(profile === undefined ? {} : {profile}),
      })
        .then(async result => {
          if (cancelled || result.kind !== 'ok') {
            return;
          }
          snapshotEtagRef.current = result.etag;
          const snapshot = result.value;
          await ensureHistory(sessionRef, uiId, snapshot.history, profile);
          if (cancelled) {
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
          hydratedTranscriptSessionIdsRef.current.add(uiId);
          applyRowSnapshot(uiId, sessionRef, profile, snapshot);
        })
        .catch(error => {
          if (!cancelled) {
            console.warn('Unable to load the session transcript:', error);
          }
        });
    };

    const loadSessions = () => {
      void fetchSessions(sessionsEtagRef.current)
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
          setDetachedRunSessionIds(current =>
            reconcileDetachedRunSessionIds(
              current,
              liveRunKeys,
            ),
          );
          const activeRunSessionId = activeRunSessionIdRef.current;
          // Every view of these sessions that has a run: the stored default's,
          // plus one per profile the server reports.
          const liveRunKeys = liveItems.flatMap(item => [
            {id: item.id, status: item.snapshot.run?.status},
            ...Object.entries(item.snapshot.runs_by_profile ?? {}).map(([profile, run]) => ({
              id: viewKey(item.id, profile),
              status: run.status,
            })),
          ]);
          setThreads(current => {
            const next = {...current};
            let hasChange = false;
            for (const item of liveItems) {
              if (
                item.id === (activeRunSessionId === null ? null : sessionRefOfViewKey(activeRunSessionId)) ||
                sessionRefOfViewKey(view.viewKey) === item.id
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
              if (item.id === (activeRunSessionId === null ? null : sessionRefOfViewKey(activeRunSessionId))) {
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
        })
        .catch(error => {
          if (!cancelled) {
            console.warn('Unable to load configured sessions:', error);
          }
        });
    };

    refreshSessionsRef.current = loadSessions;
    loadSessions();
    // While this client streams a run, the stream already carries every commit,
    // so the transcript only needs to catch up occasionally.
    const refreshInterval = isRunning
      ? appConfig.session_refresh_interval_ms_streaming
      : appConfig.session_refresh_interval_ms;
    const refreshTimer = window.setInterval(loadSessions, refreshInterval);
    return () => {
      cancelled = true;
      refreshSessionsRef.current = null;
      window.clearInterval(refreshTimer);
    };
  }, [applyHydrated, applyRowSnapshot, isRunning, setHistory]);

  // The active session carries the run the server is executing, so a page that
  // reloads mid-run attaches as soon as the session list has settled instead of
  // waiting for the next poll.
  useEffect(() => {
    const run = rowRunStatus(activeRow, activeSession);
    if (run === undefined || run.status !== 'running') {
      if (attachedTurnRef.current !== null && runAbortRef.current === null) {
        attachedTurnRef.current = null;
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
  }, [activeProfile, activeRowSession, activeSession, activeViewKey, attachToRun]);

  useEffect(() => {
    return () => {
      runAbortRef.current?.abort();
      if (runTimeoutRef.current !== null) {
        window.clearTimeout(runTimeoutRef.current);
      }
    };
  }, []);

  if (!activeSession) {
    return null;
  }

  const composer = (
    <ComposerPane
      activeSessionId={activeSessionId}
      draftKey={activeViewKey}
      onSubmit={submitMessage}
      onStop={stopRun}
      isRunning={isRunning}
      placeholder="Ask the agent to inspect a task…"
      showInspector={rightPanel === 'inspector'}
      onToggleInspector={toggleInspector}
      isLeftPanelOpen={leftPanel !== null}
      leftPanelLabel={LEFT_PANELS[leftPanel ?? lastLeftPanel].label}
      onToggleLeftPanel={toggleLeftPanel}
      onRegisterInsert={registerComposerInsert}
      drawer={
        isUsageDrawerCollapsed ? null : (
          <UsageDetailsDrawer
            sessionName={activeRowSession.title}
            usage={activeRowSession.usage}
          />
        )
      }
      toolbarActions={
        <HStack align="center" gap={1}>
          {activeSession.sessionRef &&
          activeRowSession.pathProfiles &&
          activeRowSession.pathProfiles.length > 0 ? (
            <SessionProfilePicker
              profiles={activeRowSession.pathProfiles}
              activeProfile={activeRowSession.activeProfile}
              isDisabled={isRunning || isCompactionInProgress || isProfileSwitching || isModelDefaultUpdating}
              onSelect={profile => {
                // The choice lands on the row on screen: a pinned row shows the
                // profile, the session's own row follows it. Picking the profile
                // the session already follows from a pinned row goes to that row
                // instead of leaving the same conversation in the list twice.
                const view = {...(isPinnedView ? {profile: activeProfile} : {})};
                const target = {
                  ...(activeSession.activeProfile === undefined
                    ? {}
                    : {activeProfile: activeSession.activeProfile}),
                };
                if (profileSelection(view, profile, target) === 'repin') {
                  repinRow(activeViewKey, profile);
                  return;
                }
                if (isPinnedView) {
                  removePinnedRow(activeViewKey);
                  return;
                }
                void selectSessionPathProfile(profile);
              }}
            />
          ) : null}
          {activeRowSession.models && activeRowSession.models.length > 0 ? (
            <ModelPicker
              models={activeRowSession.models}
              selectedModel={selectedModel}
              isDisabled={
                activeSession.sessionRef === undefined ||
                isRunning ||
                isCompactionInProgress ||
                isProfileSwitching ||
                isModelDefaultUpdating
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
            onClick={() => setIsUsageDrawerCollapsed(collapsed => !collapsed)}
          />
          {isLiveSession && activeSession.sessionRef && (
            <IconButton
              label={isActiveSessionCompacting ? 'Compacting conversation history' : 'Compact conversation history'}
              icon={<Icon icon="arrowsUpDown" size="sm" />}
              variant="ghost"
              size="sm"
              isDisabled={isRunning || isCompactionInProgress}
              onClick={() => void compactActiveSession()}
            />
          )}
        </HStack>
      }
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
          rows={listRows}
          rowTurns={rowTurns}
          sessions={sessions}
          runningViewKeys={sidebarRunningViewKeys}
          activeViewKey={activeViewKey}
          onSelect={activateView}
          onOpenProfile={openProfileView}
          onRemoveView={removePinnedRow}
          canPinProfile={!profileRowLimitReached}
          onNewSession={createSession}
          openLeftPanel={leftPanel}
          onSelectLeftPanel={selectLeftPanel}
          rightPanel={rightPanel}
          unreadCount={unreadNotificationCount}
          hasUnreadError={hasUnreadError}
          onToggleNotifications={toggleNotifications}
        />
      }
    >
      <VStack xstyle={styles.shell}>
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
                  page's top-level heading for assistive technology. */}
              <VisuallyHidden as="div">
                <Heading level={1}>{activeRowSession.title}</Heading>
              </VisuallyHidden>
              <ChatLayout
                density="balanced"
                xstyle={styles.chat}
                composer={centeredComposer}
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
                    <ChatMessageList density="balanced" align="top" isStreaming={isRunning}>
                      {(hiddenCount > 0 || hasOlderTurns) && (
                        <EarlierMessagesControl
                          hiddenCount={hiddenCount}
                          hasMore={hasOlderTurns}
                          onReveal={revealEarlierMessages}
                        />
                      )}
                      {visibleMessages.map(message => (
                        <TranscriptRow
                          key={message.id}
                          message={message}
                          model={activeRowSession.model}
                          onLoadFull={loadFullMessage}
                        />
                      ))}
                      {isRunning &&
                        (!isLiveSession || streamingMessage !== null || hasLiveAttachedRun) &&
                        !streamingMessageCommitted && (
                        <ChatMessage
                          sender="assistant"
                          avatar={<AgentAvatar isRunning />}
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
                                isStreaming
                              />
                            ) : (
                              <VStack gap={3} width="100%">
                                <ReasoningBlock
                                  steps={reasoningSteps}
                                  rowWidthPx={liveBubbleWidthPx}
                                  isFlush
                                  isStreaming
                                />
                                <MarkdownText
                                  density="compact"
                                  contentWidth="100%"
                                  isStreaming
                                  headingLevelStart={3}
                                >
                                  Astra is working through the request…
                                </MarkdownText>
                              </VStack>
                            )}
                          </ChatMessageBubble>
                        </ChatMessage>
                      )}
                    </ChatMessageList>
                  </VStack>
                </HStack>
              </ChatLayout>
            </LayoutContent>
          }
        />
        {leftPanel === 'prompts' && (
          <PromptLibraryPanel
            region={leftPanelRegion}
            onClose={() => setLeftPanel(null)}
            onUsePrompt={usePrompt}
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
      </VStack>
    </AppShell>
  );
}

export default App;
