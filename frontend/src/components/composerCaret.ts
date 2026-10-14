import type {CaretEdges} from '../input-history';

/** A trailing `<br>` is the caret's placeholder line in Chromium, not a line of its own. */
function hasLineBreak(fragment: DocumentFragment): boolean {
  if (fragment.lastChild instanceof HTMLBRElement) fragment.lastChild.remove();
  return (fragment.textContent ?? '').includes('\n') || fragment.querySelector('br, div, p') !== null;
}

/**
 * Where the caret sits in the composer's editable, or null when it is not inside it.
 *
 * A line is whatever the draft holds between line breaks (a newline in a text
 * node, or a `<br>` / block the browser inserts), and a badge counts as content.
 */
export function readCaretEdges(editable: HTMLElement | null): CaretEdges | null {
  const selection = window.getSelection();
  if (editable === null || selection === null || selection.rangeCount === 0) return null;
  const range = selection.getRangeAt(0);
  if (!editable.contains(range.startContainer) || !editable.contains(range.endContainer)) {
    return null;
  }
  const before = document.createRange();
  before.selectNodeContents(editable);
  before.setEnd(range.startContainer, range.startOffset);
  const after = document.createRange();
  after.selectNodeContents(editable);
  after.setStart(range.endContainer, range.endOffset);
  const head = before.cloneContents();
  const tail = after.cloneContents();
  return {
    isCollapsed: selection.isCollapsed,
    atStart: (head.textContent ?? '') === '' && head.querySelector('[data-astryx-token], br') === null,
    onFirstLine: !hasLineBreak(head),
    onLastLine: !hasLineBreak(tail),
    hasTokens: editable.querySelector('[data-astryx-token]') !== null,
  };
}
