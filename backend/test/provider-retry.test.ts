import assert from "node:assert/strict";
import test from "node:test";

import OpenAI from "openai";

import type {ProviderRetrySettings} from "../src/config.js";
import {
  isRetryableProviderError,
  providerRetryDelayMs,
  retryAfterMs,
  withProviderRetry,
} from "../src/provider-retry.js";

const settings: ProviderRetrySettings = {max_retries: 2, initial_delay_ms: 500, max_delay_ms: 8000};

function statusError(status: number, headers: Record<string, string> = {}): Error {
  return OpenAI.APIError.generate(status, {error: {message: "x"}}, "x", new Headers(headers));
}

test("which failures are retried: the SDK's statuses, connection errors, and the x-should-retry header", () => {
  for (const status of [408, 409, 429, 500, 502, 503, 529]) {
    assert.equal(isRetryableProviderError(statusError(status)), true, `status ${status}`);
  }
  for (const status of [400, 401, 403, 404, 422]) {
    assert.equal(isRetryableProviderError(statusError(status)), false, `status ${status}`);
  }
  assert.equal(isRetryableProviderError(statusError(500, {"x-should-retry": "false"})), false);
  assert.equal(isRetryableProviderError(statusError(400, {"x-should-retry": "true"})), true);
  assert.equal(isRetryableProviderError(new OpenAI.APIConnectionError({message: "down"})), true);
  assert.equal(isRetryableProviderError(new OpenAI.APIConnectionTimeoutError()), true);
});

test("a stop by the user, a plain error and a non-error are never retried", () => {
  assert.equal(isRetryableProviderError(new OpenAI.APIUserAbortError()), false);
  assert.equal(isRetryableProviderError(new Error("boom")), false);
  assert.equal(isRetryableProviderError(Object.assign(new Error("fake"), {status: 500})), false);
  assert.equal(isRetryableProviderError(undefined), false);
  assert.equal(isRetryableProviderError("429"), false);
  assert.equal(isRetryableProviderError(null), false);
});

test("Retry-After-Ms wins over Retry-After, and Retry-After accepts seconds or an HTTP date", () => {
  assert.equal(retryAfterMs(statusError(429, {"retry-after-ms": "250", "retry-after": "9"})), 250);
  assert.equal(retryAfterMs(statusError(429, {"retry-after": "2"})), 2000);
  assert.equal(retryAfterMs(statusError(429, {"retry-after": "1.5"})), 1500);
  const now = Date.UTC(2026, 0, 1, 0, 0, 0);
  assert.equal(retryAfterMs(statusError(429, {"retry-after": new Date(now + 3000).toUTCString()}), now), 3000);
  assert.equal(retryAfterMs(statusError(429, {"retry-after": new Date(now - 3000).toUTCString()}), now), 0);
  assert.equal(retryAfterMs(statusError(429, {"retry-after": "soon"})), undefined);
  assert.equal(retryAfterMs(statusError(429, {"retry-after-ms": "-5"})), undefined);
  assert.equal(retryAfterMs(statusError(429)), undefined);
});

test("backoff doubles from the initial delay, stops at the maximum, and jitter only shortens it", () => {
  const error = statusError(500);
  const noJitter = () => 0;
  assert.equal(providerRetryDelayMs(settings, 0, error, noJitter), 500);
  assert.equal(providerRetryDelayMs(settings, 1, error, noJitter), 1000);
  assert.equal(providerRetryDelayMs(settings, 2, error, noJitter), 2000);
  assert.equal(providerRetryDelayMs(settings, 4, error, noJitter), 8000);
  assert.equal(providerRetryDelayMs(settings, 9, error, noJitter), 8000);
  // The longest jitter takes a quarter off.
  assert.equal(providerRetryDelayMs(settings, 1, error, () => 1), 750);
  for (let i = 0; i < 50; i += 1) {
    const delay = providerRetryDelayMs(settings, 2, error);
    assert.ok(delay >= 1500 && delay <= 2000, `delay ${delay}`);
  }
});

test("a Retry-After from the provider replaces the backoff but never exceeds max_delay_ms", () => {
  assert.equal(providerRetryDelayMs(settings, 0, statusError(429, {"retry-after-ms": "40"}), () => 0), 40);
  assert.equal(providerRetryDelayMs(settings, 3, statusError(429, {"retry-after": "2"}), () => 0), 2000);
  assert.equal(providerRetryDelayMs(settings, 0, statusError(429, {"retry-after": "120"}), () => 0), 8000);
  assert.equal(
    providerRetryDelayMs({...settings, max_delay_ms: 1000}, 0, statusError(429, {"retry-after": "5"}), () => 0),
    1000,
  );
});

test("withProviderRetry retries until success and reports each wait", async () => {
  let calls = 0;
  const waits: number[] = [];
  const seen: Array<{attempt: number; status: number | undefined}> = [];
  const result = await withProviderRetry(
    {...settings, max_retries: 3},
    async () => {
      calls += 1;
      if (calls < 3) throw statusError(429);
      return "ok";
    },
    undefined,
    {
      random: () => 0,
      sleep: async (ms) => void waits.push(ms),
      onRetry: ({attempt, status}) => seen.push({attempt, status}),
    },
  );
  assert.equal(result, "ok");
  assert.equal(calls, 3);
  assert.deepEqual(waits, [500, 1000]);
  assert.deepEqual(seen, [{attempt: 1, status: 429}, {attempt: 2, status: 429}]);
});

test("withProviderRetry throws the last error unchanged once the retries are used up", async () => {
  let calls = 0;
  const last = statusError(503);
  await assert.rejects(
    withProviderRetry(settings, async () => {
      calls += 1;
      throw calls === 3 ? last : statusError(503);
    }, undefined, {sleep: async () => {}}),
    (error: unknown) => error === last,
  );
  assert.equal(calls, 3, "one attempt plus two retries");
});

test("max_retries 0 makes a single attempt", async () => {
  let calls = 0;
  await assert.rejects(
    withProviderRetry({...settings, max_retries: 0}, async () => {
      calls += 1;
      throw statusError(429);
    }, undefined, {sleep: async () => {}}),
  );
  assert.equal(calls, 1);
});

test("a failure that is not retryable is thrown at once", async () => {
  let calls = 0;
  await assert.rejects(
    withProviderRetry(settings, async () => {
      calls += 1;
      throw statusError(401);
    }, undefined, {sleep: async () => {}}),
  );
  assert.equal(calls, 1);
});

test("a stop during a wait ends the loop and no further attempt is made", async () => {
  let calls = 0;
  const controller = new AbortController();
  const reason = new Error("stopped");
  await assert.rejects(
    withProviderRetry(settings, async () => {
      calls += 1;
      throw statusError(429);
    }, controller.signal, {
      sleep: async () => {
        controller.abort(reason);
        throw reason;
      },
    }),
    (error: unknown) => error === reason,
  );
  assert.equal(calls, 1);
});

test("an already stopped run is not retried even when the error looks retryable", async () => {
  let calls = 0;
  const controller = new AbortController();
  controller.abort();
  await assert.rejects(
    withProviderRetry(settings, async () => {
      calls += 1;
      throw statusError(429);
    }, controller.signal, {sleep: async () => {}}),
  );
  assert.equal(calls, 1);
});
