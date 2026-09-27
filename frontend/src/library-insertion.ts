export function promptNeedsBadge(
  textLength: number,
  enabled: boolean,
  threshold: number,
): boolean {
  return enabled && textLength > threshold;
}

function separatorForLineBreaks(existing: string, minimumLineBreaks: number): string {
  if (existing.trim().length === 0) return '';
  const trailingLineBreaks = /\n*$/.exec(existing)?.[0].length ?? 0;
  return '\n'.repeat(Math.max(0, minimumLineBreaks - trailingLineBreaks));
}

export function composerInsertionSeparator(existing: string): string {
  return separatorForLineBreaks(existing, 2);
}

/** Keep inline composer badges on a new line without adding a blank spacer. */
export function composerBadgeInsertionSeparator(existing: string): string {
  return separatorForLineBreaks(existing, 1);
}

export function composerPlaceholderForDraft(value: string, placeholder: string): string {
  return value.trim().length > 0 ? '' : placeholder;
}
