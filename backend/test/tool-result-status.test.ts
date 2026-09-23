import assert from "node:assert/strict";
import test from "node:test";

import { toolResultPartFromOutput } from "../src/plugins/web-ui/tool-read-web-ui.js";

const MCP_MARKER = "MCP server reported a tool error.";

function output(value: unknown): Record<string, unknown> {
  return { type: "function_call_output", call_id: "call_1", output: value };
}

test("a result that quotes the MCP error sentence is not an error", () => {
  // Regression: reading this plugin used to mark the call as failed because the
  // file itself contains the sentence the status check looked for.
  const file = [
    '{"id":"call_1","status":"ok","summary":"Read lines 1-95 from tool-read-web-ui.ts"}',
    "",
    "--- INFORMATION ---",
    "path: backend/src/plugins/web-ui/tool-read-web-ui.ts",
    "",
    "--- CONTENT ---",
    "function toolResultStatus(output: string): WebUiToolResultStatus {",
    `  if (output.includes("${MCP_MARKER}")) return "error";`,
    "}",
  ].join("\n");

  const part = toolResultPartFromOutput(output(file), 0);
  assert.equal(part?.status, "success");
});

test("a bash result that prints the sentence in its output is not an error", () => {
  const stdout = [
    '{"id":"call_1","status":"ok","summary":"Command completed in 8 ms"}',
    "",
    "--- INFORMATION ---",
    "cwd: .",
    "",
    "--- CONTENT ---",
    `src/plugins/web-ui/tool-read-web-ui.ts:14:  if (output.includes("${MCP_MARKER}")) return "error";`,
  ].join("\n");

  assert.equal(toolResultPartFromOutput(output(stdout), 0)?.status, "success");
});

test("the sentence as an output part is an MCP tool error", () => {
  assert.equal(
    toolResultPartFromOutput(output(MCP_MARKER), 0)?.status,
    "error",
  );
  assert.equal(
    toolResultPartFromOutput(output([{ type: "input_text", text: MCP_MARKER }]), 0)?.status,
    "error",
  );
});

test("an MCP error alongside content parts is still an error", () => {
  const part = toolResultPartFromOutput(
    output([
      { type: "input_text", text: MCP_MARKER },
      { type: "input_text", text: "EISDIR: illegal operation on a directory" },
    ]),
    0,
  );
  assert.equal(part?.status, "error");
});

test("an MCP result that quotes the sentence in content is not an error", () => {
  const part = toolResultPartFromOutput(
    output([{ type: "input_text", text: `notes:\n  - ${MCP_MARKER}\n` }]),
    0,
  );
  assert.equal(part?.status, "success");
});

test("metadata status decides for string outputs", () => {
  const cases: [string, string][] = [
    ['{"status":"ok","summary":"done"}\nbody', "success"],
    ['{"status":"error","summary":"Command exited with 1"}\nbody', "error"],
    ['{"status":"not_found","summary":"No exact match"}\nbody', "error"],
    ["plain text without metadata", "unknown"],
    ["", "unknown"],
  ];
  for (const [text, expected] of cases) {
    assert.equal(
      toolResultPartFromOutput(output(text), 0)?.status,
      expected,
      `output ${JSON.stringify(text.slice(0, 40))}`,
    );
  }
});
