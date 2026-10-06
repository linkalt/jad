import assert from "node:assert/strict";
import {createServer} from "node:http";
import test from "node:test";

import OpenAI from "openai";

import type {ProviderRetrySettings} from "../src/config.js";
import {
  isRetryableProviderError,
  providerRetryDelayMs,
  rateLimitResetMs,
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

test("x-ratelimit-reset is read as an epoch on a 429 only, and only when it plainly is one", () => {
  const now = Date.UTC(2026, 0, 1, 0, 0, 0);
  const reset = (status: number, value: string) => rateLimitResetMs(statusError(status, {"x-ratelimit-reset": value}), now);
  assert.equal(reset(429, String(now + 3000)), 3000, "epoch milliseconds");
  assert.equal(reset(429, String((now + 3000) / 1000)), 3000, "epoch seconds");
  assert.equal(reset(429, `${now + 1500}.0`), 1500, "a fraction");
  assert.equal(reset(429, String(now - 3000)), undefined, "already passed");
  assert.equal(reset(429, String(now)), undefined, "now exactly");
  assert.equal(reset(429, "30"), undefined, "a delta in seconds");
  assert.equal(reset(429, "60000"), undefined, "a delta in milliseconds below the floor");
  assert.equal(reset(429, "6m0s"), undefined, "a duration string");
  assert.equal(reset(429, "soon"), undefined, "text");
  assert.equal(reset(429, `-${now + 1000}`), undefined, "negative");
  assert.equal(reset(429, "1.8e12"), undefined, "exponent");
  assert.equal(reset(429, "0x1"), undefined, "hex");
  assert.equal(reset(429, ""), undefined, "empty");
  assert.equal(reset(503, String(now + 3000)), undefined, "a 503 is ignored");
  assert.equal(rateLimitResetMs(new OpenAI.APIConnectionError({message: "down"}), now), undefined, "a transport failure");
  assert.equal(rateLimitResetMs(statusError(429), now), undefined, "no header");
});

test("the reset replaces the backoff on a 429, is capped by max_delay_ms, and yields to Retry-After", () => {
  const now = Date.UTC(2026, 0, 1, 0, 0, 0);
  const error = (headers: Record<string, string>, status = 429) => statusError(status, headers);
  const delay = (s: ProviderRetrySettings, attempt: number, e: Error) => providerRetryDelayMs(s, attempt, e, () => 0, now);
  assert.equal(delay(settings, 0, error({"x-ratelimit-reset": String(now + 3000)})), 3000);
  assert.equal(delay(settings, 0, error({"x-ratelimit-reset": String(now + 45000)})), 8000, "capped");
  assert.equal(
    delay({...settings, max_delay_ms: 60000}, 0, error({"x-ratelimit-reset": String(now + 45000)})),
    45000,
    "a higher maximum lets the wait outlast the window",
  );
  assert.equal(delay(settings, 0, error({"x-ratelimit-reset": String(now + 3000)}, 503)), 500, "a 503 keeps the backoff");
  assert.equal(
    delay(settings, 0, error({"x-ratelimit-reset": String(now + 3000), "retry-after-ms": "40"})),
    40,
    "Retry-After wins over the reset",
  );
  assert.equal(delay(settings, 1, error({"x-ratelimit-reset": String(now - 1000)})), 1000, "a reset in the past keeps the backoff");
});

test("withProviderRetry waits until the provider's reset time before the next attempt", async () => {
  let calls = 0;
  const waits: number[] = [];
  await withProviderRetry(
    {...settings, max_delay_ms: 60000},
    async () => {
      calls += 1;
      if (calls === 1) throw statusError(429, {"x-ratelimit-reset": String(Date.now() + 20000)});
      return "ok";
    },
    undefined,
    {random: () => 0, sleep: async (ms) => void waits.push(ms)},
  );
  assert.equal(calls, 2);
  assert.equal(waits.length, 1);
  const waited = waits[0] ?? -1;
  assert.ok(waited > 19000 && waited <= 20000, `wait ${waited}`);
});

test("a real SDK error from a real HTTP 429 carries x-ratelimit-reset into the wait", async () => {
  const reset = String(Date.now() + 20000);
  let requests = 0;
  const server = createServer((req, res) => {
    req.resume();
    requests += 1;
    if (requests === 1) {
      res.writeHead(429, {"content-type": "application/json", "X-RateLimit-Reset": reset});
      res.end(JSON.stringify({error: {message: "slow down"}}));
      return;
    }
    res.writeHead(200, {"content-type": "application/json"});
    res.end(JSON.stringify({id: "r", object: "response", status: "completed", output: []}));
  });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", () => resolve()));
  try {
    const address = server.address();
    assert.ok(address !== null && typeof address !== "string");
    const client = new OpenAI({apiKey: "k", baseURL: `http://127.0.0.1:${address.port}/v1`, maxRetries: 0});
    const waits: number[] = [];
    await withProviderRetry(
      {...settings, max_delay_ms: 60000},
      () => client.responses.create({model: "m", input: "hi"}),
      undefined,
      {random: () => 0, sleep: async (ms) => void waits.push(ms)},
    );
    assert.equal(requests, 2);
    assert.equal(waits.length, 1);
    const waited = waits[0] ?? -1;
    assert.ok(waited > 18000 && waited <= 20000, `wait ${waited}`);
  } finally {
    await new Promise<void>((resolve) => server.close(() => resolve()));
  }
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
