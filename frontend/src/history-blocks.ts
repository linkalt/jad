/**
 * The history blocks: turns a compaction removed, folded from the backup.
 *
 * A block belongs to a view, not to the page. Two rows of one session are two
 * conversations — each with its own transcript and its own backup — so keeping
 * a single block for whichever view was on screen dropped the block of the view
 * being left behind. That loss was invisible while every switch refetched the
 * view, but a view whose transcript refresh is skipped (a run this page is
 * streaming owns it) had no way back: it rendered its live tail alone, with no
 * history, until a reload. Every view keeps its own block here instead.
 *
 * Everything in this module is pure; App owns the state, this owns the rules.
 */

export type HistoryBlock<Message> = {
  /** The identity the server published for this block. */
  etag: string;
  messages: Message[];
};

export type HistoryBlocks<Message> = Readonly<Record<string, HistoryBlock<Message>>>;

export const EMPTY_HISTORY_BLOCKS: HistoryBlocks<never> = {};

/** The block a view holds, if any. */
export function historyBlockFor<Message>(
  blocks: HistoryBlocks<Message>,
  uiId: string,
): HistoryBlock<Message> | undefined {
  return blocks[uiId];
}

/** Stores one view's block; every other view keeps the copy it already holds. */
export function putHistoryBlock<Message>(
  blocks: HistoryBlocks<Message>,
  uiId: string,
  block: HistoryBlock<Message>,
): HistoryBlocks<Message> {
  const current = blocks[uiId];
  if (
    current !== undefined &&
    current.etag === block.etag &&
    current.messages === block.messages
  ) {
    return blocks;
  }
  return {...blocks, [uiId]: block};
}

/** Forgets one view's block, as a reset of that row does. */
export function forgetHistoryBlock<Message>(
  blocks: HistoryBlocks<Message>,
  uiId: string,
): HistoryBlocks<Message> {
  if (blocks[uiId] === undefined) {
    return blocks;
  }
  const next: Record<string, HistoryBlock<Message>> = {...blocks};
  delete next[uiId];
  return next;
}

/**
 * Forgets the blocks of views that no longer exist — the same key set every
 * other view cache is forgotten with.
 */
export function forgetHistoryBlocks<Message>(
  blocks: HistoryBlocks<Message>,
  uiIds: ReadonlySet<string>,
): HistoryBlocks<Message> {
  let next: Record<string, HistoryBlock<Message>> | undefined;
  for (const uiId of Object.keys(blocks)) {
    if (!uiIds.has(uiId)) {
      continue;
    }
    next ??= {...blocks};
    delete next[uiId];
  }
  return next ?? blocks;
}

/**
 * The turns that render ahead of a view's transcript. An absent or empty block
 * adds nothing, so the transcript renders alone.
 */
export function historyMessagesFor<Message>(
  blocks: HistoryBlocks<Message>,
  uiId: string,
): readonly Message[] | undefined {
  const block = blocks[uiId];
  return block === undefined || block.messages.length === 0 ? undefined : block.messages;
}
