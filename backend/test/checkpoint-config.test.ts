import assert from "node:assert/strict";
import {readFile} from "node:fs/promises";
import {mkdtemp, rm, writeFile} from "node:fs/promises";
import {tmpdir} from "node:os";
import {join} from "node:path";
import test from "node:test";

import {Ajv2020} from "ajv/dist/2020.js";

import {
  ConfigError,
  DEFAULT_CHECKPOINT_MAX_CHECKPOINTS,
  DEFAULT_CHECKPOINT_MAX_FILES,
  DEFAULT_CHECKPOINT_MAX_TOTAL_BYTES,
  DEFAULT_CHECKPOINT_TIMEOUT_MS,
  SessionConfigLoader,
} from "../src/config.js";

async function withSession<T>(
  setting: Record<string, unknown>,
  body: (load: () => ReturnType<SessionConfigLoader["loadSettings"]>) => Promise<T>,
): Promise<T> {
  const dir = await mkdtemp(join(tmpdir(), "agent-harness-checkpoint-config-"));
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

test("checkpoint defaults: off, safe restore, first mutation, MCP counts as a change", async () => {
  await withSession({}, async (load) => {
    const settings = await load();
    assert.deepEqual(settings.checkpoint, {
      enabled: false,
      git_binary: "git",
      max_checkpoints: DEFAULT_CHECKPOINT_MAX_CHECKPOINTS,
      max_files: DEFAULT_CHECKPOINT_MAX_FILES,
      max_total_bytes: DEFAULT_CHECKPOINT_MAX_TOTAL_BYTES,
      timeout_ms: DEFAULT_CHECKPOINT_TIMEOUT_MS,
      include_mcp_tools: true,
      snapshot_on: "first_mutation",
      safe_restore: true,
    });
  });
});

test("checkpoint accepts every key", async () => {
  await withSession(
    {
      checkpoint: {
        enabled: true,
        git_binary: " /usr/bin/git ",
        max_checkpoints: 3,
        max_files: 50,
        max_total_bytes: 1024,
        timeout_ms: 2000,
        include_mcp_tools: false,
        snapshot_on: "run_start",
        safe_restore: false,
      },
    },
    async (load) => {
      assert.deepEqual((await load()).checkpoint, {
        enabled: true,
        git_binary: "/usr/bin/git",
        max_checkpoints: 3,
        max_files: 50,
        max_total_bytes: 1024,
        timeout_ms: 2000,
        include_mcp_tools: false,
        snapshot_on: "run_start",
        safe_restore: false,
      });
    },
  );
});

test("checkpoint rejects bad values and unknown keys", async () => {
  const cases: Array<[Record<string, unknown>, RegExp]> = [
    [{enabled: "yes"}, /checkpoint\.enabled must be a boolean/],
    [{enabled: null}, /checkpoint\.enabled must be a boolean/],
    [{git_binary: ""}, /checkpoint\.git_binary must be a non-empty string/],
    [{git_binary: 7}, /checkpoint\.git_binary must be a non-empty string/],
    [{max_checkpoints: 0}, /checkpoint\.max_checkpoints must be a safe integer between 1 and 1000/],
    [{max_files: 1.5}, /checkpoint\.max_files must be a safe integer/],
    [{max_total_bytes: 0}, /checkpoint\.max_total_bytes must be a safe integer/],
    [{timeout_ms: 999}, /checkpoint\.timeout_ms must be a safe integer between 1000/],
    [{include_mcp_tools: 1}, /checkpoint\.include_mcp_tools must be a boolean/],
    [{snapshot_on: "always"}, /checkpoint\.snapshot_on must be "first_mutation" or "run_start"/],
    [{safe_restore: "true"}, /checkpoint\.safe_restore must be a boolean/],
    [{enable: true}, /checkpoint\.enable is not supported; did you mean enabled\?/],
  ];
  for (const [checkpoint, pattern] of cases) {
    await withSession({checkpoint}, async (load) => rejects(load, pattern));
  }
  await withSession({checkpoint: "on"}, async (load) => rejects(load, /checkpoint must be an object/));
});

test("the setting schema accepts a valid checkpoint node and rejects bad ones", async () => {
  const schema = JSON.parse(await readFile(new URL("../../../sample/schemes/setting.schema.json", import.meta.url), "utf8"));
  const validate = new Ajv2020({strict: false}).compile(schema);
  const base = {schema_version: 1};
  assert.equal(validate({...base, checkpoint: {enabled: true, snapshot_on: "run_start", max_files: 10}}), true, JSON.stringify(validate.errors));
  assert.equal(validate({...base, checkpoint: {}}), true);
  for (const bad of [
    {enabled: "yes"},
    {snapshot_on: "always"},
    {max_checkpoints: 0},
    {timeout_ms: 5},
    {git_binary: ""},
    {surprise: true},
  ]) {
    assert.equal(validate({...base, checkpoint: bad}), false, JSON.stringify(bad));
  }
});
