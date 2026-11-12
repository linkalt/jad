import {Badge, Button, Card, type ChatToolCallItem, ChatToolCalls, CodeBlock, HStack, Icon, StatusDot, type StatusDotVariant, Text, VStack} from '@astryxdesign/core';
import {borderVars, colorVars, radiusVars, sizeVars, spacingVars, typeScaleVars} from '@astryxdesign/core/theme/tokens.stylex';
import * as stylex from '@stylexjs/stylex';
import {Fragment, type KeyboardEvent, type MouseEvent, type ReactNode, useContext, useEffect, useId, useLayoutEffect, useRef, useState} from 'react';
import {activityRefsChanged} from '../right-panel';
import {type WebUiHostedToolPart, type WebUiMessageStep, type WebUiPart} from '../api';
import {type MessageRecord, type ReasoningStep, type TraceState} from '../app-types';
import appConfig from '../config.json';
import {cutRowText, oneLine, toolTitleBudgetPx} from '../row-text';
import {hasToolResultImages, toolResultText} from '../tool-result-view';
import {hostedToolStatus} from '../web-search-view';
import {
  type WebUiActivityGroupPart,
  type WebUiToolCall,
  type WebUiToolResult,
  groupWebUiParts,
  segmentWebUiGroups,
  webUiGroupIndex,
  webUiGroupKey,
} from '../web-ui-part-groups';
import {toolGroupLayout} from '../tool-group';
import {hiddenWebUiStepIds, isWebUiPartHidden, visibleWebUiItems} from '../web-ui-steps';
import {type ActivityDetail, ActivityPanelContext} from './activity-panel';
import {CitationList} from './CitationList';
import {MarkdownText} from './MarkdownText';
import {InlineImages, StoredImages} from './StoredImages';
import {type ScrollPositionSnapshot, captureScrollPosition, findScrollParent, holdAnchorPosition, revealScrollOptions} from './scroll-position';

export type {WebUiContentPart} from '../web-ui-part-groups';

