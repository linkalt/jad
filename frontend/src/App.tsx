import {memo, useCallback, useEffect, useMemo, useRef, useState} from 'react';
import type {ChangeEvent, CSSProperties} from 'react';
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
import {
  attachSessionRun,
  compactSession,
  fetchSessions,
  stopSessionRun,
  streamSessionRun,
  type SessionSnapshot,
  type WebUiMessage,
  type WebUiPart,
} from './api';
import appConfig from './config.json';
import {deepEqual} from './deepEqual';
import {ComposerPane} from './components/ComposerPane';
import {FloatingPanel} from './components/FloatingPanel';
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

type SessionState = 'success' | 'warning' | 'accent';
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
  model?: {
    provider?: string;
    model?: string;
  };
  usage?: SessionSnapshot['usage'];
  /** The run the server is executing for this session, when there is one. */
  run?: SessionSnapshot['run'];
}

interface CompactionFeedback {
  sessionRef: string;
  kind: 'success' | 'noop' | 'error';
  message: string;
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

const LIVE_SESSION_ID = appConfig.live_session_id;

// Off-screen transcript rows are skipped by the renderer. Without this, every
// keystroke in the composer costs a full layout of the scroll container's
// contents (the composer dock is sticky inside the same scroller), which made
// typing slow in long sessions — measured ~42ms of synchronous work per
// keystroke against a 36k-pixel scroll height, versus ~10ms with this enabled.
// The intrinsic size is only an estimate used until a row has been measured.
const DEFERRED_ROW_STYLE: CSSProperties | undefined =
  appConfig.defer_offscreen_transcript_rows
    ? {
        contentVisibility: 'auto',
        containIntrinsicSize: `auto ${appConfig.transcript_row_estimate_px}px`,
      }
    : undefined;

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
    state: 'success',
    source: 'live',
    sessionRef: snapshot.id,
    ...(snapshot.root_label ? {rootLabel: snapshot.root_label} : {}),
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
          </VStack>
        )}
      </VStack>
    ),
  };
}

function OrphanToolResultBlock({result}: {result: WebUiToolResult}) {
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

type WebUiReasoningPart = {type: 'reasoning'; text: string};
type WebUiContentPart = {type: 'text'; text: string};
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

function groupWebUiParts(parts: WebUiPart[]): WebUiPartGroup[] {
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
}: {
  group: WebUiActivityGroupPart;
  rowWidthPx: number;
  isStreaming: boolean;
}) {
  if (group.type === 'reasoning') {
    return (
      <ReasoningBlock
        steps={[]}
        content={group.item.part.text}
        rowWidthPx={rowWidthPx}
        isFlush
        isStreaming={isStreaming}
      />
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
      return toolCallItem(part, result, rowWidthPx);
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
          />
        );
      })}
    </VStack>
  );
}

