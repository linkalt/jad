/**
 * Inline markdown shortcuts for the chat composer.
 *
 * Edits go through `document.execCommand('insertText')` rather than the
 * composer's imperative handle. execCommand fires a real `input` event, and
 * that event is what keeps the composer's controlled value in sync;
 * `insertTextAtCursor` inside ChatComposerInput writes to the DOM directly and
 * fires nothing, so using the handle here would desync the value.
 */

/** Modifier+key → inline marker pair. */
export const INLINE_MARKERS: Record<string, string> = {
  b: '**',
  i: '*',
  e: '`',
};

/** Keys handled outside the generic marker path, because they need two slots. */
export const LINK_KEY = 'k';

export function findEditable(root: HTMLElement | null): HTMLElement | null {
  return root?.querySelector<HTMLElement>('[contenteditable="true"]') ?? null;
}

/** The current selection, but only when it sits inside `editable`. */
function selectionIn(editable: HTMLElement): Selection | null {
  const selection = window.getSelection();
  if (!selection || selection.rangeCount === 0) {
    return null;
  }
  const range = selection.getRangeAt(0);
  return editable.contains(range.commonAncestorContainer) ? selection : null;
}

/** Shrinks the caret back by `count` characters within its own text node. */
function nudgeCaretBack(editable: HTMLElement, count: number): void {
  const selection = window.getSelection();
  if (!selection || selection.rangeCount === 0) {
    return;
  }
  const range = selection.getRangeAt(0);
  const node = range.startContainer;
  if (node.nodeType !== Node.TEXT_NODE || !editable.contains(node)) {
    return;
  }
  const caret = document.createRange();
  caret.setStart(node, Math.max(0, range.startOffset - count));
  caret.collapse(true);
  selection.removeAllRanges();
  selection.addRange(caret);
}

function insert(editable: HTMLElement, text: string, caretBack = 0): void {
  document.execCommand('insertText', false, text);
  if (caretBack > 0) {
    nudgeCaretBack(editable, caretBack);
  }
}

/**
 * Wraps the selection in `marker`, unwraps it when it is already wrapped, or
 * inserts an empty marker pair with the caret between the markers.
 */
export function applyInlineMarker(editable: HTMLElement, marker: string): void {
  const selection = selectionIn(editable);
  if (!selection) {
    return;
  }
  const selected = selection.toString();
  if (selected.length === 0) {
    insert(editable, marker + marker, marker.length);
    return;
  }
  const isWrapped =
    selected.length > marker.length * 2 &&
    selected.startsWith(marker) &&
    selected.endsWith(marker);
  if (isWrapped) {
    insert(editable, selected.slice(marker.length, selected.length - marker.length));
    return;
  }
  insert(editable, `${marker}${selected}${marker}`);
}

/**
 * Turns the selection into a markdown link. With text selected the caret lands
 * in the empty destination; without a selection it lands in the empty label.
 */
export function applyLinkMarker(editable: HTMLElement): void {
  const selection = selectionIn(editable);
  if (!selection) {
    return;
  }
  const selected = selection.toString();
  if (selected.length === 0) {
    insert(editable, '[]()', ']()'.length);
    return;
  }
  insert(editable, `[${selected}]()`, 1);
}
