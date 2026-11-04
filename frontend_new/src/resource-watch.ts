/**
 * @file resource-watch.ts
 * @input The events route's answers: MCP resource change notifications and subscriptions
 * @output The parsed answer, the merged event list a panel shows, and whether a resource is watched
 * @position Between the events route in api.ts and the resource watch panel
 */

/** One notification a server sent: a watched resource changed ("updated") or its list changed ("list_changed"). */
export interface MCPResourceEvent {
  seq: number;
  server: string;
  kind: string;
  uri?: string;
  at: string;
}

export interface MCPResourceSubscription {
  server: string;
  uri: string;
}

/** The events after the asked seq, the seq to ask after next, and the subscriptions held now. */
export interface MCPResourceEvents {
  events: MCPResourceEvent[];
  latest: number;
  subscriptions: MCPResourceSubscription[];
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function isEvent(value: unknown): value is MCPResourceEvent {
  return (
    isRecord(value) &&
    typeof value.seq === 'number' &&
    typeof value.server === 'string' &&
    typeof value.kind === 'string' &&
    (value.uri === undefined || typeof value.uri === 'string') &&
    typeof value.at === 'string'
  );
}

function isSubscription(value: unknown): value is MCPResourceSubscription {
  return isRecord(value) && typeof value.server === 'string' && typeof value.uri === 'string';
}

/** The events route's answer, or null when it is not one. */
export function parseResourceEvents(value: unknown): MCPResourceEvents | null {
  if (
    !isRecord(value) ||
    !Array.isArray(value.events) ||
    !value.events.every(isEvent) ||
    typeof value.latest !== 'number' ||
    !Array.isArray(value.subscriptions) ||
    !value.subscriptions.every(isSubscription)
  ) {
    return null;
  }
  return {
    events: value.events.map(event => ({
      seq: event.seq,
      server: event.server,
      kind: event.kind,
      at: event.at,
      ...(event.uri === undefined ? {} : {uri: event.uri}),
    })),
    latest: value.latest,
    subscriptions: value.subscriptions.map(({server, uri}) => ({server, uri})),
  };
}

/**
 * The events a panel shows after a poll: the new ones first, newest on top, each seq once,
 * at most `max`. A `latest` below the newest seq held means the server started its log
 * again (a restart), so the old list goes.
 */
export function mergeResourceEvents(
  held: readonly MCPResourceEvent[],
  answer: MCPResourceEvents,
  max: number,
): MCPResourceEvent[] {
  const newestHeld = held.reduce((newest, event) => Math.max(newest, event.seq), 0);
  const kept = answer.latest < newestHeld ? [] : held;
  const seen = new Set(kept.map(event => event.seq));
  const fresh = answer.events.filter(event => !seen.has(event.seq)).sort((a, b) => b.seq - a.seq);
  return [...fresh, ...kept].slice(0, Math.max(0, max));
}

/** Whether the subscriptions hold the resource of `server` at `uri`. */
export function isResourceWatched(
  subscriptions: readonly MCPResourceSubscription[],
  server: string,
  uri: string,
): boolean {
  return subscriptions.some(subscription => subscription.server === server && subscription.uri === uri);
}
