/**
 * @file readme-view.ts
 * @input The JSON of GET /sessions/{id}/readme
 * @output The README the right-edge panel shows
 * @position Pure model behind the README panel
 */

/** The error the server gives for a working directory with no README; any other 404 is a real error. */
export const README_MISSING_MESSAGE = 'no README file';

/** Whether a README link leaves for another site. Any other link would navigate the app itself away. */
export function isWebLink(href: string): boolean {
  return /^https?:\/\//i.test(href);
}

export interface SessionReadme {
  name: string;
  content: string;
  /** The file was longer than the server's cap and was cut. */
  truncated: boolean;
}

/** Reads the answer; throws when it is not what the endpoint documents. */
export function parseSessionReadme(value: unknown): SessionReadme {
  if (
    typeof value !== 'object' ||
    value === null ||
    typeof (value as {name?: unknown}).name !== 'string' ||
    typeof (value as {content?: unknown}).content !== 'string' ||
    typeof (value as {truncated?: unknown}).truncated !== 'boolean'
  ) {
    throw new Error('the server returned invalid README data');
  }
  const {name, content, truncated} = value as SessionReadme;
  return {name, content, truncated};
}
