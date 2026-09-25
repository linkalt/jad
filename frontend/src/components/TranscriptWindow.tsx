import {useCallback, useLayoutEffect, useRef, useState} from 'react';
import {Button, Icon, Text, VStack} from '@astryxdesign/core';
import appConfig from '../config.json';
import {
  captureGrowthPosition,
  findScrollParent,
  holdGrowthPosition,
  revealScrollOptions,
  type GrowthSnapshot,
} from './scroll-position';

/**
 * The transcript scroller belongs to ChatLayout, so findScrollParent discovers
 * it at reveal time instead of threading it down through the list.
 */

// Older rows are inserted above the reader, so the position is held by the
// distance from the bottom rather than by a control that may unmount.
const REVEAL_MESSAGES_SCROLL_OPTIONS = revealScrollOptions('messages', appConfig);

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

  const growthRef = useRef<GrowthSnapshot | null>(null);

  const revealEarlier = useCallback(
    (node: HTMLElement | null) => {
      growthRef.current = captureGrowthPosition(node === null ? null : findScrollParent(node));
      setRevealedBySession(current => ({
        ...current,
        [sessionId]: (current[sessionId] ?? 0) + pageSize,
      }));
    },
    [pageSize, sessionId],
  );

  // Older rows are inserted above the viewport, which would otherwise shove the
  // content the user was reading off screen. The window holds the distance from
  // the bottom until the inserted rows stop changing size, so the reveal lands
  // the same way whether the engine finishes its layout in this frame or a
  // later one.
  useLayoutEffect(() => {
    const snapshot = growthRef.current;
    growthRef.current = null;
    holdGrowthPosition(snapshot, REVEAL_MESSAGES_SCROLL_OPTIONS);
  }, [revealed]);

  return {visibleMessages, hiddenCount, revealEarlier};
}

export function EarlierMessagesControl({
  hiddenCount,
  hasMore,
  onReveal,
}: {
  hiddenCount: number;
  /** Older turns are on the server and still have to be fetched. */
  hasMore?: boolean;
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
        {hiddenCount > 0
          ? `${hiddenCount} older ${hiddenCount === 1 ? 'message is' : 'messages are'} not shown`
          : hasMore
            ? 'Older turns are stored on the server'
            : ''}
      </Text>
    </VStack>
  );
}
