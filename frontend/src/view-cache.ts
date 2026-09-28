/** View identities no longer represented by the current session/pin set. */
export function removedViewKeys(
  previous: ReadonlySet<string>,
  current: ReadonlySet<string>,
): Set<string> {
  const removed = new Set<string>();
  for (const key of previous) {
    if (!current.has(key)) {
      removed.add(key);
    }
  }
  return removed;
}

/** Drop cache entries for removed views without copying an unchanged record. */
export function removeViewKeys<T>(
  record: Record<string, T>,
  keys: ReadonlySet<string>,
): Record<string, T> {
  let next: Record<string, T> | undefined;
  for (const key of keys) {
    if (!Object.prototype.hasOwnProperty.call(record, key)) {
      continue;
    }
    next ??= {...record};
    delete next[key];
  }
  return next ?? record;
}
