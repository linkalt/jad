import assert from "node:assert/strict";
import {mkdtemp, rm, writeFile} from "node:fs/promises";
import {tmpdir} from "node:os";
import {join} from "node:path";
import test from "node:test";

import {Ajv2020} from "ajv/dist/2020.js";
import {readFile} from "node:fs/promises";

import {
  ConfigError,
  DEFAULT_PROVIDER_RETRY_INITIAL_DELAY_MS,
  DEFAULT_PROVIDER_RETRY_MAX_DELAY_MS,
  DEFAULT_PROVIDER_RETRY_MAX_RETRIES,
  SessionConfigLoader,
} from "../src/config.js";

async function withSession<T>(
  setting: Record<string, unknown>,
  body: (load: () => ReturnType<SessionConfigLoader["loadSettings"]>) => Promise<T>,
): Promise<T> {
  const dir = await mkdtemp(join(tmpdir(), "agent-harness-provider-retry-config-"));
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

test("provider_retry defaults equal what the SDKs did before the setting existed", async () => {
  await withSession({}, async (load) => {
    assert.deepEqual((await load()).provider_retry, {
      max_retries: DEFAULT_PROVIDER_RETRY_MAX_RETRIES,
      initial_delay_ms: DEFAULT_PROVIDER_RETRY_INITIAL_DELAY_MS,
      max_delay_ms: DEFAULT_PROVIDER_RETRY_MAX_DELAY_MS,
    });
    assert.equal(DEFAULT_PROVIDER_RETRY_MAX_RETRIES, 2);
    assert.equal(DEFAULT_PROVIDER_RETRY_INITIAL_DELAY_MS, 500);
    assert.equal(DEFAULT_PROVIDER_RETRY_MAX_DELAY_MS, 8000);
  });
});

test("provider_retry accepts every key, and 0 retries turns retrying off", async () => {
  await withSession({provider_retry: {max_retries: 5, initial_delay_ms: 1000, max_delay_ms: 30000}}, async (load) => {
    assert.deepEqual((await load()).provider_retry, {max_retries: 5, initial_delay_ms: 1000, max_delay_ms: 30000});
  });
  await withSession({provider_retry: {max_retries: 0}}, async (load) => {
    assert.equal((await load()).provider_retry.max_retries, 0);
  });
});

test("provider_retry rejects bad values, null, unknown keys and an initial delay above the maximum", async () => {
  const cases: Array<[unknown, RegExp]> = [
    [{max_retries: -1}, /provider_retry\.max_retries must be a safe integer between 0 and 10/],
    [{max_retries: 11}, /provider_retry\.max_retries must be a safe integer between 0 and 10/],
    [{max_retries: 1.5}, /provider_retry\.max_retries must be a safe integer/],
    [{max_retries: "2"}, /provider_retry\.max_retries must be a safe integer/],
    [{max_retries: null}, /provider_retry\.max_retries must be a safe integer/],
    [{initial_delay_ms: 0}, /provider_retry\.initial_delay_ms must be a safe integer between 1 and 60000/],
    [{initial_delay_ms: null}, /provider_retry\.initial_delay_ms must be a safe integer/],
    [{max_delay_ms: 0}, /provider_retry\.max_delay_ms must be a safe integer between 1 and 600000/],
    [{max_delay_ms: 600001}, /provider_retry\.max_delay_ms must be a safe integer/],
    [{initial_delay_ms: 9000}, /provider_retry\.initial_delay_ms must not exceed max_delay_ms/],
    [{initial_delay_ms: 5000, max_delay_ms: 4000}, /provider_retry\.initial_delay_ms must not exceed max_delay_ms/],
    [{retries: 3}, /provider_retry\.retries is not supported; supported keys are initial_delay_ms, max_delay_ms, max_retries/],
    [{max_retrie: 3}, /provider_retry\.max_retrie is not supported; did you mean max_retries\?/],
    [null, /provider_retry must be an object/],
    [[], /provider_retry must be an object/],
    ["on", /provider_retry must be an object/],
  ];
  for (const [value, pattern] of cases) {
    await withSession({provider_retry: value}, (load) => rejects(load, pattern));
  }
});

test("the schema accepts provider_retry and rejects the same bad values", async () => {
  const schema = JSON.parse(await readFile(new URL("../../../sample/schemes/setting.schema.json", import.meta.url), "utf8"));
  const validate = new Ajv2020({strict: false, allErrors: true}).compile(schema);
  const base = {schema_version: 1, session_name: "s", cwd: "/tmp"};
  const ok = (provider_retry: unknown) => validate({...base, provider_retry});
  assert.equal(ok({}), true, JSON.stringify(validate.errors));
  assert.equal(ok({max_retries: 5, initial_delay_ms: 1000, max_delay_ms: 30000}), true, JSON.stringify(validate.errors));
  assert.equal(ok({max_retries: 0}), true);
  for (const bad of [
    {max_retries: -1},
    {max_retries: 11},
    {max_retries: "2"},
    {initial_delay_ms: 0},
    {max_delay_ms: 600001},
    {retries: 3},
    null,
  ]) {
    assert.equal(ok(bad), false, `schema accepted ${JSON.stringify(bad)}`);
  }
});
