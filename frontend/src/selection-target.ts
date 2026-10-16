import {type QuoteAnchor} from './components/SelectionMenu';
import appConfig from './config.json';
import {quoteDraftForSelection} from './selection-quote';

/**
 * The assistant's own words live in an Astryx message bubble. The row's
 * controls, the user's own messages and the tool documents all sit outside it,
 * so the quote action is offered from inside that bubble and nowhere else.
 */
const ASSISTANT_BUBBLE_SELECTOR = '.astryx-chat-message-bubble[data-sender="assistant"]';

/** Tool call rows and their result detail are the tool's output, not a reply. */
const TOOL_OUTPUT_SELECTOR = '.astryx-chat-tool-calls';

function elementOf(node: Node): Element | null {
  return node.nodeType === Node.ELEMENT_NODE ? (node as Element) : node.parentElement;
}

/**
 * The draft a selection should insert, with the place its menu belongs.
 *
 * Both ends of the range are checked: a selection that starts in the agent's
 * answer and ends in a tool document would quote raw tool text, and one that
 * reaches across messages would quote timestamps and controls into the draft.
 */
export function selectionQuoteTarget(): {draft: string; anchor: QuoteAnchor} | null {
  const selection = window.getSelection();
  if (selection === null || selection.isCollapsed || selection.rangeCount === 0) {
    return null;
  }
  const range = selection.getRangeAt(0);
  const start = elementOf(range.startContainer);
  const end = elementOf(range.endContainer);
  if (start === null || end === null) {
    return null;
  }
  const startBubble = start.closest(ASSISTANT_BUBBLE_SELECTOR);
  const endBubble = end.closest(ASSISTANT_BUBBLE_SELECTOR);
  // A triple click ends at the start of the next block, which can lie outside
  // the bubble; that is still inside the answer if no text was crossed to get there.
  let endsInside = endBubble !== null;
  if (!endsInside && startBubble !== null) {
    const beyondBubble = range.cloneRange();
    beyondBubble.setStart(startBubble, startBubble.childNodes.length);
    endsInside = beyondBubble.toString().trim() === '';
  }
  const draft = quoteDraftForSelection(
    {
      text: selection.toString(),
      isInsideAssistantMessage: startBubble !== null && endsInside,
      isInsideToolOutput:
        start.closest(TOOL_OUTPUT_SELECTOR) !== null ||
        end.closest(TOOL_OUTPUT_SELECTOR) !== null,
    },
    appConfig.selection_quote_max_chars,
  );
  if (draft === null) {
    return null;
  }
  const rect = range.getBoundingClientRect();
  const isAbove = rect.top - appConfig.selection_quote_menu_gap_px >= appConfig.selection_quote_menu_min_top_px;
  return {
    draft,
    anchor: {
      x: rect.left,
      y: isAbove
        ? rect.top - appConfig.selection_quote_menu_gap_px
        : rect.bottom + appConfig.selection_quote_menu_gap_px,
      isAbove,
    },
  };
}
