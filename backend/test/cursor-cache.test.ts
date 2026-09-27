import assert from "node:assert/strict";
import { mkdir, mkdtemp, readFile, rename, rm, stat, writeFile } from "node:fs/promises";
import { appendFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";

import {
  BACKUP_TAIL_WINDOW_BYTES,
  SESSION_INPUT_BATCH_ID_FIELD,
  SESSION_INPUT_BATCH_INDEX_FIELD,
  SessionResponseStore,
} from "../src/persistence.js";
import { normalizeWebUiSession } from "../src/plugins/web-ui/normalize-web-ui.js";
import { WebUiSessionViewCache } from "../src/plugins/web-ui/view-cache.js";
import type { JsonRecord, ResponseInputItem, SessionResponseStoreOptions } from "../src/persistence.js";

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
  options: Pick<SessionResponseStoreOptions, "cursorCacheCheckpointBytes"> = {},
): Promise<{store: SessionResponseStore; sessionDir: string; messagesPath: string}> {
  const sessionDir = await mkdtemp(join(tmpdir(), "agent-harness-cursor-"));
  const messagesPath = join(sessionDir, "messages.jsonl");
  await writeTranscript(messagesPath, records);
  const store = await SessionResponseStore.open(sessionDir, { messagesPath, ...options });
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

test("keeps cursor snapshots stable and public index results defensive", async () => {
  const {store, sessionDir, messagesPath} = await makeStore();
  try {
    const snapshot = await store.readCursorIndexSnapshot();
    assert.equal(snapshot.runs.length, 3);

    const defensive = await store.readCursorIndex();
    const firstRun = {...defensive.runs[0]!};
    defensive.runs[0]!.offset = -1;
    assert.deepEqual((await store.readCursorIndex()).runs[0], firstRun);

    await appendFile(
      messagesPath,
      `${JSON.stringify(completedRecord("r-2-2", 2, 2, [assistantItem("third")]))}\n`,
    );
    const updated = await store.readCursorIndexSnapshot();
    assert.equal(updated.runs.length, 4);
    assert.equal(snapshot.runs.length, 3);
    assert.deepEqual(snapshot.runs[0], firstRun);
  } finally {
    await store.close();
    await rm(sessionDir, {recursive: true, force: true});
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

test("checkpoints cursor cache after configured transcript growth and recovers stale state", async () => {
  const checkpointBytes = 512;
  const {store: initialStore, sessionDir, messagesPath} = await makeStore(TRANSCRIPT, {
    cursorCacheCheckpointBytes: checkpointBytes,
  });
  let activeStore: SessionResponseStore | undefined = initialStore;
  try {
    const initialIndex = await initialStore.readCursorIndexSnapshot();
    assert.equal(initialIndex.runs.length, 3);
    const cachePath = initialStore.cursorCachePath;
    const originalCheckpoint = await readFile(cachePath, "utf8");
    const firstAppend = `${JSON.stringify(
      completedRecord("r-3-1", 3, 1, [assistantItem("first", "msg-3-1")]),
    )}\n`;
    assert.ok(Buffer.byteLength(firstAppend) < checkpointBytes);

    await appendFile(messagesPath, firstAppend);
    const pendingIndex = await initialStore.readCursorIndexSnapshot();
    assert.equal(pendingIndex.runs.length, 4);
    assert.equal(await readFile(cachePath, "utf8"), originalCheckpoint);

    await initialStore.close();
    activeStore = undefined;
    const reopenedStore = await SessionResponseStore.open(sessionDir, {
      messagesPath,
      cursorCacheCheckpointBytes: checkpointBytes,
    });
    activeStore = reopenedStore;
    const recoveredIndex = await reopenedStore.readCursorIndexSnapshot();
    assert.deepEqual(
      recoveredIndex.runs.map(({turn, step}) => ({turn, step})),
      [
        {turn: 1, step: 1},
        {turn: 1, step: 2},
        {turn: 2, step: 1},
        {turn: 3, step: 1},
      ],
    );
    assert.equal(await readFile(cachePath, "utf8"), originalCheckpoint);

    const additionalRecords = [
      completedRecord("r-3-2", 3, 2, [assistantItem("second", "msg-3-2")]),
      completedRecord("r-3-3", 3, 3, [assistantItem("third", "msg-3-3")]),
      completedRecord("r-3-4", 3, 4, [assistantItem("fourth", "msg-3-4")]),
    ];
    const additionalBytes = `${additionalRecords.map((record) => JSON.stringify(record)).join("\n")}\n`;
    assert.ok(Buffer.byteLength(firstAppend + additionalBytes) >= checkpointBytes);
    await appendFile(messagesPath, additionalBytes);

    const checkpointedIndex = await reopenedStore.readCursorIndexSnapshot();
    assert.equal(checkpointedIndex.runs.length, 7);
    const checkpointedCache = JSON.parse(await readFile(cachePath, "utf8")) as {
      transcript: {offset: number};
    };
    assert.equal(
      checkpointedCache.transcript.offset,
      Buffer.byteLength(await readFile(messagesPath, "utf8")),
    );
  } finally {
    await activeStore?.close();
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("rejects invalid cursor-cache checkpoint byte options", async () => {
  const sessionDir = await mkdtemp(join(tmpdir(), "agent-harness-cursor-options-"));
  try {
    await assert.rejects(
      () => SessionResponseStore.open(sessionDir, {cursorCacheCheckpointBytes: 0}),
      /cursorCacheCheckpointBytes must be a safe integer/,
    );
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
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

test("projects the same messages whether or not records carry the provider request echo", async () => {
  const echo: JsonRecord = {
    tools: [{type: "function", name: "read", description: "request echo ".repeat(200)}],
    instructions: "system prompt echo",
    temperature: 1,
    top_p: 1,
    truncation: "disabled",
    tool_choice: "auto",
    metadata: {echo: true},
  };
  const withEcho = TRANSCRIPT.map((record) =>
    record.type === "response.completed"
      ? {...record, response: {...(record.response as JsonRecord), ...echo}}
      : record,
  );
  // The shape `persistence.response_metadata: "minimal"` writes.
  const minimal = TRANSCRIPT.map((record) => {
    if (record.type !== "response.completed") {
      return record;
    }
    const response = record.response as JsonRecord;
    return {
      ...record,
      response: {id: response.id, status: response.status, output: response.output},
    };
  });

  const echoed = await makeStore(withEcho);
  const stripped = await makeStore(minimal);
  try {
    const read = async (session: {store: SessionResponseStore}) =>
      plain(
        (
          await new WebUiSessionViewCache(session.store, {
            snapshotTurns: 0,
            maxPageTurns: 50,
          }).read()
        ).messages,
      );
    const echoedView = await read(echoed);
    assert.deepEqual(await read(stripped), echoedView);
    assert.deepEqual(echoedView, plain(messagesOf(TRANSCRIPT) as never));
    // The echo is really in the compared file, so the equality is meaningful.
    assert.match(await readFile(echoed.messagesPath, "utf8"), /request echo/);
    assert.doesNotMatch(await readFile(stripped.messagesPath, "utf8"), /request echo/);
  } finally {
    await echoed.store.close();
    await stripped.store.close();
    await rm(echoed.sessionDir, {recursive: true, force: true});
    await rm(stripped.sessionDir, {recursive: true, force: true});
  }
});

test("resolves turn starts with sequential scans and reuses the offsets", async () => {
  const { store, sessionDir, messagesPath } = await makeStore();
  try {
    const cache = new WebUiSessionViewCache(store, {snapshotTurns: 0, maxPageTurns: 50});
    const original = store.readRecordBefore.bind(store);
    let backwardLookups = 0;
    store.readRecordBefore = async (offset: number) => {
      backwardLookups += 1;
      return original(offset);
    };

    // TRANSCRIPT opens two turns, each with a run of its own. Resolving them
    // scans sequentially instead of opening the file once per run.
    await cache.read();
    assert.equal(backwardLookups, 0);
    await cache.read();
    assert.equal(backwardLookups, 0, "an unchanged transcript reuses resolved offsets");

    const appended: JsonRecord[] = [
      userRecord("third question"),
      completedRecord("r-3-1", 3, 1, [assistantItem("third answer", "msg-3-1")]),
    ];
    await appendFile(messagesPath, records(appended));
    const grown = await cache.read();
    assert.equal(backwardLookups, 0);
    assert.deepEqual(plain(grown.messages), plain(messagesOf([...TRANSCRIPT, ...appended]) as never));

    // Replacing the transcript drops the resolved offsets with the index.
    await writeTranscript(messagesPath, TRANSCRIPT);
    const replaced = await cache.read();
    assert.equal(backwardLookups, 0);
    assert.deepEqual(plain(replaced.messages), plain(messagesOf(TRANSCRIPT) as never));
    await store.close();
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("finds the first user record in a batched input while scanning turn starts", async () => {
  const batchId = "batch-turn-start";
  const rows: JsonRecord[] = [
    {
      type: "message",
      role: "developer",
      content: [{type: "input_text", text: "developer context"}],
      [SESSION_INPUT_BATCH_ID_FIELD]: batchId,
      [SESSION_INPUT_BATCH_INDEX_FIELD]: 0,
    },
    {
      ...userRecord("first batched question"),
      [SESSION_INPUT_BATCH_ID_FIELD]: batchId,
      [SESSION_INPUT_BATCH_INDEX_FIELD]: 1,
    },
    {
      ...userRecord("second batched question"),
      [SESSION_INPUT_BATCH_ID_FIELD]: batchId,
      [SESSION_INPUT_BATCH_INDEX_FIELD]: 2,
    },
    completedRecord("r-batch-1", 1, 1, [assistantItem("batched answer", "batch-answer")]),
  ];
  const {store, sessionDir, messagesPath} = await makeStore(rows);
  try {
    const text = await readFile(messagesPath, "utf8");
    const userOffsets: number[] = [];
    let offset = 0;
    for (const line of text.split("\n")) {
      if (line.length === 0) continue;
      const record = JSON.parse(line) as JsonRecord;
      if (record.role === "user") userOffsets.push(offset);
      offset += Buffer.byteLength(`${line}\n`, "utf8");
    }
    assert.equal(userOffsets.length, 2);
    const [firstUserOffset, secondUserOffset] = userOffsets;
    assert.ok(firstUserOffset !== undefined && secondUserOffset !== undefined);

    const view = await new WebUiSessionViewCache(store, {
      snapshotTurns: 0,
      maxPageTurns: 50,
    }).read({turns: 1});
    const firstUser = view.messages.find((message) => message.text === "first batched question");
    const secondUser = view.messages.find((message) => message.text === "second batched question");
    assert.ok(firstUser !== undefined && secondUser !== undefined);
    assert.equal(view.cursors.get(firstUser.id), firstUserOffset);
    assert.equal(view.cursors.get(secondUser.id), firstUserOffset);
    assert.equal(view.cursors.get("r-batch-1"), firstUserOffset);
  } finally {
    await store.close();
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("reads the backup's last turns once, in whole records", async () => {
  const sessionDir = await mkdtemp(join(tmpdir(), "agent-harness-view-walk-"));
  const messagesPath = join(sessionDir, "messages.jsonl");
  const line = (record: JsonRecord) => `${JSON.stringify(record)}\n`;
  const opening = [
    userRecord("first question"),
    completedRecord("r-1-1", 1, 1, [assistantItem("first answer", "msg-1-1")]),
  ];
  const closing = [
    completedRecord("r-2-2", 2, 2, [assistantItem("second answer", "msg-2-2")]),
    userRecord("third question"),
    completedRecord("r-3-1", 3, 1, [assistantItem("third answer", "msg-3-1")]),
  ];
  // The second turn's opening record is sized to straddle the walk's first
  // window, so a reader that does not start each window on a record boundary
  // sees half of it.
  const closingBytes = Buffer.byteLength(closing.map(line).join(""), "utf8");
  const target = BACKUP_TAIL_WINDOW_BYTES - closingBytes + 1000;
  const empty = line({...userRecord("second question"), content: [{type: "input_text", text: ""}]});
  const padding = target - Buffer.byteLength(empty, "utf8");
  const straddler = {...userRecord("second question"), content: [{type: "input_text", text: "x".repeat(padding)}]};
  const written = [...opening, straddler, ...closing];
  await writeFile(join(sessionDir, "messages.backup.jsonl"), written.map(line).join(""));

  const store = await SessionResponseStore.open(sessionDir, {messagesPath});
  try {
    const tail = await store.readBackupTail(3);
    assert.ok(tail !== undefined);
    const bytes = await readFile(store.messagesBackupPath);
    // The records cover the range exactly: no second read, and no record lost
    // to a window boundary.
    assert.equal(
      tail.records.map((record) => `${record.line}\n`).join(""),
      bytes.subarray(tail.start, tail.end).toString("utf8"),
    );
    assert.equal(tail.records[0]?.offset, tail.start);
    assert.equal(tail.records.length, written.length);

    const cache = new WebUiSessionViewCache(store, {snapshotTurns: 0, maxPageTurns: 50});
    const history = await cache.readHistory(20);
    assert.deepEqual(plain(history.messages), plain(messagesOf(written) as never));
    await store.close();
  } finally {
    await rm(sessionDir, {recursive: true, force: true});
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
    const older = await cache.read({ before: newest.nextBefore!, generation: newest.generation });
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

test("keeps every user item from one multi-item submission in the same page", async () => {
  const {store, sessionDir} = await makeStore([]);
  try {
    const first = userRecord("submission part one");
    const second = userRecord("submission part two");
    await store.appendInputItems([first, second]);
    const answer = assistantItem("one answer", "multi-answer");
    const turn = await store.beginTurn("multi-item-submission");
    await turn.stageOutputItem({type: "response.output_item.done", output_index: 0, item: answer});
    await turn.commit({
      type: "response.completed",
      session_turn: 1,
      session_step: 1,
      response: {id: "multi-item-response", status: "completed", output: [answer]},
    });

    const cache = new WebUiSessionViewCache(store, {snapshotTurns: 1, maxPageTurns: 50});
    const page = await cache.read();
    assert.deepEqual(plain(page.messages), plain(messagesOf([
      first,
      second,
      completedRecord("multi-item-response", 1, 1, [answer]),
    ]) as never));
  } finally {
    await store.close();
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("does not consume cursor runs beyond the transcript snapshot boundary", async () => {
  const {store, sessionDir, messagesPath} = await makeStore([
    userRecord("first question"),
    completedRecord("race-first", 1, 1, [assistantItem("first answer")]),
  ]);
  try {
    const cache = new WebUiSessionViewCache(store, {snapshotTurns: 1, maxPageTurns: 50});
    const readCursorIndexSnapshot = store.readCursorIndexSnapshot.bind(store);
    let appended = false;
    store.readCursorIndexSnapshot = async () => {
      if (!appended) {
        appended = true;
        await appendFile(messagesPath, records([
          userRecord("second question"),
          completedRecord("race-second", 2, 1, [assistantItem("second answer")]),
        ]));
      }
      return readCursorIndexSnapshot();
    };

    const first = await cache.read();
    assert.deepEqual(first.messages.map((message) => message.text), ["first question", "first answer"]);
    const second = await cache.read();
    assert.deepEqual(second.messages.map((message) => message.text), ["second question", "second answer"]);
    assert.equal(second.hasMore, true);
  } finally {
    await store.close();
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("rejects a page cursor after compaction replaces its transcript generation", async () => {
  const {store, sessionDir} = await makeStore();
  try {
    const cache = new WebUiSessionViewCache(store, {snapshotTurns: 1, maxPageTurns: 50});
    const newest = await cache.read();
    assert.notEqual(newest.nextBefore, null);
    const compacted = await store.compactHistory({
      turnType: "assistant_final", keepTurns: 1, keepToolPairs: 0, backupMode: "replace",
    });
    assert.equal(compacted.compacted, true);

    const stale = await cache.read({before: newest.nextBefore!, generation: newest.generation});
    assert.equal(stale.staleCursor, true);
    assert.deepEqual(stale.messages, []);
    assert.equal(stale.nextBefore, null);
  } finally {
    await store.close();
    await rm(sessionDir, {recursive: true, force: true});
  }
});

test("caps whitespace-formatted JSONL backups to the requested turns", async () => {
  const sessionDir = await mkdtemp(join(tmpdir(), "agent-harness-spaced-backup-"));
  const rows = [
    userRecord("first"),
    completedRecord("spaced-r1", 1, 1, [assistantItem('quoted marker {"role":"user"}')]),
    userRecord("second"),
    completedRecord("spaced-r2", 2, 1, [assistantItem("answer two")]),
  ];
  const backup = rows.map((record) => JSON.stringify(record).replace('"role":"user"', '"role": "user"')).join("\n") + "\n";
  await writeFile(join(sessionDir, "messages.backup.jsonl"), backup);
  const store = await SessionResponseStore.open(sessionDir);
  try {
    const cache = new WebUiSessionViewCache(store, {snapshotTurns: 0, maxPageTurns: 50});
    const history = await cache.readHistory(1);
    assert.deepEqual(plain(history.messages), plain(messagesOf(rows.slice(2)) as never));
    const fullHistory = await cache.readHistory(2);
    assert.deepEqual(plain(fullHistory.messages), plain(messagesOf(rows) as never));
  } finally {
    await store.close();
    await rm(sessionDir, {recursive: true, force: true});
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
    const history = await cache.readHistory(20);
    assert.deepEqual(plain(history.messages), plain(messagesOf(records) as never));
    assert.equal(
      history.messages.some((message) => message.parts.some((part) => part.type === "reasoning")),
      true,
    );
    assert.equal(
      history.messages.some((message) => message.parts.some((part) => part.type === "tool_result")),
      true,
    );
    assert.equal(typeof history.etag, "string");
    // The snapshot carries the live tail and a reference, not the block itself.
    const view = await cache.read();
    assert.deepEqual(view.messages, []);
    assert.deepEqual(view.history, {etag: history.etag, count: history.messages.length});
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
    // run-2 comes from the transcript, everything before it from the backup: the
    // seam keeps the record out of the block, so it is rendered once.
    const history = await cache.readHistory(20);
    assert.deepEqual(
      history.messages.map((message) => message.sender),
      ["user", "assistant"],
    );
    assert.equal(history.messages.filter((message) => message.id === "run-2").length, 0);
    assert.equal(
      history.messages[1]?.parts.some((part) => part.type === "tool_result"),
      true,
    );
    const view = await cache.read();
    assert.deepEqual(view.messages.map((message) => message.id), ["run-2"]);
    assert.equal(
      [...history.messages, ...view.messages].filter((message) => message.text === "draft answer").length,
      1,
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
    const whole = await cache.readHistory(20);
    assert.deepEqual(plain(whole.messages), plain(messagesOf(TRANSCRIPT) as never));

    const capped = await cache.readHistory(1);
    assert.deepEqual(
      plain(capped.messages),
      plain(messagesOf([TRANSCRIPT[4]!, TRANSCRIPT[5]!]) as never),
    );
    assert.equal(capped.messages[0]?.text, "second question");
    assert.notEqual(capped.etag, whole.etag);

    const hidden = await cache.readHistory(0);
    assert.deepEqual(hidden.messages, []);
    assert.equal(hidden.etag, null);
    const view = await cache.read({backupTurns: 0});
    assert.deepEqual(view.messages, []);
    assert.deepEqual(view.history, {etag: null, count: 0});
    assert.equal(view.hasMore, false);
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
    assert.deepEqual(await cache.readHistory(20), {etag: null, messages: []});
  } finally {
    await store.close();
    await rm(sessionDir, { recursive: true, force: true });
  }
});

test("keeps the history block stable while the tail grows", async () => {
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
    // The transcript is empty after compaction, so the snapshot is a reference
    // to the block alone.
    const history = await cache.readHistory(20);
    assert.equal(history.messages.length > 0, true);
    const before = await cache.read();
    assert.deepEqual(before.messages, []);
    assert.equal(before.history?.etag, history.etag);

    await appendFile(
      messagesPath,
      records([completedRecord("r-9-1", 9, 1, [assistantItem("tail answer", "msg-9-1")])]),
    );
    const grown = await cache.read();
    assert.equal(grown.messages.at(-1)?.id, "r-9-1");
    // The append moved the tail, not the block: same identity, same messages.
    const after = await cache.readHistory(20);
    assert.equal(after.etag, history.etag);
    assert.deepEqual(plain(after.messages), plain(history.messages));
  } finally {
    await store.close();
    await rm(sessionDir, { recursive: true, force: true });
  }
});
