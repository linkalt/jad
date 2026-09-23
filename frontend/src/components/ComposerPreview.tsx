/**
 * @file ComposerPreview.tsx
 * @input Uses React, StyleX, Astryx ChatComposerDrawer/Heading/Text/VStack
 * @output Exports ComposerPreview — renders the composer draft as it will appear
 * @position Composer UI; rendered by ComposerPane in the composer's drawer slot
 *
 * Renders through the same MarkdownText path as the transcript, so the preview
 * shows what the message will actually look like once sent (including the
 * line-break behaviour that `preserve_line_breaks` controls).
 *
 * The body is clamped to a viewport-relative height so a long draft cannot
 * push the composer itself off screen. Tokens are used for everything else;
 * the theme has no panel-height token, and a viewport unit keeps this
 * responsive rather than tying it to a fixed pixel value.
 */
import * as stylex from '@stylexjs/stylex';
import {Heading, Text, VStack} from '@astryxdesign/core';
import {ChatComposerDrawer} from '@astryxdesign/core/Chat';
import {MarkdownText} from './MarkdownText';

const styles = stylex.create({
  body: {
    maxHeight: '40vh',
    overflowY: 'auto',
  },
});

export function ComposerPreview({value}: {value: string}) {
  const isEmpty = value.trim().length === 0;
  return (
    <ChatComposerDrawer>
      <VStack gap={4} padding={4}>
        <VStack gap={0.5}>
          <Heading level={2}>Preview</Heading>
          <Text type="supporting" color="secondary">
            How this message will be rendered
          </Text>
        </VStack>
        <VStack gap={2} xstyle={styles.body}>
          {isEmpty ? (
            <Text type="supporting" color="secondary">
              Nothing to preview yet.
            </Text>
          ) : (
            <MarkdownText density="compact" contentWidth="100%" headingLevelStart={3}>
              {value}
            </MarkdownText>
          )}
        </VStack>
      </VStack>
    </ChatComposerDrawer>
  );
}