function WebUiParts({
  parts,
  rowWidthPx,
  isStreaming = false,
}: {
  parts: WebUiPart[];
  rowWidthPx: number;
  isStreaming?: boolean;
}) {
  const segments = segmentWebUiGroups(groupWebUiParts(parts));
  const lastPartIndex = parts.length - 1;
  const [revealedToolGroups, setRevealedToolGroups] = useState(0);

  /**
   * Tool steps are the heavy rows of a long turn — every call carries its own
   * title and output. The window keeps the newest ones mounted and hides the
   * rest behind a control, the same trade the transcript window makes one level
   * up. Reasoning and text are never hidden, so the turn still reads in order.
   */
  const toolGroupKeys: string[] = [];
  segments.forEach(segment => {
    if (segment.type !== 'activity') {
      return;
    }
    segment.items.forEach(({group, index}) => {
      if (group.type === 'tools') {
        toolGroupKeys.push(webUiGroupKey(group, index));
      }
    });
  });
  const visibleToolGroups = appConfig.message_tool_window + revealedToolGroups;
  const hiddenToolGroupKeys = new Set(
    toolGroupKeys.slice(0, Math.max(0, toolGroupKeys.length - visibleToolGroups)),
  );

  return (
    <VStack gap={3} width="100%">
      {hiddenToolGroupKeys.size > 0 ? (
        <VStack align="center" gap={1} width="100%">
          <Button
            label="Load earlier steps"
            variant="ghost"
            size="sm"
            icon={<Icon icon="arrowUp" size="sm" />}
            onClick={() =>
              setRevealedToolGroups(current => current + appConfig.message_tool_window_page)
            }
          />
          <Text type="supporting" color="secondary">
            {hiddenToolGroupKeys.size} earlier{' '}
            {hiddenToolGroupKeys.size === 1 ? 'step is' : 'steps are'} not shown
          </Text>
        </VStack>
      ) : null}
      {segments.map((segment, segmentIndex) => {
        if (segment.type === 'content') {
          return (
            <MarkdownText
              key={webUiGroupKey(segment.item.group, segment.item.index)}
              density="compact"
              contentWidth="100%"
              isStreaming={
                isStreaming && segment.item.group.item.index === lastPartIndex
              }
              headingLevelStart={3}
            >
              {segment.item.group.item.part.text}
            </MarkdownText>
          );
        }

        return (
          <VStack key={`activity-${segmentIndex}`} gap={0} width="100%">
            {segment.items.map(({group, index}) => {
              const groupKey = webUiGroupKey(group, index);
              if (hiddenToolGroupKeys.has(groupKey)) {
                return null;
              }
              return (
                <WebUiActivityGroup
                  key={groupKey}
                  group={group}
                  rowWidthPx={rowWidthPx}
                  isStreaming={
                    isStreaming && webUiGroupIndex(group) === lastPartIndex
                  }
                />
              );
            })}
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

function SessionNavigation({
  sessions,
  activeSessionId,
  onSelect,
  onNewSession,
  openLeftPanel,
  onSelectLeftPanel,
}: {
  sessions: Session[];
  activeSessionId: string;
  onSelect: (sessionId: string) => void;
  onNewSession: () => void;
  openLeftPanel: LeftPanelId | null;
  onSelectLeftPanel: (panel: LeftPanelId) => void;
}) {
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
        {sessions.map(session => (
          <SideNavItem
            key={session.id}
            label={session.title}
            isSelected={session.id === activeSessionId}
            onClick={() => onSelect(session.id)}
            icon={
              <StatusDot
                variant={session.state}
                label={`${session.title} ${session.state}`}
              />
            }
            endContent={<Badge label={String(session.turns)} />}
          />
        ))}
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
}: {
  message: MessageRecord;
  model?: Session['model'];
}) {
  const [rowRef, rowWidthPx] = useRowWidth();
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
      style={DEFERRED_ROW_STYLE}
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
            <WebUiParts parts={message.parts} rowWidthPx={rowWidthPx} />
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
  }: {
    message: MessageRecord;
    model: Session['model'] | undefined;
  }) {
    if (message.sender === 'system') {
      return (
        <ChatSystemMessage
          variant="divider"
          icon={<Icon icon="check" color="success" size="sm" />}
          style={DEFERRED_ROW_STYLE}
        >
          {message.text}
        </ChatSystemMessage>
      );
    }

    if (message.sender === 'user') {
      return (
        <ChatMessage sender="user" avatar={<Avatar name="You" size="sm" />} style={DEFERRED_ROW_STYLE}>
          <ChatMessageBubble
            metadata={<ChatMessageMetadata timestamp={message.timestamp} status="read" />}
          >
            <MarkdownText density="compact" contentWidth="100%">{message.text}</MarkdownText>
          </ChatMessageBubble>
        </ChatMessage>
      );
    }

    return <AssistantMessage message={message} model={model} />;
  },
  // The poll and the session list rebuild messages and models as fresh objects,
  // so fall back to a structural compare when the identities differ.
  (previous, next) =>
    (previous.message === next.message || deepEqual(previous.message, next.message)) &&
    (previous.model === next.model || deepEqual(previous.model, next.model)),
);

function App() {
  const [sessions, setSessions] = useState<Session[]>(initialSessions);
  // The configured live session is a preference, not a row: the sidebar lists
  // whatever the backend reports, and this id is only used to choose which of
  // those live sessions to open on.
  const [activeSessionId, setActiveSessionId] = useState(LIVE_SESSION_ID);
  const hasChosenSessionRef = useRef(false);
  const [threads, setThreads] = useState<Record<string, MessageRecord[]>>(initialThreads);
  const [showInspector, setShowInspector] = useState(appConfig.show_inspector_by_default);
  // One left slot, several datasets: the panel shows whichever workspace view
  // was picked last, so two panels never stack on the same edge. The closed
  // state remembers that choice, because the composer toggle reopens it.
  const [leftPanel, setLeftPanel] = useState<LeftPanelId | null>(
    appConfig.show_prompt_library_by_default ? 'prompts' : null,
  );
  const [lastLeftPanel, setLastLeftPanel] = useState<LeftPanelId>('prompts');
  const [runningSessionId, setRunningSessionId] = useState<string | null>(null);
  const [compactingSessionRef, setCompactingSessionRef] = useState<string | null>(null);
  const [compactionFeedback, setCompactionFeedback] = useState<CompactionFeedback | null>(null);
  const [isUsageDrawerCollapsed, setIsUsageDrawerCollapsed] = useState(true);
  const [runProgress, setRunProgress] = useState(baseReasoning.length);
  const [streamingMessage, setStreamingMessage] = useState<WebUiMessage | null>(null);
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
  const insertIntoComposerRef = useRef<((text: string) => void) | null>(null);
  const registerComposerInsert = useCallback(
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
  // The run this client is streaming, if any. It is how a page reload
  // re-attaches to a run it did not start.
  const attachedTurnRef = useRef<string | null>(null);
  const activeSessionIdRef = useRef<string>(activeSessionId);
  const activeSessionEntryRef = useRef<Session | undefined>(undefined);
  const refreshSessionsRef = useRef<(() => void) | null>(null);

  // Live sessions come from the backend, so the active id can outlive its row —
  // on the first paint (before the first poll answers) and after a session is
  // deleted server-side. Reconcile instead of rendering a phantom: the
  // configured live session when the backend has it, otherwise the first live
  // session, otherwise whatever the list starts with. A session the user picked
  // is never taken away from them.
  useEffect(() => {
    if (sessions.length === 0) {
      return;
    }
    const preferredLive =
      sessions.find(session => session.id === LIVE_SESSION_ID) ??
      sessions.find(session => session.source === 'live');
    if (!sessions.some(session => session.id === activeSessionId)) {
      setActiveSessionId((preferredLive ?? sessions[0]).id);
      hasChosenSessionRef.current = false;
      return;
    }
    if (
      !hasChosenSessionRef.current &&
      preferredLive !== undefined &&
      activeSessionId !== preferredLive.id
    ) {
      setActiveSessionId(preferredLive.id);
    }
  }, [activeSessionId, sessions]);

  const activeSession =
    sessions.find(session => session.id === activeSessionId) ?? sessions[0];
  const messages = threads[activeSessionId] ?? [];
  const {visibleMessages, hiddenCount, revealEarlier} = useTranscriptWindow({
    messages,
    sessionId: activeSessionId,
    initialCount: appConfig.transcript_window_messages,
    pageSize: appConfig.transcript_window_page_messages,
  });
  const isRunning = runningSessionId === activeSessionId;
  const isLiveSession = activeSession?.source === 'live';
  const activeSessionRef = activeSession?.sessionRef;
  const isCompactionInProgress = compactingSessionRef !== null;
  const isActiveSessionCompacting = activeSessionRef !== undefined && compactingSessionRef === activeSessionRef;
  const activeCompactionFeedback =
    activeSessionRef !== undefined && compactionFeedback?.sessionRef === activeSessionRef
      ? compactionFeedback
      : null;
  const streamingMessageCommitted = streamingMessage !== null && messages.some(
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
      void stopSessionRun(session.sessionRef).catch(error => {
        console.warn('Unable to stop the run:', error);
      });
    }
  }, [detachRun]);

  const compactActiveSession = useCallback(async () => {
    const sessionRef = activeSession?.sessionRef;
    if (!isLiveSession || !sessionRef || isRunning || isCompactionInProgress) {
      return;
    }

    setCompactingSessionRef(sessionRef);
    setCompactionFeedback(null);
    try {
      const result = await compactSession(sessionRef);
      setCompactionFeedback({
        sessionRef,
        kind: result.compacted ? 'success' : 'noop',
        message: result.compacted
          ? `History compacted: retained ${result.retained_items} of ${result.compacted_items} items.`
          : `No compaction needed; ${result.retained_items} items retained.`,
      });
    } catch (error) {
      setCompactionFeedback({
        sessionRef,
        kind: 'error',
        message: error instanceof Error ? error.message : String(error),
      });
    } finally {
      setCompactingSessionRef(current => current === sessionRef ? null : current);
    }
  }, [activeSession?.sessionRef, isCompactionInProgress, isLiveSession, isRunning]);

  const submitMessage = useCallback(
    (value: string) => {
      const trimmedValue = value.trim();
      if (!trimmedValue || isRunning) {
        return false;
      }

      const sessionId = activeSessionId;
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
      setSessions(current =>
        current.map(session =>
          session.id === sessionId
            ? {...session, turns: session.turns + 1, state: 'accent'}
            : session,
        ),
      );
      activeRunSessionIdRef.current = sessionId;
      setRunningSessionId(sessionId);
      setRunProgress(isLiveRun ? 0 : 1);
      setStreamingMessage(null);

      if (isLiveRun && sessionRef) {
        const controller = new AbortController();
        runAbortRef.current = controller;
        void streamSessionRun(sessionRef, trimmedValue, {
          signal: controller.signal,
          onMessage: message => {
            setStreamingMessage(message);
            setRunProgress(1);
          },
          onCompleted: message => {
            setStreamingMessage(message);
            setRunProgress(2);
          },
          onUsage: usage => {
            setSessions(current => current.map(session =>
              session.id === sessionId ? {...session, usage} : session,
            ));
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
            setSessions(current =>
              current.map(session =>
                session.id === sessionId ? {...session, state: 'success'} : session,
              ),
            );
          })
          .catch(error => {
            if (controller.signal.aborted || runAbortRef.current !== controller) {
              return;
            }
            const message = error instanceof Error ? error.message : String(error);
            setThreads(current => ({
              ...current,
              [sessionId]: [
                ...(current[sessionId] ?? []),
                {
                  id: `${sessionId}-error-${Date.now()}`,
                  sender: 'assistant',
                  text: `The run failed: ${message}`,
                  timestamp: messageTime(),
                },
              ],
            }));
            setSessions(current =>
              current.map(session =>
                session.id === sessionId ? {...session, state: 'warning'} : session,
              ),
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
        setSessions(current =>
          current.map(session =>
            session.id === sessionId
              ? {...session, state: 'success'}
              : session,
          ),
        );
        activeRunSessionIdRef.current = null;
        setRunProgress(baseReasoning.length);
        setRunningSessionId(current => (current === sessionId ? null : current));
        runTimeoutRef.current = null;
      }, appConfig.mock_run_delay_ms);
      return true;
    },
    [activeSession, activeSessionId, isRunning],
  );

  /**
   * Re-attaches to a run that is already in flight on the server — the case
   * after a reload, where only the server still knows about it. The stream
   * replays the current snapshot of the message, then continues live.
   */
  const attachToRun = useCallback(
    (sessionId: string, sessionRef: string, turnId: string) => {
      if (attachedTurnRef.current === turnId || runAbortRef.current !== null) {
        return;
      }
      attachedTurnRef.current = turnId;
      activeRunSessionIdRef.current = sessionId;
      const controller = new AbortController();
      runAbortRef.current = controller;
      setRunningSessionId(sessionId);
      setRunProgress(1);
      setStreamingMessage(null);

      void attachSessionRun(sessionRef, {
        signal: controller.signal,
        onMessage: message => setStreamingMessage(message),
        onCompleted: message => setStreamingMessage(message),
        onUsage: usage => {
          setSessions(current =>
            current.map(session =>
              session.id === sessionId ? {...session, usage} : session,
            ),
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
          setSessions(current =>
            current.map(session =>
              session.id === sessionId ? {...session, state: 'success'} : session,
            ),
          );
        })
        .catch(error => {
          if (controller.signal.aborted || runAbortRef.current !== controller) {
            return;
          }
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
          activeRunSessionIdRef.current = null;
          setStreamingMessage(null);
          setRunProgress(baseReasoning.length);
          setRunningSessionId(current => (current === sessionId ? null : current));
          // The committed transcript is authoritative now: let the poll replace
          // the locally appended copy instead of waiting for the next tick.
          refreshSessionsRef.current?.();
        });
    },
    [],
  );

  const createSession = useCallback(() => {
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
    setActiveSessionId(sessionId);
    setRunProgress(0);
  }, [detachRun]);

  const selectSession = useCallback(
    (sessionId: string) => {
      // Only this client detaches: a run in flight keeps going on the server and
      // is picked up again when the session is selected once more.
      detachRun();
      hasChosenSessionRef.current = true;
      setActiveSessionId(sessionId);
      setRunProgress(baseReasoning.length);
    },
    [detachRun],
  );

  useEffect(() => {
    activeSessionIdRef.current = activeSessionId;
    // Switching to a session that has a run in flight must attach at once: the
    // transcript of that session is stale until the fresh snapshot arrives.
    refreshSessionsRef.current?.();
  }, [activeSessionId]);

  useEffect(() => {
    activeSessionEntryRef.current = activeSession;
  }, [activeSession]);

  useEffect(() => {
    let cancelled = false;
    const loadSessions = () => {
      void fetchSessions()
        .then(snapshots => {
          if (cancelled) {
            return;
          }
          const liveItems = snapshots.map(snapshot => ({
            snapshot,
            id: sessionUiId(snapshot, initialSessions),
          }));
          const activeRunSessionId = activeRunSessionIdRef.current;
          setThreads(current => {
            const next = {...current};
            let hasChange = false;
            for (const item of liveItems) {
              if (item.id === activeRunSessionId) {
                continue;
              }
              const previous = current[item.id];
              // Reuse the existing array when the payload is structurally
              // identical: a new identity re-renders every row of that thread.
              if (previous !== undefined && deepEqual(previous, item.snapshot.messages)) {
                continue;
              }
              next[item.id] = item.snapshot.messages;
              hasChange = true;
            }
            return hasChange ? next : current;
          });
          setSessions(current => {
            const liveIds = new Set(liveItems.map(item => item.id));
            const liveSessions = liveItems.map(item => {
              if (item.id === activeRunSessionId) {
                const existing = current.find(session => session.id === item.id);
                if (existing) {
                  return existing;
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
    const refreshTimer = window.setInterval(loadSessions, appConfig.session_refresh_interval_ms);
    return () => {
      cancelled = true;
      refreshSessionsRef.current = null;
      window.clearInterval(refreshTimer);
    };
  }, [isRunning]);

  // The active session carries the run the server is executing, so a page that
  // reloads mid-run attaches as soon as the session list has settled instead of
  // waiting for the next poll.
  useEffect(() => {
    const run = activeSession?.run;
    if (run === undefined || run.status !== 'running') {
      if (attachedTurnRef.current !== null && runAbortRef.current === null) {
        attachedTurnRef.current = null;
      }
      return;
    }
    if (activeSession.source !== 'live' || activeSession.sessionRef === undefined) {
      return;
    }
    attachToRun(activeSessionId, activeSession.sessionRef, run.turn_id);
  }, [activeSession, activeSessionId, attachToRun]);

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
      onSubmit={submitMessage}
      onStop={stopRun}
      isRunning={isRunning}
      placeholder="Ask the agent to inspect a task…"
      showInspector={showInspector}
      onToggleInspector={() => setShowInspector(current => !current)}
      isLeftPanelOpen={leftPanel !== null}
      leftPanelLabel={LEFT_PANELS[leftPanel ?? lastLeftPanel].label}
      onToggleLeftPanel={toggleLeftPanel}
      onRegisterInsert={registerComposerInsert}
      drawer={
        isUsageDrawerCollapsed ? null : (
          <UsageDetailsDrawer
            sessionName={activeSession.title}
            usage={activeSession.usage}
          />
        )
      }
      toolbarActions={
        <HStack align="center" gap={1}>
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
          {activeCompactionFeedback && activeCompactionFeedback.kind !== 'error' && (
            <Text type="supporting" color="secondary" role="status" aria-live="polite">
              {activeCompactionFeedback.message}
            </Text>
          )}
        </HStack>
      }
      status={
        activeCompactionFeedback?.kind === 'error'
          ? {type: 'error', message: `Compaction failed: ${activeCompactionFeedback.message}`}
          : undefined
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
          sessions={sessions}
          activeSessionId={activeSessionId}
          onSelect={selectSession}
          onNewSession={createSession}
          openLeftPanel={leftPanel}
          onSelectLeftPanel={selectLeftPanel}
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
                <Heading level={1}>{activeSession.title}</Heading>
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
                      {hiddenCount > 0 && (
                        <EarlierMessagesControl
                          hiddenCount={hiddenCount}
                          onReveal={revealEarlier}
                        />
                      )}
                      {visibleMessages.map(message => (
                        <TranscriptRow
                          key={message.id}
                          message={message}
                          model={activeSession.model}
                        />
                      ))}
                      {isRunning && (!isLiveSession || streamingMessage) && !streamingMessageCommitted && (
                        <ChatMessage sender="assistant" avatar={<AgentAvatar isRunning />} style={DEFERRED_ROW_STYLE}>
                          <ChatMessageBubble
                            ref={liveBubbleRef}
                            variant="ghost"
                            width="100%"
                            xstyle={styles.assistantBubble}
                            name={<AssistantIdentity model={activeSession.model} />}
                          >
                            {isLiveSession && streamingMessage ? (
                              <WebUiParts
                                parts={streamingMessage.parts}
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
            sessionTitle={activeSession.title}
            sessionTurns={activeSession.turns}
            isLiveSession={isLiveSession}
          />
        )}
        {showInspector && (
          <RunInspector
            activeSession={activeSession}
            steps={reasoningSteps}
            tools={tools}
            liveToolParts={liveToolParts}
            inspector={inspector}
            isRunning={isRunning}
            onClose={() => setShowInspector(false)}
          />
        )}
      </VStack>
    </AppShell>
  );
}

export default App;
