import OpenAI from "openai";

import type {ProviderRetrySettings} from "./config.js";

/**
 * Retry of the provider's initial request. The OpenAI SDK retries on its own, but its
 * count and delays cannot be configured per session and the Go SDK's cannot be matched
 * exactly, so both backends run this loop and switch the SDK's own retries off.
 *
 * Only the request that opens a response stream is retried. Once events have been
 * received the partial output is already shown and staged, so a failure after that
 * point is never retried here.
 */

/** The parts of an SDK error this module reads; real SDK errors satisfy it. */
interface ProviderErrorLike {
  status?: unknown;
  headers?: {get(name: string): string | null | undefined} | undefined;
}

export interface ProviderRetryHooks {
  /** Called before each wait, with the failure that caused it. */
  onRetry?: (info: {attempt: number; delayMs: number; status: number | undefined}) => void;
  /** Source of randomness for the jitter; tests pass a fixed one. */
  random?: () => number;
  /** Wait implementation; tests pass an instant one. */
  sleep?: (ms: number, signal?: AbortSignal) => Promise<void>;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null;
}

function statusOf(error: unknown): number | undefined {
  if (!isRecord(error)) return undefined;
  const status = (error as ProviderErrorLike).status;
  return typeof status === "number" && Number.isInteger(status) ? status : undefined;
}

function headerOf(error: unknown, name: string): string | undefined {
  if (!isRecord(error)) return undefined;
  const headers = (error as ProviderErrorLike).headers;
  if (headers === undefined || headers === null || typeof headers.get !== "function") return undefined;
  const value = headers.get(name);
  return typeof value === "string" && value.length > 0 ? value : undefined;
}

/**
 * True for a failure worth another attempt: a provider status the SDKs also retry
 * (408, 409, 429, 5xx), a connection failure or timeout (no status), and whatever the
 * provider's `x-should-retry` header says. A stop by the user is never retried.
 */
export function isRetryableProviderError(error: unknown): boolean {
  // Every SDK error has name "Error", so the class decides, not the name. The SDK's
  // timeout error extends its connection error; a stop by the user is a separate class.
  if (error instanceof OpenAI.APIUserAbortError) return false;
  if (!(error instanceof OpenAI.APIError)) return false;
  const status = statusOf(error);
  if (status === undefined) {
    // Connection failures and timeouts carry no status. A stream error event does not
    // either, but it is not a connection error, so it is not retried.
    return error instanceof OpenAI.APIConnectionError;
  }
  const hint = headerOf(error, "x-should-retry");
  if (hint === "true") return true;
  if (hint === "false") return false;
  return status === 408 || status === 409 || status === 429 || status >= 500;
}

/** The wait the provider asked for in `Retry-After-Ms` or `Retry-After`, in ms. */
export function retryAfterMs(error: unknown, now: number = Date.now()): number | undefined {
  const millis = headerOf(error, "retry-after-ms");
  if (millis !== undefined) {
    const parsed = Number.parseFloat(millis);
    if (Number.isFinite(parsed) && parsed >= 0) return parsed;
  }
  const after = headerOf(error, "retry-after");
  if (after === undefined) return undefined;
  const seconds = Number.parseFloat(after);
  if (Number.isFinite(seconds) && seconds >= 0) return seconds * 1000;
  const date = Date.parse(after);
  return Number.isNaN(date) ? undefined : Math.max(0, date - now);
}

/**
 * The wait before retry number `attempt` (0 for the first retry). A `Retry-After` the
 * provider sent wins over the backoff, but never exceeds `max_delay_ms`. The backoff
 * doubles from `initial_delay_ms` up to `max_delay_ms`, minus up to a quarter as jitter.
 */
export function providerRetryDelayMs(
  settings: ProviderRetrySettings,
  attempt: number,
  error: unknown,
  random: () => number = Math.random,
  now: number = Date.now(),
): number {
  const requested = retryAfterMs(error, now);
  if (requested !== undefined) return Math.min(requested, settings.max_delay_ms);
  const backoff = Math.min(settings.initial_delay_ms * 2 ** attempt, settings.max_delay_ms);
  return Math.floor(backoff * (1 - random() * 0.25));
}

function abortableSleep(ms: number, signal?: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal?.aborted) {
      reject(signal.reason);
      return;
    }
    const timer = setTimeout(() => {
      signal?.removeEventListener("abort", onAbort);
      resolve();
    }, ms);
    const onAbort = () => {
      clearTimeout(timer);
      reject(signal?.reason);
    };
    signal?.addEventListener("abort", onAbort, {once: true});
  });
}

/**
 * Runs `attempt` and, while it fails with a retryable error and retries remain, waits
 * and runs it again. The last error is thrown unchanged, so callers report it exactly
 * as before. A stop during a wait ends the loop with the stop's reason.
 */
export async function withProviderRetry<T>(
  settings: ProviderRetrySettings,
  attempt: () => Promise<T>,
  signal?: AbortSignal,
  hooks: ProviderRetryHooks = {},
): Promise<T> {
  const sleep = hooks.sleep ?? abortableSleep;
  for (let retry = 0; ; retry += 1) {
    try {
      return await attempt();
    } catch (error) {
      if (retry >= settings.max_retries || signal?.aborted === true || !isRetryableProviderError(error)) {
        throw error;
      }
      const delayMs = providerRetryDelayMs(settings, retry, error, hooks.random);
      hooks.onRetry?.({attempt: retry + 1, delayMs, status: statusOf(error)});
      await sleep(delayMs, signal);
    }
  }
}
