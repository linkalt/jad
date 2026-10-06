import assert from "node:assert/strict";
import {mkdtemp, rm, writeFile} from "node:fs/promises";
import {tmpdir} from "node:os";
import {join} from "node:path";
import test from "node:test";

import {
  ConfigError,
  DEFAULT_APPROVAL_EXTEND_MS,
  DEFAULT_APPROVAL_PREVIEW_BYTES,
  DEFAULT_APPROVAL_TIMEOUT_MS,
  MAX_APPROVAL_PREVIEW_BYTES,
  MAX_APPROVAL_TIMEOUT_MS,
  MIN_APPROVAL_PREVIEW_BYTES,
  SessionConfigLoader,
} from "../src/config.js";

async function withSession<T>(setting: Record<string, unknown>, body: (load: () => ReturnType<SessionConfigLoader["loadSettings"]>) => Promise<T>): Promise<T> {
  const dir = await mkdtemp(join(tmpdir(), "agent-harness-approval-config-"));
  try {
    await writeFile(join(dir, "model.json"), JSON.stringify({model: "gpt-test"}));
    await writeFile(join(dir, "setting.json"), JSON.stringify({cwd: dir, ...setting}));
    return await body(() => new SessionConfigLoader(dir).loadSettings());
  } finally {
    await rm(dir, {recursive: true, force: true});
  }
}

const rejects = (load: () => Promise<unknown>, pattern: RegExp) =>
  assert.rejects(load, (error: unknown) => error instanceof ConfigError && pattern.test(error.message));

test("approval defaults: every tool is off, headless denies, waits follow request_draft", async () => {
  await withSession(
    {tools: {native: {bash: {}, write: {}, edit: {}, process: {}}}, mcp_servers: [{id: "srv", config_path: "s.json"}]},
    async (load) => {
      const settings = await load();
      assert.deepEqual(settings.approval, {
        timeout_ms: DEFAULT_APPROVAL_TIMEOUT_MS,
        extend_ms: DEFAULT_APPROVAL_EXTEND_MS,
        max_timeout_ms: MAX_APPROVAL_TIMEOUT_MS,
        headless: "deny",
        preview_bytes: DEFAULT_APPROVAL_PREVIEW_BYTES,
      });
      assert.equal(settings.tools.native.bash?.approval, "off");
      assert.equal(settings.tools.native.write?.approval, "off");
      assert.equal(settings.tools.native.edit?.approval, "off");
      assert.equal(settings.tools.native.process?.approval, "off");
      assert.equal(settings.mcp_servers[0]?.approval, "off");
    },
  );
});

test("approval ask is accepted per tool and per MCP server", async () => {
  await withSession(
    {
      tools: {native: {bash: {approval: "ask"}, write: {approval: "ask"}, edit: {approval: "off"}, process: {approval: "ask"}}},
      mcp_servers: [{id: "srv", config_path: "s.json", approval: "ask"}],
    },
    async (load) => {
      const settings = await load();
      assert.equal(settings.tools.native.bash?.approval, "ask");
      assert.equal(settings.tools.native.write?.approval, "ask");
      assert.equal(settings.tools.native.edit?.approval, "off");
      assert.equal(settings.tools.native.process?.approval, "ask");
      assert.equal(settings.mcp_servers[0]?.approval, "ask");
    },
  );
});

test("approval modes other than off and ask are refused with the field named", async () => {
  for (const value of ["on", "always", true, 1, null]) {
    await withSession({tools: {native: {bash: {approval: value}}}}, (load) =>
      rejects(load, /tools\.native\.bash\.approval must be "off" or "ask"/));
  }
  await withSession({mcp_servers: [{id: "srv", config_path: "s.json", approval: "yes"}]}, (load) =>
    rejects(load, /mcp_servers\[0\]\.approval must be "off" or "ask"/));
});

test("approval waits and preview bytes are bounded, and a typo is named", async () => {
  await withSession({approval: {timeout_ms: 1000, extend_ms: 2000, max_timeout_ms: 5000, headless: "allow", preview_bytes: 128}}, async (load) => {
    const settings = await load();
    assert.deepEqual(
      [settings.approval.timeout_ms, settings.approval.extend_ms, settings.approval.max_timeout_ms, settings.approval.headless, settings.approval.preview_bytes],
      [1000, 2000, 5000, "allow", 128],
    );
  });
  for (const [field, value] of [
    ["timeout_ms", 0],
    ["timeout_ms", MAX_APPROVAL_TIMEOUT_MS + 1],
    ["extend_ms", 999],
    ["max_timeout_ms", 1.5],
    ["preview_bytes", MIN_APPROVAL_PREVIEW_BYTES - 1],
    ["preview_bytes", MAX_APPROVAL_PREVIEW_BYTES + 1],
    ["preview_bytes", "4096"],
  ] as const) {
    await withSession({approval: {[field]: value}}, (load) => rejects(load, new RegExp(`approval\\.${field} must be a safe integer`)));
  }
  await withSession({approval: {timeout_ms: 9000, max_timeout_ms: 8000}}, (load) =>
    rejects(load, /approval\.timeout_ms must not exceed max_timeout_ms/));
  await withSession({approval: {headless: "maybe"}}, (load) => rejects(load, /approval\.headless must be "deny" or "allow"/));
  await withSession({approval: {timeout: 1000}}, (load) => rejects(load, /approval\.timeout is not supported/));
  await withSession({approval: "ask"}, (load) => rejects(load, /approval must be an object/));
});
