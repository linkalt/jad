import {useEffect, useRef, type ReactNode} from 'react';
import * as stylex from '@stylexjs/stylex';
import {Card, Divider, HStack, Icon, IconButton, Text, VStack} from '@astryxdesign/core';
import {ResizeHandle, type ResizableRegion} from '@astryxdesign/core/Resizable';
import appConfig from '../config.json';

const styles = stylex.create({
  startEdge: {
    position: 'absolute',
    insetBlockStart: 0,
    insetBlockEnd: 0,
    insetInlineStart: 0,
    zIndex: 3,
  },
  endEdge: {
    position: 'absolute',
    insetBlockStart: 0,
    insetBlockEnd: 0,
    insetInlineEnd: 0,
    zIndex: 3,
  },
  card: {
    flex: 1,
    minWidth: 0,
    height: '100%',
    overflow: 'hidden',
    // A column so the scrolling viewport and an optional footer bar share the card.
    display: 'flex',
    flexDirection: 'column',
  },
  viewport: {
    flex: 1,
    minHeight: 0,
    minWidth: 0,
  },
  footer: {
    flexShrink: 0,
  },
  /**
   * The native scrollbar follows the viewport's inline direction, so `rtl` is
   * what puts a panel's scrollbar on its left edge. The content box restores
   * `ltr`, which keeps text, icons and row order exactly as they were.
   */
  scrollbarAtStartEdge: {
    direction: 'rtl',
  },
  content: {
    direction: 'ltr',
    minWidth: 0,
  },
  heading: {
    minWidth: 0,
  },
  subtitle: {
    display: 'block',
    minWidth: 0,
    maxWidth: '100%',
    overflow: 'hidden',
    textOverflow: 'ellipsis',
    whiteSpace: 'nowrap',
  },
});

export type FloatingPanelSide = 'start' | 'end';

type FloatingPanelProps = {
  /** Edge the panel is anchored to: `start` is the left edge, `end` the right. */
  side: FloatingPanelSide;
  /** Accessible name for the panel region; also drives the resize and close labels. */
  label: string;
  title: string;
  subtitle?: string;
  /** Size and drag props from `useResizable`. */
  region: ResizableRegion;
  onClose: () => void;
  /** Treat clicks on session rows (`data-session-row`) as inside, for panels that follow the active session. */
  followsSession?: boolean;
  /** Fixed bar pinned below the scrolling content, e.g. the library control bar. */
  footer?: ReactNode;
  children: ReactNode;
};

/**
 * Floating panel shell shared by every side data view.
 *
 * The panel is an overlay rather than a layout region: it is absolutely
 * positioned against the shell, spans the full height, and floats above the
 * conversation on an elevated card. It deliberately reserves no space, so an
 * open panel can cover the composer and the transcript; its own close button,
 * and clicking outside it, are the ways back.
 *
 * Dismissal is config-gated by `close_panels_on_outside_click`. Clicks landing
 * in any floating panel (`data-floating-panel`) or on a composer panel toggle
 * (`data-panel-toggle`) count as inside, so moving between a panel and its
 * toggle never dismisses anything.
 *
 * Both sides use this one shell, so a new dataset only has to supply content:
 * the run inspector fills the right edge, the prompt library the left.
 *
 * Each panel's scrollbar sits on its outer edge — the window edge — so the
 * `start` panels put it on the left and the `end` panels keep it on the right.
 */
export function FloatingPanel({
  side,
  label,
  title,
  subtitle,
  region,
  onClose,
  followsSession = false,
  footer,
  children,
}: FloatingPanelProps) {
  const accessibleName = label.toLowerCase();
  const insideSelector = followsSession
    ? '[data-floating-panel], [data-panel-toggle], [data-session-row]'
    : '[data-floating-panel], [data-panel-toggle]';

  // The latest callback lives in a ref so the document listener can be
  // registered once per mount. Re-registering on every render is not just
  // wasteful: with two panels open, the first panel's close re-renders and would
  // remove the other panel's listener mid-dispatch, so the second panel would
  // survive the very click that dismissed the first.
  const onCloseRef = useRef(onClose);
  useEffect(() => {
    onCloseRef.current = onClose;
  }, [onClose]);

  useEffect(() => {
    if (!appConfig.close_panels_on_outside_click) {
      return;
    }
    // pointerdown rather than click: a resize drag starts inside the panel and
    // can finish outside it, which would otherwise register as an outside click.
    const handlePointerDown = (event: PointerEvent) => {
      const target = event.target;
      if (!(target instanceof Element)) {
        return;
      }
      if (target.closest(insideSelector)) {
        return;
      }
      onCloseRef.current();
    };
    document.addEventListener('pointerdown', handlePointerDown);
    return () => document.removeEventListener('pointerdown', handlePointerDown);
  }, [insideSelector]);

  // Reversed when the handle sits on the panel's inner left edge, matching how
  // the conversation lane's own handles are configured.
  const resizeHandle = (
    <ResizeHandle
      direction="horizontal"
      isReversed={side === 'end'}
      hasDivider
      isAlwaysVisible
      label={`Resize ${accessibleName}`}
      resizable={region.props}
    />
  );

  return (
    <HStack
      as="aside"
      data-floating-panel=""
      align="stretch"
      gap={0}
      width={region.size}
      height="100%"
      maxWidth="100%"
      role="complementary"
      aria-label={label}
      xstyle={side === 'start' ? styles.startEdge : styles.endEdge}
    >
      {side === 'end' ? resizeHandle : null}
      <Card variant="default" elevation="med" padding={0} xstyle={styles.card}>
        <VStack
          isScrollable
          xstyle={[styles.viewport, side === 'start' && styles.scrollbarAtStartEdge]}
        >
          {/* One content box inside the viewport: the padding and spacing live
              here so the viewport itself stays a pure scroll container. */}
          <VStack gap={5} padding={4} xstyle={styles.content}>
            <HStack align="start" justify="between" gap={3}>
              <VStack gap={0.5} xstyle={styles.heading}>
                <Text type="label" weight="semibold">
                  {title}
                </Text>
                {subtitle ? (
                  <Text type="supporting" color="secondary" xstyle={styles.subtitle}>
                    {subtitle}
                  </Text>
                ) : null}
              </VStack>
              <IconButton
                label={`Hide ${accessibleName}`}
                icon={<Icon icon="close" size="sm" />}
                variant="ghost"
                size="sm"
                onClick={onClose}
              />
            </HStack>
            {children}
          </VStack>
        </VStack>
        {footer ? (
          <VStack gap={0} xstyle={styles.footer}>
            <Divider variant="subtle" />
            <HStack align="center" gap={2} paddingInline={4} paddingBlock={3}>
              {footer}
            </HStack>
          </VStack>
        ) : null}
      </Card>
      {side === 'start' ? resizeHandle : null}
    </HStack>
  );
}
