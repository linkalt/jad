import assert from "node:assert/strict";
import test from "node:test";

import {runToolBatch} from "../src/tool-batch.js";

function delay(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

test("runs calls concurrently up to the limit and reports each as it finishes", async () => {
  let active = 0;
  let peak = 0;
  const finished: number[] = [];
  const outcome = await runToolBatch(
    [0, 1, 2, 3, 4, 5],
    3,
    async (call) => {
      active += 1;
      peak = Math.max(peak, active);
      // Later calls finish first, so completion order differs from call order.
      await delay((6 - call) * 5);
      active -= 1;
      return call * 10;
    },
    (call, output) => {
      assert.equal(output, call * 10);
      finished.push(call);
    },
  );
  assert.equal(outcome.failed, false);
  assert.equal(peak, 3);
  assert.deepEqual([...finished].sort(), [0, 1, 2, 3, 4, 5]);
  assert.notDeepEqual(finished, [0, 1, 2, 3, 4, 5]);
});

test("a limit of one preserves strict sequential order", async () => {
  const order: string[] = [];
  await runToolBatch(
    ["a", "b", "c"],
    1,
    async (call) => {
      order.push(`start ${call}`);
      await delay(5);
      order.push(`end ${call}`);
      return call;
    },
    () => undefined,
  );
  assert.deepEqual(order, ["start a", "end a", "start b", "end b", "start c", "end c"]);
});

test("a failure starts no further calls and lets in-flight calls settle", async () => {
  const started: number[] = [];
  const finished: number[] = [];
  const outcome = await runToolBatch(
    [0, 1, 2, 3],
    2,
    async (call) => {
      started.push(call);
      if (call === 0) throw new Error("boom");
      await delay(20);
      return call;
    },
    (call) => {
      finished.push(call);
    },
  );
  assert.equal(outcome.failed, true);
  assert.equal((outcome.error as Error).message, "boom");
  assert.deepEqual(started, [0, 1]);
  assert.deepEqual(finished, [1]);
});

test("shouldStop prevents later calls from starting", async () => {
  let stop = false;
  const started: number[] = [];
  await runToolBatch(
    [0, 1, 2],
    1,
    async (call) => {
      started.push(call);
      stop = true;
      return call;
    },
    () => undefined,
    () => stop,
  );
  assert.deepEqual(started, [0]);
});

test("an error thrown by the finished hook is reported like an execution error", async () => {
  const outcome = await runToolBatch(
    [0, 1],
    1,
    async (call) => call,
    () => {
      throw new Error("publish failed");
    },
  );
  assert.equal(outcome.failed, true);
  assert.equal((outcome.error as Error).message, "publish failed");
});

test("rejects a non-positive limit", async () => {
  await assert.rejects(
    () => runToolBatch([1], 0, async (call) => call, () => undefined),
    /positive safe integer/,
  );
});
