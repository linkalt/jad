import {useCallback, useEffect, useMemo, useRef, useState} from 'react';
import * as stylex from '@stylexjs/stylex';
import {
  AppShell,
  Avatar,
  Badge,
  Button,
  Card,
  ChatComposer,
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
  LayoutHeader,
  Markdown,
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
import {fetchSessions, streamSessionRun, type SessionSnapshot, type WebUiMessage, type WebUiPart} from './api';
import appConfig from './config.json';
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
  header: {
    backgroundColor: colorVars['--color-background-surface'],
  },
  headerTitle: {
    minWidth: 0,
  },
  singleLine: {
    display: 'block',
    minWidth: 0,
    maxWidth: '100%',
    overflow: 'hidden',
    textOverflow: 'ellipsis',
    whiteSpace: 'nowrap',
  },
  headerActions: {
    flexShrink: 0,
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
  },
  composerResizeHandle: {
    alignSelf: 'stretch',
    height: 'auto',
  },
  floatingWindow: {
    position: 'absolute',
    insetBlockStart: 0,
    insetBlockEnd: 0,
    insetInlineEnd: 0,
    zIndex: 3,
  },
  floatingCard: {
    flex: 1,
    minWidth: 0,
    height: '100%',
    overflow: 'hidden',
  },
  inspectorStack: {
    height: '100%',
    minHeight: 0,
    minWidth: 0,
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

const initialSessions: Session[] = [
  {
    id: LIVE_SESSION_ID,
    title: 'Session 1',
    time: 'Ready',
    turns: 0,
    state: 'success',
    source: 'live',
    sessionRef: LIVE_SESSION_ID,
  },
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
  [LIVE_SESSION_ID]: [],
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
          <Markdown
            density="compact"
            contentWidth="100%"
            xstyle={styles.reasoningContent}
          >
            {content}
          </Markdown>
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
  document.body.appendChild(probe);
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
  if (!result || result.status === 'unknown') {
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
  return {
    key: `tool-${call.callId}`,
    name: call.name,
    status: toolCallStatus(result),
    target: toolTarget(call.arguments, toolTitleBudgetPx(rowWidthPx, call.name)),
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

  return (
    <VStack gap={3} width="100%">
      {segments.map((segment, segmentIndex) => {
        if (segment.type === 'content') {
          return (
            <Markdown
              key={webUiGroupKey(segment.item.group, segment.item.index)}
              density="compact"
              contentWidth="100%"
              isStreaming={
                isStreaming && segment.item.group.item.index === lastPartIndex
              }
              headingLevelStart={3}
            >
              {segment.item.group.item.part.text}
            </Markdown>
          );
        }

        return (
          <VStack key={`activity-${segmentIndex}`} gap={0} width="100%">
            {segment.items.map(({group, index}) => (
              <WebUiActivityGroup
                key={webUiGroupKey(group, index)}
                group={group}
                rowWidthPx={rowWidthPx}
                isStreaming={
                  isStreaming && webUiGroupIndex(group) === lastPartIndex
                }
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

function SessionNavigation({
  sessions,
  activeSessionId,
  onSelect,
  onNewSession,
}: {
  sessions: Session[];
  activeSessionId: string;
  onSelect: (sessionId: string) => void;
  onNewSession: () => void;
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
            Session 1 uses the backend stream; seeded runs remain previews.
          </Text>
        </VStack>
      }
      footerIcons={
        <HStack align="center" justify="between" gap={2} width="100%">
          <IconButton
            label="Search sessions"
            icon={<Icon icon="search" size="sm" />}
            variant="ghost"
            size="sm"
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
        <SideNavItem label="Prompt library" icon={<Icon icon="copy" size="sm" />} />
        <SideNavItem label="Run settings" icon={<Icon icon="chevronDown" size="sm" />} />
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
    <HStack
      as="aside"
      align="stretch"
      gap={0}
      width={inspector.size}
      height="100%"
      maxWidth="100%"
      role="complementary"
      aria-label="Run inspector"
      xstyle={styles.floatingWindow}
    >
      <ResizeHandle
        direction="horizontal"
        isReversed
        hasDivider
        isAlwaysVisible
        label="Resize run inspector"
        resizable={inspector.props}
      />
      <Card variant="default" elevation="med" padding={0} xstyle={styles.floatingCard}>
        <VStack gap={5} padding={4} isScrollable xstyle={styles.inspectorStack}>
        <HStack align="start" justify="between" gap={3}>
          <VStack gap={0.5} xstyle={styles.sectionHeading}>
            <Text type="label" weight="semibold">
              Run inspector
            </Text>
            <Text type="supporting" color="secondary" xstyle={styles.singleLine}>
              {activeSession.title}
            </Text>
          </VStack>
          <IconButton
            label="Hide run inspector"
            icon={<Icon icon="close" size="sm" />}
            variant="ghost"
            size="sm"
            onClick={onClose}
          />
        </HStack>

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
        </VStack>
      </Card>
    </HStack>
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
              <HStack align="center" gap={2}>
                <Text type="supporting" color="secondary">agent response</Text>
                <Button
                  label={copyActionLabel(copyState)}
                  icon={<Icon icon={copyState === 'copied' ? 'check' : copyState === 'error' ? 'error' : 'copy'} size="sm" />}
                  variant="ghost"
                  size="sm"
                  isDisabled={markdown.length === 0}
                  onClick={() => { void copyAsMarkdown(); }}
                />
              </HStack>
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
              <Markdown density="compact" contentWidth="100%" headingLevelStart={3}>
                {message.text}
              </Markdown>
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

function renderMessage(
  message: MessageRecord,
  model: Session['model'] | undefined,
) {
  if (message.sender === 'system') {
    return (
      <ChatSystemMessage
        key={message.id}
        variant="divider"
        icon={<Icon icon="check" color="success" size="sm" />}
      >
        {message.text}
      </ChatSystemMessage>
    );
  }

  if (message.sender === 'user') {
    return (
      <ChatMessage
        key={message.id}
        sender="user"
        avatar={<Avatar name="You" size="sm" />}
      >
        <ChatMessageBubble
          metadata={<ChatMessageMetadata timestamp={message.timestamp} status="read" />}
        >
          <Markdown density="compact" contentWidth="100%">{message.text}</Markdown>
        </ChatMessageBubble>
      </ChatMessage>
    );
  }

  return (
    <AssistantMessage key={message.id} message={message} model={model} />
  );
}

function App() {
  const [sessions, setSessions] = useState<Session[]>(initialSessions);
  const [activeSessionId, setActiveSessionId] = useState(LIVE_SESSION_ID);
  const [threads, setThreads] = useState<Record<string, MessageRecord[]>>(initialThreads);
  const [composerValue, setComposerValue] = useState('');
  const [showInspector, setShowInspector] = useState(appConfig.show_inspector_by_default);
  const [runningSessionId, setRunningSessionId] = useState<string | null>(null);
  const [runProgress, setRunProgress] = useState(baseReasoning.length);
  const [streamingMessage, setStreamingMessage] = useState<WebUiMessage | null>(null);
  const conversation = useResizable({
    defaultSize: pixel(800),
    minSize: pixel(520),
    maxSize: pixel(1200),
    direction: 'horizontal',
    autoSaveId: 'agent-harness-conversation-width',
  });
  // The in-flight assistant bubble is rendered here rather than through
  // AssistantMessage, so it measures its own row width for title fitting.
  const [liveBubbleRef, liveBubbleWidthPx] = useRowWidth();
  const inspector = useResizable({
    defaultSize: pixel(360),
    minSize: pixel(280),
    maxSize: pixel(640),
    direction: 'horizontal',
    autoSaveId: 'agent-harness-run-inspector',
  });
  const runAbortRef = useRef<AbortController | null>(null);
  const runTimeoutRef = useRef<number | null>(null);
  const activeRunSessionIdRef = useRef<string | null>(null);

  const activeSession =
    sessions.find(session => session.id === activeSessionId) ?? sessions[0];
  const messages = threads[activeSessionId] ?? [];
  const isRunning = runningSessionId === activeSessionId;
  const isLiveSession = activeSession?.source === 'live';
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

  const stopRun = useCallback(() => {
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

  const submitMessage = useCallback(
    (value: string) => {
      const trimmedValue = value.trim();
      if (!trimmedValue || isRunning) {
        return;
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
      setComposerValue('');
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
        return;
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
    },
    [activeSession, activeSessionId, isRunning],
  );

  const createSession = useCallback(() => {
    stopRun();
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
    setActiveSessionId(sessionId);
    setRunProgress(0);
    setComposerValue('');
  }, [stopRun]);

  const selectSession = useCallback(
    (sessionId: string) => {
      stopRun();
      setActiveSessionId(sessionId);
      setRunProgress(baseReasoning.length);
    },
    [stopRun],
  );

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
            for (const item of liveItems) {
              if (item.id === activeRunSessionId) {
                continue;
              }
              next[item.id] = item.snapshot.messages;
            }
            return next;
          });
          setSessions(current => {
            const liveIds = new Set(liveItems.map(item => item.id));
            const initialLiveIds = new Set(
              initialSessions
                .filter(session => session.source === 'live')
                .map(session => session.id),
            );
            const liveSessions = liveItems.map(item => {
              if (item.id === activeRunSessionId) {
                const existing = current.find(session => session.id === item.id);
                if (existing) {
                  return existing;
                }
              }
              return sessionFromSnapshot(item.snapshot, item.id);
            });
            const existingLive = current.filter(
              session =>
                session.source === 'live' &&
                initialLiveIds.has(session.id) &&
                !liveIds.has(session.id),
            );
            const samples = current.filter(
              session => session.source === 'sample' && !liveIds.has(session.id),
            );
            return [...liveSessions, ...existingLive, ...samples];
          });
        })
        .catch(error => {
          if (!cancelled) {
            console.warn('Unable to load configured sessions:', error);
          }
        });
    };

    loadSessions();
    const refreshTimer = window.setInterval(loadSessions, appConfig.session_refresh_interval_ms);
    return () => {
      cancelled = true;
      window.clearInterval(refreshTimer);
    };
  }, [isRunning]);

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

  const header = (
    <LayoutHeader hasDivider padding={4} xstyle={styles.header} label="Run header">
      <HStack align="center" justify="between" gap={4}>
        <VStack gap={0.5} xstyle={styles.headerTitle}>
          <HStack align="center" gap={2}>
            <Heading level={1} xstyle={styles.singleLine}>
              {activeSession.title}
            </Heading>
            <Text type="supporting" color="secondary">
              {activeSession.time}
            </Text>
          </HStack>
          <Text type="supporting" color="secondary">
            Agent workspace · {isLiveSession ? 'live Responses stream' : 'reasoning preview'}
          </Text>
        </VStack>
        <HStack align="center" gap={2} xstyle={styles.headerActions}>
          <HStack align="center" gap={2}>
            <StatusDot
              variant={isRunning ? 'accent' : 'success'}
              label={isRunning ? 'Run in progress' : 'Run complete'}
              isPulsing={isRunning}
            />
            <Text type="supporting" color="secondary">
              {isRunning ? 'Working' : 'Ready'}
            </Text>
          </HStack>
          <Button
            label={isRunning ? 'Running…' : 'Run again'}
            variant="secondary"
            size="sm"
            isDisabled={isRunning}
            icon={<Icon icon="arrowUp" size="sm" />}
            onClick={() => submitMessage('Run the default harness check again.')}
          />
          <IconButton
            label={showInspector ? 'Hide run inspector' : 'Show run inspector'}
            icon={<Icon icon="viewColumns" size="sm" />}
            variant={showInspector ? 'secondary' : 'ghost'}
            size="sm"
            onClick={() => setShowInspector(current => !current)}
          />
        </HStack>
      </HStack>
    </LayoutHeader>
  );

  const composer = (
    <ChatComposer
      value={composerValue}
      onChange={setComposerValue}
      onSubmit={submitMessage}
      onStop={stopRun}
      isStopShown={isRunning}
      placeholder="Ask the agent to inspect a task…"
      headerActions={
        <IconButton
          label="Attach context"
          icon={<Icon icon="copy" size="sm" />}
          variant="ghost"
          size="sm"
        />
      }
      headerContext={
        <HStack align="center" gap={2}>
          <StatusDot
            variant={isLiveSession ? 'success' : 'accent'}
            label={isLiveSession ? 'Live backend transport' : 'Mock transport'}
          />
          <Text type="supporting" color="secondary">
            {isLiveSession ? 'live' : 'mock'}
          </Text>
        </HStack>
      }
      footerActions={
        <Text type="supporting" color="secondary">
          Enter to send · Shift+Enter for a new line
        </Text>
      }
      elevation="low"
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
        />
      }
    >
      <VStack xstyle={styles.shell}>
        <Layout
          height="fill"
          header={header}
          content={
            <LayoutContent
              isScrollable={false}
              padding={0}
              role="region"
              label="Agent conversation"
            >
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
                      {messages.map(message =>
                        renderMessage(message, activeSession.model),
                      )}
                      {isRunning && (!isLiveSession || streamingMessage) && !streamingMessageCommitted && (
                        <ChatMessage sender="assistant" avatar={<AgentAvatar isRunning />}>
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
                                <Markdown
                                  density="compact"
                                  contentWidth="100%"
                                  isStreaming
                                  headingLevelStart={3}
                                >
                                  Astra is working through the request…
                                </Markdown>
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
