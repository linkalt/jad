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

async function waitForLog(path: string, minimumRecords: number): Promise<string> {
  for (let attempt = 0; attempt < 100; attempt += 1) {
    try {
      const contents = await readFile(path, "utf8");
      if (contents.trimEnd().split("\n").length >= minimumRecords) {
        return contents;
      }
    } catch {
      // The logger creates the file asynchronously.
    }
    await new Promise((resolve) => setTimeout(resolve, 5));
  }
  throw new Error(`HTTP debug log was not written: ${path}`);
}

test("logs bounded native request and response data with secret headers redacted", async () => {
  const sessionDir = await mkdtemp(join(tmpdir(), "agent-harness-http-debug-"));
  try {
    const debugFetch = createHttpDebugFetch(
      sessionDir,
      debugSettings,
      async () =>
        new Response("response-body-long", {
          status: 200,
          statusText: "OK",
          headers: { "content-type": "text/event-stream" },
        }),
    );
    assert.ok(debugFetch);

    const response = await debugFetch("https://provider.test/v1/responses", {
      method: "POST",
      headers: {
        authorization: "Bearer secret-token",
        "x-test-header": "visible",
      },
      body: "request-body-long",
    });
    assert.equal(response.status, 200);

    const log = await waitForLog(join(sessionDir, "http-debug.jsonl"), 2);
    const records = log
      .trimEnd()
      .split("\n")
      .map((line) => JSON.parse(line) as Record<string, unknown>);
    assert.equal(records.length, 2);

    const request = records[0]!;
    assert.equal(request.direction, "request");
    assert.deepEqual(request.headers, {
      authorization: "[REDACTED]",
      "x-test-header": "visible",
    });
    assert.equal(request.body, "request-");
    assert.equal(request.body_truncated, true);

    const loggedResponse = records[1]!;
    assert.equal(loggedResponse.direction, "response");
    assert.equal(loggedResponse.status, 200);
    assert.deepEqual(loggedResponse.headers, {
      "content-type": "text/event-stream",
    });
    assert.equal(loggedResponse.body, "response");
    assert.equal(loggedResponse.body_truncated, true);
  } finally {
    await rm(sessionDir, { recursive: true, force: true });
  }
});

test("returns no wrapper when HTTP debugging is disabled", async () => {
  const disabled = createHttpDebugFetch(
    "/tmp/agent-harness-http-debug-disabled",
    { ...debugSettings, enabled: false },
  );
  assert.equal(disabled, undefined);
});
