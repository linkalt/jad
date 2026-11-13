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
  // The drawer renders its children into a wrapping flex row, so a child that
  // does not ask for a width is sized by its content. A draft holding one long
  // unbreakable run of characters (a URL, a hash, an identifier) then sized the
  // whole preview past the drawer, which clips its own overflow — the "something
  // overflows" case. Asking for the width takes the drawer's, and minWidth lets
  // it shrink below its content instead of pushing past the edge.
  frame: {
    width: '100%',
    minWidth: 0,
    // The wrapping row is also a grid item with an automatic minimum width.
    // A fenced line's intrinsic width otherwise sizes that row before our
    // percentage width resolves, leaving the code scrollport outside the drawer.
    // Contain only inline sizing: keep natural height and the default renderer's
    // unwrapped, keyboard-scrollable code, but do not size ancestors from it.
    contain: 'inline-size',
  },
  body: {
    maxHeight: '40vh',
    overflowY: 'auto',
    minWidth: 0,
  },
  // The markdown element carries its own `overflow-wrap: break-word` from Astryx,
  // and `break-word` leaves the min-content width alone: a 160-character run with
  // no space in it kept the whole preview 1291px wide inside an 883px drawer even
  // once the frame asked for the width. `anywhere` is the value that also shrinks
  // the min-content width, so the run wraps inside the frame instead of widening
  // it. It has to be set on that element: the element's own rule wins over an
  // inherited value.
  content: {
    overflowWrap: 'anywhere',
  },
});

export function ComposerPreview({value}: {value: string}) {
  const isEmpty = value.trim().length === 0;
  return (
    <ChatComposerDrawer>
      <VStack gap={4} padding={4} xstyle={styles.frame}>
        <VStack gap={0.5}>
          <Heading level={2}>Preview</Heading>
          <Text type="supporting" color="secondary">
            How this message will be rendered
          </Text>
        </VStack>
        <VStack
          gap={2}
          xstyle={styles.body}
          /*
           * Astryx draws a list with `margin-inline: -8px` on the `ol`/`ul` and
           * `padding-inline: 8px` on each `li`, so the list box is 8px wider than
           * its container on each side. overflow-y here computes overflow-x to
           * `auto`, which turned that bleed into a horizontal scrollbar on every
           * draft containing a list. The same 8px as inline padding gives the
           * bleed somewhere to land, and lines the list text up with the
           * paragraphs above it.
           */
          paddingInline={2}
        >
          {isEmpty ? (
            <Text type="supporting" color="secondary">
              Nothing to preview yet.
            </Text>
          ) : (
            <MarkdownText density="compact" contentWidth="100%" headingLevelStart={3} xstyle={styles.content}>
              {value}
            </MarkdownText>
          )}
        </VStack>
      </VStack>
    </ChatComposerDrawer>
  );
}
