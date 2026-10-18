/**
 * Structural equality for JSON-shaped values (primitives, arrays, plain objects).
 *
 * Needed because the session poll rebuilds every snapshot object from a fresh
 * fetch, so structurally identical payloads always arrive as new identities.
 * Comparing by identity would report "changed" on every tick and force a full
 * transcript re-render; this lets callers keep the previous value instead.
 */
export function deepEqual(a: unknown, b: unknown): boolean {
  if (a === b) {
    return true;
  }
  if (typeof a !== typeof b) {
    return false;
  }
  if (typeof a === 'number') {
    // The only numbers that are not `===` to themselves.
    return Number.isNaN(a) && Number.isNaN(b);
  }
  if (a === null || b === null || typeof a !== 'object') {
    return false;
  }

  if (Array.isArray(a) || Array.isArray(b)) {
    if (!Array.isArray(a) || !Array.isArray(b) || a.length !== b.length) {
      return false;
    }
    return a.every((item, index) => deepEqual(item, b[index]));
  }

  const left = a as Record<string, unknown>;
  const right = b as Record<string, unknown>;
  const leftKeys = Object.keys(left);
  if (leftKeys.length !== Object.keys(right).length) {
    return false;
  }
  return leftKeys.every(
    key => Object.prototype.hasOwnProperty.call(right, key) && deepEqual(left[key], right[key]),
  );
}
