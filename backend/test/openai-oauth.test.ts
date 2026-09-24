import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { homedir, tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";

import type { ResponseCreateParamsStreaming } from "openai/resources/responses/responses.js";

import { ConfigError, SessionConfigLoader } from "../src/config.js";
import { SessionResponseStore } from "../src/persistence.js";
import { persistOpenAIResponse } from "../src/responses.js";
import { createClient } from "../src/session.js";
import { responseFunctionCalls } from "../src/tools/index.js";

async function loadModelConfig(
  sessionDir: string,
  model: Record<string, unknown>,
  settings: Record<string, unknown> = {},
) {
  await writeFile(join(sessionDir, "model.json"), JSON.stringify(model));
  await writeFile(join(sessionDir, "setting.json"), JSON.stringify({cwd: sessionDir, ...settings}));
  return new SessionConfigLoader(sessionDir, {globalJadRoot: sessionDir}).load();
}

async function waitForJsonFile(path: string): Promise<Record<string, unknown>> {
  for (let attempt = 0; attempt < 100; attempt += 1) {
    try {
      const contents = await readFile(path, "utf8");
      if (contents.trim().length > 0) return JSON.parse(contents) as Record<string, unknown>;
    } catch {
      // The debug logger writes its bounded records asynchronously.
    }
    await new Promise((resolve) => setTimeout(resolve, 5));
  }
  throw new Error(`HTTP debug file was not written: ${path}`);
}

const authCliPath = fileURLToPath(new URL("../src/auth-cli.js", import.meta.url));

async function withSessionDirectory<T>(run: (sessionDir: string) => Promise<T>): Promise<T> {
  const sessionDir = await mkdtemp(join(tmpdir(), "agent-harness-oauth-"));
  try {
    return await run(sessionDir);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
}

test("OpenAI OAuth config selects the adapter without requiring an API key", async () => {
  await withSessionDirectory(async (sessionDir) => {
    const config = await loadModelConfig(sessionDir, {
      schema_version: 1,
      provider: "openai-codex",
      model: "gpt-5.4",
      auth_mode: "openai_oauth",
    });

    assert.equal(config.model.auth_mode, "openai_oauth");
    assert.equal(
      config.model.oauth_auth_file,
      join(homedir(), ".jad", "auth", "openai-codex.json"),
    );

    const client = createClient(config, {});
    assert.equal(client.apiKey, "openai-oauth");
    assert.equal(client.baseURL, "https://openai-oauth.local/v1");
  });
});

test("OAuth auth file accepts an explicit home-relative path", async () => {
  await withSessionDirectory(async (sessionDir) => {
    const config = await loadModelConfig(sessionDir, {
      model: "gpt-5.4",
      auth_mode: "openai_oauth",
      oauth_auth_file: "~/.jad/auth/test-codex.json",
    });
    assert.equal(
      config.model.oauth_auth_file,
      join(homedir(), ".jad", "auth", "test-codex.json"),
    );
  });
});

test("legacy model configs keep API-key authentication by default", async () => {
  await withSessionDirectory(async (sessionDir) => {
    const config = await loadModelConfig(sessionDir, {model: "gpt-test"});
    assert.equal(config.model.auth_mode, "api_key");

    const client = createClient(config, {OPENAI_API_KEY: "test-api-key"});
    assert.equal(client.apiKey, "test-api-key");
    assert.throws(
      () => createClient(config, {}),
      /environment variable OPENAI_API_KEY is not set/,
    );
  });
});

test("OAuth adapter streams a Responses result into the existing persistence pipeline", async () => {
  await withSessionDirectory(async (sessionDir) => {
    const authFile = join(sessionDir, "test-auth.json");
    const payload = Buffer.from(JSON.stringify({
      exp: Math.floor(Date.now() / 1000) + 3600,
    })).toString("base64url");
    const accessToken = `eyJhbGciOiJub25lIn0.${payload}.signature`;
    await writeFile(authFile, JSON.stringify({
      auth_mode: "chatgpt",
      tokens: {access_token: accessToken, account_id: "test-account"},
      last_refresh: new Date().toISOString(),
    }));

    const config = await loadModelConfig(sessionDir, {
      model: "gpt-5.4",
      auth_mode: "openai_oauth",
      oauth_auth_file: authFile,
    }, {
      http_debug: {
        enabled: true,
        include_request_headers: true,
        include_request_body: true,
        include_response_headers: false,
        include_response_body: false,
        max_body_bytes: 4096,
      },
    });
    let authenticatedHeaders: Headers | undefined;
    const nativeFetch: typeof fetch = async (input, init) => {
      const rawUrl = input instanceof Request ? input.url : String(input);
      const url = new URL(rawUrl);
      if (url.href === "https://registry.npmjs.org/@openai/codex/latest") {
        return Response.json({version: "0.144.1"});
      }
      if (url.pathname === "/backend-api/codex/models") {
        return Response.json({
          models: [{slug: "gpt-5.4", visibility: "list", supported_in_api: true}],
        });
      }
      if (url.pathname === "/backend-api/codex/responses") {
        authenticatedHeaders = new Headers(init?.headers);
        return new Response([
          "event: response.output_item.done",
          'data: {"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc_1","status":"completed","call_id":"call_1","name":"read","arguments":"{}"}}',
          "",
          "event: response.output_item.done",
          'data: {"type":"response.output_item.done","output_index":1,"item":{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","annotations":[],"text":"hello"}]}}',
          "",
          "event: response.completed",
          'data: {"type":"response.completed","response":{"id":"resp_1","created_at":1735689600,"model":"gpt-5.4","status":"completed","output":[],"usage":{"input_tokens":2,"input_tokens_details":{"cached_tokens":0},"output_tokens":1,"output_tokens_details":{"reasoning_tokens":0}}}}',
          "",
          "",
        ].join("\n"), {headers: {"Content-Type": "text/event-stream"}});
      }
      throw new Error(`Unexpected OAuth adapter request: ${url.href}`);
    };

    const client = createClient(config, {}, nativeFetch);
    const store = await SessionResponseStore.open(sessionDir);
    try {
      const persisted = await persistOpenAIResponse(
        client,
        store,
        {model: "gpt-5.4", input: "Say hello.", stream: true} as ResponseCreateParamsStreaming,
        {turnId: "oauth-adapter-smoke"},
      );
      assert.equal(persisted.event.response.status, "completed");
      const output = persisted.event.response.output as Array<Record<string, unknown>>;
      assert.deepEqual(output.map((item) => item.type), ["function_call", "message"]);
      assert.equal(responseFunctionCalls(persisted.event.response).length, 1);
      assert.equal(authenticatedHeaders?.get("authorization"), `Bearer ${accessToken}`);
      assert.equal(authenticatedHeaders?.get("chatgpt-account-id"), "test-account");
      assert.equal(authenticatedHeaders?.get("openai-beta"), null);
      const debugRequest = await waitForJsonFile(join(config.paths.http_debug_dir, "request1"));
      assert.equal((debugRequest.headers as Record<string, unknown>).authorization, "[REDACTED]");
      assert.ok(!JSON.stringify(debugRequest).includes(accessToken));
    } finally {
      await store.close();
    }
  });
});

test("OAuth mode rejects ambiguous endpoints and relative credential paths", async () => {
  await withSessionDirectory(async (sessionDir) => {
    await assert.rejects(
      loadModelConfig(sessionDir, {
        model: "gpt-test",
        auth_mode: "openai_oauth",
        base_url: "https://provider.example/v1",
      }),
      (error: unknown) => error instanceof ConfigError && /base_url is managed/.test(error.message),
    );

    await assert.rejects(
      loadModelConfig(sessionDir, {
        model: "gpt-test",
        auth_mode: "openai_oauth",
        oauth_auth_file: "auth/openai-codex.json",
      }),
      (error: unknown) => error instanceof ConfigError && /must be absolute or start with ~/.test(error.message),
    );
  });
});

test("auth CLI status never prints stored tokens and clear only removes the local file", async () => {
  await withSessionDirectory(async (sessionDir) => {
    const authFile = join(sessionDir, "auth.json");
    const testToken = "test-access-token-that-must-not-be-printed";
    await writeFile(authFile, JSON.stringify({tokens: {access_token: testToken}}));

    const status = spawnSync(process.execPath, [authCliPath, "status", "--oauth-file", authFile], {
      encoding: "utf8",
    });
    assert.equal(status.status, 0, status.stderr);
    assert.match(status.stdout, /credential file is present/);
    assert.ok(!status.stdout.includes(testToken));

    const clear = spawnSync(process.execPath, [authCliPath, "clear", "--oauth-file", authFile, "--force"], {
      encoding: "utf8",
    });
    assert.equal(clear.status, 0, clear.stderr);
    assert.match(clear.stdout, /does not revoke the OAuth grant/);
    await assert.rejects(readFile(authFile), {code: "ENOENT"});
  });
});
