import {Badge, Card, ChatToolCalls, HStack, Heading, StatusDot, Text, VStack} from '@astryxdesign/core';
import {type ResizableRegion} from '@astryxdesign/core/Resizable';
import * as stylex from '@stylexjs/stylex';
import {type WebUiPart} from '../api';
import {type Session} from '../app-types';
import {useRowWidth} from '../use-row-width';
import {FloatingPanel} from './FloatingPanel';
import {toolActivityForParts} from './TranscriptParts';

const styles = stylex.create({
  sectionHeading: {
    minWidth: 0,
  },
});

export function RunInspector({
  activeSession,
  liveToolParts,
  inspector,
  isRunning,
  isEdgeFixed,
  onClose,
}: {
  activeSession: Session;
  liveToolParts: WebUiPart[] | null;
  inspector: ResizableRegion;
  isRunning: boolean;
  /** True with a single pane on screen: the panel takes the window's right edge. */
  isEdgeFixed?: boolean;
  onClose: () => void;
}) {
  // Rows measure their own width so titles fit the floating panel too.
  const [toolListRef, toolListWidthPx] = useRowWidth();
  const toolItems = liveToolParts === null ? [] : toolActivityForParts(liveToolParts, toolListWidthPx);

  return (
    <FloatingPanel
      side="end"
      layer="pane"
      label="Run inspector"
      title="Run inspector"
      subtitle={activeSession.title}
      region={inspector}
      isEdgeFixed={isEdgeFixed}
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
            </HStack>
            <Text type="supporting" color="secondary">
              Live response events and reasoning appear in the conversation when available.
            </Text>
          </VStack>
        </Card>

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
