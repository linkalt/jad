import {userMessageImages} from './image-attachments.ts';

/**
 * Recall of earlier user messages in the composer (ArrowUp / ArrowDown).
 *
 * The history is read from the session's own transcript, so it survives a reload
 * and is the same for every tab. The module is pure: it picks the messages and
 * decides the next draft; the composer owns the DOM and the caret.
 */

type HistoryMessage = {
  sender: string;
  text: string;
  sentImages?: readonly string[];
  sentFiles?: readonly string[];
  parts?: readonly {type: string; images?: readonly string[]; files?: readonly string[]}[];
};

/** What the composer knows about the caret when a key is pressed. */
export interface CaretEdges {
  /** Nothing is selected. */
  isCollapsed: boolean;
  /** The caret is at the very start of the draft. */
  atStart: boolean;
  /** No line break lies before the caret. */
  onFirstLine: boolean;
  /** No line break lies after the caret. */
  onLastLine: boolean;
  /** The draft holds a badge, which a recall would flatten to text. */
  hasTokens: boolean;
}

/** `index` is the history entry on show, or -1 when the draft is the user's own; `draft` is what ArrowDown returns to. */
export interface HistoryCursor {
  index: number;
  draft: string;
}

export const IDLE_HISTORY: HistoryCursor = {index: -1, draft: ''};

/**
 * The text of the newest `limit` user messages, oldest first.
 *
 * Placeholders for attachments are dropped, so an image-only message is skipped,
 * and a message that repeats the one before it is listed once.
 */
export function recentUserTexts(messages: readonly HistoryMessage[], limit: number): string[] {
  if (!Number.isFinite(limit) || limit <= 0) return [];
  const newestFirst: string[] = [];
  for (let i = messages.length - 1; i >= 0 && newestFirst.length < limit; i -= 1) {
    const message = messages[i];
    if (message.sender !== 'user') continue;
    const text = userMessageImages(message).text.trim();
    if (text === '' || text === newestFirst[newestFirst.length - 1]) continue;
    newestFirst.push(text);
  }
  return newestFirst.reverse();
}

export interface HistoryStep {
  cursor: HistoryCursor;
  text: string;
}

/**
 * The draft that ArrowUp or ArrowDown should show, or null to leave the key to the editor.
 *
 * While an entry is on show the keys walk the history from its first and last
 * line, so the caret can still move through a multi-line entry. Otherwise ArrowUp
 * recalls only from an empty draft or with the caret at the very start, and never
 * over a draft with a badge. Editing the entry on show ends the walk, because the
 * draft is then the user's own again.
 */
export function stepHistory(
  history: readonly string[],
  cursor: HistoryCursor,
  value: string,
  key: 'ArrowUp' | 'ArrowDown',
  edges: CaretEdges,
): HistoryStep | null {
  if (history.length === 0 || !edges.isCollapsed) return null;
  const isBrowsing =
    cursor.index >= 0 && cursor.index < history.length && history[cursor.index] === value;
  if (key === 'ArrowUp') {
    if (isBrowsing) {
      if (!edges.onFirstLine) return null;
      const index = Math.max(0, cursor.index - 1);
      return {cursor: {...cursor, index}, text: history[index]};
    }
    if (edges.hasTokens || (value !== '' && !edges.atStart)) return null;
    const index = history.length - 1;
    return {cursor: {index, draft: value}, text: history[index]};
  }
  if (!isBrowsing || !edges.onLastLine) return null;
  const index = cursor.index + 1;
  if (index >= history.length) return {cursor: IDLE_HISTORY, text: cursor.draft};
  return {cursor: {...cursor, index}, text: history[index]};
}
