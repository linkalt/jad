/** The catalog fields the library search matches; panel summaries carry more. */
export type LibrarySearchItem = {
  name: string;
  description: string;
  source: string;
};

/**
 * Filters catalog entries for the library bar: case-insensitive, and every
 * whitespace-separated term has to appear somewhere in the name, the
 * description or the source path. An empty query keeps the list untouched.
 */
export function filterLibraryItems<T extends LibrarySearchItem>(
  items: readonly T[],
  query: string,
): T[] {
  const terms = query.toLowerCase().split(/\s+/).filter(term => term.length > 0);
  if (terms.length === 0) return [...items];
  return items.filter(item => {
    const haystack = `${item.name} ${item.description} ${item.source}`.toLowerCase();
    return terms.every(term => haystack.includes(term));
  });
}
