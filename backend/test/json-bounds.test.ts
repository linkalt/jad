import assert from "node:assert/strict";
import test from "node:test";

import {draftBoundsError, jsonBytes, jsonDepth} from "../src/json-bounds.js";

test("iterative JSON byte count matches JSON.stringify for dynamic JSON values", () => {
  const values: unknown[] = [
    {goal: "café\n\"draft\"", nested: {items: [1, true, null, "🙂"]}},
    [],
    {empty: {}, array: []},
    {large: "x".repeat(1024)},
  ];
  for (const value of values) {
    assert.equal(
      jsonBytes(value),
      Buffer.byteLength(JSON.stringify(value) ?? "", "utf8"),
    );
  }
});

test("deep draft bounds return a normal rejection without using the call stack", () => {
  let draft: unknown = {value: "x"};
  for (let depth = 0; depth < 4_000; depth += 1) {
    draft = {nested: draft};
  }

  assert.equal(jsonDepth(draft), 4_001);
  assert.match(
    draftBoundsError(draft, {max_draft_bytes: 100_000, max_draft_depth: 8}) ?? "",
    /nests 4001 levels; the limit is 8/,
  );
});
