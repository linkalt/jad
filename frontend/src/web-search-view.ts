import type {WebUiCitation, WebUiSearchPart} from './api';

export type SearchRowStatus = 'running' | 'complete' | 'error';

/**
 * Maps the provider's web_search_call state to a tool-row status. An empty or
 * unknown state counts as complete: a stored transcript only keeps finished
 * searches, and a row that spins forever is worse than one that is not exact.
 */
export function searchRowStatus(status: string | undefined): SearchRowStatus {
  if (status === 'in_progress' || status === 'searching') {
    return 'running';
  }
  if (status === 'failed' || status === 'incomplete') {
    return 'error';
  }
  return 'complete';
}

/** The text shown next to the tool name: what was searched or opened. */
export function searchRowTarget(part: WebUiSearchPart): string {
  return part.query ?? '';
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
