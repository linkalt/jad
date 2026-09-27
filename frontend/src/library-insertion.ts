export function promptNeedsBadge(
  textLength: number,
  enabled: boolean,
  threshold: number,
): boolean {
  return enabled && textLength > threshold;
}

/**
 * What the composer shows at its very end — the only thing an insert has to
 * react to. Deriving it from the draft's *serialized* text is not possible: a
 * badge serializes as the whole prompt, and library files end with a newline,
 * so a trailing badge reads as "already ends with a line break" while its line
 * is still open. The next badge then shares that line, which is what the badge
 * insert exists to prevent.
 */
export type ComposerTail = 'empty' | 'line-break' | 'content';

/** A composer child node, reduced to what decides where an insert lands. */
export type ComposerTailNode = {
  kind: 'br' | 'text' | 'token';
  text: string;
};

export interface ComposerInsertionPlan {
  /** Text inserted before the payload. */
  before: string;
  /**
   * Whether the payload's line is ended, so the caret waits on the line below
   * it. A badge is a chip the user must be able to type underneath; visible
   * text ends its own line by being text.
   */
  endsLine: boolean;
  /** Whether the draft holds nothing but a stray line break the insert replaces. */
  replacesDraft: boolean;
}

export function composerTail(
  lastNode: ComposerTailNode | null,
  hasContent: boolean,
): ComposerTail {
  // Clearing the composer leaves the browser's placeholder break behind; that is
  // an empty draft, not a draft that already ends on a new line.
  if (!hasContent) return 'empty';
  if (lastNode === null) return 'content';
  if (lastNode.kind === 'br') return 'line-break';
  // A badge is never a line break, whatever its value ends with.
  if (lastNode.kind === 'token') return 'content';
  if (lastNode.text.endsWith('\n')) return 'line-break';
  // The non-breaking space glued after a badge is a spacer, not a line break.
  return 'content';
}

/**
 * A badge takes a line of its own and leaves the caret on the line below it, so
 * it cannot end up sharing a line with whatever is typed next.
 */
export function composerBadgeInsertionPlan(tail: ComposerTail): ComposerInsertionPlan {
  return {
    before: tail === 'content' ? '\n' : '',
    endsLine: true,
    replacesDraft: tail === 'empty',
  };
}

function separatorForLineBreaks(existing: string, minimumLineBreaks: number): string {
  if (existing.trim().length === 0) return '';
  const trailingLineBreaks = /\n*$/.exec(existing)?.[0].length ?? 0;
  return '\n'.repeat(Math.max(0, minimumLineBreaks - trailingLineBreaks));
}

export function composerInsertionSeparator(existing: string): string {
  return separatorForLineBreaks(existing, 2);
}

/** Text keeps the blank-line separator it has always had; the text ends its own line. */
export function composerTextInsertionPlan(
  existing: string,
  tail: ComposerTail,
): ComposerInsertionPlan {
  return {
    before: composerInsertionSeparator(existing),
    endsLine: false,
    replacesDraft: tail === 'empty',
  };
}

/**
 * Whether the text between the caret and the start of its node is only spacer.
 *
 * A badge cannot hold a caret, so every position the browser leaves beside one is
 * glue (the non-breaking space after it) or break space (the line the badge's own
 * break opened). Anything else means the caret is inside the user's own text,
 * which is none of a badge's business.
 */
export function composerCaretAfterGlue(beforeCaret: string): boolean {
  return /^[\u00A0\n]*$/.test(beforeCaret);
}

/** What sits between a badge and the start of its line. */
export interface ComposerLineBefore {
  /** Visible text after the last line break in front of the badge. */
  text: string;
  /** Whether another inline element — a badge — sits on that line. */
  hasInlineBefore: boolean;
  /** Whether a line break separates the badge from everything before it. */
  hasBreak: boolean;
}

/**
 * Whether a badge still shares its line with something.
 *
 * Tokens are `contenteditable="false"`, so the browser never places a caret
 * before one: a line break typed next to a badge can only land *after* it, and
 * the badge itself never moves. Shift+Enter is therefore handled by the composer,
 * which puts the break in front of the badge — while it still shares its line.
 */
export function composerBadgeSharesLine(before: ComposerLineBefore): boolean {
  return before.hasInlineBefore || before.text.trim().length > 0;
}

/**
 * Whether a badge starts the draft's first line.
 *
 * A badge that was just inserted owns the first line, so Shift+Enter can still
 * move it down and make room for a message above it. Once it has a line of its
 * own below something, the badge stays where it is.
 */
export function composerBadgeStartsDraft(before: ComposerLineBefore): boolean {
  return !before.hasBreak && !before.hasInlineBefore && composerCaretAfterGlue(before.text);
}

export function composerPlaceholderForDraft(value: string, placeholder: string): string {
  return value.trim().length > 0 ? '' : placeholder;
}
