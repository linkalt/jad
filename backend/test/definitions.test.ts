import assert from "node:assert/strict";
import {readFile} from "node:fs/promises";
import {dirname, join} from "node:path";
import test from "node:test";
import {fileURLToPath} from "node:url";

import {nativeBashDefinition} from "../src/tools/native/bash.js";
import {nativeEditDefinition} from "../src/tools/native/edit.js";
import {nativeGlobDefinition} from "../src/tools/native/glob.js";
import {nativeGrepDefinition} from "../src/tools/native/grep.js";
import {nativeReadDefinition} from "../src/tools/native/read.js";
import {nativeWriteDefinition} from "../src/tools/native/write.js";
import {nativeProcessDefinition} from "../src/tools/native/process.js";
import {nativeTaskDefinition} from "../src/tools/native/task.js";

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
    ["glob.json", nativeGlobDefinition],
    ["process.json", nativeProcessDefinition],
    ["task.json", nativeTaskDefinition],
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
