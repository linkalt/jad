/**
 * Turning a text selection in the conversation into a composer draft.
 *
 * The format is a Markdown blockquote: every line of the selection gets a `> `
 * marker, so the text reads as a quotation in the message the user sends and
 * the agent can tell the quoted material from the instruction that follows it.
 * A blank line inside the selection keeps the marker alone (`>`), which is what
 * keeps the quote one block instead of two.
 *
 * The draft ends with a line break. The composer puts the caret at the end of
 * whatever it is given, and without that break the reply would begin inside the
 * quote, on the last quoted line.
 *
 * This module is pure on purpose: the DOM side — in `App.tsx` — decides *what*
 * may be quoted, this one decides what the quote looks like.
 */

/** One `> ` per quoted line. */
const QUOTE_MARKER = '> ';

/** A line that was blank in the selection keeps the marker on its own. */
const BLANK_LINE_IN_QUOTE = '>';

/** Marks where the quote stops being the whole selection. */
const TRUNCATION_MARKER = '…[truncated]';

/**
 * What the browser knows about a selection that the quote rules care about.
 *
 * Both ends of the selection are checked: a selection that starts in the
 * agent's answer and ends in a tool document, or in the user's own message,
 * would quote timestamps, buttons and raw tool output into the draft.
 */
export interface SelectionFacts {
  /** The selection's text, as the browser reports it. */
  text: string;
  /** Whether both ends sit in an assistant message. */
  isInsideAssistantMessage: boolean;
  /** Whether either end sits in a tool call row or its result detail. */
  isInsideToolOutput: boolean;
}

export interface Quote {
  /** The blockquote itself, without the trailing line break the draft gets. */
  quote: string;
  /** Characters the selection held, before any cut. */
  characters: number;
  /** Whether the quote was cut to fit its budget. */
  isTruncated: boolean;
}

/** Whether a selection should offer the quote action at all. */
export function shouldOfferQuote(facts: SelectionFacts): boolean {
  return (
    facts.text.trim().length > 0 &&
    facts.isInsideAssistantMessage &&
    !facts.isInsideToolOutput
  );
}

/** The blockquote for a selection, or null when there is nothing to quote. */
export function quoteSelection(raw: string, maxChars: number): Quote | null {
  const text = normalizeSelection(raw);
  if (text.length === 0 || maxChars <= 0) {
    return null;
  }
  const isTruncated = text.length > maxChars;
  const body = isTruncated ? `${text.slice(0, maxChars)}${TRUNCATION_MARKER}` : text;
  const quote = body
    .split('\n')
    .map(line => (line.trim().length === 0 ? BLANK_LINE_IN_QUOTE : `${QUOTE_MARKER}${line}`))
    .join('\n');
  return {quote, characters: text.length, isTruncated};
}

/** The draft the composer receives: the quote, then the line the reply starts on. */
export function quoteDraftText(quote: Quote): string {
  return `${quote.quote}\n`;
}

/** The draft for a selection, or null when the selection may not be quoted. */
export function quoteDraftForSelection(
  facts: SelectionFacts,
  maxChars: number,
): string | null {
  if (!shouldOfferQuote(facts)) {
    return null;
  }
  const quote = quoteSelection(facts.text, maxChars);
  return quote === null ? null : quoteDraftText(quote);
}

/**
 * A dragged selection arrives with the whitespace it crossed: trailing spaces
 * on every line, runs of blank lines where paragraphs met, and the indentation
 * the text was rendered with. Indentation is kept — inside a quote it is often
 * the code's own layout — while trailing space and blank-line runs are not
 * worth carrying into the message.
 */
function normalizeSelection(raw: string): string {
  return raw
    .replace(/\r\n?/g, '\n')
    .split('\n')
    .map(line => line.replace(/[ \t]+$/, ''))
    .join('\n')
    .replace(/\n{3,}/g, '\n\n')
    .trim();
}
