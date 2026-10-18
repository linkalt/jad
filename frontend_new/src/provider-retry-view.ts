/**
 * @file provider-retry-view.ts
 * @input The `provider_retry` run-stream event: a retry the run is waiting on, or null
 * @output A validated wait, the seconds left in it, and the notice wording
 * @position Pure model behind the "retrying the provider" notice in the running area
 *
 * While a provider request fails and is retried, the run is silent for seconds. The
 * server says so with one event per wait and one that clears it; this file only reads
 * those and words them. The countdown counts down to `resume_at`, the instant the
 * server's loop stops sleeping, so it needs no clock agreement beyond the browser's own.
 */

export interface ProviderRetryWait {
  attempt: number;
  maxRetries: number;
  /** The provider's HTTP status, or null for a connection failure. */
  status: number | null;
  waitMs: number;
  /** When the wait ends, as epoch milliseconds. */
  resumeAtMs: number;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function isCount(value: unknown, minimum: number): value is number {
  return typeof value === 'number' && Number.isSafeInteger(value) && value >= minimum;
}

/**
 * Reads the `waiting` member of a `provider_retry` event: `undefined` clears the
 * notice (the server sent null) and a malformed value is also treated as a clear, so
 * a bad event can never leave a notice on screen.
 */
export function parseProviderRetryWait(value: unknown): ProviderRetryWait | undefined {
  if (!isRecord(value)) return undefined;
  const {attempt, max_retries: maxRetries, status, wait_ms: waitMs, resume_at: resumeAt} = value;
  if (!isCount(attempt, 1) || !isCount(maxRetries, 1) || attempt > maxRetries) return undefined;
  if (status !== null && !isCount(status, 100)) return undefined;
  if (!isCount(waitMs, 0) || typeof resumeAt !== 'string') return undefined;
  const resumeAtMs = Date.parse(resumeAt);
  if (!Number.isFinite(resumeAtMs)) return undefined;
  return {attempt, maxRetries, status, waitMs, resumeAtMs};
}

/** Whole seconds left in the wait at `nowMs`, rounded up so it never shows 0 early. */
export function secondsLeft(wait: ProviderRetryWait, nowMs: number): number {
  return Math.max(0, Math.ceil((wait.resumeAtMs - nowMs) / 1000));
}

/** What went wrong, in the words a user would use. */
export function providerRetryReason(status: number | null): string {
  if (status === null) return 'The connection to the provider failed';
  if (status === 429) return 'The provider is rate limiting requests (429)';
  if (status === 408) return 'The provider request timed out (408)';
  if (status === 409) return 'The provider reported a conflict (409)';
  if (status >= 500) return `The provider had an error (${status})`;
  return `The provider request failed (${status})`;
}

/** The notice line: the reason, the countdown and which retry this is. */
export function providerRetryNotice(wait: ProviderRetryWait, nowMs: number): string {
  const seconds = secondsLeft(wait, nowMs);
  const when = seconds === 0 ? 'Retrying now' : `Retrying in ${seconds} s`;
  return `${providerRetryReason(wait.status)}. ${when} (retry ${wait.attempt} of ${wait.maxRetries}).`;
}
