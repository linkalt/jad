import {Badge, Card, type ChatToolCallItem, ChatToolCalls, HStack, Heading, StatusDot, Text, VStack} from '@astryxdesign/core';
import {type ResizableRegion} from '@astryxdesign/core/Resizable';
import * as stylex from '@stylexjs/stylex';
import {type WebUiPart} from '../api';
import {type ReasoningStep, type Session} from '../app-types';
import {useRowWidth} from '../use-row-width';
import {FloatingPanel} from './FloatingPanel';
import {ReasoningBlock, toolActivityForParts} from './TranscriptParts';

const styles = stylex.create({
  sectionHeading: {
    minWidth: 0,
  },
});

export function RunInspector({
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
              <Heading level={3} xstyle={styles.sectionHeading}>
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
            <Heading level={3} xstyle={styles.sectionHeading}>
              Tool activity
            </Heading>
            <Badge label={String(toolItems.length)} />
          </HStack>
          <ChatToolCalls calls={toolItems} defaultIsExpanded />
        </VStack>
    </FloatingPanel>
  );
}
