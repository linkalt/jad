import {useCallback, useLayoutEffect, useRef, useState} from 'react';
import {Button, Icon, Text, VStack} from '@astryxdesign/core';

/**
 * Nearest ancestor that can actually scroll. The transcript scroller belongs to
 * ChatLayout, so it is discovered from the DOM at reveal time instead of being
 * threaded down through the list.
 */
function findScrollParent(node: HTMLElement): HTMLElement | null {
  let current = node.parentElement;
  while (current !== null) {
    const {overflowY} = getComputedStyle(current);
    if (
      (overflowY === 'auto' || overflowY === 'scroll') &&
      current.scrollHeight > current.clientHeight
    ) {
      return current;
    }
    current = current.parentElement;
  }
  return null;
}

/**
 * Caps how much of a transcript is mounted.
 *
 * Mounting every row of a long session costs a second of blocking work — the
 * element creation and markdown parsing dominate, and no amount of memoisation
 * helps a first mount. Only the newest slice is rendered; older messages stay in
 * state and are revealed a page at a time.
 *
 * The reveal counts are remembered per session, so switching away and back does
 * not throw away what the user already unfolded.
 */
export function useTranscriptWindow<Message extends {id: string}>({
  messages,
  sessionId,
  initialCount,
  pageSize,
}: {
  messages: readonly Message[];
  sessionId: string;
  initialCount: number;
  pageSize: number;
}): {
  visibleMessages: readonly Message[];
  hiddenCount: number;
  revealEarlier: (node: HTMLElement | null) => void;
} {
  const [revealedBySession, setRevealedBySession] = useState<Record<string, number>>({});
  const revealed = revealedBySession[sessionId] ?? 0;
  // A window of zero would mount nothing at all, so keep a floor of one row.
  const visibleCount = Math.max(1, initialCount + revealed);
  const hiddenCount = Math.max(0, messages.length - visibleCount);
  const visibleMessages =
    hiddenCount > 0 ? messages.slice(messages.length - visibleCount) : messages;

  const scrollerRef = useRef<HTMLElement | null>(null);
  const anchorRef = useRef<{scrollTop: number; scrollHeight: number} | null>(null);

  const revealEarlier = useCallback(
    (node: HTMLElement | null) => {
      const scroller = node === null ? null : findScrollParent(node);
      scrollerRef.current = scroller;
      anchorRef.current =
        scroller === null
          ? null
          : {scrollTop: scroller.scrollTop, scrollHeight: scroller.scrollHeight};
      setRevealedBySession(current => ({
        ...current,
        [sessionId]: (current[sessionId] ?? 0) + pageSize,
      }));
    },
    [pageSize, sessionId],
  );

  // Older rows are inserted above the viewport, which would otherwise shove the
  // content the user was reading off screen. Browsers already compensate for that
  // on their own (scroll anchoring), so the correction has to be the *remaining*
  // difference: how much taller the scroller got, minus how much it already
  // scrolled. When the browser did the work this is a no-op.
  useLayoutEffect(() => {
    const scroller = scrollerRef.current;
    const anchor = anchorRef.current;
    scrollerRef.current = null;
    anchorRef.current = null;
    if (scroller === null || anchor === null) {
      return;
    }
    const grew = scroller.scrollHeight - anchor.scrollHeight;
    const moved = scroller.scrollTop - anchor.scrollTop;
    if (grew !== moved) {
      scroller.scrollTop = anchor.scrollTop + grew;
    }
  }, [revealed]);

  return {visibleMessages, hiddenCount, revealEarlier};
}

export function EarlierMessagesControl({
  hiddenCount,
  onReveal,
}: {
  hiddenCount: number;
  onReveal: (node: HTMLElement | null) => void;
}) {
  return (
    <VStack align="center" gap={1} width="100%">
      <Button
        label="Load earlier messages"
        variant="ghost"
        size="sm"
        icon={<Icon icon="arrowUp" size="sm" />}
        onClick={event => onReveal(event.currentTarget)}
      />
      <Text type="supporting" color="secondary">
        {hiddenCount} older {hiddenCount === 1 ? 'message is' : 'messages are'} not shown
      </Text>
    </VStack>
  );
}
