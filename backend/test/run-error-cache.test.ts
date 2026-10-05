import assert from "node:assert/strict";
import {mkdir, mkdtemp, readFile, rm, stat, writeFile} from "node:fs/promises";
import {tmpdir} from "node:os";
import {join} from "node:path";
import test from "node:test";

import {SessionResponseStore} from "../src/persistence.js";
import {AgentSession} from "../src/session.js";
import {
  appendRunError,
  readRunErrors,
  type RunErrorEntry,
} from "../src/run-error-cache.js";

async function tempDir(): Promise<string> {
  return mkdtemp(join(tmpdir(), "agent-harness-run-error-cache-"));
}

function entry(turn: number, message = `failure ${turn}`): RunErrorEntry {
  return {turn, turn_id: `turn-${turn}`, at: "2026-01-01T00:00:00.000Z", message};
}

const plainWrite = async (path: string, value: unknown): Promise<void> => {
  await writeFile(path, `${JSON.stringify(value)}\n`);
};

test("a missing, corrupt or foreign-version cache reads as empty", async () => {
  const dir = await tempDir();
  try {
    const path = join(dir, "run-error-cache.json");
    assert.deepEqual(await readRunErrors(path), []);

    await writeFile(path, "{not json");
    assert.deepEqual(await readRunErrors(path), []);

    await writeFile(path, JSON.stringify({schema_version: 99, errors: [entry(1)]}));
    assert.deepEqual(await readRunErrors(path), []);

    await writeFile(path, JSON.stringify({schema_version: 1, errors: "nope"}));
    assert.deepEqual(await readRunErrors(path), []);

    // Entries that do not match the shape are dropped, valid ones are kept.
    await writeFile(
      path,
      JSON.stringify({schema_version: 1, errors: [entry(2), {turn: 0, turn_id: "x", at: "x", message: "x"}, {turn: "3"}, null]}),
    );
    assert.deepEqual(await readRunErrors(path), [entry(2)]);
  } finally {
    await rm(dir, {recursive: true, force: true});
  }
});

test("appends in turn order, replaces a repeated turn and drops the oldest past the cap", async () => {
  const dir = await tempDir();
  try {
    const path = join(dir, "nested", "run-error-cache.json");
    await appendRunError(path, entry(3), 3, plainWrite);
    await appendRunError(path, entry(1), 3, plainWrite);
    assert.deepEqual((await readRunErrors(path)).map((item) => item.turn), [1, 3]);

    // The same turn failing again replaces its earlier record.
    await appendRunError(path, entry(3, "second failure"), 3, plainWrite);
    const afterRetry = await readRunErrors(path);
    assert.equal(afterRetry.length, 2);
    assert.equal(afterRetry.find((item) => item.turn === 3)?.message, "second failure");

    await appendRunError(path, entry(4), 3, plainWrite);
    await appendRunError(path, entry(5), 3, plainWrite);
    assert.deepEqual((await readRunErrors(path)).map((item) => item.turn), [3, 4, 5]);
  } finally {
    await rm(dir, {recursive: true, force: true});
  }
});

async function openStore(dir: string, maxEntries?: number): Promise<SessionResponseStore> {
  return SessionResponseStore.open(dir, {
    ...(maxEntries === undefined ? {} : {runErrorCacheMaxEntries: maxEntries}),
  });
}

test("the store keeps failures beside the transcript and reset removes them", async () => {
  const dir = await tempDir();
  try {
    const store = await openStore(dir);
    try {
      await store.appendInputItem({type: "message", role: "user", content: [{type: "input_text", text: "hello"}]});
      const transcriptBefore = await readFile(store.messagesPath, "utf8");

      await store.recordRunError(entry(1));
      assert.deepEqual(await store.readRunErrors(), [entry(1)]);
      assert.equal(await readFile(store.messagesPath, "utf8"), transcriptBefore, "recording a failure must not touch messages.jsonl");

      await store.reset();
      assert.deepEqual(await store.readRunErrors(), []);
      await assert.rejects(() => stat(store.runErrorCachePath), {code: "ENOENT"});
    } finally {
      await store.close();
    }
  } finally {
    await rm(dir, {recursive: true, force: true});
  }
});

test("appending input reports the byte offset of the last visible user message", async () => {
  const dir = await tempDir();
  try {
    const store = await openStore(dir);
    try {
      const user = (text: string) => ({type: "message", role: "user", content: [{type: "input_text", text}]}) as const;
      const first = await store.appendInputItems([user("first")]);
      assert.equal(first.messageOffset, 0);

      // The offset is where the record starts, so it equals the transcript size before the append.
      const sizeBefore = (await stat(store.messagesPath)).size;
      const second = await store.appendInputItems([user("second")]);
      assert.equal(second.messageOffset, sizeBefore);

      // A batch of several visible items points at the last one.
      const sizeMid = (await stat(store.messagesPath)).size;
      const batch = await store.appendInputItems([user("a"), user("b")]);
      const lines = (await readFile(store.messagesPath, "utf8")).split("\n").filter((line) => line.length > 0);
      const lastLine = lines[lines.length - 1] ?? "";
      assert.ok(batch.messageOffset !== undefined && batch.messageOffset > sizeMid);
      assert.equal((await stat(store.messagesPath)).size, batch.messageOffset + Buffer.byteLength(lastLine) + 1);

      // Input the projection would not show as a message has no offset to point at.
      const imageOnly = await store.appendInputItems([
        {type: "message", role: "user", content: [{type: "input_image", image_url: "data:image/png;base64,AA=="}]},
      ]);
      assert.equal(imageOnly.messageOffset, undefined);
    } finally {
      await store.close();
    }
  } finally {
    await rm(dir, {recursive: true, force: true});
  }
});

