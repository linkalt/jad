import assert from "node:assert/strict";
import { mkdir, mkdtemp, readFile, rename, rm, stat, writeFile } from "node:fs/promises";
import { appendFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";

import { SessionResponseStore } from "../src/persistence.js";
import { normalizeWebUiSession } from "../src/plugins/web-ui/normalize-web-ui.js";
import { WebUiSessionViewCache } from "../src/plugins/web-ui/view-cache.js";
import type { JsonRecord, ResponseInputItem } from "../src/persistence.js";

function userRecord(text: string): JsonRecord {
  return {
    type: "message",
    role: "user",
    content: [{ type: "input_text", text }],
  };
}

function assistantItem(text: string, id = `msg-${text}`): JsonRecord {
  return {
    id,
    type: "message",
    role: "assistant",
    content: [{ type: "output_text", text }],
  };
}

function callItem(callId: string, name = "read", args = "{}"): JsonRecord {
  return { type: "function_call", call_id: callId, name, arguments: args };
}

function toolOutput(callId: string, output = "ok"): JsonRecord {
  return { type: "function_call_output", call_id: callId, output };
}

function reasoningItem(text: string): JsonRecord {
  return { type: "reasoning", summary: [{ type: "summary_text", text }] };
}

function completedRecord(
  responseId: string,
  turn: number,
  step: number,
  output: JsonRecord[],
): JsonRecord {
  return {
    type: "response.completed",
    session_turn: turn,
    session_step: step,
    response: { id: responseId, status: "completed", output },
  };
}

/** A transcript with a tool round, two turns and a trailing torn line. */
const TRANSCRIPT: JsonRecord[] = [
  userRecord("first question"),
  completedRecord("r-1-1", 1, 1, [callItem("call-1"), assistantItem("working", "msg-1-1")]),
  toolOutput("call-1", "file contents"),
  completedRecord("r-1-2", 1, 2, [assistantItem("first answer", "msg-1-2")]),
  userRecord("second question"),
  completedRecord("r-2-1", 2, 1, [assistantItem("second answer", "msg-2-1")]),
];

async function writeTranscript(path: string, records: readonly JsonRecord[]): Promise<void> {
  await writeFile(path, records.map((record) => `${JSON.stringify(record)}\n`).join(""));
}

async function makeStore(
  records: readonly JsonRecord[] = TRANSCRIPT,
): Promise<{store: SessionResponseStore; sessionDir: string; messagesPath: string}> {
  const sessionDir = await mkdtemp(join(tmpdir(), "agent-harness-cursor-"));
  const messagesPath = join(sessionDir, "messages.jsonl");
  await writeTranscript(messagesPath, records);
  const store = await SessionResponseStore.open(sessionDir, { messagesPath });
  return { store, sessionDir, messagesPath };
}

/**
 * Message content without ids: a full read numbers synthetic ids by record
 * index, an incremental read by byte offset, and both are correct for their own
 * transcript slice.
 */
function plain(messages: readonly { id: string; sender: string; text: string }[]): unknown {
  return messages.map(({ id: _id, ...rest }) => rest);
}

function messagesOf(records: readonly JsonRecord[]): unknown {
  return normalizeWebUiSession(
    "session1",
    {
      model: { provider: "fake", model: "fake-model" },
      settings: { session_name: "cursor" },
    } as never,
    records,
  ).messages;
}

test("indexes one cursor per completed response and reads it back from disk", async () => {
  const { store, sessionDir, messagesPath } = await makeStore();
  try {
    const index = await store.readCursorIndex();
    assert.deepEqual(
      index.runs.map((run) => ({ turn: run.turn, step: run.step })),
      [
        { turn: 1, step: 1 },
        { turn: 1, step: 2 },
        { turn: 2, step: 1 },
      ],
    );
    // Offsets point at the record that opens each run, and each run extends to
    // the next one, so no length has to be stored.
    const contents = await readFile(messagesPath, "utf8");
    for (const run of index.runs) {
      const record = JSON.parse(contents.slice(run.offset).split("\n")[0]!) as JsonRecord;
      assert.equal(record.type, "response.completed");
    }
    assert.equal(index.size, (await stat(messagesPath)).size);

    // The index is persisted beside the other staging caches.
    const cached = JSON.parse(
      await readFile(join(sessionDir, "staging", "cursor-cache.json"), "utf8"),
    ) as { schema_version: number; runs: unknown[] };
    assert.equal(cached.schema_version, 1);
    assert.equal(cached.runs.length, 3);
  } finally {
    await store.close();
    await rm(sessionDir, { recursive: true, force: true });
  }
});

test("reads only appended bytes and skips work when nothing changed", async () => {
  const { store, sessionDir, messagesPath } = await makeStore();
  try {
    await store.readCursorIndex();
    const cachePath = join(sessionDir, "staging", "cursor-cache.json");
    const first = await stat(cachePath);

    // An unchanged transcript must not rewrite the cache.
    await store.readCursorIndex();
    assert.equal((await stat(cachePath)).mtimeMs, first.mtimeMs);

    // A tool output extends the last run's extent and adds no entry.
    await appendFile(messagesPath, `${JSON.stringify(toolOutput("call-2", "more"))}\n`);
    const afterToolOutput = await store.readCursorIndex();
    assert.equal(afterToolOutput.runs.length, 3);

    await appendFile(
      messagesPath,
      `${JSON.stringify(completedRecord("r-2-2", 2, 2, [assistantItem("third", "msg-2-2")]))}\n`,
    );
    const afterRun = await store.readCursorIndex();
    assert.equal(afterRun.runs.length, 4);
    assert.deepEqual(
      afterRun.runs.map((run) => run.step),
      [1, 2, 1, 2],
    );
  } finally {
    await store.close();
    await rm(sessionDir, { recursive: true, force: true });
  }
});

test("indexes records written with human-readable JSON spacing", async () => {
  const { store, sessionDir, messagesPath } = await makeStore([]);
  try {
    // Another writer may pretty-print its records; the scan must not depend on
    // this store's compact serialization.
    const spaced = [
      JSON.stringify(userRecord("spaced question"), null, 0).replace('"type":"message"', '"type": "message"'),
      JSON.stringify(completedRecord("r-1-1", 1, 1, [assistantItem("spaced answer")]))
        .replace('"type":"response.completed"', '"type": "response.completed"'),
    ];
    await writeFile(messagesPath, `${spaced.join("\n")}\n`);
    const index = await store.readCursorIndex();
    assert.deepEqual(index.runs.map((run) => ({ turn: run.turn, step: run.step })), [
      { turn: 1, step: 1 },
    ]);
  } finally {
    await store.close();
    await rm(sessionDir, { recursive: true, force: true });
  }
});

test("leaves a torn trailing record for the next read", async () => {
  const { store, sessionDir, messagesPath } = await makeStore();
  try {
    const complete = `${JSON.stringify(completedRecord("r-3-1", 3, 1, []))}\n`;
    await appendFile(messagesPath, `${complete.slice(0, complete.length - 20)}`);
    const torn = await store.readCursorIndex();
    assert.equal(torn.runs.length, 3);

    await appendFile(messagesPath, `${complete.slice(complete.length - 20)}`);
    const repaired = await store.readCursorIndex();
    assert.equal(repaired.runs.length, 4);
    assert.equal(repaired.runs[3]?.turn, 3);
  } finally {
    await store.close();
    await rm(sessionDir, { recursive: true, force: true });
  }
});

test("rebuilds after the transcript is replaced or truncated", async () => {
  const { store, sessionDir, messagesPath } = await makeStore();
  try {
    assert.equal((await store.readCursorIndex()).runs.length, 3);

    // A compaction replaces the file by rename, so the inode changes.
    const replacement = records([
      completedRecord("r-9-1", 9, 1, [assistantItem("after compaction")]),
    ]);
    await writeFile(`${messagesPath}.next`, replacement);
    await rename(`${messagesPath}.next`, messagesPath);
    const replaced = await store.readCursorIndex();
    assert.equal(replaced.runs.length, 1);

    await writeFile(messagesPath, "");
    const truncated = await store.readCursorIndex();
    assert.deepEqual(truncated.runs, []);
  } finally {
    await store.close();
    await rm(sessionDir, { recursive: true, force: true });
  }
});

function records(items: readonly JsonRecord[]): string {
  return items.map((record) => `${JSON.stringify(record)}\n`).join("");
}

test("ignores records without a session turn and text that mimics one", async () => {
  const { store, sessionDir, messagesPath } = await makeStore([
    userRecord('a prompt that mentions {"type":"response.completed"}'),
    // A tool output quoting the marker must not be mistaken for a run.
    {
      type: "function_call_output",
      call_id: "call-quoted",
      output: '{"type":"response.completed","session_turn":9,"session_step":9}',
    },
    { type: "response.completed", response: { id: "no-turn", output: [] } },
    completedRecord("r-1-1", 1, 1, [assistantItem("answer")]),
  ]);
  try {
    const index = await store.readCursorIndex();
    assert.deepEqual(index.runs.map((run) => run.step), [1]);
  } finally {
    await store.close();
    await rm(sessionDir, { recursive: true, force: true });
  }
});

test("projects the transcript identically to a full normalization", async () => {
  const { store, sessionDir, messagesPath } = await makeStore();
  try {
    const cache = new WebUiSessionViewCache(store, {
      snapshotTurns: 0,
      maxPageTurns: 50,
    });
    const view = await cache.read();
    assert.deepEqual(plain(view.messages), plain(messagesOf(TRANSCRIPT) as never));
    assert.equal(view.hasMore, false);
    assert.equal(view.nextBefore, null);
    assert.equal((await stat(messagesPath)).size > 0, true);
  } finally {
    await store.close();
    await rm(sessionDir, { recursive: true, force: true });
  }
});

test("pages the transcript by turn and tiles it exactly", async () => {
  const { store, sessionDir, messagesPath } = await makeStore();
  try {
    const cache = new WebUiSessionViewCache(store, {
      snapshotTurns: 1,
      maxPageTurns: 50,
    });

    // The snapshot is capped to its newest turn and reports where to continue.
    const newest = await cache.read();
    assert.equal(newest.hasMore, true);
    assert.equal(typeof newest.nextBefore, "number");
    const newestOnly = messagesOf([TRANSCRIPT[4]!, TRANSCRIPT[5]!]);
    assert.deepEqual(plain(newest.messages), plain(newestOnly as never));

    // Following the cursor yields the older turn, and then the transcript start.
    const older = await cache.read({ before: newest.nextBefore! });
    assert.deepEqual(plain(older.messages), plain(messagesOf(TRANSCRIPT.slice(0, 4)) as never));
    assert.equal(older.hasMore, false);
    assert.equal(older.nextBefore, null);

    // Union of pages equals the whole transcript.
    assert.deepEqual(plain([...older.messages, ...newest.messages]), plain(messagesOf(TRANSCRIPT) as never));

    const cursor = await store.readCursorIndex();
    assert.equal(cursor.runs.length, 3);
    assert.equal(await stat(messagesPath).then((info) => info.size > 0), true);
  } finally {
    await store.close();
    await rm(sessionDir, { recursive: true, force: true });
  }
});

test("keeps message ids stable between a page and a full read", async () => {
  const { store, sessionDir } = await makeStore();
  try {
    const cache = new WebUiSessionViewCache(store, {
      snapshotTurns: 1,
      maxPageTurns: 50,
    });
    const page = await cache.read();
    const whole = await cache.read({ turns: 0 });
    const wholeIds = new Set(whole.messages.map((message) => message.id));
    for (const message of page.messages) {
      assert.equal(wholeIds.has(message.id), true, message.id);
    }
    assert.deepEqual(
      whole.messages.slice(-page.messages.length),
      page.messages,
    );
  } finally {
    await store.close();
    await rm(sessionDir, { recursive: true, force: true });
  }
});

test("renders compacted history from the backup, keeping reasoning and tool calls", async () => {
  const sessionDir = await mkdtemp(join(tmpdir(), "agent-harness-view-compaction-"));
  const messagesPath = join(sessionDir, "messages.jsonl");
  const records: JsonRecord[] = [
    userRecord("first question"),
    completedRecord("r-1-1", 1, 1, [reasoningItem("first thinking"), callItem("call-1")]),
    toolOutput("call-1", "file contents"),
    completedRecord("r-1-2", 1, 2, [assistantItem("first answer", "msg-1-2")]),
  ];
  await writeTranscript(messagesPath, records);
  const store = await SessionResponseStore.open(sessionDir, { messagesPath });
  try {
    await store.compactHistory({
      turnType: "assistant_final",
      keepTurns: 1,
      keepToolPairs: 0,
      backupMode: "replace",
    });
    // The compacted request items keep the user message and the final answer,
    // but not the reasoning or the tool round — which is exactly what the view
    // has to keep showing, so it reads the backup instead.
    const items = (await store.readCompactionItems()) ?? [];
    assert.equal(items.some((item) => item.type === "reasoning"), false);
    assert.equal(items.some((item) => item.type === "function_call"), false);

    const cache = new WebUiSessionViewCache(store, {
      snapshotTurns: 0,
      maxPageTurns: 50,
    });
    const view = await cache.read();
    assert.deepEqual(plain(view.messages), plain(messagesOf(records) as never));
    assert.equal(
      view.messages.some((message) => message.parts.some((part) => part.type === "reasoning")),
      true,
    );
    assert.equal(
      view.messages.some((message) => message.parts.some((part) => part.type === "tool_result")),
      true,
    );
  } finally {
    await store.close();
    await rm(sessionDir, { recursive: true, force: true });
  }
});

test("trims the backup at the seam so a preserved response stays one message", async () => {
  const sessionDir = await mkdtemp(join(tmpdir(), "agent-harness-view-seam-"));
  const messagesPath = join(sessionDir, "messages.jsonl");
  const records: JsonRecord[] = [
    userRecord("do the work"),
    completedRecord("run-1", 1, 1, [reasoningItem("first thinking"), callItem("call-1")]),
    toolOutput("call-1", "tool result"),
    completedRecord("run-2", 1, 2, [
      reasoningItem("second thinking"),
      assistantItem("draft answer", "msg-1-2"),
    ]),
  ];
  await writeTranscript(messagesPath, records);
  const store = await SessionResponseStore.open(sessionDir, { messagesPath });
  try {
    // A mid-turn compaction keeps the in-flight response in the transcript and
    // leaves the very same record at the end of the backup.
    const compaction = await store.compactHistory({
      turnType: "assistant_final",
      keepTurns: 1,
      keepToolPairs: 1,
      backupMode: "replace",
      preserveResponseId: "run-2",
    });
    assert.equal(compaction.compacted, true);
    assert.equal((await readFile(messagesPath, "utf8")).startsWith('{"type":"response.completed"'), true);

    const cache = new WebUiSessionViewCache(store, {
      snapshotTurns: 0,
      maxPageTurns: 50,
    });
    const view = await cache.read();
    // run-2 comes from the transcript, everything before it from the backup.
    // Without the seam the same record would be rendered twice.
    assert.deepEqual(
      view.messages.map((message) => message.sender),
      ["user", "assistant", "assistant"],
    );
    assert.equal(view.messages.filter((message) => message.id === "run-2").length, 1);
    assert.equal(view.messages.filter((message) => message.text === "draft answer").length, 1);
    assert.equal(
      view.messages[1]?.parts.some((part) => part.type === "tool_result"),
      true,
    );
  } finally {
    await store.close();
    await rm(sessionDir, { recursive: true, force: true });
  }
});

test("caps the rendered backup at keepbackup.turns", async () => {
  const sessionDir = await mkdtemp(join(tmpdir(), "agent-harness-view-backup-cap-"));
  const messagesPath = join(sessionDir, "messages.jsonl");
  await writeTranscript(messagesPath, TRANSCRIPT);
  const store = await SessionResponseStore.open(sessionDir, { messagesPath });
  try {
    await store.compactHistory({
      turnType: "assistant_final",
      keepTurns: 1,
      keepToolPairs: 0,
      backupMode: "replace",
    });
    const cache = new WebUiSessionViewCache(store, {
      snapshotTurns: 0,
      maxPageTurns: 50,
    });
    // The transcript is empty after compaction, so the backup is the history.
    const whole = await cache.read({backupTurns: 20});
    assert.deepEqual(plain(whole.messages), plain(messagesOf(TRANSCRIPT) as never));

    const capped = await cache.read({backupTurns: 1});
    assert.deepEqual(
      plain(capped.messages),
      plain(messagesOf([TRANSCRIPT[4]!, TRANSCRIPT[5]!]) as never),
    );
    assert.equal(capped.messages[0]?.text, "second question");

    const hidden = await cache.read({backupTurns: 0});
    assert.deepEqual(hidden.messages, []);
    assert.equal(hidden.hasMore, false);
  } finally {
    await store.close();
    await rm(sessionDir, { recursive: true, force: true });
  }
});

test("serves the whole transcript when the snapshot cap is disabled", async () => {
  const { store, sessionDir } = await makeStore();
  try {
    const cache = new WebUiSessionViewCache(store, {
      snapshotTurns: 0,
      maxPageTurns: 50,
    });
    const view = await cache.read();
    assert.equal(view.messages.length, (messagesOf(TRANSCRIPT) as unknown[]).length);
    assert.equal(view.hasMore, false);
  } finally {
    await store.close();
    await rm(sessionDir, { recursive: true, force: true });
  }
});

test("appends to an existing transcript without renumbering earlier messages", async () => {
  const { store, sessionDir, messagesPath } = await makeStore();
  try {
    const cache = new WebUiSessionViewCache(store, {
      snapshotTurns: 0,
      maxPageTurns: 50,
    });
    const before = await cache.read();
    const appended: JsonRecord[] = [
      userRecord("third question"),
      completedRecord("r-3-1", 3, 1, [assistantItem("third answer", "msg-3-1")]),
    ];
    await appendFile(messagesPath, records(appended));
    const after = await cache.read();
    assert.deepEqual(after.messages.slice(0, before.messages.length), before.messages);
    assert.deepEqual(plain(after.messages), plain(messagesOf([...TRANSCRIPT, ...appended]) as never));
  } finally {
    await store.close();
    await rm(sessionDir, { recursive: true, force: true });
  }
});

test("reads an empty transcript and a session without compaction as empty", async () => {
  const sessionDir = await mkdtemp(join(tmpdir(), "agent-harness-view-empty-"));
  const messagesPath = join(sessionDir, "messages.jsonl");
  await mkdir(join(sessionDir, "staging"), { recursive: true });
  const store = await SessionResponseStore.open(sessionDir, { messagesPath });
  try {
    const cache = new WebUiSessionViewCache(store, {
      snapshotTurns: 24,
      maxPageTurns: 50,
    });
    const view = await cache.read();
    assert.deepEqual(view.messages, []);
    assert.equal(view.hasMore, false);
    assert.deepEqual((await store.readCursorIndex()).runs, []);
    assert.equal(await store.readBackupStamp(), undefined);
    assert.equal(await store.readBackupTail(20), undefined);
  } finally {
    await store.close();
    await rm(sessionDir, { recursive: true, force: true });
  }
});

test("keeps the compaction prefix while the tail grows", async () => {
  const sessionDir = await mkdtemp(join(tmpdir(), "agent-harness-view-head-"));
  const messagesPath = join(sessionDir, "messages.jsonl");
  await writeTranscript(messagesPath, TRANSCRIPT);
  const store = await SessionResponseStore.open(sessionDir, { messagesPath });
  try {
    const compaction = await store.compactHistory({
      turnType: "assistant_final",
      keepTurns: 2,
      keepToolPairs: 0,
      backupMode: "replace",
    });
    assert.equal(compaction.compacted, true);
    const cache = new WebUiSessionViewCache(store, {
      snapshotTurns: 24,
      maxPageTurns: 50,
    });
    const withHead = await cache.read();
    // The head supplies the retained turns; the tail continues after them.
    const items: ResponseInputItem[] = (await store.readCompactionItems()) ?? [];
    assert.equal(items.length > 0, true);
    assert.equal(withHead.messages.length > 0, true);
    await appendFile(
      messagesPath,
      records([completedRecord("r-9-1", 9, 1, [assistantItem("tail answer", "msg-9-1")])]),
    );
    const grown = await cache.read();
    assert.deepEqual(grown.messages.slice(0, withHead.messages.length), withHead.messages);
    assert.equal(grown.messages.at(-1)?.id, "r-9-1");
  } finally {
    await store.close();
    await rm(sessionDir, { recursive: true, force: true });
  }
});
