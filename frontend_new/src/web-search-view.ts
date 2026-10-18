import type {WebUiCitation} from './api';

export type HostedToolStatus = 'running' | 'complete' | 'error';

// The states providers report while a hosted tool is still working. Anything
// else, including a state this client has never seen, counts as finished.
const RUNNING_STATES: ReadonlySet<string> = new Set([
  'in_progress',
  'queued',
  'searching',
  'fetching',
  'generating',
  'interpreting',
  'running',
]);
const ERROR_STATES: ReadonlySet<string> = new Set(['failed', 'incomplete', 'error', 'cancelled']);

/**
 * Maps a hosted tool's provider state to a tool-row status. An empty or unknown
 * state counts as complete: a stored transcript only keeps finished items, and a
 * row that spins forever is worse than one that is not exact.
 */
export function hostedToolStatus(status: string | undefined): HostedToolStatus {
  if (status !== undefined && RUNNING_STATES.has(status)) {
    return 'running';
  }
  if (status !== undefined && ERROR_STATES.has(status)) {
    return 'error';
  }
  return 'complete';
}

/** Only http and https sources become links; anything else stays plain text. */
export function safeCitationHref(url: string): string | undefined {
  try {
    const parsed = new URL(url);
    return parsed.protocol === 'https:' || parsed.protocol === 'http:' ? parsed.href : undefined;
  } catch {
    return undefined;
  }
}

/**
 * The host of a source, without a leading "www.", or the raw text when it is not
 * a URL or has no host (javascript:, data:, mailto: and file: parse but have none).
 */
export function citationHost(url: string): string {
  try {
    const host = new URL(url).hostname.replace(/^www\./, '');
    return host.length > 0 ? host : url;
  } catch {
    return url;
  }
}

/** The link text of a source: its title, or its host when it has none. */
export function citationLabel(citation: WebUiCitation): string {
  const title = citation.title?.trim();
  return title !== undefined && title.length > 0 ? title : citationHost(citation.url);
}
