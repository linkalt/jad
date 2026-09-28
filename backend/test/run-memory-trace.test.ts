import assert from "node:assert/strict";
import test from "node:test";

import {RunMemoryTrace} from "../src/run-memory-trace.js";

const tracePrefix = "[provider-memory] ";

function memory(rss: number): NodeJS.MemoryUsage {
  const current = process.memoryUsage();
  return {
    ...current,
    rss,
    heapTotal: rss + 10,
    heapUsed: rss - 10,
    external: rss / 10,
    arrayBuffers: rss / 20,
  };
}

function records(lines: string[]): Record<string, unknown>[] {
  return lines.map((line) => {
    assert.ok(line.startsWith(tracePrefix));
    return JSON.parse(line.slice(tracePrefix.length)) as Record<string, unknown>;
  });
}

test("samples process memory and tracks overlapping provider runs without content", () => {
  const readings = [100, 120, 450, 300, 250, 170];
  const lines: string[] = [];
  const trace = new RunMemoryTrace({
    enabled: true,
    sampleIntervalMs: 60_000,
    readMemory: () => memory(readings.shift() ?? 170),
    writeLine: (line) => lines.push(line),
    now: () => 1_000,
  });

  const first = trace.beginRun();
  const second = trace.beginRun();
  trace.sampleNow();
  first.mark("request-built", {round: 0, history_items: 8});
  first.stop();
  second.stop();
  trace.sampleNow();

  const output = records(lines);
  const starts = output.filter((record) => record.event === "start");
  const sample = output.find((record) => record.event === "sample");
  const phase = output.find((record) => record.event === "phase");
  const ends = output.filter((record) => record.event === "end");

  assert.equal(starts.length, 2);
  assert.deepEqual(starts.map((record) => record.active_runs), [1, 2]);
  assert.deepEqual(sample?.run_ids, [1, 2]);
  assert.equal((sample?.memory as {rss_bytes: number}).rss_bytes, 450);
  assert.equal(phase?.phase, "request-built");
  assert.deepEqual(phase?.details, {round: 0, history_items: 8});
  assert.equal(ends.length, 2);
  assert.equal(ends[0]?.active_runs, 1);
  assert.equal(ends[1]?.active_runs, 0);
  assert.equal((ends[0]?.peak_memory as {rss_bytes: number}).rss_bytes, 450);
  assert.equal((ends[1]?.peak_memory as {rss_bytes: number}).rss_bytes, 450);
  assert.equal(
    output.some((record) => Object.hasOwn(record, "session_id") || Object.hasOwn(record, "prompt")),
    false,
  );
});

test("disabled tracing does not read memory or emit logs", () => {
  let reads = 0;
  const lines: string[] = [];
  const trace = new RunMemoryTrace({
    enabled: false,
    sampleIntervalMs: 250,
    readMemory: () => {
      reads += 1;
      return memory(100);
    },
    writeLine: (line) => lines.push(line),
  });

  const run = trace.beginRun();
  run.mark("request-built", {history_items: 1});
  run.stop();
  trace.sampleNow();

  assert.equal(reads, 0);
  assert.deepEqual(lines, []);
});
