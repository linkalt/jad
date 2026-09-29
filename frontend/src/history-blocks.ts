/**
 * The history blocks: turns a compaction removed, folded from the backup.
 *
 * A block belongs to a view, not to the page. Two rows of one session are two
 * conversations — each with its own transcript and its own backup — so keeping
 * a single block for whichever view was on screen dropped the block of the view
 * being left behind. That loss was invisible while every switch refetched the
 * view, but a view whose transcript refresh is skipped (a run this page is
 * streaming owns it) had no way back: it rendered its live tail alone, with no
 * history, until a reload. Every view keeps its own block here instead — and a
 * view a stream owns can be asked directly (`historyEtagFromHeader` folds the
 * HTTP header into the identity a snapshot would publish), so one lookup
 * answers for as long as the run lasts.
 *
 * Everything in this module is pure; App owns the state, this owns the rules.
 */

export type HistoryBlock<Message> = {
  /**
   * The identity the server published for this block — the same value a snapshot
   * restates — or `null` when the server says the view has none. A record with
   * `null` and no messages is the answer "there is nothing here": it keeps a
   * skipped transcript refresh from asking again on every poll.
   */
  etag: string | null;
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
  if (current !== undefined && current.etag === block.etag) {
    if (current.messages === block.messages) {
      return blocks;
    }
    // Two empty blocks are the same answer — "this view has none" — and a fresh
    // empty array would re-render the view for nothing.
    if (current.messages.length === 0 && block.messages.length === 0) {
      return blocks;
    }
  }
  return {...blocks, [uiId]: block};
}

/**
 * The identity a snapshot publishes, from the HTTP `ETag` a direct fetch of the
 * block answered.
 *
 * The header is quoted — `"abc"`, or `W/"abc"` when weak — while the snapshot
 * publishes the bare hash, and the two are only ever compared to each other, so
 * the header form is reduced to the snapshot's here, where the copy is created.
 */
export function historyEtagFromHeader(header: string | undefined): string | null {
  if (header === undefined) {
    return null;
  }
  const value = header.trim().replace(/^W\//, '').trim();
  const quoted = value.length >= 2 && value.startsWith('"') && value.endsWith('"');
  const etag = quoted ? value.slice(1, -1) : value;
  return etag.length === 0 ? null : etag;
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
