import {useEffect, useRef, type ReactNode} from 'react';
import * as stylex from '@stylexjs/stylex';
import {Card, Divider, HStack, Icon, IconButton, Text, VStack} from '@astryxdesign/core';
import {ResizeHandle, type ResizableRegion} from '@astryxdesign/core/Resizable';
import appConfig from '../config.json';
import {APP_WINDOW_Z_INDEX, PANE_PANEL_Z_INDEX} from '../panel-layers';

const styles = stylex.create({
  startEdge: {
    position: 'absolute',
    insetBlockStart: 0,
    insetBlockEnd: 0,
    insetInlineStart: 0,
  },
  endEdge: {
    position: 'absolute',
    insetBlockStart: 0,
    insetBlockEnd: 0,
    insetInlineEnd: 0,
  },
  /**
   * The right window of the whole app, as the single-pane app has it: pinned to
   * the viewport's right edge instead of to the pane's. One conversation keeps
   * the centred lane, so a pane-anchored panel would put its scrollbar in the
   * middle of the screen; the window edge is where that scrollbar belongs. It
   * starts below the pane's title strip, whose bottom edge ViewPane publishes
   * (edgePanelTop in panel-layers.ts), and at the window top without a strip.
   */
  viewportEndEdge: {
    position: 'fixed',
    insetBlockStart: 'var(--pane-panel-top, 0px)',
    insetBlockEnd: 0,
    insetInlineEnd: 0,
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
  /**
   * Pin an `end` panel to the viewport's right edge instead of its pane's.
   *
   * With one conversation on screen the lane is centred, so a pane-anchored
   * panel would keep its scrollbar in the middle of the window. A single pane
   * therefore anchors its panel to the window — the right window of the
   * single-pane app — which is also the edge its scrollbar belongs on.
   */
  /**
   * Which window this panel belongs to, and therefore what it may cover.
   *
   * `app` (the default) is the app's left window: it floats above every pane,
   * including their title strips. `pane` is a panel anchored inside one pane
   * (the run inspector, the prompt library): it sits under that pane's title
   * strip so the strip's label and buttons stay clickable while it is open.
   */
  layer?: 'app' | 'pane';
  isEdgeFixed?: boolean;
  onClose: () => void;
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
 * the run inspector and the prompt library fill a pane's own right edge, while
 * session search, the running configuration and the two activity panels all
 * share the app's left window.
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
  layer = 'app',
  isEdgeFixed = false,
  onClose,
  footer,
  children,
}: FloatingPanelProps) {
  const accessibleName = label.toLowerCase();

  // The latest callback lives in a ref so the document listener can be
  // registered once per mount. Re-registering on every render is not just
  // wasteful: with two panels open, the first panel's close re-renders and would
  // remove the other panel's listener mid-dispatch, so the second panel would
  // survive the very click that dismissed the first.
  const onCloseRef = useRef(onClose);
  useEffect(() => {
    onCloseRef.current = onClose;
  }, [onClose]);
  // The pane this panel floats over, so a click in a *different* pane leaves it
  // alone: each pane's panel is its own, and working in one pane is not a
  // request to close the panel of another.
  const rootRef = useRef<HTMLDivElement | null>(null);

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
      if (target.closest('[data-floating-panel], [data-panel-toggle]')) {
        return;
      }
      const targetScope = target.closest('[data-pane-scope]');
      const ownScope = rootRef.current?.closest('[data-pane-scope]') ?? null;
      if (targetScope !== null && ownScope !== null && targetScope !== ownScope) {
        return;
      }
      onCloseRef.current();
    };
    document.addEventListener('pointerdown', handlePointerDown);
    return () => document.removeEventListener('pointerdown', handlePointerDown);
  }, []);

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
      ref={rootRef}
      data-floating-panel=""
      align="stretch"
      gap={0}
      width={region.size}
      // A fixed panel is sized by its two insets; 100% would be a full viewport
      // height pushed down by the strip, and run off the window's bottom.
      height={side === 'end' && isEdgeFixed ? undefined : '100%'}
      maxWidth="100%"
      role="complementary"
      aria-label={label}
      style={{zIndex: layer === 'app' ? APP_WINDOW_Z_INDEX : PANE_PANEL_Z_INDEX}}
      xstyle={
        side === 'start'
          ? styles.startEdge
          : isEdgeFixed
            ? styles.viewportEndEdge
            : styles.endEdge
      }
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
