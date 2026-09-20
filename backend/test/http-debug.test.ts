import assert from "node:assert/strict";
import { mkdtemp, readFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";

import { createHttpDebugFetch } from "../src/http-debug.js";
import type { HttpDebugSettings } from "../src/config.js";

const debugSettings: HttpDebugSettings = {
  enabled: true,
  include_request_headers: true,
  include_request_body: true,
  include_response_headers: true,
  include_response_body: true,
  max_body_bytes: 8,
};

async function waitForJsonFile(path: string): Promise<Record<string, unknown>> {
  for (let attempt = 0; attempt < 100; attempt += 1) {
    try {
      const contents = await readFile(path, "utf8");
      if (contents.trim().length > 0) {
        return JSON.parse(contents) as Record<string, unknown>;
      }
    } catch {
      // The logger creates and writes each file asynchronously.
    }
    await new Promise((resolve) => setTimeout(resolve, 5));
  }
  throw new Error(`HTTP debug file was not written: ${path}`);
}

test("writes numbered request and response files with secret headers redacted", async () => {
  const sessionDir = await mkdtemp(join(tmpdir(), "agent-harness-http-debug-"));
  try {
    let callCount = 0;
    const debugFetch = createHttpDebugFetch(
      sessionDir,
      debugSettings,
      async () => {
        callCount += 1;
        return new Response(`response-body-${callCount}`, {
          status: 200,
          statusText: "OK",
          headers: { "content-type": "text/event-stream" },
        });
      },
    );
    assert.ok(debugFetch);

    const request = {
      method: "POST",
      headers: {
        authorization: "Bearer secret-token",
        "x-test-header": "visible",
      },
      body: "request-body-long",
    };
    assert.equal((await debugFetch("https://provider.test/v1/responses", request)).status, 200);
    assert.equal((await debugFetch("https://provider.test/v1/responses", request)).status, 200);

    const debugDir = join(sessionDir, "debug", "http");
    const request1 = await waitForJsonFile(join(debugDir, "request1"));
    const response1 = await waitForJsonFile(join(debugDir, "response1"));
    const request2 = await waitForJsonFile(join(debugDir, "request2"));
    const response2 = await waitForJsonFile(join(debugDir, "response2"));

    assert.equal(request1.direction, "request");
    assert.deepEqual(request1.headers, {
      authorization: "[REDACTED]",
      "x-test-header": "visible",
    });
    assert.equal(request1.body, "request-");
    assert.equal(request1.body_truncated, true);

    assert.equal(response1.direction, "response");
    assert.equal(response1.status, 200);
    assert.deepEqual(response1.headers, {
      "content-type": "text/event-stream",
    });
    assert.equal(response1.body, "response");
    assert.equal(response1.body_truncated, true);
    assert.equal(request2.direction, "request");
    assert.equal(response2.direction, "response");

    const nextDebugFetch = createHttpDebugFetch(
      sessionDir,
      debugSettings,
      async () => new Response("third-response", {status: 200}),
    );
    assert.ok(nextDebugFetch);
    await nextDebugFetch("https://provider.test/v1/responses", request);
    await waitForJsonFile(join(debugDir, "request3"));
    await waitForJsonFile(join(debugDir, "response3"));

    await assert.rejects(readFile(join(sessionDir, "http-debug.jsonl")));
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("returns no wrapper when HTTP debugging is disabled", async () => {
  const disabled = createHttpDebugFetch(
    "/tmp/agent-harness-http-debug-disabled",
    {...debugSettings, enabled: false},
  );
  assert.equal(disabled, undefined);
});
