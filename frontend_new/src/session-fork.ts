/**
 * @file session-fork.ts
 * @input A session reference or profile id and the time of the fork
 * @output The name of the new session or profile a fork creates, and its check
 * @position Shared by the transcript's fork button and its tests
 */

import appConfig from './config.json' with {type: 'json'};

/**
 * The longest session id the server accepts (remote.SessionPattern: 1 + 127 characters). It
 * is the identifier format, not a limit the server lets one configure, so it stays here.
 */
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

/**
 * The answers that cannot be a fork point after their turn: those whose turn starts with a
 * user message of the compacted history. The server finds fork points in the live history
 * only, so it refuses them (404). An answer with no user message before it in the list is
 * left out: its own id may still name a live turn.
 */
export function answersOfCompactedTurns<M extends {id: string; sender: string}>(messages: readonly M[]): Set<string> {
  const compacted = new Set<string>();
  let turnIsCompacted = false;
  for (const message of messages) {
    if (message.sender === 'user') turnIsCompacted = !LIVE_USER_MESSAGE_ID.test(message.id);
    else if (message.sender === 'assistant' && turnIsCompacted) compacted.add(message.id);
  }
  return compacted;
}

/**
 * Whether a profile can be forked into a new profile (or duplicated): the server forks only
 * a catalog profile, `<category>/<name>`, and answers 400 for any other.
 */
export function isForkableProfile(profile: string | undefined): boolean {
  if (profile === undefined) return false;
  const slash = profile.indexOf('/');
  return slash > 0 && slash < profile.length - 1;
}

/**
 * The longest profile name the server accepts for a fork: the server's
 * server.fork_name_max_bytes once GET /api/server answered (setForkProfileNameMax), and
 * until then, or with an older server, config.json's fork_profile_name_max.
 */
let MAX_PROFILE_NAME: number = appConfig.fork_profile_name_max;

/** Takes the server's fork name limit; a value that is not a positive integer is ignored. */
export function setForkProfileNameMax(limit: number): void {
  if (Number.isInteger(limit) && limit >= 1) MAX_PROFILE_NAME = limit;
}

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
