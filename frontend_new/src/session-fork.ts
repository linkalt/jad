/**
 * @file session-fork.ts
 * @input A session reference and the time of the fork
 * @output The reference of the new session a fork creates
 * @position Shared by the transcript's fork button and its tests
 */

/** The longest session id the server accepts (remote.SessionPattern: 1 + 127 characters). */
const MAX_SESSION_ID = 128;

/**
 * The new session a fork of `sourceRef` is created as: the same root, the source id with the
 * configured suffix and a base-36 stamp of `now`, so two forks never share a name. A source id
 * too long for the suffix is cut, keeping the result a valid id.
 */
export function forkSessionRef(sourceRef: string, suffix: string, now: number): string {
  const colon = sourceRef.indexOf(':');
  const root = colon < 0 ? '' : sourceRef.slice(0, colon + 1);
  const id = colon < 0 ? sourceRef : sourceRef.slice(colon + 1);
  const tail = `${suffix}${Math.max(0, Math.floor(now)).toString(36)}`;
  const base = id.slice(0, Math.max(1, MAX_SESSION_ID - tail.length));
  return `${root}${base}${tail}`;
}