const styles = stylex.create({
  // Reasoning rows and tool rows share one tight stack, so the group itself
  // carries no block margin; the stack gap supplies the row rhythm.
  activityGroup: {
    width: '100%',
    marginBlockStart: spacingVars['--spacing-0'],
  },
  reasoningContent: {
    color: colorVars['--color-text-secondary'],
    fontSize: typeScaleVars['--text-supporting-size'],
    lineHeight: typeScaleVars['--text-supporting-leading'],
  },
  toolCallsFlush: {
    marginBlockStart: spacingVars['--spacing-0'],
  },
  // The first call of a group shrinks (its text is cut) before the toggle does.
  groupHeaderCall: {
    flexShrink: 1,
    minWidth: 0,
  },
  // Fills the rest of the header row, so the empty space after the chevron
  // toggles too while the icon group stays next to the first call.
  groupToggle: {
    flexGrow: 1,
    paddingBlock: spacingVars['--spacing-1'],
  },
  activityTrigger: {
    cursor: 'pointer',
    borderRadius: radiusVars['--radius-inner'],
    outlineStyle: {default: 'none', ':focus-visible': 'solid'},
    outlineWidth: '2px',
    outlineColor: colorVars['--color-accent'],
    outlineOffset: '-2px',
  },
  traceCopy: {
    minWidth: 0,
    flex: 1,
  },
  traceList: {
    listStyleType: 'none',
    margin: spacingVars['--spacing-0'],
    padding: spacingVars['--spacing-0'],
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
  traceRow: {
    minWidth: 0,
    paddingBlock: spacingVars['--spacing-1'],
    borderBlockEnd: `${borderVars['--border-width']} solid ${colorVars['--color-border']}`,
  },
});

// Revealing steps inserts content next to the control that was pressed, so the
// reader is held on that control until the rows around it stop changing size.
const REVEAL_STEPS_SCROLL_OPTIONS = revealScrollOptions('steps', appConfig);

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

/**
 * A reasoning or tool row. It opens the pane's right-edge panel instead of
 * expanding inline; while the panel shows this row, the row hands it the latest
 * data whenever `refs` change, so a streaming block updates the open panel.
 */
function ActivityRow({
  detail,
  refs,
  children,
}: {
  detail: ActivityDetail;
  /** The values `detail` is built from; a change refreshes the open panel. */
  refs: unknown[];
  children: ReactNode;
}) {
  const control = useContext(ActivityPanelContext);
  const isOpen = control !== null && control.openKey === detail.key;
  const update = control?.update;
  const pushedRefs = useRef<unknown[] | undefined>(undefined);
  useEffect(() => {
    if (!isOpen) {
      pushedRefs.current = undefined;
      return;
    }
    // `detail` is rebuilt on every render; `refs` say when its data changed
    // (a group's length varies, so they are compared rather than used as deps).
    if (activityRefsChanged(pushedRefs.current, refs)) {
      pushedRefs.current = refs;
      update?.(detail);
    }
  });

  if (control === null) {
    return <>{children}</>;
  }
  const open = (element: HTMLElement) => control.toggle(detail, element);
  const handleClick = (event: MouseEvent<HTMLDivElement>) => {
    // Dragging across the row's text selects it; that is not a request to open.
    const selection = window.getSelection();
    if (
      selection !== null &&
      !selection.isCollapsed &&
      event.currentTarget.contains(selection.anchorNode)
    ) {
      return;
    }
    open(event.currentTarget);
  };
  const handleKeyDown = (event: KeyboardEvent<HTMLDivElement>) => {
    if (event.target === event.currentTarget && (event.key === 'Enter' || event.key === ' ')) {
      event.preventDefault();
      open(event.currentTarget);
    }
  };
  return (
    <div
      role="button"
      tabIndex={0}
      aria-expanded={isOpen}
      // Part of the panel's own toggle set: pressing the open block must not
      // dismiss the panel on pointerdown and then reopen it on click.
      data-panel-toggle=""
      onClick={handleClick}
      onKeyDown={handleKeyDown}
      {...stylex.props(styles.activityTrigger)}
    >
      {children}
    </div>
  );
}

/** The row of a reasoning block, without what opens it; null when there is nothing to show. */
export function reasoningCallItem({
  content,
  steps,
  rowWidthPx,
  isStreaming,
}: {
  content: string;
  steps: ReasoningStep[];
  rowWidthPx: number;
  isStreaming: boolean;
}): ChatToolCallItem | null {
  if (content.trim().length === 0 && steps.length === 0) {
    return null;
  }
  const title = reasoningTitleText(
    content ||
      steps.find(step => step.status === 'running')?.detail ||
      steps[0]?.detail ||
      '',
    toolTitleBudgetPx(rowWidthPx, REASONING_ROW_NAME),
  );
  return {
    key: `reasoning-${title}-${steps.length}`,
    name: REASONING_ROW_NAME,
    status: isStreaming ? 'running' : 'complete',
    target: title || undefined,
  };
}

export function ReasoningBlock({
  steps,
  content = '',
  rowWidthPx,
  isFlush = false,
  isStreaming = false,
  activityKey,
}: {
  /** Identifies this block to the panel that shows it; unique per message. */
  activityKey: string;
  steps: ReasoningStep[];
  content?: string;
  /** Width available to the row; the title is cut to it so the name stays whole. */
  rowWidthPx: number;
  /** Drop the row's own block margin; the parent supplies the spacing. */
  isFlush?: boolean;
  isStreaming?: boolean;
}) {
  const item = reasoningCallItem({content, steps, rowWidthPx, isStreaming});
  if (item === null) {
    return null;
  }

  return (
    <ActivityRow
      detail={{key: activityKey, kind: 'reasoning', content, steps, isStreaming}}
      refs={[content, steps.length, isStreaming]}
    >
      <ChatToolCalls
        calls={[item]}
        xstyle={isFlush ? styles.toolCallsFlush : undefined}
      />
    </ActivityRow>
  );
}

/** The reasoning text and steps, as the panel shows them. */
export function ReasoningDetail({
  content,
  steps,
}: {
  content: string;
  steps: ReasoningStep[];
}) {
  const hasContent = content.trim().length > 0;
  const hasSteps = steps.length > 0;
  const completedSteps = steps.filter(step => step.status === 'complete').length;
  return (
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
  );
}

function prettyToolArguments(argumentsText: string): string {
  try {
    const parsed = JSON.parse(argumentsText) as unknown;
    return JSON.stringify(parsed, null, 2) ?? argumentsText;
  } catch {
    return argumentsText;
  }
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

function resultText(result: WebUiToolResult): string | undefined {
  return toolResultText(result.output, result);
}

/** The images of one tool result, shown under its row without opening it. */
function ToolResultImages({result, source}: {result: WebUiToolResult; source?: TruncatedSource}) {
  return (
    <>
      {result.images !== undefined && <StoredImages names={result.images} />}
      {result.inlineImages !== undefined && result.inlineImages > 0 && (
        <InlineImages
          messageId={source?.message.id}
          callId={result.callId}
          count={result.inlineImages}
          cursor={source?.message.cursor}
        />
      )}
    </>
  );
}

function visibleToolOutput(output: string): string {
  const limit = appConfig.tool_output_max_chars;
  if (output.length <= limit) {
    return output;
  }
  return `${output.slice(0, limit)}\n\n[Output truncated in the UI]`;
}

/** The message a truncated part came from, with the action that loads it whole. */
export interface TruncatedSource {
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
export function TruncatedNotice({
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

export function toolCallItem(
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
  };
}

/** One tool call with its arguments and result, as the panel shows them. */
export function ToolCallDetail({
  call,
  result,
  source,
  children,
}: {
  call: WebUiToolCall;
  result?: WebUiToolResult;
  source?: TruncatedSource;
  /**
   * Extra sections below the result. The host that shows the call in the right panel
   * passes the subagent snapshot here; the inline detail of a read-only transcript
   * passes nothing, so a snapshot never nests inside a snapshot.
   */
  children?: ReactNode;
}) {
  return (
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
      {result && resultText(result) !== undefined && (
        <CodeBlock
          code={visibleToolOutput(resultText(result) ?? '')}
          language="text"
          size="sm"
          width="100%"
          container="card"
          maxHeight={appConfig.tool_result_max_height_px}
          hasCopyButton
        />
      )}
      {result?.truncated !== undefined && (
        <TruncatedNotice bytes={result.truncated.bytes} source={source} />
      )}
      {children}
    </VStack>
  );
}

/** The raw item a provider-run tool reported, as the panel shows it. */
export function HostedToolDetail({
  part,
  source,
}: {
  part: WebUiHostedToolPart;
  source?: TruncatedSource;
}) {
  return (
    <VStack gap={2} width="100%">
      {part.raw !== undefined && (
        <CodeBlock
          code={prettyToolArguments(part.raw)}
          language="json"
          size="sm"
          width="100%"
          container="card"
          maxHeight={appConfig.tool_result_max_height_px}
          hasCopyButton
        />
      )}
      {part.truncated !== undefined && (
        <TruncatedNotice bytes={part.truncated.bytes} source={source} />
      )}
    </VStack>
  );
}

export function OrphanToolResultBlock({
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
        {resultText(result) !== undefined && (
          <CodeBlock
            code={visibleToolOutput(resultText(result) ?? '')}
            language="text"
            size="sm"
            width="100%"
            container="section"
            maxHeight={appConfig.tool_result_max_height_px}
            hasCopyButton
          />
        )}
        <ToolResultImages result={result} source={source} />
        {result.truncated !== undefined && (
          <TruncatedNotice bytes={result.truncated.bytes} source={source} />
        )}
      </VStack>
    </Card>
  );
}

export function toolActivityForParts(
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

/**
 * A tool group with several calls: the first call is the header row, followed
 * directly by the wrench, the call count and a chevron, all left aligned. The
 * first call opens the panel like any call row; pressing the wrench, count or
 * chevron (or the empty space after them) only expands or collapses the list of
 * the remaining calls, each a row of its own. While the panel shows one of the
 * listed calls the list stays expanded, so the row that opened the panel stays
 * on screen. The expanded state is local, so calls streaming in neither
 * collapse nor re-expand it.
 */
function ToolGroupRow({
  calls,
  renderCall,
}: {
  calls: {call: WebUiToolCall; result: WebUiToolResult | undefined}[];
  renderCall: (call: WebUiToolCall, result: WebUiToolResult | undefined) => ReactNode;
}) {
  const control = useContext(ActivityPanelContext);
  const [isToggled, setIsToggled] = useState(false);
  const {header, listed, holdsOpenPanel} = toolGroupLayout(
    calls.map(entry => ({...entry, callId: entry.call.callId})),
    control?.openKey,
  );
  const isExpanded = isToggled || holdsOpenPanel;
  const toggle = () => setIsToggled(!isExpanded);
  const listId = useId();
  return (
    <VStack gap={1} width="100%">
      <HStack gap={2} align="center" width="100%">
        <div {...stylex.props(styles.groupHeaderCall)}>{renderCall(header.call, header.result)}</div>
        <div
          role="button"
          tabIndex={0}
          aria-expanded={isExpanded}
          aria-controls={listId}
          aria-label={`${isExpanded ? 'Hide' : 'Show'} ${listed.length} more tool ${listed.length === 1 ? 'call' : 'calls'}`}
          onClick={toggle}
          onKeyDown={event => {
            if (event.target === event.currentTarget && (event.key === 'Enter' || event.key === ' ')) {
              event.preventDefault();
              toggle();
            }
          }}
          {...stylex.props(styles.activityTrigger, styles.groupToggle)}
        >
          <HStack gap={1} align="center">
            <Icon icon="wrench" size="xsm" color="inherit" />
            <Text type="supporting">{calls.length}</Text>
            <Icon
              icon="chevronDown"
              size="xsm"
              color="inherit"
              style={{transform: isExpanded ? 'rotate(180deg)' : undefined}}
            />
          </HStack>
        </div>
      </HStack>
      <div id={listId} hidden={!isExpanded}>
        {isExpanded && (
          <VStack gap={1} width="100%">
            {listed.map(({call, result}) => renderCall(call, result))}
          </VStack>
        )}
      </div>
    </VStack>
  );
}

function WebUiActivityGroup({
  group,
  scope,
  rowWidthPx,
  isStreaming,
  source,
}: {
  group: WebUiActivityGroupPart;
  scope: string;
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
          activityKey={`${scope}:reasoning:${group.item.index}`}
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
    // for. The row shows what the provider reported; the panel shows the raw
    // item, like any other tool.
    return (
      <VStack gap={1} width="100%" xstyle={styles.activityGroup}>
        <ActivityRow
          detail={{
            key: `hosted-${part.callId}`,
            kind: 'hosted',
            name: part.name,
            target: part.target,
            part,
            source,
          }}
          refs={[part]}
        >
          <ChatToolCalls
            calls={[
              {
                key: `hosted-${part.callId}`,
                name: part.name,
                status,
                target: part.target ?? '',
                errorMessage: status === 'error' ? 'The provider tool failed.' : undefined,
              },
            ]}
            xstyle={styles.toolCallsFlush}
          />
        </ActivityRow>
        {part.images !== undefined && <StoredImages names={part.images} />}
      </VStack>
    );
  }

  const resultFor = (call: WebUiToolCall, itemIndex: number) =>
    group.items
      .slice(itemIndex + 1)
      .map(item => item.part)
      .find(
        (candidate): candidate is WebUiToolResult =>
          candidate.type === 'tool_result' && candidate.callId === call.callId,
      );
  const lastToolCallIndex = group.items
    .filter(({part}) => part.type === 'tool_call')
    .pop()?.index;
  const callIds = new Set(
    group.items.flatMap(({part}) => (part.type === 'tool_call' ? [part.callId] : [])),
  );
  const toolCallResults = group.items.flatMap(({part}) =>
    part.type === 'tool_result' && callIds.has(part.callId) ? [part] : [],
  );

  const groupCalls = group.items.flatMap(({part}, itemIndex) =>
    part.type === 'tool_call' ? [{call: part, result: resultFor(part, itemIndex)}] : [],
  );
  const firstToolCallIndex = group.items.find(({part}) => part.type === 'tool_call')?.index;
  const isGrouped = groupCalls.length >= 2;
  const images = toolCallResults.some(hasToolResultImages) && (
    <HStack gap={2} wrap="wrap" align="start">
      {toolCallResults.map(result => (
        <ToolResultImages key={result.callId} result={result} source={source} />
      ))}
    </HStack>
  );
  const callRow = (
    call: WebUiToolCall,
    result: WebUiToolResult | undefined,
  ) => {
    const item = toolCallItem(call, result, rowWidthPx);
    return (
      <ActivityRow
        key={`tool-call-${call.callId}`}
        detail={{
          key: item.key ?? `tool-${call.callId}`,
          kind: 'tool',
          name: call.name,
          target: item.target,
          call,
          result,
          source,
        }}
        refs={[call, result]}
      >
        <ChatToolCalls calls={[item]} xstyle={styles.toolCallsFlush} />
      </ActivityRow>
    );
  };

  return (
    <VStack gap={1} width="100%" xstyle={styles.activityGroup}>
      {group.items.map(({part, index}, itemIndex) => {
        if (part.type === 'tool_call' && isGrouped) {
          // Several calls share one expander row; each listed call is its own
          // panel row, so the panel never shows a group.
          return index === firstToolCallIndex ? (
            <Fragment key={`tool-calls-${index}`}>
              <ToolGroupRow
                calls={groupCalls}
                renderCall={callRow}
              />
              {images}
            </Fragment>
          ) : null;
        }
        if (part.type === 'tool_call') {
          return (
            <Fragment key={`tool-call-${index}`}>
              {callRow(part, resultFor(part, itemIndex))}
              {index === lastToolCallIndex && images}
            </Fragment>
          );
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

export function WebUiParts({
  parts,
  steps,
  rowWidthPx,
  isStreaming = false,
  source,
  scope,
}: {
  /** Names the message these parts belong to, so its blocks have their own panel keys. */
  scope: string;
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
                scope={scope}
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
