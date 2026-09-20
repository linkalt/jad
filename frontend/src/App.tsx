import {useCallback, useEffect, useMemo, useRef, useState} from 'react';
import * as stylex from '@stylexjs/stylex';
import {
  AppShell,
  Avatar,
  Badge,
  Button,
  Card,
  ChatComposer,
  Collapsible,
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
import {fetchSession, streamSessionRun} from './api';
import {ResizeHandle, useResizable} from '@astryxdesign/core/Resizable';
import {pixel} from '@astryxdesign/core/Resizable/utils';
import {
  borderVars,
  colorVars,
  radiusVars,
  sizeVars,
  spacingVars,
  typeScaleVars,
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
  headerActions: {
    flexShrink: 0,
  },
  chat: {
    height: '100%',
    minHeight: 0,
    minWidth: 0,
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
  reasoningCard: {
    width: '100%',
  },
  reasoningTrigger: {
    minWidth: 0,
    width: '100%',
  },
  reasoningTitle: {
    minWidth: 0,
    maxWidth: '100%',
    flex: 1,
    overflow: 'hidden',
    textOverflow: 'ellipsis',
    whiteSpace: 'nowrap',
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
type MessageSender = 'system' | 'user' | 'assistant';

interface Session {
  id: string;
  title: string;
  time: string;
  turns: number;
  state: SessionState;
}

interface ReasoningStep {
  label: string;
  detail: string;
  status: TraceState;
}

interface MessageRecord {
  id: string;
  sender: MessageSender;
  text: string;
  timestamp: string;
  reasoning?: ReasoningStep[];
  reasoningContent?: string;
  tools?: ChatToolCallItem[];
}

const LIVE_SESSION_ID = 'session1';

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function textFromParts(value: unknown, types: string[]): string {
  if (!Array.isArray(value)) {
    return '';
  }
  return value
    .filter(
      (part): part is Record<string, unknown> =>
        isRecord(part) && typeof part.type === 'string' && types.includes(part.type),
    )
    .map(part => (typeof part.text === 'string' ? part.text : ''))
    .join('');
}

function responseText(value: unknown): string {
  if (!isRecord(value)) {
    return '';
  }
  const response = isRecord(value.response) ? value.response : value;
  if (!Array.isArray(response.output)) {
    return '';
  }
  return response.output
    .filter(
      (item): item is Record<string, unknown> =>
        isRecord(item) && item.type === 'message' && item.role === 'assistant',
    )
    .map(item => textFromParts(item.content, ['output_text']))
    .join('');
}

function responseReasoningText(value: unknown): string {
  if (!isRecord(value)) {
    return '';
  }
  const response = isRecord(value.response) ? value.response : value;
  if (!Array.isArray(response.output)) {
    return '';
  }
  return response.output
    .filter(
      (item): item is Record<string, unknown> =>
        isRecord(item) && item.type === 'reasoning',
    )
    .flatMap(item => [
      textFromParts(item.content, ['reasoning_text', 'output_text', 'text']),
      textFromParts(item.summary, ['summary_text', 'reasoning_text', 'output_text', 'text']),
    ])
    .filter(Boolean)
    .join('\n\n')
    .trim();
}

function messageTime(): string {
  return new Date().toLocaleTimeString([], {
    hour: 'numeric',
    minute: '2-digit',
  });
}

function oneLine(value: string): string {
  return value.replace(/\s+/g, ' ').trim();
}

function recordsToMessages(records: unknown[]): MessageRecord[] {
  const messages: MessageRecord[] = [];
  records.forEach((value, index) => {
    if (!isRecord(value)) {
      return;
    }
    if (value.type === 'message' && value.role === 'user') {
      const text = textFromParts(value.content, ['input_text']);
      if (text) {
        messages.push({
          id: `session-record-user-${index}`,
          sender: 'user',
          text,
          timestamp: messageTime(),
        });
      }
      return;
    }
    if (value.type === 'response.completed') {
      const text = responseText(value);
      const reasoningContent = responseReasoningText(value);
      if (text) {
        messages.push({
          id: `session-record-assistant-${index}`,
          sender: 'assistant',
          text,
          timestamp: messageTime(),
          ...(reasoningContent ? {reasoningContent} : {}),
        });
      }
    }
  });
  return messages;
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
  },
  {
    id: 'atlas',
    title: 'Atlas onboarding flow',
    time: '2 min ago',
    turns: 8,
    state: 'success',
  },
  {
    id: 'settings',
    title: 'Settings surface audit',
    time: '18 min ago',
    turns: 5,
    state: 'warning',
  },
  {
    id: 'empty',
    title: 'Empty state exploration',
    time: 'Yesterday',
    turns: 3,
    state: 'success',
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
  compact = false,
}: {
  steps: ReasoningStep[];
  content?: string;
  compact?: boolean;
}) {
  const [isOpen, setIsOpen] = useState(false);
  const hasContent = content.trim().length > 0;
  const hasSteps = steps.length > 0;
  const title = oneLine(
    content ||
      steps.find(step => step.status === 'running')?.detail ||
      steps[0]?.detail ||
      '',
  );
  const completedSteps = steps.filter(step => step.status === 'complete').length;

  if (!hasContent && !hasSteps) {
    return null;
  }

  return (
    <Card variant="muted" padding={compact ? 2 : 3} xstyle={styles.reasoningCard}>
      <Collapsible
        isOpen={isOpen}
        onOpenChange={setIsOpen}
        trigger={
          <HStack
            as="span"
            align="center"
            gap={2}
            width="100%"
            xstyle={styles.reasoningTrigger}
          >
            <Icon icon="wrench" size="sm" color="accent" />
            {isOpen ? (
              <VisuallyHidden>Thinking</VisuallyHidden>
            ) : (
              <>
                <Text type="label" weight="semibold">
                  Thinking
                </Text>
                {title && (
                  <Text
                    type="supporting"
                    color="secondary"
                    maxLines={1}
                    xstyle={styles.reasoningTitle}
                  >
                    {title}
                  </Text>
                )}
              </>
            )}
          </HStack>
        }
      >
        <VStack gap={2}>
          {hasContent && (
            <Markdown density="compact" xstyle={styles.reasoningContent}>
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
      </Collapsible>
    </Card>
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
  isRunning,
  onClose,
}: {
  activeSession: Session;
  steps: ReasoningStep[];
  tools: ChatToolCallItem[];
  isRunning: boolean;
  onClose: () => void;
}) {
  const completedSteps = steps.filter(step => step.status === 'complete').length;
  const isLive = activeSession.id === LIVE_SESSION_ID;
  const inspector = useResizable({
    defaultSize: pixel(360),
    minSize: pixel(280),
    maxSize: pixel(640),
    direction: 'horizontal',
    autoSaveId: 'agent-harness-run-inspector',
  });

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
            <Text type="supporting" color="secondary" maxLines={1}>
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
            <ReasoningBlock steps={steps} compact />
          </VStack>
        )}

        <VStack as="section" gap={2}>
          <HStack align="center" justify="between" gap={2}>
            <Heading level={2} accessibilityLevel={3}>
              Tool activity
            </Heading>
            <Badge label={String(tools.length)} />
          </HStack>
          <ChatToolCalls calls={tools} defaultIsExpanded />
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

function renderMessage(message: MessageRecord) {
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
          name={<Text type="label" weight="semibold">You</Text>}
          metadata={<ChatMessageMetadata timestamp={message.timestamp} status="read" />}
        >
          <Markdown density="compact">{message.text}</Markdown>
        </ChatMessageBubble>
      </ChatMessage>
    );
  }

  return (
    <ChatMessage
      key={message.id}
      sender="assistant"
      avatar={<AgentAvatar />}
    >
      <ChatMessageBubble
        variant="ghost"
        width="100%"
        name={
          <HStack align="center" gap={2}>
            <Text type="label" weight="semibold">Astra</Text>
            <Text type="supporting" color="secondary">agent</Text>
          </HStack>
        }
        metadata={
          <ChatMessageMetadata
            timestamp={message.timestamp}
            footer="agent response"
            status="delivered"
          />
        }
      >
        <VStack gap={3} width="100%">
          {(message.reasoning || message.reasoningContent) && (
            <ReasoningBlock
              steps={message.reasoning ?? []}
              content={message.reasoningContent}
            />
          )}
          <Markdown density="compact" headingLevelStart={3}>
            {message.text}
          </Markdown>
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

function App() {
  const [sessions, setSessions] = useState<Session[]>(initialSessions);
  const [activeSessionId, setActiveSessionId] = useState(LIVE_SESSION_ID);
  const [threads, setThreads] = useState<Record<string, MessageRecord[]>>(initialThreads);
  const [composerValue, setComposerValue] = useState('');
  const [showInspector, setShowInspector] = useState(true);
  const [runningSessionId, setRunningSessionId] = useState<string | null>(null);
  const [runProgress, setRunProgress] = useState(baseReasoning.length);
  const [streamingText, setStreamingText] = useState('');
  const [streamingReasoning, setStreamingReasoning] = useState('');
  const runAbortRef = useRef<AbortController | null>(null);
  const runTimeoutRef = useRef<number | null>(null);

  const activeSession =
    sessions.find(session => session.id === activeSessionId) ?? sessions[0];
  const messages = threads[activeSessionId] ?? [];
  const isRunning = runningSessionId === activeSessionId;
  const isLiveSession = activeSessionId === LIVE_SESSION_ID;

  const reasoningSteps = useMemo(() => {
    const steps = isLiveSession ? [] : baseReasoning;
    return getTraceForSteps(steps, isRunning ? runProgress : steps.length);
  }, [isLiveSession, isRunning, runProgress]);

  const tools = useMemo(() => {
    if (activeSessionId === LIVE_SESSION_ID) {
      return [];
    }
    const latestAssistant = [...messages]
      .reverse()
      .find(message => message.sender === 'assistant' && message.tools);
    const sourceTools = latestAssistant?.tools ?? defaultTools;

    if (!isRunning) {
      return sourceTools;
    }

    return sourceTools.map(
      (tool, index): ChatToolCallItem => ({
        ...tool,
        status: index === 0 ? 'running' : 'pending',
        duration: undefined,
      }),
    );
  }, [activeSessionId, isRunning, messages]);

  const stopRun = useCallback(() => {
    runAbortRef.current?.abort();
    runAbortRef.current = null;
    if (runTimeoutRef.current !== null) {
      window.clearTimeout(runTimeoutRef.current);
      runTimeoutRef.current = null;
    }
    setStreamingText('');
    setStreamingReasoning('');
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
      setRunningSessionId(sessionId);
      setRunProgress(sessionId === LIVE_SESSION_ID ? 0 : 1);
      setStreamingText('');
      setStreamingReasoning('');

      if (sessionId === LIVE_SESSION_ID) {
        const controller = new AbortController();
        let streamedText = '';
        let streamedReasoning = '';
        runAbortRef.current = controller;
        void streamSessionRun(sessionId, trimmedValue, {
          signal: controller.signal,
          onDelta: text => {
            streamedText += text;
            setStreamingText(streamedText);
            setRunProgress(1);
          },
          onReasoning: text => {
            streamedReasoning += text;
            setStreamingReasoning(streamedReasoning);
          },
          onCompleted: () => setRunProgress(2),
        })
          .then(result => {
            if (controller.signal.aborted || runAbortRef.current !== controller) {
              return;
            }
            const assistantText =
              streamedText || responseText(result.response) || 'The run completed without text output.';
            const reasoningContent = responseReasoningText(result.response) || streamedReasoning;
            const assistantMessage: MessageRecord = {
              id: `${sessionId}-assistant-${Date.now()}`,
              sender: 'assistant',
              text: assistantText,
              timestamp: messageTime(),
              ...(reasoningContent ? {reasoningContent} : {}),
            };
            setThreads(current => ({
              ...current,
              [sessionId]: [...(current[sessionId] ?? []), assistantMessage],
            }));
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
            setStreamingText('');
            setStreamingReasoning('');
            setRunProgress(sessionId === LIVE_SESSION_ID ? 0 : baseReasoning.length);
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
        setRunProgress(baseReasoning.length);
        setRunningSessionId(current => (current === sessionId ? null : current));
        runTimeoutRef.current = null;
      }, 1100);
    },
    [activeSessionId, isRunning],
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
    void fetchSession(LIVE_SESSION_ID)
      .then(snapshot => {
        if (cancelled) {
          return;
        }
        setThreads(current => ({
          ...current,
          [LIVE_SESSION_ID]: recordsToMessages(snapshot.messages),
        }));
        setSessions(current =>
          current.map(session =>
            session.id === LIVE_SESSION_ID
              ? {
                  ...session,
                  title: snapshot.session_name || session.title,
                  turns: snapshot.turns,
                  time: snapshot.turns > 0 ? 'Local transcript' : 'Ready',
                  state: 'success',
                }
              : session,
          ),
        );
      })
      .catch(error => {
        if (!cancelled) {
          console.warn('Unable to load the live session:', error);
        }
      });

    return () => {
      cancelled = true;
    };
  }, []);

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
            <Heading level={1} maxLines={1}>
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
                composer={composer}
              >
                <ChatMessageList density="balanced" align="top" isStreaming={isRunning}>
                  {messages.map(renderMessage)}
                  {isRunning && (
                    <ChatMessage sender="assistant" avatar={<AgentAvatar isRunning />}>
                      <ChatMessageBubble variant="ghost" width="100%">
                        <VStack gap={3} width="100%">
                          <ReasoningBlock
                            steps={reasoningSteps}
                            content={streamingReasoning}
                          />
                          <Markdown density="compact" isStreaming headingLevelStart={3}>
                            {streamingText || 'Astra is working through the request…'}
                          </Markdown>
                        </VStack>
                      </ChatMessageBubble>
                    </ChatMessage>
                  )}
                </ChatMessageList>
              </ChatLayout>
            </LayoutContent>
          }
        />
        {showInspector && (
          <RunInspector
            activeSession={activeSession}
            steps={reasoningSteps}
            tools={tools}
            isRunning={isRunning}
            onClose={() => setShowInspector(false)}
          />
        )}
      </VStack>
    </AppShell>
  );
}

export default App;
