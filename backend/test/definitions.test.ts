import assert from "node:assert/strict";
import {Ajv} from "ajv";
import {readFile} from "node:fs/promises";
import {dirname, join} from "node:path";
import test from "node:test";
import {fileURLToPath} from "node:url";

import {nativeBashDefinition} from "../src/tools/native/bash.js";
import {nativeExecDefinition} from "../src/tools/native/exec.js";
import {nativeEditDefinition} from "../src/tools/native/edit.js";
import {nativeGlobDefinition} from "../src/tools/native/glob.js";
import {nativeGrepDefinition, normalizeGrepArguments} from "../src/tools/native/grep.js";
import {nativeReadDefinition} from "../src/tools/native/read.js";
import {nativeWriteDefinition} from "../src/tools/native/write.js";
import {nativeProcessDefinition, normalizeProcessArguments} from "../src/tools/native/process.js";
import {nativeTaskDefinition, normalizeTaskArguments} from "../src/tools/native/task.js";
import {nativeMemoryDefinition, normalizeMemoryArguments} from "../src/tools/native/memory.js";
import {nativeFetchDefinition, normalizeFetchArguments} from "../src/tools/native/fetch.js";
import {nativeRequestDraftDefinition} from "../src/tools/native/request-draft.js";
import {nativeSkillDefinition, normalizeSkillArguments} from "../src/tools/native/skill.js";

const sampleDirectory = join(
  dirname(fileURLToPath(import.meta.url)),
  "..",
  "..",
  "..",
  "sample",
  "tools",
  "native",
);

// The catalog loads definitions from these documents, while tests and the
// provider payload comparison use the exported constants. Keep one source of
// truth by asserting that both agree.
test("shipped definition documents match the code definitions", async () => {
  const definitions = [
    ["read.json", nativeReadDefinition],
    ["write.json", nativeWriteDefinition],
    ["edit.json", nativeEditDefinition],
    ["grep.json", nativeGrepDefinition],
    ["bash.json", nativeBashDefinition],
    ["exec.json", nativeExecDefinition],
    ["glob.json", nativeGlobDefinition],
    ["process.json", nativeProcessDefinition],
    ["task.json", nativeTaskDefinition],
    ["memory.json", nativeMemoryDefinition],
    ["fetch.json", nativeFetchDefinition],
    ["request_draft.json", nativeRequestDraftDefinition],
    ["skill.json", nativeSkillDefinition],
  ] as const;
  for (const [file, definition] of definitions) {
    const document = JSON.parse(
      await readFile(join(sampleDirectory, file), "utf8"),
    ) as unknown;
    assert.deepEqual(
      document,
      {schema_version: 1, tools: [definition]},
      `${file} drifted from ${definition.id}`,
    );
  }
});

test("normalizes harmless numeric bounds before schema validation", () => {
  assert.deepEqual(normalizeGrepArguments({context: 99}), {context: 20});
  assert.deepEqual(normalizeFetchArguments({url: " https://example.com ", limit: 0}), {url: "https://example.com", limit: 1});
  assert.deepEqual(normalizeMemoryArguments({limit: -1}), {limit: 1});
  assert.deepEqual(normalizeProcessArguments({timeout_ms: 0}), {timeout_ms: 1});
  assert.deepEqual(normalizeTaskArguments({limit: 0}), {limit: 1, action: "list"});
  assert.deepEqual(normalizeSkillArguments({name: " Agent_Tool Design "}), {name: "agent-tool-design"});
});

test("conditional tool schemas reject incomplete explicit actions", () => {
  const ajv = new Ajv();
  const fetch = ajv.compile(nativeFetchDefinition.inputSchema);
  const process = ajv.compile(nativeProcessDefinition.inputSchema);
  const task = ajv.compile(nativeTaskDefinition.inputSchema);
  assert.equal(fetch({}), false);
  assert.equal(fetch({url: "https://example.com"}), true);
  assert.equal(process({action: "start"}), false);
  assert.equal(process({action: "start", command: "npm run dev"}), true);
  assert.equal(process({action: "stop"}), false);
  assert.equal(process({action: "stop", id: "p_1"}), true);
  assert.equal(task({action: "add"}), false);
  assert.equal(task({action: "add", title: "Plan"}), true);
  assert.equal(task({action: "delete"}), false);
  assert.equal(task({action: "delete", id: "T-100"}), true);
  assert.equal(task({action: "update", id: "T-100"}), false);
  assert.equal(task({action: "update", id: "T-100", title: "Updated"}), true);
});