test("the store enforces the configured entry cap", async () => {
  const dir = await tempDir();
  try {
    const store = await openStore(dir, 2);
    try {
      for (const turn of [1, 2, 3]) await store.recordRunError(entry(turn));
      assert.deepEqual((await store.readRunErrors()).map((item) => item.turn), [2, 3]);
    } finally {
      await store.close();
    }
    await assert.rejects(() => openStore(dir, 0), /runErrorCacheMaxEntries must be a safe integer/);
  } finally {
    await rm(dir, {recursive: true, force: true});
  }
});

test("front compaction removes failure entries whose turn numbers it re-bases", async () => {
  const dir = await tempDir();
  try {
    const store = await openStore(dir);
    try {
      await store.appendInputItem({type: "message", role: "user", content: [{type: "input_text", text: "first"}]});
      await store.recordRunError(entry(1));
      assert.equal((await store.readRunErrors()).length, 1);

      const messageId = "session-user-o0";
      await store.frontCompactFromMessage(messageId, "permanent");
      assert.deepEqual(await store.readRunErrors(), []);
    } finally {
      await store.close();
    }
  } finally {
    await rm(dir, {recursive: true, force: true});
  }
});

test("history compaction removes failure entries only when it rewrites the transcript", async () => {
  const dir = await tempDir();
  try {
    const store = await openStore(dir);
    try {
      const user = {type: "message", role: "user", content: [{type: "input_text", text: "question"}]} as const;
      const reasoning = {type: "reasoning", summary: [{type: "summary_text", text: "long reasoning ".repeat(100)}]};
      const answer = {type: "message", role: "assistant", content: [{type: "output_text", text: "answer"}]};
      await store.appendInputItem(user);
      await store.recordRunError(entry(1));

      // Nothing is compactable yet, so the transcript and the failure record stay as they are.
      const untouched = await store.compactHistory({turnType: "assistant_final", keepTurns: 1, keepToolPairs: 0, backupMode: "replace"});
      assert.equal(untouched.compacted, false);
      assert.deepEqual(await store.readRunErrors(), [entry(1)]);

      const turn = await store.beginTurn("compact-with-failure");
      await turn.stageOutputItem({type: "response.output_item.done", output_index: 0, item: reasoning});
      await turn.stageOutputItem({type: "response.output_item.done", output_index: 1, item: answer});
      await turn.commit({
        type: "response.completed",
        response: {
          id: "resp-compact-with-failure",
          object: "response",
          status: "completed",
          model: "gpt-test",
          usage: {input_tokens: 10, output_tokens: 4},
          output: [reasoning, answer],
        },
      });
      assert.deepEqual(await store.readRunErrors(), [entry(1)]);

      // A compaction that rewrites the transcript re-bases turn numbers, so the record goes.
      const compacted = await store.compactHistory({turnType: "assistant_final", keepTurns: 1, keepToolPairs: 0, backupMode: "replace"});
      assert.equal(compacted.compacted, true);
      assert.deepEqual(await store.readRunErrors(), []);
      await assert.rejects(() => stat(store.runErrorCachePath), {code: "ENOENT"});
    } finally {
      await store.close();
    }
  } finally {
    await rm(dir, {recursive: true, force: true});
  }
});

test("a run with image-only input reports its turn without a message offset", async () => {
  const dir = await tempDir();
  try {
    await writeFile(
      join(dir, "model.json"),
      JSON.stringify({schema_version: 1, provider: "fake", api_key_env: "FAKE_API_KEY", model: "fake-model"}),
    );
    await writeFile(
      join(dir, "setting.json"),
      JSON.stringify({schema_version: 1, session_name: "image-only", cwd: dir, tool_definition_paths: ["missing-tools.json"]}),
    );
    const session = await AgentSession.open(dir);
    try {
      const accepted: Array<{turn: number; offset: number | undefined}> = [];
      const options = {onInputAccepted: (turn: number, offset: number | undefined) => { accepted.push({turn, offset}); }};

      // Both runs fail on the missing tool file, after their input has been saved.
      const imageOnly = {type: "message", role: "user", content: [{type: "input_image", image_url: "data:image/png;base64,AA=="}]};
      await assert.rejects(() => session.run(imageOnly, options));
      await assert.rejects(() => session.run("with text", options));

      assert.equal(accepted.length, 2);
      assert.deepEqual(accepted[0], {turn: 1, offset: undefined});
      assert.equal(accepted[1]?.turn, 2);
      assert.equal(typeof accepted[1]?.offset, "number");

      // The record can be written without a message id, and the cache keeps it as is.
      await session.recordRunError({turn: 1, turn_id: "image-turn", at: "2026-01-01T00:00:00.000Z", message: "failed"});
      const [only] = await session.readRunErrors();
      assert.equal(only?.turn, 1);
      assert.equal(Object.hasOwn(only ?? {}, "message_id"), false);
    } finally {
      await session.close();
    }
  } finally {
    await rm(dir, {recursive: true, force: true});
  }
});

test("a failure written through a missing staging directory still succeeds", async () => {
  const dir = await tempDir();
  try {
    await mkdir(dir, {recursive: true});
    const path = join(dir, "does", "not", "exist", "run-error-cache.json");
    await appendRunError(path, entry(1), 5, plainWrite);
    assert.deepEqual(await readRunErrors(path), [entry(1)]);
  } finally {
    await rm(dir, {recursive: true, force: true});
  }
});
