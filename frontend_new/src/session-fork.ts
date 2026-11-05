/**
 * @file session-fork.ts
 * @input A session reference or profile id and the time of the fork
 * @output The name of the new session or profile a fork creates, and its check
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

/** A transcript message id of the live history (not the compacted one) that starts a turn. */
const LIVE_USER_MESSAGE_ID = /^session-user-o(0|[1-9][0-9]*)$/;

/**
 * The message a fork names. A fork after an answer's turn names the user message that started
 * the turn: its id is the transcript offset, unique, where an answer's id is the provider's
 * response id, which a provider may repeat. Without such a message before it, the answer
 * itself.
 */
export function forkPointMessage<M extends {id: string; sender: string}>(messages: readonly M[], message: M, keepTurn: boolean): M {
  if (!keepTurn || message.sender !== 'assistant') return message;
  const index = messages.indexOf(message);
  for (let at = index - 1; at >= 0; at--) {
    const candidate = messages[at];
    if (candidate.sender === 'user') return LIVE_USER_MESSAGE_ID.test(candidate.id) ? candidate : message;
  }
  return message;
}

/** The longest profile name the server accepts for a fork. */
const MAX_PROFILE_NAME = 200;

/** A catalog identifier: letters, digits, '-', '_' and '.', starting with a letter or digit. */
const IDENTIFIER = /^[A-Za-z0-9][A-Za-z0-9._-]*$/;

/**
 * The name a fork of profile `sourceProfile` (`<category>/<name>`) proposes for the new
 * profile: the source name with the suffix and a base-36 stamp of `now`, in the same category.
 */
export function forkProfileName(sourceProfile: string, suffix: string, now: number): string {
  const slash = sourceProfile.indexOf('/');
  const name = slash < 0 ? sourceProfile : sourceProfile.slice(slash + 1);
  const tail = `${suffix}${Math.max(0, Math.floor(now)).toString(36)}`;
  return `${name.slice(0, Math.max(1, MAX_PROFILE_NAME - tail.length))}${tail}`;
}

/** Why `name` cannot name a new profile or session, or null when it can. */
export function forkNameProblem(name: string, kind: 'profile' | 'session'): string | null {
  const limit = kind === 'profile' ? MAX_PROFILE_NAME : MAX_SESSION_ID;
  if (name === '') return `Enter a ${kind} name.`;
  if (name.length > limit) return `At most ${limit} characters.`;
  if (!IDENTIFIER.test(name)) return 'Letters, digits, - _ and . only, starting with a letter or digit.';
  return null;
}

/** The session reference `name` makes in the root of `sourceRef`. */
export function sessionRefInRootOf(sourceRef: string, name: string): string {
  const colon = sourceRef.indexOf(':');
  return colon < 0 ? name : `${sourceRef.slice(0, colon + 1)}${name}`;
}

/** The session id of a reference, without its root. */
export function sessionIdOf(sessionRef: string): string {
  const colon = sessionRef.indexOf(':');
  return colon < 0 ? sessionRef : sessionRef.slice(colon + 1);
}
