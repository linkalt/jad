import assert from "node:assert/strict";
import {mkdir, mkdtemp, readFile, rm, writeFile} from "node:fs/promises";
import {tmpdir} from "node:os";
import {join} from "node:path";
import test from "node:test";

import {DEFAULT_MAX_PARALLEL_TOOL_CALLS, SessionConfigLoader} from "../src/config.js";
import {runToolBatch} from "../src/tool-batch.js";
import {nativeEditDefinition} from "../src/tools/native/edit.js";
import {nativeReadDefinition} from "../src/tools/native/read.js";
import {loadToolCatalog} from "../src/tools/index.js";
import {firstLine, outputText} from "./tool-session.js";

interface Call {
  name: "edit" | "read";
  callId: string;
  args: Record<string, unknown>;
}

/** A session with the read and edit tools enabled, as a model response would see them. */
async function makeReadEditSession(): Promise<string> {
  const sessionDir = await mkdtemp(join(tmpdir(), "agent-harness-batch-order-"));
  await writeFile(join(sessionDir, "model.json"), JSON.stringify({model: "gpt-test"}));
  await writeFile(
    join(sessionDir, "tools.json"),
    JSON.stringify({schema_version: 1, tools: [nativeReadDefinition, nativeEditDefinition]}),
  );
  await writeFile(
    join(sessionDir, "setting.json"),
    JSON.stringify({
      cwd: sessionDir,
      tool_definition_paths: [join(sessionDir, "tools.json")],
      tools: {native: {read: {}, edit: {}}},
    }),
  );
  return sessionDir;
}

/**
 * Runs one provider response the way AgentSession does: runToolBatch over the
 * calls, each through the catalog, with the configured concurrency.
 */
async function runResponse(sessionDir: string, calls: readonly Call[], limit: number) {
  const catalog = await loadToolCatalog(await new SessionConfigLoader(sessionDir).load());
  const outputs = new Map<string, string>();
  const outcome = await runToolBatch(
    calls,
    limit,
    (call) =>
      catalog.executeFunctionCall(
        {type: "function_call", call_id: call.callId, name: call.name, arguments: JSON.stringify(call.args)},
        {workspaceRoot: sessionDir, sessionDir},
      ),
    (call, result) => {
      outputs.set(call.callId, outputText(result.output));
    },
  );
  assert.equal(outcome.failed, false);
  return outputs;
}

/** Three chained edits and a read of the same file, in the order a model writes them. */
function chainedCalls(path: string): Call[] {
  return [
    {name: "edit", callId: "e1", args: {path, oldText: "v0", newText: "v1"}},
    {name: "edit", callId: "e2", args: {path, oldText: "v1", newText: "v2"}},
    {name: "edit", callId: "e3", args: {path, oldText: "v2", newText: "v3"}},
    {name: "read", callId: "r1", args: {path}},
  ];
}

// Known defect: the calls of one response start together, so the read does not
// wait for the edits written before it, and the edits can reach the per-path
// lock out of order. Marked todo so it reports without failing the suite; remove
// `todo` when the scheduler honours the model's order (see the bug report
// "Tool calls of one response run concurrently with no ordering guarantee").
// Measured on 2026-10-05 at the default concurrency over 200 runs: the read saw
// the final content in 1, and the edits applied out of order in 3.
test(
  "edit+edit+edit+read of one file runs in the order the model wrote it",
  {todo: "tool calls of one response have no ordering guarantee"},
  async () => {
    const runs = 20;
    let readSawFinal = 0;
    let editsInOrder = 0;
    for (let run = 0; run < runs; run += 1) {
      const sessionDir = await makeReadEditSession();
      try {
        await mkdir(join(sessionDir, "src"));
        await writeFile(join(sessionDir, "src/a.txt"), "v0\n", "utf8");
        const outputs = await runResponse(sessionDir, chainedCalls("src/a.txt"), DEFAULT_MAX_PARALLEL_TOOL_CALLS);
        if (/v3/.test(outputs.get("r1")!)) readSawFinal += 1;
        const statuses = ["e1", "e2", "e3"].map((id) => firstLine(outputs.get(id)!).status);
        if (statuses.every((status) => status === "ok") && (await readFile(join(sessionDir, "src/a.txt"), "utf8")) === "v3\n") {
          editsInOrder += 1;
        }
      } finally {
        await rm(sessionDir, {recursive: true, force: true});
      }
    }
    assert.equal(readSawFinal, runs, `the read saw the final content in ${readSawFinal}/${runs} runs`);
    assert.equal(editsInOrder, runs, `the edits applied in order in ${editsInOrder}/${runs} runs`);
  },
);

test("a concurrency limit of one runs edit+edit+edit+read in order", async () => {
  const sessionDir = await makeReadEditSession();
  try {
    await mkdir(join(sessionDir, "src"));
    await writeFile(join(sessionDir, "src/a.txt"), "v0\n", "utf8");
    const outputs = await runResponse(sessionDir, chainedCalls("src/a.txt"), 1);
    assert.deepEqual(
      ["e1", "e2", "e3"].map((id) => firstLine(outputs.get(id)!).status),
      ["ok", "ok", "ok"],
    );
    assert.match(outputs.get("r1")!, /v3/);
    assert.equal(await readFile(join(sessionDir, "src/a.txt"), "utf8"), "v3\n");
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});
