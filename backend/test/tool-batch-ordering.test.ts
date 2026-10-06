import assert from "node:assert/strict";
import {mkdir, mkdtemp, readFile, rm, writeFile} from "node:fs/promises";
import {tmpdir} from "node:os";
import {join} from "node:path";
import test from "node:test";

import {DEFAULT_MAX_PARALLEL_TOOL_CALLS, SessionConfigLoader} from "../src/config.js";
import {runToolBatch, toolBatchConcurrency} from "../src/tool-batch.js";
import {nativeEditDefinition} from "../src/tools/native/edit.js";
import {nativeReadDefinition} from "../src/tools/native/read.js";
import {ToolCatalog} from "../src/tools/catalog.js";
import {loadToolCatalog} from "../src/tools/index.js";
import type {LoadedTool} from "../src/tools/types.js";
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
async function runResponse(
  sessionDir: string,
  calls: readonly Call[],
  limit: number,
  scheduling: "ordered" | "concurrent" = "ordered",
) {
  const catalog = await loadToolCatalog(await new SessionConfigLoader(sessionDir).load());
  const outputs = new Map<string, string>();
  const outcome = await runToolBatch(
    calls,
    toolBatchConcurrency(calls, limit, scheduling, (call) => catalog.changesState({name: call.name})),
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

// The calls of one response used to start together, so the read did not wait for
// the edits written before it and the edits could reach the per-path lock out of
// order (measured at the default concurrency over 200 runs: the read saw the final
// content in 1, the edits applied out of order in 3). With tool_call_scheduling
// "ordered" (the default) a response holding a call that can change state runs one
// call at a time in the model's order. The twin is
// backend-go/internal/session/tool_batch_ordering_test.go.
test(
  "edit+edit+edit+read of one file runs in the order the model wrote it",
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

test("the catalog treats read-only native tools as safe to run together and everything else as state-changing", async () => {
  const sessionDir = await makeReadEditSession();
  try {
    const catalog = await loadToolCatalog(await new SessionConfigLoader(sessionDir).load());
    assert.equal(catalog.changesState({name: "read"}), false);
    assert.equal(catalog.changesState({name: "edit"}), true);
    // An unknown tool runs nothing, so it never forces a serial run.
    assert.equal(catalog.changesState({name: "no_such_tool"}), false);
    assert.equal(catalog.changesState({}), false);
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("a tool that is not marked read-only counts as state-changing, as every MCP tool does", () => {
  const definition = {
    id: "mcp:srv:lookup", name: "lookup", description: "", inputSchema: {type: "object"}, source: "mcp",
  } as unknown as LoadedTool["definition"];
  const validate = (() => true) as unknown as LoadedTool["validate"];
  const execute = async () => ({output: ""}) as unknown as Awaited<ReturnType<LoadedTool["execute"]>>;
  const catalog = new ToolCatalog([
    {definition, validate, execute},
    {definition: {...definition, id: "native:probe", name: "probe", source: "native"}, validate, execute, readOnly: true},
  ]);
  assert.equal(catalog.changesState({name: "lookup"}), true);
  assert.equal(catalog.changesState({name: "probe"}), false);
});
